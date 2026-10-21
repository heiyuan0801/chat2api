package controlplane

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// spec.json is the Go-owned baseline contract: field defaults, metadata and
// request schemas. Runtime Go code never imports or invokes Python.
//
//go:embed spec.json
var specBytes []byte

type baselineSpec struct {
	Fields    map[string]Map `json:"fields"`
	Managed   []string       `json:"managed"`
	Sensitive []string       `json:"sensitive"`
	Effective Map            `json:"effective_defaults"`
	Models    map[string]Map `json:"models"`
}

var baseline = func() baselineSpec {
	var v baselineSpec
	d := json.NewDecoder(bytes.NewReader(specBytes))
	d.UseNumber()
	if d.Decode(&v) != nil {
		panic("invalid embedded contract")
	}
	return v
}()

type Settings struct {
	db      *repository.Database
	envBase string
	now     func() time.Time
}

func NewSettings(db *repository.Database, envBase string, now func() time.Time) *Settings {
	if now == nil {
		now = time.Now
	}
	return &Settings{db: db, envBase: urlText(envBase), now: now}
}
func promote(raw Map) Map {
	m := copyMap(raw)
	for _, k := range []string{"auto_relogin_after_refresh", "image_auth_refresh_concurrency", "image_preflight_token_refresh_enabled"} {
		delete(m, k)
	}
	basic := object(m["basic"])
	for _, k := range []string{"proxy", "base_url"} {
		if _, ok := m[k]; !ok {
			if v, found := basic[k]; found {
				if k == "base_url" {
					m[k] = urlText(v)
				} else {
					m[k] = text(v)
				}
			}
		}
	}
	if _, ok := m["image_retention_hours"]; !ok {
		if v, found := basic["image_expire_hours"]; found {
			m["image_retention_hours"] = max(1, integer(v, 24))
		}
	}
	return m
}
func normalizedField(path string, value any) any {
	spec := baseline.Fields[path]
	def := spec["default"]
	switch def.(type) {
	case bool:
		return boolean(value, def.(bool))
	case json.Number:
		f, ok := number(value)
		if !ok {
			f, _ = number(def)
		}
		minimum, hasMin := number(spec["min"])
		maximum, hasMax := number(spec["max"])
		if hasMin && f < minimum {
			f = minimum
		}
		if hasMax && f > maximum {
			f = maximum
		}
		if strings.Contains(text(def), ".") {
			return json.Number(canonical(f))
		}
		return int(f)
	case []any:
		xs := stringsOf(value)
		if path == "log_levels" {
			allowed := map[string]bool{"debug": true, "info": true, "warning": true, "error": true}
			next := []string{}
			for _, s := range xs {
				if s = strings.ToLower(s); allowed[s] {
					next = append(next, s)
				}
			}
			xs = next
		}
		return unique(xs)
	default:
		s := text(defaultValue(value, def))
		options := stringsOf(spec["options"])
		if len(options) > 0 {
			s = strings.ToLower(s)
			found := false
			for _, v := range options {
				if s == v {
					found = true
				}
			}
			if !found {
				s = text(def)
			}
		}
		if strings.HasSuffix(path, "url") {
			s = urlText(s)
		}
		switch path {
		case "proxy_runtime.clearance.user_agent", "image_storage.webdav_root_path", "third_party_apps.infinite_canvas.url":
			if s == "" {
				s = text(def)
			}
		case "backup.prefix":
			s = strings.Trim(s, "/")
			if s == "" {
				s = "backups"
			}
		}
		return s
	}
}
func sensitiveFlag(path string) string {
	switch path {
	case "ai_review.api_key":
		return "has_api_key"
	case "image_storage.webdav_password":
		return "has_webdav_password"
	case "genbox_push.push_key":
		return "has_push_key"
	case "backup.secret_access_key":
		return "has_secret_access_key"
	case "backup.passphrase":
		return "has_passphrase"
	case "proxy_runtime.clearance.cf_cookies":
		return "has_cf_cookies"
	case "proxy_runtime.clearance.cf_clearance":
		return "has_cf_clearance"
	}
	return ""
}
func isSensitive(path string) bool { return sensitiveFlag(path) != "" }
func (s *Settings) effective(stored Map) Map {
	out := copyMap(baseline.Effective)
	for k, v := range stored {
		out[k] = v
	}
	out["base_url"] = urlText(stored["base_url"])
	if s.envBase != "" {
		out["base_url"] = s.envBase
	}
	return out
}
func (s *Settings) revision(stored Map) string {
	effective := s.effective(stored)
	relevant := Map{}
	for _, k := range baseline.Managed {
		if k == "base_url" {
			relevant[k] = effective[k]
		} else if v, ok := stored[k]; ok {
			relevant[k] = v
		} else {
			relevant[k] = effective[k]
		}
	}
	return digest(canonical(relevant))[:16]
}
func normalizeRoot(root string, raw Map) Map {
	out := copyMap(raw)
	for path := range baseline.Fields {
		if !strings.HasPrefix(path, root+".") {
			continue
		}
		part := strings.TrimPrefix(path, root+".")
		v, _ := pathGet(raw, part)
		if part == "provider" {
			out[part] = "cloudflare_r2"
			continue
		}
		pathSet(out, part, normalizedField(path, v))
	}
	switch root {
	case "image_storage":
		if !boolean(out["enabled"], false) {
			out["mode"] = "local"
		}
		out["webdav_root_path"] = strings.Trim(text(out["webdav_root_path"]), "/")
		if out["webdav_root_path"] == "" {
			out["webdav_root_path"] = "chatgpt2api/images"
		}
	case "genbox_push":
		out["base_url"] = genboxURL(out["base_url"])
		delete(out, "clear_push_key")
	case "backup":
		delete(object(out["include"]), "register")
	case "proxy_runtime":
		delete(out, "proxy_url")
		delete(out, "egress_mode")
		delete(out, "_existing_cf_cookies")
		delete(out, "_existing_cf_clearance")
		clearance := object(out["clearance"])
		if text(clearance["browser"]) == "" {
			clearance["browser"] = "chrome"
		}
		out["clearance"] = clearance
		codes := []int{}
		seen := map[int]bool{}
		for _, v := range list(out["reset_session_status_codes"]) {
			if _, isBool := v.(bool); isBool {
				continue
			}
			n := integer(v, 0)
			if n >= 100 && n <= 599 && !seen[n] {
				codes = append(codes, n)
				seen[n] = true
			}
		}
		if len(codes) == 0 {
			codes = []int{403}
		}
		out["reset_session_status_codes"] = codes
	}
	for _, p := range baseline.Sensitive {
		if strings.HasPrefix(p, root+".") {
			bits := strings.Split(strings.TrimPrefix(p, root+"."), ".")
			parent := out
			for _, k := range bits[:len(bits)-1] {
				parent = object(parent[k])
			}
			delete(parent, sensitiveFlag(p))
		}
	}
	return out
}
func genboxURL(v any) string {
	u, e := url.Parse(text(v))
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" && p != "/api/sync/push" {
		return ""
	}
	u.Path = ""
	u.RawPath = ""
	return u.String()
}
func (s *Settings) projection(raw Map) Map {
	effective := s.effective(raw)
	for _, root := range []string{"image_storage", "genbox_push", "backup", "proxy_runtime"} {
		effective[root] = normalizeRoot(root, object(effective[root]))
	}
	out := Map{}
	for path := range baseline.Fields {
		v, _ := pathGet(effective, path)
		pathSet(out, path, normalizedField(path, v))
	}
	for _, p := range baseline.Sensitive {
		v, _ := pathGet(raw, p)
		pathSet(out, p, "")
		bits := strings.Split(p, ".")
		flag := strings.Join(append(bits[:len(bits)-1], sensitiveFlag(p)), ".")
		pathSet(out, flag, text(v) != "")
	}
	object(out["backup"])["provider"] = "cloudflare_r2"
	return out
}
func (s *Settings) view(raw Map) Map {
	stored := promote(raw)
	fields := Map{}
	for path, spec := range baseline.Fields {
		m := copyMap(spec)
		_, ok := pathGet(stored, path)
		m["source"] = "default"
		if ok {
			m["source"] = "configured"
		}
		if path == "base_url" && s.envBase != "" {
			m["source"] = "environment"
			m["read_only"] = true
		}
		fields[path] = m
	}
	return Map{"schema_version": 1, "generated_at": generated(s.now()), "revision": s.revision(stored), "settings": s.projection(stored), "fields": fields}
}
func (s *Settings) View(ctx context.Context) (Map, error) {
	m, e := s.db.Document(ctx, "system_settings")
	if e != nil {
		return nil, e
	}
	return s.view(m), nil
}
func (s *Settings) Values(ctx context.Context) (Map, error) {
	v, e := s.View(ctx)
	if e != nil {
		return nil, e
	}
	return object(v["settings"]), nil
}
func (s *Settings) Public(ctx context.Context) (Map, error) {
	v, e := s.Values(ctx)
	if e != nil {
		return nil, e
	}
	return Map{"api_base_url": v["base_url"], "console_request_timeout_secs": v["console_request_timeout_secs"], "third_party_apps": v["third_party_apps"]}, nil
}
func comparePath(projected, stored Map, path string) any {
	if isSensitive(path) {
		v, _ := pathGet(stored, path)
		return text(v)
	}
	v, _ := pathGet(projected, path)
	return v
}

var genboxSource = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func validateStoredSettings(m Map) error {
	storage := object(m["image_storage"])
	if boolean(storage["enabled"], false) {
		if text(storage["webdav_url"]) == "" {
			return problem(400, "启用 WebDAV 图片存储后必须填写 WebDAV URL")
		}
		if text(storage["webdav_password"]) == "" {
			return problem(400, "启用 WebDAV 图片存储后必须填写 WebDAV 密码")
		}
	}
	g := object(m["genbox_push"])
	if boolean(g["enabled"], false) {
		if genboxURL(g["base_url"]) == "" {
			return problem(400, "启用 GenBox Push 后必须填写有效的 HTTP(S) 地址")
		}
		if !genboxSource.MatchString(text(g["source_id"])) {
			return problem(400, "GenBox 来源 ID 格式无效")
		}
		if text(g["push_key"]) == "" {
			return problem(400, "启用 GenBox Push 后必须填写 Push Key")
		}
	}
	return nil
}
func (s *Settings) Update(ctx context.Context, patch Map) (Map, error) {
	values := copyMap(patch)
	expected := text(values["revision"])
	delete(values, "revision")
	clear := boolean(object(values["genbox_push"])["clear_push_key"], false)
	for _, p := range baseline.Sensitive {
		if v, ok := pathGet(values, p); ok && text(v) == "" {
			pathDelete(values, p)
		}
	}
	requested := leafPaths(values, "")
	changed := []string{}
	next, e := s.db.UpdateDocument(ctx, "system_settings", func(raw Map) (Map, error) {
		stored := promote(raw)
		if expected != s.revision(stored) {
			return nil, problem(409, "settings revision does not match current configuration")
		}
		if _, ok := values["base_url"]; ok && s.envBase != "" {
			return nil, problem(400, "base_url is read-only while CHATGPT2API_BASE_URL is configured")
		}
		before := s.projection(stored)
		updates := Map{}
		effective := s.effective(stored)
		for k, v := range values {
			if child, ok := v.(map[string]any); ok {
				base, exists := stored[k]
				if _, isMap := base.(map[string]any); !exists || !isMap {
					base = effective[k]
				}
				updates[k] = merge(object(base), child)
			} else {
				updates[k] = v
			}
		}
		if clear {
			g := object(updates["genbox_push"])
			if text(object(values["genbox_push"])["push_key"]) == "" {
				delete(g, "push_key")
			}
			updates["genbox_push"] = g
		}
		roots := map[string]bool{}
		for _, p := range requested {
			if canonical(comparePath(before, stored, p)) != canonical(comparePath(updates, updates, p)) {
				roots[strings.Split(p, ".")[0]] = true
			}
		}
		if clear {
			roots["genbox_push"] = true
		}
		result := copyMap(raw)
		for k, v := range updates {
			if !roots[k] {
				continue
			}
			if child, ok := v.(map[string]any); ok {
				switch k {
				case "backup", "image_storage", "genbox_push", "proxy_runtime", "third_party_apps":
					v = normalizeRoot(k, child)
				}
			}
			result[k] = v
		}
		if e := validateStoredSettings(result); e != nil {
			return nil, e
		}
		afterStored := promote(result)
		after := s.projection(afterStored)
		for _, p := range requested {
			if canonical(comparePath(before, stored, p)) != canonical(comparePath(after, afterStored, p)) {
				changed = append(changed, p)
			}
		}
		if clear {
			changed = append(changed, "genbox_push.push_key")
		}
		return result, nil
	})
	if e != nil {
		return nil, e
	}
	view := s.view(next)
	changed = unique(changed)
	sort.Strings(changed)
	view["changed_fields"] = changed
	view["restart_required"] = false
	return view, nil
}

// runtimeValues preserves unmanaged legacy runtime switches while applying the
// same normalized field defaults as the public settings projection. Secrets
// stay in their owning configuration and are not replaced with masked values.
func (s *Settings) runtimeValues(ctx context.Context) (Map, error) {
	raw, err := s.db.Document(ctx, "system_settings")
	if err != nil {
		return nil, err
	}
	stored := promote(raw)
	values := s.effective(stored)
	projection := s.projection(stored)
	for field := range baseline.Fields {
		if isSensitive(field) {
			continue
		}
		v, _ := pathGet(projection, field)
		pathSet(values, field, v)
	}
	return values, nil
}
