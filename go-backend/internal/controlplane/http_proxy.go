package controlplane

import (
	"context"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/http"
	"strings"
	"time"
)

func nativeProxyProbe(ctx context.Context, url string) (Map, error) {
	started := time.Now()
	c, e := upstream.New(upstream.Config{ProxyURL: url, Timeout: 15 * time.Second})
	status := 0
	if e == nil {
		status, e = c.ProxyStatus(ctx)
		c.Close()
	}
	var message any
	if e != nil {
		message = "proxy connection failed"
	} else if status >= 500 {
		message = fmt.Sprintf("HTTP %d", status)
	}
	return Map{"ok": e == nil && status < 500, "status": status, "latency_ms": int(time.Since(started).Milliseconds()), "error": message, "proxy_source": "input", "has_proxy": url != ""}, nil
}
func proxyTestSummary(rows []Map) Map {
	succeeded, latency := 0, 0
	for _, row := range rows {
		result := object(row["result"])
		if boolean(result["ok"], false) {
			succeeded++
		}
		latency = max(latency, integer(result["latency_ms"], 0))
	}
	failed, total := len(rows)-succeeded, len(rows)
	status, tone, label, message := "failed", "danger", "代理组无可测试节点", "当前代理组没有可用节点"
	if total > 0 && failed == 0 {
		status, tone, label, message = "success", "success", "代理组可用", fmt.Sprintf("%d 个节点全部可用，最慢 %dms", total, latency)
	} else if succeeded > 0 {
		status, tone, label, message = "partial", "warning", "代理组部分可用", fmt.Sprintf("%d 个节点中 %d 个可用，%d 个失败", total, succeeded, failed)
	} else if total > 0 {
		label, message = "代理组不可用", fmt.Sprintf("%d 个节点全部失败", total)
	}
	var result any
	if len(rows) == 1 {
		result = rows[0]["result"]
	}
	return Map{"summary": Map{"status": status, "tone": tone, "total": total, "succeeded": succeeded, "failed": failed, "max_latency_ms": latency, "label": label, "message": message}, "results": rows, "result": result}
}
func (s *Server) registerProxy() {
	s.route("GET", "/api/proxy/view", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Proxy.View(ctx) })
	s.route("POST", "/api/proxy/defaults", "admin", "ProxyDefaultsRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Proxy.Defaults(ctx, b) })
	s.route("POST", "/api/proxy/groups", "admin", "ProxyGroupPatch", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Proxy.SaveGroup(ctx, b) })
	s.route("DELETE", "/api/proxy/groups/{group_id}", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.Proxy.DeleteGroup(ctx, r.PathValue("group_id"))
	})
	s.route("POST", "/api/proxy/nodes/import", "admin", "ProxyNodeImportRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return ImportProxyNodes(b) })
	s.route("POST", "/api/proxy/test", "admin", "ProxyTestRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		candidate := text(b["url"])
		var e error
		if candidate != "" {
			candidate, e = NormalizeProxy(candidate)
			if e != nil {
				return nil, e
			}
		} else {
			lease, e := s.Proxy.Resolve(ctx, nil, "", false, true, false)
			if e != nil {
				return nil, e
			}
			candidate = lease.Profile.ProxyURL
			lease.Release()
		}
		if candidate == "" {
			return nil, problem(400, "proxy url is required")
		}
		result, e := s.proxyProbe(ctx, candidate)
		return Map{"result": result}, e
	})
	s.route("POST", "/api/proxy/groups/test", "admin", "ProxyGroupTestRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		rows := []Map{}
		if explicit := text(b["url"]); explicit != "" {
			candidate, e := NormalizeProxy(explicit)
			if e != nil {
				return nil, e
			}
			result, e := s.proxyProbe(ctx, candidate)
			if e != nil {
				return nil, e
			}
			return proxyTestSummary(append(rows, Map{"node_id": "", "result": result})), nil
		}
		gid := groupID(b["id"])
		if gid == "" {
			return nil, problem(400, "proxy group id or url is required")
		}
		view, e := s.Proxy.View(ctx)
		if e != nil {
			return nil, e
		}
		var group Map
		for _, g := range mapSlice(view["groups"]) {
			if g["id"] == gid {
				group = g
				break
			}
		}
		if group == nil {
			return nil, problem(404, "proxy group not found")
		}
		for _, node := range mapSlice(group["nodes"]) {
			if !boolean(node["enabled"], true) || text(node["url"]) == "" || (text(b["node_id"]) != "" && node["id"] != text(b["node_id"])) {
				continue
			}
			result, e := s.proxyProbe(ctx, text(node["url"]))
			if e != nil {
				return nil, e
			}
			rows = append(rows, Map{"node_id": node["id"], "result": result})
		}
		if len(rows) == 0 {
			return nil, problem(400, "proxy group node url is required")
		}
		return proxyTestSummary(rows), nil
	})
	s.route("GET", "/api/proxy/runtime", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		raw, e := s.DB.Document(ctx, "system_settings")
		if e != nil {
			return nil, e
		}
		runtime := normalizeRoot("proxy_runtime", object(raw["proxy_runtime"]))
		clearance := object(runtime["clearance"])
		cookies, clear := text(clearance["cf_cookies"]), text(clearance["cf_clearance"])
		clearance["cf_cookies"], clearance["cf_clearance"], clearance["has_cf_cookies"], clearance["has_cf_clearance"] = "", "", cookies != "", clear != ""
		lease, e := s.Proxy.Resolve(ctx, nil, "", false, true, false)
		if e != nil {
			return nil, e
		}
		defer lease.Release()
		profile := lease.Profile
		mode := "direct"
		if profile.ProxyURL != "" {
			mode = "proxy"
		}
		cmode := strings.ToLower(text(profile.Clearance["mode"]))
		enabled := profile.RuntimeEnabled && boolean(profile.Clearance["enabled"], false) && (cmode == "manual" || cmode == "flaresolverr")
		label := profile.EgressLabel
		status := Map{"enabled": profile.RuntimeEnabled, "egress_mode": mode, "proxy_source": profile.Source, "egress_key": profile.egressKey, "egress_label": label, "image_concurrency_limit": profile.ImageConcurrencyLimit, "has_proxy": profile.ProxyURL != "", "skip_ssl_verify": profile.SkipSSLVerify, "clearance_enabled": enabled, "clearance_mode": cmode, "has_clearance_bundle": false, "cached_clearance_hosts": []string{}}
		if s.Clearance != nil {
			status = s.Clearance.status(profile)
		}
		return Map{"runtime": runtime, "status": status}, nil
	})
}
