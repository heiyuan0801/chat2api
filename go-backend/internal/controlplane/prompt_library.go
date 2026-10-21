package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed prompt_defaults.json
var promptDefaultsJSON []byte
var promptDefaults = func() Map {
	var m Map
	if json.Unmarshal(promptDefaultsJSON, &m) != nil {
		panic("invalid prompt defaults")
	}
	return m
}()

type PromptLibrary struct {
	mutate                              sync.Mutex
	s                                   *Server
	repository                          *repository.PromptLibrary
	mu                                  sync.Mutex
	view                                Map
	serialized                          []byte
	state                               repository.PromptState
	sources                             []Map
	items, status                       Map
	registryRevision, registryGenerated string
	verified                            bool
}

func NewPromptLibrary(s *Server) *PromptLibrary {
	return &PromptLibrary{s: s, repository: repository.NewPromptLibrary(s.DB)}
}
func promptJSON(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	raw := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	raw = bytes.ReplaceAll(raw, []byte(`\u2028`), []byte("\u2028"))
	return bytes.ReplaceAll(raw, []byte(`\u2029`), []byte("\u2029"))
}
func promptHash(v any) string { sum := sha256.Sum256(promptJSON(v)); return hex.EncodeToString(sum[:]) }
func promptContent(items []Map) string {
	h := sha256.New()
	for _, item := range items {
		h.Write([]byte(text(item["id"])))
		h.Write([]byte{0})
		h.Write([]byte(text(item["prompt"])))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var promptSourceID = regexp.MustCompile(`^[a-z0-9-]+$`)
var promptHex = regexp.MustCompile(`^[a-f0-9]{64}$`)
var promptPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func validPromptPath(raw string) bool {
	raw = strings.ReplaceAll(raw, "\\", "/")
	return raw != "" && promptPath.MatchString(raw) && !slices.Contains(strings.Split(raw, "/"), "..")
}
func promptURL(raw string, optional bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" && optional {
		return "", nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", errors.New("invalid registry URL")
	}
	return raw, nil
}
func promptSnapshotDigest(sources []Map, revision, generated string, items Map) string {
	return promptHash(Map{"registry_sources": sources, "registry_revision": revision, "registry_generated_at": generated, "items_by_source": items})
}
func promptDefaultSetting(id string, index int) Map {
	for _, v := range dicts(promptDefaults["sources"]) {
		if v["id"] == id {
			return Map{"enabled": true, "sort_order": index * 10, "updated_at": ""}
		}
	}
	return Map{"enabled": false, "sort_order": index * 10, "updated_at": ""}
}

// promptInteger matches Python int() for registry values: numeric JSON values
// truncate, while strings must contain an integer rather than a decimal.
func promptInteger(v any) (int, bool) {
	switch value := v.(type) {
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		return n, err == nil
	case bool, nil:
		return 0, false
	case int:
		return value, true
	case int64:
		if value < math.MinInt || value > math.MaxInt {
			return 0, false
		}
		return int(value), true
	case json.Number, float64:
		n, ok := number(value)
		bound := math.Ldexp(1, strconv.IntSize-1)
		if !ok || n < -bound || n >= bound {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}
func promptNonnegativeInteger(v any) int {
	n, _ := promptInteger(v)
	return max(0, n)
}
func promptCachedSource(raw Map) (Map, bool) {
	id := text(raw["id"])
	if !promptSourceID.MatchString(id) {
		return nil, false
	}
	fallback := Map{}
	for _, v := range dicts(promptDefaults["sources"]) {
		if v["id"] == id {
			fallback = v
			break
		}
	}
	home, e := promptURL(text(orValue(raw["homepage"], fallback["homepage"])), false)
	if e != nil {
		return nil, false
	}
	up, e := promptURL(text(orValue(raw["upstream_url"], orValue(raw["upstreamUrl"], orValue(raw["url"], fallback["upstream_url"])))), false)
	if e != nil {
		return nil, false
	}
	path := text(orValue(raw["path"], orValue(fallback["path"], "sources/"+id+".json")))
	path = strings.TrimLeft(strings.ReplaceAll(path, "\\", "/"), "/")
	if !validPromptPath(path) {
		return nil, false
	}
	name := truncateRunes(strings.Join(strings.Fields(text(orValue(raw["name"], orValue(fallback["name"], id)))), " "), 120)
	return Map{"id": id, "name": name, "homepage": home, "upstream_url": up, "path": path, "count": promptNonnegativeInteger(raw["count"]), "sha256": strings.ToLower(text(raw["sha256"]))}, true
}
func (p *PromptLibrary) load(ctx context.Context) error {
	if p.view != nil {
		a, b, e := p.repository.Revisions(ctx)
		if e != nil {
			return e
		}
		if a == p.state.SettingsRevision && b == p.state.SnapshotRevision {
			return nil
		}
	}
	state, e := p.repository.Load(ctx)
	if e != nil {
		return e
	}
	if p.view != nil && p.state.SettingsRevision == state.SettingsRevision && p.state.SnapshotRevision == state.SnapshotRevision {
		return nil
	}
	p.state = state
	p.sources = dicts(copyMap(promptDefaults)["sources"])
	snapshot := state.Snapshot
	registry := object(snapshot["registry"])
	rawSources := list(registry["sources"])
	sources := []Map{}
	seen := map[string]bool{}
	metadataValid := len(rawSources) > 0
	for _, raw := range rawSources {
		source, ok := promptCachedSource(object(raw))
		if !ok || seen[text(source["id"])] {
			metadataValid = false
			continue
		}
		seen[text(source["id"])] = true
		sources = append(sources, source)
	}
	p.registryRevision, p.registryGenerated = "", ""
	if metadataValid && len(sources) == len(rawSources) {
		p.sources = sources
		p.registryRevision = text(registry["revision"])
		p.registryGenerated = text(registry["generated_at"])
	}
	p.items, p.status = Map{}, Map{}
	rawStatus := object(snapshot["source_status"])
	rawItems, hasItems := snapshot["items_by_source"].(map[string]any)
	if !hasItems {
		rawItems = Map{}
		for id, v := range object(snapshot["sources"]) {
			if cache, ok := v.(map[string]any); ok {
				rawItems[id] = orValue(cache["items"], []any{})
			}
		}
	}
	defs := object(stage4Models["PromptLibraryView"]["$defs"])
	for _, source := range p.sources {
		id := text(source["id"])
		items := []Map{}
		for _, raw := range dicts(rawItems[id]) {
			candidate := Map{}
			schema := object(defs["PromptLibraryItem"])
			for field := range object(schema["properties"]) {
				if v, ok := raw[field]; ok {
					candidate[field] = v
				}
			}
			if _, ok := candidate["source_id"]; !ok {
				candidate["source_id"] = id
			}
			if _, ok := candidate["source_name"]; !ok {
				candidate["source_name"] = source["name"]
			}
			if _, ok := candidate["created_at"]; !ok {
				candidate["created_at"] = text(orValue(raw["created_at"], raw["created"]))
			}
			for field, definition := range object(schema["properties"]) {
				if _, ok := candidate[field]; !ok {
					if fallback, hasDefault := object(definition)["default"]; hasDefault {
						candidate[field] = fallback
					}
				}
			}
			v, issues := validateSchema(candidate, schema, defs, nil)
			if len(issues) == 0 {
				items = append(items, object(v))
			}
		}
		if _, ok := rawItems[id].([]any); ok {
			p.items[id] = items
			raw := object(rawStatus[id])
			fetch := any(nil)
			if n, ok := promptInteger(raw["last_fetch_ms"]); ok {
				fetch = max(0, n)
			}
			p.status[id] = Map{"content_revision": orValue(text(raw["content_revision"]), promptContent(items)), "last_sync_at": text(raw["last_sync_at"]), "last_error": text(raw["last_error"]), "last_fetch_ms": fetch}
		}
	}
	p.verified = metadataValid && snapshot["snapshot_verified"] == true && promptHex.MatchString(p.registryRevision) && text(snapshot["snapshot_digest"]) == promptSnapshotDigest(p.sources, p.registryRevision, p.registryGenerated, p.items)
	if len(dicts(p.items["banana-prompt-quicker"])) == 0 {
		p.items["banana-prompt-quicker"] = dicts(copyMap(promptDefaults)["items"])
		p.status["banana-prompt-quicker"] = Map{"content_revision": promptContent(dicts(p.items["banana-prompt-quicker"])), "last_sync_at": promptDefaults["modified_at"], "last_error": "", "last_fetch_ms": 0}
		p.state, e = p.repository.Replace(ctx, "snapshot", p.snapshot())
		if e != nil {
			return e
		}
	}
	p.view = p.build()
	return nil
}
func (p *PromptLibrary) settings() Map {
	out := Map{}
	for index, value := range list(p.state.Settings["sources"]) {
		raw, ok := value.(map[string]any)
		if !ok || !truth(raw["id"]) {
			continue
		}
		id := text(raw["id"])
		if !promptSourceID.MatchString(id) {
			continue
		}
		defaults := promptDefaultSetting(id, index)
		enabled := boolean(defaults["enabled"], false)
		switch value := raw["enabled"].(type) {
		case nil:
		case bool:
			enabled = value
		case json.Number, float64, int, int64:
			if n, ok := number(value); ok {
				enabled = n != 0
			}
		default:
			switch strings.ToLower(text(value)) {
			case "1", "true", "yes", "y", "on":
				enabled = true
			case "0", "false", "no", "n", "off", "none", "null", "":
				enabled = false
			}
		}
		order := index * 10
		switch value := raw["sort_order"].(type) {
		case string:
			if n, e := strconv.Atoi(strings.TrimSpace(value)); e == nil {
				order = n
			}
		case json.Number, float64, int, int64:
			order = integer(value, order)
		}
		updated := ""
		if truth(raw["updated_at"]) {
			updated = text(raw["updated_at"])
		}
		out[id] = Map{"enabled": enabled, "sort_order": order, "updated_at": updated}
	}
	return out
}

func (p *PromptLibrary) setting(source Map, index int) Map {
	defaults := promptDefaultSetting(text(source["id"]), index)
	return merge(defaults, object(p.settings()[text(source["id"])]))
}
func promptSync(total, failed, count int) Map {
	ok := max(0, total-failed)
	status, tone, message := "success", "success", "没有启用的提示词源"
	if failed > 0 && ok == 0 {
		status, tone, message = "failed", "danger", fmt.Sprintf("词源同步失败：%d 个词源失败，已保留可用的本地缓存", failed)
	} else if failed > 0 {
		status, tone, message = "partial", "warning", fmt.Sprintf("词源同步完成：成功 %d，失败 %d，共 %d 条提示词", ok, failed, count)
	} else if total > 0 {
		message = fmt.Sprintf("词源同步完成：成功 %d，共 %d 条提示词", ok, count)
	}
	return Map{"status": status, "tone": tone, "total": total, "succeeded": ok, "failed": failed, "message": message}
}
func (p *PromptLibrary) build() Map {
	p.serialized = nil
	type pair struct{ source, setting Map }
	ordered := []pair{}
	for i, s := range p.sources {
		ordered = append(ordered, pair{s, p.setting(s, i)})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		x, y := integer(a.setting["sort_order"], 9999), integer(b.setting["sort_order"], 9999)
		if x != y {
			return x < y
		}
		if strings.ToLower(text(a.source["name"])) != strings.ToLower(text(b.source["name"])) {
			return strings.ToLower(text(a.source["name"])) < strings.ToLower(text(b.source["name"]))
		}
		return text(a.source["id"]) < text(b.source["id"])
	})
	sources, items, errs, revisions := []Map{}, []Map{}, []Map{}, []Map{}
	enabledCount, cachedCount := 0, 0
	for _, pair := range ordered {
		source, setting := pair.source, pair.setting
		id := text(source["id"])
		values := dicts(p.items[id])
		status := object(p.status[id])
		enabled, cached := boolean(setting["enabled"], false), len(values) > 0
		lastError, lastSync := text(status["last_error"]), text(status["last_sync_at"])
		state, label, message, tone := "pending", "待同步", "提示词源等待首次同步", "muted"
		if !enabled {
			state, label, message = "disabled", "已停用", "提示词源已停用"
		} else if lastError != "" {
			if cached {
				state, label, message, tone = "cached", "缓存可用", "同步失败，继续使用本地缓存", "warning"
			} else {
				state, label, message, tone = "failed", "同步失败", "同步失败，当前没有可用缓存", "danger"
			}
		} else if cached || lastSync != "" {
			state, label, message, tone = "synced", "已同步", "提示词源已更新到本地", "success"
		}
		if enabled {
			enabledCount++
			items = append(items, values...)
			if lastError != "" {
				errs = append(errs, Map{"id": id, "name": source["name"], "error": lastError})
			}
		}
		if cached {
			cachedCount++
		}
		sources = append(sources, Map{"id": id, "name": source["name"], "url": source["upstream_url"], "homepage": source["homepage"], "enabled": enabled, "built_in": true, "sort_order": max(0, integer(setting["sort_order"], 9999)), "prompt_count": len(values), "cached": cached, "sync_state": state, "sync_label": label, "sync_message": message, "sync_tone": tone, "last_sync_at": lastSync, "last_error": lastError, "last_fetch_ms": status["last_fetch_ms"]})
		revisions = append(revisions, Map{"id": id, "enabled": enabled, "sort_order": max(0, integer(setting["sort_order"], 9999)), "content_revision": text(status["content_revision"]), "last_error": lastError})
	}
	revision := promptHash(Map{"registry_revision": p.registryRevision, "sources": revisions})[:24]
	return Map{"schema_version": 1, "generated_at": p.s.now().UTC().Format(time.RFC3339Nano), "revision": revision, "registry_revision": p.registryRevision, "registry_generated_at": p.registryGenerated, "synced": cachedCount > 0, "prompt_count": len(items), "source_count": len(sources), "enabled_source_count": enabledCount, "cached_source_count": cachedCount, "source_error_count": len(errs), "sync_summary": promptSync(enabledCount, len(errs), len(items)), "source_errors": errs, "items": items, "sources": sources}
}
func (p *PromptLibrary) snapshot() Map {
	snap := Map{"schema_version": 1, "updated_at": p.s.now().UTC().Format(time.RFC3339Nano), "snapshot_verified": p.verified, "registry": Map{"revision": p.registryRevision, "generated_at": p.registryGenerated, "sources": p.sources}, "items_by_source": p.items, "source_status": p.status}
	snap["snapshot_digest"] = promptSnapshotDigest(p.sources, p.registryRevision, p.registryGenerated, p.items)
	return snap
}

// wire returns an immutable serialized projection. Updates replace the slice;
// active readers retain their previous complete version without a copy or lock.
func (p *PromptLibrary) wire(ctx context.Context) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.load(ctx); e != nil {
		return nil, e
	}
	if p.serialized == nil {
		p.serialized = promptJSON(p.view)
	}
	return json.RawMessage(p.serialized), nil
}
func (p *PromptLibrary) View(ctx context.Context) (Map, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.load(ctx); e != nil {
		return nil, e
	}
	return copyMap(p.view), nil
}
func (p *PromptLibrary) Update(ctx context.Context, id string, body Map) (Map, error) {
	p.mutate.Lock()
	defer p.mutate.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.load(ctx); e != nil {
		return nil, e
	}
	known := false
	settings := p.settings()
	for i, s := range p.sources {
		key := text(s["id"])
		setting := p.setting(s, i)
		if key == id {
			known = true
			if body["enabled"] != nil {
				setting["enabled"] = body["enabled"]
			}
			setting["updated_at"] = p.s.now().UTC().Format(time.RFC3339Nano)
		}
		settings[key] = setting
	}
	if !known {
		return nil, problem(404, "prompt source not found")
	}
	rows := []Map{}
	for _, s := range p.sources {
		key := text(s["id"])
		setting := object(settings[key])
		setting["sort_order"] = max(0, integer(setting["sort_order"], 0))
		rows = append(rows, merge(Map{"id": key}, setting))
	}
	var e error
	p.state, e = p.repository.Replace(ctx, "settings", Map{"schema_version": 1, "sources": rows})
	if e != nil {
		return nil, e
	}
	p.view = p.build()
	return copyMap(p.view), nil
}
func (p *PromptLibrary) fetch(ctx context.Context, raw string, limit int64) ([]byte, error) {
	if _, e := promptURL(raw, false); e != nil {
		return nil, e
	}
	client, done, e := p.s.configuredHTTP(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", "application/json,*/*;q=0.5")
	req.Header.Set("User-Agent", "chatgpt2api-prompt-library/4.0")
	response, e := client.Do(req)
	if e != nil {
		return nil, errors.New("prompt registry request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("prompt registry HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("registry response exceeds %d bytes", limit)
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("registry response exceeds %d bytes", limit)
	}
	return data, nil
}

var promptMarkup = regexp.MustCompile(`<[^>]+>|!\[[^\]]*\]\([^)]*\)`)
var promptLinks = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)

// Registry strings use Python's str(value or "") normalization. Keep this
// scoped here: other control-plane projections intentionally use text().
func promptClean(v any) string {
	if !truth(v) {
		return ""
	}
	return text(v)
}
func promptDisplay(v any) string {
	s := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'", "&apos;", "'").Replace(promptClean(v))
	s = promptMarkup.ReplaceAllString(s, " ")
	s = promptLinks.ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(s), " ")
}
func promptList(v any, maxCount int) []string {
	out := []string{}
	for _, raw := range list(v) {
		v := promptClean(raw)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
			if len(out) >= maxCount {
				break
			}
		}
	}
	return out
}
func (p *PromptLibrary) candidate(ctx context.Context) ([]Map, string, string, Map, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("PROMPT_LIBRARY_REGISTRY_URL")), "/")
	if base == "" {
		base = "https://raw.githubusercontent.com/yukkcat/image-prompts/main/dist"
	}
	base = strings.TrimSuffix(base, "/manifest.json")
	raw, e := p.fetch(ctx, base+"/manifest.json", 128<<10)
	if e != nil {
		return nil, "", "", nil, e
	}
	manifest := Map{}
	if json.Unmarshal(raw, &manifest) != nil {
		return nil, "", "", nil, errors.New("registry manifest is not valid JSON")
	}
	// Python compares the schema value directly, without coercing strings or
	// truncating fractional numbers (True compares equal to 1 there).
	if manifest["schemaVersion"] != float64(1) && manifest["schemaVersion"] != true {
		return nil, "", "", nil, errors.New("unsupported registry schema")
	}
	revision := strings.ToLower(promptClean(manifest["registryHash"]))
	generated := promptClean(manifest["generatedAt"])
	path := strings.TrimLeft(strings.ReplaceAll(promptClean(manifest["promptsPath"]), "\\", "/"), "/")
	if !promptHex.MatchString(revision) || generated == "" || !validPromptPath(path) {
		return nil, "", "", nil, errors.New("invalid registry manifest metadata")
	}
	sources := []Map{}
	seen := map[string]bool{}
	total := 0
	rawSources, ok := manifest["sources"].([]any)
	if !ok || len(rawSources) == 0 {
		return nil, "", "", nil, errors.New("registry manifest has no sources")
	}
	for _, raw := range rawSources {
		s, ok := raw.(map[string]any)
		if !ok {
			return nil, "", "", nil, errors.New("registry source must be an object")
		}
		id := promptClean(s["id"])
		hash := strings.ToLower(promptClean(s["sha256"]))
		count, countOK := promptInteger(s["count"])
		sourcePath := strings.TrimLeft(strings.ReplaceAll(promptClean(s["path"]), "\\", "/"), "/")
		if !promptSourceID.MatchString(id) || seen[id] || !promptHex.MatchString(hash) || !countOK || count < 0 || count > math.MaxInt-total || promptClean(s["name"]) == "" || !validPromptPath(sourcePath) {
			return nil, "", "", nil, errors.New("invalid registry source")
		}
		home, e := promptURL(promptClean(s["homepage"]), false)
		if e != nil {
			return nil, "", "", nil, e
		}
		up, e := promptURL(promptClean(s["upstreamUrl"]), false)
		if e != nil {
			return nil, "", "", nil, e
		}
		seen[id] = true
		total += count
		sources = append(sources, Map{"id": id, "name": truncateRunes(strings.Join(strings.Fields(promptClean(s["name"])), " "), 120), "homepage": home, "upstream_url": up, "path": sourcePath, "count": count, "sha256": hash})
	}
	declaredTotal, totalOK := promptInteger(manifest["total"])
	if !totalOK || declaredTotal != total {
		return nil, "", "", nil, errors.New("registry manifest total does not match source counts")
	}
	p.mu.Lock()
	complete := p.verified && p.registryRevision == revision && len(p.sources) == len(sources) && len(p.items) == len(sources)
	if complete {
		for _, src := range sources {
			id := promptClean(src["id"])
			values := dicts(p.items[id])
			if len(values) != integer(src["count"], 0) || promptClean(object(p.status[id])["content_revision"]) != promptClean(src["sha256"]) {
				complete = false
				break
			}
		}
	}
	if complete {
		cached := copyMap(p.items)
		p.mu.Unlock()
		return sources, revision, generated, cached, nil
	}
	p.mu.Unlock()
	payload, e := p.fetch(ctx, base+"/"+path, 8<<20)
	if e != nil {
		return nil, "", "", nil, e
	}
	hashInput := append(promptJSON(rawSources), payload...)
	hash := sha256.Sum256(hashInput)
	if hex.EncodeToString(hash[:]) != revision {
		return nil, "", "", nil, errors.New("registry manifest and prompt payload hashes do not match")
	}
	var values []Map
	if json.Unmarshal(payload, &values) != nil || len(values) != total {
		return nil, "", "", nil, errors.New("registry prompt count does not match the manifest")
	}
	bySource := Map{}
	for _, s := range sources {
		bySource[promptClean(s["id"])] = []Map{}
	}
	seen = map[string]bool{}
	for _, v := range values {
		sourceID, id := promptClean(v["sourceId"]), promptClean(v["id"])
		var source Map
		for _, s := range sources {
			if s["id"] == sourceID {
				source = s
			}
		}
		if source == nil || !strings.HasPrefix(id, sourceID+":") || seen[id] || promptClean(v["title"]) == "" || promptClean(v["prompt"]) == "" {
			return nil, "", "", nil, errors.New("registry prompt has an invalid id/sourceId pair")
		}
		seen[id] = true
		allowed := strings.Fields("id sourceId title prompt description coverUrl referenceImageUrls tags author sourceUrl createdAt imageMode imageModel imageSize imageCount")
		for field := range v {
			if !slices.Contains(allowed, field) {
				return nil, "", "", nil, errors.New("registry prompt contains unsupported fields")
			}
		}
		for _, field := range allowed {
			if field != "imageSize" && field != "imageCount" {
				if _, ok := v[field]; !ok {
					return nil, "", "", nil, errors.New("registry prompt is missing fields")
				}
			}
		}
		tags, refs := promptList(v["tags"], 24), promptList(v["referenceImageUrls"], 12)
		for _, u := range append(append([]string{}, refs...), promptClean(v["sourceUrl"])) {
			if _, e := promptURL(u, false); e != nil {
				return nil, "", "", nil, e
			}
		}
		preview, e := promptURL(promptClean(v["coverUrl"]), true)
		if e != nil {
			return nil, "", "", nil, e
		}
		mode := strings.ToLower(promptClean(v["imageMode"]))
		switch mode {
		case "generate", "image", "text-to-image", "t2i":
			mode = "generate"
		case "edit", "image-to-image", "i2i":
			mode = "edit"
		default:
			mode = ""
		}
		category, sub := "", ""
		if len(tags) > 0 {
			category = truncateRunes(tags[0], 80)
		}
		if len(tags) > 1 {
			sub = truncateRunes(tags[1], 80)
		}
		count, hasCount := v["imageCount"]
		if hasCount {
			parsed, ok := promptInteger(count)
			if !ok || parsed < 1 {
				return nil, "", "", nil, errors.New("registry prompt has an invalid imageCount")
			}
			count = parsed
		}
		item := Map{"id": id, "source_id": sourceID, "source_name": source["name"], "title": truncateRunes(strings.Join(strings.Fields(promptClean(v["title"])), " "), 200), "prompt": promptClean(v["prompt"]), "description": truncateRunes(promptDisplay(v["description"]), 700), "preview": preview, "link": promptClean(v["sourceUrl"]), "author": truncateRunes(strings.Join(strings.Fields(promptClean(v["author"])), " "), 120), "category": category, "sub_category": sub, "tags": list(tags), "reference_image_urls": list(refs), "image_mode": mode, "image_model": truncateRunes(strings.Join(strings.Fields(promptClean(v["imageModel"])), " "), 80), "image_size": truncateRunes(strings.Join(strings.Fields(promptClean(v["imageSize"])), " "), 40), "image_count": count, "created_at": promptClean(v["createdAt"])}
		defs := object(stage4Models["PromptLibraryView"]["$defs"])
		validated, issues := validateSchema(item, object(defs["PromptLibraryItem"]), defs, nil)
		if len(issues) > 0 {
			return nil, "", "", nil, errors.New("registry prompt does not match item contract")
		}
		bySource[sourceID] = append(dicts(bySource[sourceID]), object(validated))
	}
	for _, s := range sources {
		if len(dicts(bySource[promptClean(s["id"])])) != integer(s["count"], 0) {
			return nil, "", "", nil, errors.New("registry source count does not match")
		}
	}
	return sources, revision, generated, bySource, nil
}
func (p *PromptLibrary) Refresh(ctx context.Context, id string) (Map, error) {
	p.mutate.Lock()
	defer p.mutate.Unlock()
	p.mu.Lock()
	if e := p.load(ctx); e != nil {
		p.mu.Unlock()
		return nil, e
	}
	if id != "" {
		known := false
		for _, s := range p.sources {
			known = known || s["id"] == id
		}
		if !known {
			p.mu.Unlock()
			return nil, problem(404, "prompt source not found")
		}
	}
	p.mu.Unlock()
	started := time.Now()
	sources, revision, generated, items, e := p.candidate(ctx)
	elapsed := time.Since(started).Milliseconds()
	p.mu.Lock()
	defer p.mu.Unlock()
	if e != nil {
		for i, s := range p.sources {
			key := text(s["id"])
			if key == id || (id == "" && boolean(p.setting(s, i)["enabled"], false)) {
				status := object(p.status[key])
				status["last_error"] = truncateRunes(e.Error(), 500)
				status["last_fetch_ms"] = elapsed
				p.status[key] = status
			}
		}
	} else {
		p.sources, p.registryRevision, p.registryGenerated, p.items, p.verified = sources, revision, generated, items, true
		p.status = Map{}
		for _, s := range sources {
			p.status[text(s["id"])] = Map{"content_revision": s["sha256"], "last_sync_at": p.s.now().UTC().Format(time.RFC3339Nano), "last_error": "", "last_fetch_ms": elapsed}
		}
	}
	next, writeErr := p.repository.Replace(ctx, "snapshot", p.snapshot())
	if writeErr != nil {
		return nil, writeErr
	}
	p.state = next
	p.view = p.build()
	return copyMap(p.view), nil
}
func (s *Server) registerPrompts() {
	s.route("GET", "/api/prompts", "identity", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Prompts.wire(ctx) })
	s.route("POST", "/api/admin/prompt-sources/refresh", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Prompts.Refresh(ctx, "") })
	s.route("POST", "/api/admin/prompt-sources/{source_id}/refresh", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.Prompts.Refresh(ctx, r.PathValue("source_id"))
	})
	s.route("POST", "/api/admin/prompt-sources/{source_id}", "admin", "PromptSourceRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.Prompts.Update(ctx, r.PathValue("source_id"), b)
	})
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n >= 0 && len(r) > n {
		return string(r[:n])
	}
	return s
}
