package controlplane

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// StartBackground runs the baseline account lifecycle and metrics schedules only
// after explicit opt-in. Every child belongs to Maintenance's shutdown barrier.
func (m *Maintenance) StartBackground(dashboard *Dashboard) {
	m.mu.Lock()
	if m.closing || m.background {
		m.mu.Unlock()
		return
	}
	m.background = true
	m.workers.Add(2)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			if e := dashboard.Sync(m.ctx); e != nil && m.ctx.Err() == nil {
				slog.Error("dashboard refresh failed", "error", safeCallText(e.Error(), 2000))
			}
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer m.workers.Done()
		for {
			m.lifecycleRound(m.ctx)
			values, e := m.accounts.settings.Values(m.ctx)
			minutes := 60
			if e == nil {
				minutes = max(1, integer(values["refresh_account_interval_minute"], 60))
			}
			timer := time.NewTimer(time.Duration(minutes) * time.Minute)
			select {
			case <-m.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}
func (m *Maintenance) lifecycleRound(ctx context.Context) {
	items, e := m.accounts.List(ctx)
	if e != nil {
		return
	}
	values, e := m.accounts.settings.Values(ctx)
	if e != nil {
		return
	}
	limit := max(1, min(128, integer(values["account_processing_concurrency"], 4)))
	jobs := make(chan Map)
	var workers sync.WaitGroup
	for i := 0; i < limit; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for a := range jobs {
				if ctx.Err() != nil {
					return
				}
				mid := text(a["management_id"])
				if a["last_remote_check_result"] == "pending" && a["pending_auth_scope"] == "image" {
					_ = m.scheduleVerification(ctx, a, text(orValue(a["last_remote_check_event"], "get_available_access_token")))
					continue
				}
				if (a["status"] == "正常" || a["status"] == "限流") && a["last_remote_check_result"] != "pending" && text(a["refresh_token"]) != "" && !truth(a["refresh_token_invalid_at"]) {
					expiry := CredentialLifecycle(a, m.accounts.now())["access_token_expires_at"]
					if expiry != nil && int64(integer(expiry, 0))-m.accounts.now().Unix() <= 86400 {
						_, _, _ = m.Refresh(ctx, mid, false)
					}
				}
				if a["status"] == "限流" && a["last_remote_check_result"] != "pending" {
					_, _ = m.Sync(ctx, mid)
				}
			}
		}()
	}
	for _, a := range items {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		case jobs <- a:
		}
	}
	close(jobs)
	workers.Wait()
}
