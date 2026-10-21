package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type PoolRequest struct {
	accountID      string
	pollOnly       bool
	Plan           string
	Plans          []string
	Source         string
	ImageRoute     string   `json:"-"`
	ExcludedTokens []string `json:"-"`
}

type imageEvidenceKey struct{ account, route string }
type imageEvidence struct {
	expires, failedAt time.Time
	proven            bool
}

// Execution evidence belongs to the Account Pool and never changes account
// credentials, quota/status, database payloads or model catalog projections.
func (a *Accounts) recordImageEvidence(account, route string, started time.Time, success bool, failure *ImageExecutionError) {
	if account == "" || route == "" || (!success && (failure == nil || failure.Code != "image_tool_error")) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	key := imageEvidenceKey{account, route}
	previous := a.imageEvidence[key]
	if success && previous.expires.After(now) && !previous.proven && !started.After(previous.failedAt) {
		return
	}
	for k, entry := range a.imageEvidence {
		if !entry.expires.After(now) {
			delete(a.imageEvidence, k)
		}
	}
	if _, exists := a.imageEvidence[key]; !exists && len(a.imageEvidence) >= 4096 {
		var oldest imageEvidenceKey
		var expiry time.Time
		for k, entry := range a.imageEvidence {
			if expiry.IsZero() || entry.expires.Before(expiry) {
				oldest, expiry = k, entry.expires
			}
		}
		delete(a.imageEvidence, oldest)
	}
	entry := imageEvidence{expires: now.Add(10 * time.Minute), proven: success}
	if !success {
		entry.failedAt = now
	}
	a.imageEvidence[key] = entry
	close(a.changed)
	a.changed = make(chan struct{})
}

type PoolError struct {
	Kind    string
	Status  int
	Message string
}

func (e *PoolError) Error() string { return e.Message }

type AccountLease struct {
	Account Map          `json:"-"`
	Egress  *EgressLease `json:"-"`
	once    sync.Once
	release func()
}

func (l *AccountLease) Release() {
	if l != nil {
		l.once.Do(func() {
			if l.Egress != nil {
				l.Egress.Release()
			}
			if l.release != nil {
				l.release()
			}
		})
	}
}
func matchesPool(m Map, r PoolRequest) bool {
	if r.accountID != "" && text(m["management_id"]) != r.accountID {
		return false
	}
	p := text(plan(m["type"]))
	if r.Plan != "" {
		expected := text(plan(r.Plan))
		if expected == "" || p == "" || p != expected {
			return false
		}
	}
	if len(r.Plans) > 0 {
		ok := false
		for _, v := range r.Plans {
			if expected := text(plan(v)); expected != "" && expected == p {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return r.Source == "" || source(m["source_type"]) == source(r.Source)
}
func excludedAccount(m Map, tokens []string) bool {
	token := text(m["access_token"])
	fps := stringsOf(m["access_token_fingerprints"])
	for _, excluded := range tokens {
		if token == strings.TrimSpace(excluded) {
			return true
		}
		fp := digest(strings.TrimSpace(excluded))
		for _, v := range fps {
			if v == fp {
				return true
			}
		}
	}
	return false
}
func imageCandidateVersion(m Map) string {
	return digest(canonical(merge(credentialGuard(m, false), Map{"status": m["status"], "last_remote_check_result": m["last_remote_check_result"], "last_remote_checked_at": m["last_remote_checked_at"]})))
}

func (a *Accounts) slot(ctx context.Context, r PoolRequest, attempted map[string]string, skip func(Map, string)) (*AccountLease, error) {
	for {
		if e := ctx.Err(); e != nil {
			if errors.Is(e, context.Canceled) {
				return nil, e
			}
			return nil, &PoolError{"deadline_exceeded", 504, "image request deadline exceeded while waiting for an account slot"}
		}
		items, e := a.List(ctx)
		if e != nil {
			return nil, e
		}
		values, e := a.settings.Values(ctx)
		if e != nil {
			return nil, e
		}
		limit := max(1, min(3, integer(values["image_account_concurrency"], 1)))
		ready := []Map{}
		matched, limited := 0, 0
		for _, m := range items {
			mid := text(m["management_id"])
			if version, ok := attempted[mid]; ok && version == imageCandidateVersion(m) {
				continue
			}
			reason := ""
			if excludedAccount(m, r.ExcludedTokens) {
				reason = "already_attempted"
			} else if !matchesPool(m, r) {
				reason = "model_filter"
			}
			if reason != "" {
				if skip != nil {
					skip(m, reason)
				}
				continue
			}
			matched++
			if m["status"] == "限流" {
				limited++
			}
			if imageAvailable(m, a.now()) || (r.pollOnly && selectable(m, a.now(), true, true)) {
				ready = append(ready, m)
			} else if skip != nil {
				reason = "account_unavailable"
				switch {
				case m["status"] == "限流":
					reason = "quota_exhausted"
				case m["status"] == "禁用":
					reason = "disabled"
				case CredentialLifecycle(m, a.now())["credential_availability"] == "unavailable":
					reason = "credentials_unavailable"
				case m["last_remote_check_result"] == "pending":
					reason = "remote_check_pending"
				}
				skip(m, reason)
			}
		}
		if len(ready) == 0 {
			if matched > 0 && matched == limited {
				return nil, &PoolError{"quota_exhausted", 429, "all matched image accounts are remote-confirmed quota exhausted"}
			}
			return nil, &PoolError{"unavailable", 503, "no image account is ready for current model/status filters"}
		}
		type rejection struct {
			account Map
			reason  string
		}
		rejections := []rejection{}
		report := func() {
			if skip != nil {
				for _, rejected := range rejections {
					skip(rejected.account, rejected.reason)
				}
			}
		}
		a.mu.Lock()
		present := map[string]bool{}
		for _, m := range items {
			present[text(m["management_id"])] = true
		}
		for key, entry := range a.imageEvidence {
			if !present[key.account] || !entry.expires.After(a.now()) {
				delete(a.imageEvidence, key)
			}
		}
		available := []Map{}
		preferred := []Map{}
		eligible := 0
		for _, m := range ready {
			mid := text(m["management_id"])
			entry, known := a.imageEvidence[imageEvidenceKey{mid, r.ImageRoute}]
			if known && !entry.proven && !r.pollOnly {
				rejections = append(rejections, rejection{m, "route_cooldown"})
				continue
			}
			eligible++
			if a.inflight[mid] < limit {
				available = append(available, m)
				if known && entry.proven {
					preferred = append(preferred, m)
				}
			} else if skip != nil {
				rejections = append(rejections, rejection{m, "account_busy"})
			}
		}
		if eligible == 0 {
			a.mu.Unlock()
			report()
			return nil, &PoolError{"unavailable", 503, "image route is temporarily unavailable after upstream tool failures"}
		}
		if len(preferred) > 0 {
			available = preferred
		}
		if len(available) > 0 {
			m := available[a.index%uint64(len(available))]
			a.index++
			mid := text(m["management_id"])
			a.inflight[mid]++
			a.mu.Unlock()
			report()
			return &AccountLease{Account: m, release: func() {
				a.mu.Lock()
				if a.inflight[mid] <= 1 {
					delete(a.inflight, mid)
				} else {
					a.inflight[mid]--
				}
				close(a.changed)
				a.changed = make(chan struct{})
				a.mu.Unlock()
			}}, nil
		}
		changed := a.changed
		a.mu.Unlock()
		report()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ctx.Err()
			}
			return nil, &PoolError{"deadline_exceeded", 504, "image request deadline exceeded while waiting for an account slot"}
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// AcquireImagePoll reserves the originating account's image capacity without
// generation preflight, quota admission or account substitution.
func (a *Accounts) AcquireImagePoll(ctx context.Context, accountID string) (*AccountLease, error) {
	if strings.TrimSpace(accountID) == "" || a.maintenance == nil {
		return nil, &PoolError{"unavailable", 503, "original image account is unavailable for polling"}
	}
	lease, err := a.slot(ctx, PoolRequest{accountID: accountID, pollOnly: true}, nil, nil)
	if err != nil {
		return nil, err
	}
	current, _, err := a.maintenance.Refresh(ctx, accountID, false)
	if err != nil || current == nil || !selectable(current, a.now(), true, true) {
		lease.Release()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &PoolError{"unavailable", 503, "original image account is unavailable for polling"}
	}
	egress, err := a.proxy.Resolve(ctx, current, "", false, true, true)
	if err != nil {
		lease.Release()
		return nil, err
	}
	if ctx.Err() != nil {
		egress.Release()
		lease.Release()
		return nil, ctx.Err()
	}
	lease.Account, lease.Egress = current, egress
	return lease, nil
}

// AcquireImage owns account admission, remote preflight, proxy capacity and
// release as one lease. Local quota estimates of zero do not exclude an account.
func (a *Accounts) AcquireImage(ctx context.Context, r PoolRequest) (*AccountLease, error) {
	if a.maintenance == nil {
		return nil, errors.New("account maintenance is not configured")
	}
	started := time.Now()
	stopReason := "selected"
	defer func() { recordAccountSelection(ctx, "finished", "", stopReason, time.Since(started)) }()
	attempted := map[string]string{}
	seen := map[string]string{}
	skip := func(account Map, reason string) {
		mid := text(account["management_id"])
		if seen[mid] != reason {
			seen[mid] = reason
			recordAccountSelection(ctx, "skipped", mid, reason, 0)
		}
	}
	waitReason := ""
	for {
		// Capture before reading the pool so a concurrent import cannot be missed.
		a.mu.Lock()
		changed := a.changed
		a.mu.Unlock()
		lease, e := a.slot(ctx, r, attempted, skip)
		if e != nil {
			var selection *PoolError
			if !errors.As(e, &selection) || selection.Kind == "deadline_exceeded" {
				stopReason = "selection_error"
				if ctx.Err() != nil {
					stopReason = ctx.Err().Error()
				}
				return nil, e
			}
			quotaOnly := false
			for _, reason := range seen {
				if reason == "quota_exhausted" {
					quotaOnly = true
				} else if reason != "model_filter" && reason != "already_attempted" {
					quotaOnly = false
					break
				}
			}
			if selection.Kind != "quota_exhausted" && quotaOnly {
				selection = &PoolError{"quota_exhausted", 429, "all usable image accounts remote-confirmed quota exhausted"}
			}
			stopReason = selection.Kind
			if _, bounded := ctx.Deadline(); !bounded {
				return nil, selection
			}
			if waitReason != selection.Message {
				waitReason = selection.Message
				recordAccountSelection(ctx, "waiting", "", waitReason, 0)
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				stopReason = ctx.Err().Error()
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil, ctx.Err()
				}
				return nil, selection
			case <-changed:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		mid := text(lease.Account["management_id"])
		attempted[mid] = imageCandidateVersion(lease.Account)
		recordAccountSelection(ctx, "checked", mid, "", 0)
		current, e := a.maintenance.Preflight(ctx, mid)
		if e != nil {
			lease.Release()
			if ctx.Err() != nil {
				stopReason = ctx.Err().Error()
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil, ctx.Err()
				}
				return nil, &PoolError{"deadline_exceeded", 504, "image request deadline exceeded during remote account validation"}
			}
			skip(lease.Account, "preflight_failed")
			latest, lookupErr := a.Get(ctx, mid)
			if lookupErr != nil {
				stopReason = "selection_error"
				return nil, lookupErr
			}
			if latest != nil && latest["last_remote_check_result"] != "ok" {
				attempted[mid] = imageCandidateVersion(latest)
			}
			continue
		}
		if current != nil {
			attempted[mid] = imageCandidateVersion(current)
		}
		if current != nil && imageAvailable(current, a.now()) && matchesPool(current, r) {
			egress, e := a.proxy.Resolve(ctx, current, "", false, true, true)
			if e != nil {
				lease.Release()
				stopReason = "egress_unavailable"
				return nil, e
			}
			if ctx.Err() != nil {
				egress.Release()
				lease.Release()
				stopReason = ctx.Err().Error()
				return nil, ctx.Err()
			}
			lease.Account, lease.Egress = current, egress
			stopReason = "selected"
			return lease, nil
		}
		if current != nil && current["status"] == "限流" {
			skip(lease.Account, "quota_exhausted")
		} else {
			skip(lease.Account, "preflight_unavailable")
		}
		lease.Release()
	}
}
func (a *Accounts) AcquireText(ctx context.Context, excluded []string) (*AccountLease, error) {
	attempted := map[string]bool{}
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		items, e := a.List(ctx)
		if e != nil {
			return nil, e
		}
		ready := []Map{}
		for _, m := range items {
			if !attempted[text(m["management_id"])] && !excludedAccount(m, excluded) && selectable(m, a.now(), true, true) {
				ready = append(ready, m)
			}
		}
		if len(ready) == 0 {
			return nil, &PoolError{"unavailable", 503, "no text account is ready"}
		}
		a.mu.Lock()
		candidate := ready[a.index%uint64(len(ready))]
		a.index++
		a.mu.Unlock()
		mid := text(candidate["management_id"])
		attempted[mid] = true
		current, _, e := a.maintenance.Refresh(ctx, mid, false)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if current == nil {
			continue
		}
		egress, e := a.proxy.Resolve(ctx, current, "", false, true, false)
		if e != nil {
			return nil, e
		}
		return &AccountLease{Account: current, Egress: egress}, nil
	}
}

// RecordTextUse atomically preserves additive counters from other writers.
func (a *Accounts) RecordTextUse(ctx context.Context, mid string) error {
	_, e := a.db.Modify(ctx, "accounts", nil, func(raw []Map) ([]Map, error) {
		for _, m := range raw {
			if m["management_id"] == mid {
				m["success"] = integer(m["success"], 0) + 1
				m["last_used_at"] = iso(a.now())
				break
			}
		}
		return raw, nil
	})
	return e
}
