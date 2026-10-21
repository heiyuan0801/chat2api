package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
)

// ImageTaskOptions binds the task lifecycle to native execution without making
// the task owner a second account scheduler or asset storage owner.
type ImageTaskOptions struct {
	Path               string
	Workers, QueueSize int
	Now                func() time.Time
	RetentionHours     func() int
	Execute            func(context.Context, Map, func(string), func([]Map)) (Map, error)
	Resume             func(context.Context, string, Map, time.Duration, func([]Map)) (Map, error)
	BeginCall          func(context.Context, string, Map, Map) *CallRecorder
	AfterSuccess       func(result, payload Map, resumed bool)
}

type ImageTaskQueueFullError struct{}

func (*ImageTaskQueueFullError) Error() string {
	return "Image task queue is full. Please try again later."
}

type ImageTasks struct {
	options       ImageTaskOptions
	mu            sync.Mutex
	tasks         map[string]Map
	order         []string
	pending       map[string]chan struct{}
	reservations  map[*ImageTaskReservation]bool
	jobs          chan imageTaskJob
	accepted      int
	closed        bool
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeDone     chan struct{}
	closeErr      error
	backgroundErr error
	ownerRelease  func()
	spoolRelease  func()
	spoolRoot     string
}

// ImageTaskReservation owns one admission slot until commit or Rollback. Both
// operations are idempotent, including when server shutdown revokes the slot.
type ImageTaskReservation struct {
	tasks *ImageTasks
	state string
}

func (r *ImageTaskReservation) Rollback() bool {
	if r == nil || r.tasks == nil {
		return false
	}
	t := r.tasks
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.state != "reserved" {
		return false
	}
	r.state = "rolled_back"
	delete(t.reservations, r)
	t.accepted--
	return true
}

type imageTaskJob struct {
	key             string
	payload         Map
	mode            string
	spool           string
	images, masks   []imageTaskSpoolInput
	conversationID  string
	originAccountID string
	extra           time.Duration
	submitted       time.Time
	identity        Map
}
type imageTaskSpoolInput struct{ path, name, mime string }

var imageTaskDetailKeys = []string{
	"error_code", "error_message_version", "failure_scope", "failure_capability",
	"failure_retryable", "failure_account_failure", "failure_retry_after", "status_code",
	"error_type", "can_resume_poll", "poll_attempts", "poll_timeout_secs", "stream_timeout_secs",
}

const imageTaskPublicError = "The image generation tool encountered an error. Please try again."

var imageTaskZone = time.FixedZone("Asia/Shanghai", 8*3600)

func imageTaskTime(v time.Time) string { return v.In(imageTaskZone).Format("2006-01-02 15:04:05") }
func imageTaskOwner(identity Map) string {
	if owner := text(identity["id"]); owner != "" {
		return owner
	}
	return "anonymous"
}
func imageTaskKey(owner, taskID string) string { return owner + ":" + taskID }
func imageTaskCount(v any) int                 { return min(4, max(1, integer(v, 1))) }
func imageTaskText(v any, fallback string) string {
	if s := text(v); s != "" {
		return s
	}
	return fallback
}

// NewImageTasks requires an explicit isolated file path. An exclusive owner
// lease prevents a second Go process from interrupting another live worker's
// tasks. Startup recovers state; it never replays upstream generation.
func NewImageTasks(ctx context.Context, options ImageTaskOptions) (*ImageTasks, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, errors.New("explicit image task path required")
	}
	if options.Execute == nil {
		return nil, errors.New("native image execution handler required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RetentionHours == nil {
		options.RetentionHours = func() int { return 24 }
	}
	if options.Workers <= 0 {
		options.Workers = imageTaskEnv("CHATGPT2API_IMAGE_TASK_WORKERS", 16)
	}
	if options.QueueSize <= 0 {
		options.QueueSize = imageTaskEnv("CHATGPT2API_IMAGE_TASK_QUEUE_SIZE", 256)
	}
	if options.Workers > 1024 || options.QueueSize > 100000 {
		return nil, errors.New("image task capacity exceeds supported limit")
	}
	path, err := filepath.Abs(options.Path)
	if err != nil {
		return nil, err
	}
	options.Path = path
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	owner, err := filepersist.TryLock(path + ".owner.lock")
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("image task file already has a live Go worker owner")
	}
	taskCtx, cancel := context.WithCancel(ctx)
	t := &ImageTasks{options: options, tasks: map[string]Map{}, pending: map[string]chan struct{}{}, reservations: map[*ImageTaskReservation]bool{}, jobs: make(chan imageTaskJob, options.Workers+options.QueueSize), ctx: taskCtx, cancel: cancel, ownerRelease: owner, closeDone: make(chan struct{})}
	if err = t.initializeSpool(); err != nil {
		cancel()
		owner()
		return nil, err
	}
	if err = t.load(); err != nil {
		cancel()
		t.releaseSpool()
		owner()
		return nil, err
	}
	for i := 0; i < options.Workers; i++ {
		t.wg.Add(1)
		go t.worker()
	}
	return t, nil
}
func imageTaskEnv(name string, fallback int) int {
	v := integer(strings.TrimSpace(os.Getenv(name)), fallback)
	return max(1, v)
}

func (t *ImageTasks) Reserve() (*ImageTaskReservation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.ctx.Err() != nil || t.accepted >= cap(t.jobs) {
		return nil, &ImageTaskQueueFullError{}
	}
	r := &ImageTaskReservation{tasks: t, state: "reserved"}
	t.reservations[r] = true
	t.accepted++
	return r, nil
}
func (t *ImageTasks) SubmitGeneration(identity, payload Map) (Map, error) {
	p := copyMap(payload)
	p["response_format"] = "url"
	return t.submit(identity, p, "generate", nil, nil, nil)
}
func (t *ImageTasks) SubmitEdit(identity, payload Map, images, masks []upstream.ImageInput, reservation *ImageTaskReservation) (Map, error) {
	p := copyMap(payload)
	p["response_format"] = "url"
	p["_edit"] = true
	return t.submit(identity, p, "edit", images, masks, reservation)
}

func (t *ImageTasks) submit(identity, payload Map, mode string, images, masks []upstream.ImageInput, reservation *ImageTaskReservation) (Map, error) {
	if reservation != nil {
		defer reservation.Rollback()
	}
	taskID := text(payload["client_task_id"])
	if taskID == "" {
		return nil, problem(400, "client_task_id is required")
	}
	owner := imageTaskOwner(identity)
	key := imageTaskKey(owner, taskID)
	for {
		t.mu.Lock()
		if t.cleanupLocked() {
			if err := t.saveLocked(); err != nil {
				t.mu.Unlock()
				return nil, err
			}
		}
		if existing := t.tasks[key]; existing != nil {
			row := ImageTaskRow(existing, t.options.Now())
			t.mu.Unlock()
			return row, nil
		}
		if pending := t.pending[key]; pending != nil {
			t.mu.Unlock()
			if reservation != nil {
				reservation.Rollback()
				reservation = nil
			}
			select {
			case <-pending:
				continue
			case <-t.ctx.Done():
				return nil, &ImageTaskQueueFullError{}
			}
		}
		if t.closed || t.ctx.Err() != nil {
			t.mu.Unlock()
			return nil, &ImageTaskQueueFullError{}
		}
		t.pending[key] = make(chan struct{})
		t.mu.Unlock()
		break
	}
	defer func() {
		t.mu.Lock()
		if pending := t.pending[key]; pending != nil {
			delete(t.pending, key)
			close(pending)
		}
		t.mu.Unlock()
	}()
	var err error
	if reservation == nil {
		reservation, err = t.Reserve()
		if err != nil {
			return nil, err
		}
		defer reservation.Rollback()
	}
	job := imageTaskJob{key: key, payload: copyMap(payload), mode: mode, submitted: t.options.Now()}
	job.identity = Map{"id": identity["id"], "name": identity["name"], "role": identity["role"]}
	job.payload["model"] = imageTaskText(payload["model"], "gpt-image-2")
	job.payload["quality"] = imageTaskText(payload["quality"], "auto")
	job.payload["n"] = imageTaskCount(payload["n"])
	delete(job.payload, "client_task_id")
	if mode == "edit" {
		if err = t.spoolInputs(&job, images, masks); err != nil {
			return nil, err
		}
	}
	committed := false
	defer func() {
		if !committed && job.spool != "" {
			_ = os.RemoveAll(job.spool)
		}
	}()
	now := t.options.Now()
	task := Map{"id": taskID, "owner_id": owner, "status": "queued", "mode": mode, "model": job.payload["model"], "n": imageTaskCount(payload["n"]), "size": text(payload["size"]), "quality": job.payload["quality"], "base_url": text(payload["base_url"]), "created_at": imageTaskTime(now), "updated_at": imageTaskTime(now), "created_ts": float64(now.UnixNano()) / 1e9}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || reservation.tasks != t || reservation.state != "reserved" {
		return nil, &ImageTaskQueueFullError{}
	}
	t.tasks[key] = task
	t.order = append(t.order, key)
	if err = t.saveLocked(); err != nil {
		delete(t.tasks, key)
		t.removeOrderLocked(key)
		return nil, err
	}
	reservation.state = "committed"
	delete(t.reservations, reservation)
	t.jobs <- job
	committed = true
	return ImageTaskRow(task, now), nil
}

func (t *ImageTasks) List(identity Map, ids []string) (Map, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cleanupLocked() {
		if err := t.saveLocked(); err != nil {
			return nil, err
		}
	}
	owner := imageTaskOwner(identity)
	raw := []Map{}
	missing := []string{}
	requested := []string{}
	for _, v := range ids {
		if id := strings.TrimSpace(v); id != "" {
			requested = append(requested, id)
		}
	}
	if len(requested) > 0 {
		for _, id := range requested {
			if task := t.tasks[imageTaskKey(owner, id)]; task != nil {
				raw = append(raw, task)
			} else {
				missing = append(missing, id)
			}
		}
	} else {
		for _, key := range t.order {
			if task := t.tasks[key]; task != nil && task["owner_id"] == owner {
				raw = append(raw, task)
			}
		}
		sort.SliceStable(raw, func(i, j int) bool { return text(raw[i]["updated_at"]) > text(raw[j]["updated_at"]) })
	}
	items := []Map{}
	now := t.options.Now()
	for _, task := range raw {
		items = append(items, ImageTaskRow(task, now))
	}
	return Map{"items": items, "missing_ids": unique(missing)}, nil
}

func (t *ImageTasks) Resume(identity Map, taskID string, extra time.Duration) (Map, error) {
	key := imageTaskKey(imageTaskOwner(identity), strings.TrimSpace(taskID))
	t.mu.Lock()
	defer t.mu.Unlock()
	task := t.tasks[key]
	if task == nil {
		return nil, problem(400, "task not found")
	}
	if task["status"] != "error" {
		return nil, problem(400, "task is not in error state")
	}
	if text(task["error_code"]) != "image_poll_timeout" {
		return nil, problem(400, "task error is not a timeout error")
	}
	conversationID := text(task["conversation_id"])
	if conversationID == "" {
		return nil, problem(400, "task has no conversation_id")
	}
	if t.closed || t.ctx.Err() != nil || t.accepted >= cap(t.jobs) {
		return nil, &ImageTaskQueueFullError{}
	}
	if t.options.Resume == nil {
		return nil, problem(503, "image polling is unavailable")
	}
	previous := copyMap(task)
	queued := copyMap(task)
	now := t.options.Now()
	queued["status"] = "queued"
	queued["error"] = ""
	queued["updated_at"] = imageTaskTime(now)
	queued["updated_ts"] = float64(now.UnixNano()) / 1e9
	for _, field := range imageTaskDetailKeys {
		delete(queued, field)
	}
	t.tasks[key] = queued
	if err := t.saveLocked(); err != nil {
		t.tasks[key] = previous
		return nil, err
	}
	t.accepted++
	t.jobs <- imageTaskJob{key: key, payload: Map{"model": queued["model"], "n": queued["n"], "size": queued["size"], "quality": queued["quality"], "base_url": queued["base_url"], "response_format": "b64_json"}, mode: text(queued["mode"]), conversationID: conversationID, originAccountID: text(queued["upstream_account_id"]), extra: extra, submitted: now, identity: Map{"id": identity["id"], "name": identity["name"], "role": identity["role"]}}
	return ImageTaskRow(queued, now), nil
}

func (t *ImageTasks) saveLocked() error {
	items := []Map{}
	for _, key := range t.order {
		if task := t.tasks[key]; task != nil {
			items = append(items, task)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return text(items[i]["updated_at"]) > text(items[j]["updated_at"]) })
	return filepersist.WriteJSON(t.options.Path, Map{"tasks": items})
}
func (t *ImageTasks) removeOrderLocked(key string) {
	for i, v := range t.order {
		if v == key {
			t.order = append(t.order[:i], t.order[i+1:]...)
			return
		}
	}
}
func (t *ImageTasks) cleanupLocked() bool {
	hours := max(1, t.options.RetentionHours())
	cutoff := t.options.Now().Add(-time.Duration(hours) * time.Hour)
	changed := false
	for _, key := range append([]string{}, t.order...) {
		task := t.tasks[key]
		if task == nil {
			continue
		}
		if task["status"] != "success" && task["status"] != "error" {
			continue
		}
		stamp := imageTaskParseTime(text(task["updated_at"]))
		if stamp.Before(cutoff) {
			delete(t.tasks, key)
			t.removeOrderLocked(key)
			changed = true
		}
	}
	return changed
}
func imageTaskParseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02 15:04:05"} {
		if stamp, err := time.ParseInLocation(layout, s, imageTaskZone); err == nil {
			return stamp
		}
	}
	return time.Time{}
}

func (t *ImageTasks) load() error {
	var raw any
	if err := filepersist.ReadJSON(t.options.Path, &raw); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, e := os.Stat(t.options.Path); errors.Is(e, os.ErrNotExist) {
				if _, e = os.Stat(t.options.Path + ".bak"); errors.Is(e, os.ErrNotExist) {
					return nil
				}
			}
		}
		return err
	}
	items := list(raw)
	if m, ok := raw.(map[string]any); ok {
		items = list(m["tasks"])
	}
	changed := false
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		taskID, owner := text(item["id"]), text(item["owner_id"])
		if taskID == "" || owner == "" {
			continue
		}
		status := text(item["status"])
		if status != "queued" && status != "running" && status != "success" && status != "error" {
			status = "error"
		}
		task := Map{"id": taskID, "owner_id": owner, "status": status, "mode": "generate", "model": "gpt-image-2", "n": imageTaskCount(item["n"]), "size": text(item["size"]), "quality": "auto", "base_url": text(item["base_url"]), "created_at": imageTaskTime(t.options.Now()), "created_ts": item["created_ts"], "updated_ts": item["updated_ts"], "started_ts": item["started_ts"], "duration_ms": item["duration_ms"]}
		if text(item["mode"]) == "edit" {
			task["mode"] = "edit"
		}
		for _, field := range []string{"model", "quality", "created_at"} {
			if v := text(item[field]); v != "" {
				task[field] = v
			}
		}
		task["updated_at"] = task["created_at"]
		if v := text(item["updated_at"]); v != "" {
			task["updated_at"] = v
		}
		for _, field := range []string{"data", "usage"} {
			if v := item[field]; v != nil {
				task[field] = v
			}
		}
		for _, field := range imageTaskDetailKeys {
			if v := item[field]; v != nil && v != "" {
				task[field] = v
			}
		}
		if conversationID := text(item["conversation_id"]); conversationID != "" {
			task["conversation_id"] = conversationID
			if accountID := text(item["upstream_account_id"]); accountID != "" {
				task["upstream_account_id"] = accountID
			}
		}
		if status == "error" {
			failure := imageProblem(text(task["error_code"]), 0, "", nil)
			task["error"] = failure.Message
			if text(item["error_message_version"]) == "1" {
				task["error"] = text(item["error"])
			}
			task["error_code"] = failure.Code
			task["error_message_version"] = 1
		}
		key := imageTaskKey(owner, taskID)
		if t.tasks[key] == nil {
			t.order = append(t.order, key)
		}
		t.tasks[key] = task
		if status == "queued" || status == "running" {
			t.interruptLocked(key)
			changed = true
		}
		if !changed {
			for field := range item {
				if _, ok := task[field]; !ok {
					changed = true
					break
				}
			}
		}
	}
	if t.cleanupLocked() {
		changed = true
	}
	if changed {
		return t.saveLocked()
	}
	return nil
}

func (t *ImageTasks) interruptLocked(key string) {
	if task := t.tasks[key]; task != nil {
		task["status"] = "error"
		task["error"] = imageTaskPublicError
		task["error_code"] = "task_interrupted"
		task["error_message_version"] = 1
		task["status_code"] = 503
		task["error_type"] = "server_error"
		task["failure_scope"] = "request"
		task["failure_retryable"] = false
		task["failure_account_failure"] = false
		task["updated_at"] = imageTaskTime(t.options.Now())
		delete(task, "can_resume_poll")
	}
}

func (t *ImageTasks) worker() {
	defer t.wg.Done()
	for {
		select {
		case <-t.ctx.Done():
			return
		case job, ok := <-t.jobs:
			if !ok {
				return
			}
			t.runJob(job)
			t.mu.Lock()
			t.accepted--
			t.mu.Unlock()
		}
	}
}
func (t *ImageTasks) runJob(job imageTaskJob) {
	if job.spool != "" {
		defer os.RemoveAll(job.spool)
	}
	if err := t.update(job.key, true, Map{"status": "running", "error": ""}); err != nil {
		t.recordError(err)
		return
	}
	var call *CallRecorder
	defer func() {
		if recover() != nil {
			err := errors.New("image task execution failed")
			t.recordError(t.finish(job, nil, err))
			if call != nil {
				t.recordError(call.Finish(nil, err))
			}
		}
	}()
	payload := copyMap(job.payload)
	if job.spool != "" {
		images, err := imageTaskReadSpool(job.images)
		if err != nil {
			t.recordError(t.finish(job, nil, err))
			return
		}
		masks, err := imageTaskReadSpool(job.masks)
		if err != nil {
			t.recordError(t.finish(job, nil, err))
			return
		}
		payload["images"] = images
		payload["mask"] = masks
	}
	if t.options.BeginCall != nil {
		endpoint := "/v1/images/generations"
		if job.mode == "edit" {
			endpoint = "/v1/images/edits"
		}
		call = t.options.BeginCall(t.ctx, endpoint, payload, job.identity)
		if call == nil {
			t.recordError(t.finish(job, nil, imageProblem("task_interrupted", 0, "", nil)))
			return
		}
		call.started = job.submitted
		call.resumed = job.conversationID != ""
	}
	progress := func(step string) {
		updates := Map{"progress": step}
		if step == "image_stream_resolve_start" {
			updates["started_ts"] = float64(t.options.Now().UnixNano()) / 1e9
		}
		t.recordError(t.update(job.key, false, updates))
	}
	partial := func(data []Map) {
		if len(data) > 0 {
			t.recordError(t.update(job.key, true, Map{"data": copyImageTaskResults(data)}))
		}
	}
	var result Map
	var err error
	executionCtx := t.ctx
	if call != nil {
		executionCtx = context.WithValue(executionCtx, publicCallContextKey{}, call)
	}
	if job.conversationID != "" {
		executionCtx = context.WithValue(executionCtx, imageResumeOriginKey{}, imageResumeOrigin{job.originAccountID, imageTaskOwner(job.identity)})
		result, err = t.options.Resume(executionCtx, job.conversationID, payload, job.extra, partial)
	} else {
		result, err = t.options.Execute(executionCtx, payload, progress, partial)
	}
	finishErr := t.finish(job, result, err)
	t.recordError(finishErr)
	if finishErr == nil && err == nil && len(dicts(result["data"])) > 0 && t.options.AfterSuccess != nil {
		func() {
			defer func() { _ = recover() }()
			t.options.AfterSuccess(result, payload, job.conversationID != "")
		}()
	}
	if call != nil {
		t.recordError(call.Finish(result, err))
	}
}
func (t *ImageTasks) recordError(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.backgroundErr = errors.Join(t.backgroundErr, err)
}
func copyImageTaskResults(data []Map) []Map {
	out := make([]Map, len(data))
	for i, item := range data {
		out[i] = copyMap(item)
	}
	return out
}
func (t *ImageTasks) update(key string, persist bool, updates Map) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	task := t.tasks[key]
	if task == nil {
		return nil
	}
	previous := copyMap(task)
	for field, value := range updates {
		task[field] = value
	}
	task["updated_at"] = imageTaskTime(t.options.Now())
	task["updated_ts"] = float64(t.options.Now().UnixNano()) / 1e9
	if persist {
		if err := t.saveLocked(); err != nil {
			t.tasks[key] = previous
			return err
		}
	}
	return nil
}

func (t *ImageTasks) finish(job imageTaskJob, result Map, err error) error {
	updates := Map{"duration_ms": max(int64(0), t.options.Now().Sub(job.submitted).Milliseconds())}
	if conversation := text(result["_conversation_id"]); conversation != "" {
		updates["conversation_id"] = conversation
		if accountID := text(result["_upstream_account_id"]); accountID != "" {
			updates["upstream_account_id"] = accountID
		}
	}
	if err == nil && len(dicts(result["data"])) == 0 {
		err = imageProblem("no_image_generated", 0, "", nil)
	}
	if err == nil {
		updates["status"] = "success"
		updates["data"] = copyImageTaskResults(dicts(result["data"]))
		updates["usage"] = result["usage"]
		updates["error"] = ""
		for _, field := range imageTaskDetailKeys {
			updates[field] = ""
		}
	} else {
		updates["status"] = "error"
		updates["data"] = []Map{}
		updates["error"] = imageTaskPublicError
		updates["error_code"] = "upstream_error"
		updates["error_message_version"] = 1
		execution := classifyExecutionError(err)
		if execution != nil {
			updates["error_code"] = execution.Code
			updates["status_code"] = execution.Status
			if execution.Message != "" {
				updates["error"] = execution.Message
			}
			for _, field := range imageTaskDetailKeys {
				if v := execution.Details[field]; v != nil && v != "" {
					updates[field] = v
				}
			}
			if v := text(execution.Details["conversation_id"]); v != "" {
				updates["conversation_id"] = v
				if execution.origin.conversationID == v && execution.origin.accountID != "" {
					updates["upstream_account_id"] = execution.origin.accountID
				}
			}
			if execution.Code == "image_poll_timeout" {
				conversationID := text(updates["conversation_id"])
				if conversationID == "" {
					conversationID = job.conversationID
				}
				if conversationID != "" {
					updates["conversation_id"] = conversationID
					updates["can_resume_poll"] = true
				}
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			updates["error_code"] = "task_interrupted"
			updates["status_code"] = 503
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	task := t.tasks[job.key]
	if task == nil {
		return nil
	}
	previous := copyMap(task)
	for field, value := range updates {
		if slices.Contains(imageTaskDetailKeys, field) && value == "" {
			delete(task, field)
		} else {
			task[field] = value
		}
	}
	task["updated_at"] = imageTaskTime(t.options.Now())
	task["updated_ts"] = float64(t.options.Now().UnixNano()) / 1e9
	if err := t.saveLocked(); err != nil {
		t.tasks[job.key] = previous
		return err
	}
	return nil
}

// Close closes admission, cancels queued work, and waits for running callbacks.
// The owner lease is retained until every callback has stopped, even if ctx
// expires, so a replacement process cannot recover a task still being written.
func (t *ImageTasks) Close(ctx context.Context) error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		for r := range t.reservations {
			r.state = "cancelled"
			t.accepted--
			delete(t.reservations, r)
		}
		drain := true
		for drain {
			select {
			case job := <-t.jobs:
				t.interruptLocked(job.key)
				t.accepted--
				if job.spool != "" {
					_ = os.RemoveAll(job.spool)
				}
			default:
				drain = false
			}
		}
		t.closeErr = t.saveLocked()
		t.cancel()
		close(t.jobs)
		t.mu.Unlock()
		go func() { t.wg.Wait(); t.releaseSpool(); t.ownerRelease(); close(t.closeDone) }()
	})
	select {
	case <-t.closeDone:
		return errors.Join(t.closeErr, t.backgroundErr)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *ImageTasks) initializeSpool() error {
	parent := filepath.Join(filepath.Dir(t.options.Path), "image_task_spool")
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	release, err := filepersist.Lock(ctx, filepath.Join(parent, ".lifecycle.lock"))
	if err != nil {
		return err
	}
	defer release()
	prefix := strings.TrimSuffix(filepath.Base(t.options.Path), filepath.Ext(t.options.Path)) + "-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		candidate := filepath.Join(parent, entry.Name())
		if info, e := os.Lstat(filepath.Join(candidate, ".owner.lock")); e != nil || !info.Mode().IsRegular() {
			continue
		}
		probe, e := filepersist.TryLock(filepath.Join(candidate, ".owner.lock"))
		if e == nil && probe != nil {
			_ = os.RemoveAll(candidate)
			probe()
		}
	}
	root, err := os.MkdirTemp(parent, fmt.Sprintf("%s%d-", prefix, os.Getpid()))
	if err != nil {
		return err
	}
	lease, err := filepersist.TryLock(filepath.Join(root, ".owner.lock"))
	if err != nil || lease == nil {
		os.RemoveAll(root)
		if err == nil {
			err = errors.New("image task spool owner lock unavailable")
		}
		return err
	}
	t.spoolRoot = root
	t.spoolRelease = lease
	return nil
}
func (t *ImageTasks) releaseSpool() {
	if t.spoolRelease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := filepersist.Lock(ctx, filepath.Join(filepath.Dir(t.spoolRoot), ".lifecycle.lock"))
	if err == nil {
		defer release()
	}
	t.spoolRelease()
	t.spoolRelease = nil
	_ = os.RemoveAll(t.spoolRoot)
}
func (t *ImageTasks) spoolInputs(job *imageTaskJob, images, masks []upstream.ImageInput) error {
	root, err := os.MkdirTemp(t.spoolRoot, "task-")
	if err != nil {
		return err
	}
	job.spool = root
	write := func(name string, inputs []upstream.ImageInput) ([]imageTaskSpoolInput, error) {
		out := []imageTaskSpoolInput{}
		for i, input := range inputs {
			path := filepath.Join(root, fmt.Sprintf("%s-%d.bin", name, i))
			if err := os.WriteFile(path, input.Data, 0600); err != nil {
				return nil, err
			}
			out = append(out, imageTaskSpoolInput{path: path, name: input.Name, mime: input.MIME})
		}
		return out, nil
	}
	if job.images, err = write("images", images); err == nil {
		job.masks, err = write("mask", masks)
	}
	if err != nil {
		os.RemoveAll(root)
	}
	return err
}
func imageTaskReadSpool(inputs []imageTaskSpoolInput) ([]upstream.ImageInput, error) {
	out := []upstream.ImageInput{}
	for _, input := range inputs {
		data, err := os.ReadFile(input.path)
		if err != nil {
			return nil, err
		}
		out = append(out, upstream.ImageInput{Data: data, Name: input.name, MIME: input.mime})
	}
	return out, nil
}
