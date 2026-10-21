package controlplane

import (
	"context"
	"errors"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/http"
	"strings"
	"unicode/utf8"
)

func publicModel(body Map, def string) string {
	if model := text(body["model"]); model != "" {
		return model
	}
	return def
}
func publicChatChunk(model, completionID string, created int64, delta Map, finish any) Map {
	return Map{"id": completionID, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []Map{{"index": 0, "delta": delta, "finish_reason": finish}}}
}
func (s *Server) publicChatResponse(model, content string, messages []publicMessage) Map {
	inputText, inputImages := 0, 0
	output := 0
	if messages != nil {
		inputText, inputImages = countPublicMessages(messages, model)
		output = countPublicTextTokens(content, model)
	}
	return Map{"id": "chatcmpl-" + id(16), "object": "chat.completion", "created": s.now().Unix(), "model": model, "choices": []Map{{"index": 0, "message": Map{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": publicChatUsage(inputText, inputImages, output, 0)}
}
func (s *Server) streamPublicChat(ctx context.Context, messages []publicMessage, model, effort string, emit func(Map) error) error {
	completionID, created := "chatcmpl-"+id(16), s.now().Unix()
	sent := false
	e := s.publicTextDeltas(ctx, messages, model, effort, func(delta string) error {
		d := Map{"content": delta}
		if !sent {
			sent = true
			d["role"] = "assistant"
		}
		return emit(publicChatChunk(model, completionID, created, d, nil))
	})
	if e != nil {
		return e
	}
	if !sent {
		if e = emit(publicChatChunk(model, completionID, created, Map{"role": "assistant", "content": ""}, nil)); e != nil {
			return e
		}
	}
	return emit(publicChatChunk(model, completionID, created, Map{}, "stop"))
}
func (s *Server) publicChat(ctx context.Context, r *http.Request, body Map, _ Map) (any, error) {
	body["base_url"] = s.resolveImageBaseURL(r)
	if isPublicImageChat(body) {
		return s.publicImageChat(ctx, body)
	}
	raw, e := publicChatMessages(body)
	if e != nil {
		return nil, e
	}
	messages, e := s.normalizePublicMessages(ctx, raw, nil)
	if e != nil {
		return nil, e
	}
	messages, e = s.normalizePublicHistory(ctx, messages)
	if e != nil {
		return nil, e
	}
	unsupported := hasPublicUnsupportedTools(body, publicSearchTypes)
	if unsupported {
		messages = append([]publicMessage{{Role: "system", Content: publicToolUnavailable}}, messages...)
	}
	model := publicModel(body, "auto")
	stream := truth(body["stream"])
	if isPublicSearchChat(body) && !unsupported {
		return s.publicSearchChat(ctx, body, messages, model, stream)
	}
	compute := func(emit func(Map) error) error {
		if stream {
			return s.streamPublicChat(ctx, messages, model, publicEffort(body), emit)
		}
		content := ""
		e := s.publicTextDeltas(ctx, messages, model, publicEffort(body), func(delta string) error { content += delta; return nil })
		if e != nil {
			return e
		}
		return emit(s.publicChatResponse(model, content, messages))
	}
	if stream {
		return &PublicStream{Protocol: "openai", Produce: func(ctx context.Context, emit func(Map) error) error {
			return s.cachedPublicEvents(ctx, "chat", body, messages, true, compute, emit)
		}}, nil
	}
	var out Map
	e = s.cachedPublicEvents(ctx, "chat", body, messages, false, compute, func(v Map) error { out = v; return nil })
	return out, e
}
func publicResponseItem(value, itemID, status string, annotations []Map) Map {
	if annotations == nil {
		annotations = []Map{}
	}
	return Map{"id": itemID, "type": "message", "status": status, "role": "assistant", "content": []Map{{"type": "output_text", "text": value, "annotations": annotations}}}
}
func publicResponseCreated(responseID, model string, created int64) Map {
	return Map{"type": "response.created", "response": Map{"id": responseID, "object": "response", "created_at": created, "status": "in_progress", "error": nil, "incomplete_details": nil, "model": model, "output": []any{}, "parallel_tool_calls": false}}
}
func publicResponseCompleted(responseID, model string, created int64, items []Map, usage Map) Map {
	response := Map{"id": responseID, "object": "response", "created_at": created, "status": "completed", "error": nil, "incomplete_details": nil, "model": model, "output": items, "parallel_tool_calls": false}
	if len(usage) > 0 {
		response["usage"] = usage
	}
	return Map{"type": "response.completed", "response": response}
}
func (s *Server) publicResponseEvents(ctx context.Context, body Map, messages []publicMessage, emit func(Map) error) error {
	model := publicModel(body, "auto")
	responseID, itemID, created := "resp_"+id(16), "msg_"+id(16), s.now().Unix()
	if e := emit(publicResponseCreated(responseID, model, created)); e != nil {
		return e
	}
	if e := emit(Map{"type": "response.output_item.added", "output_index": 0, "item": publicResponseItem("", itemID, "in_progress", nil)}); e != nil {
		return e
	}
	full := ""
	if e := s.publicTextDeltas(ctx, messages, model, publicEffort(body), func(delta string) error {
		full += delta
		return emit(Map{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta})
	}); e != nil {
		return e
	}
	if e := emit(Map{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": full}); e != nil {
		return e
	}
	item := publicResponseItem(full, itemID, "completed", nil)
	if e := emit(Map{"type": "response.output_item.done", "output_index": 0, "item": item}); e != nil {
		return e
	}
	it, ii := countPublicMessages(messages, model)
	return emit(publicResponseCompleted(responseID, model, created, []Map{item}, publicTokenUsage(it, ii, countPublicTextTokens(full, model), 0)))
}
func (s *Server) publicResponses(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
	if isPublicImageResponse(body) {
		return s.publicImageResponse(ctx, body)
	}
	raw := publicResponseMessages(body["input"], body["instructions"])
	messages, e := s.normalizePublicMessages(ctx, raw, nil)
	if e != nil {
		return nil, e
	}
	messages, e = s.normalizePublicHistory(ctx, messages)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{"image_generation": true}
	for k, v := range publicSearchTypes {
		allowed[k] = v
	}
	unsupported := hasPublicUnsupportedTools(body, allowed)
	if unsupported {
		messages = append([]publicMessage{{Role: "system", Content: publicToolUnavailable}}, messages...)
	}
	compute := func(ctx context.Context, emit func(Map) error) error {
		if hasPublicSearch(body) && !unsupported {
			return s.publicSearchResponseEvents(ctx, body, messages, emit)
		}
		return s.cachedPublicEvents(ctx, "responses", body, messages, truth(body["stream"]), func(send func(Map) error) error { return s.publicResponseEvents(ctx, body, messages, send) }, emit)
	}
	if truth(body["stream"]) {
		return &PublicStream{Protocol: "openai", Produce: compute}, nil
	}
	out := Map{}
	e = compute(ctx, func(event Map) error {
		if event["type"] == "response.completed" {
			out = object(event["response"])
		}
		return nil
	})
	return out, e
}
func publicSearchQuery(messages []publicMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			if query := strings.TrimSpace(messages[i].Content); query != "" {
				return query
			}
		}
	}
	return ""
}
func publicSearchSources(result Map) []Map {
	out := []Map{}
	seen := map[string]bool{}
	for _, source := range dicts(result["sources"]) {
		url := text(source["url"])
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, Map{"title": text(source["title"]), "url": url, "snippet": text(source["snippet"])})
	}
	return out
}
func publicSearchCitations(result Map) (string, []Map) {
	value := strings.TrimSpace(sanitizePublicText(publicString(result["answer"])))
	annotations := []Map{}
	sources := publicSearchSources(result)
	if len(sources) > 0 {
		if value != "" {
			value += "\n\n"
		}
		value += "Sources:\n"
		for i, source := range sources {
			title, url := text(source["title"]), text(source["url"])
			if title == "" {
				title = url
			}
			value += fmt.Sprintf("%d. %s", i+1, title)
			if text(source["title"]) != "" {
				value += " - "
			}
			start := utf8.RuneCountInString(value)
			value += url
			annotations = append(annotations, Map{"type": "url_citation", "start_index": start, "end_index": utf8.RuneCountInString(value), "url": url, "title": title})
			value += "\n"
		}
	}
	return strings.TrimSpace(value), annotations
}
func (s *Server) publicSearchChat(ctx context.Context, body Map, messages []publicMessage, model string, stream bool) (any, error) {
	query := publicSearchQuery(messages)
	if query == "" {
		return nil, problem(400, "messages or prompt is required for web search")
	}
	compute := func(ctx context.Context, emit func(Map) error) error {
		result, e := s.Text.Search(ctx, query)
		if e != nil {
			return e
		}
		value, annotations := publicSearchCitations(result)
		if stream {
			completionID, created := "chatcmpl-"+id(16), s.now().Unix()
			if e = emit(publicChatChunk(model, completionID, created, Map{"role": "assistant", "content": value}, nil)); e != nil {
				return e
			}
			return emit(publicChatChunk(model, completionID, created, Map{}, "stop"))
		}
		response := s.publicChatResponse(model, value, messages)
		wrapped := []Map{}
		for _, a := range annotations {
			wrapped = append(wrapped, Map{"type": "url_citation", "url_citation": Map{"start_index": a["start_index"], "end_index": a["end_index"], "url": a["url"], "title": a["title"]}})
		}
		if len(wrapped) > 0 {
			object(dicts(response["choices"])[0]["message"])["annotations"] = wrapped
		}
		return emit(response)
	}
	if stream {
		return &PublicStream{Protocol: "openai", Produce: compute}, nil
	}
	var out Map
	e := compute(ctx, func(v Map) error { out = v; return nil })
	return out, e
}
func publicSearchCall(query, itemID, status string, sources []Map) Map {
	action := Map{"type": "search", "query": query, "queries": []string{query}}
	if len(sources) > 0 {
		items := []Map{}
		for _, source := range sources {
			items = append(items, Map{"type": "url", "url": source["url"]})
		}
		action["sources"] = items
	}
	return Map{"id": itemID, "type": "web_search_call", "status": status, "action": action}
}
func (s *Server) publicSearchResponseEvents(ctx context.Context, body Map, messages []publicMessage, emit func(Map) error) error {
	query := publicSearchQuery(messages)
	if query == "" {
		return problem(400, "input text is required for web_search")
	}
	model := publicModel(body, "auto")
	rid, sid, mid, created := "resp_"+id(16), "ws_"+id(16), "msg_"+id(16), s.now().Unix()
	events := []Map{publicResponseCreated(rid, model, created), {"type": "response.output_item.added", "output_index": 0, "item": publicSearchCall(query, sid, "in_progress", nil)}, {"type": "response.web_search_call.in_progress", "output_index": 0, "item_id": sid}, {"type": "response.web_search_call.searching", "output_index": 0, "item_id": sid}}
	for _, event := range events {
		if e := emit(event); e != nil {
			return e
		}
	}
	result, e := s.Text.Search(ctx, query)
	if e != nil {
		return e
	}
	searchItem := publicSearchCall(query, sid, "completed", publicSearchSources(result))
	value, annotations := publicSearchCitations(result)
	messageItem := publicResponseItem(value, mid, "completed", annotations)
	events = []Map{{"type": "response.web_search_call.completed", "output_index": 0, "item_id": sid}, {"type": "response.output_item.done", "output_index": 0, "item": searchItem}, {"type": "response.output_item.added", "output_index": 1, "item": publicResponseItem("", mid, "in_progress", annotations)}}
	if value != "" {
		events = append(events, Map{"type": "response.output_text.delta", "item_id": mid, "output_index": 1, "content_index": 0, "delta": value})
	}
	events = append(events, Map{"type": "response.output_text.done", "item_id": mid, "output_index": 1, "content_index": 0, "text": value}, Map{"type": "response.output_item.done", "output_index": 1, "item": messageItem})
	it, ii := countPublicMessages(messages, model)
	events = append(events, publicResponseCompleted(rid, model, created, []Map{searchItem, messageItem}, publicTokenUsage(it, ii, countPublicTextTokens(value, model), 0)))
	for _, event := range events {
		if e = emit(event); e != nil {
			return e
		}
	}
	return nil
}
func publicImagePrompt(body Map) string {
	if prompt := text(body["prompt"]); prompt != "" {
		return prompt
	}
	parts := []string{}
	for _, m := range dicts(body["messages"]) {
		if text(m["role"]) == "user" {
			if value := publicImagePromptText(m["content"]); value != "" {
				parts = append(parts, value)
			}
		}
	}
	return strings.Join(parts, "\n")
}
func publicImageContent(result Map) string {
	parts := []string{}
	for i, item := range dicts(result["data"]) {
		source := text(defaultValue(item["url"], item["image_url"]))
		if source == "" {
			if b64 := text(item["b64_json"]); b64 != "" {
				source = "data:image/png;base64," + b64
			}
		}
		if source != "" {
			parts = append(parts, fmt.Sprintf("![image_%d](%s)", i+1, source))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	if message := publicString(result["message"]); message != "" {
		return message
	}
	return "Image generation completed."
}
func (s *Server) publicImageChat(ctx context.Context, body Map) (any, error) {
	prompt := publicImagePrompt(body)
	if prompt == "" {
		return nil, problem(400, "prompt is required")
	}
	n := integer(body["n"], 1)
	if n == 0 {
		n = 1
	}
	if n < 1 || n > 4 {
		return nil, problem(400, "n must be between 1 and 4")
	}
	inputs := []upstream.ImageInput{}
	for i := len(dicts(body["messages"])) - 1; i >= 0; i-- {
		m := dicts(body["messages"])[i]
		if text(m["role"]) != "user" {
			continue
		}
		var e error
		inputs, e = s.readImageReferences(ctx, publicImageParts(m["content"]))
		if e != nil {
			return nil, e
		}
		if len(inputs) > 0 {
			break
		}
	}
	payload := Map{"prompt": prompt, "model": publicModel(body, "gpt-image-2"), "n": n, "response_format": "b64_json", "images": inputs, "base_url": body["base_url"]}
	compute := func(ctx context.Context, emit func(Map) error) error {
		model := text(payload["model"])
		stream := truth(body["stream"])
		cid, created := "chatcmpl-"+id(16), s.now().Unix()
		sent, count := false, 0
		var sendErr error
		sendContent := func(content string) error {
			delta := Map{"content": content}
			if !sent {
				delta["role"] = "assistant"
				sent = true
			}
			return emit(publicChatChunk(model, cid, created, delta, nil))
		}
		var partial func([]Map)
		if stream {
			partial = func(items []Map) {
				if sendErr != nil || len(items) <= count {
					return
				}
				newItems := items[count:]
				count = len(items)
				sendErr = sendContent(publicImageContent(Map{"data": newItems}))
			}
		}
		result, e := s.Images.Execute(ctx, payload, nil, partial)
		observePublicResult(ctx, result)
		if sendErr != nil {
			return sendErr
		}
		if e != nil {
			var image *ImageExecutionError
			if stream && errors.As(e, &image) && image.Code == "upstream_text_reply" {
				result = Map{"message": image.Message}
			} else {
				return e
			}
		}
		if stream {
			items := dicts(result["data"])
			if len(items) > count {
				if e = sendContent(publicImageContent(Map{"data": items[count:]})); e != nil {
					return e
				}
			} else if len(items) == 0 {
				if e = sendContent(publicImageContent(result)); e != nil {
					return e
				}
			}
			if !sent {
				if e = sendContent(""); e != nil {
					return e
				}
			}
			return emit(publicChatChunk(model, cid, created, Map{}, "stop"))
		}
		response := s.publicChatResponse(model, publicImageContent(result), nil)
		if truth(result["created"]) {
			response["created"] = result["created"]
		}
		if usage := object(result["usage"]); len(usage) > 0 {
			response["usage"] = Map{"prompt_tokens": usage["input_tokens"], "completion_tokens": usage["output_tokens"], "total_tokens": usage["total_tokens"], "prompt_tokens_details": usage["input_tokens_details"], "completion_tokens_details": usage["output_tokens_details"]}
		}
		return emit(response)
	}
	if truth(body["stream"]) {
		return &PublicStream{Protocol: "openai", Produce: compute}, nil
	}
	var out Map
	e := compute(ctx, func(v Map) error { out = v; return nil })
	return out, e
}
func (s *Server) publicImageResponse(ctx context.Context, body Map) (any, error) {
	prompt := publicResponseImagePrompt(body["input"])
	if prompt == "" {
		return nil, problem(400, "input text is required")
	}
	images, e := s.publicResponseImageInputs(ctx, body["input"])
	if e != nil {
		return nil, e
	}

	tool := Map{}
	for _, t := range dicts(body["tools"]) {
		if t["type"] == "image_generation" {
			tool = t
			break
		}
	}
	model := publicModel(body, "gpt-image-2")
	payload := Map{"prompt": prompt, "model": model, "n": 1, "size": tool["size"], "quality": defaultValue(tool["quality"], "auto"), "response_format": "b64_json", "images": images}
	compute := func(ctx context.Context, emit func(Map) error) error {
		rid, created := "resp_"+id(16), s.now().Unix()
		if e := emit(publicResponseCreated(rid, model, created)); e != nil {
			return e
		}
		result, e := s.Images.Execute(ctx, payload, nil, nil)
		observePublicResult(ctx, result)
		if e != nil {
			var image *ImageExecutionError
			if errors.As(e, &image) && image.Code == "upstream_text_reply" {
				result = Map{"message": image.Message}
			} else {
				return e
			}
		}
		items := []Map{}
		usage := object(result["usage"])
		if len(usage) > 0 {
			usage = copyMap(usage)
			inputImage := publicRawResponseImageTokens(body["input"])
			it := countPublicTextTokens(prompt, model)
			usage["input_tokens"] = it + inputImage
			usage["total_tokens"] = it + inputImage + integer(usage["output_tokens"], 0)
			usage["input_tokens_details"] = Map{"text_tokens": it, "image_tokens": inputImage, "cached_tokens": 0}
		}
		if value := publicString(result["message"]); value != "" && len(dicts(result["data"])) == 0 {
			mid := "msg_" + id(16)
			for _, event := range []Map{{"type": "response.output_text.delta", "item_id": mid, "output_index": 0, "content_index": 0, "delta": value}, {"type": "response.output_text.done", "item_id": mid, "output_index": 0, "content_index": 0, "text": value}} {
				if e = emit(event); e != nil {
					return e
				}
			}
			items = append(items, publicResponseItem(value, mid, "completed", nil))
			usage = publicTokenUsage(countPublicTextTokens(prompt, model), publicRawResponseImageTokens(body["input"]), countPublicTextTokens(value, model), 0)
		} else {
			for _, item := range dicts(result["data"]) {
				if b64 := text(item["b64_json"]); b64 != "" {
					items = append(items, Map{"id": fmt.Sprintf("ig_%d", len(items)+1), "type": "image_generation_call", "status": "completed", "result": b64, "revised_prompt": defaultValue(item["revised_prompt"], prompt)})
				}
			}
		}
		for i, item := range items {
			if e = emit(Map{"type": "response.output_item.done", "output_index": i, "item": item}); e != nil {
				return e
			}
		}
		return emit(publicResponseCompleted(rid, model, created, items, usage))
	}
	if truth(body["stream"]) {
		return &PublicStream{Protocol: "openai", Produce: compute}, nil
	}
	var out Map
	e = compute(ctx, func(v Map) error {
		if v["type"] == "response.completed" {
			out = object(v["response"])
		}
		return nil
	})
	return out, e
}

func publicImageParts(content any) []any {
	out := []any{}
	for _, v := range list(content) {
		m := object(v)
		switch text(m["type"]) {
		case "image_url", "input_image", "image":
			out = append(out, m)
		}
	}
	return out
}

func publicImagePromptText(content any) string {
	if s, ok := content.(string); ok {
		return strings.TrimSpace(s)
	}
	parts := []string{}
	for _, p := range dicts(content) {
		if p["type"] == "text" || p["type"] == "input_text" {
			value := text(orValue(p["text"], p["input_text"]))
			if value != "" {
				parts = append(parts, value)
			}
		}
	}
	return strings.Join(parts, "\n")
}
func publicResponseImagePrompt(input any) string {
	if s, ok := input.(string); ok {
		return strings.TrimSpace(s)
	}
	if m, ok := input.(map[string]any); ok {
		role := strings.ToLower(text(m["role"]))
		if role != "" && role != "user" {
			return ""
		}
		return publicImagePromptText(m["content"])
	}
	parts := []string{}
	for _, m := range dicts(input) {
		if m["type"] == "input_text" {
			if value := text(m["text"]); value != "" {
				parts = append(parts, value)
			}
			continue
		}
		role := strings.ToLower(text(m["role"]))
		if role != "" && role != "user" {
			continue
		}
		if value := publicImagePromptText(m["content"]); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}
func (s *Server) publicResponseImageInputs(ctx context.Context, input any) ([]upstream.ImageInput, error) {
	candidates := []Map{}
	if m, ok := input.(map[string]any); ok {
		candidates = append(candidates, m)
	} else {
		items := dicts(input)
		for i := len(items) - 1; i >= 0; i-- {
			candidates = append(candidates, items[i])
		}
	}
	for _, m := range candidates {
		parts := publicImageParts(m["content"])
		if m["type"] == "input_image" {
			parts = []any{m}
		}
		found, e := s.readImageReferences(ctx, parts)
		if e != nil {
			return nil, e
		}
		if len(found) > 0 {
			return found[:1], nil
		}
	}
	return nil, nil
}
