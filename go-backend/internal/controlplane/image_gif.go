package controlplane

import (
	"context"
	"fmt"
	"strings"
)

var imageGIFFields = []string{"output_format", "frame_count", "frame_duration_ms"}

// Reuse the public contract for JSON, multipart edits and queued execution.
// Null and omitted values intentionally do not supply animation defaults.
func imageGIFOptions(payload Map) (Map, error) {
	options := Map{}
	properties := object(publicSpec.Models["ImageGenerationRequest"]["properties"])
	for _, key := range imageGIFFields {
		value := payload[key]
		if value == nil {
			continue
		}
		if _, isBool := value.(bool); isBool && key != "output_format" {
			return nil, problem(400, key+" must be an integer")
		}
		normalized, issues := validateSchema(value, object(properties[key]), nil, []any{"body", key})
		if len(issues) > 0 {
			return nil, problem(400, key+": "+text(issues[0]["msg"]))
		}
		options[key] = normalized
	}
	if options["output_format"] != "gif" {
		for _, key := range imageGIFFields[1:] {
			if options[key] != nil {
				return nil, problem(400, key+" requires output_format=gif")
			}
		}
	}
	return options, nil
}

func validateImageGIFScope(payload, settings Map) error {
	if payload["output_format"] != "gif" || settings["image_gif_model_scope"] == "all" || isImage26Model(text(payload["model"])) {
		return nil
	}
	return &ImageExecutionError{
		Code: "request_validation_failed", Status: 400,
		Message: "output_format=gif requires gpt-image-2.6-flare or gpt-image-2.6-sunburst; set image_gif_model_scope=all to allow other supported image models",
		Details: Map{"failure_code": "request_validation_failed", "failure_scope": "request", "failure_retryable": false, "outcome": "failed"},
	}
}

func (s *Server) checkImageGIFScope(ctx context.Context, payload Map) error {
	if payload["output_format"] != "gif" {
		return nil
	}
	settings, err := s.Settings.Values(ctx)
	if err != nil {
		return err
	}
	if err := validateImageGIFScope(payload, settings); err != nil {
		return problem(400, err.Error())
	}
	return nil
}

// Only the internal upstream prompt gets these hints. The caller payload remains
// unchanged for Call Records, retries, task metadata and revised_prompt.
func imageGIFPrompt(prompt string, payload Map) string {
	if payload["output_format"] != "gif" {
		return prompt
	}
	var hint strings.Builder
	hint.WriteString("\n\nOutput requirements: Create and return a downloadable animated GIF file, not only a static preview or sprite sheet.")
	if payload["frame_count"] != nil {
		fmt.Fprintf(&hint, " Use %d animation frames.", integer(payload["frame_count"], 0))
	}
	if payload["frame_duration_ms"] != nil {
		fmt.Fprintf(&hint, " Set each frame duration to %d milliseconds.", integer(payload["frame_duration_ms"], 0))
	}
	return prompt + hint.String()
}
