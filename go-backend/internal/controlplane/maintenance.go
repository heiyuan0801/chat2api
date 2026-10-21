package controlplane

import (
	"context"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"strings"
	"sync"
	"time"
)

// AccountUpstream is the network seam for maintenance. NativeAccountUpstream is
// the real Go adapter; deterministic fixtures implement this same interface.
type AccountUpstream interface {
	Refresh(context.Context, Map, ProxyProfile) (upstream.CredentialTokens, error)
	Synchronize(context.Context, Map, ProxyProfile) (Map, error)
}
type NativeAccountUpstream struct{}

func (NativeAccountUpstream) Refresh(ctx context.Context, account Map, profile ProxyProfile) (upstream.CredentialTokens, error) {
	cfg := upstream.Config{AccessToken: text(account["access_token"]), ProxyURL: profile.ProxyURL, Timeout: 60 * time.Second}
	applyProxySession(profile, &cfg)
	c, e := upstream.New(cfg)
	if e != nil {
		return upstream.CredentialTokens{}, e
	}
	defer c.Close()
	return c.RefreshCredentials(ctx, text(account["refresh_token"]))
}
func (NativeAccountUpstream) Synchronize(ctx context.Context, account Map, profile ProxyProfile) (Map, error) {
	cfg := upstream.Config{AccessToken: text(account["access_token"]), ProxyURL: profile.ProxyURL, Timeout: 60 * time.Second}
	applyProxySession(profile, &cfg)
	c, e := upstream.New(cfg)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	info, e := c.AccountInformation(ctx)
	if e != nil {
		return nil, e
	}
	limits := list(info.Conversation["limits_progress"])
	quota, unknown := 0, true
	var restore any
	for _, v := range limits {
		m := object(v)
		if m["feature_name"] == "image_gen" {
			quota = integer(m["remaining"], 0)
			unknown = false
			restore = orValue(text(m["reset_after"]), nil)
			break
		}
	}
	status := "正常"
	if !unknown && quota <= 0 {
		status = "限流"
	}
	out := Map{"email": info.Me["email"], "user_id": info.Me["id"], "quota": quota, "image_quota_unknown": unknown, "limits_progress": limits, "default_model_slug": info.Conversation["default_model_slug"], "restore_at": restore, "status": status}
	if p := plan(info.Account["plan_type"]); p != nil {
		out["type"] = p
	}
	return out, nil
}

type refreshFlight struct {
	done   chan struct{}
	result Map
	err    error
}
type Maintenance struct {
	accounts   *Accounts
	upstream   AccountUpstream
	mu         sync.Mutex
	flights    map[string]*refreshFlight
	ctx        context.Context
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	verifying  map[string]bool
	closing    bool
	background bool
}

func NewMaintenance(parent context.Context, a *Accounts, remote AccountUpstream) *Maintenance {
	if remote == nil {
		remote = NativeAccountUpstream{}
	}
	ctx, cancel := context.WithCancel(parent)
	m := &Maintenance{accounts: a, upstream: remote, flights: map[string]*refreshFlight{}, ctx: ctx, cancel: cancel, verifying: map[string]bool{}}
	a.maintenance = m
	return m
}
func credentialGuard(m Map, includeRemote bool) Map {
	out := Map{"access_token": m["access_token"], "refresh_token": m["refresh_token"], "last_token_refresh_at": m["last_token_refresh_at"]}
	if includeRemote {
		for _, k := range []string{"last_remote_check_attempt_at", "last_remote_checked_at", "last_remote_check_result", "pending_auth_verification_id"} {
			out[k] = m[k]
		}
	}
	return out
}
func flightKey(m Map) string {
	return text(m["management_id"]) + ":" + digest(canonical(credentialGuard(m, false)))
}

// apply preserves a credential generation against late remote results. It never
// calls the network under a SQL lock and never resurrects a deleted account.
func (m *Maintenance) apply(ctx context.Context, mid string, snapshot, patch Map, remoteMarker, allowRemove bool) (Map, error) {
	a := m.accounts
	settings, e := a.settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	guard := credentialGuard(snapshot, remoteMarker)
	var result Map
	_, e = a.db.Modify(ctx, "accounts", nil, func(raw []Map) ([]Map, error) {
		items := normalizeAccounts(raw, a.now(), false)
		current := byID(items, mid)
		if current == nil {
			return nil, ErrCredentialsChanged
		}
		for k, v := range guard {
			if canonical(current[k]) != canonical(v) {
				return nil, ErrCredentialsChanged
			}
		}
		next := copyMap(current)
		for k, v := range patch {
			next[k] = v
		}
		if current["status"] == "禁用" {
			next["status"] = "禁用"
		}
		next = NormalizeAccount(next, a.now())
		if next == nil {
			return nil, ErrCredentialsChanged
		}
		if other := byToken(items, text(next["access_token"])); other != nil && other["management_id"] != current["management_id"] {
			return nil, ErrCredentialsChanged
		}
		removed := allowRemove && ((next["status"] == "异常" && boolean(settings["auto_remove_invalid_accounts"], true)) || (next["status"] == "限流" && boolean(settings["auto_remove_rate_limited_accounts"], false)))
		out := []Map{}
		for _, v := range items {
			if v["management_id"] == mid {
				if !removed {
					out = append(out, next)
					result = next
				} else {
					result = copyMap(next)
					result["_removed_after_refresh"] = true
				}
			} else {
				out = append(out, v)
			}
		}
		return out, nil
	})
	if e == nil {
		a.notify()
	}
	return result, e
}
func refreshProblem(code, message string) error {
	return &maintenanceProblem{code: code, message: message}
}

type maintenanceProblem struct{ code, message string }

func (e *maintenanceProblem) Error() string { return e.message }
func (m *Maintenance) Refresh(ctx context.Context, mid string, force bool) (Map, bool, error) {
	return m.refresh(ctx, mid, force, 0)
}
func (m *Maintenance) refresh(ctx context.Context, mid string, force bool, attempt int) (Map, bool, error) {
	a := m.accounts
	mid = strings.ToLower(strings.TrimSpace(mid))
	current, e := a.Get(ctx, mid)
	if e != nil {
		return nil, false, e
	}
	if current == nil {
		return nil, false, refreshProblem("account_not_found", "account not found")
	}
	lifecycle := CredentialLifecycle(current, a.now())
	expires := lifecycle["access_token_expires_at"]
	if !force && (expires == nil || int64(integer(expires, 0))-a.now().Unix() > 86400) {
		return current, false, nil
	}
	if text(current["refresh_token"]) == "" {
		if force {
			return current, false, refreshProblem("refresh_token_missing", "refresh token is missing")
		}
		return current, false, nil
	}
	if truth(current["refresh_token_invalid_at"]) {
		if !force && lifecycle["access_token_status"] != "invalid" {
			return current, false, nil
		}
		return current, false, refreshProblem("refresh_token_invalid", "refresh token is invalid")
	}
	if !force && text(current["last_token_refresh_error"]) != "" {
		if at, ok := timestamp(current["last_token_refresh_error_at"]).(int64); ok && a.now().Unix()-at < 300 {
			return current, false, nil
		}
	}
	key := flightKey(current)
	complete := func(result Map, err error) (Map, bool, error) {
		if err == nil {
			return copyMap(result), true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if errors.Is(err, ErrCredentialsChanged) && attempt == 0 {
			latest, lookupErr := a.Get(ctx, mid)
			if lookupErr == nil && latest != nil && flightKey(latest) != key {
				if latest["last_token_refresh_at"] != current["last_token_refresh_at"] {
					return latest, true, nil
				}
				return m.refresh(ctx, mid, force, attempt+1)
			}
		}
		var terminal *upstream.CredentialError
		if !force && !errors.Is(err, ErrCredentialsChanged) && (!errors.As(err, &terminal) || !terminal.Terminal) {
			return current, false, nil
		}
		return result, false, err
	}
	m.mu.Lock()
	if existing := m.flights[key]; existing != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-existing.done:
			return complete(existing.result, existing.err)
		}
	}
	flight := &refreshFlight{done: make(chan struct{})}
	m.flights[key] = flight
	m.mu.Unlock()
	result, err := m.exchange(ctx, current)
	m.mu.Lock()
	flight.result, flight.err = result, err
	delete(m.flights, key)
	close(flight.done)
	m.mu.Unlock()
	return complete(result, err)
}
func (m *Maintenance) exchange(ctx context.Context, current Map) (Map, error) {
	a := m.accounts
	ctx, release, e := a.gate.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	fresh, lookupErr := a.Get(ctx, text(current["management_id"]))
	if lookupErr != nil {
		return nil, lookupErr
	}
	if fresh == nil || flightKey(fresh) != flightKey(current) {
		return nil, ErrCredentialsChanged
	}
	lease, e := a.proxy.Resolve(ctx, current, "", false, false, false)
	if e != nil {
		return nil, e
	}
	defer lease.Release()
	tokens, e := m.upstream.Refresh(ctx, current, lease.Profile)
	mid := text(current["management_id"])
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := diagnostic(e.Error(), current, 2000)
		patch := Map{"last_token_refresh_error": message, "last_token_refresh_error_at": iso(a.now())}
		var refresh *upstream.CredentialError
		if errors.As(e, &refresh) && refresh.Terminal {
			patch["refresh_token_invalid_at"] = iso(a.now())
		}
		_, writeErr := m.apply(ctx, mid, current, patch, false, false)
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, e
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, refreshProblem("upstream_unavailable", "refresh returned an empty access token")
	}
	patch := Map{"access_token": tokens.AccessToken, "last_token_refresh_at": iso(a.now()), "last_token_refresh_error": nil, "last_token_refresh_error_at": nil, "refresh_token_invalid_at": nil, "invalid_count": 0, "last_invalid_at": nil, "last_refresh_error": nil, "last_refresh_error_at": nil}
	if tokens.RefreshToken != "" {
		patch["refresh_token"] = tokens.RefreshToken
	}
	if tokens.IDToken != "" {
		patch["id_token"] = tokens.IDToken
	}
	if current["status"] == "异常" {
		patch["status"] = "正常"
	}
	if current["last_remote_check_result"] == "invalid" {
		for _, k := range []string{"last_remote_check_result", "last_remote_check_error", "last_remote_check_error_at", "last_remote_check_event", "pending_auth_remove_invalid", "pending_auth_scope"} {
			patch[k] = nil
		}
	}
	return m.apply(ctx, mid, current, patch, false, false)
}
func authFailure(e error) bool { var v *upstream.Error; return errors.As(e, &v) && v.Status == 401 }
func (m *Maintenance) Sync(ctx context.Context, mid string) (Map, error) {
	return m.synchronize(ctx, mid, 0)
}
func (m *Maintenance) Preflight(ctx context.Context, mid string) (Map, error) {
	return m.synchronize(ctx, mid, 1)
}
func (m *Maintenance) synchronize(ctx context.Context, mid string, mode int) (Map, error) {
	a := m.accounts
	mid = strings.ToLower(strings.TrimSpace(mid))
	current, e := a.Get(ctx, mid)
	if mode == 1 {
		current, _, e = m.Refresh(ctx, mid, false)
	}
	if e != nil {
		return nil, e
	}
	if current == nil {
		return nil, ErrCredentialsChanged
	}
	event := "sync_accounts_and_quota"
	if mode == 1 {
		event = "get_available_access_token"
	} else if mode == 2 {
		event = text(current["last_remote_check_event"])
	}
	var result Map
	for attempt := 0; attempt < 2; attempt++ {
		lease, e := a.proxy.Resolve(ctx, current, "", false, false, false)
		if e != nil {
			return nil, e
		}
		callCtx, release, slotErr := a.gate.Acquire(ctx)
		if slotErr != nil {
			lease.Release()
			return nil, slotErr
		}
		result, e = m.upstream.Synchronize(callCtx, current, lease.Profile)
		release()
		lease.Release()
		if e == nil {
			now := iso(a.now())
			patch := merge(result, Map{"invalid_count": 0, "last_invalid_at": nil, "last_refresh_error": nil, "last_refresh_error_at": nil, "last_remote_checked_at": now, "last_remote_check_attempt_at": now, "last_remote_check_error": nil, "last_remote_check_error_at": nil, "last_remote_check_event": event, "last_remote_check_result": "ok", "pending_auth_remove_invalid": nil, "pending_auth_scope": nil})
			if current["status"] == "禁用" {
				patch["status"] = "禁用"
			}
			updated, writeErr := m.apply(ctx, mid, current, patch, true, true)
			if errors.Is(writeErr, ErrCredentialsChanged) {
				latest, lookupErr := a.Get(ctx, mid)
				if lookupErr == nil && latest != nil && canonical(credentialGuard(latest, false)) == canonical(credentialGuard(current, false)) {
					return latest, nil
				}
			}
			return updated, writeErr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if authFailure(e) && mode == 1 {
			if scheduleErr := m.scheduleVerification(ctx, current, event); scheduleErr != nil {
				return nil, scheduleErr
			}
			return nil, e
		}
		if authFailure(e) && attempt == 0 && text(current["refresh_token"]) != "" && !truth(current["refresh_token_invalid_at"]) {
			latest, lookupErr := a.Get(ctx, mid)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if latest == nil || canonical(credentialGuard(latest, true)) != canonical(credentialGuard(current, true)) {
				return nil, ErrCredentialsChanged
			}
			next, _, refreshErr := m.Refresh(ctx, mid, true)
			if refreshErr == nil && next != nil {
				current = next
				continue
			}
			if refreshErr != nil {
				e = refreshErr
			}
		}
		now := iso(a.now())
		patch := Map{"last_remote_check_attempt_at": now, "last_remote_check_error": diagnostic(e.Error(), current, 2000), "last_remote_check_error_at": now, "last_remote_check_event": event, "last_remote_check_result": "error", "pending_auth_remove_invalid": nil, "pending_auth_scope": nil}
		if current["last_remote_check_result"] == "invalid" {
			patch["last_remote_check_result"] = "invalid"
		}
		var refresh *upstream.CredentialError
		terminal := errors.As(e, &refresh) && refresh.Terminal
		if authFailure(e) || terminal {
			patch["last_remote_check_result"] = "invalid"
			patch["invalid_count"] = integer(current["invalid_count"], 0) + 1
			patch["last_invalid_at"] = now
			if text(current["refresh_token"]) == "" || truth(current["refresh_token_invalid_at"]) || terminal {
				if current["status"] != "禁用" {
					patch["status"] = "异常"
				}
			}
		}
		_, writeErr := m.apply(ctx, mid, current, patch, true, authFailure(e) || terminal)
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, e
	}
	return nil, ErrCredentialsChanged
}

// scheduleVerification blocks image admission immediately, then verifies the
// rejected generation outside the request. The shared processing gate bounds
// network work; deduplication and a queue bound prevent unbounded goroutines.
func (m *Maintenance) scheduleVerification(ctx context.Context, current Map, event string) error {
	mid := text(current["management_id"])
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return context.Canceled
	}
	if m.verifying[mid] {
		m.mu.Unlock()
		return nil
	}
	if len(m.verifying) >= 128 {
		m.mu.Unlock()
		return problem(503, "account verification queue is full")
	}
	m.verifying[mid] = true
	m.workers.Add(1)
	m.mu.Unlock()
	queued := false
	defer func() {
		if !queued {
			m.mu.Lock()
			delete(m.verifying, mid)
			m.mu.Unlock()
			m.workers.Done()
		}
	}()
	values, e := m.accounts.settings.Values(ctx)
	if e != nil {
		return e
	}
	pending, e := m.apply(ctx, mid, current, Map{"last_remote_check_result": "pending", "last_remote_check_event": event, "last_remote_check_attempt_at": iso(m.accounts.now()), "last_remote_check_error": nil, "last_remote_check_error_at": nil, "pending_auth_scope": "image", "pending_auth_verification_id": id(16), "pending_auth_remove_invalid": boolean(values["auto_remove_invalid_accounts"], true)}, true, false)
	if e != nil {
		return e
	}
	queued = true
	go func() {
		defer m.workers.Done()
		defer func() { m.mu.Lock(); delete(m.verifying, mid); m.mu.Unlock() }()
		jobCtx, cancel := context.WithTimeout(m.ctx, 3*time.Minute)
		defer cancel()
		latest, e := m.accounts.Get(jobCtx, mid)
		if e != nil || latest == nil || latest["pending_auth_verification_id"] != pending["pending_auth_verification_id"] {
			return
		}
		_, _ = m.synchronize(jobCtx, mid, 2)
	}()
	return nil
}
func (m *Maintenance) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	m.cancel()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}
