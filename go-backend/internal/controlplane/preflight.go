package controlplane

import (
	"context"
	"errors"
	"strings"
	"time"
)

const preflightTTL = 10 * time.Second
const preflightCacheCapacity = 1024

type preflightEvidence struct {
	signature string
	expires   time.Time
}
type preflightFlight struct {
	done        chan struct{}
	mid         string
	invalidated bool
	err         error
}

// Only evidence is cached; the Account Repository remains the source of quota,
// status and credentials. Runtime slot counts must not change the signature.
func (m *Maintenance) preflightSignature(ctx context.Context, account Map) (string, string, error) {
	configuration := Map{}
	for _, name := range []string{"proxy_configuration", "account_group_configuration", "system_settings"} {
		raw, err := m.accounts.db.Document(ctx, name)
		if err != nil {
			return "", "", err
		}
		if name == "system_settings" {
			raw = object(raw["proxy_runtime"])
		}
		configuration[name] = raw
	}
	routing := digest(canonical(configuration))
	return digest(canonical(stripRuntime(account)) + routing), routing, nil
}

func (m *Maintenance) invalidatePreflight(mid string) {
	mid = strings.ToLower(strings.TrimSpace(mid))
	m.mu.Lock()
	defer m.mu.Unlock()
	if mid == "" {
		clear(m.preflightCache)
	} else {
		delete(m.preflightCache, mid)
	}
	for _, flight := range m.preflights {
		if mid == "" || flight.mid == mid {
			flight.invalidated = true
		}
	}
}

// Preflight merges concurrent checks for one account generation. The initiating
// caller owns the remote call and its deadline; a surviving waiter may retry a
// canceled flight within its own deadline. Errors and unusable accounts are not
// cached. Manual synchronization always uses the existing fresh network path.
func (m *Maintenance) Preflight(ctx context.Context, mid string) (Map, error) {
	mid = strings.ToLower(strings.TrimSpace(mid))
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return nil, context.Canceled
	}
	m.workers.Add(1)
	m.mu.Unlock()
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel(); m.workers.Done() }()
	ctx = callCtx
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, _, err := m.Refresh(ctx, mid, false)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, ErrCredentialsChanged
		}
		signature, routing, err := m.preflightSignature(ctx, current)
		if err != nil {
			return nil, err
		}
		key := flightKey(current) + ":" + routing
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return nil, context.Canceled
		}
		now := m.accounts.now()
		for id, evidence := range m.preflightCache {
			if !now.Before(evidence.expires) {
				delete(m.preflightCache, id)
			}
		}
		if evidence, ok := m.preflightCache[mid]; ok && evidence.signature == signature && imageAvailable(current, now) {
			m.mu.Unlock()
			return current, nil
		}
		if flight := m.preflights[key]; flight != nil {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight.done:
			}
			if flight.err != nil {
				if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
					continue
				}
				return nil, flight.err
			}
			if flight.invalidated {
				continue
			}
			// Re-read canonical data, including changes made while waiting.
			latest, err := m.accounts.Get(ctx, mid)
			if err != nil {
				return nil, err
			}
			if latest == nil {
				return nil, ErrCredentialsChanged
			}
			_, latestRouting, err := m.preflightSignature(ctx, latest)
			if err != nil {
				return nil, err
			}
			if flightKey(latest) != flightKey(current) || latestRouting != routing {
				continue
			}
			return latest, nil
		}
		if len(m.preflights) >= 128 {
			changed := m.preflightChanged
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		flight := &preflightFlight{done: make(chan struct{}), mid: mid}
		m.preflights[key] = flight
		m.mu.Unlock()
		result, err := m.synchronize(ctx, mid, 1)
		var evidence preflightEvidence
		if err == nil && result != nil && imageAvailable(result, m.accounts.now()) && result["last_remote_check_result"] == "ok" && (boolean(result["image_quota_unknown"], false) || integer(result["quota"], 0) > 0) {
			latest, lookupErr := m.accounts.Get(ctx, mid)
			if lookupErr == nil && latest != nil && canonical(stripRuntime(latest)) == canonical(stripRuntime(result)) {
				postSignature, postRouting, lookupErr := m.preflightSignature(ctx, latest)
				if lookupErr == nil && routing == postRouting {
					evidence = preflightEvidence{signature: postSignature, expires: m.accounts.now().Add(preflightTTL)}
				}
			}
		}
		m.mu.Lock()
		if evidence.signature != "" && !flight.invalidated && !m.closing {
			if len(m.preflightCache) >= preflightCacheCapacity {
				clear(m.preflightCache)
			}
			m.preflightCache[mid] = evidence
		}
		flight.err = err
		delete(m.preflights, key)
		close(m.preflightChanged)
		m.preflightChanged = make(chan struct{})
		close(flight.done)
		m.mu.Unlock()
		return result, err
	}
}
