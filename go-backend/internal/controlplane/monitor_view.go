package controlplane

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

//go:embed monitor_spec.json
var monitorSpecBytes []byte
var monitorSpec = func() Map {
	var out Map
	if json.Unmarshal(monitorSpecBytes, &out) != nil {
		panic("invalid monitor constants")
	}
	return out
}()

func monitorInt(value any) int { return max(0, integer(value, 0)) }
func monitorMetric(value any) Map {
	out := Map{}
	for key, v := range object(value) {
		if strings.HasSuffix(key, "_ms") {
			out[key] = monitorInt(v)
		}
	}
	return out
}
func monitorTimings(row Map) Map {
	out := monitorMetric(row["metrics"])
	for key, v := range monitorMetric(row["perf"]) {
		out[key] = max(monitorInt(out[key]), monitorInt(v))
	}
	return out
}
func monitorMS(value any) string {
	n := monitorInt(value)
	if n == 0 {
		return "-"
	}
	if n >= 60000 {
		return fmt.Sprintf("%.1fm", float64(n)/60000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fs", float64(n)/1000)
	}
	return fmt.Sprintf("%dms", n)
}
func monitorHas(values any, key string) bool {
	for _, v := range stringsOf(values) {
		if v == key {
			return true
		}
	}
	return false
}
func monitorFields(source Map, fields string) Map {
	out := Map{}
	for _, key := range stringsOf(monitorSpec[fields]) {
		value, ok := source[key]
		if !ok || value == nil {
			continue
		}
		switch {
		case monitorHas(monitorSpec["INTEGER_FIELDS"], key):
			out[key] = monitorInt(value)
		case monitorHas(monitorSpec["BOOLEAN_FIELDS"], key):
			out[key] = truth(value)
		default:
			out[key] = fmt.Sprint(value)
		}
	}
	return out
}
func monitorEgress(row Map) Map {
	source := text(row["proxy_source"])
	if source == "" {
		source = "direct"
	}
	label := RequestProxySource(source)
	group, node, nodeID := text(row["proxy_group_id"]), text(row["proxy_node_name"]), text(row["proxy_node_id"])
	if node == "" {
		node = nodeID
	}
	parts := []string{}
	if group != "" {
		parts = append(parts, group)
	}
	if node != "" {
		parts = append(parts, node)
	}
	nodeLabel := strings.Join(parts, "/")
	rawLabel := text(row["egress_label"])
	if nodeLabel == "" && rawLabel != "" && rawLabel != "direct" && rawLabel != source && rawLabel != source+"_profile" && !strings.HasPrefix(rawLabel, "proxy:") {
		nodeLabel = rawLabel
	}
	hash := text(row["proxy_hash"])
	display := label
	if nodeLabel != "" {
		display += " " + nodeLabel
	} else if hash != "" && hash != "direct" {
		display += " " + hash
	}
	out := Map{"source": source, "source_label": label, "label": nodeLabel, "hash": hash, "display": display, "key": text(row["egress_key"]), "mode": text(row["egress_mode"]), "group_id": group, "node_id": nodeID, "node_name": text(row["proxy_node_name"]), "has_proxy": nil}
	if _, ok := row["has_proxy"]; ok {
		out["has_proxy"] = truth(row["has_proxy"])
	}
	return out
}
func monitorAccountAttempt(row Map) Map {
	attempt := monitorInt(row["image_account_attempt"])
	maximum := max(attempt, monitorInt(row["image_account_max_attempts"]))
	switches := monitorInt(row["image_account_switch_count"])
	images := 0
	for _, v := range object(row["images"]) {
		images = max(images, monitorInt(object(v)["total"]))
	}
	parts := []string{}
	if attempt > 0 && maximum > 0 {
		prefix := "第 "
		if images > 1 {
			prefix = "最高第 "
		}
		parts = append(parts, fmt.Sprintf("%s%d/%d 次", prefix, attempt, maximum))
	}
	if attempt > 0 || maximum > 0 || switches > 0 {
		switchText := "未切换"
		if switches > 0 {
			switchText = fmt.Sprintf("已切换 %d 次", switches)
		}
		parts = append(parts, switchText)
	}
	return Map{"attempt": attempt, "max_attempts": maximum, "switch_count": switches, "image_count": images, "display": strings.Join(parts, " · ")}
}
func monitorSlowReason(items []Map) (string, string) {
	candidates := []Map{}
	for _, item := range items {
		key := text(item["key"])
		if key != "stream_ms" && key != "total_ms" && key != "duration_ms" {
			candidates = append(candidates, item)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return monitorInt(candidates[i]["value_ms"]) > monitorInt(candidates[j]["value_ms"])
	})
	if len(candidates) == 0 || monitorInt(candidates[0]["value_ms"]) < 1000 {
		return "", ""
	}
	top := candidates[0]
	key, label, value := text(top["key"]), text(top["label"]), text(top["value_text"])
	switch key {
	case "untracked_ms":
		return "instrumentation_gap", "仍有 " + value + " 没有落到具体阶段，说明这段链路还缺埋点。"
	case "poll_wait_ms":
		return "image_poll_wait", "主要卡在等待图片结果，通常是 ChatGPT 图片任务尚未完成或轮询退避。"
	case "poll_request_ms":
		return "image_poll_request", "主要卡在查询图片结果，通常是任务或会话查询接口响应较慢。"
	case "resolve_ms":
		return "image_result_resolve", "主要卡在结果处理，通常是图片文件 ID 转换下载地址较慢。"
	case "conversation_stream_ms":
		return "upstream_generation", "主要卡在上游生成中，通常是 ChatGPT 生成阶段耗时。"
	case "stream_error_ms":
		return "upstream_stream_error", "主要卡在上游断流，通常是 HTTP2/SSE、代理或上游边缘节点中断。"
	case "http_ttfb_ms", "http_wait_ms":
		return "http_first_byte", "主要卡在 HTTP 首包，通常是代理出口、上游边缘节点或请求排队变慢。"
	case "http_dns_ms", "http_tcp_ms", "http_tls_ms":
		return "http_connect", "主要卡在 HTTP 建连阶段：" + label + " " + value + "。"
	case "sse_first_event_ms":
		return "sse_first_event", "主要卡在 SSE 首事件，说明连接已建立但上游长时间没有返回首个事件。"
	case "sse_max_gap_ms", "sse_last_gap_ms":
		return "sse_gap", "主要卡在 SSE 空窗，说明上游流中间长时间没有新事件。"
	case "egress_wait_ms":
		return "egress_wait", "主要卡在等待出口，通常是代理组、默认出口、资源代理或出站会话准备变慢。"
	case "upload_ms", "bootstrap_ms", "requirements_ms", "prepare_conversation_ms", "generation_start_ms":
		return "upstream_prepare", "主要卡在上游准备阶段：" + label + " " + value + "。"
	case "account_wait_ms":
		return "account_wait", "主要卡在等待账号，通常是可用账号不足或账号并发被占满。"
	case "handler_queue_ms", "stream_first_queue_ms":
		return "entry_queue", "主要卡在等待入口，通常是后端同步线程容量不足；可通过环境变量 CHATGPT2API_THREAD_TOKENS 调整。"
	}
	return "primary_metric", "主要耗时：" + label + " " + value + "。"
}
func monitorEventView(event Map) Map {
	out := monitorFields(event, "EVENT_FIELDS")
	for _, key := range []string{"time", "call_id", "event"} {
		if out[key] == nil {
			out[key] = ""
		}
	}
	if text(out["label"]) == "" {
		out["label"] = text(out["event"])
	}
	parts := []string{}
	for _, pair := range list(monitorSpec["EVENT_METRIC_PAIRS"]) {
		p := stringsOf(pair)
		if len(p) == 2 && monitorInt(event[p[1]]) > 0 {
			parts = append(parts, p[0]+" "+monitorMS(event[p[1]]))
		}
	}
	if len(parts) > 3 {
		parts = parts[:3]
	}
	out["timing_text"] = "-"
	if len(parts) > 0 {
		out["timing_text"] = strings.Join(parts, " / ")
	}
	parts = []string{}
	if truth(event["switched_account"]) {
		parts = append(parts, "已切换账号")
	}
	account, previous := text(event["account_email"]), text(event["previous_account_email"])
	if account != "" {
		if previous != "" {
			account = previous + " → " + account
		}
		parts = append(parts, account)
	}
	errorText := text(event["public_error"])
	if errorText == "" {
		errorText = text(event["error"])
	}
	if errorText != "" {
		parts = append(parts, errorText)
	}
	out["detail_text"] = strings.Join(parts, " · ")
	return out
}
func MonitorRecordView(row Map, detail bool) Map {
	out := monitorFields(row, "RECORD_FIELDS")
	if out["call_id"] == nil {
		out["call_id"] = ""
	}
	out["metrics"] = monitorMetric(row["metrics"])
	out["perf"] = monitorMetric(row["perf"])
	timings := monitorTimings(row)
	out["timings_ms"] = timings
	images := Map{}
	for key, value := range object(row["images"]) {
		if _, ok := value.(map[string]any); ok {
			image := monitorFields(object(value), "IMAGE_FIELDS")
			image["metrics"] = monitorMetric(object(value)["metrics"])
			images[key] = image
		}
	}
	out["images"] = images
	egress, attempt := monitorEgress(row), monitorAccountAttempt(row)
	out["egress"] = egress
	out["account_attempt"] = attempt
	duration := max(monitorInt(row["duration_ms"]), monitorInt(row["elapsed_ms"]))
	queue, linear := 0, 0
	for _, key := range stringsOf(monitorSpec["ENTRY_QUEUE_METRIC_KEYS"]) {
		queue += monitorInt(timings[key])
	}
	for _, key := range stringsOf(monitorSpec["LINEAR_STAGE_KEYS"]) {
		linear += monitorInt(timings[key])
	}
	tracked := queue + max(linear, monitorInt(timings["total_ms"]), monitorInt(timings["stream_ms"]))
	untracked := max(0, duration-tracked)
	slow := []Map{}
	appendSlow := func(key, label string, n int) {
		slow = append(slow, Map{"key": key, "label": label, "value_ms": n, "value_text": monitorMS(n), "important": n >= 10000})
	}
	for _, pair := range list(monitorSpec["SLOW_METRIC_PAIRS"]) {
		p := stringsOf(pair)
		if len(p) == 2 && monitorInt(timings[p[0]]) > 0 {
			appendSlow(p[0], p[1], monitorInt(timings[p[0]]))
		}
	}
	if untracked >= 1000 {
		appendSlow("untracked_ms", "未标记", untracked)
	}
	if len(slow) == 0 && duration > 0 {
		appendSlow("duration_ms", "总耗时", duration)
	}
	reasonCode, reason := monitorSlowReason(slow)
	digest := []Map{}
	for _, pair := range list(monitorSpec["DIGEST_METRIC_PAIRS"]) {
		p := stringsOf(pair)
		if len(p) == 2 && monitorInt(timings[p[1]]) > 0 {
			digest = append(digest, Map{"n": monitorInt(timings[p[1]]), "text": p[0] + " " + monitorMS(timings[p[1]])})
		}
	}
	sort.SliceStable(digest, func(i, j int) bool { return monitorInt(digest[i]["n"]) > monitorInt(digest[j]["n"]) })
	digestTexts := []string{}
	if row["status"] == "running" && monitorInt(row["stage_elapsed_ms"]) > 0 {
		digestTexts = append(digestTexts, "当前阶段 "+monitorMS(row["stage_elapsed_ms"]))
	}
	for _, d := range digest {
		digestTexts = append(digestTexts, text(d["text"]))
	}
	if len(digestTexts) > 4 {
		digestTexts = digestTexts[:4]
	}
	digestText := "-"
	if len(digestTexts) > 0 {
		digestText = strings.Join(digestTexts, " / ")
	}
	status := text(row["outcome"])
	if status == "" {
		status = text(row["status"])
	}
	fallback := status
	if fallback == "" {
		fallback = "-"
	}
	presentation := RequestStatus(status, fallback)
	stage := text(row["stage_label"])
	if stage == "" {
		stage = text(row["stage"])
	}
	if stage == "" {
		stage = text(presentation["label"])
		if row["status"] == "running" {
			stage = "运行中"
		}
	}
	errorText := text(row["public_error"])
	if errorText == "" {
		errorText = text(row["error"])
	}
	out["presentation"] = Map{"status_label": presentation["label"], "status_tone": presentation["tone"], "stage_text": stage, "error_text": errorText, "duration_text": monitorMS(duration), "metric_digest": digestText, "egress_text": egress["display"], "account_attempt_text": attempt["display"], "account_egress_text": "账号 " + monitorMS(timings["account_wait_ms"]) + " / 出口 " + monitorMS(timings["egress_wait_ms"]), "tracked_duration_ms": tracked, "untracked_duration_ms": untracked, "slow_metrics": slow, "slow_reason_code": reasonCode, "slow_reason": reason}
	if detail {
		events := []Map{}
		for _, event := range dicts(row["events"]) {
			events = append(events, monitorEventView(event))
		}
		out["events"] = events
	}
	return out
}
func monitorSummaryView(source Map) Map {
	out := Map{}
	for _, key := range strings.Fields("active completed success partial_success failed rate_limited text_review account_switch_requests account_switches account_switch_success stream_error_requests avg_duration_ms p95_duration_ms") {
		out[key] = monitorInt(source[key])
	}
	measured := monitorInt(out["success"]) + monitorInt(out["failed"]) + monitorInt(out["rate_limited"])
	out["measured"] = measured
	switched, success := monitorInt(out["account_switch_requests"]), monitorInt(out["account_switch_success"])
	out["switch_unrecovered"] = max(0, switched-success)
	out["switch_average"] = 0.0
	if switched > 0 {
		out["switch_average"] = math.RoundToEven(float64(monitorInt(out["account_switches"]))*10/float64(switched)) / 10
	}
	for _, key := range []string{"success_rate", "account_switch_recovery_rate"} {
		rate, ok := number(source[key])
		if !ok {
			n, denominator := monitorInt(out["success"]), measured
			if key == "account_switch_recovery_rate" {
				n, denominator = success, switched
			}
			if denominator > 0 {
				rate = math.RoundToEven(float64(n)*1000/float64(denominator)) / 10
			}
		}
		out[key] = max(0.0, rate)
	}
	p95 := monitorMetric(source["metric_p95"])
	out["metric_p95"] = p95
	out["entry_queue_p95_ms"] = max(monitorInt(p95["handler_queue_ms"]), monitorInt(p95["stream_first_queue_ms"]))
	for _, key := range []string{"by_model", "active_by_model", "active_by_egress", "active_by_stage"} {
		m := Map{}
		for k, n := range object(source[key]) {
			m[k] = monitorInt(n)
		}
		out[key] = m
	}
	out["active_egress_count"] = len(object(out["active_by_egress"]))
	slow := Map{}
	for _, key := range []string{"handler_queue", "stream_first_queue", "account_wait", "egress_wait", "total_over_120s", "local_reject_or_busy"} {
		slow[key] = monitorInt(object(source["slow_counts"])[key])
	}
	out["slow_counts"] = slow
	return out
}

func monitorOrderedCounts(values Map, limit int) []string {
	keys := []string{}
	for key := range values {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := monitorInt(values[keys[i]]), monitorInt(values[keys[j]])
		if a == b {
			return keys[i] < keys[j]
		}
		return a > b
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys
}
func monitorDiagnosticGroups(summary Map, tokens int, window string) []Map {
	groups := dicts(copyMap(Map{"groups": monitorSpec["DIAGNOSTIC_GROUPS"]})["groups"])
	p95, slow := object(summary["metric_p95"]), object(summary["slow_counts"])
	activeEgress := object(summary["active_by_egress"])
	egressParts := []string{}
	for _, key := range monitorOrderedCounts(activeEgress, 2) {
		source, detail, found := strings.Cut(key, ":")
		label := RequestProxySource(source)
		if found {
			label += " " + detail
		}
		egressParts = append(egressParts, fmt.Sprintf("%s %d", label, monitorInt(activeEgress[key])))
	}
	egressMeta := "暂无活跃出口"
	if len(egressParts) > 0 {
		egressMeta = strings.Join(egressParts, " / ")
	}
	slowKeys := map[string]string{"slow_total": "total_over_120s", "egress_wait_slow": "egress_wait", "local_busy": "local_reject_or_busy", "account_wait_slow": "account_wait"}
	summaryKeys := map[string]string{"active_egress": "active_egress_count", "account_switch_unrecovered": "switch_unrecovered", "account_switch_average": "switch_average"}
	positiveTones := map[string]string{"failed": "danger", "slow_total": "warning", "egress_wait_slow": "warning", "local_busy": "danger", "account_wait_slow": "warning", "account_switch_requests": "warning", "account_switches": "warning", "account_switch_success": "success", "account_switch_recovery_rate": "success", "account_switch_unrecovered": "danger", "account_switch_average": "warning", "stream_error_requests": "danger"}
	for _, group := range groups {
		for _, item := range dicts(group["items"]) {
			key := text(item["key"])
			value := summary[key]
			if source := summaryKeys[key]; source != "" {
				value = summary[source]
			}
			if source := slowKeys[key]; source != "" {
				value = slow[source]
			}
			switch {
			case strings.HasSuffix(key, "_ms"):
				value = monitorMS(p95[key])
			case key == "average":
				value = monitorMS(summary["avg_duration_ms"])
			case key == "p95":
				value = monitorMS(summary["p95_duration_ms"])
			case key == "thread_capacity":
				value = tokens
			case key == "success_rate" || key == "account_switch_recovery_rate":
				n, _ := number(summary[key])
				value = fmt.Sprintf("%.1f%%", n)
			}
			if value == nil {
				value = 0
			}
			item["value"] = value
			if tone := positiveTones[key]; tone != "" {
				condition := monitorInt(value) > 0
				if key == "account_switch_recovery_rate" {
					condition = monitorInt(summary["account_switch_success"]) > 0
				}
				if key == "account_switch_average" {
					condition = monitorInt(summary["account_switches"]) > 0
				}
				item["tone"] = "muted"
				if condition {
					item["tone"] = tone
				}
			}
			switch key {
			case "active":
				item["meta"] = fmt.Sprintf("线程容量 %d", tokens)
			case "completed":
				item["meta"] = window
			case "handler_queue_ms":
				item["meta"] = fmt.Sprintf("线程容量 %d · 慢 %d", tokens, monitorInt(slow["handler_queue"]))
			case "stream_first_queue_ms":
				item["meta"] = fmt.Sprintf("慢 %d", monitorInt(slow["stream_first_queue"]))
			case "egress_wait_ms", "active_egress":
				item["meta"] = egressMeta
			case "account_switch_success":
				item["meta"] = "暂无切号请求"
				if monitorInt(summary["account_switch_requests"]) > 0 {
					item["meta"] = "已恢复请求"
				}
			}
		}
	}
	return groups
}
func MonitorView(snapshot Map) Map {
	thread, window := Map{}, Map{}
	for _, key := range []string{"tokens", "previous_tokens"} {
		thread[key] = monitorInt(object(snapshot["threadpool"])[key])
	}
	for _, key := range []string{"completed", "completed_capacity", "events", "event_capacity"} {
		window[key] = monitorInt(object(snapshot["window"])[key])
	}
	summary := monitorSummaryView(object(snapshot["summary"]))
	windowText := fmt.Sprintf("窗口 %d / %d", monitorInt(window["completed"]), monitorInt(window["completed_capacity"]))
	out := Map{"schema_version": 1, "updated_at": text(snapshot["updated_at"]), "threadpool": thread, "window": window, "summary": summary, "metric_labels": copyMap(object(snapshot["metric_labels"])), "completed_window_text": windowText, "entry_queue_text": monitorMS(summary["entry_queue_p95_ms"])}
	for _, key := range []string{"active", "recent", "slow"} {
		items := []Map{}
		for _, row := range dicts(snapshot[key]) {
			items = append(items, MonitorRecordView(row, false))
		}
		out[key] = items
	}
	stages := []Map{}
	for _, key := range monitorOrderedCounts(object(summary["active_by_stage"]), 8) {
		n := monitorInt(object(summary["active_by_stage"])[key])
		if n > 0 {
			stages = append(stages, Map{"label": key, "count": n})
		}
	}
	out["active_stage_items"] = stages
	out["diagnostic_groups"] = monitorDiagnosticGroups(summary, monitorInt(thread["tokens"]), windowText)
	return out
}
