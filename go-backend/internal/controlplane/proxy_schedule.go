package controlplane

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

type ProxyProfile struct {
	ProxyURL                                string `json:"-"`
	Source                                  string
	EgressLabel                             string
	GroupID, NodeID, NodeName               string
	ImageConcurrencyLimit                   int
	WaitMS                                  int
	RuntimeEnabled, Resource, SkipSSLVerify bool
	Clearance                               Map `json:"-"`
	egressKey                               string
}
type EgressLease struct {
	Profile ProxyProfile
	once    sync.Once
	release func()
}

func (l *EgressLease) Release() {
	if l != nil {
		l.once.Do(func() {
			if l.release != nil {
				l.release()
			}
		})
	}
}
func (p *Proxy) WithAssignments(ctx context.Context, refs [][2]string, fn func([]string) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, e := p.db.Document(ctx, "proxy_configuration")
	if e != nil {
		return e
	}
	normalized := make([]string, len(refs))
	for i, ref := range refs {
		normalized[i], e = normalizeAssignment(strings.TrimSpace(ref[0]), ref[1], raw)
		if e != nil {
			return e
		}
	}
	return fn(normalized)
}
func egressKey(gid string, node Map, index int) string {
	return "group:" + gid + ":" + nodeID(node, index)
}
func (p *Proxy) groupSelection(ctx context.Context, raw Map, gid, source string, reserve bool) (*EgressLease, error) {
	var nodes []Map
	for _, g := range dicts(raw["proxy_groups"]) {
		if g["id"] == gid && g["enabled"] != false {
			for _, n := range dicts(g["nodes"]) {
				if truth(defaultValue(n["enabled"], true)) && text(n["url"]) != "" {
					nodes = append(nodes, n)
				}
			}
			break
		}
	}
	if len(nodes) == 0 {
		return nil, problem(503, "proxy group is unavailable: "+gid)
	}
	start := time.Now()
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		p.egressMu.Lock()
		available := []int{}
		for i, n := range nodes {
			limit := nodeLimit(n, nil)
			if limit <= 0 || p.inflight[egressKey(gid, n, i)] < limit {
				available = append(available, i)
			}
		}
		selected := -1
		if len(available) > 0 {
			selected = available[rand.IntN(len(available))]
		} else if !reserve {
			bestRatio := 1e30
			bestCurrent := int(^uint(0) >> 1)
			for i, n := range nodes {
				current := p.inflight[egressKey(gid, n, i)]
				limit := nodeLimit(n, nil)
				ratio := 0.0
				if limit > 0 {
					ratio = float64(current) / float64(limit)
				}
				if ratio < bestRatio || (ratio == bestRatio && current < bestCurrent) {
					selected = i
					bestRatio, bestCurrent = ratio, current
				}
			}
		}
		if selected >= 0 {
			n := nodes[selected]
			key := egressKey(gid, n, selected)
			limit := nodeLimit(n, nil)
			profile := ProxyProfile{ProxyURL: normalizeRuntimeProxy(text(n["url"])), Source: source + "_group", GroupID: gid, NodeID: nodeID(n, selected), NodeName: text(n["name"]), ImageConcurrencyLimit: limit, WaitMS: int(time.Since(start).Milliseconds()), egressKey: key}
			if profile.NodeName == "" {
				profile.NodeName = profile.NodeID
			}
			profile.EgressLabel = gid + "/" + profile.NodeName
			lease := &EgressLease{Profile: profile}
			if reserve && limit > 0 {
				p.inflight[key]++
				lease.release = func() {
					p.egressMu.Lock()
					defer p.egressMu.Unlock()
					if p.inflight[key] <= 1 {
						delete(p.inflight, key)
					} else {
						p.inflight[key]--
					}
					close(p.changed)
					p.changed = make(chan struct{})
				}
			}
			p.egressMu.Unlock()
			return lease, nil
		}
		changed := p.changed
		p.egressMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Resolve follows the baseline's actual precedence and capacity-aware random
// group selection. Stored strategy/rotation fields are retained, but Python's
// runtime does not currently execute round-robin or time-window strategies.
func (p *Proxy) Resolve(ctx context.Context, account Map, explicit string, resource, upstream, reserve bool) (*EgressLease, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	raw, e := p.db.Document(ctx, "proxy_configuration")
	if e != nil {
		return nil, e
	}
	settings, e := p.db.Document(ctx, "system_settings")
	if e != nil {
		return nil, e
	}
	runtime := normalizeRoot("proxy_runtime", object(settings["proxy_runtime"]))
	groups, e := p.db.Document(ctx, "account_group_configuration")
	if e != nil {
		return nil, e
	}
	type candidate struct {
		value, source string
		terminal      bool
	}
	candidates := []candidate{{text(account["proxy"]), "account", true}}
	gid := text(account["group_id"])
	if gid != "" {
		for _, g := range dicts(groups["items"]) {
			if g["id"] == gid && g["enabled"] != false {
				v := text(g["proxy"])
				if v == "" && text(g["proxy_group_id"]) != "" {
					v = "group:" + text(g["proxy_group_id"])
				}
				candidates = append(candidates, candidate{v, "account_group", false})
				break
			}
		}
	}
	candidates = append(candidates, candidate{strings.TrimSpace(explicit), "explicit", true})
	if resource && upstream && boolean(runtime["enabled"], false) {
		candidates = append(candidates, candidate{text(runtime["resource_proxy_url"]), "resource", true})
	}
	candidates = append(candidates, candidate{text(raw["proxy"]), "default", false})
	profile := ProxyProfile{Source: "direct", EgressLabel: "direct", egressKey: "direct"}
	var lease *EgressLease
	for _, c := range candidates {
		if c.value == "" {
			continue
		}
		lower := strings.ToLower(c.value)
		profile.Source = c.source
		profile.EgressLabel = "direct"
		if lower == "global" {
			continue
		}
		if lower == "direct" {
			profile.Source = c.source + "_direct"
			break
		}
		if strings.HasPrefix(lower, "group:") {
			lease, e = p.groupSelection(ctx, raw, groupID(c.value), c.source, reserve)
			if e != nil {
				return nil, e
			}
			profile = lease.Profile
			break
		}
		if strings.HasPrefix(lower, "profile:") {
			profile.Source = c.source + "_profile"
			profile.EgressLabel = profile.Source
			pid := strings.TrimSpace(strings.SplitN(c.value, ":", 2)[1])
			for _, legacy := range dicts(raw["proxy_profiles"]) {
				if text(legacy["id"]) == pid && truth(defaultValue(legacy["enabled"], true)) {
					profile.ProxyURL = normalizeRuntimeProxy(text(legacy["proxy"]))
					break
				}
			}
			if profile.ProxyURL != "" || c.terminal {
				break
			}
			continue
		}
		profile.ProxyURL = normalizeRuntimeProxy(c.value)
		profile.EgressLabel = c.source
		break
	}
	if lease == nil {
		lease = &EgressLease{}
		if profile.ProxyURL != "" {
			profile.egressKey = "proxy:" + profile.ProxyURL
		}
	}
	profile.Resource = resource
	profile.RuntimeEnabled = boolean(runtime["enabled"], false)
	profile.SkipSSLVerify = boolean(runtime["skip_ssl_verify"], false)
	profile.Clearance = copyMap(object(runtime["clearance"]))
	if p.clearance != nil {
		p.clearance.prepareProfile(&profile)
	}
	lease.Profile = profile
	return lease, nil
}
func (p *Proxy) Fallback(ctx context.Context, resource, reserve bool) (*EgressLease, error) {
	raw, e := p.db.Document(ctx, "proxy_configuration")
	if e != nil {
		return nil, e
	}
	ref := text(raw["fallback_proxy"])
	if ref == "" || strings.EqualFold(ref, "global") {
		return nil, nil
	}
	lease, e := p.Resolve(ctx, nil, ref, resource, true, reserve)
	if e != nil {
		return nil, e
	}
	profile := &lease.Profile
	if strings.HasPrefix(profile.Source, "explicit") {
		profile.Source = "fallback" + strings.TrimPrefix(profile.Source, "explicit")
	} else if !strings.HasPrefix(profile.Source, "fallback") {
		profile.Source = "fallback_" + profile.Source
	}
	if profile.EgressLabel == "" || strings.HasPrefix(profile.EgressLabel, "explicit") {
		if profile.ProxyURL != "" {
			profile.EgressLabel = "fallback"
		} else {
			profile.EgressLabel = profile.Source
		}
	}
	return lease, nil
}
func (p *Proxy) Inflight() map[string]int {
	p.egressMu.Lock()
	defer p.egressMu.Unlock()
	out := map[string]int{}
	for k, n := range p.inflight {
		out[k] = n
	}
	return out
}
