package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
)

type AssetPointer struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type conversationState struct {
	ConversationID    string
	Text              string
	Pointers          []AssetPointer
	role              string
	channel           string
	status            string
	endTurnKnown      bool
	assistantTerminal bool
	toolTerminal      bool
	done              bool
	imageMode         bool
	imageArguments    bool
	contentType       string
}

var pointerPattern = regexp.MustCompile(`^(file-service|sediment)://([A-Za-z0-9_-]+)$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func text(v any) string           { s, _ := v.(string); return s }
func completed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "complete", "completed", "done", "finished", "finished_successfully", "finished_partial_completion", "success", "succeeded":
		return true
	}
	return false
}

func (s *conversationState) addPointer(value any) {
	switch v := value.(type) {
	case map[string]any:
		if pointer, ok := v["asset_pointer"].(string); ok {
			match := pointerPattern.FindStringSubmatch(pointer)
			if len(match) == 3 && !strings.HasPrefix(match[2], "file_upload") {
				p := AssetPointer{Kind: match[1], ID: match[2]}
				found := false
				for _, old := range s.Pointers {
					if old == p {
						found = true
						break
					}
				}
				if !found {
					s.Pointers = append(s.Pointers, p)
				}
			}
		}
		for _, child := range v {
			s.addPointer(child)
		}
	case []any:
		for _, child := range v {
			s.addPointer(child)
		}
	}
}

func (s *conversationState) message(m map[string]any) error {
	role := strings.ToLower(strings.TrimSpace(text(object(m["author"])["role"])))
	if s.imageMode && role == "tool" {
		s.addPointer(object(m["content"]))
		s.addPointer(object(m["metadata"]))
	}
	classifiedKind := ""
	if (role == "assistant" || role == "tool") && !(s.imageMode && len(s.Pointers) > 0) {
		classifiedKind = structuredImageFailure(m)
		if classifiedKind != "" && classifiedKind != "upstream_text_reply" {
			return failure(classifiedKind, "conversation")
		}
	}
	s.role = role
	s.channel = text(m["channel"])
	s.status = strings.ToLower(strings.TrimSpace(text(m["status"])))
	if s.status == "" {
		s.status = strings.ToLower(strings.TrimSpace(text(object(m["metadata"])["status"])))
	}
	if s.imageMode && len(s.Pointers) == 0 && (m["blocked"] == true || object(m["metadata"])["blocked"] == true) {
		return failure("content_policy_violation", "conversation")
	}
	// A tool can finish its transport successfully while reporting failed work.
	// This is structured execution evidence, not an assistant text-review result.
	if s.imageMode && role == "tool" && len(s.Pointers) == 0 && object(m["metadata"])["is_error"] == true {
		return failure("image_tool_error", "conversation")
	}
	_, s.endTurnKnown = m["end_turn"].(bool)
	if (role == "assistant" || role == "tool") && (s.status == "failed" || s.status == "error" || s.status == "cancelled") && !(s.imageMode && len(s.Pointers) > 0) {
		return failure("upstream_message_failed", "conversation")
	}
	content := object(m["content"])
	s.contentType = text(content["content_type"])
	if s.imageMode && role == "tool" && s.contentType == "system_error" && len(s.Pointers) == 0 {
		return failure("image_tool_error", "conversation")
	}
	if role == "tool" {
		s.addPointer(content)
		s.toolTerminal = completed(s.status)
	}
	// Tool and analysis-channel content must never become a user-facing answer.
	if role == "assistant" && (s.channel == "" || s.channel == "final") {
		var parts []string
		if text(content["content_type"]) == "text" || (s.imageMode && text(content["content_type"]) == "code") {
			for _, part := range slice(content["parts"]) {
				if str, ok := part.(string); ok {
					parts = append(parts, str)
				}
			}
			s.Text = strings.Join(parts, "")
			if str, ok := content["text"].(string); ok {
				s.Text = str
			}
		}
		s.assistantTerminal = m["end_turn"] == true || (!s.endTurnKnown && completed(s.status))
		if s.imageMode && s.contentType == "code" && s.assistantTerminal {
			s.imageArguments = imageToolArguments(s.Text)
		}
	}
	if classifiedKind != "" {
		return failure(classifiedKind, "conversation")
	}
	return nil
}

func hasError(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case bool:
		return v
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

func slice(v any) []any { a, _ := v.([]any); return a }

func (s *conversationState) apply(event map[string]any) error {
	if hasError(event["error"]) {
		if kind := structuredImageFailure(event); kind != "" {
			return failure(kind, "conversation")
		}
		return failure("upstream_event_error", "conversation")
	}
	if event["type"] == "moderation" && object(event["moderation_response"])["blocked"] == true {
		return failure("content_blocked", "conversation")
	}
	if id := text(event["conversation_id"]); id != "" {
		if !idPattern.MatchString(id) {
			return failure("invalid_conversation_id", "conversation")
		}
		s.ConversationID = id
	}
	if message := object(event["message"]); message != nil {
		if err := s.message(message); err != nil {
			return err
		}
	}
	if v := object(event["v"]); v != nil {
		if err := s.apply(v); err != nil {
			return err
		}
	}
	for _, child := range slice(event["v"]) {
		if m := object(child); m != nil {
			if err := s.apply(m); err != nil {
				return err
			}
		}
	}
	path, op := text(event["p"]), text(event["o"])
	value := event["v"]
	if s.imageMode && (s.role == "assistant" || s.role == "tool") && len(s.Pointers) == 0 {
		if s.role == "tool" && path == "/message/metadata/is_error" && value == true {
			return failure("image_tool_error", "conversation")
		}
		if strings.HasPrefix(path, "/message/metadata/") || path == "/message/error" || path == "/message/code" {
			key := path[strings.LastIndex(path, "/")+1:]
			if key == "blocked" && value == true {
				return failure("content_policy_violation", "conversation")
			}
			if kind := structuredImageFailure(map[string]any{key: value}); kind != "" {
				return failure(kind, "conversation")
			}
		}
	}
	if path == "/message/content/content_type" {
		s.contentType = text(value)
	}
	if path == "/message" && object(value) != nil {
		if err := s.message(object(value)); err != nil {
			return err
		}
	}
	if path == "/message/author/role" {
		s.role = text(value)
	}
	if path == "/message/channel" {
		s.channel = text(value)
	}
	if path == "/message/end_turn" {
		_, s.endTurnKnown = value.(bool)
	}
	if path == "/message/status" && (s.role == "assistant" || s.role == "tool") && (value == "failed" || value == "error" || value == "cancelled") {
		return failure("upstream_message_failed", "conversation")
	}
	if s.role == "assistant" && (s.channel == "" || s.channel == "final") {
		if path == "/message/content/parts/0" || path == "/message/content/text" {
			switch op {
			case "append":
				s.Text += text(value)
			case "replace":
				s.Text = text(value)
			}
		}
		if path == "" && op == "" {
			if v, ok := value.(string); ok {
				s.Text += v
			}
		}
		if path == "/message/end_turn" && value == true {
			s.assistantTerminal = true
		}
		if path == "/message/status" && !s.endTurnKnown && completed(text(value)) {
			s.assistantTerminal = true
		}
		if s.imageMode && s.contentType == "code" && s.assistantTerminal {
			s.imageArguments = imageToolArguments(s.Text)
		}
	}
	if s.role == "tool" && strings.HasPrefix(path, "/message/content/") {
		if strings.HasSuffix(path, "/asset_pointer") {
			s.addPointer(map[string]any{"asset_pointer": value})
		} else {
			s.addPointer(value)
		}
	}
	return nil
}

func (s *conversationState) consume(event Event) error {
	if event.Data == "[DONE]" {
		s.done = true
		return errStreamDone
	}
	if event.Data == "v1" || event.Data == `"v1"` {
		return nil
	}
	if event.Name == "error" {
		return failure("upstream_event_error", "conversation")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(event.Data), &value); err != nil || value == nil {
		return failure("invalid_sse_json", "conversation")
	}
	return s.apply(value)
}
