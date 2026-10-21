package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
)

var imageFetchSlots = make(chan struct{}, 8)
var inputNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func inputFilename(name, mimeType string) string {
	name = strings.Trim(inputNamePattern.ReplaceAllString(path.Base(name), "_"), "._")
	if name == "" {
		name = "image"
	}
	if !strings.Contains(name, ".") {
		ext := "png"
		if x, _, e := mime.ParseMediaType(mimeType); e == nil && strings.HasPrefix(x, "image/") {
			ext = strings.TrimPrefix(x, "image/")
			if ext == "jpeg" {
				ext = "jpg"
			}
		}
		name += "." + ext
	}
	return name
}
func acquireInputSlot(ctx context.Context) (func(), error) {
	select {
	case imageFetchSlots <- struct{}{}:
		return func() { <-imageFetchSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func decodeImageBase64(encoded, name, media string) (upstream.ImageInput, error) {
	var out upstream.ImageInput
	if len(encoded) > ((maxImageBytes+2)/3)*4+4 {
		return out, problem(400, "image URL exceeds 50MB limit")
	}
	data, e := base64.StdEncoding.Strict().DecodeString(encoded)
	if e != nil {
		return out, problem(400, "invalid base64 image data")
	}
	if len(data) == 0 {
		return out, problem(400, "image file is empty")
	}
	if len(data) > maxImageBytes {
		return out, problem(400, "image URL exceeds 50MB limit")
	}
	if media == "" {
		media = "image/png"
	}
	return upstream.ImageInput{Data: data, MIME: media, Name: inputFilename(name, media)}, nil
}

// readImageReferences is shared by edits and text multimodal adapters. Network
// references go through the pinned-address upstream fetch seam, never proxies.
func (s *Server) readImageReferences(ctx context.Context, value any) ([]upstream.ImageInput, error) {
	inputs := []upstream.ImageInput{}
	total := 0
	var walk func(any, int) error
	add := func(v upstream.ImageInput) error {
		if len(inputs) >= 16 {
			return problem(400, "at most 16 image inputs are supported")
		}
		if len(v.Data) > maxImageBytes {
			return problem(400, "image input exceeds 50MB limit")
		}
		total += len(v.Data)
		if total > 100<<20 {
			return problem(400, "combined image inputs exceed 100MB limit")
		}
		inputs = append(inputs, v)
		return nil
	}
	walk = func(raw any, depth int) error {
		if depth > 16 {
			return problem(400, "invalid image reference")
		}
		switch v := raw.(type) {
		case nil:
			return nil
		case upstream.ImageInput:
			return add(v)
		case []upstream.ImageInput:
			for _, item := range v {
				if e := add(item); e != nil {
					return e
				}
			}
			return nil
		case []any:
			for _, item := range v {
				if e := walk(item, depth+1); e != nil {
					return e
				}
			}
			return nil
		case map[string]any:
			if truth(v["file_id"]) {
				return problem(400, "file_id image references are not supported; use image_url instead")
			}
			if inline := text(orValue(v["b64_json"], v["base64"])); inline != "" {
				input, e := decodeImageBase64(inline, text(orValue(v["filename"], v["file_name"])), text(orValue(v["mime_type"], v["mimeType"])))
				if e != nil {
					return e
				}
				return add(input)
			}
			if source := object(v["source"]); source["type"] == "base64" {
				input, e := decodeImageBase64(text(source["data"]), "", text(source["media_type"]))
				if e != nil {
					return e
				}
				return add(input)
			}
			for _, k := range []string{"image_url", "url"} {
				if child, ok := v[k]; ok {
					return walk(child, depth+1)
				}
			}
			return problem(400, "image reference must include image_url")
		case string:
			raw := strings.TrimSpace(v)
			if raw == "" {
				return nil
			}
			if (strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{")) && len(raw) < 150<<20 {
				var decoded any
				d := json.NewDecoder(strings.NewReader(raw))
				d.UseNumber()
				if d.Decode(&decoded) == nil {
					return walk(decoded, depth+1)
				}
			}
			var input upstream.ImageInput
			var e error
			if strings.HasPrefix(strings.ToLower(raw), "data:") {
				header, payload, ok := strings.Cut(raw, ",")
				if !ok {
					return problem(400, "invalid data image URL")
				}
				media := strings.Split(header[5:], ";")[0]
				if media == "" {
					media = "image/png"
				}
				if !strings.HasPrefix(media, "image/") {
					return problem(400, "image_url must point to an image")
				}
				if strings.Contains(header, ";base64") {
					if len(payload) > ((maxImageBytes+2)/3)*4+4 {
						return problem(400, "image URL exceeds 50MB limit")
					}
					data, decodeErr := base64.StdEncoding.Strict().DecodeString(payload)
					if decodeErr != nil {
						return problem(400, "invalid data image URL")
					}
					if len(data) == 0 {
						return problem(400, "image URL is empty")
					}
					if len(data) > maxImageBytes {
						return problem(400, "image URL exceeds 50MB limit")
					}
					input = upstream.ImageInput{Data: data, MIME: media, Name: inputFilename("image_url", media)}
				} else {
					if len(payload) > maxImageBytes*2 {
						return problem(400, "image URL exceeds 50MB limit")
					}
					decoded, de := url.PathUnescape(payload)
					if de != nil {
						return problem(400, "invalid data image URL")
					}
					input = upstream.ImageInput{Data: []byte(decoded), MIME: media, Name: inputFilename("image_url", media)}
					if len(input.Data) == 0 {
						return problem(400, "image URL is empty")
					}
				}
			} else if strings.HasPrefix(strings.ToLower(raw), "http://") || strings.HasPrefix(strings.ToLower(raw), "https://") {
				release, slotErr := acquireInputSlot(ctx)
				if slotErr != nil {
					return slotErr
				}
				data, media, fetchErr := upstream.FetchPublicImage(ctx, raw, maxImageBytes, true)
				release()
				if fetchErr != nil {
					return problem(400, "image_url fetch failed")
				}
				if media == "" || media == "application/octet-stream" || media == "binary/octet-stream" {
					media = "image/png"
				}
				if !strings.HasPrefix(media, "image/") {
					return problem(400, "image_url must point to an image")
				}
				parsed, _ := url.Parse(raw)
				input = upstream.ImageInput{Data: data, MIME: media, Name: inputFilename(path.Base(parsed.Path), media)}
			} else {
				input, e = decodeImageBase64(raw, "image.png", "image/png")
			}
			if e != nil {
				return e
			}
			return add(input)
		default:
			return problem(400, "invalid image reference")
		}
	}
	if e := walk(value, 0); e != nil {
		return nil, e
	}
	return inputs, nil
}
func parseEditFields(fields Map) (Map, error) {
	prompt := text(fields["prompt"])
	if prompt == "" {
		return nil, problem(400, "prompt is required")
	}
	n := 1
	if truth(fields["n"]) {
		if raw, ok := fields["n"].(string); ok {
			var err error
			n, err = strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return nil, problem(400, "n must be an integer")
			}
		} else {
			f, ok := number(fields["n"])
			if !ok {
				return nil, problem(400, "n must be an integer")
			}
			n = int(f)
		}
	}
	if n < 1 || n > 4 {
		return nil, problem(400, "n must be between 1 and 4")
	}
	var stream any
	if v := fields["stream"]; v != nil && v != "" {
		if b, ok := v.(bool); ok {
			stream = b
		} else {
			switch strings.ToLower(text(v)) {
			case "true", "1", "yes", "y", "on":
				stream = true
			case "false", "0", "no", "n", "off":
				stream = false
			default:
				return nil, problem(400, "stream must be a boolean")
			}
		}
	}
	payload := Map{"prompt": prompt, "model": orValue(text(fields["model"]), "gpt-image-2"), "n": n, "size": nil, "quality": orValue(text(fields["quality"]), "auto"), "response_format": orValue(text(fields["response_format"]), "b64_json"), "stream": stream}
	if size := text(fields["size"]); size != "" {
		payload["size"] = size
	}
	if v, ok := fields["client_task_id"]; ok {
		payload["client_task_id"] = text(v)
	}
	return payload, nil
}
func (s *Server) parseImageEdit(ctx context.Context, w http.ResponseWriter, r *http.Request) (Map, []upstream.ImageInput, []upstream.ImageInput, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 300<<20)
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	fields := Map{}
	var refs, masks []any
	if media == "application/json" {
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		var rawBody any
		if e := decoder.Decode(&rawBody); e != nil {
			return nil, nil, nil, problem(400, "invalid JSON body")
		}
		var objectBody bool
		fields, objectBody = rawBody.(map[string]any)
		if !objectBody {
			return nil, nil, nil, problem(400, "JSON body must be an object")
		}

		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return nil, nil, nil, problem(400, "invalid JSON body")
		}
		for _, k := range []string{"images", "image", "image_url"} {
			if v, ok := fields[k]; ok {
				refs = append(refs, v)
			}
		}
		if v, ok := fields["mask"]; ok {
			masks = append(masks, v)
		}
	} else if media == "application/x-www-form-urlencoded" {
		if e := r.ParseForm(); e != nil {
			return nil, nil, nil, problem(400, "invalid image form")
		}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				fields[k] = v[len(v)-1]
			}
			switch k {
			case "image", "image[]", "images", "images[]", "image_url", "image_url[]":
				for _, ref := range v {
					refs = append(refs, ref)
				}
			case "mask", "mask[]":
				for _, ref := range v {
					masks = append(masks, ref)
				}
			}
		}
	} else {
		reader, e := r.MultipartReader()
		if e != nil {
			return nil, nil, nil, problem(400, "invalid image form")
		}
		fieldBytes := int64(0)
		imageBytes, maskBytes := 0, 0
		for parts := 0; ; parts++ {
			if parts > 128 {
				return nil, nil, nil, problem(400, "too many multipart fields")
			}
			part, e := reader.NextPart()
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				return nil, nil, nil, problem(400, "invalid image form")
			}
			name := part.FormName()
			isImage := false
			isMask := false
			switch name {
			case "image", "image[]", "images", "images[]", "image_url", "image_url[]":
				isImage = true
			case "mask", "mask[]":
				isMask = true
			}
			limit := int64(1 << 20)
			if isImage || isMask {
				limit = maxImageBytes
			}
			data, e := io.ReadAll(io.LimitReader(part, limit+1))
			part.Close()
			if e != nil || int64(len(data)) > limit {
				return nil, nil, nil, problem(400, "image file exceeds 50MB limit")
			}
			fieldBytes += int64(len(data))
			if fieldBytes > 220<<20 {
				return nil, nil, nil, problem(400, "combined image inputs exceed 100MB limit")
			}
			if !isImage && !isMask {
				fields[name] = string(data)
				continue
			}
			var ref any = string(data)
			if part.FileName() != "" {
				if len(data) == 0 {
					return nil, nil, nil, problem(400, "image file is empty")
				}
				media := part.Header.Get("Content-Type")
				if media == "" {
					media = "image/png"
				}
				ref = upstream.ImageInput{Data: data, Name: inputFilename(part.FileName(), media), MIME: media}
			}
			if isMask {
				maskBytes += len(data)
				masks = append(masks, ref)
			} else {
				imageBytes += len(data)
				refs = append(refs, ref)
			}
			if len(refs) > 16 || len(masks) > 16 {
				return nil, nil, nil, problem(400, "at most 16 image inputs are supported")
			}
			if imageBytes > 100<<20 || maskBytes > 100<<20 {
				return nil, nil, nil, problem(400, "combined image inputs exceed 100MB limit")
			}
		}
	}
	payload, e := parseEditFields(fields)
	if e != nil {
		return nil, nil, nil, e
	}
	images, e := s.readImageReferences(ctx, refs)
	if e != nil {
		return nil, nil, nil, e
	}
	maskInputs, e := s.readImageReferences(ctx, masks)
	if e != nil {
		return nil, nil, nil, e
	}
	if len(images) == 0 {
		return nil, nil, nil, problem(400, "image file or image_url is required")
	}
	return payload, images, maskInputs, nil
}
func (s *Server) resolveImageBaseURL(r *http.Request) string {
	values, e := s.Settings.Values(r.Context())
	if e == nil && text(values["base_url"]) != "" {
		return strings.TrimRight(text(values["base_url"]), "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, r.Host)
}
