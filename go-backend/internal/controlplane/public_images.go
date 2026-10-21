package controlplane

import (
	"context"
	"net/http"
)

func (s *Server) registerPublicImages() {
	imageResult := func(ctx context.Context, payload Map, prefix string) (any, error) {
		if !boolean(payload["stream"], false) {
			return s.Images.Execute(ctx, payload, nil, nil)
		}
		return &PublicStream{Protocol: "image", Produce: func(ctx context.Context, emit func(Map) error) error {
			emitted := 0
			var sendErr error
			result, err := s.Images.Execute(ctx, payload, nil, func(items []Map) {
				if sendErr != nil {
					return
				}
				for ; emitted < len(items); emitted++ {
					item := items[emitted]
					if text(item["b64_json"]) == "" && text(item["url"]) == "" {
						continue
					}
					event := Map{
						"type":            prefix + ".completed",
						"usage":           publicTokenUsage(countPublicTextTokens(publicString(payload["prompt"]), publicString(payload["model"])), publicInputImageTokens(imageInputs(payload["images"])), 0, countOutputImageTokens([]Map{item}, publicString(payload["size"]), publicString(payload["quality"]))),
						"_image_metadata": []Map{{"width": item["width"], "height": item["height"]}},
					}
					for _, field := range []string{"b64_json", "url"} {
						if text(item[field]) != "" {
							event[field] = item[field]
						}
					}
					sendErr = emit(event)
					if sendErr != nil {
						return
					}
				}
			})
			// Image payloads were already counted by the emitted events.
			observePublicResult(ctx, Map{
				"_conversation_id":     result["_conversation_id"],
				"_account_email":       result["_account_email"],
				"_upstream_account_id": result["_upstream_account_id"],
				"_image_attempts":      result["_image_attempts"],
				"_image_urls":          result["_image_urls"],
			})
			if sendErr != nil {
				return sendErr
			}
			return err
		}}, nil
	}
	s.publicRoute("POST", "/v1/images/generations", "identity", "ImageGenerationRequest", func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
		body["base_url"] = s.resolveImageBaseURL(r)
		return imageResult(ctx, body, "image_generation")
	})
	// Edits has its own multipart/JSON parser and size boundaries. Authenticate
	// before materializing inputs, as the Python route does.
	s.routes = append(s.routes, registeredRoute{Route{"POST", "/v1/images/edits", "identity", ""}, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		var call *CallRecorder
		defer func() {
			if recover() != nil {
				err := problem(500, "Internal Server Error")
				_ = call.Finish(nil, err)
				writePublicError(w, r, err)
			}
		}()
		if s.executionContext != nil {
			stop := context.AfterFunc(s.executionContext, cancel)
			defer stop()
		}
		who, err := s.Auth.Identity(ctx, r.Header.Get("Authorization"))
		if err != nil {
			writePublicError(w, r, err)
			return
		}
		payload, images, masks, err := s.parseImageEdit(ctx, w, r)
		if err != nil {
			writePublicError(w, r, err)
			return
		}
		payload["images"], payload["mask"], payload["_edit"], payload["base_url"] = images, masks, true, s.resolveImageBaseURL(r)
		call = s.beginPublicCall(ctx, r.URL.Path, payload, who)
		if call == nil {
			writePublicError(w, r, problem(503, "server is shutting down"))
			return
		}
		ctx = context.WithValue(ctx, publicCallContextKey{}, call)
		if err = s.checkContent(ctx, payload["prompt"]); err != nil {
			_ = call.Finish(nil, err)
			writePublicError(w, r, err)
			return
		}
		result, err := imageResult(ctx, payload, "image_edit")
		if err != nil {
			_ = call.Finish(nil, err)
			writePublicError(w, r, err)
			return
		}
		if stream, ok := result.(*PublicStream); ok {
			writePublicStream(ctx, w, r, recordPublicStream(stream, call))
		} else {
			if err := call.Finish(result, nil); err != nil {
				writePublicError(w, r, err)
				return
			}
			writeJSON(w, 200, stripPublicInternal(result))
		}
	}})
	s.route("GET", "/images/{image_path:path}", "public", "", func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
		data, media, err := s.Assets.Read(ctx, r.PathValue("image_path"))
		if err != nil {
			return nil, problem(404, "image not found")
		}
		return &Download{Bytes: data, ContentType: media, Headers: map[string]string{"Access-Control-Allow-Origin": "*", "Cache-Control": "public, max-age=31536000, immutable"}}, nil
	})
}
