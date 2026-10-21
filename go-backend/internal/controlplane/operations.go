package controlplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

func operationEvents(action string, successIDs, removedIDs []string, errors []Map, labels map[string]string, successMessage, removedMessage string, now time.Time) []Map {
	errorIDs := map[string]bool{}
	for _, e := range errors {
		errorIDs[text(e["id"])] = true
	}
	events := []Map{}
	add := func(mid, status, message string) {
		label := labels[mid]
		if label == "" {
			label = mid
		}
		tone := map[string]string{"success": "success", "failed": "danger", "skipped": "warning", "info": "info"}[status]
		events = append(events, Map{"sequence": len(events) + 1, "timestamp": iso(now), "account_id": mid, "account_label": label, "action": action, "status": status, "tone": tone, "message": message})
	}
	for _, mid := range unique(successIDs) {
		if !errorIDs[mid] {
			add(mid, "success", successMessage)
		}
	}
	for _, mid := range unique(removedIDs) {
		if !errorIDs[mid] {
			status := "failed"
			if action == "delete_account" {
				status = "success"
			}
			message := removedMessage
			if message == "" {
				message = successMessage
			}
			add(mid, status, message)
		}
	}
	for _, e := range errors {
		code, message := text(orValue(e["code"], e["failure_code"])), text(orValue(e["message"], e["error"]))
		if code != "" && message != "" {
			message = code + " · " + message
		} else if message == "" {
			message = code
		}
		if message == "" {
			message = "account operation failed"
		}
		add(text(e["id"]), "failed", message)
	}
	if len(events) > 500 {
		events = events[len(events)-500:]
	}
	return events
}
func operationPresentation(progress Map, legacy bool) Map {
	result, _ := progress["result"].(map[string]any)
	total, processed := max(0, integer(progress["total"], 0)), max(0, integer(progress["processed"], 0))
	summary := []Map{}
	item := func(key, label string, value int, tone string) {
		m := Map{"key": key, "label": label, "value": max(0, value)}
		if tone != "" {
			m["tone"] = tone
		}
		summary = append(summary, m)
	}
	failed := 0
	if result != nil {
		failed = len(mapSlice(result["errors"]))
		if v, ok := result["synced"]; ok {
			item("synced", "同步", integer(v, 0), "")
		} else if v, ok := result["refreshed"]; ok {
			label := "刷新"
			if legacy {
				label = "同步"
			}
			item("refreshed", label, integer(v, 0), "")
		} else if v, ok := result["added"]; ok {
			item("added", "新增", integer(v, 0), "")
		} else if v, ok := result["updated"]; ok {
			item("updated", "更新", integer(v, 0), "")
		}
		tone := ""
		if failed > 0 {
			tone = "danger"
		}
		item("failed", "失败", failed, tone)
		item("skipped", "跳过", integer(result["skipped"], 0), "")
		removed := len(stringsOf(result["removed_ids"]))
		tone = ""
		if removed > 0 {
			tone = "warning"
		}
		item("removed", "移除", removed, tone)
	} else {
		for _, e := range mapSlice(progress["events"]) {
			if e["status"] == "failed" {
				failed++
			}
		}
		item("processed", "已处理", processed, "")
		item("remaining", "待处理", max(0, total-processed), "")
		item("total", "总数", total, "")
	}
	statusLabel, tone, message := "已完成", "success", ""
	switch {
	case truth(progress["error"]):
		statusLabel, tone, message = "失败", "danger", text(progress["error"])
	case !boolean(progress["done"], false):
		statusLabel, tone = text(progress["stage_label"]), "info"
		if statusLabel == "" {
			statusLabel = "处理中"
		}
		message = text(progress["stage_label"])
		if message == "" {
			message = fmt.Sprintf("已处理 %d / %d", processed, total)
		}
	case failed > 0:
		statusLabel, tone = "部分完成", "warning"
	}
	if message == "" {
		parts := []string{}
		for _, i := range summary {
			if integer(i["value"], 0) > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", i["label"], integer(i["value"], 0)))
			}
		}
		message = "任务完成"
		if failed > 0 {
			message = "任务部分完成"
		}
		if len(parts) > 0 {
			message += " · " + strings.Join(parts, " · ")
		}
	}
	return Map{"status_label": statusLabel, "tone": tone, "message": message, "summary_items": summary}
}
func (a *Accounts) MutationPayload(ctx context.Context, counts Map, updated, removed []string, errs, events []Map, include, presentation bool) (Map, error) {
	removed = unique(removed)
	removeSet := map[string]bool{}
	for _, s := range removed {
		removeSet[s] = true
	}
	next := []string{}
	for _, s := range unique(updated) {
		if !removeSet[s] {
			next = append(next, s)
		}
	}
	updated = next
	out := copyMap(counts)
	if _, ok := out["updated"]; ok {
		out["updated"] = len(updated)
	}
	if _, ok := out["removed"]; ok {
		out["removed"] = len(removed)
	}
	if errs == nil {
		errs = []Map{}
	}
	if events == nil {
		events = []Map{}
	}
	out["updated_ids"], out["removed_ids"], out["errors"], out["events"], out["items"] = updated, removed, errs, events, []Map{}
	if include {
		items, e := a.List(ctx)
		if e != nil {
			return nil, e
		}
		names, e := a.GroupNames(ctx)
		if e != nil {
			return nil, e
		}
		rows := []Map{}
		for _, mid := range updated {
			if m := byID(items, mid); m != nil {
				gid := text(m["group_id"])
				name := names[gid]
				if name == "" {
					name = gid
				}
				rows = append(rows, AccountRow(m, name, a.now()))
			}
		}
		out["items"] = rows
	}
	if presentation {
		total := map[string]bool{}
		for _, s := range append(append([]string{}, updated...), removed...) {
			total[s] = true
		}
		for _, err := range errs {
			if s := text(err["id"]); s != "" {
				total[s] = true
			}
		}
		for k, v := range operationPresentation(Map{"total": len(total), "processed": len(total), "done": true, "result": out}, false) {
			out[k] = v
		}
	}
	return out, nil
}
func labelsFor(items []Map) map[string]string {
	labels := map[string]string{}
	for _, m := range items {
		mid := text(m["management_id"])
		label := text(m["email"])
		if label == "" {
			label = mid
		}
		labels[mid] = label
	}
	return labels
}

type operationJob struct {
	value   Map
	updated time.Time
}
type Operations struct {
	accounts *Accounts
	now      func() time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	jobs     map[string]*operationJob
	wg       sync.WaitGroup
	closing  bool
}

func NewOperations(parent context.Context, a *Accounts) *Operations {
	ctx, cancel := context.WithCancel(parent)
	return &Operations{accounts: a, now: a.now, ctx: ctx, cancel: cancel, jobs: map[string]*operationJob{}}
}
func (o *Operations) pruneLocked() {
	now := o.now()
	for id, j := range o.jobs {
		ttl := time.Hour
		if boolean(j.value["done"], false) {
			ttl = 10 * time.Minute
		}
		if now.Sub(j.updated) > ttl {
			delete(o.jobs, id)
		}
	}
}
func operationID() string {
	s := id(16)
	return s[:8] + "-" + s[8:12] + "-4" + s[13:16] + "-a" + s[17:20] + "-" + s[20:]
}
func (o *Operations) Start(total int, missing []string, action string, fn func(context.Context, string) (Map, error)) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked()
	if o.closing {
		return "", problem(503, "account operations are shutting down")
	}
	active := 0
	for _, j := range o.jobs {
		if !boolean(j.value["done"], false) {
			active++
		}
	}
	if active >= 128 {
		return "", problem(429, "too many active account operations")
	}
	jid := operationID()
	j := &operationJob{value: Map{"total": total, "processed": len(missing), "done": false, "error": nil, "status_counts": Map{"正常": 0, "限流": 0, "异常": len(missing), "禁用": 0}, "total_quota": 0, "events": operationEvents(action, nil, nil, notFoundErrors(missing), nil, "", "", o.now())}, updated: o.now()}
	for _, event := range mapSlice(j.value["events"]) {
		event["message"] = "account_not_found · 账号不存在"
		event["account_id"], event["account_label"] = "[credential]", "[credential]"
	}
	o.jobs[jid] = j
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() {
			if recover() != nil {
				o.finish(jid, nil, problem(500, "account operation failed"))
			}
		}()
		result, e := fn(o.ctx, jid)
		o.finish(jid, result, e)
	}()
	return jid, nil
}
func (o *Operations) Stage(jid, stage, label string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if j := o.jobs[jid]; j != nil && !boolean(j.value["done"], false) {
		j.value["stage"], j.value["stage_label"] = stage, label
		j.updated = o.now()
	}
}
func (o *Operations) Advance(jid string, m Map, event Map) {
	o.mu.Lock()
	defer o.mu.Unlock()
	j := o.jobs[jid]
	if j == nil || boolean(j.value["done"], false) {
		return
	}
	j.value["processed"] = integer(j.value["processed"], 0) + 1
	status := "异常"
	quota := 0
	if m != nil {
		status = text(m["status"])
		quota = max(0, integer(m["quota"], 0))
	}
	counts := object(j.value["status_counts"])
	counts[status] = integer(counts[status], 0) + 1
	j.value["total_quota"] = integer(j.value["total_quota"], 0) + quota
	if event != nil {
		events := mapSlice(j.value["events"])
		sequence := 1
		if len(events) > 0 {
			sequence = integer(events[len(events)-1]["sequence"], 0) + 1
		}
		event = copyMap(event)
		event["sequence"] = sequence
		events = append(events, event)
		if len(events) > 500 {
			events = events[len(events)-500:]
		}
		j.value["events"] = events
	}
	j.updated = o.now()
}
func (o *Operations) finish(jid string, result Map, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	j := o.jobs[jid]
	if j == nil {
		return
	}
	j.value["done"], j.value["result"], j.value["stage"], j.value["stage_label"] = true, result, "completed", ""
	if err != nil {
		j.value["error"] = "账号操作失败"
	}
	j.updated = o.now()
}
func (o *Operations) Progress(ctx context.Context, jid string, legacy bool) (Map, error) {
	o.mu.Lock()
	o.pruneLocked()
	j := o.jobs[jid]
	if j == nil {
		o.mu.Unlock()
		return nil, problem(404, "progress not found")
	}
	view := copyMap(j.value)
	o.mu.Unlock()
	if raw, ok := view["result"].(map[string]any); ok {
		counts := Map{"skipped": max(0, integer(raw["skipped"], 0))}
		for _, key := range []string{"synced", "refreshed", "updated", "removed", "added"} {
			if v, ok := raw[key]; ok {
				target := key
				if legacy && key == "synced" {
					target = "refreshed"
				}
				counts[target] = max(0, integer(v, 0))
			}
		}
		result, e := o.accounts.MutationPayload(ctx, counts, stringsOf(raw["updated_ids"]), stringsOf(raw["removed_ids"]), mapSlice(raw["errors"]), mapSlice(raw["events"]), true, false)
		if e != nil {
			return nil, e
		}
		view["result"] = result
	} else {
		view["result"] = nil
	}
	for k, v := range operationPresentation(view, legacy) {
		view[k] = v
	}
	return view, nil
}
func (o *Operations) Close(ctx context.Context) error {
	o.mu.Lock()
	o.closing = true
	o.cancel()
	o.mu.Unlock()
	done := make(chan struct{})
	go func() { o.wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}
func orderIDs(ids, desired []string) []string {
	set := map[string]bool{}
	for _, s := range ids {
		set[s] = true
	}
	out := []string{}
	for _, s := range desired {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}
