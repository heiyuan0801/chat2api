package controlplane

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RealtimeMonitor owns the bounded process-local Active Request window. SQL
// Call Records remain the independent durable history owner.
type RealtimeMonitor struct {
	server                     *Server
	mu                         sync.Mutex
	active                     map[string]Map
	completed, events          []Map
	completedLimit, eventLimit int
}

func NewRealtimeMonitor(s *Server) *RealtimeMonitor {
	return &RealtimeMonitor{server: s, active: map[string]Map{}, completed: []Map{}, events: []Map{}, completedLimit: monitorEnv("CHATGPT2API_MONITOR_COMPLETED_LIMIT", 500, 50, 5000), eventLimit: monitorEnv("CHATGPT2API_MONITOR_EVENT_LIMIT", 1000, 100, 10000)}
}
func (m *RealtimeMonitor) Active() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.active) }

func monitorEnv(name string, fallback, minimum, maximum int) int {
	n, e := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if e != nil {
		return fallback
	}
	return max(minimum, min(maximum, n))
}
func (m *RealtimeMonitor) appendEvent(callID, event string, row, data Map) {
	label := text(object(monitorSpec["STAGE_LABELS"])[event])
	if label == "" {
		label = event
	}
	payload := Map{"time": imageTaskTime(m.server.now()), "call_id": callID, "event": event, "label": label, "model": text(row["model"])}
	for _, key := range stringsOf(monitorSpec["EVENT_FIELDS"]) {
		if value, ok := data[key]; ok {
			if _, ok := value.(string); ok {
				value = safeCallText(value, 1000)
			}
			payload[key] = value
		}
	}
	m.events = append(m.events, payload)
	if len(m.events) > m.eventLimit {
		m.events = m.events[len(m.events)-m.eventLimit:]
	}
}
func (m *RealtimeMonitor) Start(callID, endpoint, model, summary, role, keyName string) {
	if strings.TrimSpace(callID) == "" {
		return
	}
	model = safeCallText(model, 256)
	summary = safeCallText(summary, 1000)
	keyName = safeCallText(keyName, 256)
	now := m.server.now()
	row := Map{"call_id": callID, "endpoint": endpoint, "model": model, "summary": summary, "role": role, "key_name": keyName, "status": "running", "stage": "handler_submitted", "stage_label": "等待入口", "started_ts": editableTS(now), "stage_started_ts": editableTS(now), "started_at": imageTaskTime(now), "updated_at": imageTaskTime(now), "metrics": Map{}, "perf": Map{}, "images": Map{}}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[callID] = row
	m.appendEvent(callID, "handler_submitted", row, nil)
}
func monitorMergeMetrics(target, source Map) {
	for key, v := range source {
		if strings.HasSuffix(key, "_ms") {
			target[key] = max(monitorInt(target[key]), monitorInt(v))
		}
	}
}
func monitorMergeFields(target, data Map) {
	for _, key := range stringsOf(monitorSpec["RECORD_FIELDS"]) {
		if value, ok := data[key]; ok {
			switch key {
			case "call_id", "endpoint", "summary", "role", "key_name", "started_at", "ended_at", "elapsed_ms", "stage_elapsed_ms", "duration_ms", "status", "outcome", "stage", "stage_label", "updated_at":
				continue
			}
			if key == "model" && text(target[key]) != "" {
				continue
			}
			if value != nil {
				if monitorHas(monitorSpec["INTEGER_FIELDS"], key) {
					target[key] = monitorInt(value)
				} else if monitorHas(monitorSpec["BOOLEAN_FIELDS"], key) {
					target[key] = truth(value)
				} else {
					target[key] = safeCallText(value, 4000)
				}
			}
		}
	}
	metrics := object(target["metrics"])
	monitorMergeMetrics(metrics, data)
	target["metrics"] = metrics
}

// imageTiming accumulates phase durations without creating lifecycle events or
// resetting the current stage clock. Only fixed upstream timing names are accepted.
func (m *RealtimeMonitor) imageTiming(callID string, slot int, metric string, elapsed time.Duration) {
	switch metric {
	case "poll_conversation_ms", "poll_tasks_ms", "poll_sleep_ms", "poll_settle_ms", "asset_resolve_ms", "asset_download_ms", "asset_publish_ms":
	default:
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.active[callID]
	if row == nil {
		return
	}
	target := row
	if slot > 0 {
		target = object(object(row["images"])[strconv.Itoa(slot)])
		if len(target) == 0 {
			return
		}
	}
	metrics := object(target["metrics"])
	metrics[metric] = monitorInt(metrics[metric]) + max(0, int(elapsed.Milliseconds()))
	target["metrics"] = metrics
	monitorMergeMetrics(object(row["metrics"]), metrics)
}

func (m *RealtimeMonitor) Stage(callID, event string, data Map) {
	if strings.TrimSpace(event) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.active[callID]
	if row == nil {
		return
	}
	now := m.server.now()
	if truth(data["_measure"]) {
		data = copyMap(data)
		previous := row
		index := monitorInt(data["index"])
		if index > 0 {
			previous = object(object(row["images"])[strconv.Itoa(index)])
		}
		at, _ := number(previous["stage_started_ts"])
		if at > 0 {
			duration := max(0, int((editableTS(now)-at)*1000))
			metric := map[string]string{"handler_submitted": "handler_queue_ms", "image_getting_account": "account_wait_ms", "image_egress_waiting": "egress_wait_ms", "image_uploading": "upload_ms", "image_bootstrapping": "bootstrap_ms", "image_getting_token": "requirements_ms", "image_preparing_conversation": "prepare_conversation_ms", "image_starting_generation": "generation_start_ms", "image_generating": "conversation_stream_ms", "image_stream_resolve_start": "poll_wait_ms", "image_resolve_done": "download_ms"}[text(previous["stage"])]
			if metric != "" && duration > 0 {
				data[metric] = max(monitorInt(data[metric]), duration)
			}
		}
	}
	if row["stage"] != event {
		row["stage_started_ts"] = editableTS(now)
	}
	row["stage"] = event
	row["stage_label"] = orValue(object(monitorSpec["STAGE_LABELS"])[event], event)
	row["updated_at"] = imageTaskTime(now)
	monitorMergeFields(row, data)
	if index := monitorInt(data["index"]); index > 0 {
		images := object(row["images"])
		key := strconv.Itoa(index)
		image := object(images[key])
		image["index"] = index
		if image["stage"] != event {
			image["stage_started_ts"] = editableTS(now)
		}
		image["metrics"] = object(image["metrics"])
		image["stage"], image["stage_label"], image["updated_at"] = row["stage"], row["stage_label"], row["updated_at"]
		for _, field := range stringsOf(monitorSpec["IMAGE_FIELDS"]) {
			if value, ok := data[field]; ok {
				image[field] = value
			}
		}
		for _, pair := range [][2]string{{"attempt", "account_attempt"}, {"max_account_attempts", "max_account_attempts"}, {"account_switch_count", "account_switch_count"}} {
			if v, ok := data[pair[0]]; ok {
				image[pair[1]] = monitorInt(v)
			}
		}
		monitorMergeMetrics(object(image["metrics"]), data)
		images[key] = image
		row["images"] = images
		attempt, maximum, switches := 0, 0, 0
		for _, value := range images {
			im := object(value)
			attempt = max(attempt, monitorInt(im["account_attempt"]))
			maximum = max(maximum, monitorInt(im["max_account_attempts"]))
			switches += monitorInt(im["account_switch_count"])
		}
		row["image_account_attempt"], row["image_account_max_attempts"], row["image_account_switch_count"] = attempt, maximum, switches
	}
	m.appendEvent(callID, event, row, data)
}
func (m *RealtimeMonitor) Merge(callID string, data Map) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row := m.active[callID]; row != nil {
		metadata := Map{}
		for _, key := range stringsOf(monitorSpec["RECORD_FIELDS"]) {
			if key != "duration_ms" && key != "elapsed_ms" && key != "stage_elapsed_ms" {
				if value, ok := data[key]; ok {
					metadata[key] = value
				}
			}
		}
		monitorMergeFields(row, metadata)
	}
}
func (m *RealtimeMonitor) CaptureAttempts(callID string, attempts []Map) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.active[callID]
	if row == nil {
		return
	}
	captured := object(row["_image_attempts"])
	for _, value := range attempts {
		slot, attempt := monitorInt(value["slot"]), monitorInt(value["attempt"])
		if slot <= 0 || attempt <= 0 || text(value["status"]) == "" {
			continue
		}
		key := fmt.Sprintf("%06d:%06d", slot, attempt)
		captured[key] = merge(object(captured[key]), safeCallAttempt(value))
	}
	row["_image_attempts"] = captured
}
func monitorDiagnostic(row Map, events []Map) Map {
	out := Map{}
	for _, key := range strings.Fields("stage stage_label proxy_source proxy_hash egress_mode egress_key egress_label proxy_group_id proxy_node_id proxy_node_name image_egress_limit local_reason error raw_error upstream_error upstream_message has_proxy") {
		if value, ok := row[key]; ok && value != nil && value != "" {
			out[key] = value
		}
	}
	metrics := Map{}
	for key, value := range monitorTimings(row) {
		if monitorInt(value) > 0 {
			metrics[key] = value
		}
	}
	if len(metrics) > 0 {
		out["metrics"] = metrics
	}
	images := Map{}
	for key, raw := range object(row["images"]) {
		source := object(raw)
		image := monitorFields(source, "IMAGE_FIELDS")
		delete(image, "account_email")
		delete(image, "previous_account_email")
		delete(image, "account_attempt")
		delete(image, "max_account_attempts")
		delete(image, "account_switch_count")
		delete(image, "updated_at")
		ms := Map{}
		for k, v := range monitorMetric(source["metrics"]) {
			if monitorInt(v) > 0 {
				ms[k] = v
			}
		}
		if len(ms) > 0 {
			image["metrics"] = ms
		}
		if len(image) > 0 {
			images[key] = image
		}
	}
	if len(images) > 0 {
		out["images"] = images
	}
	if len(events) > 0 {
		compact := []Map{}
		for _, event := range events {
			item := Map{}
			for key, value := range event {
				if monitorHas(monitorSpec["CANONICAL_FAILURE_FIELDS"], key) || monitorHas(monitorSpec["RAW_DIAGNOSTIC_FIELDS"], key) || monitorHas([]string{"time", "event", "label", "index", "total", "attempt", "status", "public_error", "account_failure", "switched_account", "local_reason"}, key) || (strings.HasSuffix(key, "_ms") && monitorInt(value) > 0) {
					item[key] = value
				}
			}
			compact = append(compact, item)
		}
		out["events"] = compact
	}
	return out
}
func (m *RealtimeMonitor) Finish(detail Map) {
	callID := text(detail["call_id"])
	if callID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.active[callID]
	if row == nil {
		for _, done := range m.completed {
			if done["call_id"] == callID {
				return
			}
		}
		now := m.server.now()
		row = Map{"call_id": callID, "endpoint": detail["endpoint"], "model": detail["model"], "summary": "", "role": detail["role"], "key_name": detail["key_name"], "started_ts": editableTS(now) - float64(monitorInt(detail["duration_ms"]))/1000, "stage_started_ts": editableTS(now), "started_at": detail["started_at"], "metrics": Map{}, "perf": Map{}, "images": Map{}}
	}
	delete(m.active, callID)
	for _, key := range []string{"endpoint", "model", "role", "key_name", "account_email", "conversation_id"} {
		if text(detail[key]) != "" {
			row[key] = safeCallText(detail[key], 4000)
		}
	}
	row["ended_at"] = orValue(detail["ended_at"], imageTaskTime(m.server.now()))
	row["updated_at"] = row["ended_at"]
	row["duration_ms"] = monitorInt(detail["duration_ms"])
	metadata := copyMap(detail)
	for _, key := range []string{"duration_ms", "elapsed_ms", "stage_elapsed_ms"} {
		delete(metadata, key)
	}
	monitorMergeFields(row, metadata)
	perf := object(row["perf"])
	monitorMergeMetrics(perf, object(detail["perf"]))
	row["perf"] = perf
	attempts := object(row["_image_attempts"])
	for _, attempt := range dicts(detail["image_attempts"]) {
		slot, n := monitorInt(attempt["slot"]), monitorInt(attempt["attempt"])
		if slot > 0 && n > 0 && text(attempt["status"]) != "" {
			key := fmt.Sprintf("%06d:%06d", slot, n)
			attempts[key] = merge(object(attempts[key]), attempt)
		}
	}
	if len(attempts) > 0 {
		keys := []string{}
		for key := range attempts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		merged := []Map{}
		for _, key := range keys {
			merged = append(merged, object(attempts[key]))
		}
		detail["image_attempts"] = merged
	}
	delete(row, "_image_attempts")
	events := []Map{}
	for _, event := range m.events {
		if event["call_id"] == callID {
			events = append(events, copyMap(event))
		}
	}
	for _, attempt := range dicts(detail["image_attempts"]) {
		slot, n := monitorInt(attempt["slot"]), monitorInt(attempt["attempt"])
		matching := []Map{}
		metrics := Map{}
		for _, event := range events {
			if monitorInt(event["index"]) == slot && monitorInt(event["attempt"]) == n {
				monitorMergeMetrics(metrics, event)
				compact := monitorDiagnostic(Map{}, []Map{event})
				matching = append(matching, dicts(compact["events"])[0])
			}
		}
		if len(matching) > 0 {
			if len(matching) > 40 {
				matching = matching[len(matching)-40:]
			}
			monitor := Map{"events": matching}
			if len(metrics) > 0 {
				monitor["metrics"] = metrics
			}
			attempt["monitor"] = monitor
		}
	}
	summary := CallSummary(Map{"id": callID, "time": orValue(detail["started_at"], row["started_at"]), "type": "call", "summary": row["summary"], "detail": detail})
	outcome := text(summary["outcome"])
	successful := outcome == "success" || outcome == "partial_success"
	stage := "failed"
	if successful {
		stage = "completed"
	} else if outcome == "text_review" {
		stage = "text_review"
	} else if outcome == "rate_limited" {
		stage = "rate_limited"
	}
	label := text(object(monitorSpec["STAGE_LABELS"])[stage])
	if stage == "text_review" {
		label = "文本"
	}
	for _, key := range strings.Fields("outcome attempt_count switch_count image_requested_count image_succeeded_count image_failed_count image_result_status recovered_after_switch public_error") {
		row[key] = summary[key]
	}
	if successful {
		for _, key := range stringsOf(monitorSpec["CALL_FAILURE_FIELDS"]) {
			delete(row, key)
		}
		row["public_error"] = ""
	}
	row["status"] = outcome
	if outcome == "partial_success" {
		row["status"] = "success"
	} else if outcome != "success" && outcome != "failed" && outcome != "rate_limited" && outcome != "text_review" {
		row["status"] = "failed"
	}
	row["stage"], row["stage_label"] = stage, label
	if len(events) > 60 {
		events = events[len(events)-60:]
	}
	diagnostic := monitorDiagnostic(row, events)
	if len(diagnostic) > 0 {
		detail["monitor"] = diagnostic
		for _, key := range strings.Fields("proxy_source proxy_hash has_proxy egress_mode egress_key egress_label proxy_group_id proxy_node_id proxy_node_name image_egress_limit local_reason") {
			if _, ok := detail[key]; !ok {
				if value, ok := diagnostic[key]; ok {
					detail[key] = value
				}
			}
		}
	}
	m.completed = append(m.completed, copyMap(row))
	if len(m.completed) > m.completedLimit {
		m.completed = m.completed[len(m.completed)-m.completedLimit:]
	}
	m.appendEvent(callID, stage, row, nil)
}
func monitorP95(values []int) int {
	out := []int{}
	for _, n := range values {
		if n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return 0
	}
	sort.Ints(out)
	return out[min(len(out)-1, max(0, int(math.Ceil(float64(len(out))*.95))-1))]
}
func monitorCount(rows []Map, key string) Map {
	out := Map{}
	for _, row := range rows {
		value := text(row[key])
		if value != "" {
			out[value] = monitorInt(out[value]) + 1
		}
	}
	return out
}
func monitorRuntimeSummary(active, completed []Map) Map {
	counts := monitorCount(completed, "outcome")
	success := monitorInt(counts["success"]) + monitorInt(counts["partial_success"])
	failed := monitorInt(counts["failed"]) + monitorInt(counts["unknown"])
	summary := Map{"active": len(active), "completed": len(completed), "success": success, "partial_success": monitorInt(counts["partial_success"]), "failed": failed, "rate_limited": monitorInt(counts["rate_limited"]), "text_review": monitorInt(counts["text_review"])}
	switchRequests, switchCount, switchSuccess, streamErrors := 0, 0, 0, 0
	durations := []int{}
	metrics := map[string][]int{}
	slow := Map{"handler_queue": 0, "stream_first_queue": 0, "account_wait": 0, "egress_wait": 0, "total_over_120s": 0, "local_reject_or_busy": 0}
	for _, row := range completed {
		n := monitorInt(row["switch_count"])
		if n > 0 {
			switchRequests++
		}
		switchCount += n
		if row["recovered_after_switch"] == true {
			switchSuccess++
		}
		timings := monitorTimings(row)
		if monitorInt(timings["stream_error_ms"]) > 0 {
			streamErrors++
		}
		if n = monitorInt(row["duration_ms"]); n > 0 {
			durations = append(durations, n)
		}
		for key, value := range timings {
			metrics[key] = append(metrics[key], monitorInt(value))
		}
		for key, limit := range map[string]int{"handler_queue": 1000, "stream_first_queue": 1000, "account_wait": 5000, "egress_wait": 1000} {
			if monitorInt(timings[key+"_ms"]) >= limit {
				slow[key] = monitorInt(slow[key]) + 1
			}
		}
		if monitorInt(row["duration_ms"]) >= 120000 {
			slow["total_over_120s"] = monitorInt(slow["total_over_120s"]) + 1
		}
		if row["stage"] == "image_local_rejected" || text(row["local_reason"]) != "" {
			slow["local_reject_or_busy"] = monitorInt(slow["local_reject_or_busy"]) + 1
		}
	}
	p95 := Map{}
	for key := range object(monitorSpec["METRIC_LABELS"]) {
		p95[key] = monitorP95(metrics[key])
	}
	summary["metric_p95"], summary["slow_counts"] = p95, slow
	summary["account_switch_requests"], summary["account_switches"], summary["account_switch_success"], summary["stream_error_requests"] = switchRequests, switchCount, switchSuccess, streamErrors
	sum := 0
	for _, n := range durations {
		sum += n
	}
	summary["avg_duration_ms"] = 0
	if len(durations) > 0 {
		summary["avg_duration_ms"] = int(math.RoundToEven(float64(sum) / float64(len(durations))))
	}
	summary["p95_duration_ms"] = monitorP95(durations)
	summary["by_model"] = monitorCount(completed, "model")
	summary["active_by_model"] = monitorCount(active, "model")
	egress, stages := Map{}, Map{}
	for _, row := range active {
		source := text(row["proxy_source"])
		if source == "" {
			source = "direct"
		}
		label := text(row["egress_label"])
		if label == "" || label == "direct" || label == source || label == source+"_profile" || strings.HasPrefix(label, "proxy:") {
			label = text(row["proxy_hash"])
		}
		key := source
		if label != "" && label != "direct" {
			key += ":" + label
		}
		egress[key] = monitorInt(egress[key]) + 1
		stage := text(object(monitorSpec["ACTIVE_STAGE_GROUPS"])[text(row["stage"])])
		if stage == "" {
			stage = text(orValue(row["stage_label"], orValue(row["stage"], "运行中")))
		}
		stages[stage] = monitorInt(stages[stage]) + 1
	}
	summary["active_by_egress"], summary["active_by_stage"] = egress, stages
	for _, key := range []string{"by_model", "active_by_model", "active_by_egress", "active_by_stage"} {
		limit := 10
		if key == "active_by_egress" {
			limit = 8
		}
		all := object(summary[key])
		trimmed := Map{}
		for _, k := range monitorOrderedCounts(all, limit) {
			trimmed[k] = all[k]
		}
		summary[key] = trimmed
	}
	return summary
}
func (m *RealtimeMonitor) elapsed(row Map) Map {
	row = copyMap(row)
	if row["status"] == "running" {
		now := editableTS(m.server.now())
		start, _ := number(row["started_ts"])
		stage, _ := number(row["stage_started_ts"])
		row["elapsed_ms"] = max(0, int((now-start)*1000))
		row["stage_elapsed_ms"] = max(0, int((now-stage)*1000))
	}
	delete(row, "started_ts")
	delete(row, "stage_started_ts")
	delete(row, "_image_attempts")
	return row
}
func (m *RealtimeMonitor) Snapshot() Map {
	m.mu.Lock()
	active, completed, events := []Map{}, []Map{}, []Map{}
	for _, row := range m.active {
		active = append(active, copyMap(row))
	}
	for _, row := range m.completed {
		completed = append(completed, copyMap(row))
	}
	for _, event := range m.events {
		events = append(events, copyMap(event))
	}
	m.mu.Unlock()
	sort.SliceStable(active, func(i, j int) bool {
		a, _ := number(active[i]["started_ts"])
		b, _ := number(active[j]["started_ts"])
		if a == b {
			return text(active[i]["call_id"]) < text(active[j]["call_id"])
		}
		return a < b
	})
	summary := monitorRuntimeSummary(active, completed)
	recent, slow := []Map{}, append([]Map{}, completed...)
	for i := len(completed) - 1; i >= 0 && len(recent) < 80; i-- {
		recent = append(recent, m.elapsed(completed[i]))
	}
	sort.SliceStable(slow, func(i, j int) bool {
		score := func(row Map) int {
			ms := monitorTimings(row)
			return max(monitorInt(row["duration_ms"]), monitorInt(ms["handler_queue_ms"])*4, monitorInt(ms["stream_first_queue_ms"])*4, monitorInt(ms["account_wait_ms"])*2)
		}
		return score(slow[i]) > score(slow[j])
	})
	if len(slow) > 50 {
		slow = slow[:50]
	}
	for i := range slow {
		slow[i] = m.elapsed(slow[i])
	}
	if len(active) > 100 {
		active = active[:100]
	}
	for i := range active {
		active[i] = m.elapsed(active[i])
	}
	return MonitorView(Map{"updated_at": imageTaskTime(m.server.now()), "threadpool": Map{"tokens": 0, "previous_tokens": 0}, "window": Map{"completed": len(completed), "completed_capacity": m.completedLimit, "events": len(events), "event_capacity": m.eventLimit}, "summary": summary, "active": active, "recent": recent, "slow": slow, "metric_labels": monitorSpec["METRIC_LABELS"]})
}
func (m *RealtimeMonitor) Detail(callID string) (Map, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.active[strings.TrimSpace(callID)]
	if row == nil {
		for i := len(m.completed) - 1; i >= 0; i-- {
			if m.completed[i]["call_id"] == callID {
				row = m.completed[i]
				break
			}
		}
	}
	if row == nil {
		return nil, false
	}
	out := m.elapsed(row)
	events := []Map{}
	for _, event := range m.events {
		if event["call_id"] == callID {
			events = append(events, copyMap(event))
		}
	}
	if len(events) > 60 {
		events = events[len(events)-60:]
	}
	out["events"] = events
	return MonitorRecordView(out, true), true
}
func (s *Server) registerMonitor() {
	s.route("GET", "/api/monitor/realtime", "admin", "", func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
		return monitorHTTPValue(s.Monitor.Snapshot()), nil
	})
	s.route("GET", "/api/monitor/realtime/{call_id}", "admin", "", func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
		value, ok := s.Monitor.Detail(r.PathValue("call_id"))
		if !ok {
			return nil, problem(404, "monitor call not found")
		}
		return monitorHTTPValue(value), nil
	})
}

type monitorImageSlotKey struct{}

func monitorImageData(ctx context.Context, extra Map) Map {
	data := Map{}
	if slot, ok := ctx.Value(monitorImageSlotKey{}).(Map); ok {
		data = copyMap(slot)
	}
	return merge(data, extra)
}

// FastAPI's monitor response model omits None and coerces event timings to int.
func monitorHTTPValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := Map{}
		for key, raw := range v {
			if raw == nil {
				continue
			}
			if strings.HasSuffix(key, "_ms") {
				if _, ok := raw.(string); ok {
					out[key] = monitorInt(raw)
					continue
				}
			}
			out[key] = monitorHTTPValue(raw)
		}
		return out
	case []Map:
		out := []Map{}
		for _, raw := range v {
			out = append(out, monitorHTTPValue(raw).(Map))
		}
		return out
	case []any:
		out := []any{}
		for _, raw := range v {
			out = append(out, monitorHTTPValue(raw))
		}
		return out
	default:
		return value
	}
}
