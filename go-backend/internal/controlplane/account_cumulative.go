package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
)

func readCumulative(name string, fallback int) int {
	raw, e := os.ReadFile(name)
	if e == nil {
		if value, e := strconv.Atoi(strings.TrimSpace(string(raw))); e == nil && value >= 0 {
			return value
		}
	}
	return fallback
}
func writeCumulative(name string, value int) error {
	f, e := os.CreateTemp(filepath.Dir(name), ".cumulative-total-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.WriteString(strconv.Itoa(value)); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), name)
}
func (a *Accounts) configureCumulative(ctx context.Context, dir string) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	name := filepath.Join(dir, ".cumulative_total")
	unlock, e := filepersist.Lock(ctx, name+".lock")
	if e != nil {
		return e
	}
	defer unlock()
	value := readCumulative(name, -1)
	if value < 0 {
		snapshot, e := a.db.Snapshot(ctx, "accounts")
		if e != nil {
			return e
		}
		if e = writeCumulative(name, len(snapshot.Items)); e != nil {
			return e
		}
	}
	a.cumulativeDir = dir
	return nil
}
func (a *Accounts) cumulativeBeforeAdd(ctx context.Context) (int, func(), error) {
	if a.cumulativeDir == "" {
		return 0, func() {}, nil
	}
	name := filepath.Join(a.cumulativeDir, ".cumulative_total")
	unlock, e := filepersist.Lock(ctx, name+".lock")
	if e != nil {
		return 0, nil, e
	}
	value := readCumulative(name, -1)
	if value < 0 {
		snapshot, e := a.db.Snapshot(ctx, "accounts")
		if e != nil {
			unlock()
			return 0, nil, e
		}
		value = len(snapshot.Items)
	}
	return value, unlock, nil
}
func (a *Accounts) cumulativeAdded(previous, added int) {
	if a.cumulativeDir != "" && added > 0 {
		_ = writeCumulative(filepath.Join(a.cumulativeDir, ".cumulative_total"), previous+added)
	}
}
