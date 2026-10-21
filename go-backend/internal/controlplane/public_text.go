package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const publicToolUnavailable = "This compatibility backend cannot execute local tools, shell commands, non-search tools, or file operations. Do not claim to have run tools or inspected external resources. If a user asks you to use a tool, say that tool execution is unavailable through this backend."

type publicMessage struct {
	Role, Content string
	Images        []upstream.ImageInput
}

func publicString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if !truth(v) {
		return ""
	}
	switch v.(type) {
	case map[string]any, []any, []Map, []string:
		return publicPythonRepr(v)
	}
	return text(v)
}
func publicPythonRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		quote := byte('\'')
		if strings.Contains(x, "'") && !strings.Contains(x, "\"") {
			quote = '"'
		}
		var b strings.Builder
		b.WriteByte(quote)
		for _, r := range x {
			switch r {
			case '\\':
				b.WriteString("\\\\")
			case '\n':
				b.WriteString("\\n")
			case '\r':
				b.WriteString("\\r")
			case '\t':
				b.WriteString("\\t")
			default:
				if r == rune(quote) {
					b.WriteByte('\\')
					b.WriteRune(r)
				} else if r < 32 || r == 127 {
					b.WriteString(fmt.Sprintf("\\x%02x", r))
				} else {
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte(quote)
		return b.String()
	case map[string]any:
		keys := []string{}
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, k := range keys {
			parts = append(parts, publicPythonRepr(k)+": "+publicPythonRepr(x[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any, []Map, []string:
		parts := []string{}
		for _, item := range list(x) {
			parts = append(parts, publicPythonRepr(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return text(x)
	}
}
func publicMessageText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b strings.Builder
	for _, part := range list(v) {
		if s, ok := part.(string); ok {
			b.WriteString(s)
			continue
		}
		p := object(part)
		switch text(p["type"]) {
		case "text", "input_text", "output_text":
			b.WriteString(publicString(p["text"]))
		}
	}
	return b.String()
}
func publicEffort(body Map) string {
	var v any
	var ok bool
	if v, ok = body["thinking_effort"]; !ok {
		if v, ok = body["reasoning_effort"]; !ok {
			v = object(body["reasoning"])["effort"]
		}
	}
	switch strings.ToLower(text(v)) {
	case "low", "medium", "high":
		return strings.ToLower(text(v))
	case "xhigh", "extended":
		return "extended"
	}
	return ""
}
func (s *Server) normalizePublicMessages(ctx context.Context, messages []Map, system any) ([]publicMessage, error) {
	values, e := s.Settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	out := []publicMessage{}
	if global := publicString(values["global_system_prompt"]); global != "" {
		out = append(out, publicMessage{Role: "system", Content: global})
	}
	if systemText := publicMessageText(system); systemText != "" {
		out = append(out, publicMessage{Role: "system", Content: systemText})
	}
	for _, m := range messages {
		role := "user"
		if r, ok := m["role"]; ok {
			role = publicString(r)
		}
		message := publicMessage{Role: role, Content: publicMessageText(m["content"])}
		if role == "user" {
			parts := []any{}
			for _, part := range list(m["content"]) {
				p := object(part)
				switch text(p["type"]) {
				case "image_url", "input_image", "image":
					parts = append(parts, p)
				}
			}
			if len(parts) > 0 {
				message.Images, e = s.readImageReferences(ctx, parts)
				if e != nil {
					return nil, e
				}
			}
		}
		out = append(out, message)
	}
	return out, nil
}
func (s *Server) normalizePublicHistory(ctx context.Context, messages []publicMessage) ([]publicMessage, error) {
	raw, e := s.DB.Document(ctx, "system_settings")
	if e != nil {
		return nil, e
	}
	settings := object(raw["chat_completion_cache"])
	if !boolean(settings["normalize_messages"], true) {
		return messages, nil
	}
	out := []publicMessage{}
	previous := ""
	for _, m := range messages {
		if boolean(settings["drop_assistant_history"], false) && m.Role == "assistant" {
			continue
		}
		signature := publicMessageSignature(m)
		if boolean(settings["drop_adjacent_duplicates"], true) && signature == previous {
			continue
		}
		out = append(out, m)
		previous = signature
	}
	return out, nil
}
func publicMessageSignature(m publicMessage) string {
	images := []Map{}
	for _, im := range m.Images {
		images = append(images, Map{"sha256": digest(string(im.Data)), "length": len(im.Data), "mime": im.MIME})
	}
	return canonical(Map{"role": m.Role, "content": m.Content, "images": images})
}
func hasPublicUnsupportedTools(body Map, allowed map[string]bool) bool {
	for _, t := range dicts(body["tools"]) {
		if !allowed[text(t["type"])] {
			return true
		}
	}
	return false
}

var publicSearchTypes = map[string]bool{"web_search": true, "web_search_preview": true, "web_search_preview_2025_03_11": true}

func hasPublicSearch(body Map) bool {
	switch body["tools"].(type) {
	case []any, []Map, []string:
		for _, tool := range dicts(body["tools"]) {
			if publicSearchTypes[text(tool["type"])] {
				return true
			}
		}
		return false
	}
	return publicSearchTypes[text(object(body["tool_choice"])["type"])]
}
func isPublicSearchChat(body Map) bool {
	if hasPublicSearch(body) {
		return true
	}
	if _, ok := body["web_search_options"].(map[string]any); ok {
		return true
	}
	model := text(body["model"])
	for _, prefix := range []string{"gpt-4o-search-preview", "gpt-4o-mini-search-preview", "gpt-5-search-api"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}
func isPublicImageChat(body Map) bool {
	model := strings.ToLower(text(body["model"]))
	for _, valid := range append(append([]string{}, webImageModels...), "codex-gpt-image-2", "plus-codex-gpt-image-2", "team-codex-gpt-image-2", "pro-codex-gpt-image-2") {
		if model == valid {
			return true
		}
	}
	for _, v := range stringsOf(body["modalities"]) {
		if strings.EqualFold(v, "image") {
			return true
		}
	}
	return false
}
func isPublicImageResponse(body Map) bool {
	for _, t := range dicts(body["tools"]) {
		if text(t["type"]) == "image_generation" {
			return true
		}
	}
	return text(object(body["tool_choice"])["type"]) == "image_generation"
}
func publicChatMessages(body Map) ([]Map, error) {
	if messages := dicts(body["messages"]); len(messages) > 0 {
		return messages, nil
	}
	if prompt := text(body["prompt"]); prompt != "" {
		return []Map{{"role": "user", "content": prompt}}, nil
	}
	return nil, problem(400, "messages or prompt is required")
}
func responsePart(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	t := text(m["type"])
	switch t {
	case "text", "input_text", "output_text", "image_url", "input_image", "image":
		return true
	}
	_, image := m["image_url"]
	return image && t != "message"
}
func publicResponseMessages(input, instructions any) []Map {
	out := []Map{}
	appendMessage := func(role string, content any) {
		if s, ok := content.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, Map{"role": role, "content": s})
			}
		} else if len(list(content)) > 0 {
			out = append(out, Map{"role": role, "content": content})
		}
	}
	if system := strings.TrimSpace(publicString(instructions)); system != "" {
		appendMessage("system", system)
	}
	if s, ok := input.(string); ok {
		appendMessage("user", s)
		return out
	}
	if m, ok := input.(map[string]any); ok {
		if responsePart(m) {
			appendMessage("user", []Map{m})
		} else {
			appendMessage(text(defaultValue(m["role"], "user")), m["content"])
		}
		return out
	}
	parts := []any{}
	for _, v := range list(input) {
		if responsePart(v) {
			parts = append(parts, v)
			continue
		}
		if len(parts) > 0 {
			appendMessage("user", parts)
			parts = []any{}
		}
		if m, ok := v.(map[string]any); ok {
			appendMessage(text(defaultValue(m["role"], "user")), m["content"])
		}
	}
	if len(parts) > 0 {
		appendMessage("user", parts)
	}
	return out
}

var publicAnnotation = regexp.MustCompile("\ue200([^\ue201]*)\ue201")
var publicIncompleteAnnotation = regexp.MustCompile("\ue200[^\ue201]*$")
var publicPunctuationSpace = regexp.MustCompile(`\s+([.,;:!?])`)

func sanitizePublicText(value string) string {
	value = publicAnnotation.ReplaceAllStringFunc(value, func(marker string) string {
		payload := strings.TrimSuffix(strings.TrimPrefix(marker, "\ue200"), "\ue201")
		parts := strings.Split(payload, "\ue202")
		kind := strings.ToLower(parts[0])
		data := parts[1:]
		if kind == "url" {
			label, url := "", ""
			if len(data) > 0 {
				label = strings.TrimSpace(data[0])
			}
			if len(data) > 1 {
				url = strings.TrimSpace(data[1])
			}
			if label != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
				return label + " (" + url + ")"
			}
			if label != "" {
				return label
			}
			return url
		}
		for _, v := range data {
			v = strings.TrimSpace(v)
			lower := strings.ToLower(v)
			if v != "" && !strings.HasPrefix(lower, "turn") && !strings.HasPrefix(lower, "source") {
				return v
			}
		}
		return ""
	})
	value = publicIncompleteAnnotation.ReplaceAllString(value, "")
	return publicPunctuationSpace.ReplaceAllString(value, "$1")
}
func (s *Server) publicTextDeltas(ctx context.Context, messages []publicMessage, model, effort string, emit func(string) error) error {
	values, e := s.Settings.Values(ctx)
	if e != nil {
		return e
	}
	converted := []upstream.Message{}
	if global := publicString(values["global_system_prompt"]); global != "" {
		converted = append(converted, upstream.Message{Role: "system", Content: global})
	}
	history, historyMessages := "", []string{}
	for _, m := range messages {
		converted = append(converted, upstream.Message{Role: m.Role, Content: m.Content, Images: m.Images})
		if m.Role == "assistant" {
			history += m.Content
			if m.Content != "" {
				historyMessages = append(historyMessages, m.Content)
			}
		}
	}
	if len(converted) == 0 {
		converted = append(converted, upstream.Message{Role: "user", Content: ""})
	}
	previous := ""
	historyIndex := 0
	_, e = s.Text.Stream(ctx, converted, model, effort, func(snapshot string) error {
		for history != "" && strings.HasPrefix(snapshot, history) {
			snapshot = strings.TrimPrefix(snapshot, history)
		}
		if historyIndex < len(historyMessages) && snapshot == historyMessages[historyIndex] {
			historyIndex++
			previous = ""
			return nil
		}
		next := sanitizePublicText(snapshot)
		if next == previous {
			return nil
		}
		delta := next
		if strings.HasPrefix(next, previous) {
			delta = next[len(previous):]
		}
		previous = next
		if delta != "" {
			return emit(delta)
		}
		return nil
	})
	return e
}

type publicCacheEntry struct {
	expires time.Time
	events  []Map
}
type publicInflight struct {
	done   chan struct{}
	events []Map
	err    error
}
type publicTextState struct {
	mu       sync.Mutex
	entries  map[string]publicCacheEntry
	inflight map[string]*publicInflight
}

func newPublicTextState() *publicTextState {
	return &publicTextState{entries: map[string]publicCacheEntry{}, inflight: map[string]*publicInflight{}}
}
func publicCacheKey(protocol string, body Map, messages []publicMessage, stream bool) string {
	keys := []string{"frequency_penalty", "max_completion_tokens", "max_tokens", "metadata", "model", "presence_penalty", "reasoning", "reasoning_effort", "response_format", "seed", "stop", "temperature", "tool_choice", "tools", "top_p", "thinking_effort", "user"}
	m := Map{"protocol": protocol, "stream": stream}
	for _, key := range keys {
		if v, ok := body[key]; ok {
			m[key] = v
		}
	}
	signatures := []string{}
	for _, message := range messages {
		signatures = append(signatures, publicMessageSignature(message))
	}
	m["messages"] = signatures
	return digest(canonical(m))
}
func (s *Server) cachedPublicEvents(ctx context.Context, protocol string, body Map, messages []publicMessage, stream bool, compute func(func(Map) error) error, emit func(Map) error) error {
	raw, e := s.DB.Document(ctx, "system_settings")
	if e != nil {
		return e
	}
	settings := object(raw["chat_completion_cache"])
	ttl := max(0, integer(settings["ttl_seconds"], 60))
	if !boolean(settings["enabled"], true) || ttl == 0 || (stream && !boolean(settings["stream_cache"], true)) {
		return compute(emit)
	}
	state := s.publicText
	key := publicCacheKey(protocol, body, messages, stream)
	capacity := max(1, integer(settings["max_entries"], 256))
	if s.textCache != nil {
		if events, hit, err := s.textCache.Get(ctx, key); err == nil && hit {
			for _, event := range events {
				if err := emit(copyMap(event)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	now := s.now()
	state.mu.Lock()
	for k, v := range state.entries {
		if !v.expires.After(now) {
			delete(state.entries, k)
		}
	}
	if entry, ok := state.entries[key]; ok {
		events := entry.events
		state.mu.Unlock()
		for _, v := range events {
			if e := emit(copyMap(v)); e != nil {
				return e
			}
		}
		return nil
	}
	if call := state.inflight[key]; call != nil && boolean(settings["dedupe_inflight"], true) {
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
		}
		if call.err != nil {
			return call.err
		}
		for _, v := range call.events {
			if e := emit(copyMap(v)); e != nil {
				return e
			}
		}
		return nil
	}
	call := &publicInflight{done: make(chan struct{})}
	if len(state.inflight) >= 1024 {
		state.mu.Unlock()
		return problem(503, "text request capacity is full")
	}
	dedupe := boolean(settings["dedupe_inflight"], true)
	if dedupe {
		state.inflight[key] = call
	}
	state.mu.Unlock()
	events := []Map{}
	bytesStored := 0
	cacheable := true
	e = compute(func(v Map) error {
		clean := object(stripPublicInternal(v))
		encoded, _ := json.Marshal(clean)
		bytesStored += len(encoded)
		if bytesStored > 32<<20 {
			cacheable = false
		}
		if cacheable {
			events = append(events, copyMap(clean))
		}
		return emit(v)
	})
	if e == nil && cacheable && s.textCache != nil {
		// Redis is an optional accelerator. An outage cannot replace native
		// execution results with a cache error or hold the singleflight lock.
		_ = s.textCache.Put(ctx, key, events, time.Duration(ttl)*time.Second, capacity)
	}
	state.mu.Lock()
	if dedupe {
		delete(state.inflight, key)
	}
	call.err = e
	if e == nil && cacheable {
		call.events = events
		if s.textCache == nil {
			state.entries[key] = publicCacheEntry{s.now().Add(time.Duration(ttl) * time.Second), events}
		}
		for len(state.entries) > capacity {
			oldest := ""
			var when time.Time
			for k, v := range state.entries {
				if oldest == "" || v.expires.Before(when) {
					oldest, when = k, v.expires
				}
			}
			delete(state.entries, oldest)
		}
	} else if e == nil {
		call.err = problem(502, "text response exceeds cache limits")
	}
	close(call.done)
	state.mu.Unlock()
	return e
}
func (s *Server) registerPublicText() {
	s.publicText = newPublicTextState()
	s.publicRoute("GET", "/v1/models", "identity", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		catalog, e := s.modelCatalog(ctx)
		if e != nil {
			return nil, e
		}
		items := []Map{}
		for _, model := range stringsOf(catalog["all_models"]) {
			items = append(items, Map{"id": model, "object": "model", "created": 0, "owned_by": "chatgpt2api", "permission": []any{}, "root": model, "parent": nil})
		}
		return Map{"object": "list", "data": items}, nil
	})
	s.publicRoute("POST", "/v1/chat/completions", "identity", "ChatCompletionRequest", s.publicChat)
	s.publicRoute("POST", "/v1/responses", "identity", "ResponseCreateRequest", s.publicResponses)
	s.publicRoute("POST", "/v1/messages", "identity", "AnthropicMessageRequest", s.publicMessages)
	s.publicRoute("POST", "/v1/search", "identity", "SearchRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
		return s.Text.Search(ctx, publicString(body["prompt"]))
	})
}
