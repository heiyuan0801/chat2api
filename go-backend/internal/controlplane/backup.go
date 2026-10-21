package controlplane

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// BackupService owns backup scheduling/execution. Archive bodies are spooled to
// private temporary files so configured image archives do not scale heap usage.
type BackupService struct {
	s       *Server
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	start   sync.Once
	mu      sync.Mutex
	closing bool
}

func NewBackupService(s *Server) (*BackupService, error) {
	ctx, cancel := context.WithCancel(s.executionContext)
	b := &BackupService{s: s, ctx: ctx, cancel: cancel}
	lease, e := s.DB.TryCoordinationLease(ctx, "backup_execution:run", 300*time.Second)
	if e != nil {
		cancel()
		return nil, e
	}
	if lease != nil {
		defer lease.Close()
		state, readErr := s.DB.CoordinationState(ctx, "backup_execution_state")
		if readErr != nil {
			cancel()
			return nil, readErr
		}
		if text(state["last_status"]) != "running" {
			return b, nil
		}
		_, e = s.DB.UpdateCoordinationState(ctx, "backup_execution_state", func(m Map) (Map, error) {
			if text(m["last_status"]) == "running" {
				m["last_finished_at"], m["last_status"], m["last_error"] = s.now().UTC().Format(time.RFC3339), "error", "备份执行被进程重启中断"
			}
			return m, nil
		})
	}
	if e != nil {
		cancel()
		return nil, e
	}
	return b, nil
}
func (b *BackupService) Start() {
	b.start.Do(func() {
		b.mu.Lock()
		if b.closing {
			b.mu.Unlock()
			return
		}
		b.wg.Add(1)
		b.mu.Unlock()
		go func() {
			defer b.wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-b.ctx.Done():
					return
				case <-ticker.C:
					_ = b.RunScheduled(b.ctx)
				}
			}
		}()
	})
}
func (b *BackupService) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closing = true
	b.mu.Unlock()
	b.cancel()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *BackupService) settings(ctx context.Context) (Map, error) {
	raw, e := b.s.DB.Document(ctx, "system_settings")
	return normalizeRoot("backup", object(raw["backup"])), e
}
func (b *BackupService) State(ctx context.Context) (Map, error) {
	m, e := b.s.DB.CoordinationState(ctx, "backup_execution_state")
	if e != nil {
		return nil, e
	}
	status := text(m["last_status"])
	if status != "idle" && status != "running" && status != "success" && status != "error" {
		status = "idle"
	}
	out := Map{"last_status": status, "running": status == "running"}
	for _, key := range []string{"last_started_at", "last_finished_at", "last_error", "last_object_key"} {
		if value := text(m[key]); value != "" {
			out[key] = value
		} else {
			out[key] = nil
		}
	}
	return out, nil
}
func (b *BackupService) client(ctx context.Context, settings Map) (*r2Client, func(), error) {
	client, done, e := b.s.configuredHTTP(ctx)
	return &r2Client{settings: settings, client: client, now: b.s.now}, done, e
}
func (b *BackupService) Test(ctx context.Context) (Map, error) {
	settings, e := b.settings(ctx)
	if e != nil {
		return nil, e
	}
	client, done, e := b.client(ctx, settings)
	if e != nil {
		return nil, e
	}
	defer done()
	return client.test(ctx)
}
func (b *BackupService) List(ctx context.Context) ([]Map, error) {
	settings, e := b.settings(ctx)
	if e != nil {
		return nil, e
	}
	if r2Missing(settings) != nil {
		return []Map{}, nil
	}
	client, done, e := b.client(ctx, settings)
	if e != nil {
		return nil, e
	}
	defer done()
	items, e := client.list(ctx)
	if e != nil {
		return nil, e
	}
	for _, item := range items {
		key := text(item["key"])
		item["name"], item["encrypted"] = path.Base(key), strings.HasSuffix(key, ".enc")
	}
	return items, nil
}
func (b *BackupService) Delete(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return problem(400, "备份对象 key 不能为空")
	}
	settings, e := b.settings(ctx)
	if e != nil {
		return e
	}
	client, done, e := b.client(ctx, settings)
	if e != nil {
		return e
	}
	defer done()
	return client.delete(ctx, key)
}
func (b *BackupService) RunScheduled(ctx context.Context) error {
	settings, e := b.settings(ctx)
	if e != nil {
		return e
	}
	if !boolean(settings["enabled"], false) {
		return nil
	}
	state, e := b.State(ctx)
	if e != nil {
		return e
	}
	if boolean(state["running"], false) {
		return nil
	}
	if last, e := time.Parse(time.RFC3339, text(state["last_finished_at"])); e == nil && b.s.now().Sub(last) < time.Duration(integer(settings["interval_minutes"], 360))*time.Minute {
		return nil
	}
	_, e = b.Run(ctx, "schedule")
	return e
}
func (b *BackupService) Run(parent context.Context, trigger string) (Map, error) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, context.Canceled
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()
	ctx, cancel := context.WithTimeout(parent, time.Hour)
	stop := context.AfterFunc(b.ctx, cancel)
	defer stop()
	defer cancel()
	lease, e := b.s.DB.TryCoordinationLease(ctx, "backup_execution:run", 300*time.Second)
	if e != nil {
		return nil, e
	}
	if lease == nil {
		return nil, problem(400, "当前已有备份任务正在执行")
	}
	defer lease.Close()
	ctx = lease.Context()
	current, e := b.State(ctx)
	if e != nil {
		return nil, e
	}
	started := b.s.now().UTC().Format(time.RFC3339)
	state := Map{"last_started_at": started, "last_finished_at": current["last_finished_at"], "last_status": "running", "last_error": nil, "last_object_key": current["last_object_key"]}
	if _, e = b.s.DB.UpdateCoordinationState(ctx, "backup_execution_state", func(Map) (Map, error) { return state, nil }); e != nil {
		return nil, e
	}
	result, runErr := b.runOnce(ctx, trigger)
	state["last_finished_at"] = b.s.now().UTC().Format(time.RFC3339)
	if runErr != nil {
		state["last_status"], state["last_error"] = "error", safeBackupError(runErr)
	} else {
		state["last_status"], state["last_error"], state["last_object_key"] = "success", nil, result["key"]
	}
	commit, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCommit()
	_, stateErr := b.s.DB.UpdateCoordinationState(commit, "backup_execution_state", func(Map) (Map, error) { return state, nil })
	return result, errors.Join(runErr, stateErr)
}
func safeBackupError(e error) string {
	var p *Problem
	if errors.As(e, &p) {
		return p.Message
	}
	if errors.Is(e, context.Canceled) {
		return "备份执行被中断"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return "备份执行超时"
	}
	return "创建或上传备份失败"
}
func (b *BackupService) runOnce(ctx context.Context, trigger string) (Map, error) {
	settings, e := b.settings(ctx)
	if e != nil {
		return nil, e
	}
	if e = r2Missing(settings); e != nil {
		return nil, e
	}
	encrypted := boolean(settings["encrypt"], false)
	if encrypted && text(settings["passphrase"]) == "" {
		return nil, problem(400, "已启用备份加密，但未设置加密口令")
	}
	tmp, e := os.MkdirTemp(b.s.DataDir, ".backup-*")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(tmp)
	snapshot := filepath.Join(tmp, "database")
	if e = b.s.DB.BackupToFile(ctx, snapshot); e != nil {
		return nil, e
	}
	archivePath := filepath.Join(tmp, "archive")
	if e = b.archive(ctx, archivePath, snapshot, settings, trigger); e != nil {
		return nil, e
	}
	if encrypted {
		encoded := filepath.Join(tmp, "encrypted")
		if e = encryptBackup(ctx, archivePath, encoded, text(settings["passphrase"])); e != nil {
			return nil, e
		}
		archivePath = encoded
	}
	payload, e := os.Open(archivePath)
	if e != nil {
		return nil, e
	}
	defer payload.Close()
	st, e := payload.Stat()
	if e != nil {
		return nil, e
	}
	suffix := ".tar.gz"
	if encrypted {
		suffix += ".enc"
	}
	prefix := text(settings["prefix"])
	if prefix == "" {
		prefix = "backups"
	}
	key := strings.TrimRight(prefix, "/") + "/backup-" + b.s.now().UTC().Format("20060102T150405Z") + "-" + id(2) + suffix
	client, done, e := b.client(ctx, settings)
	if e != nil {
		return nil, e
	}
	defer done()
	if e = client.upload(ctx, key, payload, map[string]string{"created-at": b.s.now().UTC().Format(time.RFC3339), "encrypted": fmt.Sprint(encrypted), "trigger": trigger}); e != nil {
		return nil, e
	}
	keep := integer(settings["rotation_keep"], 0)
	if keep > 0 {
		items, e := client.list(ctx)
		if e != nil {
			return nil, e
		}
		backups := []Map{}
		for _, item := range items {
			name := path.Base(text(item["key"]))
			if strings.HasPrefix(name, "backup-") && (strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tar.gz.enc")) {
				backups = append(backups, item)
			}
		}
		if len(backups) > keep {
			for _, item := range backups[keep:] {
				if e = client.delete(ctx, text(item["key"])); e != nil {
					return nil, e
				}
			}
		}
	}
	return Map{"key": key, "size": st.Size(), "encrypted": encrypted}, nil
}
func (b *BackupService) archive(ctx context.Context, destination, snapshot string, settings Map, trigger string) (err error) {
	out, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	gz := gzip.NewWriter(out)
	defer func() { err = errors.Join(err, gz.Close()) }()
	tarball := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, tarball.Close()) }()
	backend := b.s.DB.Backend()
	metadata := Map{"version": 3, "created_at": b.s.now().UTC().Format(time.RFC3339), "trigger": trigger, "app_version": b.s.version, "database_backend": backend, "application_database": b.s.DB.BackupInfo()}
	data, e := json.MarshalIndent(metadata, "", "  ")
	if e != nil {
		return e
	}
	if e = tarball.WriteHeader(&tar.Header{Name: "backup-metadata.json", Mode: 0600, Size: int64(len(data)), ModTime: b.s.now()}); e != nil {
		return e
	}
	if _, e = tarball.Write(data); e != nil {
		return e
	}
	addFile := func(source, name string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		f, e := os.Open(source)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return nil
		}
		if e = tarball.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: st.Size(), ModTime: st.ModTime()}); e != nil {
			return e
		}
		_, e = io.CopyN(tarball, &contextReader{ctx: ctx, r: f}, st.Size())
		return e
	}
	name := "data/application-database.sqlite3"
	if backend == "postgresql" {
		name = "data/application-database.pgdump"
	}
	if e = addFile(snapshot, name); e != nil {
		return e
	}
	if e = addFile(filepath.Join(b.s.DataDir, ".cumulative_total"), "data/.cumulative_total"); e != nil {
		return e
	}
	include := object(settings["include"])
	if boolean(include["image_tasks"], false) {
		for _, name := range []string{"image_tasks.json", "image_index.json", "image_index.json.remote-deletes.json"} {
			if e = addFile(filepath.Join(b.s.DataDir, name), "data/"+name); e != nil {
				return e
			}
		}
	}
	if boolean(include["images"], false) {
		if e = addFile(filepath.Join(b.s.DataDir, "image_tags.json"), "data/image_tags.json"); e != nil {
			return e
		}
	}
	for _, pair := range []struct{ key, dir string }{{"images", "images"}, {"editable_files", "files"}} {
		if !boolean(include[pair.key], false) {
			continue
		}
		dir := filepath.Join(b.s.DataDir, pair.dir)
		root, e := os.OpenRoot(dir)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		e = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() && (rel == ".staging" || strings.HasPrefix(rel, ".staging/")) {
				return fs.SkipDir
			}
			if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			f, e := root.Open(rel)
			if e != nil {
				return e
			}
			defer f.Close()
			st, e := f.Stat()
			if e != nil || !st.Mode().IsRegular() {
				return e
			}
			if e = tarball.WriteHeader(&tar.Header{Name: "data/" + pair.dir + "/" + rel, Mode: 0600, Size: st.Size(), ModTime: st.ModTime()}); e != nil {
				return e
			}
			_, e = io.CopyN(tarball, &contextReader{ctx, f}, st.Size())
			return e
		})
		root.Close()
		if e != nil {
			return e
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.r.Read(p)
}
func encryptBackup(ctx context.Context, source, destination, passphrase string) (err error) {
	in, e := os.Open(source)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	salt := make([]byte, 8)
	if _, e = rand.Read(salt); e != nil {
		return e
	}
	material := pbkdf2.Key([]byte(passphrase), salt, 10000, 48, sha256.New)
	block, e := aes.NewCipher(material[:32])
	if e != nil {
		return e
	}
	cbc := cipher.NewCBCEncrypter(block, material[32:])
	if _, e = out.Write(append([]byte("Salted__"), salt...)); e != nil {
		return e
	}
	buf := make([]byte, 64<<10)
	pending := []byte{}
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		n, readErr := in.Read(buf)
		pending = append(pending, buf[:n]...)
		complete := len(pending) / aes.BlockSize * aes.BlockSize
		if readErr == io.EOF {
			padding := aes.BlockSize - len(pending)%aes.BlockSize
			for range padding {
				pending = append(pending, byte(padding))
			}
			complete = len(pending)
		} else if readErr != nil {
			return readErr
		}
		if complete > 0 {
			encrypted := make([]byte, complete)
			cbc.CryptBlocks(encrypted, pending[:complete])
			if _, e = out.Write(encrypted); e != nil {
				return e
			}
			pending = append(pending[:0], pending[complete:]...)
		}
		if readErr == io.EOF {
			return nil
		}
	}
}
