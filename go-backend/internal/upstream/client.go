package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

type Client struct {
	cfg       Config
	http      HTTPClient
	assets    AssetHTTPClient
	operation chan struct{}
}
type Message struct {
	Role    string       `json:"role"`
	Content string       `json:"content"`
	Images  []ImageInput `json:"-"`
}
type TextResult struct {
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
}
type ImageResult struct {
	ConversationID string       `json:"conversation_id"`
	Assets         []ImageAsset `json:"assets"`
	Text           string       `json:"text,omitempty"`
}
type ImageAsset struct {
	AssetPointer
	URL  string `json:"url,omitempty"`
	Data []byte `json:"-"`
}
type HandshakeResult struct {
	Authenticated bool `json:"authenticated"`
	ProofUsed     bool `json:"proof_used"`
	TurnstileUsed bool `json:"turnstile_used"`
}
type requirements struct{ Token, Proof, Turnstile, SO string }

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) begin(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, requestError(ctx, "admission", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	select {
	case c.operation <- struct{}{}:
		return ctx, func() { <-c.operation; cancel() }, nil
	case <-ctx.Done():
		cancel()
		return nil, nil, requestError(ctx, "admission", ctx.Err())
	}
}

func (c *Client) headers(path string, authenticated bool) http.Header {
	h := http.Header{
		"user-agent": {c.cfg.UserAgent}, "origin": {origin}, "referer": {origin + "/"},
		"accept-language": {"zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"},
		"oai-device-id":   {c.cfg.DeviceID}, "oai-session-id": {c.cfg.SessionID},
		"oai-client-version": {"prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"}, "oai-client-build-number": {"6708908"},
		"sec-ch-ua":        {`"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`},
		"sec-ch-ua-mobile": {"?0"}, "sec-ch-ua-platform": {`"Windows"`},
		"sec-fetch-dest": {"empty"}, "sec-fetch-mode": {"cors"}, "sec-fetch-site": {"same-origin"},
		"x-openai-target-path": {path}, "x-openai-target-route": {path},
	}
	h[http.HeaderOrderKey] = []string{"user-agent", "accept", "accept-language", "content-type", "authorization", "origin", "referer", "oai-device-id", "oai-session-id"}
	if c.cfg.CookieHeader != "" {
		h["cookie"] = []string{c.cfg.CookieHeader}
	}
	if authenticated && c.cfg.AccessToken != "" {
		h["authorization"] = []string{"Bearer " + c.cfg.AccessToken}
	}
	return h
}

func (c *Client) request(ctx context.Context, method, path, stage string, payload any, h http.Header) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(compactJSON(payload))
		h["content-type"] = []string{"application/json"}
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+path, body)
	if err != nil {
		return nil, failure("invalid_request", stage)
	}
	req.Header = h
	resp, err := c.http.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, requestError(ctx, stage, err)
	}
	if resp == nil || resp.Body == nil {
		return nil, failure("invalid_response", stage)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		kind := "upstream_http_error"
		switch resp.StatusCode {
		case 401:
			kind = "auth_invalid"
		case 403:
			kind = "access_denied"
		case 429:
			kind = "rate_limited"
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var structured map[string]any
		if json.Unmarshal(body, &structured) == nil {
			if code := structuredImageFailure(structured); code != "" {
				kind = code
			}
		}
		retry, retryErr := strconv.Atoi(resp.Header.Get("Retry-After"))
		retryPresent := retryErr == nil && retry >= 0
		if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			retry = max(0, int(time.Until(date).Seconds()))
			retryPresent = true
		}
		return nil, &Error{Kind: kind, Stage: stage, Status: resp.StatusCode, RetryAfterSeconds: max(0, retry), RetryAfterPresent: retryPresent}
	}
	return resp, nil
}

func (c *Client) readBody(ctx context.Context, resp *http.Response, stage string) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes+1))
	if err != nil {
		return nil, requestError(ctx, stage, err)
	}
	if int64(len(body)) > c.cfg.MaxBodyBytes {
		return nil, failure("response_too_large", stage)
	}
	return body, nil
}

func (c *Client) json(ctx context.Context, method, path, stage string, payload any, h http.Header) (map[string]any, error) {
	result, _, err := c.jsonRaw(ctx, method, path, stage, payload, h)
	return result, err
}

func (c *Client) jsonRaw(ctx context.Context, method, path, stage string, payload any, h http.Header) (map[string]any, []byte, error) {
	resp, err := c.request(ctx, method, path, stage, payload, h)
	if err != nil {
		return nil, nil, err
	}
	body, err := c.readBody(ctx, resp, stage)
	if err != nil {
		return nil, nil, err
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, nil, failure("invalid_json", stage)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, nil, failure("invalid_json", stage)
	}
	return result, body, nil
}

func (c *Client) prepare(ctx context.Context) (requirements, error) {
	h := c.headers("/", false)
	h["accept"] = []string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"}
	h["sec-fetch-dest"] = []string{"document"}
	h["sec-fetch-mode"] = []string{"navigate"}
	h["sec-fetch-site"] = []string{"none"}
	h["sec-fetch-user"] = []string{"?1"}
	h["upgrade-insecure-requests"] = []string{"1"}
	resp, err := c.request(ctx, "GET", "/", "bootstrap", nil, h)
	if err != nil {
		return requirements{}, err
	}
	body, err := c.readBody(ctx, resp, "bootstrap")
	if err != nil {
		return requirements{}, err
	}
	scripts, build := powResources(body)
	p := "gAAAAAC" + base64.StdEncoding.EncodeToString(compactJSON(powConfig(c.cfg.UserAgent, scripts, build)))
	base := "/backend-anon/sentinel/chat-requirements"
	if c.cfg.AccessToken != "" {
		base = "/backend-api/sentinel/chat-requirements"
	}
	path := base + "/prepare"
	result, err := c.json(ctx, "POST", path, "requirements_prepare", map[string]any{"p": p}, c.headers(path, true))
	if err != nil {
		return requirements{}, err
	}
	if object(result["arkose"])["required"] == true {
		return requirements{}, failure("challenge_arkose", "requirements_prepare")
	}
	turnstile := ""
	if info := object(result["turnstile"]); info["required"] == true {
		turnstile, err = SolveTurnstile(ctx, text(info["dx"]), p)
		if err != nil {
			if ctx.Err() != nil {
				return requirements{}, requestError(ctx, "turnstile", err)
			}
			return requirements{}, err
		}
	}
	prepareToken := text(result["prepare_token"])
	if prepareToken == "" {
		return requirements{}, failure("missing_prepare_token", "requirements_prepare")
	}
	proof := ""
	if info := object(result["proofofwork"]); info["required"] == true {
		proof, err = solveProof(ctx, text(info["seed"]), text(info["difficulty"]), powConfig(c.cfg.UserAgent, scripts, build), c.cfg.ProofAttempts)
		if err != nil {
			if ctx.Err() != nil {
				return requirements{}, requestError(ctx, "proof", err)
			}
			return requirements{}, err
		}
	}
	path = base + "/finalize"
	result, err = c.json(ctx, "POST", path, "requirements_finalize", map[string]any{"prepare_token": prepareToken, "proof_token": proof, "turnstile_token": turnstile}, c.headers(path, true))
	if err != nil {
		return requirements{}, err
	}
	token := text(result["token"])
	if token == "" {
		return requirements{}, failure("missing_requirements_token", "requirements_finalize")
	}
	return requirements{Token: token, Proof: proof, Turnstile: turnstile, SO: text(result["so_token"])}, nil
}

func (c *Client) Handshake(ctx context.Context) (HandshakeResult, error) {
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return HandshakeResult{}, err
	}
	defer end()
	r, err := c.prepare(ctx)
	return HandshakeResult{Authenticated: c.cfg.AccessToken != "", ProofUsed: r.Proof != "", TurnstileUsed: r.Turnstile != ""}, err
}

func (c *Client) conversationHeaders(path string, r requirements) http.Header {
	h := c.headers(path, true)
	h["accept"] = []string{"text/event-stream"}
	h["openai-sentinel-chat-requirements-token"] = []string{r.Token}
	if r.Proof != "" {
		h["openai-sentinel-proof-token"] = []string{r.Proof}
	}
	if r.Turnstile != "" {
		h["openai-sentinel-turnstile-token"] = []string{r.Turnstile}
	}
	if r.SO != "" {
		h["openai-sentinel-so-token"] = []string{r.SO}
	}
	return h
}

func (c *Client) stream(ctx context.Context, path string, payload any, h http.Header, state *conversationState, image bool, onText func(string) error) error {
	resp, err := c.request(ctx, "POST", path, "conversation", payload, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return failure("expected_sse", "conversation")
	}
	err = ReadSSE(ctx, resp.Body, c.cfg.MaxEventBytes, c.cfg.MaxStreamBytes, func(e Event) error {
		before := state.Text
		if err := state.consume(e); err != nil {
			return err
		}
		if onText != nil && state.Text != before {
			if err := onText(state.Text); err != nil {
				return failure("consumer_error", "conversation")
			}
		}
		if state.assistantTerminal || (image && state.toolTerminal && len(state.Pointers) > 0) {
			return errStreamDone
		}
		return nil
	})
	if ctx.Err() != nil {
		return requestError(ctx, "conversation", ctx.Err())
	}
	if err != nil {
		return err
	}
	if !state.done && !state.assistantTerminal && !(image && state.toolTerminal && len(state.Pointers) > 0) {
		return failure("incomplete_stream", "conversation")
	}
	return nil
}

// StreamText emits complete answer snapshots so upstream replace patches can be
// represented faithfully. Public protocol adapters will own delta conversion.
func (c *Client) StreamText(ctx context.Context, messages []Message, model string, onText func(string) error) (TextResult, error) {
	return c.StreamTextWithOptions(ctx, messages, model, "", onText)
}

func (c *Client) StreamTextWithOptions(ctx context.Context, messages []Message, model, thinkingEffort string, onText func(string) error) (TextResult, error) {
	if len(messages) == 0 || len(messages) > 1024 {
		return TextResult{}, failure("messages_required", "validation")
	}
	totalBytes := 0
	imageBytes, imageCount := 0, 0
	for _, m := range messages {
		for _, in := range m.Images {
			imageBytes += len(in.Data)
			imageCount++
		}
		totalBytes += len(m.Content)
		if err := validateImageInputs(m.Images); err != nil {
			return TextResult{}, err
		}
		if len(m.Images) > 0 && (m.Role != "user" || c.cfg.AccessToken == "") {
			return TextResult{}, failure("authenticated_image_input_required", "validation")
		}
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "developer" {
			return TextResult{}, failure("unsupported_role", "validation")
		}
	}
	if totalBytes > 1<<20 || len(model) > 255 || imageBytes > 100<<20 || imageCount > 16 {
		return TextResult{}, failure("input_too_large", "validation")
	}
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return TextResult{}, err
	}
	defer end()
	r, err := c.prepare(ctx)
	if err != nil {
		return TextResult{}, err
	}
	if model == "" {
		model = "auto"
	}
	converted := make([]any, 0, len(messages))
	for _, m := range messages {
		content, metadata, e := c.messageContent(ctx, m.Content, m.Images)
		if e != nil {
			return TextResult{}, e
		}
		converted = append(converted, map[string]any{"id": uuid(), "author": map[string]any{"role": m.Role}, "content": content, "metadata": metadata})
	}
	path, timezone := "/backend-anon/conversation", "America/Los_Angeles"
	if c.cfg.AccessToken != "" {
		path, timezone = "/backend-api/conversation", "Asia/Shanghai"
	}
	payload := map[string]any{
		"action": "next", "messages": converted, "model": model, "parent_message_id": uuid(), "conversation_mode": map[string]any{"kind": "primary_assistant"},
		"conversation_origin": nil, "force_paragen": false, "force_paragen_model_slug": "", "force_rate_limit": false, "force_use_sse": true,
		"history_and_training_disabled": true, "reset_rate_limits": false, "suggestions": []any{}, "supported_encodings": []any{}, "system_hints": []any{},
		"timezone": timezone, "timezone_offset_min": -480, "variant_purpose": "comparison_implicit", "websocket_request_id": uuid(),
		"client_contextual_info": map[string]any{"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900, "page_width": 1400, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560},
	}
	switch strings.ToLower(strings.TrimSpace(thinkingEffort)) {
	case "low", "medium", "high":
		payload["thinking_effort"] = strings.ToLower(strings.TrimSpace(thinkingEffort))
	case "xhigh", "extended":
		payload["thinking_effort"] = "extended"
	}
	state := &conversationState{}
	err = c.stream(ctx, path, payload, c.conversationHeaders(path, r), state, false, onText)
	result := TextResult{ConversationID: state.ConversationID, Text: state.Text}
	return result, err
}

func (c *Client) Models(ctx context.Context) ([]string, error) {
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer end()
	// Models are diagnostic upstream slugs, never a substitute for Model Catalog.
	h := c.headers("/", false)
	resp, err := c.request(ctx, "GET", "/", "bootstrap", nil, h)
	if err != nil {
		return nil, err
	}
	if _, err = c.readBody(ctx, resp, "bootstrap"); err != nil {
		return nil, err
	}
	path := "/backend-anon/models?iim=false&is_gizmo=false"
	if c.cfg.AccessToken != "" {
		path = "/backend-api/models?history_and_training_disabled=false"
	}
	result, err := c.json(ctx, "GET", path, "models", nil, c.headers(strings.Split(path, "?")[0], true))
	if err != nil {
		return nil, err
	}
	models := []string{}
	for _, m := range slice(result["models"]) {
		if slug := text(object(m)["slug"]); slug != "" {
			models = append(models, slug)
		}
	}
	return models, nil
}

// WebImageRoute owns the actual upstream route shared by public image aliases.
func WebImageRoute(model string) (string, error) {
	switch model {
	case "", "gpt-image-2":
		return "gpt-5-3", nil
	case "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst":
		return "auto", nil
	}
	return "", failure("unsupported_image_model", "validation")
}

func (c *Client) GenerateImage(ctx context.Context, prompt, model string) (ImageResult, error) {
	return c.generateWebImage(ctx, ImageRequest{Prompt: prompt, Model: model}, nil, false)
}

func (c *Client) generateWebImage(ctx context.Context, input ImageRequest, progress func(string), enhanced bool) (ImageResult, error) {
	prompt, model := input.Prompt, input.Model
	if c.cfg.AccessToken == "" {
		return ImageResult{}, failure("access_token_required", "validation")
	}
	if strings.TrimSpace(prompt) == "" {
		return ImageResult{}, failure("prompt_required", "validation")
	}
	if len(prompt) > 1<<20 {
		return ImageResult{}, failure("input_too_large", "validation")
	}
	slug, err := WebImageRoute(model)
	if err != nil {
		return ImageResult{}, err
	}
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return ImageResult{}, err
	}
	defer end()
	reportProgress(progress, "uploading")
	content, metadata, err := c.messageContent(ctx, prompt, input.Images)
	if err != nil {
		return ImageResult{}, err
	}
	reportProgress(progress, "bootstrapping")
	r, err := c.prepare(ctx)
	if err != nil {
		return ImageResult{}, err
	}
	reportProgress(progress, "preparing_conversation")
	path := "/backend-api/f/conversation/prepare"
	payload := map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": uuid(), "model": slug, "client_prepare_state": "success",
		"timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"}, "system_hints": []string{"picture_v2"},
		"partial_query":      map[string]any{"id": uuid(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}},
		"supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}
	h := c.conversationHeaders(path, r)
	h["accept"] = []string{"*/*"}
	prepared, err := c.json(ctx, "POST", path, "image_prepare", payload, h)
	if err != nil {
		return ImageResult{}, err
	}
	conduit := text(prepared["conduit_token"])
	if conduit == "" {
		return ImageResult{}, failure("missing_conduit_token", "image_prepare")
	}
	path = "/backend-api/f/conversation"
	delete(payload, "partial_query")
	delete(payload, "fork_from_shared_post")
	payload["client_prepare_state"] = "sent"
	payload["parent_message_id"] = uuid()
	payload["messages"] = []any{map[string]any{"id": uuid(), "author": map[string]any{"role": "user"}, "create_time": float64(time.Now().UnixMilli()) / 1000,
		"content": content, "metadata": imageMessageMetadata(metadata)}}
	payload["enable_message_followups"] = true
	payload["paragen_cot_summary_display_override"] = "allow"
	payload["force_parallel_switch"] = "auto"
	payload["client_contextual_info"] = map[string]any{"is_dark_mode": false, "time_since_loaded": 1200, "page_height": 1072, "page_width": 1724, "pixel_ratio": 1.2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"}
	h = c.conversationHeaders(path, r)
	h["x-conduit-token"] = []string{conduit}
	h["x-oai-turn-trace-id"] = []string{uuid()}
	state := &conversationState{imageMode: enhanced}
	reportProgress(progress, "starting_generation")
	streamCtx, streamCancel := context.WithTimeout(ctx, c.cfg.StreamTimeout)
	reportProgress(progress, "generating")
	err = c.stream(streamCtx, path, payload, h, state, true, nil)
	streamCancel()
	var streamFailure *Error
	recoverable := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &streamFailure) && streamFailure.Kind == "incomplete_stream")
	if ctx.Err() == nil && state.ConversationID != "" && recoverable {
		err = nil
	}
	if err != nil {
		return ImageResult{ConversationID: state.ConversationID, Text: state.Text}, err
	}
	if enhanced {
		if state.imageArguments && len(state.Pointers) == 0 {
			return ImageResult{ConversationID: state.ConversationID, Text: state.Text}, failure("image_tool_error", "conversation")
		}
		reportProgress(progress, "image_stream_resolve_start")
		if err = c.pollImageConfigured(ctx, state); err != nil {
			return ImageResult{ConversationID: state.ConversationID, Text: state.Text}, err
		}
	} else if len(state.Pointers) == 0 {
		if err = c.pollImage(ctx, state); err != nil {
			return ImageResult{ConversationID: state.ConversationID, Text: state.Text}, err
		}
	}
	reportProgress(progress, "receiving_image")
	startedResolve := time.Now()
	assets, err := c.resolveAssets(ctx, state)
	if c.cfg.ImageTiming != nil {
		c.cfg.ImageTiming("asset_resolve_ms", time.Since(startedResolve))
	}
	return ImageResult{ConversationID: state.ConversationID, Assets: assets, Text: state.Text}, err
}

// PollImage resumes result discovery for a known conversation without issuing
// another generation request. It does not create or persist an Image Task.
func (c *Client) PollImage(ctx context.Context, conversationID string) (ImageResult, error) {
	if c.cfg.AccessToken == "" {
		return ImageResult{}, failure("access_token_required", "validation")
	}
	if len(conversationID) == 0 || len(conversationID) > 128 || !idPattern.MatchString(conversationID) {
		return ImageResult{}, failure("invalid_conversation_id", "validation")
	}
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return ImageResult{}, err
	}
	defer end()
	state := &conversationState{ConversationID: conversationID}
	if err = c.pollImage(ctx, state); err != nil {
		return ImageResult{ConversationID: conversationID}, err
	}
	assets, err := c.resolveAssets(ctx, state)
	return ImageResult{ConversationID: conversationID, Assets: assets}, err
}

func (c *Client) pollImage(ctx context.Context, state *conversationState) error {
	if state.ConversationID == "" {
		return failure("missing_conversation_id", "image_poll")
	}
	initial := time.NewTimer(c.cfg.PollInitialWait)
	select {
	case <-ctx.Done():
		initial.Stop()
		return requestError(ctx, "image_poll", ctx.Err())
	case <-initial.C:
	}
	for {
		path := "/backend-api/conversation/" + state.ConversationID
		result, err := c.json(ctx, "GET", path, "image_poll", nil, c.headers(path, true))
		if err != nil {
			return err
		}
		for _, node := range object(result["mapping"]) {
			if m := object(object(node)["message"]); m != nil {
				if err := state.message(m); err != nil {
					return err
				}
			}
		}
		if len(state.Pointers) > 0 {
			return nil
		}
		if state.assistantTerminal && state.Text != "" {
			return failure("upstream_text_reply", "image_poll")
		}
		path = "/backend-api/tasks"
		tasks, err := c.json(ctx, "GET", path, "image_tasks", nil, c.headers(path, true))
		if err != nil {
			return err
		}
		for _, item := range slice(tasks["tasks"]) {
			task := object(item)
			if text(task["conversation_id"]) != state.ConversationID && text(task["original_conversation_id"]) != state.ConversationID {
				continue
			}
			if task["status"] == "failed" || hasError(task["error"]) {
				return failure("image_task_failed", "image_tasks")
			}
			// Only an explicitly tool-authored image message is an output boundary.
			if m := object(task["image_gen_message"]); m != nil {
				if err := state.message(m); err != nil {
					return err
				}
			}
		}
		if len(state.Pointers) > 0 {
			return nil
		}
		timer := time.NewTimer(c.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return requestError(ctx, "image_poll", ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) resolveAssets(ctx context.Context, state *conversationState) ([]ImageAsset, error) {
	if len(state.Pointers) > 16 {
		return nil, failure("too_many_images", "image_resolve")
	}
	assets := make([]ImageAsset, 0, len(state.Pointers))
	seen := map[string]bool{}
	var best error
	remember := func(e error) {
		if best == nil {
			best = e
			return
		}
		var candidate, old *Error
		if errors.As(e, &candidate) && errors.As(best, &old) && candidate.Status == 401 && old.Status != 401 {
			best = e
		}
	}
	for _, p := range state.Pointers {
		path := "/backend-api/files/" + p.ID + "/download"
		if p.Kind == "sediment" {
			if state.ConversationID == "" {
				remember(failure("missing_conversation_id", "image_resolve"))
				continue
			}
			path = "/backend-api/conversation/" + state.ConversationID + "/attachment/" + p.ID + "/download"
		}
		for attempt := 0; attempt < 2; attempt++ {
			result, e := c.json(ctx, "GET", path, "image_resolve", nil, c.headers(path, true))
			if e != nil {
				var controlled *Error
				if errors.As(e, &controlled) && controlled.Status == 401 {
					remember(e)
					break
				}
				if attempt == 0 && ctx.Err() == nil {
					continue
				}
				remember(e)
				break
			}
			assetURL := text(result["download_url"])
			if assetURL == "" {
				assetURL = text(result["url"])
			}
			if assetURL == "" && attempt == 0 {
				continue
			}
			if _, e := validateAssetURL(assetURL, false); e != nil {
				remember(failure("invalid_asset_url", "image_resolve"))
				break
			}
			if !seen[assetURL] {
				assets = append(assets, ImageAsset{AssetPointer: p, URL: assetURL})
				seen[assetURL] = true
			}
			break
		}
	}
	if len(assets) > 0 {
		return assets, nil
	}
	if best != nil {
		return nil, best
	}
	return nil, failure("no_image_generated", "image_resolve")
}

// SafeError is used by CLI callers to serialize only controlled diagnostics.
func SafeError(err error) *Error {
	var controlled *Error
	if errors.As(err, &controlled) {
		return controlled
	}
	return &Error{Kind: "operation_failed", Stage: "probe"}
}
