package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type clearanceBundle struct {
	host, proxy, userAgent, cookies string
	expires                         time.Time
	signature                       string
}
type clearanceFlight struct{ done chan struct{} }
type ProxyClearance struct {
	server  *Server
	mu      sync.Mutex
	cache   map[string]clearanceBundle
	flights map[string]*clearanceFlight
}

func NewProxyClearance(s *Server) *ProxyClearance {
	owner := &ProxyClearance{server: s, cache: map[string]clearanceBundle{}, flights: map[string]*clearanceFlight{}}
	s.Proxy.clearance = owner
	return owner
}
func clearanceEnabled(profile ProxyProfile) bool {
	mode := strings.ToLower(text(profile.Clearance["mode"]))
	return profile.RuntimeEnabled && boolean(profile.Clearance["enabled"], false) && (mode == "manual" || mode == "flaresolverr")
}
func clearanceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, problem(400, "invalid clearance target URL")
	}
	return u, nil
}
func (c *ProxyClearance) Status(ctx context.Context) (Map, error) {
	lease, err := c.server.Proxy.Resolve(ctx, nil, "", false, true, false)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return c.status(lease.Profile), nil
}
func (c *ProxyClearance) status(profile ProxyProfile) Map {
	mode := strings.ToLower(text(profile.Clearance["mode"]))
	if mode == "" {
		mode = "manual"
	}
	egress := "direct"
	if profile.ProxyURL != "" {
		egress = "proxy"
	}
	hosts := []string{}
	c.mu.Lock()
	count := len(c.cache)
	for _, bundle := range c.cache {
		hosts = append(hosts, bundle.host)
	}
	c.mu.Unlock()
	hosts = unique(hosts)
	sort.Strings(hosts)
	return Map{"enabled": profile.RuntimeEnabled, "egress_mode": egress, "proxy_source": profile.Source, "egress_key": profile.egressKey, "egress_label": profile.EgressLabel, "image_concurrency_limit": profile.ImageConcurrencyLimit, "has_proxy": profile.ProxyURL != "", "skip_ssl_verify": profile.SkipSSLVerify, "clearance_enabled": clearanceEnabled(profile), "clearance_mode": mode, "has_clearance_bundle": count > 0, "cached_clearance_hosts": hosts}
}
func clearanceManual(profile ProxyProfile, host string) clearanceBundle {
	cookies := map[string]string{}
	order := []string{}
	for _, part := range strings.Split(text(profile.Clearance["cf_cookies"]), ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if ok && name != "" && !strings.ContainsAny(name+value, "\r\n") {
			if _, exists := cookies[name]; !exists {
				order = append(order, name)
			}
			cookies[name] = value
		}
	}
	if value := text(profile.Clearance["cf_clearance"]); value != "" && cookies["cf_clearance"] == "" {
		cookies["cf_clearance"] = value
		order = append(order, "cf_clearance")
	}
	parts := []string{}
	for _, name := range order {
		parts = append(parts, name+"="+cookies[name])
	}
	return clearanceBundle{host: host, proxy: profile.ProxyURL, cookies: strings.Join(parts, "; "), userAgent: text(profile.Clearance["user_agent"]), signature: clearanceSignature(profile.Clearance)}
}
func (c *ProxyClearance) remember(key string, bundle clearanceBundle, profile ProxyProfile) {
	if bundle.cookies == "" && bundle.userAgent == "" {
		return
	}
	interval := integer(profile.Clearance["refresh_interval"], 1800)
	if interval > 0 {
		bundle.expires = c.server.now().Add(time.Duration(interval) * time.Second)
	}
	bundle.signature = clearanceSignature(profile.Clearance)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= 256 {
		for k, value := range c.cache {
			if !value.expires.IsZero() && !value.expires.After(c.server.now()) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= 256 {
			for k := range c.cache {
				delete(c.cache, k)
				break
			}
		}
	}
	c.cache[key] = bundle
}
func (c *ProxyClearance) cached(profile ProxyProfile, host string) (clearanceBundle, bool) {
	key := profile.ProxyURL + "\x00" + host
	c.mu.Lock()
	bundle, ok := c.cache[key]
	c.mu.Unlock()
	if !ok || bundle.signature != clearanceSignature(profile.Clearance) || (!bundle.expires.IsZero() && !bundle.expires.After(c.server.now())) {
		return clearanceBundle{}, false
	}
	return bundle, true
}
func (c *ProxyClearance) Refresh(ctx context.Context, target string, profile ProxyProfile, force bool) (clearanceBundle, bool, error) {
	if !clearanceEnabled(profile) {
		return clearanceBundle{}, false, nil
	}
	u, err := clearanceURL(target)
	if err != nil {
		return clearanceBundle{}, false, err
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	key := profile.ProxyURL + "\x00" + host
	if text(profile.Clearance["mode"]) == "manual" {
		bundle := clearanceManual(profile, host)
		ok := bundle.cookies != "" || bundle.userAgent != ""
		if ok {
			c.remember(key, bundle, profile)
		}
		return bundle, ok, nil
	}
	if !force {
		if bundle, ok := c.cached(profile, host); ok {
			return bundle, true, nil
		}
	}
	c.mu.Lock()
	if pending := c.flights[key]; pending != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return clearanceBundle{}, false, ctx.Err()
		case <-pending.done:
			bundle, ok := c.cached(profile, host)
			return bundle, ok, nil
		}
	}
	flight := &clearanceFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.flights, key); close(flight.done); c.mu.Unlock() }()
	base := strings.TrimRight(text(profile.Clearance["flaresolverr_url"]), "/")
	endpoint, err := clearanceURL(base + "/v1")
	if err != nil {
		return clearanceBundle{}, false, nil
	}
	timeout := time.Duration(max(1, min(600, integer(profile.Clearance["timeout_sec"], 60)))) * time.Second
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload := Map{"cmd": "request.get", "url": target, "maxTimeout": int(timeout.Milliseconds())}
	if profile.ProxyURL != "" {
		payload["proxy"] = Map{"url": profile.ProxyURL}
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(callCtx, "POST", endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return clearanceBundle{}, false, nil
	}
	req.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	if c.server.ManagementHTTP != nil {
		client = c.server.ManagementHTTP
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := boundedClient.Do(req)
	if err != nil {
		bundle, ok := c.cached(profile, host)
		return bundle, ok, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		bundle, ok := c.cached(profile, host)
		return bundle, ok, nil
	}
	var data Map
	if json.Unmarshal(body, &data) != nil || strings.ToLower(text(data["status"])) != "ok" {
		bundle, ok := c.cached(profile, host)
		return bundle, ok, nil
	}
	solution := object(data["solution"])
	cookies := []string{}
	for _, cookie := range dicts(solution["cookies"]) {
		name, value, domain := text(cookie["name"]), text(cookie["value"]), strings.TrimPrefix(strings.ToLower(text(cookie["domain"])), ".")
		if name == "" || strings.ContainsAny(name+value, "\r\n;") {
			continue
		}
		if domain == "" || host == domain || strings.HasSuffix(host, "."+domain) {
			cookies = append(cookies, name+"="+value)
		}
	}
	bundle := clearanceBundle{host: host, proxy: profile.ProxyURL, cookies: strings.Join(cookies, "; "), userAgent: text(solution["userAgent"])}
	if bundle.cookies == "" && bundle.userAgent == "" {
		previous, ok := c.cached(profile, host)
		return previous, ok, nil
	}
	c.remember(key, bundle, profile)
	return bundle, true, nil
}
func (s *Server) testClearance(ctx context.Context, target string) (Map, error) {
	if strings.TrimSpace(target) == "" {
		target = "https://chatgpt.com"
	}
	started := time.Now()
	lease, err := s.Proxy.Resolve(ctx, nil, "", false, true, false)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	profile := lease.Profile
	status := s.Clearance.status(profile)
	result := Map{"ok": false, "status": "disabled", "latency_ms": 0, "has_cookies": false, "user_agent": "", "error": "clearance is disabled", "runtime": status}
	if !clearanceEnabled(profile) {
		return result, nil
	}
	bundle, ok, err := s.Clearance.Refresh(ctx, target, profile, true)
	result["latency_ms"] = time.Since(started).Milliseconds()
	result["runtime"] = s.Clearance.status(profile)
	if err != nil {
		result["status"], result["error"] = "error", diagnostic(err.Error(), nil, 2000)
		return result, nil
	}
	if !ok {
		result["status"], result["error"] = "failed", "clearance refresh returned no bundle"
		return result, nil
	}
	result["ok"], result["status"], result["has_cookies"], result["user_agent"], result["error"] = true, "ok", bundle.cookies != "", bundle.userAgent, nil
	return result, nil
}

// prepareProfile reads cache only. Explicit refresh owns remote solver requests.
func (c *ProxyClearance) prepareProfile(profile *ProxyProfile) {
	if !clearanceEnabled(*profile) {
		return
	}
	host := "chatgpt.com"
	var bundle clearanceBundle
	var ok bool
	if text(profile.Clearance["mode"]) == "manual" {
		bundle = clearanceManual(*profile, host)
		ok = bundle.cookies != "" || bundle.userAgent != ""
		if ok {
			c.remember(profile.ProxyURL+"\x00"+host, bundle, *profile)
		}
	} else {
		bundle, ok = c.cached(*profile, host)
	}
	if ok {
		profile.Clearance["_cookie_header"], profile.Clearance["_user_agent"] = bundle.cookies, bundle.userAgent
	}
}
func applyProxySession(profile ProxyProfile, cfg *upstream.Config) {
	cfg.SkipSSLVerify = profile.SkipSSLVerify
	if clearanceEnabled(profile) {
		cfg.CookieHeader = text(profile.Clearance["_cookie_header"])
		if cfg.UserAgent == "" && text(profile.Clearance["_user_agent"]) != "" {
			cfg.UserAgent = text(profile.Clearance["_user_agent"])
		}
	}
}

func clearanceSignature(raw Map) string {
	m := copyMap(raw)
	delete(m, "_cookie_header")
	delete(m, "_user_agent")
	return digest(canonical(m))
}
