package upstream

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strings"
)

var svgRequest = regexp.MustCompile(`(?i)\bsvg\b`)

// SVG delivery is a prompt-driven Web attachment flow, not an official Images
// API output_format control or a native image-model selector.
func wantsSVG(prompt string) bool { return svgRequest.MatchString(prompt) }

func (s *conversationState) hasImageResult() bool {
	if s.svgRequested {
		return len(s.svgArtifacts) > 0
	}
	return len(s.Pointers) > 0
}

// Inspect only the current turn, and only committed output messages. Code and
// reasoning may mention a filename before the file actually exists.
func (s *conversationState) collectSVG(doc map[string]any) (bool, error) {
	mapping := map[string]any{}
	terminal := false
	var resultErr error
	for _, m := range currentTurnWithUser(doc) {
		role, channel := text(object(m["author"])["role"]), text(m["channel"])
		if role == "user" && wantsSVG(searchMessageText(m)) {
			s.svgRequested = true
		}
		if role != "assistant" && role != "tool" {
			continue
		}
		if s.svgRequested {
			if kind := structuredImageFailure(m); kind != "" && kind != "upstream_text_reply" {
				resultErr = mergeUpstreamImageError(resultErr, failure(kind, "svg_attachment"))
			}
			if role == "tool" && object(m["metadata"])["is_error"] == true {
				resultErr = mergeUpstreamImageError(resultErr, failure("image_tool_error", "svg_attachment"))
			}
		}
		if channel == "analysis" || text(object(m["content"])["content_type"]) == "code" {
			continue
		}
		if role == "assistant" && channel != "" && channel != "final" {
			continue
		}
		if role == "assistant" && (m["end_turn"] == true || completed(text(m["status"]))) {
			terminal = true
		}
		mapping[text(m["id"])] = map[string]any{"message": m}
	}
	if !s.svgRequested {
		return false, nil
	}
	if !terminal {
		return true, resultErr
	}
	s.svgArtifacts = editableTargets(map[string]any{"mapping": mapping}, "svg")
	if len(s.svgArtifacts) == 0 {
		if resultErr != nil {
			return true, resultErr
		}
		return true, failure("no_image_generated", "svg_attachment")
	}
	return true, nil
}

func (c *Client) resolveSVG(ctx context.Context, state *conversationState) ([]ImageAsset, error) {
	if len(state.svgArtifacts) == 0 {
		return nil, failure("no_image_generated", "svg_attachment")
	}
	assets := []ImageAsset{}
	for _, a := range state.svgArtifacts {
		// An attachment may contain an arbitrary sandbox URL. Constrain it to a
		// real file under /mnt/data before using the fixed interpreter endpoint.
		if a.SandboxPath != "" && (!strings.HasPrefix(a.SandboxPath, "/mnt/data/") || path.Clean(a.SandboxPath) != a.SandboxPath || !strings.EqualFold(path.Ext(a.SandboxPath), ".svg")) {
			return nil, failure("invalid_asset_url", "svg_attachment")
		}
		if !idPattern.MatchString(a.MessageID) || len(a.MessageID) > 128 {
			return nil, failure("invalid_asset_url", "svg_attachment")
		}
		raw, err := c.editableDownloadURL(ctx, state.ConversationID, a)
		if err != nil {
			return nil, svgAttachmentFailure(err)
		}
		if _, err = validateAssetURL(raw, false); err != nil {
			return nil, svgAttachmentFailure(err)
		}
		assets = append(assets, ImageAsset{URL: raw})
	}
	return assets, nil
}

func svgAttachmentFailure(err error) error {
	var up *Error
	if errors.As(err, &up) {
		copy := *up
		copy.Stage = "svg_attachment"
		return &copy
	}
	return &Error{Kind: "image_download_failed", Stage: "svg_attachment", cause: err}
}
