package repository

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// BackupToFile writes a transactionally consistent database snapshot. It never
// copies a live SQLite database or its WAL. PostgreSQL uses its custom dump.
func (d *Database) BackupToFile(ctx context.Context, destination string) error {
	if _, e := os.Stat(destination); e == nil {
		return errors.New("backup destination already exists")
	}
	if d.postgres {
		return d.postgresBackup(ctx, destination)
	}
	conn, e := d.db.Conn(ctx)
	if e != nil {
		return errors.New("创建 SQLite Application Database 快照失败")
	}
	defer conn.Close()
	e = conn.Raw(func(raw any) (err error) {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return ErrInvalid
		}
		backup, e := source.NewBackup(destination)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, backup.Finish()) }()
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			more, e := backup.Step(128)
			if e != nil {
				return e
			}
			if !more {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		return nil
	})
	if e != nil {
		os.Remove(destination)
		return errors.New("创建 SQLite Application Database 快照失败")
	}
	copied, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: destination}).String()+"?mode=ro")
	if e != nil {
		return e
	}
	defer copied.Close()
	var result string
	if e = copied.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); e != nil || result != "ok" {
		os.Remove(destination)
		return errors.New("SQLite 快照完整性检查失败")
	}
	return os.Chmod(destination, 0600)
}
func (d *Database) postgresBackup(ctx context.Context, destination string) error {
	parsed, e := url.Parse(strings.Replace(d.rawURL, "postgresql+psycopg2://", "postgresql://", 1))
	if e != nil {
		return ErrInvalid
	}
	args := []string{"--format=custom", "--no-owner", "--no-privileges", "--file", destination}
	if parsed.Hostname() != "" {
		args = append(args, "--host", parsed.Hostname())
	}
	if parsed.Port() != "" {
		if _, e = strconv.Atoi(parsed.Port()); e != nil {
			return ErrInvalid
		}
		args = append(args, "--port", parsed.Port())
	}
	env := os.Environ()
	if parsed.User != nil {
		args = append(args, "--username", parsed.User.Username())
		if password, ok := parsed.User.Password(); ok {
			env = append(env, "PGPASSWORD="+password)
		}
	}
	if db := strings.TrimPrefix(parsed.Path, "/"); db != "" {
		args = append(args, "--dbname", db)
	}
	if ssl := parsed.Query().Get("sslmode"); ssl != "" {
		env = append(env, "PGSSLMODE="+ssl)
	}
	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Env = env
	if e = cmd.Run(); e != nil {
		os.Remove(destination)
		if errors.Is(e, exec.ErrNotFound) {
			return errors.New("当前环境缺少 pg_dump，无法备份 PostgreSQL")
		}
		return errors.New("PostgreSQL 备份失败：pg_dump 执行失败")
	}
	st, e := os.Stat(destination)
	if e != nil || st.Size() == 0 {
		os.Remove(destination)
		return errors.New("PostgreSQL 备份失败：pg_dump 未输出数据")
	}
	return os.Chmod(destination, 0600)
}

// BackupInfo excludes all database credentials from archive metadata.
func (d *Database) BackupInfo() Document {
	raw := strings.Replace(d.rawURL, "postgresql+psycopg2://", "postgresql://", 1)
	u, e := url.Parse(raw)
	if e == nil && u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "***")
		raw = u.String()
	}
	return Document{"type": "database", "db_type": d.Backend(), "description": "应用数据库（" + d.Backend() + "）", "database_url": raw}
}
