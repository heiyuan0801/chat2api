package controlplane

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
)

// CallRecorder owns exactly one final immutable invocation record. It stores
// bounded request text and measured metadata, never input images or responses.
type CallRecorder struct {
	server                                  *Server
	ctx                                     context.Context
	mu                                      sync.Mutex
	once                                    sync.Once
	finishErr                               error
	started                                 time.Time
	endpoint, model, summary, callID        string
	identity, detail                        Map
	image                                   bool
	resumed                                 bool
	urls                                    []string
	attempts                                []Map
	images                                  []Map
	b64Count, b64Chars, dataCount, urlCount int
}

var callCredentialText = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)

func safeCallText(value any, limit int) string {
	s := inlineReviewImage.ReplaceAllString(text(value), "[image]")
	s = callCredentialText.ReplaceAllString(s, "[credential]")
	s = diagnostic(s, nil, 0)
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if limit > 0 && len(r) > limit {
		return strings.TrimSpace(string(r[:limit-1])) + "…"
	}
	return s
}
func (s *Server) beginPublicCall(ctx context.Context, endpoint string, payload, identity Map) *CallRecorder {
	model := safeCallText(imageTaskText(payload["model"], "auto"), 255)
	imageRequest := strings.HasPrefix(endpoint, "/v1/images") || (endpoint == "/v1/chat/completions" && isPublicImageChat(payload)) || (endpoint == "/v1/responses" && isPublicImageResponse(payload))
	summary := map[string]string{"/v1/images/generations": "文生图", "/v1/images/edits": "图生图", "/v1/chat/completions": "文本生成", "/v1/responses": "Responses", "/v1/messages": "Messages", "/v1/search": "搜索"}[endpoint]
	if summary == "" {
		summary = "调用"
	}
	if endpoint == "/v1/chat/completions" && imageRequest {
		summary = "聊天生图"
	}
	if strings.HasPrefix(endpoint, "/api/accounts/") && strings.HasSuffix(endpoint, "/test") {
		imageRequest = text(payload["mode"]) == "image"
		summary = "账号对话测试"
		if imageRequest {
			summary = "账号画图测试"
		}
	}
	c := &CallRecorder{server: s, ctx: ctx, started: s.now(), endpoint: endpoint, model: model, summary: summary, callID: id(8), identity: Map{"id": identity["id"], "name": safeCallText(identity["name"], 256), "role": identity["role"]}, detail: Map{}, image: imageRequest}
	request := requestContentText(payload)
	excerpt := safeCallText(request, 1000)
	if excerpt != "" {
		c.detail["request_text"] = excerpt
		full := safeCallText(request, 50000)
		if full != excerpt {
			c.detail["request_text_full"] = full
			c.detail["request_text_truncated"] = len([]rune(safeCallText(request, 0))) > 50000
		}
	}
	if imageRequest {
		source := payload
		for _, tool := range dicts(payload["tools"]) {
			if tool["type"] == "image_generation" {
				source = merge(payload, tool)
				break
			}
		}
		metadata := Map{}
		for _, key := range []string{"n", "size", "quality", "response_format", "stream", "partial_images"} {
			if value, ok := source[key]; ok && value != nil && value != "" {
				switch value.(type) {
				case string, bool, int, int64, float64:
					metadata[key] = value
				default:
					if _, valid := number(value); valid {
						metadata[key] = value
					}
				}
			}
		}
		if count := len(imageInputs(payload["images"])); count > 0 {
			metadata["input_image_count"] = count
		}
		if len(metadata) > 0 {
			c.detail["request_meta"] = metadata
		}
	}
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	if s.callsClosing {
		return nil
	}
	s.calls.Add(1)
	if s.Monitor != nil {
		s.Monitor.Start(c.callID, endpoint, model, summary, text(identity["role"]), text(identity["name"]))
		s.Monitor.Stage(c.callID, "handler_started", nil)
	}
	return c
}

// waitCalls is the final persistence barrier for admitted public and task calls.
// Closing admission under callsMu prevents Add racing with Wait at shutdown.
func (s *Server) waitCalls(ctx context.Context) (bool, error) {
	done := make(chan struct{})
	go func() { s.calls.Wait(); close(done) }()
	var waitErr error
	finished := false
	select {
	case <-done:
		finished = true
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	return finished, errors.Join(waitErr, s.callsErr)
}

// Observe accepts native metadata and protocol events before their internal
// fields are stripped. It does not retain text/binary result payloads.
func (c *CallRecorder) Observe(value any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(value)
}
func (c *CallRecorder) observe(value any) {
	if c.server.Monitor != nil {
		if record, ok := value.(map[string]any); ok {
			c.server.Monitor.Merge(c.callID, record)
			for _, key := range []string{"_image_attempts", "image_attempts"} {
				c.server.Monitor.CaptureAttempts(c.callID, dicts(record[key]))
			}
		}
	}
	switch v := value.(type) {
	case map[string]any:
		for _, key := range []string{"_account_email", "account_email", "_conversation_id", "conversation_id", "_upstream_account_id"} {
			if x := text(v[key]); x != "" {
				target := strings.TrimPrefix(key, "_")
				if text(c.detail[target]) == "" {
					limit := 255
					if target == "account_email" {
						limit = 320
					}
					c.detail[target] = safeCallText(x, limit)
				}
			}
		}
		for _, key := range []string{"_call_status", "_call_error", "error_code"} {
			if value := v[key]; value != nil && value != "" {
				c.detail[strings.TrimPrefix(key, "_")] = safeCallText(value, 2000)
			}
		}
		if c.image {
			if items, ok := v["data"]; ok {
				c.dataCount = max(c.dataCount, len(list(items)))
				if len(list(v["_image_metadata"])) == 0 {
					for _, asset := range dicts(items) {
						if text(asset["url"]) == "" && text(asset["b64_json"]) == "" {
							continue
						}
						measured := Map{}
						for _, key := range []string{"width", "height"} {
							if n := integer(asset[key], 0); n > 0 {
								measured[key] = n
							}
						}
						c.images = append(c.images, measured)
					}
				}
			}
			for _, item := range dicts(v["_image_metadata"]) {
				image := Map{}
				for _, key := range []string{"width", "height"} {
					if n := integer(item[key], 0); n > 0 {
						image[key] = n
					}
				}
				c.images = append(c.images, image)
			}
			if b64, ok := v["b64_json"].(string); ok && b64 != "" {
				c.b64Count++
				c.b64Chars += len(b64)
			}
			if rawURL, ok := v["url"].(string); ok && rawURL != "" {
				c.urlCount++
			}
		}
		for _, key := range []string{"_image_urls", "urls"} {
			for _, u := range stringsOf(v[key]) {
				if callPublicURL(u) {
					c.urls = append(c.urls, u)
				}
			}
		}
		if u, ok := v["url"].(string); ok && callPublicURL(u) {
			c.urls = append(c.urls, u)
		}
		for _, key := range []string{"_image_attempts", "image_attempts"} {
			for _, attempt := range dicts(v[key]) {
				safe := safeCallAttempt(attempt)
				if len(safe) > 0 && !slices.ContainsFunc(c.attempts, func(existing Map) bool { return canonical(existing) == canonical(safe) }) {
					c.attempts = append(c.attempts, safe)
				}
			}
		}
		for key, nested := range v {
			if key == "_image_metadata" || key == "_image_attempts" || key == "image_attempts" || key == "_image_urls" || key == "urls" {
				continue
			}
			switch nested.(type) {
			case map[string]any, []any, []Map:
				c.observe(nested)
			}
		}
	case []any:
		for _, item := range v {
			c.observe(item)
		}
	case []Map:
		for _, item := range v {
			c.observe(item)
		}
	}
}
func callPublicURL(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "/images/") || strings.HasPrefix(value, "/image-thumbnails/")
}
func safeCallAttempt(raw Map) Map {
	out := Map{}
	for _, key := range []string{"slot", "attempt", "account_email", "status", "failure_code", "failure_scope", "failure_capability", "failure_retryable", "failure_account_failure", "failure_retry_after", "status_code", "error_type", "public_error", "account_failure", "switched_account", "conversation_id", "duration_ms"} {
		if value, ok := raw[key]; ok {
			switch value.(type) {
			case string:
				out[key] = safeCallText(value, 2000)
			case bool, int, int64, float64:
				out[key] = value
			default:
				if _, ok := number(value); ok {
					out[key] = value
				}
			}
		}
	}
	return out
}

func (c *CallRecorder) Finish(result any, executionErr error) error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		defer func() {
			c.server.callsMu.Lock()
			if c.finishErr != nil && c.server.callsErr == nil {
				c.server.callsErr = c.finishErr
			}
			c.server.callsMu.Unlock()
			c.server.calls.Done()
		}()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.observe(result)
		now := c.server.now()
		detail := copyMap(c.detail)
		status, suffix := "success", "调用完成"
		if s := text(detail["call_status"]); s == "failed" || s == "text_review" {
			status = s
			if status == "failed" {
				suffix = "调用失败"
			}
		}
		if executionErr != nil {
			status, suffix = "failed", "调用失败"
			detail["error"] = safeCallText(executionErr.Error(), 2000)
			var imageErr *ImageExecutionError
			if errors.As(executionErr, &imageErr) {
				detail["error_code"] = imageErr.Code
				detail["status_code"] = imageErr.Status
				for _, field := range imageTaskDetailKeys {
					if v := imageErr.Details[field]; v != nil && v != "" {
						detail[field] = v
					}
				}
				if v := text(imageErr.Details["conversation_id"]); v != "" {
					detail["conversation_id"] = safeCallText(v, 255)
					if imageErr.origin.conversationID == v {
						if imageErr.origin.email != "" {
							detail["account_email"] = safeCallText(imageErr.origin.email, 320)
						}
						if imageErr.origin.accountID != "" {
							detail["upstream_account_id"] = safeCallText(imageErr.origin.accountID, 255)
						}
					}
				}
				for _, attempt := range dicts(imageErr.Details["image_attempts"]) {
					c.attempts = append(c.attempts, safeCallAttempt(attempt))
				}
			}
		}
		if errorText := text(detail["call_error"]); errorText != "" && text(detail["error"]) == "" {
			detail["error"] = errorText
		}
		delete(detail, "call_status")
		delete(detail, "call_error")
		detail["key_id"] = c.identity["id"]
		detail["key_name"] = c.identity["name"]
		detail["role"] = c.identity["role"]
		detail["endpoint"] = c.endpoint
		detail["model"] = c.model
		detail["call_id"] = c.callID
		detail["started_at"] = imageTaskTime(c.started)
		detail["ended_at"] = imageTaskTime(now)
		detail["duration_ms"] = max(int64(0), now.Sub(c.started).Milliseconds())
		detail["status"] = status
		detail["image_request"] = c.image
		if c.endpoint != "/v1/search" && len(c.urls) > 0 {
			detail["urls"] = unique(c.urls)
		}
		if len(c.attempts) > 0 {
			detail["image_attempts"] = c.attempts
		}
		if c.image {
			c.dataCount = max(c.dataCount, c.b64Count)
			if c.dataCount > 0 {
				detail["result_data_count"] = c.dataCount
			}
			if c.urlCount > 0 {
				detail["result_url_count"] = c.urlCount
			}
			if c.b64Count > 0 {
				detail["result_b64_count"] = c.b64Count
				detail["result_b64_chars"] = c.b64Chars
			}
			if len(c.images) > 0 {
				detail["result_images"] = c.images
				detail["result_data_count"] = max(c.dataCount, len(c.images))
			}
		}
		if callTextFailure(text(detail["error_code"])) {
			detail["status"] = "text_review"
			suffix = "文本"
		}
		if c.resumed {
			suffix += "（续轮询）"
		}
		item := Map{"id": id(16), "time": imageTaskTime(now), "type": "call", "summary": c.summary + suffix, "detail": detail}
		if c.server.Monitor != nil {
			c.server.Monitor.Finish(detail)
		}
		projection := CallRecordProjection(item)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
		defer cancel()
		_, c.finishErr = repository.NewCallRecords(c.server.DB).Append(ctx, repository.CallRecordWrite{Payload: item, Outcome: text(projection["outcome"]), Endpoint: c.endpoint, Model: c.model, AccountEmail: text(detail["account_email"]), ConversationID: text(detail["conversation_id"]), BusinessKind: text(projection["business_kind"]), SearchText: text(projection["search_text"])})
	})
	return c.finishErr
}

func callTextFailure(code string) bool { return imageProblem(code, 0, "", nil).Status == 400 }
func callRateFailure(code string) bool {
	return slices.Contains([]string{"429", "file_upload_throttled", "image_quota_exhausted", "insufficient_quota", "limited", "quota_exhausted", "rate_limit", "rate_limit_exceeded", "rate_limited", "upstream_rate_limited", "限流"}, code)
}
func callValue(item Map, key string) any {
	if value := object(item["detail"])[key]; value != nil && value != "" {
		return value
	}
	return item[key]
}

// CallRecordProjection reproduces the Python repository's derived query fields;
// it is not a replacement for the later-stage Logs and metrics projections.
func CallRecordProjection(item Map) Map {
	kind, outcome := callBusiness(item), callOutcome(item)
	parts := []string{}
	for _, field := range []string{"id", "time", "type", "summary"} {
		parts = append(parts, text(item[field]))
	}
	for _, field := range []string{"endpoint", "model", "status", "key_id", "key_name", "account_email", "conversation_id", "request_text", "request_text_full", "error", "error_code", "reason", "stage"} {
		parts = append(parts, text(callValue(item, field)))
	}
	return Map{"outcome": outcome, "business_kind": kind, "search_text": strings.Join(parts, " ")}
}

// Stage reports one measured execution stage through the process-local owner.
func (c *CallRecorder) Stage(event string, data Map) {
	if c == nil || c.server.Monitor == nil {
		return
	}
	normalized := event
	if replacement := map[string]string{"getting_account": "image_getting_account", "uploading": "image_uploading", "bootstrapping": "image_bootstrapping", "getting_token": "image_getting_token", "preparing_conversation": "image_preparing_conversation", "starting_generation": "image_starting_generation", "generating": "image_generating", "receiving_image": "image_resolve_done"}[event]; replacement != "" {
		normalized = replacement
	}
	measured := copyMap(data)
	measured["_measure"] = true
	c.server.Monitor.Stage(c.callID, normalized, measured)
}
func recordPublicStage(ctx context.Context, event string, data Map) {
	if c, ok := ctx.Value(publicCallContextKey{}).(*CallRecorder); ok {
		c.Stage(event, data)
	}
}

// Account admission diagnostics belong to the final Call Record, independently
// of the monitor's shorter event window. Only stable IDs and fixed reasons enter
// the bounded sample; credentials and upstream response text are never retained.
func recordAccountSelection(ctx context.Context, event, accountID, reason string, elapsed time.Duration) {
	c, ok := ctx.Value(publicCallContextKey{}).(*CallRecorder)
	if !ok || c == nil {
		return
	}
	c.mu.Lock()
	detail := object(c.detail["account_selection"])
	switch event {
	case "checked":
		detail["checked"] = integer(detail["checked"], 0) + 1
	case "skipped":
		detail["skipped"] = integer(detail["skipped"], 0) + 1
		reasons := object(detail["skip_reasons"])
		reasons[reason] = integer(reasons[reason], 0) + 1
		detail["skip_reasons"] = reasons
		samples := dicts(detail["skips"])
		if len(samples) < 100 {
			detail["skips"] = append(samples, Map{"upstream_account_id": accountID, "reason": reason})
		} else {
			detail["skips_truncated"] = integer(detail["skips_truncated"], 0) + 1
		}
	case "waiting":
		detail["wait_count"] = integer(detail["wait_count"], 0) + 1
		detail["last_wait_reason"] = reason
	case "finished":
		detail["selection_ms"] = integer(detail["selection_ms"], 0) + max(0, int(elapsed.Milliseconds()))
		detail["stop_reason"] = reason
	}
	c.detail["account_selection"] = detail
	c.mu.Unlock()
	if event == "skipped" || event == "waiting" {
		if accountID != "" {
			reason += " [" + accountID + "]"
		}
		c.Stage("image_getting_account", monitorImageData(ctx, Map{"local_reason": reason}))
	}
}
