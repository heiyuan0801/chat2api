package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

// GenerateImageRequest executes one native upstream attempt. Output count,
// account switching, asset publication, and public envelopes belong to callers.
func (c *Client) GenerateImageRequest(ctx context.Context, in ImageRequest, progress func(string)) (ImageResult, error) {
	if c.cfg.AccessToken == "" {
		return ImageResult{}, failure("access_token_required", "validation")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return ImageResult{}, failure("prompt_required", "validation")
	}
	if len(in.Prompt) > 1<<20 || len(in.Size) > 128 || len(in.Quality) > 128 {
		return ImageResult{}, failure("input_too_large", "validation")
	}
	if e := validateImageInputs(in.Images); e != nil {
		return ImageResult{}, e
	}
	in.Model = strings.ToLower(strings.TrimSpace(in.Model))
	for _, prefix := range []string{"plus-", "team-", "pro-"} {
		if in.Model == prefix+"codex-gpt-image-2" {
			in.Model = "codex-gpt-image-2"
		}
	}
	if in.Model == "codex-gpt-image-2" {
		return c.generateCodexImage(ctx, in, progress)
	}
	if _, e := WebImageRoute(in.Model); e != nil {
		return ImageResult{}, e
	}
	hints := ""
	if in.Size != "" {
		hints += "输出图片尺寸为 " + in.Size + "。"
	}
	if in.Quality != "" {
		hints += "输出图片质量为 " + in.Quality + "。"
	}
	if hints != "" {
		in.Prompt = strings.TrimSpace(in.Prompt) + "\n\n" + hints
	}
	return c.generateWebImage(ctx, in, progress, true)
}

func (c *Client) generateCodexImage(ctx context.Context, in ImageRequest, progress func(string)) (ImageResult, error) {
	ctx, end, e := c.begin(ctx)
	if e != nil {
		return ImageResult{}, e
	}
	defer end()
	operationCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, c.cfg.StreamTimeout)
	defer cancel()
	reportProgress(progress, "starting_generation")
	content := []any{map[string]any{"type": "input_text", "text": in.Prompt}}
	for _, im := range in.Images {
		mime := im.MIME
		if mime == "" {
			mime = "image/png"
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(im.Data)})
	}
	action := "generate"
	if len(in.Images) > 0 {
		action = "edit"
	}
	size := in.Size
	if size == "" {
		size = "1024x1024"
	}
	quality := in.Quality
	if quality == "" {
		quality = "auto"
	}
	payload := map[string]any{"model": "gpt-5.5", "instructions": "Use the image_generation tool to create exactly one image for the user's request. Return the generated image result.", "store": false, "input": []any{map[string]any{"role": "user", "content": content}}, "tools": []any{map[string]any{"type": "image_generation", "model": "gpt-image-2", "action": action, "size": size, "quality": quality, "output_format": "png"}}, "tool_choice": map[string]any{"type": "image_generation"}, "stream": true}
	path := "/backend-api/codex/responses"
	h := http.Header{"authorization": {"Bearer " + c.cfg.AccessToken}, "content-type": {"application/json"}, "accept": {"text/event-stream"}}
	resp, e := c.request(ctx, "POST", path, "codex_image", payload, h)
	if e != nil {
		return ImageResult{}, codexOperationError(operationCtx, ctx, e)
	}
	defer resp.Body.Close()
	reportProgress(progress, "generating")
	result := ImageResult{}
	seen := map[string]bool{}
	terminal := false
	var resultErr error
	consume := func(value map[string]any) error {
		if kind := structuredImageFailure(value); kind != "" {
			resultErr = failure(kind, "codex_image")
		}
		var visit func(any) error
		visit = func(v any) error {
			switch item := v.(type) {
			case map[string]any:
				if item["type"] == "image_generation_call" {
					encoded := strings.TrimSpace(text(item["result"]))
					if strings.HasPrefix(encoded, "data:image/") {
						_, encoded, _ = strings.Cut(encoded, ",")
					}
					if encoded != "" && !seen[encoded] {
						if len(encoded) > base64.StdEncoding.EncodedLen(50<<20) {
							return failure("response_too_large", "codex_image")
						}
						data, e := base64.StdEncoding.Strict().DecodeString(encoded)
						if e != nil || len(data) == 0 {
							return failure("invalid_image_result", "codex_image")
						}
						if len(result.Assets) >= 16 {
							return failure("too_many_images", "codex_image")
						}
						total := len(data)
						for _, a := range result.Assets {
							total += len(a.Data)
						}
						if total > 100<<20 {
							return failure("response_too_large", "codex_image")
						}
						result.Assets = append(result.Assets, ImageAsset{Data: data})
						seen[encoded] = true
					}
					return nil
				}
				// Only explicit output_text items are eligible user-facing text.
				if item["type"] == "output_text" {
					if t := text(item["text"]); t != "" {
						result.Text = t
					}
				}
				for _, child := range item {
					if e := visit(child); e != nil {
						return e
					}
				}
			case []any:
				for _, child := range item {
					if e := visit(child); e != nil {
						return e
					}
				}
			}
			return nil
		}
		if e := visit(value); e != nil {
			return e
		}
		switch value["type"] {
		case "response.completed", "response.failed", "response.incomplete":
			terminal = true
			if value["type"] != "response.completed" && resultErr == nil {
				resultErr = failure("image_task_failed", "codex_image")
			}
		}
		return nil
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (96<<20)+1))
		if readErr != nil {
			return result, codexOperationError(operationCtx, ctx, requestError(ctx, "codex_image", readErr))
		}
		if len(data) > 96<<20 {
			return result, failure("response_too_large", "codex_image")
		}
		var value map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&value) != nil || value == nil {
			return result, failure("invalid_json", "codex_image")
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			return result, failure("invalid_json", "codex_image")
		}
		terminal = true
		e = consume(value)
	} else {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return result, failure("expected_sse", "codex_image")
		}
		e = ReadSSE(ctx, resp.Body, 72<<20, c.cfg.MaxStreamBytes, func(ev Event) error {
			if ev.Data == "[DONE]" {
				terminal = true
				return errStreamDone
			}
			var value map[string]any
			if json.Unmarshal([]byte(ev.Data), &value) != nil || value == nil {
				return failure("invalid_sse_json", "codex_image")
			}
			if e := consume(value); e != nil {
				return e
			}
			if terminal {
				return errStreamDone
			}
			return nil
		})
	}
	if e != nil {
		if ctx.Err() != nil {
			return result, codexOperationError(operationCtx, ctx, requestError(ctx, "codex_image", ctx.Err()))
		}
		return result, e
	}
	if len(result.Assets) > 0 {
		reportProgress(progress, "receiving_image")
		return result, nil
	}
	if resultErr != nil {
		return result, resultErr
	}
	if result.Text != "" {
		return result, failure("upstream_text_reply", "codex_image")
	}
	if !terminal {
		return result, failure("incomplete_stream", "codex_image")
	}
	return result, failure("no_image_generated", "codex_image")
}

// PollImageWithOptions resumes discovery only. It never submits a generation.
func (c *Client) PollImageWithOptions(ctx context.Context, conversationID string, progress func(string)) (ImageResult, error) {
	if c.cfg.AccessToken == "" {
		return ImageResult{}, failure("access_token_required", "validation")
	}
	if len(conversationID) == 0 || len(conversationID) > 128 || !idPattern.MatchString(conversationID) {
		return ImageResult{}, failure("invalid_conversation_id", "validation")
	}
	ctx, end, e := c.begin(ctx)
	if e != nil {
		return ImageResult{}, e
	}
	defer end()
	state := &conversationState{ConversationID: conversationID, imageMode: true}
	reportProgress(progress, "image_stream_resolve_start")
	if e = c.pollImageConfigured(ctx, state); e != nil {
		return ImageResult{ConversationID: conversationID, Text: state.Text}, e
	}
	reportProgress(progress, "receiving_image")
	assets, e := c.resolveAssets(ctx, state)
	return ImageResult{ConversationID: conversationID, Text: state.Text, Assets: assets}, e
}
func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
func waitImage(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func retryablePoll(e error) bool {
	var controlled *Error
	if !errors.As(e, &controlled) {
		return false
	}
	return controlled.Kind == "network_error" || controlled.Kind == "stream_read_error" || controlled.Status == 404 || controlled.Status == 409 || controlled.Status == 423 || controlled.Status == 429 || controlled.Status == 408 || controlled.Status >= 500
}
func (c *Client) pollImageConfigured(parent context.Context, state *conversationState) error {
	if state.ConversationID == "" {
		if state.Text != "" {
			return failure("upstream_text_reply", "image_poll")
		}
		return failure("missing_conversation_id", "image_poll")
	}
	check, settle := boolDefault(c.cfg.CheckBeforeHit, true), boolDefault(c.cfg.SettleEnabled, true)
	if len(state.Pointers) > 0 && !check && !settle {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.PollTimeout)
	defer cancel()
	initial := c.cfg.PollInitialWait
	initialMetric := "poll_sleep_ms"
	if len(state.Pointers) > 0 && settle {
		initial = c.cfg.SettleTime
		initialMetric = "poll_settle_ms"
	}
	if e := c.waitImageTiming(ctx, initial, initialMetric); e != nil {
		if len(state.Pointers) > 0 {
			return nil
		}
		return pollDeadline(parent, ctx)
	}
	previous := append([]AssetPointer(nil), state.Pointers...)
	attempt := 0
	var taskErr error
	for ctx.Err() == nil {
		attempt++
		path := "/backend-api/conversation/" + state.ConversationID
		conversation, e := c.imagePollJSON(ctx, path, "image_poll", "poll_conversation_ms")
		var convErr error
		if e == nil {
			// Only the current turn can commit outputs; stale branches remain excluded.
			for _, m := range currentTurnMessages(conversation) {
				if err := state.pollMessage(m); err != nil {
					convErr = mergeUpstreamImageError(convErr, err)
				}
			}
		}
		// A committed conversation result needs no diagnostic task lookup.
		// Pending or unavailable conversations still consult tasks, including
		// task-only outputs and structured failures.
		if len(state.Pointers) == 0 {
			// Task probes are diagnostic. A transient probe failure must not hide a
			// committed result in the conversation document.
			tasksPath := "/backend-api/tasks"
			taskCtx, taskCancel := context.WithTimeout(ctx, 5*time.Second)
			tasks, e := c.imagePollJSON(taskCtx, tasksPath, "image_tasks", "poll_tasks_ms")
			taskCancel()
			if e != nil {
				var controlled *Error
				if errors.As(e, &controlled) && controlled.Status == 401 {
					if len(state.Pointers) > 0 {
						return nil
					}
					return e
				}
			} else {
				for _, value := range slice(tasks["tasks"]) {
					task := object(value)
					if text(task["conversation_id"]) != state.ConversationID && text(task["original_conversation_id"]) != state.ConversationID {
						continue
					}
					if m := object(task["image_gen_message"]); m != nil {
						if kind := structuredImageFailure(m); kind != "" {
							taskErr = mergeUpstreamImageError(taskErr, failure(kind, "image_tasks"))
						}
						if e := state.pollMessage(m); e != nil {
							taskErr = mergeUpstreamImageError(taskErr, e)
						}
					}
					if task["status"] == "failed" && taskErr == nil {
						taskErr = failure("image_task_failed", "image_tasks")
					}
				}
			}
		}

		if e != nil {
			if len(state.Pointers) > 0 {
				return nil
			}
			if ctx.Err() != nil {
				break
			}
			if !retryablePoll(e) {
				return e
			}
			delay := time.Duration(1<<min(attempt, 4)) * time.Second
			var controlled *Error
			if errors.As(e, &controlled) && controlled.RetryAfterPresent {
				delay = time.Duration(controlled.RetryAfterSeconds) * time.Second
			}
			if e := c.waitImageTiming(ctx, delay, "poll_sleep_ms"); e != nil {
				break
			}
			continue
		}
		if len(state.Pointers) > 0 {
			if !check || samePointers(previous, state.Pointers) || !settle {
				return nil
			}
			previous = append([]AssetPointer(nil), state.Pointers...)
			if e := c.waitImageTiming(ctx, c.cfg.SettleTime, "poll_settle_ms"); e != nil {
				return nil
			}
			continue
		}
		if state.imageArguments {
			return failure("image_tool_error", "image_poll")
		}
		combined := mergeUpstreamImageError(taskErr, convErr)
		if state.assistantTerminal && state.Text != "" {
			combined = mergeUpstreamImageError(combined, failure("upstream_text_reply", "image_poll"))
		}
		if combined != nil {
			return combined
		}
		if e := c.waitImageTiming(ctx, c.cfg.PollInterval, "poll_sleep_ms"); e != nil {
			break
		}
	}
	if len(state.Pointers) > 0 {
		return nil
	}
	return pollDeadline(parent, ctx)
}
func pollDeadline(parent, ctx context.Context) error {
	if parent.Err() != nil {
		return requestError(parent, "image_poll", parent.Err())
	}
	if ctx.Err() != nil {
		return failure("image_poll_timeout", "image_poll")
	}
	return failure("no_image_generated", "image_poll")
}
func samePointers(a, b []AssetPointer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func codexOperationError(operation, stream context.Context, err error) error {
	if operation.Err() == nil && errors.Is(stream.Err(), context.DeadlineExceeded) {
		return failure("upstream_connection_timeout", "codex_image")
	}
	return err
}

// Timing callbacks carry fixed phase names and durations only, never transport
// paths, payloads, credentials or signed asset addresses.
func (c *Client) imagePollJSON(ctx context.Context, path, stage, metric string) (map[string]any, error) {
	started := time.Now()
	defer func() {
		if c.cfg.ImageTiming != nil {
			c.cfg.ImageTiming(metric, time.Since(started))
		}
	}()
	return c.json(ctx, "GET", path, stage, nil, c.headers(path, true))
}
func (c *Client) waitImageTiming(ctx context.Context, d time.Duration, metric string) error {
	started := time.Now()
	defer func() {
		if c.cfg.ImageTiming != nil {
			c.cfg.ImageTiming(metric, time.Since(started))
		}
	}()
	return waitImage(ctx, d)
}
