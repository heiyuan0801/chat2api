package controlplane

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

//go:embed public_spec.json
var publicSpecBytes []byte
var publicSpec = func() struct {
	Models          map[string]Map    `json:"models"`
	ModelEncodings  map[string]string `json:"model_encodings"`
	PrefixEncodings map[string]string `json:"prefix_encodings"`
} {
	var v struct {
		Models          map[string]Map    `json:"models"`
		ModelEncodings  map[string]string `json:"model_encodings"`
		PrefixEncodings map[string]string `json:"prefix_encodings"`
	}
	d := json.NewDecoder(bytes.NewReader(publicSpecBytes))
	d.UseNumber()
	if d.Decode(&v) != nil {
		panic("invalid public contracts")
	}
	return v
}()

func validatePublic(model string, raw any) (Map, error) {
	schema, ok := publicSpec.Models[model]
	if !ok {
		return Validate(model, raw)
	}
	value, issues := validateSchema(raw, schema, object(schema["$defs"]), []any{"body"})
	if len(issues) > 0 {
		for _, issue := range issues {
			if issue["type"] == "model_attributes_type" && len(list(issue["loc"])) > 2 {
				issue["msg"] = "Input should be a valid dictionary"
				issue["type"] = "dict_type"
			}
		}
		return nil, &ValidationError{issues}
	}
	out := object(value)
	for key, field := range object(schema["properties"]) {
		if _, ok := out[key]; !ok {
			if def, present := object(field)["default"]; present {
				out[key] = copyMap(Map{"value": def})["value"]
			}
		}
	}
	return out, nil
}
func decodePublic(w http.ResponseWriter, r *http.Request, model string) (Map, error) {
	if model == "" {
		return Map{}, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	var raw any
	if e := d.Decode(&raw); e != nil {
		var size *http.MaxBytesError
		if errors.As(e, &size) {
			return nil, problem(413, "request body is too large")
		}
		if e == io.EOF {
			return nil, &ValidationError{[]Map{validationIssue("missing", "Field required", []any{"body"}, nil, nil)}}
		}
		return nil, &ValidationError{[]Map{validationIssue("json_invalid", "JSON decode error", []any{"body", 0}, nil, nil)}}
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, &ValidationError{[]Map{validationIssue("json_invalid", "JSON decode error", []any{"body", 0}, nil, nil)}}
	}
	return validatePublic(model, raw)
}

type PublicStream struct {
	Protocol string
	Produce  func(context.Context, func(Map) error) error
}
type publicEvent struct {
	value Map
	err   error
	done  bool
}

func (s *Server) publicRoute(method, path, access, model string, fn endpoint) {
	s.routes = append(s.routes, registeredRoute{Route{method, path, access, model}, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
		defer cancel()
		if s.executionContext != nil {
			stop := context.AfterFunc(s.executionContext, cancel)
			defer stop()
		}
		var call *CallRecorder
		defer func() {
			if recover() != nil {
				err := problem(500, "Internal Server Error")
				if call != nil {
					_ = call.Finish(nil, err)
				}
				writePublicError(w, r, err)
			}
		}()
		var identity Map
		var e error
		if access != "public" {
			auth := r.Header.Get("Authorization")
			if path == "/v1/messages" && auth == "" && r.Header.Get("x-api-key") != "" {
				auth = "Bearer " + r.Header.Get("x-api-key")
			}
			identity, e = s.Auth.Identity(ctx, auth)
			if e != nil {
				writePublicError(w, r, e)
				return
			}
		}
		body, e := decodePublic(w, r, model)
		if e != nil {
			writePublicError(w, r, e)
			return
		}
		if method == "POST" {
			call = s.beginPublicCall(ctx, path, body, identity)
			if call == nil {
				writePublicError(w, r, problem(503, "server is shutting down"))
				return
			}
			ctx = context.WithValue(ctx, publicCallContextKey{}, call)
			if e = s.checkContent(ctx, body); e != nil {
				_ = call.Finish(nil, e)
				writePublicError(w, r, e)
				return
			}
		}
		value, e := fn(ctx, r, body, identity)
		if e != nil {
			if call != nil {
				_ = call.Finish(nil, e)
			}
			writePublicError(w, r, e)
			return
		}
		stream, ok := value.(*PublicStream)
		if !ok {
			if call != nil {
				if e = call.Finish(value, nil); e != nil {
					writePublicError(w, r, e)
					return
				}
			}
			writeJSON(w, 200, stripPublicInternal(value))
			return
		}
		writePublicStream(ctx, w, r, recordPublicStream(stream, call))
	}})
}

func recordPublicStream(stream *PublicStream, call *CallRecorder) *PublicStream {
	if call == nil {
		return stream
	}
	return &PublicStream{Protocol: stream.Protocol, Produce: func(ctx context.Context, emit func(Map) error) (err error) {
		defer func() {
			if recover() != nil {
				err = problem(500, "Internal Server Error")
			}
			if finishErr := call.Finish(nil, err); err == nil {
				err = finishErr
			}
		}()
		first := true
		return stream.Produce(ctx, func(event Map) error {
			if first {
				call.Stage("stream_first_item", nil)
				first = false
			}
			call.Observe(event)
			return emit(event)
		})
	}}
}
func writePublicStream(ctx context.Context, w http.ResponseWriter, r *http.Request, stream *PublicStream) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan publicEvent)
	go func() {
		emit := func(value Map) error {
			select {
			case events <- publicEvent{value: value}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var err error
		func() {
			defer func() {
				if recover() != nil {
					err = problem(500, "Internal Server Error")
				}
			}()
			err = stream.Produce(ctx, emit)
		}()
		select {
		case events <- publicEvent{err: err, done: true}:
		case <-ctx.Done():
		}
	}()
	var first publicEvent
	select {
	case first = <-events:
	case <-ctx.Done():
		writePublicError(w, r, ctx.Err())
		return
	}
	if first.done && first.err != nil {
		writePublicError(w, r, first.err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if stream.Protocol != "anthropic" && stream.Protocol != "image" {
		if _, e := io.WriteString(w, ": stream-open\n\n"); e != nil {
			return
		}
		flush()
	}
	send := func(event publicEvent) bool {
		if event.done {
			return false
		}
		value := stripPublicInternal(event.value)
		data, e := json.Marshal(value)
		if e != nil {
			return false
		}
		prefix := ""
		if stream.Protocol == "anthropic" || stream.Protocol == "image" {
			prefix = "event: " + text(event.value["type"]) + "\n"
		}
		if _, e = io.WriteString(w, prefix+"data: "+string(data)+"\n\n"); e != nil {
			return false
		}
		flush()
		return true
	}
	event := first
	for {
		if event.done {
			if event.err != nil {
				payload := publicStreamErrorPayload(event.err, stream.Protocol)
				if stream.Protocol == "anthropic" || stream.Protocol == "image" {
					io.WriteString(w, "event: error\n")
				}
				data, _ := json.Marshal(payload)
				io.WriteString(w, "data: "+string(data)+"\n\n")
			}
			if stream.Protocol != "anthropic" && stream.Protocol != "image" {
				io.WriteString(w, "data: [DONE]\n\n")
			}
			flush()
			return
		}
		if !send(event) {
			return
		}
		select {
		case event = <-events:
		case <-ctx.Done():
			event = publicEvent{err: ctx.Err(), done: true}
		}
	}
}
func stripPublicInternal(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := Map{}
		for k, x := range v {
			if publicInternalFields[k] {
				continue
			}
			out[k] = stripPublicInternal(x)
		}
		return out
	case []Map:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = stripPublicInternal(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = stripPublicInternal(x)
		}
		return out
	default:
		return value
	}
}

var publicInternalFields = map[string]bool{"_account_email": true, "_call_error": true, "_conversation_id": true, "_upstream_account_id": true, "_call_id": true, "_call_status": true, "_image_urls": true, "_image_attempts": true, "_image_metadata": true}

type publicCallContextKey struct{}

func observePublicAccount(ctx context.Context, account Map) {
	if call, ok := ctx.Value(publicCallContextKey{}).(*CallRecorder); ok && call != nil {
		call.Observe(Map{"_account_email": account["email"]})
	}
}

func observePublicResult(ctx context.Context, result Map) {
	if call, ok := ctx.Value(publicCallContextKey{}).(*CallRecorder); ok && call != nil {
		call.Observe(result)
	}
}
