package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
)

func (a *ImageAssets) stripePath(rel string) string {
	hash := sha256.Sum256([]byte(rel))
	return a.index + ".item-locks/" + hex.EncodeToString(hash[:1]) + ".lock"
}
func fsWalk(root *os.Root, fn func(string) error) error {
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if !d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			return fn(name)
		}
		return nil
	})
}
func (a *ImageAssets) webdavRequest(ctx context.Context, client *http.Client, settings Map, method, rel string, data []byte) (int, error) {
	target, e := a.remoteURL(settings, rel)
	if e != nil {
		return 0, e
	}
	req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if e != nil {
		return 0, e
	}
	if text(settings["webdav_username"]) != "" || text(settings["webdav_password"]) != "" {
		req.SetBasicAuth(text(settings["webdav_username"]), text(settings["webdav_password"]))
	}
	req.Header.Set("Content-Type", "image/png")
	resp, e := client.Do(req)
	if e != nil {
		return 0, problem(400, "WebDAV request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if method == "MKCOL" && resp.StatusCode == 405 {
		return resp.StatusCode, nil
	}
	if method == "DELETE" && resp.StatusCode == 404 {
		return 404, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, problem(400, fmt.Sprintf("WebDAV %s failed: HTTP %d", method, resp.StatusCode))
	}
	return resp.StatusCode, nil
}
func (a *ImageAssets) remotePut(ctx context.Context, client *http.Client, settings Map, rel string, data []byte) (string, error) {
	target, e := a.remoteURL(settings, rel)
	if e != nil {
		return "", e
	}
	parsed, _ := url.Parse(target)
	base, _ := url.Parse(text(settings["webdav_url"]))
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	baseDepth := len(strings.Split(strings.Trim(base.Path, "/"), "/"))
	if strings.Trim(base.Path, "/") == "" {
		baseDepth = 0
	}
	for i := baseDepth + 1; i < len(parts); i++ {
		parent := *parsed
		parent.Path = "/" + strings.Join(parts[:i], "/") + "/"
		req, e := http.NewRequestWithContext(ctx, "MKCOL", parent.String(), nil)
		if e != nil {
			return "", e
		}
		if text(settings["webdav_username"]) != "" || text(settings["webdav_password"]) != "" {
			req.SetBasicAuth(text(settings["webdav_username"]), text(settings["webdav_password"]))
		}
		resp, e := client.Do(req)
		if e != nil {
			return "", problem(400, "WebDAV request failed")
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if (resp.StatusCode < 200 || resp.StatusCode >= 300) && resp.StatusCode != 405 {
			return "", problem(400, fmt.Sprintf("WebDAV MKCOL failed: HTTP %d", resp.StatusCode))
		}
	}
	if _, e = a.webdavRequest(ctx, client, settings, "PUT", rel, data); e != nil {
		return "", e
	}
	return target, nil
}

func (a *ImageAssets) remoteDelete(ctx context.Context, client *http.Client, settings Map, rel string) (bool, error) {
	status, e := a.webdavRequest(ctx, client, settings, "DELETE", rel, nil)
	if e == nil && status != 200 && status != 202 && status != 204 && status != 404 {
		e = problem(400, fmt.Sprintf("WebDAV DELETE failed: HTTP %d", status))
	}
	return e == nil && status != 404, e
}
func (a *ImageAssets) TestWebDAV(ctx context.Context) (Map, error) {
	settings, e := a.settings(ctx)
	if e != nil {
		return nil, e
	}
	if text(settings["webdav_url"]) == "" {
		return Map{"ok": false, "status": 0, "error": "WebDAV URL is required"}, nil
	}
	if _, e = a.remoteURL(settings, ""); e != nil {
		return Map{"ok": false, "status": 0, "error": "invalid WebDAV URL"}, nil
	}
	client, done, e := a.server.configuredHTTP(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	rel := ".chatgpt2api_webdav_test.txt"
	_, e = a.remotePut(ctx, client, settings, rel, []byte("chatgpt2api webdav test\n"))
	if e == nil {
		_, e = a.remoteDelete(ctx, client, settings, rel)
	}
	if e != nil {
		return Map{"ok": false, "status": 0, "error": e.Error()}, nil
	}
	return Map{"ok": true, "status": 200, "error": nil}, nil
}
func (a *ImageAssets) replayDelete(ctx context.Context, client *http.Client, settings Map, rel string) error {
	unlock, e := a.itemLock(ctx, rel)
	if e != nil {
		return e
	}
	defer unlock()
	var item any
	var tombstone Map
	e = a.catalog(ctx, func(_ Map, items Map) error {
		p, e := loadFileMap(a.pendingName())
		item = items[rel]
		tombstone = object(object(p["items"])[rel])
		return e
	})
	if e != nil || text(tombstone["op_id"]) == "" {
		return e
	}
	scope := text(tombstone["scope"])
	_, localError := a.localInfo(rel)
	superseded := scope == "asset" && !generationMatch(item, tombstone) || scope == "remote" && (item != nil || localError == nil)
	if !superseded {
		if scope == "asset" && localError == nil {
			if e = a.root.Remove(rel); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		if boolean(tombstone["remote"], false) {
			if _, e = a.remoteDelete(ctx, client, settings, rel); e != nil {
				return e
			}
		}
	}
	return a.catalog(ctx, func(catalog, items Map) error {
		p, e := loadFileMap(a.pendingName())
		if e != nil {
			return e
		}
		pending := object(p["items"])
		if !tombstoneMatch(object(pending[rel]), tombstone) {
			return nil
		}
		if !superseded && scope == "asset" && generationMatch(items[rel], tombstone) {
			delete(items, rel)
			if e = a.storeCatalog(catalog, items); e != nil {
				return e
			}
		}
		delete(pending, rel)
		p["items"] = pending
		return filepersist.WriteJSON(a.pendingName(), p)
	})
}
func (a *ImageAssets) Sync(ctx context.Context) (Map, error) {
	settings, e := a.settings(ctx)
	if e != nil {
		return nil, e
	}
	mode := text(settings["mode"])
	if mode != "webdav" && mode != "both" {
		return nil, problem(400, "WebDAV 图片存储未启用")
	}
	unlock, e := filepersist.Lock(ctx, a.index+".sync.lock")
	if e != nil {
		return nil, e
	}
	defer unlock()
	client, done, e := a.server.configuredHTTP(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	rels, e := a.localPaths()
	if e != nil {
		return nil, e
	}
	uploaded, skipped, failed := 0, 0, 0
	for _, rel := range rels {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		itemUnlock, e := a.itemLock(ctx, rel)
		if e != nil {
			return nil, e
		}
		var item, stone Map
		e = a.catalog(ctx, func(_ Map, items Map) error {
			item = copyMap(object(items[rel]))
			p, e := loadFileMap(a.pendingName())
			stone = object(object(p["items"])[rel])
			return e
		})
		if e != nil {
			itemUnlock()
			return nil, e
		}
		if text(stone["scope"]) == "asset" && generationMatch(itemOrNil(item), stone) {
			itemUnlock()
			continue
		}
		st, e := a.localInfo(rel)
		if e != nil {
			itemUnlock()
			continue
		}
		if boolean(item["webdav"], false) && !boolean(item["remote_sync_pending"], false) && integer(item["size"], -1) == int(st.Size()) {
			skipped++
			itemUnlock()
			continue
		}
		payload, e := a.localData(rel)
		var target string
		if e == nil {
			target, e = a.remotePut(ctx, client, settings, rel, payload)
		}
		if e != nil {
			failed++
			itemUnlock()
			continue
		}
		e = a.catalog(ctx, func(catalog, items Map) error {
			current := object(items[rel])
			updated := a.localEntry(rel, current, payload)
			updated["webdav"], updated["storage"], updated["remote_url"] = true, "both", target
			delete(updated, "remote_sync_pending")
			items[rel] = updated
			return a.storeCatalog(catalog, items)
		})
		itemUnlock()
		if e != nil {
			return nil, e
		}
		uploaded++
	}
	var pending Map
	e = a.catalog(ctx, func(_ Map, _ Map) error { p, e := loadFileMap(a.pendingName()); pending = object(p["items"]); return e })
	if e != nil {
		return nil, e
	}
	keys := []string{}
	for rel := range pending {
		if _, e := imageRelative(rel); e == nil {
			keys = append(keys, rel)
		}
	}
	sort.Strings(keys)
	for _, rel := range keys {
		if e = a.replayDelete(ctx, client, settings, rel); e != nil {
			failed++
		}
	}
	return Map{"uploaded": uploaded, "skipped": skipped, "failed": failed}, nil
}
func itemOrNil(m Map) any {
	if len(m) == 0 {
		return nil
	}
	return m
}
