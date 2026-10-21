package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"
)

const publicXMLToolRule = "Tool output adapter: when calling tools, output ONLY this XML and no prose/markdown:\n<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>"
const publicToolRules = "Tool use rules:\n- If the user asks to list/read/search files, inspect project state, run a command, or answer from local code, you MUST call a suitable tool first. Do not say you cannot access files.\n- To call tools, output ONLY XML and no prose/markdown:\n<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>\n- Put parameters under <parameters> using the exact schema names."

func publicJSON(v any) string {
	var buffer bytes.Buffer
	e := json.NewEncoder(&buffer)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	data := strings.TrimSuffix(buffer.String(), "\n")
	var out strings.Builder
	quoted, escaped := false, false
	for _, r := range data {
		out.WriteRune(r)
		if quoted {
			if escaped {
				escaped = false
			} else if r == '\\' {
				escaped = true
			} else if r == '"' {
				quoted = false
			}
		} else if r == '"' {
			quoted = true
		} else if r == ',' || r == ':' {
			out.WriteByte(' ')
		}
	}
	return out.String()
}
func publicToolPrompt(tools any) string {
	blocks := []string{}
	for _, tool := range dicts(tools) {
		fn := object(tool["function"])
		name := text(defaultValue(tool["name"], fn["name"]))
		if name == "" {
			continue
		}
		desc := text(defaultValue(tool["description"], fn["description"]))
		schema := tool["input_schema"]
		if !truth(schema) {
			schema = tool["parameters"]
		}
		if !truth(schema) {
			schema = fn["input_schema"]
		}
		if !truth(schema) {
			schema = fn["parameters"]
		}
		if !truth(schema) {
			schema = Map{}
		}
		blocks = append(blocks, "Tool: "+name+"\nDescription: "+desc+"\nParameters: "+publicJSON(schema))
	}
	if len(blocks) == 0 {
		return ""
	}
	return "Available tools:\n" + strings.Join(blocks, "\n") + publicToolRules
}
func publicMergeSystem(system any, extra string) any {
	claudeCode := strings.Contains(publicString(system), "You are Claude Code")
	for _, p := range dicts(system) {
		if strings.Contains(publicString(p["text"]), "You are Claude Code") {
			claudeCode = true
		}
	}
	if claudeCode {
		extra = publicXMLToolRule
	}
	if extra == "" {
		return system
	}
	if value, ok := system.(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value) + "\n\n" + extra
	}
	if _, ok := system.([]any); ok {
		return append(append([]any{}, list(system)...), Map{"type": "text", "text": extra})
	}
	return extra
}
func publicPreprocessAnthropic(body Map) ([]Map, any) {
	messages := []Map{}
	for _, m := range dicts(body["messages"]) {
		out := Map{}
		for k, v := range m {
			out[k] = v
		}
		if parts, ok := m["content"].([]any); ok {
			next := []any{}
			for _, v := range parts {
				p, ok := v.(map[string]any)
				if !ok {
					next = append(next, v)
					continue
				}
				switch text(p["type"]) {
				case "tool_use":
					next = append(next, Map{"type": "text", "text": "<tool_calls><tool_call><tool_name>" + publicString(p["name"]) + "</tool_name><parameters>" + publicJSON(defaultValue(p["input"], Map{})) + "</parameters></tool_call></tool_calls>"})
				case "tool_result":
					next = append(next, Map{"type": "text", "text": "Tool result " + publicString(p["tool_use_id"]) + ": " + publicString(p["content"])})
				default:
					next = append(next, p)
				}
			}
			out["content"] = next
		}
		messages = append(messages, out)
	}
	return messages, publicMergeSystem(body["system"], publicToolPrompt(body["tools"]))
}

var publicToolBlocks = regexp.MustCompile(`(?is)<(?:tool_call|function_call|invoke)\b[^>]*>(.*?)</(?:tool_call|function_call|invoke)>`)
var publicToolMarkup = regexp.MustCompile(`(?is)<tool_calls\b[^>]*>.*?</tool_calls>|<tool_call\b[^>]*>.*?</tool_call>|<function_call\b[^>]*>.*?</function_call>|<invoke\b[^>]*>.*?</invoke>`)
var publicToolOpening = regexp.MustCompile(`(?is)<tool_calls\b|<tool_call\b|<function_call\b|<invoke\b`)
var publicCodeFence = regexp.MustCompile("(?is)```.*?```")
var publicToolParameter = regexp.MustCompile(`(?is)<([\w.-]+)\b[^>]*>(.*?)</[\w.-]+>`)

func publicXMLValue(raw, tag string) string {
	pattern := regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(tag) + `\b[^>]*>(.*?)</` + regexp.QuoteMeta(tag) + `>`)
	m := pattern.FindStringSubmatch(raw)
	if len(m) == 0 {
		return ""
	}
	v := strings.TrimSpace(m[1])
	if strings.HasPrefix(v, "<![CDATA[") && strings.HasSuffix(v, "]]> ") {
		v = strings.TrimSuffix(strings.TrimPrefix(v, "<![CDATA["), "]]> ")
	}
	if strings.HasPrefix(v, "<![CDATA[") && strings.HasSuffix(v, "]]>") {
		v = strings.TrimSuffix(strings.TrimPrefix(v, "<![CDATA["), "]]>")
	}
	return strings.TrimSpace(html.UnescapeString(v))
}
func publicToolValue(raw string) any {
	v := publicXMLValue("<x>"+raw+"</x>", "x")
	d := json.NewDecoder(strings.NewReader(v))
	d.UseNumber()
	var value any
	if d.Decode(&value) == nil {
		return value
	}
	return v
}
func publicToolParams(raw string) Map {
	raw = strings.TrimSpace(raw)
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var value any
	if d.Decode(&value) == nil {
		return object(value)
	}
	out := Map{}
	for _, m := range publicToolParameter.FindAllStringSubmatch(raw, -1) {
		out[m[1]] = publicToolValue(m[2])
	}
	return out
}

type publicToolCall struct {
	name  string
	input Map
}

func publicParseTools(raw string) []publicToolCall {
	raw = publicCodeFence.ReplaceAllString(raw, "")
	out := []publicToolCall{}
	for _, m := range publicToolBlocks.FindAllStringSubmatch(raw, -1) {
		name := ""
		for _, tag := range []string{"tool_name", "name", "function"} {
			if name = publicXMLValue(m[1], tag); name != "" {
				break
			}
		}
		params := "{}"
		for _, tag := range []string{"parameters", "input", "arguments"} {
			if value := publicXMLValue(m[1], tag); value != "" {
				params = value
				break
			}
		}
		if name != "" {
			out = append(out, publicToolCall{name, publicToolParams(params)})
		}
	}
	return out
}
func publicUUID() string {
	value := id(16)
	return value[:8] + "-" + value[8:12] + "-4" + value[13:16] + "-8" + value[17:20] + "-" + value[20:]
}
func publicAnthropicContent(raw string, tools any) ([]Map, string) {
	calls := []publicToolCall{}
	if len(list(tools)) > 0 {
		calls = publicParseTools(raw)
	}
	value := strings.TrimSpace(publicToolMarkup.ReplaceAllString(raw, ""))
	blocks := []Map{}
	if len(calls) > 0 {
		if value != "" {
			blocks = append(blocks, Map{"type": "text", "text": value})
		}
		for _, call := range calls {
			blocks = append(blocks, Map{"type": "tool_use", "id": "toolu_" + publicUUID(), "name": call.name, "input": call.input})
		}
		return blocks, "tool_use"
	}
	return []Map{{"type": "text", "text": value}}, "end_turn"
}
func (s *Server) publicMessages(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
	raw, system := publicPreprocessAnthropic(body)
	messages, e := s.normalizePublicMessages(ctx, raw, system)
	if e != nil {
		return nil, e
	}
	model := publicModel(body, "auto")
	it, ii := countPublicMessages(messages, model)
	if truth(body["stream"]) {
		return &PublicStream{Protocol: "anthropic", Produce: func(ctx context.Context, emit func(Map) error) error {
			return s.publicAnthropicEvents(ctx, messages, model, it+ii, body["tools"], emit)
		}}, nil
	}
	full := ""
	if e = s.publicTextDeltas(ctx, messages, model, "", func(delta string) error { full += delta; return nil }); e != nil {
		return nil, e
	}
	content, reason := publicAnthropicContent(full, body["tools"])
	return Map{"id": "msg_" + publicUUID(), "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": reason, "stop_sequence": nil, "usage": Map{"input_tokens": it + ii, "output_tokens": countPublicTextTokens(full, model)}}, nil
}
func (s *Server) publicAnthropicEvents(ctx context.Context, messages []publicMessage, model string, inputTokens int, tools any, emit func(Map) error) error {
	messageID, created := "msg_"+publicUUID(), s.now().Unix()
	if e := emit(Map{"type": "message_start", "message": Map{"id": messageID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": Map{"input_tokens": inputTokens, "output_tokens": 0}}}); e != nil {
		return e
	}
	toolMode := len(list(tools)) > 0
	textOpen := !toolMode
	startText := func(index int) error {
		return emit(Map{"type": "content_block_start", "index": index, "content_block": Map{"type": "text", "text": ""}})
	}
	stopBlock := func(index int) error { return emit(Map{"type": "content_block_stop", "index": index}) }
	textDelta := func(index int, delta string) error {
		return emit(Map{"type": "content_block_delta", "index": index, "delta": Map{"type": "text_delta", "text": delta}})
	}
	if textOpen {
		if e := startText(0); e != nil {
			return e
		}
	}
	full, streamed := "", ""
	toolStarted := false
	e := s.publicTextDeltas(ctx, messages, model, "", func(delta string) error {
		full += delta
		if toolStarted {
			return nil
		}
		visible := full
		if toolMode {
			if match := publicToolOpening.FindStringIndex(full); match != nil {
				visible = strings.TrimRight(full[:match[0]], " \t\r\n")
			}
		}
		if strings.HasPrefix(visible, streamed) {
			next := visible[len(streamed):]
			if next != "" {
				if !textOpen {
					textOpen = true
					if e := startText(0); e != nil {
						return e
					}
				}
				streamed = visible
				if e := textDelta(0, next); e != nil {
					return e
				}
			}
		}
		toolStarted = toolMode && visible != full
		return nil
	})
	if e != nil {
		return e
	}
	blocks, reason := publicAnthropicContent(full, tools)
	if textOpen {
		if e = stopBlock(0); e != nil {
			return e
		}
	}
	if reason == "tool_use" {
		index := 0
		if textOpen {
			index = 1
		}
		if len(blocks) > 0 && blocks[0]["type"] == "text" {
			remaining := publicString(blocks[0]["text"])
			if len(remaining) >= len(streamed) {
				remaining = remaining[len(streamed):]
			} else {
				remaining = ""
			}
			if remaining != "" {
				if !textOpen {
					if e = startText(0); e != nil {
						return e
					}
				}
				if e = textDelta(0, remaining); e != nil {
					return e
				}
				if !textOpen {
					if e = stopBlock(0); e != nil {
						return e
					}
				}
			}
			index = 1
			blocks = blocks[1:]
		}
		for _, block := range blocks {
			if e = emit(Map{"type": "content_block_start", "index": index, "content_block": Map{"type": "tool_use", "id": block["id"], "name": block["name"], "input": Map{}}}); e != nil {
				return e
			}
			if e = emit(Map{"type": "content_block_delta", "index": index, "delta": Map{"type": "input_json_delta", "partial_json": publicJSON(block["input"])}}); e != nil {
				return e
			}
			if e = stopBlock(index); e != nil {
				return e
			}
			index++
		}
	}
	if e = emit(Map{"type": "message_delta", "delta": Map{"stop_reason": reason, "stop_sequence": nil}, "usage": Map{"output_tokens": countPublicTextTokens(full, model)}}); e != nil {
		return e
	}
	return emit(Map{"type": "message_stop", "created": created})
}
