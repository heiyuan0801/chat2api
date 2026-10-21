package controlplane

import (
	"context"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/http"
	"strings"
)

func publicErrorPayload(err error, anthropic bool) (int, Map) {
	status, message, code := 502, "upstream request failed", ""
	var p *Problem
	var validation *ValidationError
	var image *ImageExecutionError
	var pool *PoolError
	var up *upstream.Error
	switch {
	case errors.As(err, &validation):
		status = 422
		messages := []string{}
		for _, issue := range validation.Details {
			loc := []string{}
			for _, part := range list(issue["loc"]) {
				if text(part) != "body" {
					loc = append(loc, text(part))
				}
			}
			msg := text(issue["msg"])
			if len(loc) > 0 {
				msg = strings.Join(loc, ".") + ": " + msg
			}
			messages = append(messages, msg)
		}
		message = strings.Join(messages, "; ")
	case errors.As(err, &image):
		status, message, code = image.Status, image.Message, image.Code
	case errors.As(err, &p):
		status, message = p.Status, p.Message
	case errors.As(err, &pool):
		status, message = pool.Status, pool.Message
	case errors.As(err, &up):
		message = up.Error()
	case errors.Is(err, context.Canceled):
		status, message = 499, "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		message = "upstream request timed out"
	}
	kind, defaultCode := "server_error", "upstream_error"
	switch {
	case status == 401:
		kind, defaultCode = "authentication_error", "invalid_api_key"
	case status == 403:
		kind, defaultCode = "permission_error", "permission_denied"
	case status == 429:
		kind, defaultCode = "rate_limit_error", "rate_limit_exceeded"
	case status >= 400 && status < 500:
		kind, defaultCode = "invalid_request_error", "bad_request"
	}
	if image != nil && text(image.Details["error_type"]) != "" {
		kind = text(image.Details["error_type"])
	}
	if code == "" {
		code = defaultCode
	}
	if anthropic {
		if status >= 500 {
			kind = "api_error"
		}
		return status, Map{"type": "error", "error": Map{"type": kind, "message": message}}
	}
	return status, Map{"error": Map{"message": message, "type": kind, "param": nil, "code": code}}
}
func writePublicError(w http.ResponseWriter, r *http.Request, err error) {
	status, payload := publicErrorPayload(err, r.URL.Path == "/v1/messages")
	writeJSON(w, status, payload)
}

func publicStreamErrorPayload(err error, protocol string) Map {
	var image *ImageExecutionError
	if errors.As(err, &image) {
		_, payload := publicErrorPayload(err, false)
		return payload
	}
	message := "upstream request failed"
	kind := "RuntimeError"
	var p *Problem
	var up *upstream.Error
	switch {
	case errors.As(err, &p):
		message = p.Message
		kind = "HTTPException"
	case errors.As(err, &up):
		message = up.Error()
		if up.Status > 0 {
			kind = "UpstreamHTTPError"
		}
	case errors.Is(err, context.DeadlineExceeded):
		message = "upstream request timed out"
		kind = "TimeoutError"
	case errors.Is(err, context.Canceled):
		message = "request canceled"
	}
	if protocol == "anthropic" {
		return Map{"type": "error", "error": Map{"type": kind, "message": message}}
	}
	return Map{"error": Map{"type": kind, "message": message}}
}
