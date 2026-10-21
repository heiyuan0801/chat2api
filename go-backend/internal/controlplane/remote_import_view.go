package controlplane

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var remoteColonProxy = regexp.MustCompile(`(?i)(^|[^a-z0-9])((?:\[[0-9a-f:.]+\]|localhost|[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?):\d{1,5}):[^:\s,;]+:[^\s,;]+`)
var remoteActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func importActive(raw any) bool {
	s := strings.ToLower(text(object(raw)["status"]))
	return s == "pending" || s == "running"
}
func importDiagnostic(value any, sources ...Map) string {
	s := text(value)
	secrets := []string{}
	var collect func(any)
	collect = func(v any) {
		switch item := v.(type) {
		case map[string]any:
			for key, x := range item {
				n := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
				switch n {
				case "accesstoken", "refreshtoken", "idtoken", "apikey", "secretkey", "password", "authorization", "token":
					if t := text(x); t != "" {
						secrets = append(secrets, t)
					}
				}
				collect(x)
			}
		case []any:
			for _, x := range item {
				collect(x)
			}
		case []Map:
			for _, x := range item {
				collect(x)
			}
		}
	}
	for _, m := range sources {
		collect(m)
	}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, "[credential]")
	}
	s = remoteColonProxy.ReplaceAllStringFunc(s, func(value string) string {
		parts := remoteColonProxy.FindStringSubmatch(value)
		if len(parts) < 3 {
			return value
		}
		host := parts[2][:strings.LastIndex(parts[2], ":")]
		alpha := strings.ContainsAny(strings.ToLower(host), "abcdefghijklmnopqrstuvwxyz")
		if !alpha && !strings.Contains(host, ".") && !strings.HasPrefix(host, "[") {
			return value
		}
		return parts[1] + parts[2] + ":***:***"
	})
	return diagnostic(s, nil, 2000)
}
func normalizeImportJob(raw any, fail bool, now time.Time, sources ...Map) Map {
	job, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	status := text(job["status"])
	if status == "" {
		status = "failed"
	}
	if fail && importActive(job) {
		status = "failed"
	}
	stage := strings.ToLower(text(job["stage"]))
	labels := map[string]string{"read_credentials": "读取凭据", "save_accounts": "保存账号", "sync_accounts": "同步账号与额度", "completed": "完成"}
	if _, ok := labels[stage]; !ok {
		stage = "read_credentials"
	}
	terminal := status == "completed" || status == "failed"
	if terminal {
		stage = "completed"
	}
	errors := []Map{}
	rawErrors := dicts(job["errors"])
	for _, e := range rawErrors {
		st := strings.ToLower(text(e["stage"]))
		if st != "fetch" && st != "sync" {
			st = "fetch"
		}
		name := text(orValue(e["name"], orValue(e["id"], orValue(e["email"], orValue(e["token"], "remote account")))))
		message := text(orValue(e["error"], orValue(e["message"], "unknown error")))
		errors = append(errors, Map{"stage": st, "name": importDiagnosticLimit(name, 500, sources...), "error": importDiagnostic(message, sources...)})
	}
	failedRaw := job["failed_total"]
	if failedRaw == nil {
		failedRaw = job["failed"]
	}
	failed := max(0, integer(failedRaw, 0), len(rawErrors))
	if len(errors) > 20 {
		errors = errors[len(errors)-20:]
	}
	events := []Map{}
	for i, e := range dicts(job["events"]) {
		st := strings.ToLower(text(e["status"]))
		tone := map[string]string{"success": "success", "failed": "danger", "skipped": "warning", "info": "info"}[st]
		if tone == "" {
			st, tone = "info", "info"
		}
		action := strings.ToLower(text(e["action"]))
		if !remoteActionPattern.MatchString(action) {
			action = "account_operation"
		}
		timestamp := text(e["timestamp"])
		if timestamp == "" {
			timestamp = iso(now)
		}
		events = append(events, Map{"sequence": max(1, integer(e["sequence"], i+1)), "timestamp": importDiagnosticLimit(timestamp, 80, sources...), "account_id": importDiagnosticLimit(e["account_id"], 160, sources...), "account_label": importDiagnosticLimit(e["account_label"], 240, sources...), "action": action, "status": st, "tone": tone, "message": importDiagnosticLimit(e["message"], 500, sources...)})
	}
	if len(events) > 500 {
		events = events[len(events)-500:]
	}
	created := text(job["created_at"])
	if created == "" {
		created = iso(now)
	}
	updated := text(job["updated_at"])
	if updated == "" {
		updated = created
	}
	total := max(0, integer(job["total"], 0))
	completed := max(0, min(total, integer(job["completed"], 0)))
	stageTotal := max(0, integer(orValue(job["stage_total"], total), 0))
	stageCompleted := max(0, min(stageTotal, integer(orValue(job["stage_completed"], completed), 0)))
	if status == "completed" {
		stageCompleted = stageTotal
	}
	progressTotal, progressCompleted := stageTotal, stageCompleted
	if terminal {
		progressTotal, progressCompleted = total, completed
	}
	if status == "completed" {
		progressCompleted = total
	}
	errorText := ""
	if status == "failed" {
		if len(errors) > 0 {
			errorText = text(errors[0]["error"])
		}
		if errorText == "" {
			errorText = "导入失败"
		}
	}
	added, skipped, synced := max(0, integer(job["added"], 0)), max(0, integer(job["skipped"], 0)), max(0, integer(importSynced(job), 0))
	summary := []Map{{"key": "added", "label": "新增", "value": added}, {"key": "skipped", "label": "更新 / 跳过", "value": skipped}, {"key": "synced", "label": "同步", "value": synced}, {"key": "failed", "label": "失败", "value": failed}}
	if failed > 0 {
		summary[3]["tone"] = "danger"
	}
	label, tone := "已完成", "success"
	if errorText != "" {
		label, tone = "失败", "danger"
	} else if !terminal {
		label, tone = labels[stage], "info"
	} else if failed > 0 {
		label, tone = "部分完成", "warning"
	}
	message := ""
	if terminal {
		if errorText != "" {
			message = "导入失败"
			if errorText != "导入失败" {
				message += " · " + errorText
			}
		} else {
			message = "导入完成"
			if failed > 0 {
				message = "导入部分完成"
			}
			parts := []string{}
			for _, s := range summary {
				if integer(s["value"], 0) > 0 {
					parts = append(parts, fmt.Sprintf("%s %d", text(s["label"]), integer(s["value"], 0)))
				}
			}
			if len(parts) > 0 {
				message += " · " + strings.Join(parts, " · ")
			}
		}
	}
	resultTone := tone
	if !terminal {
		resultTone = "info"
	}
	jobID := text(job["job_id"])
	if jobID == "" {
		jobID = id(16)
	}
	return Map{"job_id": jobID, "status": status, "created_at": created, "updated_at": updated, "stage": stage, "stage_label": labels[stage], "stage_total": stageTotal, "stage_completed": stageCompleted, "terminal": terminal, "progress_total": progressTotal, "progress_completed": progressCompleted, "status_label": label, "tone": tone, "error": errorText, "summary_items": summary, "result_message": message, "result_tone": resultTone, "total": total, "completed": completed, "added": added, "skipped": skipped, "synced": synced, "failed": failed, "failed_total": failed, "errors": errors, "events": events}
}
func normalizeImportConfig(provider string, raw Map, fail bool, now time.Time) Map {
	out := Map{}
	fields := []string{"id", "name", "base_url", "secret_key"}
	if provider == "sub2api" {
		fields = []string{"id", "name", "base_url", "email", "password", "api_key", "group_id"}
	}
	for _, field := range fields {
		out[field] = text(raw[field])
	}
	if out["id"] == "" {
		out["id"] = id(6)
	}
	out["import_job"] = normalizeImportJob(raw["import_job"], fail, now, out)
	return out
}
func sanitizeImportConfig(provider string, raw Map) Map {
	out := copyMap(raw)
	delete(out, "secret_key")
	delete(out, "password")
	delete(out, "api_key")
	if provider == "sub2api" {
		out["has_api_key"] = text(raw["api_key"]) != ""
	}
	return out
}

func importSynced(job Map) any {
	if v, ok := job["synced"]; ok {
		return v
	}
	return job["refreshed"]
}
func importDiagnosticLimit(value any, limit int, sources ...Map) string {
	s := importDiagnostic(value, sources...)
	r := []rune(s)
	if limit > 0 && len(r) > limit {
		return string(r[:limit]) + "..."
	}
	return s
}
