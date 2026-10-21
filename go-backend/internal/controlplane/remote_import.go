package controlplane

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"golang.org/x/text/cases"
	"strings"
	"sync"
	"time"
)

type remoteLoginToken struct {
	value   string
	expires time.Time
}
type RemoteImports struct {
	server  *Server
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	workers sync.WaitGroup
	closing bool
	tokens  map[string]remoteLoginToken
}

func NewRemoteImports(ctx context.Context, s *Server) (*RemoteImports, error) {
	parent, cancel := context.WithCancel(ctx)
	owner := &RemoteImports{server: s, ctx: parent, cancel: cancel, tokens: map[string]remoteLoginToken{}}
	for _, provider := range []string{"cpa", "sub2api"} {
		_, err := s.DB.UpdateRemoteImports(ctx, provider, func(items []Map) ([]Map, error) {
			out := []Map{}
			for _, m := range items {
				out = append(out, normalizeImportConfig(provider, m, true, s.now()))
			}
			return out, nil
		})
		if err != nil {
			cancel()
			return nil, err
		}
	}
	return owner, nil
}
func (i *RemoteImports) Close(ctx context.Context) error {
	i.mu.Lock()
	i.closing = true
	i.cancel()
	i.mu.Unlock()
	done := make(chan struct{})
	go func() { i.workers.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}
func (i *RemoteImports) list(ctx context.Context, provider string) ([]Map, error) {
	raw, err := i.server.DB.RemoteImports(ctx, provider)
	if err != nil {
		return nil, err
	}
	out := []Map{}
	for _, m := range raw {
		out = append(out, normalizeImportConfig(provider, m, false, i.server.now()))
	}
	return out, nil
}
func (i *RemoteImports) get(ctx context.Context, provider, sourceID string) (Map, error) {
	items, err := i.list(ctx, provider)
	for _, m := range items {
		if m["id"] == sourceID {
			return m, err
		}
	}
	return nil, err
}
func importNoun(provider string) string {
	if provider == "cpa" {
		return "pool"
	}
	return "server"
}
func importLabel(provider string) string {
	if provider == "cpa" {
		return "CPA"
	}
	return "Sub2API"
}
func (i *RemoteImports) save(ctx context.Context, provider, sourceID string, payload Map, create, remove bool) (Map, error) {
	var result Map
	_, err := i.server.DB.UpdateRemoteImports(ctx, provider, func(items []Map) ([]Map, error) {
		out := []Map{}
		found := false
		for _, raw := range items {
			m := normalizeImportConfig(provider, raw, false, i.server.now())
			if m["id"] != sourceID {
				out = append(out, m)
				continue
			}
			found = true
			if importActive(m["import_job"]) {
				return nil, problem(409, importLabel(provider)+" import job is active")
			}
			if !remove {
				for key, v := range payload {
					if v != nil {
						m[key] = v
					}
				}
				m["id"] = sourceID
				result = normalizeImportConfig(provider, m, false, i.server.now())
				out = append(out, result)
			}
		}
		if create {
			m := copyMap(payload)
			m["id"] = id(6)
			result = normalizeImportConfig(provider, m, false, i.server.now())
			out = append(out, result)
		} else if !found {
			return nil, problem(404, importNoun(provider)+" not found")
		}
		return out, nil
	})
	if err == nil && provider == "sub2api" {
		i.mu.Lock()
		for key := range i.tokens {
			if strings.HasPrefix(key, sourceID+":") {
				delete(i.tokens, key)
			}
		}
		i.mu.Unlock()
	}
	return result, err
}
func (i *RemoteImports) jobUpdate(ctx context.Context, provider, sourceID, jobID string, patch Map) (Map, error) {
	var result Map
	_, err := i.server.DB.UpdateRemoteImports(ctx, provider, func(items []Map) ([]Map, error) {
		for index, source := range items {
			if text(source["id"]) != sourceID {
				continue
			}
			current := object(source["import_job"])
			if text(current["job_id"]) != jobID {
				return items, nil
			}
			next := merge(current, patch)
			next["updated_at"] = iso(i.server.now())
			next["failed_total"] = max(integer(current["failed_total"], integer(current["failed"], 0)), integer(patch["failed_total"], integer(patch["failed"], 0)))
			source["import_job"] = normalizeImportJob(next, false, i.server.now(), source)
			items[index] = source
			result = object(source["import_job"])
			break
		}
		return items, nil
	})
	return result, err
}
func (i *RemoteImports) start(ctx context.Context, provider string, source Map, payload Map) (Map, error) {
	field, missing := "names", "selected files is required"
	if provider == "sub2api" {
		field, missing = "account_ids", "account ids is required"
	}
	ids := unique(stringsOf(payload[field]))
	if len(ids) == 0 {
		return nil, problem(400, missing)
	}
	groupPresent := payload["target_group_id"] != nil
	gid := slug(payload["target_group_id"])
	if groupPresent {
		if err := i.server.Accounts.ValidateGroup(ctx, gid, 400); err != nil {
			return nil, err
		}
	}
	sourceID := text(source["id"])
	now := iso(i.server.now())
	job := normalizeImportJob(Map{"job_id": id(16), "status": "pending", "stage": "read_credentials", "stage_total": len(ids), "stage_completed": 0, "created_at": now, "updated_at": now, "total": len(ids), "completed": 0, "added": 0, "skipped": 0, "synced": 0, "failed": 0, "errors": []Map{}}, false, i.server.now(), source)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closing {
		return nil, problem(503, "account import service is shutting down")
	}
	err := i.server.DB.UpdateAllRemoteImports(ctx, func(all map[string][]Map) error {
		for _, items := range all {
			for _, m := range items {
				if importActive(m["import_job"]) {
					return problem(409, "an account import job is already running")
				}
			}
		}
		found := false
		for index, m := range all[provider] {
			if text(m["id"]) == sourceID {
				m["import_job"] = copyMap(job)
				all[provider][index] = m
				found = true
				break
			}
		}
		if !found {
			return problem(400, importNoun(provider)+" not found")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	bindings := map[string]string{}
	if provider == "sub2api" && !groupPresent && boolean(payload["create_account_groups"], true) {
		bindings, err = i.localBindings(ctx, dicts(payload["group_bindings"]))
		if err != nil {
			_, _ = i.jobUpdate(ctx, provider, sourceID, text(job["job_id"]), Map{"status": "failed", "errors": []Map{{"stage": "fetch", "name": "local account groups", "error": importDiagnostic(err.Error(), source)}}, "failed_total": 1})
			return nil, err
		}
	}
	i.workers.Add(1)
	go func() { defer i.workers.Done(); i.run(provider, source, ids, job, gid, groupPresent, bindings) }()
	return copyMap(job), nil
}
func (i *RemoteImports) localBindings(ctx context.Context, bindings []Map) (map[string]string, error) {
	result := map[string]string{}
	_, err := i.server.DB.UpdateDocument(ctx, "account_group_configuration", func(raw Map) (Map, error) {
		items := dicts(raw["items"])
		for _, b := range bindings {
			ids := stringsOf(b["account_ids"])
			if len(ids) == 0 {
				continue
			}
			name := text(orValue(b["name"], b["remote_group_id"]))
			if name == "" {
				continue
			}
			gid := ""
			for _, g := range items {
				if cases.Fold().String(text(g["name"])) == cases.Fold().String(name) {
					gid = slug(g["id"])
					if gid == "" {
						gid = slug(name)
					}
					break
				}
			}
			if gid == "" {
				gid = slug(name)
				if gid == "" {
					gid = "sub2api-" + remoteGroupDigest(name + ":" + text(b["remote_group_id"]))[:8]
				}
				exists := false
				for _, g := range items {
					if slug(g["id"]) == gid {
						exists = true
					}
				}
				if !exists {
					items = append(items, Map{"id": gid, "name": name, "proxy": "", "proxy_group_id": "", "enabled": true, "notes": ""})
				}
			}
			for _, accountID := range ids {
				result[accountID] = gid
			}
		}
		raw["items"] = items
		return raw, nil
	})
	return result, err
}

type remoteFetched struct {
	id      string
	payload Map
	err     error
}

func (i *RemoteImports) run(provider string, source Map, ids []string, job Map, gid string, groupPresent bool, bindings map[string]string) {
	ctx, cancel := context.WithCancel(i.ctx)
	var children sync.WaitGroup
	defer func() { cancel(); children.Wait() }()
	sourceID, jobID := text(source["id"]), text(job["job_id"])
	errorsList, events := []Map{}, []Map{}
	failed, completed := 0, 0
	persist := func(patch Map) bool {
		patch["failed_total"], patch["errors"], patch["events"] = failed, errorsList, events
		saved, err := i.jobUpdate(ctx, provider, sourceID, jobID, patch)
		return err == nil && saved != nil
	}
	appendFailure := func(stage, accountID, message string) {
		failed++
		message = importDiagnostic(message, source)
		errorsList = append(errorsList, Map{"stage": stage, "name": importDiagnostic(accountID, source), "error": message})
		if len(errorsList) > 20 {
			errorsList = errorsList[len(errorsList)-20:]
		}
		events = importAppendEvent(events, accountID, accountID, "failed", message, i.server.now(), source)
	}
	defer func() {
		if p := recover(); p != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_, _ = i.jobUpdate(cleanup, provider, sourceID, jobID, Map{"status": "failed", "stage": "completed", "failed_total": max(1, failed), "errors": []Map{{"stage": "fetch", "name": importLabel(provider) + " import worker", "error": "remote import worker failed"}}, "events": events})
		} else if ctx.Err() != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_, _ = i.jobUpdate(cleanup, provider, sourceID, jobID, Map{"status": "failed", "stage": "completed", "completed": completed, "failed_total": failed + 1, "errors": append(errorsList, Map{"stage": "fetch", "name": importLabel(provider) + " import worker", "error": "account import was interrupted"}), "events": events})
		}
	}()
	if !persist(Map{"status": "running", "stage": "read_credentials", "stage_total": len(ids), "stage_completed": 0}) {
		return
	}
	fetched := []remoteFetched{}
	remaining := append([]string(nil), ids...)
	batchError := ""
	batchErrors := map[string]string{}
	if provider == "sub2api" {
		callCtx, release, err := i.server.Accounts.gate.Acquire(ctx)
		if err != nil {
			return
		}
		batch, bad, err := i.subExport(callCtx, source, ids)
		release()
		if err != nil {
			batchError = err.Error()
		} else {
			batchErrors = bad
		}
		remaining = []string{}
		for _, accountID := range ids {
			if payload := batch[accountID]; payload != nil {
				fetched = append(fetched, remoteFetched{id: accountID, payload: payload})
				completed++
			} else {
				remaining = append(remaining, accountID)
			}
		}
	}
	values, err := i.server.Settings.Values(ctx)
	if err != nil {
		appendFailure("fetch", "settings", "could not read account processing settings")
		persist(Map{"status": "failed"})
		return
	}
	workers := min(len(remaining), max(1, min(100, integer(values["account_processing_concurrency"], 30))))
	queue := make(chan string)
	results := make(chan remoteFetched, max(1, workers))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		children.Add(1)
		go func() {
			defer wg.Done()
			defer children.Done()
			for accountID := range queue {
				callCtx, release, err := i.server.Accounts.gate.Acquire(ctx)
				var payload Map
				if err == nil {
					if provider == "cpa" {
						payload, err = i.cpaFetch(callCtx, source, accountID)
					} else {
						payload, err = i.subFetch(callCtx, source, accountID)
					}
					release()
				}
				select {
				case results <- remoteFetched{id: accountID, payload: payload, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(queue)
		for _, accountID := range remaining {
			select {
			case queue <- accountID:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	last, pending := time.Now(), 0
	for result := range results {
		completed++
		pending++
		if result.err != nil {
			if ctx.Err() != nil {
				continue
			}
			message := result.err.Error()
			if batchError != "" {
				message = "batch export failed: " + batchError + "; fallback failed: " + message
			} else if batchErrors[result.id] != "" {
				message = "batch export skipped: " + batchErrors[result.id] + "; fallback failed: " + message
			}
			appendFailure("fetch", result.id, message)
		} else if result.payload != nil {
			fetched = append(fetched, result)
		} else {
			appendFailure("fetch", result.id, "missing access_token")
		}
		if pending >= 20 || time.Since(last) >= 250*time.Millisecond {
			if !persist(Map{"completed": completed, "stage_completed": completed}) {
				cancel()
				return
			}
			pending, last = 0, time.Now()
		}
	}
	if ctx.Err() != nil {
		return
	}
	if !persist(Map{"stage": "save_accounts", "stage_total": 1, "stage_completed": 0, "completed": completed}) {
		return
	}
	payloads := []Map{}
	for _, item := range fetched {
		p := item.payload
		if groupPresent {
			p["group_id"] = gid
		} else if group := bindings[item.id]; group != "" {
			p["group_id"] = group
		}
		payloads = append(payloads, p)
	}
	add, err := i.server.Accounts.Add(ctx, payloads, false)
	if err != nil {
		for _, item := range fetched {
			appendFailure("sync", item.id, "account storage failed")
		}
		persist(Map{"status": "failed", "completed": len(ids)})
		return
	}
	// The repository preserves one save for the complete batch. Resolve statuses
	// in the same request order used for the durable Account Repository mutation.
	addedRemaining := integer(add["added"], 0)
	for _, item := range fetched {
		status, message := "skipped", "账号已存在，凭据已更新"
		if addedRemaining > 0 {
			status, message = "success", "账号导入成功"
			addedRemaining--
		}
		label := text(orValue(item.payload["email"], item.id))
		events = importAppendEvent(events, item.id, label, status, message, i.server.now(), source, item.payload)
	}
	if !persist(Map{"stage": "sync_accounts", "stage_total": len(fetched), "stage_completed": 0, "added": add["added"], "skipped": add["skipped"]}) {
		return
	}
	synced := 0
	syncQueue := make(chan remoteFetched)
	syncResults := make(chan remoteFetched, max(1, min(len(fetched), max(1, integer(values["account_processing_concurrency"], 30)))))
	wg = sync.WaitGroup{}
	for range min(len(fetched), max(1, min(100, integer(values["account_processing_concurrency"], 30)))) {
		wg.Add(1)
		children.Add(1)
		go func() {
			defer wg.Done()
			defer children.Done()
			for item := range syncQueue {
				accounts, err := i.server.Accounts.List(ctx)
				account := byToken(accounts, text(item.payload["access_token"]))
				if err == nil && account != nil {
					_, err = i.server.Maintenance.Sync(ctx, text(account["management_id"]))
				} else if err == nil {
					err = errors.New("account not found")
				}
				select {
				case syncResults <- remoteFetched{id: item.id, payload: item.payload, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(syncQueue)
		for _, item := range fetched {
			select {
			case syncQueue <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(syncResults) }()
	for item := range syncResults {
		if item.err != nil {
			appendFailure("sync", item.id, importDiagnostic(item.err.Error(), item.payload))
		} else {
			synced++
		}
	}
	if ctx.Err() != nil {
		return
	}
	persist(Map{"status": "completed", "stage": "completed", "stage_total": len(ids), "stage_completed": len(ids), "completed": len(ids), "added": add["added"], "skipped": add["skipped"], "synced": synced})
}
func importAppendEvent(events []Map, accountID, label, status, message string, now time.Time, sources ...Map) []Map {
	sequence := 1
	for _, event := range events {
		sequence = max(sequence, integer(event["sequence"], 0)+1)
	}
	events = append(events, Map{"sequence": sequence, "timestamp": iso(now), "account_id": importDiagnostic(accountID, sources...), "account_label": importDiagnostic(label, sources...), "action": "import_account", "status": status, "tone": map[string]string{"failed": "danger", "success": "success", "skipped": "warning"}[status], "message": importDiagnostic(message, sources...)})
	if len(events) > 500 {
		events = events[len(events)-500:]
	}
	return events
}
func remoteGroupDigest(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}
