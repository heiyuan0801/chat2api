package repository

import (
	"errors"
	"os"
	"strings"
)

func createSQLiteFile(raw string) error {
	if !strings.HasPrefix(raw, "sqlite:///") {
		return errors.New("SQLite URL required")
	}
	path := strings.TrimPrefix(raw, "sqlite:///")
	if path == "" || path == ":memory:" {
		return ErrInvalid
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(e) {
		return nil
	}
	if e != nil {
		return errors.New("database file creation failed")
	}
	return f.Close()
}
