package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type MetricDelta struct {
	Bucket, Model                                                        string
	Total, Success, Failed, SwitchRequests, SwitchCount, SwitchRecovered int64
	Duration                                                             float64
	DurationCount                                                        int64
	LastID, LastAt                                                       string
}
type DashboardData struct {
	State Document
	Hours map[string]Document
}
type DashboardMetrics struct{ database *Database }

func NewDashboardMetrics(d *Database) *DashboardMetrics { return &DashboardMetrics{d} }
func dashboardSchema(pg bool) []string {
	stamp := "DATETIME"
	if pg {
		stamp = "TIMESTAMP WITH TIME ZONE"
	}
	return []string{
		"CREATE TABLE IF NOT EXISTS dashboard_metric_state(id INTEGER NOT NULL PRIMARY KEY,call_record_generation VARCHAR(64) NOT NULL,last_sequence BIGINT NOT NULL,status VARCHAR(16) NOT NULL,failure_reason VARCHAR(255),last_event_id VARCHAR(64),last_event_at VARCHAR(40),checkpoint_at VARCHAR(40),retention_cutoff VARCHAR(13) NOT NULL,revision INTEGER NOT NULL,updated_at " + stamp + " NOT NULL)",
		"CREATE TABLE IF NOT EXISTS dashboard_metric_hourly(bucket_start VARCHAR(13) NOT NULL PRIMARY KEY,total BIGINT NOT NULL,success BIGINT NOT NULL,final_failed BIGINT NOT NULL,switch_requests BIGINT NOT NULL,switch_count BIGINT NOT NULL,switch_recovered BIGINT NOT NULL,success_duration_total_ms DOUBLE PRECISION NOT NULL,success_duration_count BIGINT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS dashboard_metric_model_hourly(bucket_start VARCHAR(13) NOT NULL REFERENCES dashboard_metric_hourly(bucket_start) ON DELETE CASCADE,model VARCHAR(255) NOT NULL,success BIGINT NOT NULL,success_duration_total_ms DOUBLE PRECISION NOT NULL,success_duration_count BIGINT NOT NULL,PRIMARY KEY(bucket_start,model))",
		"CREATE INDEX IF NOT EXISTS ix_dashboard_metric_model_hourly_model ON dashboard_metric_model_hourly(model,bucket_start)"}
}
func metricState(ctx context.Context, q queryer) (Document, error) {
	var generation, status, cutoff string
	var sequence int64
	var revision int
	var failure, lastID, lastAt, checkpoint sql.NullString
	e := q.QueryRowContext(ctx, "SELECT call_record_generation,last_sequence,status,failure_reason,last_event_id,last_event_at,checkpoint_at,retention_cutoff,revision FROM dashboard_metric_state WHERE id=1").Scan(&generation, &sequence, &status, &failure, &lastID, &lastAt, &checkpoint, &cutoff, &revision)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	nullable := func(v sql.NullString) any {
		if v.Valid {
			return v.String
		}
		return nil
	}
	return Document{"call_record_generation": generation, "last_sequence": sequence, "status": status, "failure_reason": nullable(failure), "last_event_id": nullable(lastID), "last_event_at": nullable(lastAt), "checkpoint_at": nullable(checkpoint), "retention_cutoff": cutoff, "revision": revision}, nil
}
func metricInt(v any) int64 {
	switch v := v.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}
func (r *DashboardMetrics) Sync(ctx context.Context, cutoff, checkpoint string, convert func(Document) MetricDelta) error {
	return r.database.transaction(ctx, true, "dashboard_metrics", func(q queryer) error {
		state, e := metricState(ctx, q)
		if e != nil {
			return e
		}
		var gen string
		// PostgreSQL sequence allocation is not commit ordering. Freeze the input
		// table before the generation row: a later committed sequence must not let
		// this checkpoint skip an earlier in-flight insert. This also covers Python
		// writers and follows their delete-before-generation-lock order.
		if r.database.postgres {
			if _, e = q.ExecContext(ctx, "LOCK TABLE call_records IN SHARE MODE"); e != nil {
				return e
			}
		}
		sourceQuery := "SELECT generation FROM call_record_state WHERE id=1"
		if r.database.postgres {
			sourceQuery += " FOR UPDATE"
		}
		if e = q.QueryRowContext(ctx, sourceQuery).Scan(&gen); e != nil {
			return e
		}
		var end int64
		if e = q.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM call_records").Scan(&end); e != nil {
			return e
		}
		start := int64(0)
		revision := 0
		var lastID, lastAt any
		rebuild := state != nil && (state["call_record_generation"] != gen || state["status"] != "ready" || state["failure_reason"] != nil)
		if state != nil {
			revision = int(metricInt(state["revision"]))
			lastID = state["last_event_id"]
			lastAt = state["last_event_at"]
			if !rebuild {
				start = metricInt(state["last_sequence"])
			}
		}
		if end < start {
			end = start
		}
		if rebuild {
			if _, e = q.ExecContext(ctx, "DELETE FROM dashboard_metric_model_hourly"); e != nil {
				return e
			}
			if _, e = q.ExecContext(ctx, "DELETE FROM dashboard_metric_hourly"); e != nil {
				return e
			}
			lastID = nil
			lastAt = nil
		}
		// A pre-existing aggregate with a missing state gets a fresh checkpoint, not
		// a duplicate replay of retained logs. This preserves the Python repair path.
		if state == nil {
			var n int
			if e = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM dashboard_metric_hourly").Scan(&n); e != nil {
				return e
			}
			if n > 0 {
				start = end
			}
		}
		if start == end && state != nil && state["retention_cutoff"] == cutoff && !rebuild {
			return nil
		}
		buckets := map[string]MetricDelta{}
		models := map[string]MetricDelta{}
		for start < end {
			rows, e := q.QueryContext(ctx, r.database.bind("SELECT sequence,payload FROM call_records WHERE type='call' AND sequence>? AND sequence<=? ORDER BY sequence LIMIT 500"), start, end)
			if e != nil {
				return e
			}
			seen := 0
			for rows.Next() {
				var seq int64
				var raw []byte
				if e = rows.Scan(&seq, &raw); e != nil {
					rows.Close()
					return e
				}
				start = seq
				seen++
				item, e := decode(raw)
				if e != nil {
					rows.Close()
					return e
				}
				delta := convert(item)
				if delta.LastID != "" {
					lastID = delta.LastID
				}
				if delta.LastAt != "" {
					lastAt = delta.LastAt
				}
				if delta.Bucket < cutoff || delta.Total == 0 {
					continue
				}
				v := buckets[delta.Bucket]
				v.Bucket = delta.Bucket
				v.Total += delta.Total
				v.Success += delta.Success
				v.Failed += delta.Failed
				v.SwitchRequests += delta.SwitchRequests
				v.SwitchCount += delta.SwitchCount
				v.SwitchRecovered += delta.SwitchRecovered
				v.Duration += delta.Duration
				v.DurationCount += delta.DurationCount
				buckets[delta.Bucket] = v
				if delta.Model != "" && delta.Success > 0 {
					k := delta.Bucket + "\x00" + delta.Model
					v := models[k]
					v.Bucket = delta.Bucket
					v.Model = delta.Model
					v.Success += delta.Success
					v.Duration += delta.Duration
					v.DurationCount += delta.DurationCount
					models[k] = v
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			if seen == 0 {
				break
			}
		}
		fields := []string{"total", "success", "final_failed", "switch_requests", "switch_count", "switch_recovered", "success_duration_total_ms", "success_duration_count"}
		updates := []string{}
		for _, f := range fields {
			updates = append(updates, f+"=dashboard_metric_hourly."+f+"+excluded."+f)
		}
		for _, v := range buckets {
			_, e = q.ExecContext(ctx, r.database.bind("INSERT INTO dashboard_metric_hourly(bucket_start,"+strings.Join(fields, ",")+") VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(bucket_start) DO UPDATE SET "+strings.Join(updates, ",")), v.Bucket, v.Total, v.Success, v.Failed, v.SwitchRequests, v.SwitchCount, v.SwitchRecovered, v.Duration, v.DurationCount)
			if e != nil {
				return e
			}
		}
		for _, v := range models {
			_, e = q.ExecContext(ctx, r.database.bind("INSERT INTO dashboard_metric_model_hourly(bucket_start,model,success,success_duration_total_ms,success_duration_count) VALUES(?,?,?,?,?) ON CONFLICT(bucket_start,model) DO UPDATE SET success=dashboard_metric_model_hourly.success+excluded.success,success_duration_total_ms=dashboard_metric_model_hourly.success_duration_total_ms+excluded.success_duration_total_ms,success_duration_count=dashboard_metric_model_hourly.success_duration_count+excluded.success_duration_count"), v.Bucket, v.Model, v.Success, v.Duration, v.DurationCount)
			if e != nil {
				return e
			}
		}
		if _, e = q.ExecContext(ctx, r.database.bind("DELETE FROM dashboard_metric_model_hourly WHERE bucket_start<?"), cutoff); e != nil {
			return e
		}
		if _, e = q.ExecContext(ctx, r.database.bind("DELETE FROM dashboard_metric_hourly WHERE bucket_start<?"), cutoff); e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, r.database.bind("INSERT INTO dashboard_metric_state(id,call_record_generation,last_sequence,status,failure_reason,last_event_id,last_event_at,checkpoint_at,retention_cutoff,revision,updated_at) VALUES(1,?,?,'ready',NULL,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET call_record_generation=excluded.call_record_generation,last_sequence=excluded.last_sequence,status=excluded.status,failure_reason=NULL,last_event_id=excluded.last_event_id,last_event_at=excluded.last_event_at,checkpoint_at=excluded.checkpoint_at,retention_cutoff=excluded.retention_cutoff,revision=excluded.revision,updated_at=excluded.updated_at"), gen, end, lastID, lastAt, checkpoint, cutoff, revision+1, r.database.documentTime())
		return e
	})
}
func (r *DashboardMetrics) Load(ctx context.Context, cutoff string) (DashboardData, error) {
	out := DashboardData{Hours: map[string]Document{}}
	e := r.database.transaction(ctx, false, "", func(q queryer) error {
		var e error
		out.State, e = metricState(ctx, q)
		if e != nil {
			return e
		}
		rows, e := q.QueryContext(ctx, r.database.bind("SELECT bucket_start,total,success,final_failed,switch_requests,switch_count,switch_recovered,success_duration_total_ms,success_duration_count FROM dashboard_metric_hourly WHERE bucket_start>=?"), cutoff)
		if e != nil {
			return e
		}
		for rows.Next() {
			var b string
			var a, c, d, f, g, h, j int64
			var i float64
			if e = rows.Scan(&b, &a, &c, &d, &f, &g, &h, &i, &j); e != nil {
				rows.Close()
				return e
			}
			out.Hours[b] = Document{"total": a, "success": c, "final_failed": d, "switch_requests": f, "switch_count": g, "switch_recovered": h, "success_duration_total_ms": i, "success_duration_count": j, "model_success": Document{}, "model_success_total_times": Document{}, "model_success_time_counts": Document{}}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		rows, e = q.QueryContext(ctx, r.database.bind("SELECT bucket_start,model,success,success_duration_total_ms,success_duration_count FROM dashboard_metric_model_hourly WHERE bucket_start>=?"), cutoff)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var b, m string
			var s, c int64
			var total float64
			if e = rows.Scan(&b, &m, &s, &total, &c); e != nil {
				return e
			}
			v := out.Hours[b]
			if v == nil {
				return fmt.Errorf("orphan metric model bucket")
			}
			v["model_success"].(Document)[m] = s
			v["model_success_total_times"].(Document)[m] = total
			v["model_success_time_counts"].(Document)[m] = c
		}
		return rows.Err()
	})
	return out, e
}
