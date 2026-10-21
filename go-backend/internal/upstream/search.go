package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Search executes the pinned Python search flow without exposing transport,
// credentials, or browser internals in its result projection.
func (c *Client) Search(ctx context.Context, prompt string) (map[string]any, error) {
	if c.cfg.AccessToken == "" {
		return nil, failure("access_token_required", "validation")
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > 1<<20 {
		return nil, failure("prompt_required", "validation")
	}
	ctx, end, e := c.begin(ctx)
	if e != nil {
		return nil, e
	}
	defer end()
	path := "/backend-api/f/conversation/prepare"
	h := c.headers(path, true)
	h["accept"] = []string{"*/*"}
	h["x-conduit-token"] = []string{"no-token"}
	prepared, e := c.json(ctx, "POST", path, "search_prepare", map[string]any{"action": "next", "fork_from_shared_post": false, "parent_message_id": "client-created-root", "model": "gpt-5-5", "client_prepare_state": "success", "timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"}, "system_hints": []string{"search"}, "partial_query": map[string]any{"id": uuid(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}}, "supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]any{"app_name": "chatgpt.com"}}, h)
	if e != nil {
		return nil, e
	}
	conduit := text(prepared["conduit_token"])
	if conduit == "" {
		return nil, failure("missing_conduit_token", "search_prepare")
	}
	requirements, e := c.prepare(ctx)
	if e != nil {
		return nil, e
	}
	path = "/backend-api/f/conversation"
	h = c.conversationHeaders(path, requirements)
	h["x-conduit-token"] = []string{conduit}
	h["x-oai-turn-trace-id"] = []string{uuid()}
	payload := map[string]any{"action": "next", "messages": []any{map[string]any{"id": uuid(), "author": map[string]any{"role": "user"}, "create_time": float64(time.Now().UnixMilli()) / 1000, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}, "metadata": map[string]any{"developer_mode_connector_ids": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false, "system_hints": []string{"search"}, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}}}}, "parent_message_id": "client-created-root", "model": "gpt-5-5", "client_prepare_state": "success", "timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"}, "enable_message_followups": true, "system_hints": []any{}, "supports_buffering": true, "supported_encodings": []string{"v1"}, "force_use_search": true, "client_reported_search_source": "conversation_composer_web_icon", "client_contextual_info": map[string]any{"is_dark_mode": false, "time_since_loaded": 36, "page_height": 925, "page_width": 886, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"}, "paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto"}
	state := &conversationState{}
	if e = c.stream(ctx, path, payload, h, state, false, nil); e != nil {
		return nil, e
	}
	if state.ConversationID == "" {
		return nil, failure("missing_conversation_id", "search_stream")
	}
	previous := ""
	stable := 0
	var last map[string]any
	for ctx.Err() == nil {
		path = "/backend-api/conversation/" + state.ConversationID
		headers := c.headers(path, true)
		headers["referer"] = []string{origin + "/c/" + state.ConversationID}
		headers["x-openai-target-route"] = []string{"/backend-api/conversation/{conversation_id}"}
		doc, raw, e := c.jsonRaw(ctx, "GET", path, "search_poll", nil, headers)
		if e != nil {
			var controlled *Error
			if !errors.As(e, &controlled) || !searchRetryStatus(controlled.Status) {
				return nil, e
			}
		} else {
			last = extractSearchResult(state.ConversationID, doc, searchJSONKeyOrder(raw))
			answer := text(last["answer"])
			if answer != "" {
				if searchDone(text(last["status"])) {
					return last, nil
				}
				if answer == previous {
					stable++
				} else {
					stable = 0
				}
				previous = answer
				if stable >= 2 {
					return last, nil
				}
			}
		}
		if e := waitImage(ctx, 3*time.Second); e != nil {
			break
		}
	}
	if last != nil && text(last["answer"]) != "" {
		return last, nil
	}
	return nil, requestError(ctx, "search_poll", ctx.Err())
}
func searchRetryStatus(status int) bool {
	switch status {
	case 404, 409, 423, 429, 500, 502, 503, 504:
		return true
	}
	return false
}
func searchDone(status string) bool {
	return status == "finished_successfully" || status == "finished_partial_completion" || status == "stop"
}

var searchURL = regexp.MustCompile(`https?://[^\s"'<>）)\]}]+`)
var searchBlock = regexp.MustCompile("\ue200([a-zA-Z0-9_]+)\ue202([^\ue201]*)\ue201")
var searchCitationNumber = regexp.MustCompile(`search(\d+)`)
var searchSpaceNewline = regexp.MustCompile(`[ \t]+\n`)
var searchExcessNewline = regexp.MustCompile(`\n{3,}`)

func cleanSearchAnswer(answer string) string {
	answer = searchBlock.ReplaceAllStringFunc(answer, func(value string) string {
		m := searchBlock.FindStringSubmatch(value)
		if len(m) == 3 && m[1] == "cite" {
			n := searchCitationNumber.FindStringSubmatch(m[2])
			if len(n) == 2 {
				v, _ := strconv.Atoi(n[1])
				return "[" + strconv.Itoa(v+1) + "]"
			}
		}
		return ""
	})
	answer = searchSpaceNewline.ReplaceAllString(answer, "\n")
	answer = searchExcessNewline.ReplaceAllString(answer, "\n\n")
	return strings.TrimSpace(answer)
}
func searchMessageText(message map[string]any) string {
	value := message["content"]
	content := object(value)
	parts := []string{}
	if s, ok := value.(string); ok {
		parts = append(parts, s)
	} else {
		if s := text(content["text"]); s != "" {
			parts = append(parts, s)
		}
		for _, part := range slice(content["parts"]) {
			if s, ok := part.(string); ok {
				parts = append(parts, s)
			} else {
				for _, key := range []string{"text", "summary", "content"} {
					if s := text(object(part)[key]); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
	}
	filtered := []string{}
	for _, part := range parts {
		if s := strings.TrimSpace(part); s != "" {
			filtered = append(filtered, s)
		}
	}
	return cleanSearchAnswer(strings.Join(filtered, "\n"))
}
func walkSearch(value any, visit func(map[string]any), visitString func(string)) {
	walkSearchOrdered(value, visit, visitString, nil, "")
}
func walkSearchOrdered(value any, visit func(map[string]any), visitString func(string), order map[string][]string, path string) {
	switch item := value.(type) {
	case map[string]any:
		visit(item)
		keys := orderedSearchKeys(item, order[path])
		for _, key := range keys {
			walkSearchOrdered(item[key], visit, visitString, order, path+"/"+searchPointerKey(key))
		}
	case []any:
		for i, child := range item {
			walkSearchOrdered(child, visit, visitString, order, path+"/"+strconv.Itoa(i))
		}
	case string:
		if visitString != nil {
			visitString(item)
		}
	}
}
func orderedSearchKeys(m map[string]any, original []string) []string {
	if len(original) > 0 {
		return original
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func searchPointerKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// Preserve wire object order where source numbering and equal-time message
// selection depend on Python's insertion-ordered JSON dictionaries.
func searchJSONKeyOrder(data []byte) map[string][]string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	order := map[string][]string{}
	var read func(string) error
	read = func(path string) error {
		tok, e := decoder.Token()
		if e != nil {
			return e
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			for decoder.More() {
				k, e := decoder.Token()
				if e != nil {
					return e
				}
				key := k.(string)
				order[path] = append(order[path], key)
				if e := read(path + "/" + searchPointerKey(key)); e != nil {
					return e
				}
			}
		case '[':
			for i := 0; decoder.More(); i++ {
				if e := read(path + "/" + strconv.Itoa(i)); e != nil {
					return e
				}
			}
		}
		_, e = decoder.Token()
		return e
	}
	if read("") != nil {
		return nil
	}
	return order
}

func cleanSearchURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), ".,;，。；")
}
func extractSearchResult(conversationID string, doc map[string]any, order map[string][]string) map[string]any {
	mapping := object(doc["mapping"])
	chosen := map[string]any{}
	chosenID := ""
	answer := ""
	chosenTerminal := false
	chosenTime := float64(-1)
	ids := orderedSearchKeys(mapping, order["/mapping"])
	for _, id := range ids {
		m := object(object(mapping[id])["message"])
		if text(object(m["author"])["role"]) != "assistant" {
			continue
		}
		ct := strings.ToLower(text(object(m["content"])["content_type"]))
		recipient := strings.ToLower(text(m["recipient"]))
		if ct == "code" || ct == "thoughts" || ct == "reasoning_recap" || (recipient != "" && recipient != "all") {
			continue
		}
		candidate := searchMessageText(m)
		if candidate == "" {
			continue
		}
		metadata := object(m["metadata"])
		terminal := m["end_turn"] == true || metadata["is_complete"] == true || searchDone(text(object(metadata["finish_details"])["type"]))
		created := numeric(m["create_time"])
		if answer == "" || (terminal && !chosenTerminal) || (terminal == chosenTerminal && created > chosenTime) {
			chosen = m
			chosenID = id
			answer = candidate
			chosenTerminal = terminal
			chosenTime = created
		}
	}
	metadata := object(chosen["metadata"])
	status := text(object(metadata["finish_details"])["type"])
	if status == "" {
		status = text(metadata["status"])
	}
	if status == "" {
		status = findSearchValue(chosen, "status")
	}
	sources := []any{}
	seenSources := map[string]bool{}
	groups := []any{}
	seenGroups := map[string]bool{}
	addSource := func(obj map[string]any) {
		meta := object(obj["metadata"])
		u := firstText(obj["url"], obj["link"], obj["source_url"], meta["url"])
		u = cleanSearchURL(u)
		if u == "" || seenSources[u] {
			return
		}
		seenSources[u] = true
		sources = append(sources, map[string]any{"title": strings.TrimSpace(firstText(obj["title"], obj["name"], obj["source"])), "url": u, "snippet": strings.TrimSpace(firstText(obj["snippet"], obj["text"], obj["description"])), "source_type": strings.TrimSpace(firstText(obj["type"], obj["source_type"]))})
	}
	addGroup := func(value any) {
		if s, ok := value.(string); ok {
			var m map[string]any
			if json.Unmarshal([]byte(s), &m) != nil {
				return
			}
			value = m
		}
		m := object(value)
		if m == nil {
			return
		}
		queriesValue := m["query"]
		if queriesValue == nil {
			queriesValue = m["queries"]
		}
		queries := []string{}
		switch v := queriesValue.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				queries = append(queries, strings.TrimSpace(v))
			}
		case []any:
			for _, item := range v {
				if s := strings.TrimSpace(text(item)); s != "" && len(queries) < 6 {
					queries = append(queries, s)
				}
			}
		}
		if len(queries) == 0 {
			return
		}
		group := map[string]any{"queries": queries}
		if s := strings.TrimSpace(text(m["aspect_ratio"])); s != "" {
			group["aspect_ratio"] = s
		}
		if n := int(numeric(m["num_per_query"])); n > 0 {
			group["num_per_query"] = n
		}
		encoded, _ := json.Marshal(group)
		if !seenGroups[string(encoded)] {
			groups = append(groups, group)
			seenGroups[string(encoded)] = true
		}
	}
	walkSearchOrdered(chosen, func(m map[string]any) {
		addSource(m)
		if v := m["image_group"]; v != nil {
			addGroup(v)
		}
		block := strings.ToLower(firstText(m["type"], m["name"], m["block_type"]))
		if block == "image_group" || (m["query"] != nil && (m["aspect_ratio"] != nil || m["num_per_query"] != nil)) {
			addGroup(m)
		}
	}, func(s string) {
		for _, match := range searchBlock.FindAllStringSubmatch(s, -1) {
			if len(match) == 3 && match[1] == "image_group" {
				addGroup(match[2])
			}
		}
	}, order, "/mapping/"+searchPointerKey(chosenID)+"/message")
	for _, u := range searchURL.FindAllString(answer, -1) {
		u = cleanSearchURL(u)
		if u != "" && !seenSources[u] {
			sources = append(sources, map[string]any{"title": "", "url": u, "snippet": "", "source_type": ""})
			seenSources[u] = true
		}
	}
	return map[string]any{"conversation_id": conversationID, "status": strings.TrimSpace(status), "answer": answer, "sources": sources, "image_groups": groups, "assistant_message_id": text(chosen["id"]), "create_time": numeric(chosen["create_time"])}
}
func firstText(values ...any) string {
	for _, v := range values {
		if s := text(v); s != "" {
			return s
		}
	}
	return ""
}
func findSearchValue(value any, key string) string {
	var found string
	walkSearch(value, func(m map[string]any) {
		if found == "" {
			found = text(m[key])
		}
	}, nil)
	return found
}
