package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/http"
	"strings"
	"sync"
)

func (s *Server) accountProjection(ctx context.Context, mid string, detail bool) (Map, error) {
	m, e := s.Accounts.Get(ctx, mid)
	if e != nil {
		return nil, e
	}
	if m == nil {
		return nil, problem(404, "account not found")
	}
	names, e := s.Accounts.GroupNames(ctx)
	if e != nil {
		return nil, e
	}
	gid := text(m["group_id"])
	name := names[gid]
	if name == "" {
		name = gid
	}
	if detail {
		return AccountDetail(m, name, s.now()), nil
	}
	return AccountRow(m, name, s.now()), nil
}
func selectionBody(body Map) Map {
	v, ok := body["selection"].(map[string]any)
	if !ok {
		return nil
	}
	return v
}
func (s *Server) targets(ctx context.Context, body Map, legacy string, all bool) ([]Map, []string, error) {
	return s.Accounts.Targets(ctx, selectionBody(body), stringsOf(body["account_ids"]), stringsOf(body[legacy]), all)
}
func maintenanceError(mid string, err error, current Map) Map {
	code, message := "account_refresh_failed", diagnostic(err.Error(), current, 2000)
	var p *maintenanceProblem
	if errors.As(err, &p) {
		return Map{"id": mid, "code": p.code, "message": p.message}
	}
	failure := Map{"failure_code": "upstream_unavailable", "failure_scope": "transient", "failure_capability": nil, "failure_retryable": true, "failure_account_failure": true, "status_code": 502, "error_type": "server_error"}
	var credential *upstream.CredentialError
	var remote *upstream.Error
	if errors.As(err, &credential) && credential.Terminal || authFailure(err) {
		failure["failure_code"], failure["failure_scope"], failure["failure_capability"], failure["failure_retryable"], failure["status_code"], failure["error_type"] = "auth_invalid", "account", "auth", false, 401, "authentication_error"
	} else if errors.As(err, &remote) {
		switch remote.Kind {
		case "network_error":
			failure["failure_code"] = "upstream_connection_failed"
		case "rate_limited":
			failure["failure_code"], failure["failure_capability"], failure["failure_retryable"], failure["status_code"], failure["error_type"] = "upstream_rate_limited", "image_generation", false, 429, "rate_limit_error"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		failure["failure_code"], failure["status_code"] = "upstream_connection_timeout", 504
	}
	code = text(failure["failure_code"])
	out := Map{"id": mid, "code": code, "message": message}
	for k, v := range failure {
		out[k] = v
	}
	return out
}
func (s *Server) maintain(ctx context.Context, targets []Map, mode, jid string) (Map, error) {
	values, e := s.Settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	workers := min(len(targets), max(1, integer(values["account_processing_concurrency"], 30)))
	type completed struct {
		mid     string
		account Map
		changed bool
		err     error
	}
	work := make(chan Map)
	results := make(chan completed, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range work {
				mid := text(target["management_id"])
				var account Map
				changed := false
				var err error
				if mode == "refresh" {
					account, changed, err = s.Maintenance.Refresh(ctx, mid, true)
				} else {
					account, err = s.Maintenance.Sync(ctx, mid)
					changed = err == nil
				}
				select {
				case results <- completed{mid, account, changed, err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(work)
		for _, target := range targets {
			select {
			case work <- target:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	successIDs := []string{}
	errs := []Map{}
	succeeded, skipped := 0, 0
	labels := labelsFor(targets)
	for result := range results {
		status, message := "success", "账号与额度已同步"
		action := "sync_account"
		if mode == "refresh" {
			action, message = "refresh_access_token", "访问令牌已刷新"
		}
		if result.err != nil {
			current, _ := s.Accounts.Get(ctx, result.mid)
			err := maintenanceError(result.mid, result.err, current)
			errs = append(errs, err)
			status, message = "failed", text(err["message"])
		} else if result.changed {
			succeeded++
			successIDs = append(successIDs, result.mid)
		} else {
			skipped++
			status, message = "skipped", "访问令牌无需更新"
		}
		if jid != "" {
			event := Map{"timestamp": iso(s.now()), "account_id": result.mid, "account_label": labels[result.mid], "action": action, "status": status, "tone": map[string]string{"success": "success", "failed": "danger", "skipped": "warning"}[status], "message": message}
			s.Operations.Advance(jid, result.account, event)
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	updated, removed := []string{}, []string{}
	items, e := s.Accounts.List(ctx)
	if e != nil {
		return nil, e
	}
	if mode == "refresh" {
		for _, mid := range successIDs {
			if byID(items, mid) != nil {
				updated = append(updated, mid)
			}
		}
	} else {
		for _, target := range targets {
			mid := text(target["management_id"])
			if byID(items, mid) != nil {
				updated = append(updated, mid)
			}
		}
	}
	for _, target := range targets {
		mid := text(target["management_id"])
		if byID(items, mid) == nil {
			removed = append(removed, mid)
		}
	}
	counts := Map{}
	if mode == "refresh" {
		counts["refreshed"], counts["skipped"] = succeeded, skipped
	} else {
		counts["synced"] = succeeded
	}
	return s.Accounts.MutationPayload(ctx, counts, updated, removed, errs, nil, false, false)
}
func (s *Server) createAccounts(ctx context.Context, r *http.Request, b, who Map) (any, error) {
	var targetGroup *string
	if v, ok := b["target_group_id"]; ok && v != nil {
		gid := slug(v)
		if e := s.Accounts.ValidateGroup(ctx, gid, 400); e != nil {
			return nil, e
		}
		targetGroup = &gid
	}
	payloads := dicts(b["accounts"])
	payloadTokens := []string{}
	for i, p := range payloads {
		p = copyMap(p)
		if targetGroup != nil {
			p["group_id"] = *targetGroup
		}
		payloads[i] = p
		payloadTokens = append(payloadTokens, payloadToken(p))
	}
	tokens := unique(append(stringsOf(b["tokens"]), payloadTokens...))
	if len(tokens) == 0 {
		return nil, problem(400, "tokens is required")
	}
	payloadSet := map[string]bool{}
	for _, token := range unique(payloadTokens) {
		payloadSet[token] = true
	}
	extras := []Map{}
	for _, token := range tokens {
		if !payloadSet[token] {
			p := Map{"access_token": token, "source_type": "web"}
			if targetGroup != nil {
				p["group_id"] = *targetGroup
			}
			extras = append(extras, p)
		}
	}
	added, skipped := 0, 0
	for _, batch := range [][]Map{payloads, extras} {
		if len(batch) > 0 {
			result, e := s.Accounts.Add(ctx, batch, boolean(b["restore"], false))
			if e != nil {
				return nil, e
			}
			added += integer(result["added"], 0)
			skipped += integer(result["skipped"], 0)
		}
	}
	items, e := s.Accounts.List(ctx)
	if e != nil {
		return nil, e
	}
	targets := []Map{}
	for _, token := range tokens {
		if m := byToken(items, token); m != nil {
			targets = append(targets, m)
		}
	}
	syncAfter := true
	if v, ok := b["sync_after_import"]; ok && v != nil {
		syncAfter = truth(v)
	} else if v, ok := b["refresh"]; ok && v != nil {
		syncAfter = truth(v)
	}
	synced := 0
	errs := []Map{}
	if syncAfter {
		result, e := s.maintain(ctx, targets, "sync", "")
		if e != nil {
			return nil, e
		}
		synced = integer(result["synced"], 0)
		errs = mapSlice(result["errors"])
	}
	updated, removed := []string{}, []string{}
	items, e = s.Accounts.List(ctx)
	if e != nil {
		return nil, e
	}
	for _, target := range targets {
		mid := text(target["management_id"])
		if byID(items, mid) == nil {
			removed = append(removed, mid)
		} else {
			updated = append(updated, mid)
		}
	}
	events := operationEvents("import_account", updated, removed, errs, labelsFor(targets), "账号已保存", "账号保存后已被移除", s.now())
	payload, e := s.Accounts.MutationPayload(ctx, Map{"added": added, "skipped": skipped, "synced": synced}, updated, removed, errs, events, boolean(b["return_items"], true), true)
	if e != nil {
		return nil, e
	}
	if b["sync_after_import"] == nil && b["refresh"] != nil {
		payload["refreshed"] = synced
	}
	return payload, nil
}
func (s *Server) updateAccount(ctx context.Context, r *http.Request, b, who Map) (any, error) {
	mid := strings.ToLower(text(b["id"]))
	usesID := mid != ""
	items, e := s.Accounts.List(ctx)
	if e != nil {
		return nil, e
	}
	var current Map
	if usesID {
		current = byID(items, mid)
	} else if token := text(b["access_token"]); token != "" {
		current = byToken(items, token)
		if current != nil {
			mid = text(current["management_id"])
		}
	} else {
		return nil, problem(400, "account id is required")
	}
	if current == nil || mid == "" {
		return nil, problem(404, "account not found")
	}
	patch := Map{}
	for _, k := range []string{"type", "source_type", "quota", "proxy", "group_id"} {
		if v, ok := b[k]; ok && v != nil {
			patch[k] = v
		}
	}
	if usesID {
		for _, k := range []string{"access_token", "refresh_token"} {
			if value := text(b[k]); value != "" {
				patch[k] = value
			}
		}
	}
	if len(patch) == 0 {
		return nil, problem(400, "no updates provided")
	}
	updated, e := s.Accounts.Update(ctx, mid, patch, nil, false)
	if e != nil {
		return nil, e
	}
	successIDs, removed := []string{}, []string{}
	if updated == nil {
		removed = append(removed, mid)
	} else {
		successIDs = append(successIDs, mid)
	}
	events := operationEvents("update_account", successIDs, removed, nil, labelsFor([]Map{current}), "账号已更新", "账号更新失败", s.now())
	payload, e := s.Accounts.MutationPayload(ctx, Map{"updated": len(successIDs), "removed": len(removed)}, successIDs, removed, nil, events, updated != nil, true)
	if e != nil {
		return nil, e
	}
	payload["item"] = nil
	if updated != nil {
		payload["item"], e = s.accountProjection(ctx, mid, false)
	}
	return payload, e
}
func statusOperation(b Map) (string, string, string, error) {
	op := text(b["operation"])
	if op == "" {
		op = "update"
	}
	expected := map[string]string{"enable": "正常", "disable": "禁用", "reset": "正常"}
	if status, ok := expected[op]; ok && b["status"] != status {
		return "", "", "", problem(400, "operation "+op+" requires status "+status)
	}
	values := map[string][3]string{"enable": {"enable_account", "账号已启用", "账号启用失败"}, "disable": {"disable_account", "账号已禁用", "账号禁用失败"}, "reset": {"reset_account", "账号状态已重置", "账号状态重置失败"}, "update": {"update_account", "账号已更新", "账号更新失败"}}
	v := values[op]
	return v[0], v[1], v[2], nil
}
func (s *Server) startLocal(ctx context.Context, b Map, remove bool) (any, error) {
	legacy := "access_tokens"
	if remove {
		legacy = "tokens"
	}
	if b["selection"] == nil && len(stringsOf(b["account_ids"])) == 0 && len(stringsOf(b[legacy])) == 0 {
		return nil, problem(400, "account_ids is required")
	}
	action, success, failure := "delete_account", "账号已删除", ""
	patch := Map{}
	if !remove {
		if b["status"] == nil {
			return nil, problem(400, "no updates provided")
		}
		var e error
		action, success, failure, e = statusOperation(b)
		if e != nil {
			return nil, e
		}
		patch["status"] = b["status"]
	}
	targets, missing, e := s.targets(ctx, b, legacy, false)
	if e != nil {
		return nil, e
	}
	if len(targets) == 0 {
		return nil, problem(400, "no valid accounts selected")
	}
	ids := targetIDs(targets)
	labels := labelsFor(targets)
	jid, e := s.Operations.Start(len(targets)+len(missing), missing, action, func(jobCtx context.Context, jid string) (Map, error) {
		s.Operations.Stage(jid, "prepare_accounts", "准备账号")
		result, e := s.Accounts.Batch(jobCtx, ids, patch, remove)
		if e != nil {
			return nil, e
		}
		updated, removed := stringsOf(result["updated_ids"]), stringsOf(result["removed_ids"])
		missingNow := append([]string{}, missing...)
		present := map[string]bool{}
		for _, mid := range append(updated, removed...) {
			present[mid] = true
		}
		for _, mid := range ids {
			if !present[mid] {
				missingNow = append(missingNow, mid)
			}
		}
		if !remove {
			missingNow = append(missingNow, removed...)
		}
		errs := notFoundErrors(missingNow)
		events := operationEvents(action, updated, removed, errs, labels, success, failure, s.now())
		for _, target := range targets {
			mid := text(target["management_id"])
			current, _ := s.Accounts.Get(jobCtx, mid)
			var event Map
			for _, v := range events {
				if v["account_id"] == mid {
					event = v
					break
				}
			}
			if current == nil {
				status := "正常"
				if event["status"] == "failed" {
					status = "异常"
				}
				current = Map{"management_id": mid, "email": labels[mid], "status": status, "quota": 0}
			}
			s.Operations.Advance(jid, current, event)
		}
		counts := Map{"removed": len(removed)}
		if !remove {
			counts["updated"] = len(updated)
		}
		return s.Accounts.MutationPayload(jobCtx, counts, updated, removed, errs, events, false, false)
	})
	if e != nil {
		return nil, e
	}
	return Map{"progress_id": jid, "target_ids": ids, "errors": notFoundErrors(missing)}, nil
}
func (s *Server) startRemote(ctx context.Context, b Map, mode string) (any, error) {
	targets, missing, e := s.targets(ctx, b, "access_tokens", mode == "sync")
	if e != nil {
		return nil, e
	}
	if len(targets) == 0 {
		return nil, &detailedProblem{400, Map{"error": "no valid accounts selected", "errors": notFoundErrors(missing)}}
	}
	action := "sync_account"
	if mode == "refresh" {
		action = "refresh_access_token"
	}
	jid, e := s.Operations.Start(len(targets)+len(missing), missing, action, func(jobCtx context.Context, jid string) (Map, error) {
		result, e := s.maintain(jobCtx, targets, mode, jid)
		if e != nil {
			return nil, e
		}
		result["errors"] = append(notFoundErrors(missing), mapSlice(result["errors"])...)
		return result, nil
	})
	if e != nil {
		return nil, e
	}
	return Map{"progress_id": jid, "target_ids": targetIDs(targets), "errors": notFoundErrors(missing)}, nil
}
func (s *Server) bindGroup(ctx context.Context, r *http.Request, b, who Map) (any, error) {
	if b["selection"] == nil && len(stringsOf(b["account_ids"])) == 0 && len(stringsOf(b["access_tokens"])) == 0 {
		return nil, problem(400, "account_ids is required")
	}
	gid := slug(b["group_id"])
	if text(b["group_id"]) == "__ungrouped__" {
		gid = ""
	}
	if e := s.Accounts.ValidateGroup(ctx, gid, 404); e != nil {
		return nil, e
	}
	targets, missing, e := s.targets(ctx, b, "access_tokens", false)
	if e != nil {
		return nil, e
	}
	if len(targets) == 0 {
		return nil, problem(400, "no valid accounts selected")
	}
	result, e := s.Accounts.Batch(ctx, targetIDs(targets), Map{"group_id": gid}, false)
	if e != nil {
		return nil, e
	}
	updated, removed := stringsOf(result["updated_ids"]), stringsOf(result["removed_ids"])
	errs := notFoundErrors(append(missing, removed...))
	message := "账号组已更新"
	if gid == "" {
		message = "账号已移出账号组"
	}
	events := operationEvents("bind_account_group", updated, removed, errs, labelsFor(targets), message, "账号组更新失败", s.now())
	payload, e := s.Accounts.MutationPayload(ctx, Map{"updated": len(updated), "removed": len(removed)}, updated, removed, errs, events, false, true)
	if e != nil {
		return nil, e
	}
	groups, e := s.Accounts.GroupData(ctx)
	if e != nil {
		return nil, e
	}
	for k, v := range groups {
		payload[k] = v
	}
	payload["group_id"] = gid
	return payload, nil
}
func (s *Server) registerAccounts() {
	s.route("GET", "/api/accounts", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		page, e := queryInt(r, "page", 1, 1, 0)
		if e != nil {
			return nil, e
		}
		size, e := queryInt(r, "page_size", 500, 1, 500)
		if e != nil {
			return nil, e
		}
		q := r.URL.Query()
		return s.Accounts.Page(ctx, page, size, q.Get("keyword"), q.Get("status"), q.Get("group_id"))
	})
	s.route("POST", "/api/accounts/selection-preview", "admin", "AccountSelectionPreviewRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.Accounts.SelectionPreview(ctx, object(b["selection"]))
	})
	s.route("GET", "/api/accounts/{account_id}", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		item, e := s.accountProjection(ctx, r.PathValue("account_id"), true)
		return Map{"item": item}, e
	})
	for _, kind := range []string{"access-token", "refresh-token"} {
		kind := kind
		s.route("GET", "/api/accounts/{account_id}/"+kind, "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			m, e := s.Accounts.Get(ctx, r.PathValue("account_id"))
			if e != nil {
				return nil, e
			}
			if m == nil {
				return nil, problem(404, "account not found")
			}
			field := strings.ReplaceAll(kind, "-", "_")
			value := text(m[field])
			if value == "" {
				return nil, problem(404, strings.ReplaceAll(kind, "-", " ")+" not found")
			}
			raw, e := json.Marshal(Map{field: value})
			return &Download{Bytes: raw, ContentType: "application/json", Headers: map[string]string{"Cache-Control": "no-store"}}, e
		})
	}
	s.route("POST", "/api/accounts", "admin", "AccountCreateRequest", s.createAccounts)
	s.route("POST", "/api/accounts/update", "admin", "AccountUpdateRequest", s.updateAccount)
	s.route("DELETE", "/api/accounts", "admin", "AccountDeleteRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.startLocal(ctx, b, true) })
	s.route("POST", "/api/accounts/batch-update", "admin", "AccountBatchUpdateRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.startLocal(ctx, b, false)
	})
	s.route("POST", "/api/accounts/group", "admin", "AccountGroupBindRequest", s.bindGroup)
	s.route("GET", "/api/account-groups", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Accounts.GroupData(ctx) })
	s.route("POST", "/api/account-groups", "admin", "AccountGroupRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.Accounts.SaveGroup(ctx, b)
	})
	s.route("DELETE", "/api/account-groups/{group_id}", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		payload, updated, removed, e := s.Accounts.DeleteGroup(ctx, r.PathValue("group_id"))
		if e != nil {
			return nil, e
		}
		mutation, e := s.Accounts.MutationPayload(ctx, Map{}, updated, removed, nil, nil, true, false)
		if e != nil {
			return nil, e
		}
		for k, v := range mutation {
			payload[k] = v
		}
		return payload, nil
	})
	s.route("POST", "/api/accounts/sync", "admin", "AccountOperationRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.startRemote(ctx, b, "sync")
	})
	s.route("POST", "/api/accounts/refresh", "admin", "AccountOperationRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.startRemote(ctx, b, "sync")
	})
	s.route("POST", "/api/accounts/refresh-access-token", "admin", "AccountOperationRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.startRemote(ctx, b, "refresh")
	})
	for _, path := range []string{"/api/accounts/operations/{progress_id}", "/api/accounts/refresh/progress/{progress_id}"} {
		path := path
		s.route("GET", path, "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			return s.Operations.Progress(ctx, r.PathValue("progress_id"), strings.Contains(path, "/refresh/"))
		})
	}
	s.route("POST", "/api/accounts/export", "admin", "AccountExportRequest", s.exportAccounts)
	s.route("POST", "/api/accounts/import-cleanup", "admin", "AccountImportCleanupRequest", s.importCleanup)
}
