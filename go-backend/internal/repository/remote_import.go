package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
)

// Remote import configuration has its own revision and transaction boundary.
// The global lock matches Python and makes reservation atomic across providers.
func remoteImportSchema(postgres bool) []string {
	j, t := "JSON", "DATETIME"
	if postgres {
		j, t = "JSONB", "TIMESTAMP WITH TIME ZONE"
	}
	return []string{"CREATE TABLE IF NOT EXISTS remote_import_configurations (provider VARCHAR(32) NOT NULL PRIMARY KEY, items " + j + " NOT NULL, revision INTEGER NOT NULL, updated_at " + t + " NOT NULL)"}
}
func remoteProvider(provider string) bool { return provider == "cpa" || provider == "sub2api" }
func (d *Database) readRemoteImports(ctx context.Context, q queryer, provider string) ([]Document, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, d.bind("SELECT items FROM remote_import_configurations WHERE provider=?"), provider).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []Document{}, nil
	}
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return nil, ErrInvalid
	}
	values, ok := decoded.([]any)
	if !ok {
		return nil, ErrInvalid
	}
	items := []Document{}
	for _, value := range values {
		if m, ok := value.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items, nil
}
func (d *Database) RemoteImports(ctx context.Context, provider string) ([]Document, error) {
	if !remoteProvider(provider) {
		return nil, ErrInvalid
	}
	return d.readRemoteImports(ctx, d.db, provider)
}
func cloneRemoteItems(items []Document) []Document {
	out := make([]Document, 0, len(items))
	for _, m := range items {
		if m != nil {
			out = append(out, Clone(m))
		}
	}
	return out
}
func (d *Database) writeRemoteImports(ctx context.Context, q queryer, provider string, items []Document) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, d.bind("INSERT INTO remote_import_configurations(provider,items,revision,updated_at) VALUES (?,?,1,?) ON CONFLICT(provider) DO UPDATE SET items=excluded.items,revision=remote_import_configurations.revision+1,updated_at=excluded.updated_at"), provider, string(raw), d.documentTime())
	return err
}
func (d *Database) UpdateRemoteImports(ctx context.Context, provider string, fn func([]Document) ([]Document, error)) ([]Document, error) {
	if !remoteProvider(provider) {
		return nil, ErrInvalid
	}
	var out []Document
	err := d.transaction(ctx, true, "remote_import_configuration:global", func(q queryer) error {
		old, err := d.readRemoteImports(ctx, q, provider)
		if err != nil {
			return err
		}
		out, err = fn(cloneRemoteItems(old))
		if err != nil {
			return err
		}
		if out == nil {
			return ErrInvalid
		}
		out = cloneRemoteItems(out)
		// A first update creates a row even for an empty collection, as Python does.
		var n int
		err = q.QueryRowContext(ctx, d.bind("SELECT revision FROM remote_import_configurations WHERE provider=?"), provider).Scan(&n)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || !reflect.DeepEqual(old, out) {
			return d.writeRemoteImports(ctx, q, provider, out)
		}
		return nil
	})
	return out, err
}
func (d *Database) UpdateAllRemoteImports(ctx context.Context, fn func(map[string][]Document) error) error {
	return d.transaction(ctx, true, "remote_import_configuration:global", func(q queryer) error {
		current := map[string][]Document{}
		old := map[string][]Document{}
		for _, provider := range []string{"cpa", "sub2api"} {
			var n int
			err := q.QueryRowContext(ctx, d.bind("SELECT revision FROM remote_import_configurations WHERE provider=?"), provider).Scan(&n)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			items, err := d.readRemoteImports(ctx, q, provider)
			if err != nil {
				return err
			}
			old[provider] = items
			current[provider] = cloneRemoteItems(items)
		}
		if err := fn(current); err != nil {
			return err
		}
		if len(current) != len(old) {
			return ErrInvalid
		}
		for provider, items := range current {
			if _, ok := old[provider]; !ok || items == nil {
				return ErrInvalid
			}
			if !reflect.DeepEqual(items, old[provider]) {
				if err := d.writeRemoteImports(ctx, q, provider, cloneRemoteItems(items)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
