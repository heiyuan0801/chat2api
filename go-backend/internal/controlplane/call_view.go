package controlplane

import (
	"context"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"net/http"
	"slices"
	"strings"
)

var callFailureLabels = map[string]string{"upstream_error": "上游请求失败", "internal_error": "内部处理异常", "upstream_unavailable": "上游服务暂不可用", "upstream_connection_failed": "无法连接上游", "upstream_connection_timeout": "上游连接超时", "upstream_rate_limited": "上游服务限流", "image_poll_timeout": "等待图片结果超时", "image_stream_timeout": "上游图片流超时", "image_stream_interrupted": "上游图片流中断", "image_tool_error": "图片工具异常", "image_quota_exhausted": "图片额度已用尽", "file_upload_throttled": "参考图上传受限", "auth_invalid": "账号登录态失效", "content_policy_violation": "内容安全策略拒绝", "invalid_image_input": "图片输入无效", "upstream_text_reply": "上游仅返回文本", "no_image_generated": "未生成图片", "unsupported_model": "模型不支持生图", "image_download_failed": "图片下载失败", "task_interrupted": "图片任务被中断", "no_available_account": "暂无可用账号", "insufficient_quota": "图片额度不足"}
var callBusinessLabels = map[string]string{"account": "账号操作", "image_generation": "文生图", "image_edit": "图生图", "image_chat": "对话生图", "chat": "对话", "responses": "响应", "messages": "消息", "search": "搜索", "file": "文件"}

func callDetail(item Map) Map {
	if d, ok := item["detail"].(map[string]any); ok {
		return d
	}
	return item
}
func callURLs(item Map) []string {
	out := []string{}
	d := callDetail(item)
	for _, k := range []string{"image_urls", "urls", "result_urls", "_image_urls", "data"} {
		for _, v := range list(d[k]) {
			if m, ok := v.(map[string]any); ok {
				v = m["url"]
			}
			u := callClean(v)
			if callPublicURL(u) && !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	}
	return out
}
func callBusiness(item Map) string {
	endpoint := strings.ToLower(strings.TrimRight(callClean(callValue(item, "endpoint")), "/"))
	model := strings.ToLower(callClean(callValue(item, "model")))
	switch {
	case strings.EqualFold(callClean(orValue(item["type"], callValue(item, "type"))), "account"):
		return "account"
	case strings.HasSuffix(endpoint, "/images/generations"):
		return "image_generation"
	case strings.HasSuffix(endpoint, "/images/edits"):
		return "image_edit"
	case truth(callValue(item, "image_request")):
		return "image_chat"
	case strings.HasSuffix(endpoint, "/chat/completions"):
		if strings.Contains(model, "image") {
			return "image_chat"
		}
		return "chat"
	case strings.HasSuffix(endpoint, "/responses"):
		return "responses"
	case strings.HasSuffix(endpoint, "/messages"):
		return "messages"
	case strings.Contains(endpoint, "search"):
		return "search"
	case strings.Contains(endpoint, "file"):
		return "file"
	}
	return "other"
}
func callAttempts(item Map) []Map {
	out := []Map{}
	for _, v := range dicts(callValue(item, "image_attempts")) {
		a := copyMap(v)
		a["slot"] = max(1, callInt(a["slot"]))
		a["attempt"] = max(1, callInt(a["attempt"]))
		if b, ok := a["switched_account"]; ok {
			if parsed := optionalBool(b); parsed != nil {
				a["switched_account"] = *parsed
			} else {
				a["switched_account"] = nil
			}
		}
		out = append(out, a)
	}
	return out
}
func callImages(item Map) (int, int, int, string) {
	d := callDetail(item)
	explicit := func(k string) (int, bool) {
		v, ok := d[k]
		if !ok || v == nil || v == "" {
			v, ok = item[k]
		}
		return callInt(v), ok && v != nil && v != ""
	}
	req, _ := explicit("image_requested_count")
	req = max(req, callInt(object(callValue(item, "request_meta"))["n"]))
	attempts := callAttempts(item)
	slots := map[int]bool{}
	for _, a := range attempts {
		req = max(req, callInt(a["slot"]))
		if strings.EqualFold(callClean(a["status"]), "success") {
			slots[callInt(a["slot"])] = true
		}
	}
	for _, v := range object(object(callValue(item, "monitor"))["images"]) {
		req = max(req, callInt(object(v)["total"]))
	}
	count := max(callInt(callValue(item, "result_data_count")), callInt(callValue(item, "result_url_count")), len(callURLs(item)))
	success, explicitSuccess := explicit("image_succeeded_count")
	if !explicitSuccess {
		success = max(len(slots), count)
	}
	req = max(req, success)
	failed, explicitFailed := explicit("image_failed_count")
	if !explicitFailed {
		failed = max(0, req-success)
	}
	req = max(req, success+failed)
	status := strings.ToLower(callClean(callValue(item, "image_result_status")))
	if !slices.Contains([]string{"success", "failed", "partial_success"}, status) {
		switch {
		case success > 0 && failed > 0:
			status = "partial_success"
		case success > 0:
			status = "success"
		case req > 0:
			status = "failed"
		default:
			status = ""
		}
	}
	if !slices.Contains([]string{"image_generation", "image_edit", "image_chat"}, callBusiness(item)) {
		er, _ := explicit("image_requested_count")
		es, _ := explicit("image_succeeded_count")
		ef, _ := explicit("image_failed_count")
		if er == 0 && es == 0 && ef == 0 && len(attempts) == 0 && count == 0 {
			return 0, 0, 0, ""
		}
	}
	return req, success, failed, status
}
func normalizeCallAccount(s string) string {
	for _, group := range []struct {
		label  string
		values []string
	}{{"正常", []string{"正常", "normal", "ready", "success", "completed", "complete", "done"}}, {"限流", []string{"限流", "limited", "rate_limited", "cooling", "backoff"}}, {"异常", []string{"异常", "abnormal", "invalid", "error", "failed", "fail", "incomplete"}}, {"禁用", []string{"禁用", "disabled"}}} {
		if slices.Contains(group.values, strings.ToLower(s)) {
			return group.label
		}
	}
	return s
}
func callOutcome(item Map) string {
	status := strings.ToLower(callClean(callValue(item, "status")))
	code := strings.ToLower(callClean(orValue(callValue(item, "error_code"), callValue(item, "failure_code"))))
	httpCode := callInt(callValue(item, "status_code"))
	req, success, failed, result := callImages(item)
	if callBusiness(item) == "account" {
		a := normalizeCallAccount(status)
		if httpCode == 429 || a == "限流" {
			return "rate_limited"
		}
		if httpCode >= 400 || a == "异常" {
			return "failed"
		}
		if a == "正常" {
			return "success"
		}
		if a == "禁用" {
			return "unknown"
		}
	}
	if status == "partial_success" || result == "partial_success" || req > 0 && success > 0 && failed > 0 {
		return "partial_success"
	}
	if status == "text_review" || httpCode == 400 || callTextFailure(code) {
		return "text_review"
	}
	if slices.Contains([]string{"success", "completed", "complete", "done"}, status) {
		return "success"
	}
	if httpCode == 429 || callRateFailure(status) || callRateFailure(code) {
		return "rate_limited"
	}
	if httpCode >= 400 || slices.Contains([]string{"failed", "fail", "error", "limited", "rate_limited", "限流"}, status) || callClean(callValue(item, "error")) != "" || code != "" {
		return "failed"
	}
	return "unknown"
}
func CallTimings(item Map) Map {
	d := callDetail(item)
	m := object(d["monitor"])
	sources := []Map{object(d["perf"]), object(d["metrics"]), object(m["metrics"])}
	for _, v := range object(m["images"]) {
		sources = append(sources, object(object(v)["metrics"]))
	}
	out := Map{}
	for _, s := range sources {
		for k, v := range s {
			if n := callInt(v); strings.HasSuffix(k, "_ms") && n > 0 {
				out[k] = max(callInt(out[k]), n)
			}
		}
	}
	return out
}
func callDuration(item Map) int {
	if n := callInt(callValue(item, "duration_ms")); n > 0 {
		return n
	}
	m := CallTimings(item)
	return max(callInt(m["total_ms"]), callInt(m["handler_exec_ms"]), callInt(m["stream_ms"]))
}
func callSwitchCount(item Map) int {
	n := 0
	for _, a := range callAttempts(item) {
		if boolean(a["switched_account"], false) {
			n++
		}
	}
	return max(n, callInt(callValue(item, "switch_count")), callInt(callValue(item, "image_account_switch_count")))
}
func callTypeLabel(v string) string {
	if v == "call" {
		return "调用日志"
	}
	if v == "account" {
		return "账号日志"
	}
	if v == "" {
		return "日志"
	}
	return v
}
func callStatus(s Map) Map {
	display := callClean(s["display_status"])
	if s["business"] == "account" && display != "" {
		tone := map[string]string{"正常": "success", "异常": "danger", "限流": "warning", "禁用": "muted"}[display]
		if tone == "" {
			tone = "muted"
		}
		return Map{"label": display, "tone": tone}
	}
	return RequestStatus(callClean(s["outcome"]), display)
}
func CallSummary(item Map) Map {
	req, success, failed, result := callImages(item)
	attempts := callAttempts(item)
	switches := callSwitchCount(item)
	outcome := callOutcome(item)
	business := callBusiness(item)
	display := callClean(callValue(item, "status"))
	if business == "account" {
		display = normalizeCallAccount(display)
	}
	err := callClean(orValue(callValue(item, "public_error"), callValue(item, "error")))
	if len([]rune(err)) > 1000 {
		err = string([]rune(err)[:1000]) + "..."
	}
	urls := callURLs(item)
	preview := ""
	if len(urls) > 0 {
		preview = urls[0]
	}
	s := Map{"id": callClean(orValue(item["id"], callValue(item, "call_id"))), "time": callClean(orValue(item["time"], callValue(item, "started_at"))), "type": callClean(orValue(item["type"], "call")), "summary": callClean(item["summary"]), "business": business, "outcome": outcome, "display_status": display, "duration_ms": callDuration(item), "status_code": callInt(callValue(item, "status_code")), "error_code": callClean(orValue(callValue(item, "error_code"), callValue(item, "failure_code"))), "public_error": err, "image_requested_count": req, "image_succeeded_count": success, "image_failed_count": failed, "image_result_status": result, "preview_image_url": preview, "attempt_count": len(attempts), "switch_count": switches, "recovered_after_switch": switches > 0 && (outcome == "success" || outcome == "partial_success")}
	for _, k := range []string{"endpoint", "model", "started_at", "ended_at", "key_id", "key_name", "role", "account_email", "conversation_id"} {
		s[k] = callClean(callValue(item, k))
	}
	s["presentation"] = callPresentation(s, attempts, callDetail(item))
	return s
}
func callResultText(s Map) string {
	outcome := text(s["outcome"])
	req, success := callInt(s["image_requested_count"]), callInt(s["image_succeeded_count"])
	summary, err := callClean(s["summary"]), callClean(s["public_error"])
	if outcome == "partial_success" {
		return fmt.Sprintf("生成 %d/%d 张图片", success, req)
	}
	if outcome == "text_review" {
		return "上游返回文本"
	}
	if outcome == "failed" || outcome == "rate_limited" {
		return callClean(orValue(err, orValue(callFailureLabels[callClean(s["error_code"])], orValue(summary, "调用失败"))))
	}
	if success > 0 {
		if req > 1 {
			return fmt.Sprintf("生成 %d/%d 张图片", success, req)
		}
		return fmt.Sprintf("生成 %d 张图片", success)
	}
	if outcome == "success" {
		switch text(s["business"]) {
		case "image_generation", "image_edit", "image_chat":
			return "图片生成完成"
		case "search":
			return "搜索完成"
		case "chat", "responses", "messages":
			return "文本响应完成"
		case "account":
			return callClean(orValue(summary, "账号操作完成"))
		default:
			return "调用完成"
		}
	}
	return callClean(orValue(summary, orValue(err, "调用完成")))
}
func attemptTimings(a Map) Map {
	out := Map{}
	for k, v := range object(object(a["monitor"])["metrics"]) {
		if n := callInt(v); strings.HasSuffix(k, "_ms") && n > 0 {
			out[k] = n
		}
	}
	return out
}
func attemptDuration(a Map) int {
	if n := callInt(a["duration_ms"]); n > 0 {
		return n
	}
	m := attemptTimings(a)
	return max(callInt(m["total_ms"]), callInt(m["stream_ms"]), callInt(m["handler_exec_ms"]))
}
func callPresentation(s Map, attempts []Map, detail Map) Map {
	task, ac := callInt(s["image_requested_count"]), callInt(s["attempt_count"])
	caller := callClean(orValue(s["key_name"], s["key_id"]))
	parts := []string{}
	if task > 0 {
		parts = append(parts, fmt.Sprintf("%d 个任务", task))
	}
	if ac > 0 {
		parts = append(parts, fmt.Sprintf("%d 次尝试", ac))
	}
	primary := strings.Join(parts, " · ")
	if primary == "" {
		primary = callClean(orValue(caller, "-"))
	}
	secondary := ""
	if task > 0 {
		secondary = caller
	}
	outcome := text(s["outcome"])
	diagnostics := []string{}
	if outcome == "failed" || outcome == "rate_limited" {
		if n := callInt(s["status_code"]); n > 0 {
			diagnostics = append(diagnostics, fmt.Sprintf("HTTP %d", n))
		}
		if code := callClean(s["error_code"]); code != "" {
			diagnostics = append(diagnostics, code)
		}
	}
	summary := callClean(s["summary"])
	if outcome == "text_review" {
		summary = strings.NewReplacer("流式调用失败", "文本", "调用失败", "文本").Replace(summary)
	}
	if summary == "" {
		summary = callClean(s["public_error"])
	}
	meta := object(detail["request_meta"])
	parameters := []string{}
	for _, p := range [][2]string{{"size", "请求"}, {"quality", "质量"}, {"response_format", "返回"}} {
		if v := callClean(meta[p[0]]); v != "" {
			parameters = append(parameters, p[1]+" "+v)
		}
	}
	res := unique(RequestImageResolutions(detail))
	bySlot := map[int][]int{}
	slots := map[int]bool{}
	for _, a := range attempts {
		slot := callInt(a["slot"])
		slots[slot] = true
		if n := attemptDuration(a); n > 0 {
			bySlot[slot] = append(bySlot[slot], n)
		}
	}
	ordered := []int{}
	for k := range bySlot {
		ordered = append(ordered, k)
	}
	slices.Sort(ordered)
	breakdowns := []string{}
	for _, k := range ordered {
		if len(bySlot[k]) < 2 {
			continue
		}
		ps := []string{}
		for _, n := range bySlot[k] {
			ps = append(ps, RequestDuration(n))
		}
		expr := strings.Join(ps, " + ")
		if len(slots) > 1 {
			expr = fmt.Sprintf("图 %d：%s", k, expr)
		}
		breakdowns = append(breakdowns, expr)
	}
	breakdown := ""
	if len(breakdowns) > 0 {
		breakdown = "(" + strings.Join(breakdowns, "；") + ")"
	}
	tone := "success"
	if outcome == "failed" {
		tone = "danger"
	} else if callInt(s["duration_ms"]) >= 60000 {
		tone = "warning"
	}
	kind := callBusinessLabels[text(s["business"])]
	if kind == "" {
		kind = callTypeLabel(text(s["type"]))
	}
	if s["type"] == "account" {
		kind = ""
	}
	return Map{"request": Map{"kind": kind, "primary": orValue(s["model"], callTypeLabel(text(s["type"]))), "secondary": s["endpoint"], "parameters": strings.Join(parameters, " · ")}, "execution": Map{"primary": primary, "secondary": secondary}, "status": callStatus(s), "result": Map{"text": callResultText(s), "diagnostics": strings.Join(diagnostics, " · "), "resolution": strings.Join(res, " / ")}, "summary_text": summary, "duration": Map{"text": RequestDuration(s["duration_ms"]), "breakdown": breakdown, "tone": tone}, "is_failure": outcome == "failed"}
}
func CallAttemptSummary(a Map) Map {
	outcome := callOutcome(a)
	code := callClean(orValue(a["error_code"], a["failure_code"]))
	scope := strings.ToLower(callClean(a["failure_scope"]))
	result := "failed"
	if scope == "delivery" || scope == "" && strings.EqualFold(code, "image_download_failed") {
		result = "generated_but_delivery_failed"
	} else if outcome == "success" {
		result = "success"
	}
	timings := attemptTimings(a)
	monitor := object(a["monitor"])
	out := Map{"slot": max(1, callInt(a["slot"])), "attempt": max(1, callInt(a["attempt"])), "account_email": callClean(a["account_email"]), "conversation_id": callClean(a["conversation_id"]), "status": callClean(a["status"]), "outcome": outcome, "result_status": result, "duration_ms": attemptDuration(a), "status_code": callInt(a["status_code"]), "error_code": code, "error_label": callFailureLabels[code], "public_error": callClean(orValue(a["public_error"], a["error"])), "upstream_error": callClean(orValue(a["upstream_error"], a["raw_upstream_error"])), "upstream_text": firstCallText(a, []string{"upstream_text", "raw_upstream_message", "upstream_message", "upstream_message_preview"}), "switched_account": nil, "timings_ms": timings, "monitor": monitor}
	if b := optionalBool(a["switched_account"]); b != nil {
		out["switched_account"] = *b
	}
	status := RequestStatus(outcome, "")
	if result == "generated_but_delivery_failed" {
		status = Map{"label": "生成成功", "tone": "warning"}
	}
	failure := callFailureLabels[code]
	if failure == "" {
		failure = "生成失败"
		if result == "generated_but_delivery_failed" {
			failure = "结果交付失败"
		}
	}
	switchLabel, httpText, marker := "", "", "danger"
	if out["switched_account"] == true {
		switchLabel = "切换账号"
	}
	if n := callInt(out["status_code"]); n > 0 {
		httpText = fmt.Sprintf("HTTP %d", n)
	}
	if result == "success" {
		marker = "success"
	}
	out["presentation"] = Map{"status": status, "failure_label": failure, "marker_tone": marker, "switch_label": switchLabel, "error_code_text": code, "status_code_text": httpText, "show_failure": result != "success", "show_error_details": out["public_error"] != "" || out["upstream_error"] != "" || out["upstream_text"] != "", "timeline": RequestTimeline(timings, monitor["events"], 1, nil)}
	return out
}
func firstCallText(d Map, keys []string) string {
	for _, k := range keys {
		if v := callClean(d[k]); v != "" {
			return v
		}
	}
	return ""
}
func CallDetail(item Map) Map {
	d := callDetail(item)
	m := object(d["monitor"])
	s := CallSummary(item)
	attempts := []Map{}
	delivery := false
	for _, a := range callAttempts(item) {
		v := CallAttemptSummary(a)
		delivery = delivery || v["result_status"] == "generated_but_delivery_failed"
		attempts = append(attempts, v)
	}
	t := CallTimings(item)
	urls := callURLs(item)
	requested := callInt(s["image_requested_count"])
	breakdown := len(attempts) > 0 && (len(attempts) > 1 || requested > 1 || delivery)
	core := RequestDetailCore(s, d, m, t, m["events"], len(urls), object(d["request_shape"]), len(attempts) > 1 || requested > 1, breakdown)
	groups := []Map{}
	maxSlot := requested
	for _, a := range attempts {
		maxSlot = max(maxSlot, callInt(a["slot"]))
	}
	if breakdown {
		for slot := 1; slot <= maxSlot; slot++ {
			count, switches := 0, 0
			success, delivered := false, false
			for _, a := range attempts {
				if callInt(a["slot"]) != slot {
					continue
				}
				count++
				if a["switched_account"] == true {
					switches++
				}
				success = success || a["result_status"] == "success"
				delivered = delivered || a["result_status"] == "generated_but_delivery_failed"
			}
			status := Map{"label": "未记录", "tone": "muted"}
			if success {
				status = Map{"label": "成功", "tone": "success"}
			} else if delivered {
				status = Map{"label": "生成成功", "tone": "warning"}
			} else if count > 0 {
				status = Map{"label": "失败", "tone": "danger"}
			}
			sw := ""
			if switches > 0 {
				sw = fmt.Sprintf("切换 %d 次", switches)
			}
			groups = append(groups, Map{"slot": slot, "slot_label": fmt.Sprintf("图片 %d", slot), "attempt_count": count, "attempt_text": fmt.Sprintf("%d 次尝试", count), "switch_count": switches, "switch_text": sw, "status": status})
		}
	}
	core["has_attempt_breakdown"] = breakdown
	core["attempt_groups"] = groups
	s["request_text"] = callClean(d["request_text"])
	s["request_text_full"] = callClean(d["request_text_full"])
	s["request_text_truncated"] = boolean(d["request_text_truncated"], false)
	s["request_shape"] = object(d["request_shape"])
	s["request_meta"] = object(d["request_meta"])
	s["upstream_error"] = firstCallText(merge(m, d), []string{"upstream_error", "raw_upstream_error"})
	s["upstream_text"] = firstCallText(d, []string{"upstream_text", "raw_upstream_message", "upstream_message", "upstream_message_preview", "upstream_preview"})
	if s["upstream_text"] == "" {
		s["upstream_text"] = firstCallText(m, []string{"upstream_text", "raw_upstream_message", "upstream_message", "upstream_message_preview", "upstream_preview"})
	}
	s["image_urls"] = urls
	s["attempts"] = attempts
	s["timings_ms"] = t
	s["perf"] = object(d["perf"])
	s["metrics"] = object(d["metrics"])
	s["monitor"] = m
	s["detail_presentation"] = core
	s["raw_detail"] = d
	return s
}
func (s *Server) registerLogs() {
	repo := repository.NewCallRecords(s.DB)
	s.route("GET", "/api/logs", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		limit, e := queryInt(r, "limit", 200, 1, 20000)
		if e != nil {
			return nil, e
		}
		offset, e := queryInt(r, "offset", 0, 0, 0)
		if e != nil {
			return nil, e
		}
		rawQuery := func(k string) string {
			values := r.URL.Query()[k]
			if len(values) == 0 {
				return ""
			}
			return values[len(values)-1]
		}
		v := func(k string) string { return strings.TrimSpace(rawQuery(k)) }
		status := rawQuery("status")
		if !slices.Contains([]string{"", "success", "failed", "limited"}, status) {
			return nil, &ValidationError{[]Map{validationIssue("literal_error", "Input should be '', 'success', 'failed' or 'limited'", []any{"query", "status"}, status, Map{"expected": "'', 'success', 'failed' or 'limited'"})}}
		}
		outcomes := []string{}
		switch status {
		case "success":
			outcomes = []string{"success", "partial_success"}
		case "failed":
			outcomes = []string{"failed"}
		case "limited":
			outcomes = []string{"rate_limited"}
		}
		query := repository.CallQuery{Type: v("type"), StartDate: v("start_date"), EndDate: v("end_date"), Endpoint: v("endpoint"), Model: v("model"), Account: v("account"), ConversationID: v("conversation_id"), Search: v("search"), Outcomes: outcomes}
		page, e := repo.List(ctx, query, limit, offset)
		if e != nil {
			return nil, e
		}
		items := []Map{}
		for _, a := range page.Items {
			items = append(items, CallSummary(a))
		}
		statuses := page.Facets["statuses"]
		scope := "all"
		if query.Type != "" {
			scope = "type"
		}
		if query.StartDate != "" || query.EndDate != "" || status != "" || query.Endpoint != "" || query.Model != "" || query.Account != "" || query.ConversationID != "" || query.Search != "" {
			scope = "filtered"
		}
		return Map{"items": items, "total": page.Total, "limit": limit, "offset": offset, "has_more": offset+len(items) < page.Total, "facets_scope": "filtered", "stats_scope": "filtered", "total_scope": scope, "facets": page.Facets, "stats": Map{"total": page.Total, "success": statuses["success"] + statuses["partial_success"], "text_review": statuses["text_review"], "failed": statuses["failed"], "limited": statuses["rate_limited"], "image": page.ImageCount}}, nil
	})
	s.route("GET", "/api/logs/{log_id}", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		v, e := repo.Get(ctx, r.PathValue("log_id"))
		if e != nil {
			return nil, e
		}
		if v == nil {
			return nil, problem(404, "log not found")
		}
		return CallDetail(v), nil
	})
	s.route("POST", "/api/logs/delete", "admin", "LogDeleteRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		if e := s.Dashboard.Sync(ctx); e != nil {
			return nil, e
		}
		n, e := repo.Delete(ctx, stringsOf(b["ids"]))
		return Map{"removed": n}, e
	})
}
