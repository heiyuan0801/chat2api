package controlplane

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

//go:embed request_timeline.json
var requestTimelineJSON []byte
var timelineSpec struct {
	Categories [][]string        `json:"categories"`
	Steps      [][]string        `json:"steps"`
	Segments   []json.RawMessage `json:"segments"`
	Thresholds map[string]int    `json:"thresholds"`
}

func init() {
	if e := json.Unmarshal(requestTimelineJSON, &timelineSpec); e != nil {
		panic(e)
	}
}
func callInt(v any) int {
	f, ok := number(v)
	if !ok {
		return 0
	}
	return max(0, int(f))
}
func callClean(v any) string {
	if v == nil {
		return ""
	}
	if b, ok := v.(bool); ok {
		if b {
			return "True"
		}
		return "False"
	}
	return strings.TrimSpace(text(v))
}
func inlineValue(v any) string {
	if v == nil || v == "" {
		return ""
	}
	switch x := v.(type) {
	case []any:
		parts := []string{}
		for _, v := range x {
			if s := inlineValue(v); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " · ")
	case []string:
		parts := []string{}
		for _, v := range x {
			if s := inlineValue(v); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " · ")
	case map[string]any:
		keys := []string{}
		simple := true
		for k, v := range x {
			if v != nil && v != "" {
				keys = append(keys, k)
				switch v.(type) {
				case map[string]any, []any, []Map, []string:
					simple = false
				}
			}
		}
		order := []string{"response_message_items", "input_image_parts", "image_url_parts", "image_parts", "data_url_images", "remote_image_urls", "literal_image_placeholders"}
		slices.SortFunc(keys, func(a, b string) int {
			ai, bi := slices.Index(order, a), slices.Index(order, b)
			if ai < 0 {
				ai = len(order)
			}
			if bi < 0 {
				bi = len(order)
			}
			if ai != bi {
				return ai - bi
			}
			return strings.Compare(a, b)
		})
		if len(keys) == 0 {
			return ""
		}
		if len(keys) <= 8 && simple {
			parts := []string{}
			for _, k := range keys {
				parts = append(parts, k+": "+inlineValue(x[k]))
			}
			return strings.Join(parts, " · ")
		}
		b, _ := json.MarshalIndent(x, "", "  ")
		return string(b)
	case bool:
		return strconv.FormatBool(x)
	}
	return callClean(v)
}
func RequestDuration(v any) string {
	n := callInt(v)
	if n < 1000 {
		return fmt.Sprintf("%dms", n)
	}
	div, digits, unit := 1000.0, 2, "s"
	if n >= 60000 {
		div, digits, unit = 60000, 1, "m"
	} else if n >= 10000 {
		digits = 1
	}
	s := strconv.FormatFloat(float64(n)/div, 'f', digits, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".") + unit
}
func RequestStatus(outcome, fallback string) Map {
	labels := map[string]string{"success": "成功", "partial_success": "部分成功", "rate_limited": "限流", "text_review": "文本", "failed": "失败", "error": "失败", "fail": "失败", "running": "运行中"}
	tones := map[string]string{"success": "success", "partial_success": "warning", "rate_limited": "warning", "text_review": "warning", "failed": "danger", "error": "danger", "fail": "danger", "running": "info"}
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if l, ok := labels[outcome]; ok {
		return Map{"label": l, "tone": tones[outcome]}
	}

	if fallback == "" {
		fallback = "记录"
	}
	return Map{"label": fallback, "tone": "muted"}
}
func RequestProxySource(source string) string {
	if source == "" {
		source = "direct"
	}
	for _, p := range [][2]string{{"account_group", "账号组"}, {"account", "账号"}, {"default", "默认"}, {"global", "默认"}, {"runtime_resource", "资源代理"}, {"runtime", "代理会话"}, {"explicit", "指定"}, {"direct", "直连"}} {
		if strings.Contains(source, p[0]) {
			return p[1]
		}
	}
	return source
}
func diagnosticRaw(detail Map, key string) any {
	if v := detail[key]; v != nil && v != "" {
		return v
	}
	return object(detail["diagnosis"])[key]
}
func RequestEgress(detail, monitor Map) string {
	value := func(k string) string {
		v := inlineValue(diagnosticRaw(detail, k))
		if v == "" {
			v = inlineValue(monitor[k])
		}
		return v
	}
	source := value("proxy_source")
	if source == "" {
		return ""
	}
	label := RequestProxySource(source)
	node := value("proxy_node_name")
	if node == "" {
		node = value("proxy_node_id")
	}
	parts := []string{}
	for _, v := range []string{value("proxy_group_id"), node} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) > 0 {
		return label + " " + strings.Join(parts, "/")
	}
	if v := value("egress_label"); v != "" && v != "direct" && !strings.HasPrefix(v, "proxy:") {
		return label + " " + v
	}
	if v := value("proxy_hash"); v != "" && v != "direct" {
		return label + " " + v
	}
	return label
}
func RequestImageResolutions(detail Map) []string {
	out := []string{}
	for _, im := range dicts(detail["result_images"]) {
		if truth(im["width"]) && truth(im["height"]) {
			out = append(out, fmt.Sprintf("%v×%v", im["width"], im["height"]))
		} else {
			out = append(out, "未知")
		}
	}
	return out
}
func timelineTone(k string, n int) string {
	if k == "stream_error_ms" {
		return "danger"
	}
	threshold := timelineSpec.Thresholds[k]
	if threshold == 0 {
		threshold = 60000
	}
	if n >= threshold {
		return "warning"
	}
	return "info"
}
func RequestTimeline(timings Map, events any, imageCount int, shape Map) Map {
	segments := []Map{}
	groups := []Map{}
	legend := []Map{}
	steps := map[string][]Map{}
	warning, danger := false, false
	for _, raw := range timelineSpec.Segments {
		var v []json.RawMessage
		_ = json.Unmarshal(raw, &v)
		var key, label, category string
		var keys []string
		_ = json.Unmarshal(v[0], &key)
		_ = json.Unmarshal(v[1], &label)
		_ = json.Unmarshal(v[2], &category)
		_ = json.Unmarshal(v[3], &keys)
		value := 0
		tone := "info"
		for _, k := range keys {
			value += callInt(timings[k])
		}
		if key == "upstream" {
			envelope := callInt(timings["conversation_stream_ms"])
			if envelope == 0 {
				envelope = callInt(timings["stream_error_ms"])
			}
			prepare := 0
			for _, k := range []string{"upload_ms", "bootstrap_ms", "requirements_ms", "prepare_conversation_ms"} {
				prepare += callInt(timings[k])
			}
			if envelope > 0 {
				value = max(value, envelope-prepare)
			}
			keys = append(keys, "stream_error_ms")
		}
		if value <= 0 {
			continue
		}
		for _, k := range keys {
			if n := callInt(timings[k]); n > 0 {
				t := timelineTone(k, n)
				if t == "danger" || t == "warning" && tone != "danger" {
					tone = t
				}
			}
		}
		warning = warning || tone == "warning"
		danger = danger || tone == "danger"
		segments = append(segments, Map{"key": key, "label": label, "category": category, "value_ms": value, "value_text": RequestDuration(value), "tone": tone})
	}
	for _, v := range timelineSpec.Steps {
		k, label, cat, description := v[0], v[1], v[2], v[3]
		n := callInt(timings[k])
		if n <= 0 {
			continue
		}
		parts := []string{description}
		if k == "upload_ms" {
			p := []string{}
			for _, a := range [][2]string{{"input_image_parts", "输入图"}, {"image_url_parts", "图链"}, {"image_parts", "图片块"}, {"data_url_images", "base64"}, {"remote_image_urls", "远程图"}, {"literal_image_placeholders", "占位图"}} {
				if n := callInt(shape[a[0]]); n > 0 {
					p = append(p, fmt.Sprintf("%s %d", a[1], n))
				}
			}
			if len(p) > 0 {
				parts = append(parts, strings.Join(p, " · "))
			}
		}
		if imageCount > 0 && k == "resolve_ms" {
			parts = append(parts, fmt.Sprintf("结果图 %d", imageCount))
		}
		if imageCount > 0 && k == "download_ms" {
			parts = append(parts, fmt.Sprintf("下载 %d 张", imageCount))
		}
		tone := timelineTone(k, n)
		status := map[string]string{"danger": "异常", "warning": "慢", "info": "记录"}[tone]
		eventTime := ""
		for _, ev := range dicts(events) {
			if callInt(ev[k]) > 0 {
				eventTime = inlineValue(ev["time"])
				break
			}
		}
		steps[cat] = append(steps[cat], Map{"key": k, "label": label, "category": cat, "value_ms": n, "value_text": RequestDuration(n), "tone": tone, "status_label": status, "time": eventTime, "description": strings.Join(parts, " · ")})
	}
	for _, v := range timelineSpec.Categories {
		if len(steps[v[0]]) > 0 {
			groups = append(groups, Map{"key": v[0], "label": v[1], "steps": steps[v[0]]})
		}
		if len(segments) > 0 {
			legend = append(legend, Map{"key": v[0], "label": v[1], "category": v[0], "tone": "info"})
		}
	}
	if warning {
		legend = append(legend, Map{"key": "warning", "label": "超过阈值", "category": "state", "tone": "warning"})
	}
	if danger {
		legend = append(legend, Map{"key": "danger", "label": "异常中断", "category": "state", "tone": "danger"})
	}
	return Map{"segments": segments, "legend_items": legend, "groups": groups}
}

var requestKeyMask = regexp.MustCompile(`sk-[A-Za-z0-9_-]{6,}`)

func RequestDetailCore(summary, detail, monitor, timings Map, events any, imageCount int, shape Map, hideIdentity, suppressTimeline bool) Map {
	primary, diagnostics := []Map{}, []Map{}
	field := func(dst *[]Map, label string, v any, copyable, wide bool) {
		s := inlineValue(v)
		if s == "" || s == "-" || s == "null" || s == "undefined" {
			return
		}
		*dst = append(*dst, Map{"label": label, "value": s, "copyable": copyable, "wide": wide})
	}
	meta := object(detail["request_meta"])
	resolutions := RequestImageResolutions(detail)
	field(&primary, "请求 ID", orValue(detail["call_id"], summary["id"]), true, false)
	field(&primary, "接口", summary["endpoint"], true, false)
	field(&primary, "模型", summary["model"], true, false)
	field(&primary, "请求尺寸", meta["size"], false, false)
	field(&primary, "实际分辨率", resolutions, false, false)
	field(&primary, "请求质量", meta["quality"], false, false)
	field(&primary, "返回格式", meta["response_format"], false, false)
	field(&primary, "请求张数", meta["n"], false, false)
	if len(resolutions) > 0 {
		field(&primary, "出图张数", len(resolutions), false, false)
	}
	if !hideIdentity {
		field(&primary, "账号", summary["account_email"], true, false)
	}
	keyParts := []string{}
	for _, v := range []any{summary["key_name"], summary["key_id"]} {
		if s := callClean(v); s != "" {
			keyParts = append(keyParts, s)
		}
	}
	key := requestKeyMask.ReplaceAllStringFunc(strings.Join(keyParts, " / "), func(s string) string { return s[:5] + "***" + s[len(s)-4:] })
	field(&primary, "密钥", key, false, false)
	field(&primary, "出口", RequestEgress(detail, monitor), false, false)
	if !hideIdentity {
		field(&primary, "会话 ID", summary["conversation_id"], true, false)
	}
	started := callClean(orValue(summary["started_at"], summary["time"]))
	ended := callClean(orValue(summary["ended_at"], summary["updated_at"]))
	timeRange := started
	if started == "" {
		timeRange = ended
	} else if ended != "" && ended != started {
		timeRange += " → " + ended
	}
	field(&primary, "时间", timeRange, false, true)
	outcome := callClean(summary["outcome"])
	code := callInt(summary["status_code"])
	errorCode := callClean(summary["error_code"])
	publicError := callClean(summary["public_error"])
	reason := inlineValue(orValue(diagnosticRaw(detail, "reason"), diagnosticRaw(detail, "local_reason")))
	upType := inlineValue(orValue(diagnosticRaw(detail, "upstream_error_type"), diagnosticRaw(detail, "error_type")))
	upID := inlineValue(diagnosticRaw(detail, "upstream_request_id"))
	stage := callClean(orValue(diagnosticRaw(detail, "stage_label"), orValue(diagnosticRaw(detail, "stage"), orValue(monitor["stage_label"], monitor["stage"]))))
	if slices.Contains([]string{"success", "partial_success"}, outcome) && slices.Contains([]string{"success", "completed", "complete", "done", "完成"}, strings.ToLower(stage)) {
		stage = ""
	}
	tool := optionalBool(diagnosticRaw(detail, "tool_invoked"))
	blocked := optionalBool(diagnosticRaw(detail, "blocked"))
	anomaly := tool != nil && !*tool || blocked != nil && *blocked
	failure := outcome == "failed" || outcome == "rate_limited"
	showBool := failure || errorCode != "" || publicError != "" || reason != "" || anomaly
	if failure || code >= 400 || errorCode != "" || publicError != "" || reason != "" || upType != "" || upID != "" || stage != "" || anomaly {
		if code > 0 {
			field(&diagnostics, "状态码", code, false, false)
		}
		field(&diagnostics, "错误码", errorCode, true, false)
		field(&diagnostics, "阶段", stage, true, false)
		field(&diagnostics, "原因", reason, true, false)
		field(&diagnostics, "上游错误", upType, true, false)
		field(&diagnostics, "上游请求 ID", upID, true, false)
		v := detail["request_shape"]
		if len(object(v)) == 0 {
			v = shape
		}
		field(&diagnostics, "请求形状", v, true, false)
		if showBool && tool != nil {
			field(&diagnostics, "工具调用", *tool, false, false)
		}
		if showBool && blocked != nil {
			field(&diagnostics, "阻断", *blocked, false, false)
		}
		field(&diagnostics, "上游文本长度", diagnosticRaw(detail, "upstream_message_len"), false, false)
	}
	t := timings
	if suppressTimeline {
		t = Map{}
	}
	return Map{"primary_fields": primary, "diagnostic_fields": diagnostics, "auto_expand_timeline": outcome == "failed" || callInt(orValue(summary["duration_ms"], summary["elapsed_ms"])) >= 180000 || callInt(timings["stream_error_ms"]) > 0, "timeline": RequestTimeline(t, events, imageCount, shape)}
}
func optionalBool(v any) *bool {
	if v == nil {
		return nil
	}
	if b, ok := v.(bool); ok {
		return &b
	}
	s := strings.ToLower(callClean(v))
	if slices.Contains([]string{"1", "true", "yes", "on", "是"}, s) {
		b := true
		return &b
	}
	if slices.Contains([]string{"0", "false", "no", "off", "否"}, s) {
		b := false
		return &b
	}
	return nil
}
