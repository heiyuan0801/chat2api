package controlplane

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	imagepkg "image"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
)

// EditableFileBackend writes only private staging files. The task owner alone
// controls durable state, account leases, publication, deletion and final logs.
type EditableFileBackend interface {
	Export(context.Context, upstream.EditableFileRequest, Map, ProxyProfile, string) (upstream.EditableFileExport, error)
}
type nativeEditableFileBackend struct{ server *Server }

func (n nativeEditableFileBackend) Export(ctx context.Context, request upstream.EditableFileRequest, account Map, profile ProxyProfile, directory string) (upstream.EditableFileExport, error) {
	client, e := n.server.NewUpstream(upstream.Config{AccessToken: text(account["access_token"]), ProxyURL: profile.ProxyURL, Timeout: 20 * time.Minute, StreamTimeout: 5 * time.Minute, PollInterval: 5 * time.Second, MaxBodyBytes: 16 << 20})
	if e != nil {
		return upstream.EditableFileExport{}, e
	}
	defer client.Close()
	return client.ExportEditable(ctx, request, directory)
}

type editableJob struct {
	owner, id, kind, prompt, baseURL string
	imageCount                       int
	validationErr                    error
	identity                         Map
}
type EditableTasks struct {
	server        *Server
	repository    *repository.EditableFiles
	backend       EditableFileBackend
	root          *os.Root
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	closing       bool
	jobs          chan editableJob
	slots         chan struct{}
	workers       sync.WaitGroup
	release       func()
	closeOnce     sync.Once
	closeErr      error
	errMu         sync.Mutex
	backgroundErr error
}

var editableTaskID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,160}$`)
var editableStorageID = regexp.MustCompile(`^asset-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func editableUUID() string {
	var raw [16]byte
	if _, e := rand.Read(raw[:]); e != nil {
		panic("system random source unavailable")
	}
	raw[6] = (raw[6] & 15) | 64
	raw[8] = (raw[8] & 63) | 128
	s := hex.EncodeToString(raw[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func NewEditableTasks(ctx context.Context, s *Server, backend EditableFileBackend) (*EditableTasks, error) {
	directory := filepath.Join(s.DataDir, "files")
	if e := os.MkdirAll(directory, 0700); e != nil {
		return nil, e
	}
	release, e := filepersist.TryLock(filepath.Join(directory, ".owner.lock"))
	if e != nil {
		return nil, e
	}
	if release == nil {
		return nil, errors.New("editable file tasks already have an active worker owner")
	}
	root, e := os.OpenRoot(directory)
	if e != nil {
		release()
		return nil, e
	}
	workerCtx, cancel := context.WithCancel(ctx)
	if backend == nil {
		backend = nativeEditableFileBackend{s}
	}
	t := &EditableTasks{server: s, repository: repository.NewEditableFiles(s.DB), backend: backend, root: root, ctx: workerCtx, cancel: cancel, jobs: make(chan editableJob, 68), slots: make(chan struct{}, 68), release: release}
	if e = t.recover(ctx); e != nil {
		cancel()
		root.Close()
		release()
		return nil, e
	}
	for i := 0; i < 4; i++ {
		t.workers.Add(1)
		go t.worker()
	}
	return t, nil
}
func (t *EditableTasks) directories(task Map) (string, string, error) {
	kind, storage := text(task["kind"]), text(task["storage_id"])
	if (kind != "ppt" && kind != "psd") || !editableStorageID.MatchString(storage) {
		return "", "", errors.New("editable file task storage id is invalid")
	}
	return path.Join(".staging", kind, storage), path.Join(kind, storage), nil
}
func (t *EditableTasks) cleanup(task Map) error {
	staging, published, e := t.directories(task)
	if e != nil {
		return nil
	}
	for _, directory := range []string{staging, published} {
		if e = t.root.RemoveAll(directory); e != nil {
			return errors.New("editable file task files could not be removed")
		}
	}
	return nil
}
func (t *EditableTasks) recover(ctx context.Context) error {
	items, e := t.repository.PendingDeletion(ctx)
	if e != nil {
		return e
	}
	for _, task := range items {
		if t.cleanup(task) == nil {
			if _, e = t.repository.Delete(ctx, text(task["owner_id"]), text(task["id"])); e != nil {
				return e
			}
		}
	}
	items, e = t.repository.Unfinished(ctx)
	if e != nil {
		return e
	}
	for _, task := range items {
		_ = t.cleanup(task)
		if e = t.update(ctx, text(task["owner_id"]), text(task["id"]), Map{"status": "error", "error": "服务已重启，未完成的任务已中断", "ended_ts": editableTS(t.server.now())}); e != nil {
			return e
		}
	}
	return nil
}
func editableTS(now time.Time) float64 { return float64(now.UnixNano()) / 1e9 }
func editableProjection(task Map, now time.Time) Map {
	status := text(task["status"])
	labels := map[string][3]string{"queued": {"等待中", "muted", "lucide:loader-circle"}, "running": {"生成中", "warning", "lucide:loader-circle"}, "success": {"已完成", "success", "lucide:file-check-2"}, "error": {"失败", "danger", "lucide:circle-alert"}}
	presentation, ok := labels[status]
	if !ok {
		status = "error"
		presentation = labels[status]
	}
	start, _ := number(task["started_ts"])
	if start == 0 {
		start, _ = number(task["created_ts"])
	}
	end, _ := number(task["ended_ts"])
	if end == 0 {
		end = editableTS(now)
	}
	elapsed := 0
	if start > 0 {
		elapsed = max(0, int(end-start))
	}
	result := object(task["result"])
	out := Map{"id": task["id"], "status": status, "status_label": presentation[0], "status_tone": presentation[1], "status_icon": presentation[2], "is_active": status == "queued" || status == "running", "kind": task["kind"], "created_at": task["created_at"], "updated_at": task["updated_at"], "elapsed_seconds": elapsed, "can_download": !truth(task["delete_pending"]) && status == "success" && (text(result["primary_url"]) != "" || text(result["zip_url"]) != ""), "can_delete": status == "success" || status == "error"}
	if len(result) > 0 {
		out["result"] = copyMap(result)
	}
	if text(task["error"]) != "" {
		out["error"] = task["error"]
	}
	return out
}
func editableOwner(who Map) string {
	owner := text(who["id"])
	if owner == "" {
		return "anonymous"
	}
	return owner
}
func (t *EditableTasks) List(ctx context.Context, who Map, requested []string, limit int) (Map, error) {
	ids := []string{}
	for _, id := range requested {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	var filter []string
	if len(ids) > 0 {
		filter = ids
	}
	items, e := t.repository.List(ctx, editableOwner(who), filter, limit)
	if e != nil {
		return nil, e
	}
	result, missing := []Map{}, []string{}
	if len(ids) > 0 {
		by := map[string]Map{}
		for _, task := range items {
			by[text(task["id"])] = task
		}
		for _, id := range ids {
			if task := by[id]; task != nil {
				result = append(result, editableProjection(task, t.server.now()))
			} else {
				missing = append(missing, id)
			}
		}
	} else {
		for _, task := range items {
			result = append(result, editableProjection(task, t.server.now()))
		}
	}
	return Map{"items": result, "missing_ids": missing}, nil
}
func (t *EditableTasks) Submit(ctx context.Context, who Map, kind, prompt string, encoded []string, taskID, baseURL string) (Map, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		taskID = editableUUID()
	}
	if !editableTaskID.MatchString(taskID) || taskID == "." || taskID == ".." {
		return nil, problem(400, "client_task_id must be 1-160 characters using letters, numbers, '.', '_' or '-'")
	}
	owner := editableOwner(who)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return nil, problem(503, "server is shutting down")
	}
	existing, e := t.repository.Get(ctx, owner, taskID)
	if e != nil {
		return nil, e
	}
	if existing != nil {
		return editableProjection(existing, t.server.now()), nil
	}
	select {
	case t.slots <- struct{}{}:
	default:
		return nil, problem(503, "editable file task queue is full")
	}
	now := t.server.now()
	task, created, e := t.repository.Create(ctx, Map{"owner_id": owner, "id": taskID, "storage_id": "asset-" + editableUUID(), "kind": kind, "status": "queued", "model": upstream.EditableFileModel, "created_at": imageTaskTime(now), "updated_at": imageTaskTime(now), "created_ts": editableTS(now), "updated_ts": editableTS(now)})
	if e != nil || !created {
		<-t.slots
		if e != nil {
			return nil, e
		}
		return editableProjection(task, now), nil
	}
	var inputErr error
	if len(encoded) > 16 {
		inputErr = problem(400, "too many images")
	}
	encodedTotal := 0
	for _, value := range encoded {
		encodedTotal += len(value)
		if len(value) > ((50<<20)+2)/3*4+4096 {
			inputErr = problem(400, "image URL exceeds 50MB limit")
		}
	}
	if encodedTotal > ((100<<20)+2)/3*4+4096*16 {
		inputErr = problem(400, "total image input is too large")
	}
	spoolValues := encoded
	if inputErr != nil {
		spoolValues = nil
	}
	staging, _, spoolErr := t.directories(task)
	if spoolErr == nil && len(spoolValues) > 0 {
		spoolErr = t.root.MkdirAll(path.Join(staging, ".inputs"), 0700)
	}
	for i, value := range spoolValues {
		if spoolErr != nil {
			break
		}
		if spoolErr = ctx.Err(); spoolErr != nil {
			break
		}
		var file *os.File
		file, spoolErr = t.root.OpenFile(path.Join(staging, ".inputs", strconv.Itoa(i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if spoolErr != nil {
			break
		}
		_, spoolErr = io.Copy(file, strings.NewReader(value))
		closeErr := file.Close()
		if spoolErr == nil {
			spoolErr = closeErr
		}
	}
	if spoolErr != nil {
		<-t.slots
		_ = t.cleanup(task)
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = t.update(finishCtx, owner, taskID, Map{"status": "error", "error": "editable file input could not be saved", "ended_ts": editableTS(t.server.now())})
		return nil, problem(500, "editable file input could not be saved")
	}
	t.jobs <- editableJob{owner: owner, id: taskID, kind: kind, prompt: prompt, imageCount: len(encoded), validationErr: inputErr, identity: copyMap(who), baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
	return editableProjection(task, now), nil
}
func (t *EditableTasks) update(ctx context.Context, owner, id string, values Map) error {
	now := t.server.now()
	values["updated_at"] = imageTaskTime(now)
	values["updated_ts"] = editableTS(now)
	_, e := t.repository.Update(ctx, owner, id, values)
	return e
}
func (t *EditableTasks) worker() {
	defer t.workers.Done()
	for {
		select {
		case <-t.ctx.Done():
			return
		case job := <-t.jobs:
			t.run(job)
			<-t.slots
		}
	}
}
func (t *EditableTasks) account(ctx context.Context) (*AccountLease, error) {
	attempted := map[string]bool{}
	for {
		items, e := t.server.Accounts.List(ctx)
		if e != nil {
			return nil, e
		}
		available := []Map{}
		for _, item := range items {
			mid := text(item["management_id"])
			if !attempted[mid] && selectable(item, t.server.now(), true, true) && matchesPool(item, PoolRequest{Plans: []string{"Plus", "Team", "Pro", "Enterprise"}}) {
				available = append(available, item)
			}
		}
		if len(available) == 0 {
			return nil, errors.New("no available plus/team/pro account")
		}
		sort.SliceStable(available, func(i, j int) bool { return text(available[i]["last_used_at"]) < text(available[j]["last_used_at"]) })
		candidate := available[0]
		mid := text(candidate["management_id"])
		attempted[mid] = true
		current, _, e := t.server.Maintenance.Refresh(ctx, mid, false)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if current == nil || !selectable(current, t.server.now(), true, true) || !matchesPool(current, PoolRequest{Plans: []string{"Plus", "Team", "Pro", "Enterprise"}}) {
			continue
		}
		egress, e := t.server.Proxy.Resolve(ctx, current, "", false, true, false)
		if e != nil {
			return nil, e
		}
		return &AccountLease{Account: current, Egress: egress}, nil
	}
}
func editablePublicError(e error) string {
	if e == nil {
		return ""
	}
	if errors.Is(e, context.Canceled) {
		return "服务已停止，未完成的任务已中断"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return "The editable file task timed out. Please try again."
	}
	var upstreamError *upstream.Error
	if errors.As(e, &upstreamError) {
		if upstreamError.Stage == "editable_download" || upstreamError.Stage == "editable_download_url" || strings.Contains(upstreamError.Kind, "asset") || upstreamError.Kind == "image_download_failed" {
			return "The editable file output could not be downloaded. Please try again."
		}
		classified := classifyExecutionError(e)
		switch classified.Code {
		case "auth_invalid":
			return "The editable file task could not authenticate with the upstream account. Please try another account."
		case "content_policy_violation", "invalid_image_input", "upstream_text_reply", "unsupported_model":
			return "The editable file request was rejected by the upstream service."
		case "file_upload_throttled", "image_quota_exhausted", "insufficient_quota", "upstream_rate_limited", "upstream_unavailable":
			return "The editable file service is temporarily unavailable. Please try again later."
		case "image_poll_timeout", "image_stream_timeout", "upstream_connection_timeout":
			return "The editable file task timed out. Please try again."
		}
		return "The editable file task failed. Please try again."
	}
	var p *Problem
	if errors.As(e, &p) {
		return p.Message
	}
	if e.Error() == "base64_images is empty" || e.Error() == "no available plus/team/pro account" || e.Error() == "editable file export produced invalid artifacts" || e.Error() == "editable file task files could not be removed" {
		return e.Error()
	}
	return "The editable file task failed. Please try again."
}
func (t *EditableTasks) run(job editableJob) {
	ctx, cancel := context.WithTimeout(t.ctx, 20*time.Minute)
	defer cancel()
	started := t.server.now()
	values := Map{"status": "running", "error": "", "started_ts": editableTS(started)}
	if e := t.update(context.WithoutCancel(ctx), job.owner, job.id, values); e != nil {
		t.recordError(e)
		return
	}
	call := t.server.beginPublicCall(ctx, "/v1/"+job.kind+"/generations", Map{"model": upstream.EditableFileModel, "prompt": job.prompt}, job.identity)
	if call != nil {
		call.summary = strings.ToUpper(job.kind) + "生成任务"
	}
	var result Map
	var executionErr error
	var task Map
	defer func() {
		if recover() != nil {
			executionErr = errors.New("editable file task failed")
		}
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer finishCancel()
		if executionErr != nil {
			if task != nil {
				if e := t.cleanup(task); e != nil {
					executionErr = e
				}
			}
			message := editablePublicError(executionErr)
			t.recordError(t.update(finishCtx, job.owner, job.id, Map{"status": "error", "error": message, "ended_ts": editableTS(t.server.now())}))
			if call != nil {
				t.recordError(call.Finish(nil, errors.New(message)))
			}
		} else if call != nil {
			t.recordError(call.Finish(result, nil))
		}
	}()
	if executionErr = ctx.Err(); executionErr != nil {
		return
	}
	task, executionErr = t.repository.Get(ctx, job.owner, job.id)
	if executionErr != nil || task == nil {
		if executionErr == nil {
			executionErr = errors.New("editable file task disappeared")
		}
		return
	}
	if job.validationErr != nil {
		executionErr = job.validationErr
		return
	}
	if job.kind == "psd" && job.imageCount == 0 {
		executionErr = errors.New("base64_images is empty")
		return
	}
	images := []upstream.ImageInput{}
	total := 0
	if job.imageCount > 16 {
		executionErr = problem(400, "too many images")
		return
	}
	stagingInput, _, pathErr := t.directories(task)
	if pathErr != nil {
		executionErr = pathErr
		return
	}
	for imageIndex := 0; imageIndex < job.imageCount; imageIndex++ {
		file, readErr := t.root.Open(path.Join(stagingInput, ".inputs", strconv.Itoa(imageIndex)))
		if readErr != nil {
			executionErr = readErr
			return
		}
		fileInfo, statErr := file.Stat()
		if statErr != nil || fileInfo.Size() > ((50<<20)+2)/3*4+1024 {
			file.Close()
			executionErr = problem(400, "image URL exceeds 50MB limit")
			return
		}
		encodedBytes, readErr := io.ReadAll(io.LimitReader(file, ((50<<20)+2)/3*4+1024))
		file.Close()
		if readErr != nil {
			executionErr = readErr
			return
		}
		encoded := string(encodedBytes)
		raw := strings.TrimSpace(encoded)
		media := ""
		if strings.HasPrefix(strings.ToLower(raw), "data:") {
			header, body, ok := strings.Cut(raw, ",")
			if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
				executionErr = problem(400, "invalid base64 image data")
				return
			}
			media = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
			raw = body
		}
		image, e := decodeImageBase64(raw, "", media)
		if e != nil {
			executionErr = e
			return
		}
		total += len(image.Data)
		if total > 100<<20 {
			executionErr = problem(400, "total image input is too large")
			return
		}
		conf, _, confErr := imagepkg.DecodeConfig(bytes.NewReader(image.Data))
		if confErr != nil || conf.Width <= 0 || conf.Height <= 0 || int64(conf.Width)*int64(conf.Height) > 100000000 {
			executionErr = problem(400, "invalid base64 image data")
			return
		}
		images = append(images, image)
	}
	if call != nil {
		call.Stage("getting_account", nil)
	}
	lease, e := t.account(ctx)
	if e != nil {
		executionErr = e
		return
	}
	defer lease.Release()
	if call != nil {
		call.Observe(Map{"account_email": lease.Account["email"]})
		call.Stage("image_egress_ready", Map{"account_email": lease.Account["email"]})
	}
	if e = t.update(ctx, job.owner, job.id, Map{"account_email": lease.Account["email"]}); e != nil {
		executionErr = e
		return
	}
	staging, published, e := t.directories(task)
	if e != nil {
		executionErr = e
		return
	}
	if e = t.root.MkdirAll(staging, 0700); e != nil {
		executionErr = e
		return
	}
	profile := ProxyProfile{}
	if lease.Egress != nil {
		profile = lease.Egress.Profile
	}
	export, e := t.backend.Export(ctx, upstream.EditableFileRequest{Kind: job.kind, Prompt: job.prompt, Images: images, Progress: func(stage string) {
		if call != nil {
			call.Stage(stage, nil)
		}
	}}, lease.Account, profile, filepath.Join(t.server.DataDir, "files", filepath.FromSlash(staging)))
	if e != nil {
		var upstreamFailure *upstream.Error
		if authTextFailure(e) && (!errors.As(e, &upstreamFailure) || (upstreamFailure.Stage != "editable_download" && upstreamFailure.Stage != "editable_upload")) {
			_ = t.server.Maintenance.scheduleVerification(ctx, lease.Account, "editable_file_task")
		}
		executionErr = e
		return
	}
	if export.AuthInvalid {
		_ = t.server.Maintenance.scheduleVerification(ctx, lease.Account, "editable_artifact_after_success")
	}
	if e = t.server.Accounts.RecordTextUse(ctx, text(lease.Account["management_id"])); e != nil {
		executionErr = e
		return
	}
	if e = t.root.RemoveAll(path.Join(staging, ".inputs")); e != nil {
		executionErr = e
		return
	}
	if e = t.validateArtifacts(staging, job.kind, export); e != nil {
		executionErr = e
		return
	}
	if e = ctx.Err(); e != nil {
		executionErr = e
		return
	}
	if _, e = t.root.Stat(published); e == nil || !os.IsNotExist(e) {
		executionErr = errors.New("editable file output directory already exists")
		return
	}
	if e = t.root.MkdirAll(job.kind, 0700); e != nil {
		executionErr = e
		return
	}
	if e = t.root.Rename(staging, published); e != nil {
		executionErr = e
		return
	}
	result = Map{"conversation_id": export.ConversationID, "primary_url": job.baseURL + "/files/" + published + "/" + urlPathName(export.PrimaryName), "zip_url": job.baseURL + "/files/" + published + "/" + urlPathName(export.ArchiveName)}
	if e = t.update(ctx, job.owner, job.id, Map{"status": "success", "result": result, "error": "", "ended_ts": editableTS(t.server.now())}); e != nil {
		executionErr = e
		return
	}
	if call != nil {
		call.detail["result"] = copyMap(result)
	}
}
func (t *EditableTasks) Delete(ctx context.Context, who Map, id string) (Map, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	owner := editableOwner(who)
	id = strings.TrimSpace(id)
	task, e := t.repository.Get(ctx, owner, id)
	if e != nil {
		return nil, e
	}
	if task == nil {
		return nil, problem(404, "editable file task not found")
	}
	status := text(task["status"])
	if status != "success" && status != "error" {
		return nil, problem(409, "editable file task is not terminal")
	}
	if !truth(task["delete_pending"]) {
		task, e = t.repository.Update(ctx, owner, id, Map{"delete_pending": true})
		if e != nil {
			return nil, e
		}
	}
	if e = t.cleanup(task); e != nil {
		return nil, problem(409, "editable file task files could not be removed")
	}
	if _, e = t.repository.Delete(ctx, owner, id); e != nil {
		return nil, e
	}
	return Map{"task_id": id, "deleted": true}, nil
}
func (t *EditableTasks) Close(ctx context.Context) error {
	t.mu.Lock()
	t.closing = true
	t.cancel()
	t.mu.Unlock()
	done := make(chan struct{})
	go func() { t.workers.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	t.closeOnce.Do(func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		for {
			select {
			case job := <-t.jobs:
				<-t.slots
				task, _ := t.repository.Get(finishCtx, job.owner, job.id)
				if task != nil {
					_ = t.cleanup(task)
				}
				t.recordError(t.update(finishCtx, job.owner, job.id, Map{"status": "error", "error": "服务已停止，未完成的任务已中断", "ended_ts": editableTS(t.server.now())}))
			default:
				t.closeErr = t.root.Close()
				t.release()
				return
			}
		}
	})
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return errors.Join(t.closeErr, t.backgroundErr)
}

func (t *EditableTasks) validateArtifacts(staging, kind string, result upstream.EditableFileExport) error {
	invalid := errors.New("editable file export produced invalid artifacts")
	if !upstream.SafeEditableFilename(result.PrimaryName) || !upstream.SafeEditableFilename(result.ArchiveName) || result.PrimaryName == result.ArchiveName || strings.ToLower(path.Ext(result.ArchiveName)) != ".zip" {
		return invalid
	}
	extension := strings.ToLower(path.Ext(result.PrimaryName))
	if (kind == "ppt" && extension != ".ppt" && extension != ".pptx") || (kind == "psd" && extension != ".psd") {
		return invalid
	}
	for _, name := range []string{result.PrimaryName, result.ArchiveName} {
		file, e := t.root.Open(path.Join(staging, name))
		if e != nil {
			return invalid
		}
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 100<<20 {
			file.Close()
			return invalid
		}
		if strings.HasSuffix(strings.ToLower(name), ".zip") || strings.HasSuffix(strings.ToLower(name), ".pptx") {
			archive, e := zip.NewReader(file, info.Size())
			if e != nil || len(archive.File) > 20000 {
				file.Close()
				return invalid
			}
			if strings.HasSuffix(strings.ToLower(name), ".pptx") {
				foundTypes, foundPPT := false, false
				for _, entry := range archive.File {
					foundTypes = foundTypes || entry.Name == "[Content_Types].xml"
					foundPPT = foundPPT || entry.Name == "ppt/presentation.xml"
				}
				if !foundTypes || !foundPPT {
					file.Close()
					return invalid
				}
			}
		} else {
			header := make([]byte, 26)
			_, e = io.ReadFull(file, header)
			if e != nil {
				file.Close()
				return invalid
			}
			if extension == ".psd" {
				if string(header[:4]) != "8BPS" || (binary.BigEndian.Uint16(header[4:6]) != 1 && binary.BigEndian.Uint16(header[4:6]) != 2) || !bytes.Equal(header[6:12], make([]byte, 6)) || binary.BigEndian.Uint16(header[12:14]) == 0 || binary.BigEndian.Uint32(header[14:18]) == 0 || binary.BigEndian.Uint32(header[18:22]) == 0 {
					file.Close()
					return invalid
				}
			} else if !bytes.Equal(header[:8], []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
				file.Close()
				return invalid
			}
		}
		file.Close()
	}
	return nil
}

func (t *EditableTasks) recordError(e error) {
	if e != nil {
		t.errMu.Lock()
		t.backgroundErr = errors.Join(t.backgroundErr, e)
		t.errMu.Unlock()
	}
}
