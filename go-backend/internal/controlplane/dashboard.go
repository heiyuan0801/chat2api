package controlplane

import (
	"context"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Dashboard struct {
	s    *Server
	repo *repository.DashboardMetrics
	mu   sync.Mutex
}

func NewDashboard(s *Server) *Dashboard {
	return &Dashboard{s: s, repo: repository.NewDashboardMetrics(s.DB)}
}

var metricNotModels = map[string]bool{}

func init() {
	for _, s := range strings.Fields("- auto default unknown null none low medium high standard hd portrait landscape square vertical horizontal image images text chat generation generations edit edits") {
		metricNotModels[s] = true
	}
}

var metricDimension = regexp.MustCompile(`^\d+\s*[x:]\s*\d+$`)

func metricTime(v any) (time.Time, bool) {
	raw := text(v)
	for _, format := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02T15:04:05.999999", "2006-01-02"} {
		if t, e := time.ParseInLocation(format, raw, imageTaskZone); e == nil {
			return t.In(imageTaskZone), true
		}
	}
	return time.Time{}, false
}
func metricMidnight(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, imageTaskZone)
}
func (d *Dashboard) delta(item Map) repository.MetricDelta {
	now := d.s.now().In(imageTaskZone)
	out := repository.MetricDelta{LastID: text(item["id"])}
	if event, ok := metricTime(item["time"]); ok {
		out.LastAt = event.Format("2006-01-02T15:04:05")
	}
	started := callValue(item, "started_at")
	if !truth(started) {
		started = item["time"]
	}
	at, ok := metricTime(started)
	if !ok || at.Before(metricMidnight(now).AddDate(0, 0, -29)) || at.After(now) {
		return out
	}
	out.Bucket = at.Format("2006-01-02T15")
	out.Total = 1
	outcome := text(CallRecordProjection(item)["outcome"])
	switch outcome {
	case "success", "partial_success":
		out.Success = 1
	case "text_review":
	default:
		out.Failed = 1
	}
	out.SwitchCount = int64(callSwitchCount(item))
	if out.SwitchCount > 0 {
		out.SwitchRequests = 1
		out.SwitchRecovered = out.Success
	}
	if out.Success > 0 {
		if n, ok := number(callValue(item, "duration_ms")); ok {
			out.Duration = math.Max(0, n)
			out.DurationCount = 1
		}
		model := text(callValue(item, "model"))
		key := strings.ReplaceAll(strings.ToLower(model), "×", "x")
		if model != "" && !metricNotModels[key] && !strings.HasPrefix(key, "/") && !metricDimension.MatchString(key) {
			out.Model = model
		}
	}
	return out
}
func (d *Dashboard) Sync(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.s.now().In(imageTaskZone)
	return d.repo.Sync(ctx, metricMidnight(now).AddDate(0, 0, -29).Format("2006-01-02T15"), now.Format(time.RFC3339), d.delta)
}
func metricAdd(target, source Map) {
	for _, key := range []string{"total", "success", "final_failed", "switch_requests", "switch_count", "switch_recovered", "success_duration_total_ms", "success_duration_count"} {
		a, _ := number(target[key])
		b, _ := number(source[key])
		target[key] = a + b
	}
	for _, key := range []string{"model_success", "model_success_total_times", "model_success_time_counts"} {
		dest := object(target[key])
		for model, value := range object(source[key]) {
			a, _ := number(dest[model])
			b, _ := number(value)
			dest[model] = a + b
		}
		target[key] = dest
	}
}
func metricRound(n float64) float64 { return math.RoundToEven(n*100) / 100 }
func metricRatio(a, b any) any {
	x, _ := number(a)
	y, _ := number(b)
	if y <= 0 {
		return nil
	}
	return metricRound(x * 100 / y)
}
func metricAverage(total, count any) any {
	x, _ := number(total)
	n, _ := number(count)
	if n <= 0 {
		return nil
	}
	return metricRound(x / n)
}
func metricBucket(v Map) Map {
	success, failed := integer(v["success"], 0), integer(v["final_failed"], 0)
	return Map{"total_calls": integer(v["total"], 0), "success_calls": success, "final_failed_calls": failed, "success_rate": metricRatio(success, success+failed), "avg_success_duration_ms": metricAverage(v["success_duration_total_ms"], v["success_duration_count"]), "switch_requests": integer(v["switch_requests"], 0), "switch_count": integer(v["switch_count"], 0), "switch_recovered": integer(v["switch_recovered"], 0), "switch_recovery_rate": metricRatio(v["switch_recovered"], v["switch_requests"])}
}
func DashboardRange(hours map[string]Map, rangeName string, now time.Time) Map {
	now = now.In(imageTaskZone)
	count := map[string]int{"24h": 24, "7d": 7, "30d": 30}[rangeName]
	daily := rangeName != "24h"
	step := time.Hour
	format := "15:00"
	unit := "hour"
	start := now.Truncate(time.Hour)
	if daily {
		step = 24 * time.Hour
		format = "01-02"
		unit = "day"
		start = metricMidnight(now)
	}
	start = start.Add(-time.Duration(count-1) * step)
	buckets := []Map{}
	series := []Map{}
	labels := []string{}
	successes, failures, switches := []int{}, []int{}, []int{}
	rates := []any{}
	totals := Map{}
	models := map[string]bool{}
	for i := 0; i < count; i++ {
		at := start.Add(time.Duration(i) * step)
		v := Map{}
		if daily {
			prefix := at.Format("2006-01-02")
			for key, hour := range hours {
				if strings.HasPrefix(key, prefix) {
					metricAdd(v, hour)
				}
			}
		} else {
			v = object(hours[at.Format("2006-01-02T15")])
		}
		metricAdd(totals, v)
		series = append(series, v)
		for model := range object(v["model_success"]) {
			models[model] = true
		}
		metrics := metricBucket(v)
		label := at.Format(format)
		labels = append(labels, label)
		successes = append(successes, integer(metrics["success_calls"], 0))
		failures = append(failures, integer(metrics["final_failed_calls"], 0))
		switches = append(switches, integer(metrics["switch_count"], 0))
		rates = append(rates, metrics["success_rate"])
		bucket := Map{"label": label, "start_at": at.Format("2006-01-02T15:04:05"), "end_at": at.Add(step).Format("2006-01-02T15:04:05")}
		for _, k := range []string{"total_calls", "success_calls", "final_failed_calls", "success_rate", "avg_success_duration_ms", "switch_count", "switch_recovered", "switch_recovery_rate"} {
			bucket[k] = metrics[k]
		}
		buckets = append(buckets, bucket)
	}
	modelSuccess, modelAverage := Map{}, Map{}
	for name := range models {
		values := []int{}
		times := []any{}
		for _, v := range series {
			values = append(values, integer(object(v["model_success"])[name], 0))
			times = append(times, metricAverage(object(v["model_success_total_times"])[name], object(v["model_success_time_counts"])[name]))
		}
		modelSuccess[name] = values
		modelAverage[name] = times
	}
	m := metricBucket(totals)
	return Map{"time_range": rangeName, "window": Map{"requested": rangeName, "start_at": start.Format("2006-01-02T15:04:05"), "end_at": now.Format("2006-01-02T15:04:05"), "bucket_unit": unit, "bucket_count": count}, "totals": Map{"total": m["total_calls"], "success": m["success_calls"], "final_failed": m["final_failed_calls"], "success_rate": m["success_rate"], "avg_success_duration_ms": m["avg_success_duration_ms"]}, "switching": Map{"requests": m["switch_requests"], "count": m["switch_count"], "recovered": m["switch_recovered"], "recovery_rate": m["switch_recovery_rate"]}, "buckets": buckets, "trend": Map{"labels": labels, "success_requests": successes, "final_failed_requests": failures, "success_rate": rates, "switch_count": switches, "model_success_requests": modelSuccess, "model_avg_success_duration_ms": modelAverage}}
}
func (d *Dashboard) View(ctx context.Context) (Map, error) {
	if e := d.Sync(ctx); e != nil {
		return nil, e
	}
	now := d.s.now().In(imageTaskZone)
	snapshot, e := d.repo.Load(ctx, metricMidnight(now).AddDate(0, 0, -29).Format("2006-01-02T15"))
	if e != nil {
		return nil, e
	}
	state := snapshot.State
	fresh := any(nil)
	if at, ok := metricTime(state["last_event_at"]); ok {
		fresh = max(int64(0), now.Sub(at).Milliseconds())
	}
	ready := state["status"] == "ready" && text(state["call_record_generation"]) != ""
	status := "degraded"
	if ready {
		status = "ready"
	}
	metrics := Map{"status": status, "ready": ready, "stale": !ready, "source": "call_record_sequence", "source_revision": state["last_event_id"], "last_ingested_at": state["last_event_at"], "freshness_ms": fresh, "checkpoint_at": state["checkpoint_at"], "failure_reason": state["failure_reason"], "retention_days": 30}
	ranges := Map{}
	for _, name := range []string{"24h", "7d", "30d"} {
		ranges[name] = DashboardRange(snapshot.Hours, name, now)
	}
	accounts, e := d.s.accountStats(ctx)
	if e != nil {
		return nil, e
	}
	values, e := d.s.Settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	healthy := boolean(accounts["healthy"], false) && ready
	overall := "degraded"
	if healthy {
		overall = "ok"
	}
	active := 0
	if d.s.Monitor != nil {
		active = d.s.Monitor.Active()
	}
	return Map{"status": overall, "healthy": healthy, "version": d.s.version, "meta": Map{"schema_version": 5, "generated_at": now.Format(time.RFC3339), "available_ranges": []string{"24h", "7d", "30d"}}, "metrics": metrics, "runtime": d.s.runtimeSnapshot(), "operations": Map{"active_requests": active}, "accounts": accounts, "storage": Map{"application_database": Map{"type": "database", "db_type": d.s.DB.Backend(), "description": "应用数据库（" + d.s.DB.Backend() + "）", "database_url": d.s.DB.DisplayURL()}, "image_storage": Map{"enabled": boolean(object(values["image_storage"])["enabled"], false), "mode": orValue(object(values["image_storage"])["mode"], "local"), "status": "not_checked", "available": nil, "image_count": nil, "image_size_bytes": nil}}, "ranges": ranges}, nil
}
func (s *Server) accountStats(ctx context.Context) (Map, error) {
	items, e := s.Accounts.List(ctx)
	if e != nil {
		return nil, e
	}
	types := Map{}
	out := Map{"total": len(items), "cumulative_total": len(items), "active": 0, "limited": 0, "abnormal": 0, "disabled": 0, "total_quota": 0, "unlimited_quota_count": 0, "unknown_quota_count": 0, "total_success": 0, "total_fail": 0, "by_type": types}
	if raw, e := os.ReadFile(filepath.Join(s.DataDir, ".cumulative_total")); e == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(string(raw))); e == nil {
			out["cumulative_total"] = n
		}
	}
	for _, a := range items {
		key := map[string]string{"正常": "active", "限流": "limited", "异常": "abnormal", "禁用": "disabled"}[text(a["status"])]
		if key != "" {
			out[key] = integer(out[key], 0) + 1
		}
		if a["status"] == "正常" {
			out["total_quota"] = integer(out["total_quota"], 0) + max(0, integer(a["quota"], 0))
			if unlimited(a) {
				out["unlimited_quota_count"] = integer(out["unlimited_quota_count"], 0) + 1
			} else if boolean(a["image_quota_unknown"], false) || integer(a["quota"], 0) <= 0 {
				out["unknown_quota_count"] = integer(out["unknown_quota_count"], 0) + 1
			}
		}
		out["total_success"] = integer(out["total_success"], 0) + integer(a["success"], 0)
		out["total_fail"] = integer(out["total_fail"], 0) + integer(a["fail"], 0)
		kind := text(a["type"])
		if kind == "" {
			kind = "unknown"
		}
		types[kind] = integer(types[kind], 0) + 1
	}
	out["healthy"] = integer(out["active"], 0) > 0 || integer(out["unlimited_quota_count"], 0) > 0
	return out, nil
}
func (s *Server) registerDashboard() {
	s.route("GET", "/api/dashboard", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Dashboard.View(ctx) })
}
