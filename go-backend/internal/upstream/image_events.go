package upstream

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Only known structured codes may cross the upstream seam. Free-form error
// bodies can contain credentials, signed URLs, or reflected input.
func structuredImageFailure(value any) string {
	aliases := map[string]string{
		"connection_failed": "upstream_connection_failed", "connection_timeout": "upstream_connection_timeout",
		"invalid_access_token": "auth_invalid", "token_invalid": "auth_invalid", "token_invalidated": "auth_invalid", "token_revoked": "auth_invalid",
		"moderation_blocked": "content_policy_violation", "safety_blocked": "content_policy_violation", "quota_exhausted": "image_quota_exhausted",
		"rate_limit_exceeded": "upstream_rate_limited", "rate_limit": "upstream_rate_limited", "rate_limited": "upstream_rate_limited", "limited": "upstream_rate_limited", "429": "upstream_rate_limited", "限流": "upstream_rate_limited", "insufficient_quota": "image_quota_exhausted", "unsupported_image_model": "unsupported_model", "upstream_timeout": "image_poll_timeout",
	}
	known := map[string]bool{"conversation_not_ready": true, "upstream_unavailable": true, "internal_error": true, "upstream_error": true, "upstream_connection_failed": true, "upstream_connection_timeout": true, "upstream_rate_limited": true, "image_poll_timeout": true, "image_stream_timeout": true, "image_stream_interrupted": true, "image_tool_error": true, "image_quota_exhausted": true, "file_upload_throttled": true, "auth_invalid": true, "content_policy_violation": true, "invalid_image_input": true, "upstream_text_reply": true, "no_image_generated": true, "unsupported_model": true, "image_download_failed": true, "task_interrupted": true, "no_available_account": true, "insufficient_quota": true}
	var found string

	var walk func(any)
	walk = func(v any) {
		switch item := v.(type) {
		case map[string]any:
			for _, key := range []string{"code", "type", "error_code", "failure_code", "error_type", "error", "status"} {
				code := strings.ToLower(strings.TrimSpace(text(item[key])))
				if a := aliases[code]; a != "" {
					code = a
				}
				if known[code] && (found == "" || upstreamImageFailurePriority(code) > upstreamImageFailurePriority(found) || (upstreamImageFailurePriority(code) == upstreamImageFailurePriority(found) && code > found)) {
					found = code
				}
			}
			for key, child := range item {
				if key != "input" && key != "prompt" {
					walk(child)
				}
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	return found
}
func (s *conversationState) pollMessage(m map[string]any) error {
	if object(m["author"]) == nil && object(m["message"]) != nil {
		m = object(m["message"])
	}
	role := strings.ToLower(strings.TrimSpace(text(object(m["author"])["role"])))
	if role != "tool" && role != "assistant" {
		return nil
	}
	s.addPointer(object(m["content"]))
	s.addPointer(object(m["metadata"]))
	return s.message(m)
}
func numeric(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case int:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}
func currentTurnMessages(doc map[string]any) []map[string]any {
	mapping := object(doc["mapping"])
	values := []map[string]any{}
	if id := text(doc["current_node"]); id != "" && object(mapping[id]) != nil {
		seen := map[string]bool{}
		for id != "" && !seen[id] {
			seen[id] = true
			node := object(mapping[id])
			if node == nil {
				break
			}
			if m := object(node["message"]); m != nil {
				values = append(values, m)
			}
			id = text(node["parent"])
		}
		for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
			values[i], values[j] = values[j], values[i]
		}
	} else {
		ids := make([]string, 0, len(mapping))
		for id := range mapping {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			a, b := object(object(mapping[ids[i]])["message"]), object(object(mapping[ids[j]])["message"])
			ta, tb := numeric(a["create_time"]), numeric(b["create_time"])
			if ta != tb {
				return ta < tb
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids {
			if m := object(object(mapping[id])["message"]); m != nil {
				values = append(values, m)
			}
		}
	}
	start := 0
	for i, m := range values {
		if text(object(m["author"])["role"]) == "user" {
			start = i + 1
		}
	}
	return values[start:]
}

func imageToolArguments(value string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(value)), &m) != nil || m == nil {
		return false
	}
	hasPrompt := strings.TrimSpace(text(m["prompt"])) != "" || strings.TrimSpace(text(m["instructions"])) != ""
	hasOptions := false
	for _, key := range []string{"size", "n", "transparent_background", "is_style_transfer", "referenced_image_ids"} {
		if _, ok := m[key]; ok {
			hasOptions = true
		}
	}
	_, size := m["size"]
	_, n := m["n"]
	return hasPrompt && hasOptions || size && n
}

func upstreamImageFailurePriority(code string) int {
	switch code {
	case "auth_invalid":
		return 8
	case "image_quota_exhausted", "insufficient_quota":
		return 7
	case "upstream_rate_limited", "file_upload_throttled":
		return 6
	case "image_download_failed":
		return 5
	case "content_policy_violation", "invalid_image_input", "unsupported_model":
		return 4
	case "image_tool_error":
		return 3
	case "upstream_text_reply":
		return 2
	case "image_poll_timeout", "image_stream_interrupted", "image_stream_timeout", "task_interrupted", "upstream_connection_failed", "upstream_connection_timeout", "upstream_unavailable":
		return 1
	}
	return 0
}

func mergeUpstreamImageError(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	x, xok := a.(*Error)
	y, yok := b.(*Error)
	if !xok || !yok {
		return a
	}
	if upstreamImageFailurePriority(y.Kind) > upstreamImageFailurePriority(x.Kind) || (upstreamImageFailurePriority(y.Kind) == upstreamImageFailurePriority(x.Kind) && y.Kind > x.Kind) {
		return b
	}
	return a
}
