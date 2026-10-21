package controlplane

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
	_ "golang.org/x/image/webp"
)

const maxImageBytes = 50 << 20
const maxImagePixels = 64 << 20

type StoredImage struct {
	Path, URL, Storage string
	Width, Height      int
}

// ImageAssets is the sole native save/read owner. Destructive gallery operations
// remain on their existing catalog/tombstone seam for the management stage.
type ImageAssets struct {
	root      *os.Root
	index     string
	server    *Server
	jobs      *assetJobs
	closeOnce sync.Once
	closeErr  error
}

func NewImageAssets(s *Server, dir string) (*ImageAssets, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("explicit absolute data directory required")
	}
	if e := os.MkdirAll(filepath.Join(dir, "images"), 0700); e != nil {
		return nil, errors.New("image directory initialization failed")
	}
	root, e := os.OpenRoot(filepath.Join(dir, "images"))
	if e != nil {
		return nil, errors.New("image directory initialization failed")
	}
	a := &ImageAssets{root: root, index: filepath.Join(dir, "image_index.json"), server: s}
	a.startJobs()
	return a, nil
}
func (a *ImageAssets) Close() error {
	a.closeOnce.Do(func() {
		if a.jobs != nil {
			a.jobs.cancel()
			a.jobs.wg.Wait()
		}
		a.closeErr = a.root.Close()
	})
	return a.closeErr
}
func imageRelative(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	if raw == "" || strings.Contains(raw, ":") || strings.HasPrefix(raw, "/") {
		return "", os.ErrNotExist
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", os.ErrNotExist
		}
	}
	for _, part := range strings.Split(raw, "/") {
		if part == "" || part == "." || part == ".." {
			return "", os.ErrNotExist
		}
	}
	rel := path.Clean(raw)
	switch strings.ToLower(path.Ext(rel)) {
	case ".png", ".jpg", ".jpeg", ".webp":
	default:
		return "", os.ErrNotExist
	}
	return rel, nil
}
func imageDimensions(data []byte) (int, int, error) {
	if len(data) == 0 || len(data) > maxImageBytes {
		return 0, 0, imageProblem("invalid_image_input", 400, "image size is invalid", nil)
	}
	cfg, _, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 32768 || cfg.Height > 32768 || int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return 0, 0, imageProblem("invalid_image_input", 400, "image dimensions or format are invalid", nil)
	}
	return cfg.Width, cfg.Height, nil
}
func loadFileMap(name string) (Map, error) {
	m := Map{}
	e := filepersist.ReadJSON(name, &m)
	if errors.Is(e, os.ErrNotExist) {
		_, primaryErr := os.Stat(name)
		_, backupErr := os.Stat(name + ".bak")
		if errors.Is(primaryErr, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
			return Map{}, nil
		}
	}
	if e != nil {
		return nil, e
	}
	return m, nil
}
func (a *ImageAssets) Save(ctx context.Context, data []byte, base string) (StoredImage, error) {
	var out StoredImage
	if e := ctx.Err(); e != nil {
		return out, e
	}
	width, height, e := imageDimensions(data)
	if e != nil {
		return out, e
	}
	now := a.server.now()
	beijing := now.In(time.FixedZone("Asia/Shanghai", 8*3600))
	hash := md5.Sum(data)
	rel := fmt.Sprintf("%s/%d_%s.png", beijing.Format("2006/01/02"), now.Unix(), hex.EncodeToString(hash[:]))
	stripe := sha256.Sum256([]byte(rel))
	unlock, e := filepersist.Lock(ctx, a.index+".item-locks/"+hex.EncodeToString(stripe[:1])+".lock")
	if e != nil {
		return out, e
	}
	defer unlock()
	if e = ctx.Err(); e != nil {
		return out, e
	}
	raw, e := a.server.DB.Document(ctx, "system_settings")
	if e != nil {
		return out, e
	}
	settings := normalizeRoot("image_storage", object(raw["image_storage"]))
	mode := text(settings["mode"])
	if mode != "webdav" && mode != "both" {
		mode = "local"
	}
	local, remote := false, false
	remoteURL := ""
	// Once the physical save starts, finish its catalog commit even if the request
	// deadline elapses. This prevents success bytes becoming unindexed orphans.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	if mode == "local" || mode == "both" {
		if e = a.root.MkdirAll(path.Dir(rel), 0700); e != nil {
			return out, e
		}
		tmp := rel + "." + id(6) + ".tmp"
		if e = a.root.WriteFile(tmp, data, 0600); e != nil {
			return out, e
		}
		defer a.root.Remove(tmp)
		if e = a.root.Rename(tmp, rel); e != nil {
			return out, e
		}
		local = true
	}
	if mode == "webdav" || mode == "both" {
		remoteURL, e = a.webdav(commitCtx, settings, "PUT", rel, data)
		if e != nil {
			return out, e
		}
		remote = true
	}
	entry := Map{"rel": rel, "path": rel, "name": path.Base(rel), "date": beijing.Format("2006-01-02"), "size": len(data), "created_at": beijing.Format("2006-01-02 15:04:05"), "storage": mode, "local": local, "webdav": remote, "remote_url": remoteURL, "generation": id(16), "width": width, "height": height}
	catalogUnlock, e := filepersist.Lock(commitCtx, a.index+".lock")
	if e != nil {
		return out, e
	}
	defer catalogUnlock()
	catalog, e := loadFileMap(a.index)
	if e != nil {
		return out, e
	}
	items := object(catalog["items"])
	items[rel] = entry
	catalog["items"] = items
	if e = filepersist.WriteJSON(a.index, catalog); e != nil {
		return out, e
	}
	pendingFile := a.index + ".remote-deletes.json"
	pending, e := loadFileMap(pendingFile)
	if e != nil {
		return out, e
	}
	pendingItems := object(pending["items"])
	if _, ok := pendingItems[rel]; ok {
		delete(pendingItems, rel)
		pending["items"] = pendingItems
		if e = filepersist.WriteJSON(pendingFile, pending); e != nil {
			return out, e
		}
	}
	public := strings.TrimRight(text(settings["public_base_url"]), "/")
	if public == "" {
		if base == "" {
			values, lookupErr := a.server.Settings.Values(commitCtx)
			if lookupErr != nil {
				return out, lookupErr
			}
			base = text(values["base_url"])
		}
		public = strings.TrimRight(base, "/") + "/images"
	}
	return StoredImage{Path: rel, URL: public + "/" + rel, Storage: mode, Width: width, Height: height}, nil
}
func (a *ImageAssets) Read(ctx context.Context, raw string) ([]byte, string, error) {
	rel, e := imageRelative(raw)
	if e != nil {
		return nil, "", os.ErrNotExist
	}
	unlock, e := filepersist.Lock(ctx, a.index+".lock")
	if e != nil {
		return nil, "", e
	}
	catalog, e := loadFileMap(a.index)
	if e != nil {
		unlock()
		return nil, "", e
	}
	entry, found := object(catalog["items"])[rel]
	unlock()
	if !found {
		return nil, "", os.ErrNotExist
	}
	m := object(entry)
	var data []byte
	var f *os.File
	if boolean(m["local"], text(m["storage"]) == "local" || text(m["storage"]) == "both") {
		f, e = a.root.Open(rel)
	} else {
		e = os.ErrNotExist
	}
	if e == nil {
		defer f.Close()
		stat, se := f.Stat()
		if se != nil || !stat.Mode().IsRegular() || stat.Size() > maxImageBytes {
			return nil, "", os.ErrNotExist
		}
		data, e = io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	} else if boolean(m["webdav"], text(m["storage"]) == "webdav" || text(m["storage"]) == "both") {
		settings, lookupErr := a.server.DB.Document(ctx, "system_settings")
		if lookupErr != nil {
			return nil, "", lookupErr
		}
		data, e = a.webdavGet(ctx, normalizeRoot("image_storage", object(settings["image_storage"])), rel)
	}
	if e != nil || len(data) == 0 || len(data) > maxImageBytes {
		return nil, "", os.ErrNotExist
	}
	return data, http.DetectContentType(data), nil
}
func (a *ImageAssets) remoteURL(settings Map, rel string) (string, error) {
	base, e := url.Parse(text(settings["webdav_url"]))
	if e != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", problem(400, "invalid WebDAV URL")
	}
	root := strings.Trim(text(settings["webdav_root_path"]), "/")
	base.Path = strings.TrimRight(base.Path, "/")
	if root != "" {
		base.Path += "/" + root
	}
	base.Path += "/" + rel
	return base.String(), nil
}
func (a *ImageAssets) webdav(ctx context.Context, settings Map, method, rel string, data []byte) (string, error) {
	client, done, e := a.server.configuredHTTP(ctx)
	if e != nil {
		return "", e
	}
	defer done()
	var target string
	if method == "PUT" {
		target, e = a.remotePut(ctx, client, settings, rel, data)
	} else {
		_, e = a.webdavRequest(ctx, client, settings, method, rel, data)
		target, _ = a.remoteURL(settings, rel)
	}
	if e != nil {
		return "", imageProblem("upstream_unavailable", 502, "WebDAV request failed", nil)
	}
	return target, nil
}

func (a *ImageAssets) webdavGet(ctx context.Context, settings Map, rel string) ([]byte, error) {
	target, e := a.remoteURL(settings, rel)
	if e != nil {
		return nil, e
	}
	client, done, e := a.server.configuredHTTP(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
	req.SetBasicAuth(text(settings["webdav_username"]), text(settings["webdav_password"]))
	resp, e := client.Do(req)
	if e != nil {
		return nil, os.ErrNotExist
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.ContentLength > maxImageBytes {
		return nil, os.ErrNotExist
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
}
