// Package repository preserves the Python Application Database domains and CAS.
package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Document = map[string]any
type Snapshot struct {
	Items    []Document `json:"items"`
	Revision string     `json:"revision"`
}
type Mutation struct {
	Upserts          []Document
	DeleteKeys       []string
	ExpectedRevision *string
}
type MutationResult struct {
	Revision string `json:"revision"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Deleted  int    `json:"deleted"`
}

var ErrRevision = errors.New("storage revision conflict")
var ErrInvalid = errors.New("invalid storage mutation")

type Database struct {
	db           *sql.DB
	memoryAnchor *sql.DB
	postgres     bool
	rawURL       string
}
type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Open requires an explicit SQLAlchemy-compatible SQLite/PostgreSQL URL. It does
// not create tables or import legacy files. Call Initialize explicitly to do so.
func Open(ctx context.Context, raw string) (*Database, error) {
	var driver, dsn string
	pg := false
	switch {
	case strings.HasPrefix(raw, "sqlite:///"):
		driver = "sqlite"
		path := strings.TrimPrefix(raw, "sqlite:///")
		if path == "" {
			return nil, ErrInvalid
		}
		if path == ":memory:" {
			dsn = "file:go-control-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "?mode=memory&cache=shared"
		} else {
			absolute, e := filepath.Abs(path)
			if e != nil {
				return nil, ErrInvalid
			}
			dsn = (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=rw"
		}
		dsn += "&_pragma=foreign_keys(1)&_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	case strings.HasPrefix(raw, "postgresql://"), strings.HasPrefix(raw, "postgres://"), strings.HasPrefix(raw, "postgresql+psycopg2://"):
		driver = "pgx"
		pg = true
		dsn = strings.Replace(raw, "postgresql+psycopg2://", "postgresql://", 1)
	default:
		return nil, ErrInvalid
	}
	db, e := sql.Open(driver, dsn)
	if e != nil {
		return nil, errors.New("database configuration failed")
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	// SQLite in-memory databases cannot use WAL. One connection avoids shared
	// cache table-lock cycles; file databases retain the bounded concurrent pool.
	if raw == "sqlite:///:memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	db.SetConnMaxLifetime(time.Hour)
	if raw == "sqlite:///:memory:" {
		db.SetConnMaxLifetime(0)
	}
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, errors.New("database connection failed")
	}
	var anchor *sql.DB
	if raw == "sqlite:///:memory:" {
		// database/sql may discard the working connection when a query is
		// interrupted. Keep a separate idle connection alive so closing that
		// connection cannot delete the shared in-memory database and schema.
		anchor, e = sql.Open(driver, dsn)
		if e == nil {
			anchor.SetMaxOpenConns(1)
			anchor.SetMaxIdleConns(1)
			e = anchor.PingContext(ctx)
		}
		if e != nil {
			if anchor != nil {
				_ = anchor.Close()
			}
			_ = db.Close()
			return nil, errors.New("in-memory database initialization failed")
		}
	}
	return &Database{db: db, memoryAnchor: anchor, postgres: pg, rawURL: raw}, nil
}

// CreateSQLiteFile is an explicit opt-in for a new rehearsal database. Existing
// database files are not overwritten, converted or silently initialized.
func CreateSQLiteFile(raw string) error { return createSQLiteFile(raw) }
func (d *Database) Close() error {
	err := d.db.Close()
	if d.memoryAnchor != nil {
		err = errors.Join(err, d.memoryAnchor.Close())
	}
	return err
}
func (d *Database) Backend() string {
	if d.postgres {
		return "postgresql"
	}
	return "sqlite"
}
func (d *Database) bind(s string) string {
	if !d.postgres {
		return s
	}
	n := 0
	var b strings.Builder
	for _, r := range s {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (d *Database) transaction(ctx context.Context, write bool, documentLock string, fn func(queryer) error) (err error) {
	if d.postgres {
		tx, e := d.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if write && documentLock != "" {
			if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", documentLock); e != nil {
				return e
			}
		}
		if e = fn(tx); e != nil {
			return e
		}
		return tx.Commit()
	}
	conn, e := d.db.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	begin := "BEGIN"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	if _, e = conn.ExecContext(ctx, begin); e != nil {
		return e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cleanup, "ROLLBACK")
	}()
	if e = fn(conn); e != nil {
		return e
	}
	_, e = conn.ExecContext(ctx, "COMMIT")
	return e
}

func (d *Database) Initialize(ctx context.Context) error {
	id := "INTEGER NOT NULL PRIMARY KEY"
	jsonType := "JSON"
	dateType := "DATETIME"
	if d.postgres {
		id = "SERIAL PRIMARY KEY"
		jsonType = "JSONB"
		dateType = "TIMESTAMP WITH TIME ZONE"
	}
	statements := []string{
		"CREATE TABLE IF NOT EXISTS accounts (id " + id + ", access_token VARCHAR(2048) NOT NULL, data TEXT NOT NULL)",
		"CREATE UNIQUE INDEX IF NOT EXISTS ix_accounts_access_token ON accounts(access_token)",
		"CREATE TABLE IF NOT EXISTS auth_keys (id " + id + ", key_id VARCHAR(255) NOT NULL, data TEXT NOT NULL)",
		"CREATE UNIQUE INDEX IF NOT EXISTS ix_auth_keys_key_id ON auth_keys(key_id)",
		"CREATE TABLE IF NOT EXISTS storage_revisions (collection VARCHAR(32) NOT NULL PRIMARY KEY, version INTEGER NOT NULL)",
	}
	for _, name := range []string{"system_settings", "proxy_configuration", "account_group_configuration"} {
		statements = append(statements, "CREATE TABLE IF NOT EXISTS "+name+" (id INTEGER NOT NULL PRIMARY KEY, data "+jsonType+" NOT NULL, updated_at "+dateType+" NOT NULL)")
	}
	statements = append(statements, callRecordSchema(d.postgres)...)
	statements = append(statements, remoteImportSchema(d.postgres)...)
	statements = append(statements, editableFileSchema(d.postgres)...)
	statements = append(statements, coordinationSchema(d.postgres)...)
	statements = append(statements, promptSchema(d.postgres)...)
	statements = append(statements, dashboardSchema(d.postgres)...)
	return d.transaction(ctx, true, "go-control-schema", func(q queryer) error {
		for _, s := range statements {
			if _, e := q.ExecContext(ctx, s); e != nil {
				return e
			}
		}
		for _, collection := range []string{"accounts", "auth_keys"} {
			if _, e := q.ExecContext(ctx, d.bind("INSERT INTO storage_revisions(collection,version) VALUES (?,0) ON CONFLICT(collection) DO NOTHING"), collection); e != nil {
				return e
			}
		}
		generation, e := callRecordID()
		if e != nil {
			return e
		}
		if _, e = q.ExecContext(ctx, d.bind("INSERT INTO call_record_state(id,generation) VALUES (1,?) ON CONFLICT(id) DO NOTHING"), generation); e != nil {
			return e
		}
		return nil
	})
}
func spec(collection string) (table, key, field string, err error) {
	switch collection {
	case "accounts":
		return "accounts", "access_token", "access_token", nil
	case "auth_keys":
		return "auth_keys", "key_id", "id", nil
	}
	return "", "", "", ErrInvalid
}
func decode(raw []byte) (Document, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m Document
	if e := dec.Decode(&m); e != nil || m == nil {
		return nil, ErrInvalid
	}
	return m, nil
}
func Clone(m Document) Document {
	if m == nil {
		return Document{}
	}
	b, e := json.Marshal(m)
	if e != nil {
		panic("non-JSON document")
	}
	out, e := decode(b)
	if e != nil {
		panic("non-JSON document")
	}
	return out
}
func (d *Database) version(ctx context.Context, q queryer, collection string, lock bool) (int, error) {
	suffix := ""
	if lock && d.postgres {
		suffix = " FOR UPDATE"
	}
	var n int
	e := q.QueryRowContext(ctx, d.bind("SELECT version FROM storage_revisions WHERE collection=?")+suffix, collection).Scan(&n)
	return n, e
}
func revision(collection string, n int) string { return fmt.Sprintf("%s:%d", collection, n) }
func (d *Database) load(ctx context.Context, q queryer, collection string) ([]Document, error) {
	table, _, _, e := spec(collection)
	if e != nil {
		return nil, e
	}
	rows, e := q.QueryContext(ctx, "SELECT data FROM "+table+" ORDER BY id ASC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []Document{}
	for rows.Next() {
		var raw []byte
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		m, e := decode(raw)
		if e == nil {
			items = append(items, m)
		}
	}
	return items, rows.Err()
}
func (d *Database) Snapshot(ctx context.Context, collection string) (Snapshot, error) {
	if _, _, _, e := spec(collection); e != nil {
		return Snapshot{}, e
	}
	var out Snapshot
	e := d.transaction(ctx, false, "", func(q queryer) error {
		for range 3 {
			before, e := d.version(ctx, q, collection, false)
			if e != nil {
				return e
			}
			items, e := d.load(ctx, q, collection)
			if e != nil {
				return e
			}
			after, e := d.version(ctx, q, collection, false)
			if e != nil {
				return e
			}
			if before == after {
				out = Snapshot{Items: items, Revision: revision(collection, after)}
				return nil
			}
		}
		return ErrRevision
	})
	return out, e
}
func keyOf(m Document, field string) string { s, _ := m[field].(string); return strings.TrimSpace(s) }
func normalize(items []Document, field string) ([]Document, error) {
	order := []string{}
	by := map[string]Document{}
	for _, m := range items {
		k := keyOf(m, field)
		if k == "" {
			return nil, ErrInvalid
		}
		if _, ok := by[k]; ok {
			return nil, ErrInvalid
		}
		order = append(order, k)
		v := Clone(m)
		by[k] = v
	}
	out := make([]Document, 0, len(order))
	for _, k := range order {
		out = append(out, by[k])
	}
	return out, nil
}
func (d *Database) Mutate(ctx context.Context, collection string, m Mutation) (MutationResult, error) {
	_, _, field, e := spec(collection)
	if e != nil {
		return MutationResult{}, e
	}
	upserts, e := normalize(m.Upserts, field)
	if e != nil {
		return MutationResult{}, e
	}
	deleted := map[string]bool{}
	for _, k := range m.DeleteKeys {
		s := strings.TrimSpace(k)
		if s == "" {
			return MutationResult{}, ErrInvalid
		}
		deleted[s] = true
	}
	for _, v := range upserts {
		if deleted[keyOf(v, field)] {
			return MutationResult{}, ErrInvalid
		}
	}
	return d.modify(ctx, collection, m.ExpectedRevision, deleted, false, func(items []Document) ([]Document, error) {
		out := []Document{}
		incoming := map[string]Document{}
		for _, v := range upserts {
			incoming[keyOf(v, field)] = v
		}
		for _, v := range items {
			k := keyOf(v, field)
			if deleted[k] {
				continue
			}
			if next, ok := incoming[k]; ok {
				out = append(out, next)
				delete(incoming, k)
			} else {
				out = append(out, v)
			}
		}
		for _, v := range upserts {
			if _, ok := incoming[keyOf(v, field)]; ok {
				out = append(out, v)
			}
		}
		return out, nil
	})
}

// Modify locks the collection before calling fn. The callback must perform only
// local domain mutation, never network work. A nil revision is unconditional.
func (d *Database) Modify(ctx context.Context, collection string, expected *string, fn func([]Document) ([]Document, error)) (MutationResult, error) {
	return d.modify(ctx, collection, expected, nil, false, fn)
}
func (d *Database) Replace(ctx context.Context, collection string, expected *string, items []Document) (MutationResult, error) {
	return d.modify(ctx, collection, expected, nil, true, func([]Document) ([]Document, error) { return items, nil })
}
func (d *Database) modify(ctx context.Context, collection string, expected *string, explicitDeletes map[string]bool, replaceMalformed bool, fn func([]Document) ([]Document, error)) (MutationResult, error) {
	table, column, field, e := spec(collection)
	if e != nil {
		return MutationResult{}, e
	}
	var result MutationResult
	e = d.transaction(ctx, true, "", func(q queryer) error {
		version, e := d.version(ctx, q, collection, true)
		if e != nil {
			return e
		}
		result.Revision = revision(collection, version)
		if expected != nil && *expected != result.Revision {
			return ErrRevision
		}
		current, old, e := d.records(ctx, q, table, column)
		if e != nil {
			return e
		}
		// The callback may mutate its input in place. Keep the committed snapshot
		// independent so those edits cannot hide changes from the SQL diff.
		editable := make([]Document, len(current))
		for i, item := range current {
			editable[i] = Clone(item)
		}
		next, e := fn(editable)
		if e != nil {
			return e
		}
		next, e = normalize(next, field)
		if e != nil {
			return e
		}
		desired := map[string]bool{}
		for _, v := range next {
			k := keyOf(v, field)
			desired[k] = true
			b, e := json.Marshal(v)
			if e != nil {
				return e
			}
			if prior, ok := old[k]; !ok {
				_, e = q.ExecContext(ctx, d.bind("INSERT INTO "+table+"("+column+",data) VALUES (?,?)"), k, string(b))
				result.Inserted++
			} else if !reflect.DeepEqual(prior, v) {
				_, e = q.ExecContext(ctx, d.bind("UPDATE "+table+" SET data=? WHERE "+column+"=?"), string(b), k)
				result.Updated++
			}
			if e != nil {
				return e
			}
		}
		for k, prior := range old {
			if !desired[k] && (prior != nil || replaceMalformed || explicitDeletes[k]) {
				if _, e = q.ExecContext(ctx, d.bind("DELETE FROM "+table+" WHERE "+column+"=?"), k); e != nil {
					return e
				}
				result.Deleted++
			}
		}
		if result.Inserted+result.Updated+result.Deleted > 0 {
			version++
			if _, e = q.ExecContext(ctx, d.bind("UPDATE storage_revisions SET version=? WHERE collection=?"), version, collection); e != nil {
				return e
			}
		}
		result.Revision = revision(collection, version)
		return nil
	})
	return result, e
}
func documentTable(name string) bool {
	return name == "system_settings" || name == "proxy_configuration" || name == "account_group_configuration"
}
func (d *Database) readDocument(ctx context.Context, q queryer, name string) (Document, error) {
	var b []byte
	e := q.QueryRowContext(ctx, "SELECT data FROM "+name+" WHERE id=1").Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return Document{}, nil
	}
	if e != nil {
		return nil, e
	}
	m, e := decode(b)
	if e != nil && json.Valid(b) {
		return Document{}, nil
	}
	return m, e
}
func (d *Database) Document(ctx context.Context, name string) (Document, error) {
	if !documentTable(name) {
		return nil, ErrInvalid
	}
	return d.readDocument(ctx, d.db, name)
}
func (d *Database) UpdateDocument(ctx context.Context, name string, fn func(Document) (Document, error)) (Document, error) {
	if !documentTable(name) {
		return nil, ErrInvalid
	}
	var out Document
	e := d.transaction(ctx, true, name, func(q queryer) error {
		old, e := d.readDocument(ctx, q, name)
		if e != nil {
			return e
		}
		out, e = fn(Clone(old))
		if e != nil {
			return e
		}
		if out == nil {
			return ErrInvalid
		}
		out = Clone(out)
		if reflect.DeepEqual(old, out) {
			return nil
		}
		b, e := json.Marshal(out)
		if e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, d.bind("INSERT INTO "+name+"(id,data,updated_at) VALUES (1,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at"), string(b), d.documentTime())
		return e
	})
	return out, e
}

func (d *Database) documentTime() any {
	now := time.Now().UTC()
	if d.postgres {
		return now
	}
	return now.Format("2006-01-02 15:04:05.000000")
}

func (d *Database) records(ctx context.Context, q queryer, table, column string) ([]Document, map[string]Document, error) {
	rows, e := q.QueryContext(ctx, "SELECT "+column+",data FROM "+table+" ORDER BY id ASC")
	if e != nil {
		return nil, nil, e
	}
	defer rows.Close()
	items := []Document{}
	all := map[string]Document{}
	for rows.Next() {
		var key string
		var raw []byte
		if e = rows.Scan(&key, &raw); e != nil {
			return nil, nil, e
		}
		m, err := decode(raw)
		all[key] = m
		if err == nil {
			items = append(items, m)
		}
	}
	return items, all, rows.Err()
}

// DisplayURL is the established management projection, never a connection DSN.
func (d *Database) DisplayURL() string {
	if !d.postgres {
		return d.rawURL
	}
	u, e := url.Parse(strings.Replace(d.rawURL, "postgresql+psycopg2://", "postgresql://", 1))
	if e != nil {
		return "postgresql://[configured]"
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
	}
	return u.String()
}
