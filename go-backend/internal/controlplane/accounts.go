package controlplane

import (
	"context"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"strings"
	"sync"
	"time"
)

var ErrCredentialsChanged = errors.New("account credentials changed during maintenance")

type workContextKey struct{}
type WorkGate struct {
	settings *Settings
	mu       sync.Mutex
	active   int
	changed  chan struct{}
}

func newWorkGate(s *Settings) *WorkGate { return &WorkGate{settings: s, changed: make(chan struct{})} }
func (g *WorkGate) Acquire(ctx context.Context) (context.Context, func(), error) {
	if ctx.Value(workContextKey{}) == g {
		return ctx, func() {}, nil
	}
	for {
		if e := ctx.Err(); e != nil {
			return ctx, nil, e
		}
		values, e := g.settings.Values(ctx)
		if e != nil {
			return ctx, nil, e
		}
		limit := max(1, min(100, integer(values["account_processing_concurrency"], 30)))
		g.mu.Lock()
		if g.active < limit {
			g.active++
			g.mu.Unlock()
			var once sync.Once
			return context.WithValue(ctx, workContextKey{}, g), func() {
				once.Do(func() { g.mu.Lock(); g.active--; close(g.changed); g.changed = make(chan struct{}); g.mu.Unlock() })
			}, nil
		}
		ch := g.changed
		g.mu.Unlock()
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx, nil, ctx.Err()
		case <-ch:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (g *WorkGate) Active() int { g.mu.Lock(); defer g.mu.Unlock(); return g.active }

type Accounts struct {
	cumulativeDir string
	db            *repository.Database
	proxy         *Proxy
	settings      *Settings
	now           func() time.Time
	gate          *WorkGate
	mu            sync.Mutex
	inflight      map[string]int
	imageEvidence map[imageEvidenceKey]imageEvidence
	index         uint64
	changed       chan struct{}
	maintenance   *Maintenance
}

func NewAccounts(db *repository.Database, p *Proxy, s *Settings, now func() time.Time) *Accounts {
	if now == nil {
		now = time.Now
	}
	return &Accounts{db: db, proxy: p, settings: s, now: now, gate: newWorkGate(s), inflight: map[string]int{}, imageEvidence: map[imageEvidenceKey]imageEvidence{}, changed: make(chan struct{})}
}
func (a *Accounts) Initialize(ctx context.Context) error {
	_, e := a.db.Modify(ctx, "accounts", nil, func(items []Map) ([]Map, error) { return normalizeAccounts(items, a.now(), true), nil })
	return e
}
func (a *Accounts) List(ctx context.Context) ([]Map, error) {
	snap, e := a.db.Snapshot(ctx, "accounts")
	if e != nil {
		return nil, e
	}
	items := normalizeAccounts(snap.Items, a.now(), false)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range items {
		m["image_inflight"] = a.inflight[text(m["management_id"])]
	}
	return items, nil
}
func byID(items []Map, mid string) Map {
	mid = strings.ToLower(strings.TrimSpace(mid))
	for _, m := range items {
		if text(m["management_id"]) == mid {
			return m
		}
	}
	return nil
}
func byToken(items []Map, token string) Map {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	for _, m := range items {
		if text(m["access_token"]) == token {
			return m
		}
	}
	fp := digest(token)
	for _, m := range items {
		for _, s := range stringsOf(m["access_token_fingerprints"]) {
			if s == fp {
				return m
			}
		}
	}
	return nil
}
func (a *Accounts) Get(ctx context.Context, mid string) (Map, error) {
	items, e := a.List(ctx)
	if e != nil {
		return nil, e
	}
	return byID(items, mid), nil
}
func stripRuntime(m Map) Map    { out := copyMap(m); delete(out, "image_inflight"); return out }
func payloadToken(m Map) string { return text(orValue(m["access_token"], m["accessToken"])) }
func (a *Accounts) Add(ctx context.Context, payloads []Map, restore bool) (Map, error) {
	ctx, release, e := a.gate.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	cumulative, unlockCumulative, e := a.cumulativeBeforeAdd(ctx)
	if e != nil {
		return nil, e
	}
	defer unlockCumulative()
	prepared := []Map{}
	indices := []int{}
	refs := [][2]string{}
	for _, m := range payloads {
		token := payloadToken(m)
		if token == "" {
			continue
		}
		p := copyMap(m)
		delete(p, "accessToken")
		if !restore {
			delete(p, "management_id")
			delete(p, "access_token_fingerprints")
		}
		p["access_token"] = token
		if strings.EqualFold(text(p["type"]), "codex") {
			p["export_type"], p["source_type"] = "codex", "codex"
			delete(p, "type")
		}
		if strings.EqualFold(text(p["export_type"]), "codex") {
			p["source_type"] = "codex"
		}
		if truth(p["plan_type"]) && !truth(p["type"]) {
			p["type"] = text(p["plan_type"])
		}
		if _, ok := p["proxy"]; ok || text(p["proxy_group_id"]) != "" {
			indices = append(indices, len(prepared))
			refs = append(refs, [2]string{text(p["proxy"]), text(p["proxy_group_id"])})
		}
		prepared = append(prepared, p)
	}
	result := Map{"added": 0, "skipped": 0}
	persist := func(normalized []string) error {
		for i, index := range indices {
			prepared[index]["proxy"] = normalized[i]
		}
		_, e := a.db.Modify(ctx, "accounts", nil, func(raw []Map) ([]Map, error) {
			items := normalizeAccounts(raw, a.now(), false)
			order := []string{}
			deduped := map[string]Map{}
			for _, p := range prepared {
				token := payloadToken(p)
				if prior, ok := deduped[token]; ok {
					for k, v := range p {
						prior[k] = v
					}
					deduped[token] = prior
				} else {
					deduped[token] = p
					order = append(order, token)
				}
			}
			added, skipped := 0, len(prepared)-len(order)
			for _, token := range order {
				incoming := deduped[token]
				current := byToken(items, token)
				if current != nil && text(current["access_token"]) != token {
					skipped++
					continue
				}
				isNew := current == nil
				if isNew {
					added++
					current = Map{"created_at": a.now().UTC().Format("2006-01-02 15:04:05")}
				} else {
					skipped++
				}
				if _, ok := incoming["refresh_token"]; ok {
					_, hasInvalidAt := incoming["refresh_token_invalid_at"]
					if !(restore && hasInvalidAt) {
						incoming["refresh_token_invalid_at"] = current["refresh_token_invalid_at"]
						if text(incoming["refresh_token"]) != text(current["refresh_token"]) {
							incoming["refresh_token_invalid_at"] = nil
						}
					}
				}
				if !truth(incoming["created_at"]) {
					delete(incoming, "created_at")
				}
				m := copyMap(current)
				for k, v := range incoming {
					m[k] = v
				}
				m["access_token"] = token
				m["type"] = orValue(plan(incoming["type"]), plan(current["type"]))
				m = NormalizeAccount(m, a.now())
				used := map[string]bool{}
				for _, v := range items {
					if v["access_token"] != token {
						used[text(v["management_id"])] = true
					}
				}
				m["management_id"] = uniqueManagement(token, m["management_id"], used)
				if isNew {
					items = append(items, m)
				} else {
					for i, v := range items {
						if v["access_token"] == token {
							items[i] = m
							break
						}
					}
				}
			}
			result["added"], result["skipped"] = added, skipped
			return items, nil
		})
		return e
	}
	if len(refs) > 0 {
		e = a.proxy.WithAssignments(ctx, refs, persist)
	} else {
		e = persist(nil)
	}
	if e == nil {
		if a.maintenance != nil {
			a.maintenance.invalidatePreflight("")
		}
		a.cumulativeAdded(cumulative, integer(result["added"], 0))
		a.notify()
	}
	return result, e
}
func (a *Accounts) notify() {
	a.mu.Lock()
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
}
func (a *Accounts) Update(ctx context.Context, mid string, patch, guard Map, preserveDisabled bool) (Map, error) {
	ctx, release, e := a.gate.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	values, e := a.settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	patch = copyMap(patch)
	var result Map
	persist := func(normalized []string) error {
		if len(normalized) > 0 {
			patch["proxy"] = normalized[0]
		}
		_, e := a.db.Modify(ctx, "accounts", nil, func(raw []Map) ([]Map, error) {
			items := normalizeAccounts(raw, a.now(), false)
			current := byID(items, mid)
			if current == nil {
				return nil, problem(404, "account not found")
			}
			for k, v := range guard {
				if canonical(current[k]) != canonical(v) {
					return nil, ErrCredentialsChanged
				}
			}
			token := text(current["access_token"])
			nextToken := text(patch["access_token"])
			if nextToken == "" {
				nextToken = token
			}
			owner := byToken(items, nextToken)
			if nextToken != token && owner != nil && owner["management_id"] != current["management_id"] {
				return nil, problem(400, "access token already belongs to another account")
			}
			refreshChanged := false
			if v, ok := patch["refresh_token"]; ok {
				refreshChanged = text(v) != text(current["refresh_token"])
				patch["refresh_token_invalid_at"] = current["refresh_token_invalid_at"]
				if refreshChanged {
					patch["refresh_token_invalid_at"] = nil
				}
			}
			credentialsChanged := nextToken != token || refreshChanged
			if credentialsChanged {
				for _, k := range []string{"last_token_refresh_error", "last_token_refresh_error_at", "last_refresh_error", "last_refresh_error_at", "last_invalid_at"} {
					patch[k] = nil
				}
				patch["invalid_count"] = 0
				if current["last_remote_check_result"] == "invalid" {
					for _, k := range []string{"last_remote_check_result", "last_remote_check_error", "last_remote_check_error_at", "last_remote_check_event", "pending_auth_remove_invalid", "pending_auth_scope"} {
						patch[k] = nil
					}
				}
				if current["status"] == "异常" {
					patch["status"] = "正常"
				}
			}
			m := copyMap(current)
			for k, v := range patch {
				m[k] = v
			}
			m["access_token"] = nextToken
			if current["status"] == "限流" && normalizedStatus(patch["status"], m) == "正常" {
				if _, changed := patch["status"]; changed {
					if _, explicit := patch["restore_at"]; !explicit {
						m["restore_at"] = nil
					}
				}
			}
			if preserveDisabled && current["status"] == "禁用" {
				m["status"] = "禁用"
			}
			if credentialsChanged {
				for _, k := range []string{"last_remote_check_error", "last_refresh_error", "last_token_refresh_error"} {
					m[k] = diagnostic(m[k], current, 0)
				}
			}
			if _, ok := patch["proxy"]; ok && text(current["proxy"]) != text(m["proxy"]) {
				for _, k := range []string{"last_remote_check_error", "last_refresh_error", "last_token_refresh_error"} {
					m[k] = diagnostic(m[k], Map{"proxy": current["proxy"]}, 0)
				}
			}
			m = NormalizeAccount(m, a.now())
			remove := (m["status"] == "异常" && boolean(values["auto_remove_invalid_accounts"], true)) || (m["status"] == "限流" && boolean(values["auto_remove_rate_limited_accounts"], false))
			out := []Map{}
			for _, v := range items {
				if v["management_id"] == current["management_id"] {
					if !remove {
						out = append(out, m)
						result = m
					}
				} else {
					out = append(out, v)
				}
			}
			return out, nil
		})
		return e
	}
	if _, ok := patch["proxy"]; ok || patch["proxy_group_id"] != nil {
		e = a.proxy.WithAssignments(ctx, [][2]string{{text(patch["proxy"]), text(patch["proxy_group_id"])}}, persist)
	} else {
		e = persist(nil)
	}
	if e == nil {
		if a.maintenance != nil {
			a.maintenance.invalidatePreflight(mid)
		}
		a.notify()
	}
	return result, e
}
func (a *Accounts) Batch(ctx context.Context, ids []string, patch Map, remove bool) (Map, error) {
	ctx, release, e := a.gate.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	values, e := a.settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	ids = unique(ids)
	requested := map[string]bool{}
	for _, mid := range ids {
		requested[mid] = true
	}
	updated, removed := []string{}, []string{}
	_, e = a.db.Modify(ctx, "accounts", nil, func(raw []Map) ([]Map, error) {
		items := normalizeAccounts(raw, a.now(), false)
		out := []Map{}
		for _, m := range items {
			mid := text(m["management_id"])
			if !requested[mid] {
				out = append(out, m)
				continue
			}
			if remove {
				removed = append(removed, mid)
				continue
			}
			next := copyMap(m)
			for k, v := range patch {
				next[k] = v
			}
			if m["status"] == "限流" && patch["status"] == "正常" {
				if _, ok := patch["restore_at"]; !ok {
					next["restore_at"] = nil
				}
			}
			next = NormalizeAccount(next, a.now())
			auto := (next["status"] == "异常" && boolean(values["auto_remove_invalid_accounts"], true)) || (next["status"] == "限流" && boolean(values["auto_remove_rate_limited_accounts"], false))
			if auto {
				removed = append(removed, mid)
			} else {
				updated = append(updated, mid)
				out = append(out, next)
			}
		}
		return out, nil
	})
	if e == nil {
		if a.maintenance != nil {
			a.maintenance.invalidatePreflight("")
		}
		a.notify()
	}
	return Map{"updated_ids": orderIDs(updated, ids), "removed_ids": orderIDs(removed, ids)}, e
}
func (a *Accounts) GroupData(ctx context.Context) (Map, error) {
	groups, e := a.db.Document(ctx, "account_group_configuration")
	if e != nil {
		return nil, e
	}
	items, e := a.List(ctx)
	if e != nil {
		return nil, e
	}
	counts := map[string]int{}
	for _, m := range items {
		counts[text(m["group_id"])]++
	}
	pv, e := a.proxy.View(ctx)
	if e != nil {
		return nil, e
	}
	proxyGroups := mapSlice(pv["groups"])
	names := map[string]string{}
	for _, g := range proxyGroups {
		names[text(g["id"])] = text(g["name"])
	}
	out := []Map{}
	for _, g := range dicts(groups["items"]) {
		gid := slug(g["id"])
		if gid == "" {
			continue
		}
		projection := assignment(g["proxy"], g["proxy_group_id"], names)
		name := text(g["name"])
		if name == "" {
			name = gid
		}
		out = append(out, Map{"id": gid, "name": name, "proxy": projection["reference"], "proxy_group_id": projection["group_id"], "proxy_mode": projection["mode"], "proxy_label": projection["label"], "enabled": truth(defaultValue(g["enabled"], true)), "notes": text(g["notes"]), "account_count": counts[gid]})
	}
	return Map{"groups": out, "proxy_groups": proxyGroups}, nil
}
func (a *Accounts) GroupNames(ctx context.Context) (map[string]string, error) {
	raw, e := a.db.Document(ctx, "account_group_configuration")
	if e != nil {
		return nil, e
	}
	out := map[string]string{}
	for _, g := range dicts(raw["items"]) {
		gid := slug(g["id"])
		name := text(g["name"])
		if name == "" {
			name = gid
		}
		if gid != "" {
			out[gid] = name
		}
	}
	return out, nil
}
func (a *Accounts) SaveGroup(ctx context.Context, body Map) (Map, error) {
	gid := slug(orValue(body["id"], body["name"]))
	if gid == "" {
		return nil, problem(400, "account group id is required")
	}
	var projected Map
	e := a.proxy.WithAssignments(ctx, [][2]string{{text(body["proxy"]), text(body["proxy_group_id"])}}, func(refs []string) error {
		_, e := a.db.UpdateDocument(ctx, "account_group_configuration", func(raw Map) (Map, error) {
			groups := dicts(raw["items"])
			out := []Map{}
			for _, g := range groups {
				if slug(g["id"]) == gid {
					if boolean(body["create_only"], false) {
						return nil, problem(400, "account group already exists")
					}
				} else {
					out = append(out, g)
				}
			}
			name := text(body["name"])
			if name == "" {
				name = gid
			}
			projection := assignment(refs[0], "", nil)
			g := Map{"id": gid, "name": name, "proxy": projection["reference"], "proxy_group_id": projection["group_id"], "enabled": boolean(body["enabled"], true), "notes": text(body["notes"])}
			raw["items"] = append(out, g)
			return raw, nil
		})
		return e
	})
	if e != nil {
		return nil, e
	}
	payload, e := a.GroupData(ctx)
	if e != nil {
		return nil, e
	}
	for _, g := range mapSlice(payload["groups"]) {
		if g["id"] == gid {
			projected = g
		}
	}
	payload["group"] = projected
	return payload, nil
}
func (a *Accounts) DeleteGroup(ctx context.Context, gid string) (Map, []string, []string, error) {
	gid = slug(gid)
	_, e := a.db.UpdateDocument(ctx, "account_group_configuration", func(raw Map) (Map, error) {
		groups := dicts(raw["items"])
		out := []Map{}
		found := false
		for _, g := range groups {
			if slug(g["id"]) == gid {
				found = true
			} else {
				out = append(out, g)
			}
		}
		if !found {
			return nil, problem(404, "account group not found")
		}
		raw["items"] = out
		return raw, nil
	})
	if e != nil {
		return nil, nil, nil, e
	}
	items, e := a.List(ctx)
	if e != nil {
		return nil, nil, nil, e
	}
	ids := []string{}
	for _, m := range items {
		if m["group_id"] == gid {
			ids = append(ids, text(m["management_id"]))
		}
	}
	result, e := a.Batch(ctx, ids, Map{"group_id": ""}, false)
	if e != nil {
		return nil, nil, nil, e
	}
	payload, e := a.GroupData(ctx)
	if e != nil {
		return nil, nil, nil, e
	}
	payload["deleted"] = gid
	return payload, stringsOf(result["updated_ids"]), stringsOf(result["removed_ids"]), nil
}
func (a *Accounts) ValidateGroup(ctx context.Context, gid string, status int) error {
	if gid == "" {
		return nil
	}
	names, e := a.GroupNames(ctx)
	if e != nil {
		return e
	}
	if _, ok := names[gid]; !ok {
		return problem(status, "account group not found")
	}
	return nil
}
func matches(m Map, keyword, status, gid string, now time.Time) bool {
	needle := strings.ToLower(strings.TrimSpace(keyword))
	if needle != "" {
		found := false
		for _, k := range []string{"access_token", "email", "user_id", "type", "source_type", "status", "proxy", "group_id"} {
			if strings.Contains(strings.ToLower(text(m[k])), needle) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "" && status != "all" {
		switch status {
		case "normal", "limited", "abnormal", "disabled":
			if statusCategory(m, now) != status {
				return false
			}
		default:
			if strings.ToLower(text(m["status"])) != status {
				return false
			}
		}
	}
	gid = strings.TrimSpace(gid)
	if gid == "__ungrouped__" {
		return text(m["group_id"]) == ""
	}
	return gid == "" || gid == "all" || text(m["group_id"]) == gid
}
func (a *Accounts) Page(ctx context.Context, page, size int, keyword, status, gid string) (Map, error) {
	items, e := a.List(ctx)
	if e != nil {
		return nil, e
	}
	names, e := a.GroupNames(ctx)
	if e != nil {
		return nil, e
	}
	filtered := []Map{}
	for _, m := range items {
		if matches(m, keyword, status, gid, a.now()) {
			filtered = append(filtered, m)
		}
	}
	start := len(filtered)
	if page <= len(filtered)/size+1 {
		start = min((page-1)*size, len(filtered))
	}
	end := min(start+size, len(filtered))
	rows := []Map{}
	for _, m := range filtered[start:end] {
		groupID := text(m["group_id"])
		name := names[groupID]
		if name == "" {
			name = groupID
		}
		rows = append(rows, AccountRow(m, name, a.now()))
	}
	return Map{"items": rows, "total": len(filtered), "all_total": len(items), "page": page, "page_size": size}, nil
}
func (a *Accounts) Targets(ctx context.Context, selection Map, ids, tokens []string, defaultAll bool) ([]Map, []string, error) {
	items, e := a.List(ctx)
	if e != nil {
		return nil, nil, e
	}
	mode := text(selection["mode"])
	if mode == "" {
		mode = "explicit"
		if selection == nil && defaultAll && len(ids) == 0 && len(tokens) == 0 {
			mode = "all"
		}
	}
	out := []Map{}
	missing := []string{}
	seen := map[string]bool{}
	add := func(m Map) {
		if m == nil {
			return
		}
		mid := text(m["management_id"])
		if mid != "" && !seen[mid] {
			seen[mid] = true
			out = append(out, m)
		}
	}
	if mode == "explicit" {
		requested := append(stringsOf(selection["account_ids"]), ids...)
		lower := []string{}
		for _, mid := range requested {
			lower = append(lower, strings.ToLower(strings.TrimSpace(mid)))
		}
		for _, mid := range unique(lower) {
			m := byID(items, mid)
			if m == nil {
				missing = append(missing, mid)
			} else {
				add(m)
			}
		}
		for _, token := range unique(tokens) {
			add(byToken(items, token))
		}
		return out, missing, nil
	}
	excluded := map[string]bool{}
	for _, mid := range stringsOf(selection["excluded_account_ids"]) {
		excluded[strings.ToLower(mid)] = true
	}
	for _, m := range items {
		if excluded[text(m["management_id"])] {
			continue
		}
		if mode == "filter" && !matches(m, text(selection["keyword"]), text(selection["status"]), text(selection["group_id"]), a.now()) {
			continue
		}
		add(m)
	}
	return out, missing, nil
}
func (a *Accounts) SelectionPreview(ctx context.Context, selection Map) (Map, error) {
	if selection["mode"] == "explicit" || text(selection["mode"]) == "" {
		targets, missing, e := a.Targets(ctx, selection, nil, nil, false)
		if e != nil {
			return nil, e
		}
		return Map{"matching_count": len(targets), "selected_count": len(targets), "excluded_account_ids": []string{}, "errors": notFoundErrors(missing)}, nil
	}
	noExclude := copyMap(selection)
	delete(noExclude, "excluded_account_ids")
	all, _, e := a.Targets(ctx, noExclude, nil, nil, false)
	if e != nil {
		return nil, e
	}
	valid := map[string]bool{}
	for _, m := range all {
		valid[text(m["management_id"])] = true
	}
	excluded := []string{}
	for _, mid := range stringsOf(selection["excluded_account_ids"]) {
		mid = strings.ToLower(mid)
		if valid[mid] {
			excluded = append(excluded, mid)
		}
	}
	excluded = unique(excluded)
	return Map{"matching_count": len(all), "selected_count": max(0, len(all)-len(excluded)), "excluded_account_ids": excluded, "errors": []Map{}}, nil
}
func notFoundErrors(ids []string) []Map {
	out := []Map{}
	for _, mid := range unique(ids) {
		out = append(out, Map{"id": mid, "code": "account_not_found", "message": "account not found"})
	}
	return out
}
func targetIDs(items []Map) []string {
	out := []string{}
	for _, m := range items {
		out = append(out, text(m["management_id"]))
	}
	return out
}
