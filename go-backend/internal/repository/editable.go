package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// EditableFiles is the SQL owner for Editable File Task metadata. Artifacts and
// resumable request bodies never enter this repository.
type EditableFiles struct{ db *Database }

func NewEditableFiles(db *Database) *EditableFiles { return &EditableFiles{db: db} }

func editableFileSchema(postgres bool) []string {
	jsonType, floatType := "JSON", "FLOAT"
	if postgres {
		jsonType, floatType = "JSONB", "DOUBLE PRECISION"
	}
	return []string{
		"CREATE TABLE IF NOT EXISTS editable_file_task (owner_id VARCHAR(256) NOT NULL,task_id VARCHAR(160) NOT NULL,storage_id VARCHAR(48) NOT NULL UNIQUE,status VARCHAR(24) NOT NULL,kind VARCHAR(8) NOT NULL,model VARCHAR(128),created_at VARCHAR(64) NOT NULL,updated_at VARCHAR(64) NOT NULL,created_ts " + floatType + " NOT NULL,updated_ts " + floatType + " NOT NULL,started_ts " + floatType + ",ended_ts " + floatType + ",result " + jsonType + ",error TEXT,account_email VARCHAR(320),delete_pending BOOLEAN NOT NULL,PRIMARY KEY(owner_id,task_id))",
		"CREATE INDEX IF NOT EXISTS ix_editable_file_task_status ON editable_file_task(status)",
		"CREATE INDEX IF NOT EXISTS ix_editable_file_task_updated_at ON editable_file_task(updated_at)",
		"CREATE INDEX IF NOT EXISTS ix_editable_file_task_delete_pending ON editable_file_task(delete_pending)",
	}
}

const editableColumns = "owner_id,task_id,storage_id,status,kind,model,created_at,updated_at,created_ts,updated_ts,started_ts,ended_ts,result,error,account_email,delete_pending"

func scanEditable(row interface{ Scan(...any) error }) (Document, error) {
	var owner, id, storage, status, kind, created, updated string
	var model, result, message, email sql.NullString
	var createdTS, updatedTS float64
	var started, ended sql.NullFloat64
	var pending bool
	e := row.Scan(&owner, &id, &storage, &status, &kind, &model, &created, &updated, &createdTS, &updatedTS, &started, &ended, &result, &message, &email, &pending)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	m := Document{"owner_id": owner, "id": id, "storage_id": storage, "status": status, "kind": kind, "created_at": created, "updated_at": updated, "created_ts": createdTS, "updated_ts": updatedTS}
	for key, v := range map[string]sql.NullString{"model": model, "error": message, "account_email": email} {
		if v.Valid && v.String != "" {
			m[key] = v.String
		}
	}
	if started.Valid {
		m["started_ts"] = started.Float64
	}
	if ended.Valid {
		m["ended_ts"] = ended.Float64
	}
	if pending {
		m["delete_pending"] = true
	}
	if result.Valid && result.String != "" && result.String != "null" {
		var value Document
		var decodeErr error
		value, decodeErr = decode([]byte(result.String))
		if decodeErr != nil {
			return nil, ErrInvalid
		}
		if len(value) > 0 {
			m["result"] = value
		}
	}
	return m, nil
}
func (r *EditableFiles) get(ctx context.Context, q queryer, owner, id string, lock bool) (Document, error) {
	query := "SELECT " + editableColumns + " FROM editable_file_task WHERE owner_id=? AND task_id=?"
	if lock && r.db.postgres {
		query += " FOR UPDATE"
	}
	return scanEditable(q.QueryRowContext(ctx, r.db.bind(query), owner, id))
}
func (r *EditableFiles) Get(ctx context.Context, owner, id string) (Document, error) {
	return r.get(ctx, r.db.db, owner, id, false)
}
func (r *EditableFiles) list(ctx context.Context, query string, args ...any) ([]Document, error) {
	rows, e := r.db.db.QueryContext(ctx, r.db.bind("SELECT "+editableColumns+" FROM editable_file_task "+query), args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		m, e := scanEditable(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (r *EditableFiles) List(ctx context.Context, owner string, ids []string, limit int) ([]Document, error) {
	query, args := "WHERE owner_id=?", []any{owner}
	if ids != nil {
		if len(ids) == 0 {
			return []Document{}, nil
		}
		query += " AND task_id IN (" + strings.TrimRight(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += " ORDER BY updated_at DESC,task_id ASC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return r.list(ctx, query, args...)
}
func (r *EditableFiles) Unfinished(ctx context.Context) ([]Document, error) {
	return r.list(ctx, "WHERE status IN ('queued','running')")
}
func (r *EditableFiles) PendingDeletion(ctx context.Context) ([]Document, error) {
	return r.list(ctx, "WHERE delete_pending=?", true)
}
func editableNumber(v any) any {
	if v == nil {
		return nil
	}
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return v
	}
}
func nullableEditable(v any) any {
	if v == nil || v == "" {
		return nil
	}
	return v
}
func (r *EditableFiles) Create(ctx context.Context, m Document) (Document, bool, error) {
	for _, key := range []string{"owner_id", "id", "storage_id", "status", "kind", "created_at", "updated_at"} {
		if s, _ := m[key].(string); strings.TrimSpace(s) == "" {
			return nil, false, ErrInvalid
		}
	}
	var result any
	if v, ok := m["result"].(map[string]any); ok {
		raw, e := json.Marshal(v)
		if e != nil {
			return nil, false, e
		}
		result = string(raw)
	}
	pending, _ := m["delete_pending"].(bool)
	var out Document
	created := false
	e := r.db.transaction(ctx, true, "editable-file-task:"+m["owner_id"].(string)+":"+m["id"].(string), func(q queryer) error {
		res, e := q.ExecContext(ctx, r.db.bind("INSERT INTO editable_file_task("+editableColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(owner_id,task_id) DO NOTHING"), m["owner_id"], m["id"], m["storage_id"], m["status"], m["kind"], nullableEditable(m["model"]), m["created_at"], m["updated_at"], editableNumber(m["created_ts"]), editableNumber(m["updated_ts"]), editableNumber(m["started_ts"]), editableNumber(m["ended_ts"]), result, nullableEditable(m["error"]), nullableEditable(m["account_email"]), pending)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		created = n > 0
		out, e = r.get(ctx, q, m["owner_id"].(string), m["id"].(string), false)
		return e
	})
	return out, created, e
}
func (r *EditableFiles) Update(ctx context.Context, owner, id string, updates Document) (Document, error) {
	allowed := map[string]bool{"status": true, "updated_at": true, "updated_ts": true, "started_ts": true, "ended_ts": true, "result": true, "error": true, "account_email": true, "delete_pending": true}
	keys := []string{}
	for key := range updates {
		if !allowed[key] {
			return nil, ErrInvalid
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out Document
	e := r.db.transaction(ctx, true, "editable-file-task:"+owner+":"+id, func(q queryer) error {
		prior, e := r.get(ctx, q, owner, id, true)
		if e != nil || prior == nil {
			return e
		}
		if len(keys) == 0 {
			out = prior
			return nil
		}
		sets, args := []string{}, []any{}
		for _, key := range keys {
			value := updates[key]
			switch key {
			case "result":
				if document, ok := value.(map[string]any); ok {
					raw, e := json.Marshal(document)
					if e != nil {
						return e
					}
					value = string(raw)
				} else {
					value = nil
				}
			case "started_ts", "ended_ts", "updated_ts":
				value = editableNumber(value)
			case "error", "account_email":
				value = nullableEditable(value)
			}
			sets = append(sets, key+"=?")
			args = append(args, value)
		}
		args = append(args, owner, id)
		_, e = q.ExecContext(ctx, r.db.bind("UPDATE editable_file_task SET "+strings.Join(sets, ",")+" WHERE owner_id=? AND task_id=?"), args...)
		if e != nil {
			return e
		}
		out, e = r.get(ctx, q, owner, id, false)
		return e
	})
	return out, e
}
func (r *EditableFiles) Delete(ctx context.Context, owner, id string) (bool, error) {
	var deleted bool
	e := r.db.transaction(ctx, true, "editable-file-task:"+owner+":"+id, func(q queryer) error {
		_, e := r.get(ctx, q, owner, id, true)
		if e != nil {
			return e
		}
		res, e := q.ExecContext(ctx, r.db.bind("DELETE FROM editable_file_task WHERE owner_id=? AND task_id=?"), owner, id)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		deleted = n > 0
		return e
	})
	return deleted, e
}
