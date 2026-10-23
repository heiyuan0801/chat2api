package upstream

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

// TransportPool owns bounded, process-local browser connections and cookies.
// Each borrow gets a separate Client: operation admission and request callbacks
// must never be shared, even when requests use the same account concurrently.
type TransportPool struct {
	mu      sync.Mutex
	entries map[[32]byte]*transportEntry
	closed  bool
}

type transportEntry struct {
	http                HTTPClient
	deviceID, sessionID string
	used                time.Time
	refs                int
	retired             bool
}

const transportPoolCapacity = 128
const transportPoolTTL = 2 * time.Minute

func NewTransportPool() *TransportPool {
	return &TransportPool{entries: make(map[[32]byte]*transportEntry)}
}

// New isolates connections by credentials, egress, browser identity, clearance
// and TLS policy. Unauthenticated flows retain independent cookie jars.
func (p *TransportPool) New(raw Config) (*Client, error) {
	cfg, err := normalizeConfig(raw)
	if err != nil {
		return nil, err
	}
	keyBytes, _ := json.Marshal([]any{cfg.AccessToken, cfg.ProxyURL, cfg.Profile,
		cfg.UserAgent, cfg.CookieHeader, cfg.SkipSSLVerify, cfg.Timeout,
		raw.DeviceID, raw.SessionID})
	key := sha256.Sum256(keyBytes)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, failure("client_closed", "config")
	}
	if cfg.AccessToken == "" {
		return New(cfg)
	}
	now := time.Now()
	for k, entry := range p.entries {
		if entry.refs == 0 && now.Sub(entry.used) >= transportPoolTTL {
			entry.http.CloseIdleConnections()
			delete(p.entries, k)
		}
	}
	entry := p.entries[key]
	if entry == nil {
		if len(p.entries) >= transportPoolCapacity {
			var oldest *transportEntry
			var oldestKey [32]byte
			for k, candidate := range p.entries {
				if candidate.refs == 0 && (oldest == nil || candidate.used.Before(oldest.used)) {
					oldest, oldestKey = candidate, k
				}
			}
			// Capacity bounds retained transports, never active request admission.
			if oldest == nil {
				return New(cfg)
			}
			oldest.http.CloseIdleConnections()
			delete(p.entries, oldestKey)
		}
		transport, err := newHTTPTransport(cfg)
		if err != nil {
			return nil, err
		}
		entry = &transportEntry{http: transport, deviceID: cfg.DeviceID, sessionID: cfg.SessionID}
		p.entries[key] = entry
	}
	entry.refs++
	entry.used = now
	cfg.DeviceID, cfg.SessionID = entry.deviceID, entry.sessionID
	lease := &pooledHTTPClient{HTTPClient: entry.http, release: func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		entry.refs--
		entry.used = time.Now()
		if entry.retired {
			entry.http.CloseIdleConnections()
		}
	}}
	return NewWithTransport(cfg, lease)
}

type pooledHTTPClient struct {
	HTTPClient
	once    sync.Once
	release func()
}

func (c *pooledHTTPClient) CloseIdleConnections() { c.once.Do(c.release) }

// Close prevents new borrows and closes idle sockets; active leases close their
// remaining idle connections when returned. Safe after a bounded server drain.
func (p *TransportPool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for key, entry := range p.entries {
		entry.retired = true
		entry.http.CloseIdleConnections()
		delete(p.entries, key)
	}
}
