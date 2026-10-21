package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
)

type PromptState struct {
	Settings, Snapshot                 Document
	SettingsRevision, SnapshotRevision int
}
type PromptLibrary struct{ database *Database }

func NewPromptLibrary(d *Database) *PromptLibrary { return &PromptLibrary{d} }
func promptSchema(pg bool) []string {
	jsonType, stamp := "JSON", "DATETIME"
	if pg {
		jsonType, stamp = "JSONB", "TIMESTAMP WITH TIME ZONE"
	}
	return []string{"CREATE TABLE IF NOT EXISTS prompt_library_state(id INTEGER NOT NULL PRIMARY KEY,settings " + jsonType + " NOT NULL,snapshot " + jsonType + " NOT NULL,settings_revision INTEGER NOT NULL,snapshot_revision INTEGER NOT NULL,updated_at " + stamp + " NOT NULL)"}
}
func promptLoad(ctx context.Context, q queryer) (PromptState, error) {
	out := PromptState{Settings: Document{}, Snapshot: Document{}}
	var a, b []byte
	e := q.QueryRowContext(ctx, "SELECT settings,snapshot,settings_revision,snapshot_revision FROM prompt_library_state WHERE id=1").Scan(&a, &b, &out.SettingsRevision, &out.SnapshotRevision)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	out.Settings, e = decode(a)
	if e == nil {
		out.Snapshot, e = decode(b)
	}
	return out, e
}
func (r *PromptLibrary) Load(ctx context.Context) (PromptState, error) {
	return promptLoad(ctx, r.database.db)
}
func (r *PromptLibrary) Replace(ctx context.Context, field string, value Document) (PromptState, error) {
	if field != "settings" && field != "snapshot" {
		return PromptState{}, ErrInvalid
	}
	var out PromptState
	e := r.database.transaction(ctx, true, "prompt_library_state", func(q queryer) error {
		var e error
		out, e = promptLoad(ctx, q)
		if e != nil {
			return e
		}
		if field == "settings" {
			if reflect.DeepEqual(out.Settings, value) {
				return nil
			}
			out.Settings = Clone(value)
			out.SettingsRevision++
		} else {
			if reflect.DeepEqual(out.Snapshot, value) {
				return nil
			}
			out.Snapshot = Clone(value)
			out.SnapshotRevision++
		}
		a, e := json.Marshal(out.Settings)
		if e != nil {
			return e
		}
		b, e := json.Marshal(out.Snapshot)
		if e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, r.database.bind("INSERT INTO prompt_library_state(id,settings,snapshot,settings_revision,snapshot_revision,updated_at) VALUES(1,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET settings=excluded.settings,snapshot=excluded.snapshot,settings_revision=excluded.settings_revision,snapshot_revision=excluded.snapshot_revision,updated_at=excluded.updated_at"), string(a), string(b), out.SettingsRevision, out.SnapshotRevision, r.database.documentTime())
		return e
	})
	return out, e
}

// Revisions checks the shared owner before reusing a process-local projection;
// it avoids reading the potentially large snapshot when neither document changed.
func (r *PromptLibrary) Revisions(ctx context.Context) (int, int, error) {
	var a, b int
	e := r.database.db.QueryRowContext(ctx, "SELECT settings_revision,snapshot_revision FROM prompt_library_state WHERE id=1").Scan(&a, &b)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return a, b, e
}
