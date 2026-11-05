package upstream

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strings"
)

var svgOutputRequest = regexp.MustCompile(`(?i)(\b(create|generate|draw|make|produce|deliver|return|provide|output|export|save)\s+((a|an|one|the|actual|standalone|downloadable|editable|vector|image|file|icon|logo|as|in|format|only|final|finished|native)\s+){0,10}\.?svg\b|\b(output|deliverable|file)\s+format\s*[:=]\s*svg\b|(生成|输出|导出|保存|提供|返回|制作|绘制)(为|成|一个|一张|一份|可下载的|独立的|真正的|可编辑的|矢量|图片|图标|文件|格式|的|[:：=]|\s){0,16}\.?svg\b)`)
var svgOutputNegation = regexp.MustCompile(`(?i)(\b(no|not|without)\s+((a|an|any|the)\s+)?\.?svg\b|\b(do not|don't|never|avoid)\s+[^,;.!?\n]{0,60}\bsvg\b|(不要|不需要|禁止|无需|别)(生成|输出|导出|提供|返回|使用|用|任何|的|\s){0,8}\.?svg\b)`)
var svgQuotedText = regexp.MustCompile(`"[^"\n]*"|“[^”]*”|‘[^’]*’|「[^」]*」|『[^』]*』|(^|[\s(:])'[^'\n]*'` + "|`[^`\\n]*`")
var svgOutputSuffix = regexp.MustCompile(`(?i)^($|[.,;:!?。；，：！？\n]|(files?|images?|icons?|logos?|vectors?|documents?|graphics?|attachments?|only)\b|文件|图片|图标|矢量|格式|图($|\s|[。！？]))`)

// Only an explicit output instruction delays raster delivery. Quoted artwork
// copy and descriptions of SVG capabilities do not select an attachment format.
// Real current-turn SVG attachments are also discovered independently below.
func wantsSVG(prompt string) bool {
	instruction := svgQuotedText.ReplaceAllStringFunc(prompt, func(quoted string) string {
		value := strings.Trim(quoted, " \t\r\n('`\"“”‘’「」『』")
		if strings.EqualFold(value, "svg") || strings.EqualFold(value, ".svg") {
			return " " + value + " "
		}
		return " "
	})
	if svgOutputNegation.MatchString(instruction) {
		return false
	}
	for _, match := range svgOutputRequest.FindAllStringIndex(instruction, -1) {
		if svgOutputSuffix.MatchString(strings.TrimSpace(instruction[match[1]:])) {
			return true
		}
	}
	return false
}

func (s *conversationState) hasImageResult() bool {
	if len(s.svgArtifacts) > 0 {
		return true
	}
	if s.svgRequested {
		return false
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
	if !terminal {
		return s.svgRequested, resultErr
	}
	s.svgArtifacts = editableTargets(map[string]any{"mapping": mapping}, "svg")
	if len(s.svgArtifacts) > 0 {
		return true, nil
	}
	if !s.svgRequested {
		return false, nil
	}
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
