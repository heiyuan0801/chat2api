package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"time"
)

func coordinationSchema(postgres bool) []string {
	jsonType, dateType := "JSON", "DATETIME"
	if postgres {
		jsonType, dateType = "JSONB", "TIMESTAMP WITH TIME ZONE"
	}
	out := []string{"CREATE TABLE IF NOT EXISTS application_coordination_lease (name VARCHAR(128) PRIMARY KEY, owner_token VARCHAR(64) NOT NULL, expires_at FLOAT NOT NULL, updated_at " + dateType + " NOT NULL)"}
	for _, table := range []string{"backup_execution_state", "retention_cleanup_state"} {
		out = append(out, "CREATE TABLE IF NOT EXISTS "+table+" (id INTEGER PRIMARY KEY, data "+jsonType+" NOT NULL, revision INTEGER NOT NULL, updated_at "+dateType+" NOT NULL)")
	}
	return out
}
func coordinationTable(name string) bool {
	return name == "backup_execution_state" || name == "retention_cleanup_state"
}
func (d *Database) CoordinationState(ctx context.Context, table string) (Document, error) {
	if !coordinationTable(table) {
		return nil, ErrInvalid
	}
	var raw []byte
	e := d.db.QueryRowContext(ctx, "SELECT data FROM "+table+" WHERE id=1").Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return Document{}, nil
	}
	if e != nil {
		return nil, e
	}
	return decode(raw)
}
func (d *Database) UpdateCoordinationState(ctx context.Context, table string, fn func(Document) (Document, error)) (Document, error) {
	if !coordinationTable(table) {
		return nil, ErrInvalid
	}
	var out Document
	e := d.transaction(ctx, true, table, func(q queryer) error {
		var raw []byte
		var rev int
		e := q.QueryRowContext(ctx, "SELECT data,revision FROM "+table+" WHERE id=1").Scan(&raw, &rev)
		old := Document{}
		if e == nil {
			old, e = decode(raw)
		} else if errors.Is(e, sql.ErrNoRows) {
			e = nil
		}
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
		if reflect.DeepEqual(old, out) && rev > 0 {
			return nil
		}
		b, e := json.Marshal(out)
		if e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, d.bind("INSERT INTO "+table+"(id,data,revision,updated_at) VALUES (1,?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data,revision=excluded.revision,updated_at=excluded.updated_at"), string(b), rev+1, d.documentTime())
		return e
	})
	return out, e
}

// CoordinationLease preserves Python's named process lease. PostgreSQL owns a
// session advisory lock; SQLite renews an owner-token guarded durable lease.
type CoordinationLease struct {
	db          *Database
	name, owner string
	ctx         context.Context
	cancel      context.CancelFunc
	conn        *sql.Conn
	once        sync.Once
	done        chan struct{}
	err         error
}

func (l *CoordinationLease) Context() context.Context { return l.ctx }
func (d *Database) TryCoordinationLease(ctx context.Context, name string, ttl time.Duration) (*CoordinationLease, error) {
	if name == "" || len(name) > 128 {
		return nil, ErrInvalid
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	owner, e := callRecordID()
	if e != nil {
		return nil, e
	}
	child, cancel := context.WithCancel(ctx)
	l := &CoordinationLease{db: d, name: name, owner: owner, ctx: child, cancel: cancel, done: make(chan struct{})}
	if d.postgres {
		l.conn, e = d.db.Conn(ctx)
		if e != nil {
			cancel()
			return nil, e
		}
		var ok bool
		e = l.conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", name).Scan(&ok)
		if e != nil || !ok {
			l.conn.Close()
			cancel()
			return nil, e
		}
		close(l.done)
		return l, nil
	}
	acquired := false
	e = d.transaction(ctx, true, "", func(q queryer) error {
		now := time.Now()
		var expires float64
		e := q.QueryRowContext(ctx, "SELECT expires_at FROM application_coordination_lease WHERE name=?", name).Scan(&expires)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil && expires > float64(now.UnixNano())/1e9 {
			return nil
		}
		_, e = q.ExecContext(ctx, "INSERT INTO application_coordination_lease(name,owner_token,expires_at,updated_at) VALUES (?,?,?,?) ON CONFLICT(name) DO UPDATE SET owner_token=excluded.owner_token,expires_at=excluded.expires_at,updated_at=excluded.updated_at", name, owner, float64(now.Add(ttl).UnixNano())/1e9, d.documentTime())
		acquired = e == nil
		return e
	})
	if e != nil || !acquired {
		cancel()
		return nil, e
	}
	go func() {
		defer close(l.done)
		t := time.NewTicker(max(500*time.Millisecond, ttl/3))
		defer t.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-t.C:
				result, e := d.db.ExecContext(child, "UPDATE application_coordination_lease SET expires_at=?,updated_at=? WHERE name=? AND owner_token=?", float64(time.Now().Add(ttl).UnixNano())/1e9, d.documentTime(), name, owner)
				if e != nil {
					cancel()
					return
				}
				n, e := result.RowsAffected()
				if e != nil || n != 1 {
					cancel()
					return
				}
			}
		}
	}()
	return l, nil
}
func (l *CoordinationLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.cancel()
		<-l.done
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if l.conn != nil {
			_, l.err = l.conn.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext($1))", l.name)
			if l.err != nil {
				_ = l.conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			l.err = errors.Join(l.err, l.conn.Close())
		} else {
			_, l.err = l.db.db.ExecContext(ctx, "DELETE FROM application_coordination_lease WHERE name=? AND owner_token=?", l.name, l.owner)
		}
	})
	return l.err
}
