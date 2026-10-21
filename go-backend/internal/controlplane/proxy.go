package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Proxy struct {
	clearance *ProxyClearance
	db        *repository.Database
	mu        sync.Mutex
	now       func() time.Time
	egressMu  sync.Mutex
	inflight  map[string]int
	changed   chan struct{}
}

func NewProxy(db *repository.Database, now func() time.Time) *Proxy {
	if now == nil {
		now = time.Now
	}
	return &Proxy{db: db, now: now, inflight: map[string]int{}, changed: make(chan struct{})}
}
func groupID(v any) string {
	s := text(v)
	if strings.HasPrefix(strings.ToLower(s), "group:") {
		return strings.TrimSpace(strings.SplitN(s, ":", 2)[1])
	}
	return s
}
func proxySlug(v any) string {
	r := []rune(slug(v))
	if len(r) > 64 {
		r = r[:64]
	}
	return string(r)
}
func NormalizeProxy(raw string) (string, error) {
	bad := func(s string) (string, error) { return "", problem(400, s) }
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bad("proxy node url is required")
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return bad("proxy node url cannot contain whitespace")
	}
	if !strings.Contains(raw, "://") {
		return bad("proxy node url must start with http://, https://, socks5://, or socks5h://")
	}
	u, e := url.Parse(raw)
	if e != nil {
		return bad("proxy node url has an invalid host or port")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return bad("proxy node url uses an unsupported scheme")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return bad("proxy node url requires a host")
	}
	port := 0
	if u.Port() != "" {
		n, e := strconv.Atoi(u.Port())
		if e != nil || n < 1 || n > 65535 {
			return bad("proxy node url has an invalid port")
		}
		port = n
	}
	if strings.HasSuffix(u.Host, ":") {
		return bad("proxy node url has an invalid port")
	}
	if u.Path != "" && u.Path != "/" {
		return bad("proxy node url cannot include a path")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return bad("proxy node url cannot include parameters, a query, or a fragment")
	}
	if !strings.Contains(host, ":") {
		host, e = idna.Lookup.ToASCII(host)
		if e != nil {
			return bad("proxy node url has an invalid internationalized host")
		}
	} else {
		host = "[" + host + "]"
	}
	if u.Scheme == "socks5" {
		u.Scheme = "socks5h"
	}
	if (u.Scheme == "http" && port == 80) || (u.Scheme == "https" && port == 443) {
		port = 0
	}
	authority := strings.SplitN(strings.SplitN(raw, "://", 2)[1], "/", 2)[0]
	auth := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		auth = authority[:at+1]
	}
	portText := ""
	if port > 0 {
		portText = ":" + strconv.Itoa(port)
	}
	return u.Scheme + "://" + auth + host + portText, nil
}
func normalizeRuntimeProxy(v string) string {
	s := strings.TrimSpace(v)
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "socks5://") {
		return "socks5h://" + s[len("socks5://"):]
	} else if strings.HasPrefix(lower, "socks://") {
		return "socks5h://" + s[len("socks://"):]
	} else if s != "" && !strings.Contains(s, "://") {
		parts := strings.SplitN(s, ":", 4)
		if len(parts) == 2 {
			if _, e := strconv.Atoi(parts[1]); e == nil {
				return "http://" + s
			}
		}
		if len(parts) == 4 {
			if _, e := strconv.Atoi(parts[1]); e == nil {
				return "http://" + url.UserPassword(parts[2], parts[3]).String() + "@" + parts[0] + ":" + parts[1]
			}
		}
	}
	return s
}
func nodeLimit(node, fallback Map) int {
	for _, m := range []Map{node, fallback} {
		for _, key := range []string{"image_concurrency_limit", "image_concurrency", "max_image_concurrency"} {
			if v, ok := m[key]; ok && v != nil && v != "" {
				f, ok := number(v)
				if !ok {
					return 30
				}
				return max(0, min(10000, int(f)))
			}
		}
	}
	return 30
}
func nodeID(m Map, index int) string {
	if s := text(m["id"]); s != "" {
		return s
	}
	if s := text(m["name"]); s != "" {
		return s
	}
	return fmt.Sprintf("node-%d", index+1)
}
func proxyHealth() Map {
	return Map{"state": "unknown", "checked_at": nil, "latency_ms": nil, "error": nil}
}
func rotation(v any) float64 { f, _ := number(v); return max(0, min(1440, f)) }
func proxyRevision(raw Map) string {
	return digest(canonical(Map{"proxy": raw["proxy"], "fallback_proxy": raw["fallback_proxy"], "proxy_profiles": raw["proxy_profiles"], "proxy_groups": raw["proxy_groups"]}))[:16]
}
func proxyReference(v any, emptyDirect bool) Map {
	raw := text(v)
	lower := strings.ToLower(raw)
	if raw == "" || lower == "global" {
		if !emptyDirect {
			return nil
		}
		lower = "direct"
	}
	if lower == "direct" {
		return Map{"mode": "direct", "group_id": "", "url": ""}
	}
	if strings.HasPrefix(lower, "group:") {
		gid := groupID(raw)
		if gid != "" {
			return Map{"mode": "group", "group_id": gid, "url": ""}
		}
		if !emptyDirect {
			return nil
		}
		return Map{"mode": "direct", "group_id": "", "url": ""}
	}
	return Map{"mode": "custom", "group_id": "", "url": raw}
}
func assignment(v, legacy any, names map[string]string) Map {
	raw := text(v)
	if strings.EqualFold(raw, "global") {
		raw = ""
	}
	if raw == "" && groupID(legacy) != "" {
		raw = "group:" + groupID(legacy)
	}
	mode, label, gid := "inherit", "使用默认出口", ""
	lower := strings.ToLower(raw)
	switch {
	case raw == "":
	case lower == "direct":
		mode, label = "direct", "强制直连"
	case strings.HasPrefix(lower, "group:"):
		mode, gid = "group", groupID(raw)
		name := names[gid]
		if name == "" {
			name = gid
		}
		if name == "" {
			name = "-"
		}
		label = "代理组：" + name
	case strings.HasPrefix(lower, "profile:"):
		mode = "profile"
		name := strings.TrimSpace(strings.SplitN(raw, ":", 2)[1])
		if name == "" {
			name = "-"
		}
		label = "历史代理：" + name
	default:
		mode, label = "custom", "自定义代理"
		candidate := raw
		if !strings.Contains(candidate, "://") {
			candidate = "http://" + candidate
		}
		if u, e := url.Parse(candidate); e == nil && u.Hostname() != "" {
			host := u.Hostname()
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			port := ""
			if u.Port() != "" {
				port = ":" + u.Port()
			}
			prefix := ""
			if strings.Contains(raw, "://") {
				prefix = u.Scheme + "://"
			}
			label = prefix + host + port
		}
	}
	return Map{"reference": raw, "mode": mode, "group_id": gid, "label": label}
}
func (p *Proxy) references(ctx context.Context, raw Map) (map[string][]string, error) {
	refs := map[string][]string{}
	add := func(gid, label string) {
		if gid == "" {
			return
		}
		for _, s := range refs[gid] {
			if s == label {
				return
			}
		}
		refs[gid] = append(refs[gid], label)
	}
	for _, v := range []struct{ k, label string }{{"proxy", "默认出口"}, {"fallback_proxy", "备用出口"}} {
		s := text(raw[v.k])
		if strings.HasPrefix(strings.ToLower(s), "group:") {
			add(groupID(s), v.label)
		}
	}
	groups, e := p.db.Document(ctx, "account_group_configuration")
	if e != nil {
		return nil, e
	}
	ref := func(m Map) string {
		s := text(m["proxy"])
		if s != "" {
			if strings.HasPrefix(strings.ToLower(s), "group:") {
				return groupID(s)
			}
			return ""
		}
		return groupID(m["proxy_group_id"])
	}
	for _, m := range dicts(groups["items"]) {
		name := text(m["id"])
		if name == "" {
			name = text(m["name"])
		}
		if name == "" {
			name = "未知"
		}
		add(ref(m), "账号组 "+name)
	}
	snap, e := p.db.Snapshot(ctx, "accounts")
	if e != nil {
		return nil, e
	}
	for _, m := range snap.Items {
		name := text(m["id"])
		if name == "" {
			name = text(m["email"])
		}
		if name == "" {
			name = "未知"
		}
		add(ref(m), "账号 "+name)
	}
	return refs, nil
}
func (p *Proxy) groups(ctx context.Context, raw Map) ([]Map, error) {
	refs, e := p.references(ctx, raw)
	if e != nil {
		return nil, e
	}
	out := []Map{}
	for _, g := range dicts(raw["proxy_groups"]) {
		gid := text(g["id"])
		if gid == "" {
			continue
		}
		strategy := text(g["strategy"])
		if strategy != "request_random" && strategy != "round_robin" && strategy != "time_window" {
			strategy = "request_random"
		}
		nodes := []Map{}
		for i, n := range dicts(g["nodes"]) {
			nid := nodeID(n, i)
			name := text(n["name"])
			if name == "" {
				name = nid
			}
			nodes = append(nodes, Map{"id": nid, "name": name, "url": text(n["url"]), "enabled": n["enabled"] != false, "image_concurrency_limit": nodeLimit(n, nil), "notes": text(n["notes"]), "health": proxyHealth()})
		}
		name := text(g["name"])
		if name == "" {
			name = gid
		}
		labels := refs[gid]
		if labels == nil {
			labels = []string{}
		}
		out = append(out, Map{"id": gid, "name": name, "strategy": strategy, "rotation_interval_minutes": rotation(g["rotation_interval_minutes"]), "enabled": g["enabled"] != false, "notes": text(g["notes"]), "nodes": nodes, "reference_text": "group:" + gid, "health": proxyHealth(), "can_delete": len(labels) == 0, "references": labels})
	}
	fold := cases.Fold()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := fold.String(text(out[i]["name"])), fold.String(text(out[j]["name"]))
		if a == b {
			return fold.String(text(out[i]["id"])) < fold.String(text(out[j]["id"]))
		}
		return a < b
	})
	return out, nil
}
func effectiveReference(ref, raw Map, groups []Map) Map {
	out := Map{"source": "disabled", "label": "未启用", "configured": false, "available": true, "has_proxy": false, "group_id": ""}
	if ref == nil {
		return out
	}
	out["configured"] = true
	switch ref["mode"] {
	case "direct":
		out["source"], out["label"] = "direct", "直连"
	case "group":
		gid := text(ref["group_id"])
		out["source"], out["group_id"], out["label"], out["available"], out["has_proxy"] = "group", gid, "代理组 "+gid+"（不存在）", false, false
		for _, g := range groups {
			if g["id"] == gid {
				out["label"] = g["name"]
				available := false
				for _, n := range mapSlice(g["nodes"]) {
					if boolean(g["enabled"], true) && boolean(n["enabled"], true) && text(n["url"]) != "" {
						available = true
					}
				}
				out["available"], out["has_proxy"] = available, available
				break
			}
		}
	default:
		rawURL := text(ref["url"])
		if strings.HasPrefix(strings.ToLower(rawURL), "profile:") {
			profileID := strings.TrimSpace(strings.SplitN(rawURL, ":", 2)[1])
			out["source"], out["label"], out["available"], out["has_proxy"] = "profile", "历史代理配置 "+profileID+"（不存在）", false, false
			for _, m := range dicts(raw["proxy_profiles"]) {
				if m["id"] == profileID {
					name := text(m["name"])
					if name == "" {
						name = profileID
					}
					available := m["enabled"] != false && text(m["proxy"]) != ""
					out["label"], out["available"], out["has_proxy"] = name, available, available
				}
			}
		} else {
			out["source"], out["label"], out["available"], out["has_proxy"] = "custom", "自定义代理", rawURL != "", rawURL != ""
		}
	}
	return out
}
func mapSlice(v any) []Map {
	if x, ok := v.([]Map); ok {
		return x
	}
	return dicts(v)
}
func (p *Proxy) view(ctx context.Context, raw Map) (Map, error) {
	groups, e := p.groups(ctx, raw)
	if e != nil {
		return nil, e
	}
	def, fall := proxyReference(raw["proxy"], true), proxyReference(raw["fallback_proxy"], false)
	return Map{"schema_version": 1, "generated_at": generated(p.now()), "revision": proxyRevision(raw), "default_reference": def, "fallback_reference": fall, "effective_default": effectiveReference(def, raw, groups), "effective_fallback": effectiveReference(fall, raw, groups), "groups": groups}, nil
}
func (p *Proxy) View(ctx context.Context) (Map, error) {
	raw, e := p.db.Document(ctx, "proxy_configuration")
	if e != nil {
		return nil, e
	}
	return p.view(ctx, raw)
}
func normalizeAssignment(raw string, legacy any, snapshot Map) (string, error) {
	if strings.EqualFold(raw, "global") {
		return "", nil
	}
	if raw == "" && text(legacy) != "" {
		raw = "group:" + text(legacy)
	}
	if raw == "" {
		return "", nil
	}
	if strings.EqualFold(raw, "direct") {
		return "direct", nil
	}
	if strings.HasPrefix(strings.ToLower(raw), "group:") {
		gid := groupID(raw)
		if gid == "" {
			return "", problem(400, "proxy group id is required")
		}
		for _, g := range dicts(snapshot["proxy_groups"]) {
			if g["id"] == gid {
				if g["enabled"] != false {
					for _, n := range dicts(g["nodes"]) {
						if n["enabled"] != false && text(n["url"]) != "" {
							return "group:" + gid, nil
						}
					}
				}
				return "", problem(400, "proxy group is unavailable")
			}
		}
		return "", problem(400, "proxy group not found")
	}
	return NormalizeProxy(raw)
}
func serializeReference(ref, raw Map) (string, error) {
	switch ref["mode"] {
	case "direct":
		return "direct", nil
	case "group":
		gid := groupID(ref["group_id"])
		if gid == "" {
			return "", problem(400, "proxy group id is required")
		}
		for _, g := range dicts(raw["proxy_groups"]) {
			if text(g["id"]) == gid {
				return "group:" + gid, nil
			}
		}
		return "", problem(400, "proxy group not found")
	default:
		s := text(ref["url"])
		if strings.HasPrefix(strings.ToLower(s), "profile:") {
			pid := strings.TrimSpace(strings.SplitN(s, ":", 2)[1])
			if pid != "" {
				if text(raw["proxy"]) == s || text(raw["fallback_proxy"]) == s {
					return "profile:" + pid, nil
				}
				for _, m := range dicts(raw["proxy_profiles"]) {
					if text(m["id"]) == pid {
						return "profile:" + pid, nil
					}
				}
			}
			return "", problem(400, "legacy proxy profile not found")
		}
		if s == "" {
			return "", problem(400, "proxy url is required")
		}
		return NormalizeProxy(s)
	}
}
func (p *Proxy) Defaults(ctx context.Context, body Map) (Map, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next, e := p.db.UpdateDocument(ctx, "proxy_configuration", func(raw Map) (Map, error) {
		def, e := serializeReference(object(body["default_reference"]), raw)
		if e != nil {
			return nil, e
		}
		fall := ""
		if body["fallback_reference"] != nil {
			fall, e = serializeReference(object(body["fallback_reference"]), raw)
			if e != nil {
				return nil, e
			}
		}
		raw["proxy"], raw["fallback_proxy"] = def, fall
		return raw, nil
	})
	if e != nil {
		return nil, e
	}
	view, e := p.view(ctx, next)
	if e != nil {
		return nil, e
	}
	delete(view, "schema_version")
	delete(view, "generated_at")
	delete(view, "groups")
	return view, nil
}
func (p *Proxy) SaveGroup(ctx context.Context, body Map) (Map, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	gid := ""
	next, e := p.db.UpdateDocument(ctx, "proxy_configuration", func(raw Map) (Map, error) {
		groups := dicts(raw["proxy_groups"])
		requested := groupID(body["id"])
		base := Map{}
		exists := false
		for _, g := range groups {
			if text(g["id"]) == requested {
				base = copyMap(g)
				exists = true
				break
			}
		}
		if boolean(body["create_only"], false) && exists {
			return nil, problem(400, "proxy group already exists")
		}
		gid = text(base["id"])
		if gid == "" {
			candidate := requested
			if candidate == "" {
				candidate = text(body["name"])
			}
			gid = proxySlug(candidate)
		}
		if gid == "" {
			return nil, problem(400, "proxy group id is required")
		}
		if !exists {
			for _, g := range groups {
				if text(g["id"]) == gid {
					return nil, problem(400, "proxy group already exists")
				}
			}
		}
		for _, k := range []string{"name", "strategy", "rotation_interval_minutes", "enabled", "notes"} {
			if v, ok := body[k]; ok && v != nil {
				base[k] = v
			}
		}
		base["id"] = gid
		if text(base["name"]) == "" {
			base["name"] = gid
		}
		base["name"] = text(base["name"])
		strategy := text(base["strategy"])
		if strategy == "" {
			strategy = "request_random"
		}
		if strategy != "request_random" && strategy != "round_robin" && strategy != "time_window" {
			return nil, problem(400, "unsupported proxy group strategy")
		}
		base["strategy"] = strategy
		base["rotation_interval_minutes"] = json.Number(canonical(rotation(base["rotation_interval_minutes"])))
		base["enabled"] = truth(defaultValue(base["enabled"], true))
		base["notes"] = text(base["notes"])
		if v, ok := body["nodes"]; ok && v != nil {
			prior := map[string]Map{}
			for i, n := range dicts(base["nodes"]) {
				prior[nodeID(n, i)] = n
			}
			nodes := []Map{}
			seenID, seenURL := map[string]bool{}, map[string]bool{}
			for i, n := range dicts(v) {
				requested := text(n["id"])
				existing, ok := prior[requested]
				nid := requested
				if !ok {
					candidate := requested
					if candidate == "" {
						candidate = text(n["name"])
					}
					if candidate == "" {
						candidate = fmt.Sprintf("node-%d", i+1)
					}
					nid = proxySlug(candidate)
				}
				if nid == "" {
					nid = fmt.Sprintf("node-%d", i+1)
				}
				if seenID[nid] {
					return nil, problem(400, "duplicate proxy node id: "+nid)
				}
				if text(n["url"]) == "" {
					return nil, problem(400, "proxy node url is required: "+nid)
				}
				u, e := NormalizeProxy(text(n["url"]))
				if e != nil {
					return nil, problem(400, e.Error()+": "+nid)
				}
				if seenURL[u] {
					return nil, problem(400, "duplicate proxy node url: "+nid)
				}
				seenID[nid], seenURL[u] = true, true
				node := copyMap(existing)
				node["id"], node["url"], node["name"], node["enabled"], node["image_concurrency_limit"], node["notes"] = nid, u, text(n["name"]), n["enabled"] != false, nodeLimit(n, existing), text(n["notes"])
				if node["name"] == "" {
					node["name"] = nid
				}
				nodes = append(nodes, node)
			}
			base["nodes"] = nodes
		}
		if len(mapSlice(base["nodes"])) == 0 {
			return nil, problem(400, "proxy group requires at least one proxy node")
		}
		out := []Map{}
		for _, g := range groups {
			if text(g["id"]) != gid {
				out = append(out, g)
			}
		}
		out = append(out, base)
		raw["proxy_groups"] = out
		return raw, nil
	})
	if e != nil {
		return nil, e
	}
	groups, e := p.groups(ctx, next)
	if e != nil {
		return nil, e
	}
	for _, g := range groups {
		if g["id"] == gid {
			return Map{"group": g, "revision": proxyRevision(next)}, nil
		}
	}
	return nil, problem(500, "proxy group projection failed")
}
func (p *Proxy) DeleteGroup(ctx context.Context, gid string) (Map, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	gid = groupID(gid)
	if gid == "" {
		return nil, problem(400, "proxy group id is required")
	}
	// Cross-domain references have their own repositories. Read them outside
	// the proxy transaction, under the same local assignment lock.
	externalRefs, e := p.references(ctx, nil)
	if e != nil {
		return nil, e
	}
	next, e := p.db.UpdateDocument(ctx, "proxy_configuration", func(raw Map) (Map, error) {
		found := false
		out := []Map{}
		for _, g := range dicts(raw["proxy_groups"]) {
			if text(g["id"]) == gid {
				found = true
			} else {
				out = append(out, g)
			}
		}
		if !found {
			return nil, problem(404, "proxy group not found")
		}
		refs := []string{}
		for _, v := range []struct{ k, label string }{{"proxy", "默认出口"}, {"fallback_proxy", "备用出口"}} {
			ref := text(raw[v.k])
			if strings.HasPrefix(strings.ToLower(ref), "group:") && groupID(ref) == gid {
				refs = append(refs, v.label)
			}
		}
		refs = append(refs, externalRefs[gid]...)
		if len(refs) > 0 {
			return nil, problem(409, "proxy group is in use: "+strings.Join(refs, ", "))
		}
		raw["proxy_groups"] = out
		return raw, nil
	})
	if e != nil {
		return nil, e
	}
	return Map{"deleted_id": gid, "revision": proxyRevision(next)}, nil
}

var importReasons = map[string]string{"proxy node url is required": "地址为空", "proxy node url cannot contain whitespace": "地址不能包含空白字符", "proxy node url must start with http://, https://, socks5://, or socks5h://": "请填写以 http://、https://、socks5:// 或 socks5h:// 开头的地址", "proxy node url has an invalid host or port": "代理主机或端口格式无效", "proxy node url uses an unsupported scheme": "仅支持 HTTP、HTTPS 和 SOCKS5 代理", "proxy node url requires a host": "缺少代理主机", "proxy node url has an invalid port": "代理端口格式无效", "proxy node url cannot include a path": "代理地址不能包含路径", "proxy node url cannot include parameters, a query, or a fragment": "代理地址不能包含参数、查询或片段", "proxy node url has an invalid internationalized host": "国际化域名格式无效"}

func ImportProxyNodes(body Map) (Map, error) {
	existing := map[string]bool{}
	for _, s := range stringsOf(body["existing_urls"]) {
		if u, e := NormalizeProxy(s); e == nil {
			existing[u] = true
		}
	}
	rawText, _ := body["text"].(string)
	lines := strings.Split(strings.ReplaceAll(rawText, "\r\n", "\n"), "\n")
	nonempty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonempty++
		}
	}
	if nonempty > 10000 {
		return nil, problem(400, "一次最多导入 10000 个代理节点")
	}
	nodes, bad := []Map{}, []Map{}
	duplicates := 0
	for i, line := range lines {
		raw := strings.TrimSpace(line)
		if raw == "" {
			continue
		}
		parts := strings.Fields(raw)
		reason := ""
		limit := 30
		if len(parts) > 2 {
			reason = "格式应为：代理地址 [图片并发]"
		} else if len(parts) == 2 {
			n, e := strconv.Atoi(parts[1])
			if e != nil || n < 0 || n > 10000 || strings.HasPrefix(parts[1], "+") {
				reason = "图片并发必须是 0 到 10000 的整数"
			} else {
				limit = n
			}
		}
		u := ""
		if reason == "" {
			var e error
			u, e = NormalizeProxy(parts[0])
			if e != nil {
				reason = importReasons[e.Error()]
				if reason == "" {
					reason = e.Error()
				}
			}
		}
		if reason != "" {
			bad = append(bad, Map{"line": i + 1, "raw": raw, "reason": reason})
			continue
		}
		if existing[u] {
			duplicates++
			continue
		}
		existing[u] = true
		nodes = append(nodes, Map{"url": u, "image_concurrency_limit": limit})
	}
	return Map{"nodes": nodes, "added_count": len(nodes), "duplicate_count": duplicates, "invalid_count": len(bad), "invalid_items": bad}, nil
}
