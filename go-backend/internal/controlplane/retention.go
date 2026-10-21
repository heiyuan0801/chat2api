package controlplane

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
)

// RetentionCleanup is the shared lifecycle owner for manual and scheduled log,
// image-retention and disk-target cleanup. Asset mutations stay in ImageAssets.
type RetentionCleanup struct {
	s       *Server
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
	start   sync.Once
	wake    chan struct{}
}

func NewRetentionCleanup(s *Server) *RetentionCleanup {
	ctx, cancel := context.WithCancel(s.executionContext)
	return &RetentionCleanup{s: s, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
}
func (r *RetentionCleanup) admit() (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return nil, context.Canceled
	}
	r.wg.Add(1)
	return r.wg.Done, nil
}
func (r *RetentionCleanup) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *RetentionCleanup) stamp() string {
	return r.s.now().In(imageTaskZone).Format("2006-01-02 15:04:05")
}
func (r *RetentionCleanup) Status(ctx context.Context) (Map, error) {
	state, e := r.s.DB.CoordinationState(ctx, "retention_cleanup_state")
	if e != nil {
		return nil, e
	}
	out := Map{"running": false, "last_started_at": nil, "last_finished_at": nil, "next_run_at": nil, "last_removed": Map{"logs": 0, "images": 0}, "last_error": Map{"logs": nil, "images": nil}}
	for _, key := range []string{"running", "last_started_at", "last_finished_at", "next_run_at"} {
		if value, ok := state[key]; ok {
			out[key] = value
		}
	}
	for _, key := range []string{"last_removed", "last_error"} {
		for field, value := range object(state[key]) {
			object(out[key])[field] = value
		}
	}
	return out, nil
}
func (r *RetentionCleanup) Notify(ctx context.Context) error {
	_, e := r.s.DB.UpdateCoordinationState(ctx, "retention_cleanup_state", func(state Map) (Map, error) {
		now := time.Now()
		state["next_run_epoch"], state["next_run_at"], state["wake_revision"] = float64(now.UnixNano())/1e9, now.In(imageTaskZone).Format("2006-01-02 15:04:05"), integer(state["wake_revision"], 0)+1
		return state, nil
	})
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return e
}
func (r *RetentionCleanup) runOwner(parent context.Context) (context.Context, func(), error) {
	done, e := r.admit()
	if e != nil {
		return nil, nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(r.ctx, cancel)
	cleanup := func() { stop(); cancel(); done() }
	for {
		lease, e := r.s.DB.TryCoordinationLease(ctx, "retention_cleanup:run", 300*time.Second)
		if e != nil {
			cleanup()
			return nil, nil, e
		}
		if lease != nil {
			return lease.Context(), func() { lease.Close(); cleanup() }, nil
		}
		select {
		case <-ctx.Done():
			cleanup()
			return nil, nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (r *RetentionCleanup) markStarted(ctx context.Context) error {
	_, e := r.s.DB.UpdateCoordinationState(ctx, "retention_cleanup_state", func(state Map) (Map, error) {
		state["running"], state["last_started_at"] = true, r.stamp()
		return state, nil
	})
	return e
}
func retentionError(e error) string {
	if e == nil {
		return ""
	}
	var problem *Problem
	if errors.As(e, &problem) {
		return problem.Message
	}
	if errors.Is(e, context.Canceled) {
		return "retention cleanup interrupted"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return "retention cleanup timed out"
	}
	return "retention cleanup failed"
}
func (r *RetentionCleanup) markFinished(ctx context.Context, results Map, failures map[string]error, domains []string) error {
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, e := r.s.DB.UpdateCoordinationState(commit, "retention_cleanup_state", func(state Map) (Map, error) {
		removed, errs := object(state["last_removed"]), object(state["last_error"])
		for _, domain := range domains {
			removed[domain] = integer(object(results[domain])["removed"], 0)
			errs[domain] = nil
			if failures[domain] != nil {
				errs[domain] = retentionError(failures[domain])
			}
		}
		state["running"], state["last_finished_at"], state["last_removed"], state["last_error"] = false, r.stamp(), removed, errs
		return state, nil
	})
	return e
}
func (r *RetentionCleanup) logCleanup(ctx context.Context, hours int, dry bool) (Map, error) {
	if !dry {
		if e := r.s.Dashboard.Sync(ctx); e != nil {
			return nil, e
		}
	}
	cutoff := r.s.now().In(imageTaskZone).Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
	return repository.NewCallRecords(r.s.DB).Cleanup(ctx, cutoff, dry)
}
func (r *RetentionCleanup) Run(ctx context.Context, body Map, dry bool) (Map, error) {
	values, e := r.s.Settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	logsHours := max(1, integer(body["log_retention_hours"], integer(values["log_retention_hours"], 24)))
	imagesHours := max(1, integer(body["image_retention_hours"], integer(values["image_retention_hours"], 24)))
	if integer(body["log_retention_hours"], 0) == 0 {
		logsHours = max(1, integer(values["log_retention_hours"], 24))
	}
	if integer(body["image_retention_hours"], 0) == 0 {
		imagesHours = max(1, integer(values["image_retention_hours"], 24))
	}
	ctx, release, e := r.runOwner(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	if !dry {
		if e = r.markStarted(ctx); e != nil {
			return nil, e
		}
	}
	logs, e := r.logCleanup(ctx, logsHours, dry)
	if e != nil {
		if !dry {
			e = errors.Join(e, r.markFinished(ctx, nil, map[string]error{"logs": e}, []string{"logs"}))
		}
		return nil, e
	}
	images, e := r.s.Assets.Retention(ctx, imagesHours, dry)
	if e != nil {
		if !dry {
			e = errors.Join(e, r.markFinished(ctx, Map{"logs": logs}, map[string]error{"images": e}, []string{"logs", "images"}))
		}
		return nil, e
	}
	if !dry {
		if e = r.markFinished(ctx, Map{"logs": logs, "images": images}, nil, []string{"logs", "images"}); e != nil {
			return nil, e
		}
	}
	logs["retention_hours"] = logsHours
	images["retention_hours"] = imagesHours
	return Map{"dry_run": dry, "logs": logs, "images": images, "total_removed": integer(logs["removed"], 0) + integer(images["removed"], 0), "total_size_bytes": integer(logs["removed_size_bytes"], 0) + integer(images["removed_size_bytes"], 0)}, nil
}
func (r *RetentionCleanup) RunImages(ctx context.Context, hours int, dry bool) (Map, error) {
	ctx, release, e := r.runOwner(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	if !dry {
		if e = r.markStarted(ctx); e != nil {
			return nil, e
		}
	}
	result, e := r.s.Assets.Retention(ctx, hours, dry)
	if !dry {
		e = errors.Join(e, r.markFinished(ctx, Map{"images": result}, map[string]error{"images": e}, []string{"images"}))
	}
	return result, e
}
func (r *RetentionCleanup) CleanupToTarget(ctx context.Context, target int, dry bool) (Map, error) {
	ctx, release, e := r.runOwner(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	if !dry {
		if e = r.markStarted(ctx); e != nil {
			return nil, e
		}
	}
	result, e := r.s.Assets.CleanupToTarget(ctx, target, dry)
	if !dry {
		e = errors.Join(e, r.markFinished(ctx, Map{"images": result}, map[string]error{"images": e}, []string{"images"}))
	}
	return result, e
}
func (r *RetentionCleanup) automaticOwned(ctx context.Context, enforce bool) (Map, error) {
	result := Map{"logs": nil, "images": nil, "image_space": nil, "errors": Map{}}
	if e := r.markStarted(ctx); e != nil {
		return nil, e
	}
	values, e := r.s.Settings.Values(ctx)
	if e != nil {
		e = errors.Join(e, r.markFinished(ctx, nil, map[string]error{"logs": e}, []string{"logs"}))
		return nil, e
	}
	failures := map[string]error{}
	logs, e := r.logCleanup(ctx, max(1, integer(values["log_retention_hours"], 24)), false)
	result["logs"] = logs
	if e != nil {
		failures["logs"] = e
		object(result["errors"])["logs"] = retentionError(e)
	}
	images, e := r.s.Assets.Retention(ctx, max(1, integer(values["image_retention_hours"], 24)), false)
	result["images"] = images
	if e == nil && enforce {
		stats, se := r.s.Assets.StorageStats(ctx)
		if se == nil {
			free := integer(stats["disk_free_mb"], 0)
			minFree := integer(values["image_min_free_mb"], 500)
			if free < minFree {
				cleanup, ce := r.s.Assets.CleanupToTarget(ctx, minFree, false)
				result["image_space"] = Map{"free_mb": free, "min_free_mb": minFree, "cleanup": cleanup}
				e = ce
			}
		} else {
			e = se
		}
	}
	if e != nil {
		failures["images"] = e
		object(result["errors"])["images"] = retentionError(e)
	}
	statusImages := copyMap(images)
	if extra := object(object(result["image_space"])["cleanup"]); len(extra) > 0 {
		statusImages["removed"] = integer(statusImages["removed"], 0) + integer(extra["removed"], 0)
	}
	e = r.markFinished(ctx, Map{"logs": logs, "images": statusImages}, failures, []string{"logs", "images"})
	return result, e
}
func (r *RetentionCleanup) scheduleAfter(ctx context.Context, wake int) error {
	_, e := r.s.DB.UpdateCoordinationState(ctx, "retention_cleanup_state", func(state Map) (Map, error) {
		if integer(state["wake_revision"], 0) == wake {
			next := time.Now().Add(1800 * time.Second)
			state["next_run_epoch"], state["next_run_at"] = float64(next.UnixNano())/1e9, next.In(imageTaskZone).Format("2006-01-02 15:04:05")
		}
		return state, nil
	})
	return e
}
func (r *RetentionCleanup) Startup(ctx context.Context) (Map, error) {
	ctx, release, e := r.runOwner(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	state, e := r.s.DB.CoordinationState(ctx, "retention_cleanup_state")
	if e != nil {
		return nil, e
	}
	last, _ := number(state["last_startup_epoch"])
	now := float64(time.Now().UnixNano()) / 1e9
	if now-last < 60 {
		return Map{"logs": nil, "images": nil, "image_space": nil, "errors": Map{}, "skipped": true}, nil
	}
	wake := integer(state["wake_revision"], 0)
	result, e := r.automaticOwned(ctx, false)
	if e == nil {
		e = r.scheduleAfter(ctx, wake)
	}
	if e == nil {
		_, e = r.s.DB.UpdateCoordinationState(ctx, "retention_cleanup_state", func(state Map) (Map, error) {
			state["last_startup_epoch"] = float64(time.Now().UnixNano()) / 1e9
			return state, nil
		})
	}
	return result, e
}
func (r *RetentionCleanup) Start() {
	r.start.Do(func() {
		done, e := r.admit()
		if e != nil {
			return
		}
		go func() {
			defer done()
			_, _ = r.Startup(r.ctx)
			_, _ = r.s.cleanupAccounts(r.ctx, Map{}, false)
			for r.ctx.Err() == nil {
				leader, e := r.s.DB.TryCoordinationLease(r.ctx, "retention_cleanup:scheduler_leader", 5*time.Second)
				if e != nil || leader == nil {
					select {
					case <-r.ctx.Done():
						return
					case <-time.After(250 * time.Millisecond):
						continue
					}
				}
				r.leaderLoop(leader.Context())
				leader.Close()
			}
		}()
	})
}
func (r *RetentionCleanup) leaderLoop(ctx context.Context) {
	for ctx.Err() == nil {
		state, e := r.s.DB.CoordinationState(ctx, "retention_cleanup_state")
		if e != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		epoch, _ := number(state["next_run_epoch"])
		if epoch <= 0 {
			_ = r.scheduleAfter(ctx, integer(state["wake_revision"], 0))
			continue
		}
		if float64(time.Now().UnixNano())/1e9 >= epoch {
			runCtx, release, e := r.runOwner(ctx)
			if e != nil {
				return
			}
			wake := integer(state["wake_revision"], 0)
			_, e = r.automaticOwned(runCtx, true)
			if e == nil {
				e = r.scheduleAfter(runCtx, wake)
			}
			release()
			if e != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (s *Server) registerCleanup() {
	for _, spec := range []struct {
		path string
		dry  bool
	}{{"preview", true}, {"run", false}} {
		dry := spec.dry
		s.route("POST", "/api/settings/retention-cleanup/"+spec.path, "admin", "RetentionCleanupRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
			return s.Retention.Run(ctx, body, dry)
		})
		s.route("POST", "/api/settings/account-cleanup/"+spec.path, "admin", "AccountCleanupRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
			return s.cleanupAccounts(ctx, body, dry)
		})
	}
}
