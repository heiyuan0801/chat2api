package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type CallQuery struct {
	Type, StartDate, EndDate, Endpoint, Model, Account, ConversationID, Search string
	Outcomes                                                                   []string
}
type CallPage struct {
	Items             []Document
	Total, ImageCount int
	Facets            map[string]map[string]int
}

func callFilters(v CallQuery) (string, []any) {
	parts := []string{}
	args := []any{}
	for _, p := range []struct{ column, value string }{{"type", v.Type}, {"endpoint", v.Endpoint}, {"model", v.Model}, {"account_email", v.Account}, {"conversation_id", v.ConversationID}} {
		if p.value != "" {
			parts = append(parts, p.column+"=?")
			args = append(args, p.value)
		}
	}
	if v.StartDate != "" {
		parts = append(parts, "event_day>=?")
		args = append(args, v.StartDate)
	}
	if v.EndDate != "" {
		parts = append(parts, "event_day<=?")
		args = append(args, v.EndDate)
	}
	if len(v.Outcomes) > 0 {
		slots := []string{}
		for _, s := range v.Outcomes {
			slots = append(slots, "?")
			args = append(args, s)
		}
		parts = append(parts, "outcome IN ("+strings.Join(slots, ",")+")")
	}
	if v.Search != "" {
		parts = append(parts, "search_text LIKE ?")
		s := strings.ToLower(v.Search)
		args = append(args, "%"+s+"%")
	}
	if len(parts) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}
func (r *CallRecords) List(ctx context.Context, query CallQuery, limit, offset int) (CallPage, error) {
	out := CallPage{Items: []Document{}, Facets: map[string]map[string]int{}}
	where, args := callFilters(query)
	err := r.database.transaction(ctx, false, "", func(q queryer) error {
		if e := q.QueryRowContext(ctx, r.database.bind("SELECT COUNT(*) FROM call_records"+where), args...).Scan(&out.Total); e != nil {
			return e
		}
		clauses := where
		if clauses == "" {
			clauses = " WHERE "
		} else {
			clauses += " AND "
		}
		if e := q.QueryRowContext(ctx, r.database.bind("SELECT COUNT(*) FROM call_records"+clauses+"business_kind IN ('image_generation','image_edit','image_chat')"), args...).Scan(&out.ImageCount); e != nil {
			return e
		}
		for _, field := range []struct{ name, column string }{{"statuses", "outcome"}, {"endpoints", "endpoint"}, {"models", "model"}, {"accounts", "account_email"}} {
			rows, e := q.QueryContext(ctx, r.database.bind("SELECT "+field.column+",COUNT(*) FROM call_records"+where+" GROUP BY "+field.column), args...)
			if e != nil {
				return e
			}
			m := map[string]int{}
			for rows.Next() {
				var k string
				var n int
				if e = rows.Scan(&k, &n); e != nil {
					rows.Close()
					return e
				}
				if strings.TrimSpace(k) != "" {
					m[k] = n
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			out.Facets[field.name] = m
		}
		params := append(append([]any{}, args...), limit, offset)
		rows, e := q.QueryContext(ctx, r.database.bind("SELECT payload FROM call_records"+where+" ORDER BY sequence DESC LIMIT ? OFFSET ?"), params...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				return e
			}
			m, e := decode(raw)
			if e != nil {
				return e
			}
			out.Items = append(out.Items, m)
		}
		return rows.Err()
	})
	return out, err
}
func (r *CallRecords) Get(ctx context.Context, id string) (Document, error) {
	var raw []byte
	e := r.database.db.QueryRowContext(ctx, r.database.bind("SELECT payload FROM call_records WHERE id=?"), strings.TrimSpace(id)).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return decode(raw)
}
func (r *CallRecords) Delete(ctx context.Context, ids []string) (int, error) {
	seen := map[string]bool{}
	args := []any{}
	slots := []string{}
	for _, s := range ids {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			slots = append(slots, "?")
			args = append(args, s)
		}
	}
	if len(args) == 0 {
		return 0, nil
	}
	removed := 0
	e := r.database.transaction(ctx, true, "call_record_cursor", func(q queryer) error {
		for start := 0; start < len(args); start += 500 {
			end := min(start+500, len(args))
			result, e := q.ExecContext(ctx, r.database.bind("DELETE FROM call_records WHERE id IN ("+strings.Join(slots[start:end], ",")+")"), args[start:end]...)
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			removed += int(n)
		}
		return nil
	})
	return removed, e
}
func (r *CallRecords) Cleanup(ctx context.Context, cutoff string, dry bool) (Document, error) {
	out := Document{"removed": 0, "kept": 0, "removed_size_bytes": 0, "dry_run": dry}
	e := r.database.transaction(ctx, !dry, "call_record_cursor", func(q queryer) error {
		rows, e := q.QueryContext(ctx, r.database.bind("SELECT payload FROM call_records WHERE event_time<?"), cutoff)
		if e != nil {
			return e
		}
		n, size := 0, 0
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return e
			}
			payload, e := decode(raw)
			if e != nil {
				rows.Close()
				return e
			}
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if e = enc.Encode(payload); e != nil {
				rows.Close()
				return e
			}
			encoded := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
			size += len(encoded)
			quoted, escaped := false, false
			for _, c := range encoded {
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' && quoted {
					escaped = true
					continue
				}
				if c == '"' {
					quoted = !quoted
				}
				if !quoted && (c == ',' || c == ':') {
					size++
				}
			}
			n++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		var kept int
		if e = q.QueryRowContext(ctx, r.database.bind("SELECT COUNT(*) FROM call_records WHERE event_time>=?"), cutoff).Scan(&kept); e != nil {
			return e
		}
		if !dry {
			if _, e = q.ExecContext(ctx, r.database.bind("DELETE FROM call_records WHERE event_time<?"), cutoff); e != nil {
				return e
			}
		}
		out["removed"], out["kept"], out["removed_size_bytes"] = n, kept, size
		return nil
	})
	return out, e
}
