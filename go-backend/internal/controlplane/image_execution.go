package controlplane

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
)

//go:embed image_failures.json
var imageFailureSpec []byte
var imageFailureContracts = func() struct {
	Policies map[string]Map    `json:"policies"`
	Aliases  map[string]string `json:"aliases"`
} {
	var spec struct {
		Policies map[string]Map    `json:"policies"`
		Aliases  map[string]string `json:"aliases"`
	}
	if json.Unmarshal(imageFailureSpec, &spec) != nil {
		panic("invalid image failure contracts")
	}
	return spec
}()

type imageConversationOrigin struct{ accountID, email, conversationID string }
type imageResumeOriginKey struct{}
type imageResumeOrigin struct{ accountID, ownerID string }

type ImageExecutionError struct {
	origin        imageConversationOrigin
	Code, Message string
	Status        int
	Details       Map
}

func (e *ImageExecutionError) Error() string { return e.Message }
func imageProblem(code string, status int, message string, details Map) *ImageExecutionError {
	if alias := imageFailureContracts.Aliases[code]; alias != "" {
		code = alias
	}
	policy, ok := imageFailureContracts.Policies[code]
	if !ok {
		code = "upstream_error"
		policy = imageFailureContracts.Policies[code]
	}
	out := copyMap(policy)
	out["failure_code"] = code
	out["failure_retry_after"] = nil
	if status == 0 {
		status = integer(policy["status_code"], 502)
	}
	out["status_code"] = status
	if message == "" {
		message = "The image generation tool encountered an error. Please try again."
		if code == "image_poll_timeout" {
			message = "Image generation timed out. Please try again."
		}
		if code == "image_quota_exhausted" || code == "insufficient_quota" {
			message = "No image generation quota is currently available."
		}
	}
	for k, v := range details {
		out[k] = v
	}
	if status == 400 {
		out["outcome"] = "text"
	}
	return &ImageExecutionError{Code: code, Message: message, Status: status, Details: out}
}
func classifyExecutionError(err error) *ImageExecutionError {
	var known *ImageExecutionError
	if errors.As(err, &known) {
		return known
	}
	if errors.Is(err, context.Canceled) {
		return imageProblem("task_interrupted", 0, "", nil)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return imageProblem("image_poll_timeout", 0, "", nil)
	}
	var pool *PoolError
	if errors.As(err, &pool) {
		code := "no_available_account"
		if pool.Kind == "quota_exhausted" {
			code = "insufficient_quota"
		}
		if pool.Kind == "deadline_exceeded" {
			code = "image_poll_timeout"
		}
		return imageProblem(code, 0, "", nil)
	}
	var remote *upstream.Error
	if errors.As(err, &remote) {
		code := remote.Kind
		switch code {
		case "network_error":
			code = "upstream_connection_failed"
		case "rate_limited":
			code = "upstream_rate_limited"
		case "poll_timeout":
			code = "image_poll_timeout"
		case "incomplete_stream", "truncated_stream":
			code = "image_stream_interrupted"
		case "invalid_image_model":
			code = "unsupported_model"
		case "content_blocked":
			code = "content_policy_violation"
		case "upstream_message_failed":
			code = "image_tool_error"
		case "image_task_failed":
			code = "upstream_error"
		case "asset_not_found", "invalid_asset", "image_download_failed", "invalid_asset_url", "invalid_image_result":
			code = "image_download_failed"
		case "access_token_required":
			code = "auth_invalid"
		case "canceled":
			code = "upstream_connection_timeout"
		}
		status := 0
		// The actual HTTP status owns the request/account boundary. A structured
		// body cannot make a real 401 a policy rejection, or a real 400 an account
		// failure. Keep the baseline's policy refinements only at equal status.
		if remote.Status > 0 {
			policyStatus := integer(imageFailureContracts.Policies[code]["status_code"], 0)
			switch {
			case remote.Status == 400:
				if policyStatus != 400 {
					code = "invalid_image_input"
				}
			case remote.Status == 401:
				code = "auth_invalid"
			case code == "conversation_not_ready" && (remote.Status == 404 || remote.Status == 409 || remote.Status == 423):
				status = remote.Status
			case remote.Status == 429 && strings.Contains(remote.Stage, "upload"):
				code = "file_upload_throttled"
			case policyStatus == remote.Status:
			case remote.Status == 403 || remote.Status == 423:
				code, status = "upstream_unavailable", remote.Status
			case remote.Status == 429:
				code = "upstream_rate_limited"
			case remote.Status == 408 || remote.Status == 504:
				code = "upstream_connection_timeout"
			case remote.Status >= 500:
				code, status = "upstream_unavailable", remote.Status
			default:
				code, status = "upstream_error", remote.Status
			}
		}
		details := Map{}
		if remote.RetryAfterSeconds > 0 {
			details["failure_retry_after"] = remote.RetryAfterSeconds
		}
		return imageProblem(code, status, "", details)
	}
	return imageProblem("internal_error", 0, "", nil)
}

// ImageBackend is one upstream attempt. Pool scheduling, switching, slots,
// public result formatting and persistence stay with NativeImageExecutor.
type ImageBackend interface {
	Generate(context.Context, upstream.ImageRequest, Map, ProxyProfile, Map, func(string)) (upstream.ImageResult, error)
	Poll(context.Context, string, Map, ProxyProfile, Map, func(string)) (upstream.ImageResult, error)
	Download(context.Context, string) ([]byte, error)
}
type NativeImageBackend struct {
	newClient func(upstream.Config) (*upstream.Client, error)
}

func (b NativeImageBackend) client(cfg upstream.Config) (*upstream.Client, error) {
	if b.newClient != nil {
		return b.newClient(cfg)
	}
	return upstream.New(cfg)
}

func settingSeconds(values Map, key string, fallback float64) time.Duration {
	n, ok := number(values[key])
	if !ok {
		n = fallback
	}
	return time.Duration(n * float64(time.Second))
}
func nativeImageConfig(account Map, profile ProxyProfile, settings Map) upstream.Config {
	stream := settingSeconds(settings, "image_stream_timeout_secs", 80)
	poll := settingSeconds(settings, "image_poll_timeout_secs", 60)
	check, settle := boolean(settings["image_check_before_hit_enabled"], true), boolean(settings["image_settle_enabled"], true)
	cfg := upstream.Config{AccessToken: text(account["access_token"]), ProxyURL: profile.ProxyURL, Timeout: stream + poll + 30*time.Second, StreamTimeout: stream, PollTimeout: poll, PollInitialWait: settingSeconds(settings, "image_poll_initial_wait_secs", 5), PollInitialWaitConfigured: true, PollInterval: settingSeconds(settings, "image_poll_interval_secs", 5), CheckBeforeHit: &check, SettleEnabled: &settle, SettleTime: settingSeconds(settings, "image_settle_secs", 5), MaxBodyBytes: 64 << 20, MaxEventBytes: 8 << 20, MaxStreamBytes: 256 << 20}
	applyProxySession(profile, &cfg)
	return cfg
}
func recordImageTiming(ctx context.Context, metric string, elapsed time.Duration) {
	if c, ok := ctx.Value(publicCallContextKey{}).(*CallRecorder); ok && c.server.Monitor != nil {
		slot := monitorInt(object(ctx.Value(monitorImageSlotKey{}))["index"])
		c.server.Monitor.imageTiming(c.callID, slot, metric, elapsed)
	}
}
func nativeImageObservedConfig(ctx context.Context, account Map, profile ProxyProfile, settings Map) upstream.Config {
	cfg := nativeImageConfig(account, profile, settings)
	cfg.ImageTiming = func(metric string, elapsed time.Duration) { recordImageTiming(ctx, metric, elapsed) }
	return cfg
}
func (b NativeImageBackend) Generate(ctx context.Context, request upstream.ImageRequest, account Map, profile ProxyProfile, settings Map, progress func(string)) (upstream.ImageResult, error) {
	c, e := b.client(nativeImageObservedConfig(ctx, account, profile, settings))
	if e != nil {
		return upstream.ImageResult{}, e
	}
	defer c.Close()
	result, err := c.GenerateImageRequest(ctx, request, progress)
	if err == nil {
		startedDownload := time.Now()
		err = hydrateNativeImages(ctx, c, &result)
		recordImageTiming(ctx, "asset_download_ms", time.Since(startedDownload))
	}
	return result, err
}
func (b NativeImageBackend) Poll(ctx context.Context, conversation string, account Map, profile ProxyProfile, settings Map, progress func(string)) (upstream.ImageResult, error) {
	c, e := b.client(nativeImageObservedConfig(ctx, account, profile, settings))
	if e != nil {
		return upstream.ImageResult{}, e
	}
	defer c.Close()
	result, err := c.PollImageWithOptions(ctx, conversation, progress)
	if err == nil {
		startedDownload := time.Now()
		err = hydrateNativeImages(ctx, c, &result)
		recordImageTiming(ctx, "asset_download_ms", time.Since(startedDownload))
	}
	return result, err
}
func hydrateNativeImages(ctx context.Context, c *upstream.Client, result *upstream.ImageResult) error {
	for i := range result.Assets {
		asset := &result.Assets[i]
		if len(asset.Data) != 0 {
			continue
		}
		data, err := c.DownloadGeneratedImage(ctx, asset.URL, maxImageBytes)
		if err != nil {
			return imageProblem("image_download_failed", 0, "", nil)
		}
		asset.Data = data
	}
	return nil
}
func (NativeImageBackend) Download(ctx context.Context, raw string) ([]byte, error) {
	var data []byte
	var err error
	for range 2 {
		data, _, err = upstream.FetchPublicImage(ctx, raw, maxImageBytes, false)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	return data, err
}

type ImageExecution interface {
	Execute(context.Context, Map, func(string), func([]Map)) (Map, error)
	Resume(context.Context, string, Map, time.Duration, func([]Map)) (Map, error)
}
type NativeImageExecutor struct {
	active  sync.WaitGroup
	mu      sync.Mutex
	closing bool
	server  *Server
	backend ImageBackend
}

func imageInputs(v any) []upstream.ImageInput {
	if x, ok := v.([]upstream.ImageInput); ok {
		return x
	}
	return nil
}
func imagePoolRequest(model string) (PoolRequest, string, error) {
	lower := strings.ToLower(strings.TrimSpace(model))
	r := PoolRequest{}
	for _, prefix := range []string{"plus", "team", "pro"} {
		if lower == prefix+"-codex-gpt-image-2" {
			r.Plan = prefix
			lower = "codex-gpt-image-2"
		}
	}
	if lower == "codex-gpt-image-2" {
		r.Source = "codex"
		r.ImageRoute = "codex:gpt-image-2"
		if r.Plan == "" {
			r.Plans = []string{"plus", "team", "pro"}
		}
		return r, lower, nil
	}
	for _, m := range webImageModels {
		if lower == m {
			r.ImageRoute, _ = upstream.WebImageRoute(lower)
			return r, lower, nil
		}
	}
	models := append(append([]string{}, webImageModels...), "codex-gpt-image-2", "plus-codex-gpt-image-2", "team-codex-gpt-image-2", "pro-codex-gpt-image-2")
	sort.Strings(models)
	return r, "", imageProblem("unsupported_model", 0, "unsupported image model,supported models: "+strings.Join(models, ", "), nil)
}
func imageFailureRank(code string) int {
	switch code {
	case "auth_invalid":
		return 8
	case "image_quota_exhausted", "insufficient_quota":
		return 7
	case "file_upload_throttled", "upstream_rate_limited":
		return 6
	case "image_download_failed":
		return 5
	case "content_policy_violation", "invalid_image_input", "unsupported_model":
		return 4
	case "image_tool_error":
		return 3
	case "upstream_text_reply":
		return 2
	}
	return 1
}
func (e *NativeImageExecutor) Execute(parent context.Context, payload Map, progress func(string), partial func([]Map)) (Map, error) {
	if !e.admit() {
		return nil, context.Canceled
	}
	defer e.active.Done()
	s := e.server
	settings, err := s.Settings.runtimeValues(parent)
	if err != nil {
		return nil, err
	}
	model := orValue(text(payload["model"]), "gpt-image-2").(string)
	pool, baseModel, err := imagePoolRequest(model)
	if err != nil {
		return nil, err
	}
	n := integer(payload["n"], 1)
	if n < 1 || n > 4 {
		return nil, imageProblem("invalid_image_input", 400, "n must be between 1 and 4", nil)
	}
	inputs := imageInputs(payload["images"])
	masks := imageInputs(payload["mask"])
	if len(masks) > 0 {
		inputs, err = compositeImageMasks(parent, inputs, masks)
		if err != nil {
			return nil, err
		}
	}
	if payload["_edit"] == true && len(inputs) == 0 {
		return nil, imageProblem("invalid_image_input", 400, "image is required", nil)
	}
	duration := time.Duration(integer(settings["image_stream_timeout_secs"], 80)+integer(settings["image_poll_timeout_secs"], 60)+30) * time.Second
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	request := upstream.ImageRequest{Prompt: publicString(payload["prompt"]), Model: baseModel, Size: publicString(payload["size"]), Quality: orValue(publicString(payload["quality"]), "auto").(string), Images: inputs}
	if request.Prompt == "" {
		return nil, imageProblem("invalid_image_input", 400, "prompt is required", nil)
	}
	type completed struct {
		result Map
		err    error
		slot   int
	}
	done := make(chan completed, n)
	parallel := boolean(settings["image_parallel_generation"], true)
	var callbackMu sync.Mutex
	report := func(stage string) {
		if progress != nil {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			progress(stage)
		}
	}
	run := func(slot int) {
		defer e.active.Done()
		slotCtx := context.WithValue(ctx, monitorImageSlotKey{}, Map{"index": slot, "total": n})
		slotReport := func(stage string) {
			if stage != "image_egress_ready" {
				recordPublicStage(slotCtx, stage, monitorImageData(slotCtx, nil))
			}
			report(stage)
		}
		result, err := e.one(slotCtx, request, pool, payload, settings, slotReport)
		done <- completed{result, err, slot}
	}
	if parallel {
		for slot := range n {
			e.active.Add(1)
			go run(slot + 1)
		}
	} else {
		e.active.Add(1)
		go func() {
			defer e.active.Done()
			for slot := range n {
				if ctx.Err() != nil {
					done <- completed{nil, ctx.Err(), slot + 1}
				} else {
					e.active.Add(1)
					run(slot + 1)
				}
			}
		}()
	}
	data := []Map{}
	urls := []string{}
	attempts := []Map{}
	conversation, email, originAccountID := "", "", ""
	var selected *ImageExecutionError
	var deadlineErr error
	requestDone := ctx.Done()
	var settle <-chan time.Time
collect:
	for remaining := n; remaining > 0; {
		var completed completed
		select {
		case completed = <-done:
			remaining--
		case <-requestDone:
			deadlineErr = ctx.Err()
			if !errors.Is(deadlineErr, context.DeadlineExceeded) {
				break collect
			}
			// Cancelled upstream workers return the originating conversation and
			// account with their final error. Collect that proof for bounded time
			// after a deadline; an explicit caller cancellation still exits now.
			requestDone = nil
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			settle = timer.C
			continue
		case <-settle:
			break collect
		}
		if completed.err != nil {
			failure := classifyExecutionError(completed.err)
			for _, attempt := range mapSlice(failure.Details["image_attempts"]) {
				item := copyMap(attempt)
				item["slot"] = completed.slot
				attempts = append(attempts, item)
			}
			if selected == nil || imageFailureRank(failure.Code) > imageFailureRank(selected.Code) {
				selected = failure
			}
			continue
		}
		result := completed.result
		data = append(data, mapSlice(result["data"])...)
		urls = append(urls, stringsOf(result["_image_urls"])...)
		for _, attempt := range mapSlice(result["_image_attempts"]) {
			item := copyMap(attempt)
			item["slot"] = completed.slot
			attempts = append(attempts, item)
		}
		if conversation == "" && text(result["_conversation_id"]) != "" {
			conversation = text(result["_conversation_id"])
			originAccountID = text(result["_upstream_account_id"])
			email = text(result["_account_email"])
		}
		if email == "" {
			email = text(result["_account_email"])
		}
		if partial != nil && len(data) > 0 {
			copyItems := make([]Map, len(data))
			for i, item := range data {
				copyItems[i] = copyMap(item)
			}
			partial(copyItems)
		}
	}
	if len(data) == 0 {
		if selected != nil {
			selected.Details["image_attempts"] = attempts
			return nil, selected
		}
		if deadlineErr != nil {
			return nil, classifyExecutionError(deadlineErr)
		}
		return nil, imageProblem("no_image_generated", 0, "image generation completed without output", nil)
	}
	// A request-level text/policy outcome outranks earlier image outputs, matching
	// collect_image_outputs. Ordinary failed slots retain partial success.
	if selected != nil && selected.Status == 400 {
		selected.Details["image_attempts"] = attempts
		return nil, selected
	}
	result := Map{"created": s.now().Unix(), "data": data, "usage": publicTokenUsage(countPublicTextTokens(request.Prompt, model), publicInputImageTokens(inputs), 0, countOutputImageTokens(data, request.Size, request.Quality))}
	if conversation != "" {
		result["_conversation_id"] = conversation
	}
	if email != "" {
		result["_account_email"] = email
	}
	if originAccountID != "" {
		result["_upstream_account_id"] = originAccountID
	}
	if len(urls) > 0 {
		result["_image_urls"] = unique(urls)
	}
	if len(attempts) > 0 {
		result["_image_attempts"] = attempts
	}
	observeImageIdentity(ctx, conversation, email, originAccountID)
	return result, nil
}
func (e *NativeImageExecutor) one(ctx context.Context, request upstream.ImageRequest, pool PoolRequest, payload, settings Map, progress func(string)) (Map, error) {
	s := e.server
	backendRequest := request
	if global := text(settings["global_system_prompt"]); global != "" && pool.Source != "codex" {
		backendRequest.Prompt = global + "\n\n" + request.Prompt
	}
	maxAttempts := 1
	if boolean(settings["image_account_retry_enabled"], true) {
		maxAttempts = max(1, min(20, integer(settings["image_max_account_attempts"], 4)))
	}
	attempts := []Map{}
	var last *ImageExecutionError
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, classifyExecutionError(ctx.Err())
		}
		progress("getting_account")
		lease, err := s.Accounts.AcquireImage(ctx, pool)
		if err != nil {
			if last != nil {
				return nil, last
			}
			return nil, classifyExecutionError(err)
		}
		account := lease.Account
		pool.ExcludedTokens = append(pool.ExcludedTokens, text(account["access_token"]))
		started := time.Now()
		evidenceStarted := s.now()
		profile := lease.Egress.Profile
		recordPublicStage(ctx, "image_egress_ready", monitorImageData(ctx, Map{"attempt": attempt, "max_account_attempts": maxAttempts, "account_switch_count": attempt - 1, "account_email": account["email"], "proxy_source": profile.Source, "egress_label": profile.EgressLabel, "egress_key": profile.egressKey, "proxy_group_id": profile.GroupID, "proxy_node_id": profile.NodeID, "proxy_node_name": profile.NodeName, "image_egress_limit": profile.ImageConcurrencyLimit, "has_proxy": profile.ProxyURL != "", "egress_wait_ms": profile.WaitMS}))
		progress("image_egress_ready")
		upstreamResult, err := e.backend.Generate(ctx, backendRequest, account, lease.Egress.Profile, settings, progress)
		// A configured fallback is used once for a connection failure before a
		// conversation exists; it never retries a started generation or poll.
		var transportErr *upstream.Error
		safeEarlyStage := errors.As(err, &transportErr) && (transportErr.Stage == "bootstrap" || transportErr.Stage == "requirements_prepare" || transportErr.Stage == "requirements_finalize" || transportErr.Stage == "prepare_conversation" || transportErr.Stage == "image_upload")
		if err != nil && upstreamResult.ConversationID == "" && safeEarlyStage {
			failure := classifyExecutionError(err)
			if failure.Code == "upstream_connection_failed" || failure.Code == "upstream_connection_timeout" {
				fallback, fallbackErr := s.Proxy.Fallback(ctx, false, true)
				if fallbackErr == nil && fallback != nil {
					lease.Egress.Release()
					upstreamResult, err = e.backend.Generate(ctx, backendRequest, account, fallback.Profile, settings, progress)
					fallback.Release()
				}
			}
		}
		if len(upstreamResult.Assets) == 0 && upstreamResult.Text != "" && (err == nil || classifyExecutionError(err).Code == "upstream_text_reply") {
			err = imageProblem("upstream_text_reply", 400, diagnostic(upstreamResult.Text, account, 2000), Map{"conversation_id": upstreamResult.ConversationID})
		}
		consumed := len(upstreamResult.Assets) > 0
		if err == nil && len(upstreamResult.Assets) == 0 {
			err = imageProblem("no_image_generated", 0, "", Map{"conversation_id": upstreamResult.ConversationID})
		}
		result := Map{}
		if err == nil {
			progress("receiving_image")
			startedPublish := time.Now()
			result, err = e.format(ctx, upstreamResult, payload, settings)
			recordImageTiming(ctx, "asset_publish_ms", time.Since(startedPublish))
		}
		var failure *ImageExecutionError
		if err != nil {
			failure = classifyExecutionError(err)
			if upstreamResult.ConversationID != "" {
				failure.Details["conversation_id"] = upstreamResult.ConversationID
				failure.origin = imageConversationOrigin{text(account["management_id"]), text(account["email"]), upstreamResult.ConversationID}
				if failure.Code == "image_poll_timeout" {
					failure.Details["can_resume_poll"] = true
				}
			}
		}
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if outcomeErr := s.Accounts.recordImageOutcome(recordCtx, account, err == nil, consumed, failure); outcomeErr == nil {
			s.Accounts.recordImageEvidence(text(account["management_id"]), pool.ImageRoute, evidenceStarted, err == nil, failure)
		}
		recordCancel()
		lease.Release()
		entry := Map{"attempt": attempt, "account_email": text(account["email"]), "duration_ms": time.Since(started).Milliseconds(), "status": "success"}
		if failure != nil {
			entry["status"] = "failed"
			for k, v := range failure.Details {
				if k != "conversation_id" {
					entry[k] = v
				}
			}
		}
		attempts = append(attempts, entry)
		monitorEntry := merge(entry, Map{"max_account_attempts": maxAttempts, "account_switch_count": attempt - 1, "conversation_id": upstreamResult.ConversationID})
		if failure != nil {
			monitorEntry = merge(monitorEntry, failure.Details)
		}
		stage := "image_download_done"
		if failure != nil {
			stage = "image_attempt_failed"
		}
		recordPublicStage(ctx, stage, monitorImageData(ctx, monitorEntry))
		if err == nil {
			result["_account_email"] = text(account["email"])
			result["_upstream_account_id"] = text(account["management_id"])
			result["_image_attempts"] = attempts
			return result, nil
		}
		failure.Details["image_attempts"] = attempts
		last = failure
		if failure.Status == 400 || failure.Code == "task_interrupted" || (consumed && failure.Code == "image_download_failed") || (upstreamResult.ConversationID != "" && failure.Code == "image_poll_timeout") {
			return nil, failure
		}
	}
	return nil, last
}
func (e *NativeImageExecutor) format(ctx context.Context, result upstream.ImageResult, payload, settings Map) (Map, error) {
	data := []Map{}
	urls := []string{}
	format := orValue(publicString(payload["response_format"]), "b64_json").(string)
	for _, asset := range result.Assets {
		content := asset.Data
		var err error
		if len(content) == 0 {
			content, err = e.backend.Download(ctx, asset.URL)
			if err != nil {
				return nil, imageProblem("image_download_failed", 0, "", nil)
			}
		}
		content, err = resizeGeneratedImage(ctx, content, text(payload["size"]), boolean(settings["image_upscale_enabled"], false), text(settings["image_upscale_engine"]))
		if err != nil {
			return nil, err
		}
		var stored StoredImage
		for attempt := 0; attempt < 2; attempt++ {
			stored, err = e.server.Assets.Save(ctx, content, text(payload["base_url"]))
			if err == nil || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			return nil, imageProblem("image_download_failed", 0, "", nil)
		}
		item := Map{"revised_prompt": text(payload["prompt"]), "width": stored.Width, "height": stored.Height}
		if format != "b64_json" {
			item["url"] = stored.URL
		} else {
			item["b64_json"] = base64.StdEncoding.EncodeToString(content)
		}
		data = append(data, item)
		urls = append(urls, stored.URL)
	}
	out := Map{"created": e.server.now().Unix(), "data": data, "_image_urls": urls}
	if result.ConversationID != "" {
		out["_conversation_id"] = result.ConversationID
	}
	return out, nil
}
func (e *NativeImageExecutor) Resume(parent context.Context, conversation string, payload Map, extra time.Duration, partial func([]Map)) (Map, error) {
	if !e.admit() {
		return nil, context.Canceled
	}
	defer e.active.Done()
	if strings.TrimSpace(conversation) == "" {
		return nil, problem(400, "task has no conversation_id")
	}
	ctx, cancel := context.WithTimeout(parent, extra)
	defer cancel()
	settings, err := e.server.Settings.runtimeValues(ctx)
	if err != nil {
		return nil, err
	}
	settings["image_poll_timeout_secs"] = extra.Seconds()
	settings["image_poll_initial_wait_secs"] = 0
	binding, _ := ctx.Value(imageResumeOriginKey{}).(imageResumeOrigin)
	accountID := binding.accountID
	if accountID == "" {
		accountID, err = e.legacyImageOrigin(ctx, binding.ownerID, conversation)
		if err != nil {
			return nil, err
		}
	}
	origin := imageConversationOrigin{accountID: accountID, conversationID: conversation}
	pollFailure := func(err error, knownAssets bool) *ImageExecutionError {
		failure := classifyExecutionError(err)
		if !knownAssets && parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			failure = imageProblem("image_poll_timeout", 0, "", nil)
		}
		failure.origin = origin
		failure.Details["conversation_id"] = conversation
		if failure.Code == "image_poll_timeout" {
			failure.Details["can_resume_poll"] = true
		}
		return failure
	}
	lease, err := e.server.Accounts.AcquireImagePoll(ctx, accountID)
	if err != nil {
		return nil, pollFailure(err, false)
	}
	defer lease.Release()
	account, profile := lease.Account, lease.Egress.Profile
	origin.email = text(account["email"])
	recordPublicStage(ctx, "image_egress_ready", Map{"account_email": origin.email, "conversation_id": conversation, "proxy_source": profile.Source, "egress_label": profile.EgressLabel, "has_proxy": profile.ProxyURL != ""})
	result, err := e.backend.Poll(ctx, conversation, account, profile, settings, nil)
	if err != nil {
		return nil, pollFailure(err, len(result.Assets) > 0)
	}
	payload = copyMap(payload)
	payload["response_format"] = "b64_json"
	out, err := e.format(ctx, result, payload, settings)
	if err != nil {
		failure := classifyExecutionError(err)
		failure.origin = origin
		failure.Details["conversation_id"] = conversation
		return nil, failure
	}
	if len(mapSlice(out["data"])) == 0 {
		failure := imageProblem("image_poll_timeout", 0, "", Map{"conversation_id": conversation, "can_resume_poll": true})
		failure.origin = origin
		return nil, failure
	}
	if partial != nil {
		partial(mapSlice(out["data"]))
	}
	out["_upstream_account_id"], out["_account_email"], out["_conversation_id"] = accountID, origin.email, conversation
	observeImageIdentity(ctx, conversation, origin.email, accountID)
	return out, nil
}
func (a *Accounts) recordImageOutcome(ctx context.Context, snapshot Map, success, consumed bool, failure *ImageExecutionError) error {
	mid := text(snapshot["management_id"])
	var updated Map
	_, err := a.db.Modify(ctx, "accounts", nil, func(raw []Map) ([]Map, error) {
		items := normalizeAccounts(raw, a.now(), false)
		current := byID(items, mid)
		if current == nil || canonical(credentialGuard(current, false)) != canonical(credentialGuard(snapshot, false)) {
			return nil, ErrCredentialsChanged
		}
		current["last_used_at"] = iso(a.now())
		if success {
			current["success"] = integer(current["success"], 0) + 1
		}
		if consumed && !boolean(current["image_quota_unknown"], true) {
			quota := max(0, integer(current["quota"], 0))
			current["quota"] = max(0, quota-1)
			if quota <= 1 {
				current["image_quota_unknown"] = true
				current["last_quota_estimated_empty_at"] = iso(a.now())
			}
		}
		if consumed && current["status"] == "限流" {
			current["status"] = "正常"
			current["image_quota_unknown"] = true
			current["restore_at"] = nil
		}
		if failure != nil && boolean(failure.Details["failure_account_failure"], false) {
			current["fail"] = integer(current["fail"], 0) + 1
		}
		updated = copyMap(current)
		return items, nil
	})
	if err == nil && updated != nil && failure != nil && boolean(failure.Details["failure_account_failure"], false) {
		_ = a.maintenance.scheduleVerification(ctx, updated, "image_failure")
	}
	a.notify()
	return err
}

var imageSizePattern = regexp.MustCompile(`(?i)(\d{2,5})\D+(\d{2,5})`)

func imageRequestedSize(size string) (int, int) {
	m := imageSizePattern.FindStringSubmatch(size)
	if len(m) != 3 {
		return 1024, 1024
	}
	w, h := integer(m[1], 1024), integer(m[2], 1024)
	if w <= 0 || h <= 0 {
		return 1024, 1024
	}
	return w, h
}
func countOutputImageTokens(items []Map, size, quality string) int {
	total := 0
	for _, item := range items {
		w, h := imageRequestedSize(size)
		if encoded := text(item["b64_json"]); encoded != "" {
			if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				if iw, ih, err := imageDimensions(data); err == nil {
					w, h = iw, ih
				}
			}
		}
		patches := float64((w + 31) / 32 * ((h + 31) / 32))
		factor := 33.0 / 32
		switch strings.ToLower(quality) {
		case "low":
			factor = 17.0 / 64
		case "high", "hd":
			factor = 65.0 / 16
		}
		total += int(math.Ceil(patches * factor))
	}
	return total
}

func (e *NativeImageExecutor) admit() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return false
	}
	e.active.Add(1)
	return true
}
func (e *NativeImageExecutor) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { e.active.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func observeImageIdentity(ctx context.Context, conversation, email, accountID string) {
	if call, ok := ctx.Value(publicCallContextKey{}).(*CallRecorder); ok {
		call.Observe(Map{"_conversation_id": conversation, "_account_email": email, "_upstream_account_id": accountID})
	}
}

// legacyImageOrigin consults existing owner-scoped audit evidence, never probes
// candidate accounts. Ambiguous, missing or deleted identity fails closed.
func (e *NativeImageExecutor) legacyImageOrigin(ctx context.Context, owner, conversation string) (string, error) {
	unavailable := func() (string, error) {
		return "", imageProblem("no_available_account", 0, "The original image account could not be identified. Polling cannot switch accounts.", nil)
	}
	if owner == "" {
		return unavailable()
	}
	records, err := repository.NewCallRecords(e.server.DB).List(ctx, repository.CallQuery{ConversationID: conversation}, 101, 0)
	if err != nil {
		return "", err
	}
	if records.Total > 100 {
		return unavailable()
	}
	accounts, err := e.server.Accounts.List(ctx)
	if err != nil {
		return "", err
	}
	origins := map[string]bool{}
	for _, record := range records.Items {
		detail := object(record["detail"])
		if text(detail["key_id"]) != owner || text(detail["conversation_id"]) != conversation {
			continue
		}
		if accountID := text(detail["upstream_account_id"]); accountID != "" {
			if byID(accounts, accountID) == nil {
				return unavailable()
			}
			origins[accountID] = true
			continue
		}
		emails := []string{}
		if email := text(detail["account_email"]); email != "" {
			emails = append(emails, email)
		} else {
			attempts := mapSlice(detail["image_attempts"])
			for _, attempt := range attempts {
				if len(attempts) == 1 || text(attempt["conversation_id"]) == conversation {
					if email := text(attempt["account_email"]); email != "" {
						emails = append(emails, email)
					}
				}
			}
		}
		for _, email := range emails {
			matched := 0
			for _, account := range accounts {
				if strings.EqualFold(text(account["email"]), email) {
					origins[text(account["management_id"])] = true
					matched++
				}
			}
			if matched != 1 {
				return unavailable()
			}
		}
	}
	if len(origins) != 1 {
		return unavailable()
	}
	for id := range origins {
		return id, nil
	}
	return unavailable()
}
