package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
	"golang.org/x/sys/unix"
)

func (a *ImageAssets) itemLocks(ctx context.Context, rels []string) (func(), error) {
	paths := []string{}
	for _, rel := range rels {
		paths = append(paths, a.stripePath(rel))
	}
	sort.Strings(paths)
	paths = unique(paths)
	releases := []func(){}
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, name := range paths {
		unlock, e := filepersist.Lock(ctx, name)
		if e != nil {
			release()
			return nil, e
		}
		releases = append(releases, unlock)
	}
	return release, nil
}
func (a *ImageAssets) pendingName() string { return a.index + ".remote-deletes.json" }
func (a *ImageAssets) deletePrepare(ctx context.Context, rels []string) (Map, error) {
	planned := Map{}
	unlock, e := a.itemLocks(ctx, rels)
	if e != nil {
		return nil, e
	}
	defer unlock()
	e = a.catalog(ctx, func(catalog, items Map) error {
		pending, e := loadFileMap(a.pendingName())
		if e != nil {
			return e
		}
		p := object(pending["items"])
		changed := false
		for _, rel := range rels {
			item, exists := items[rel]
			m := object(item)
			generation := text(m["generation"])
			if exists && generation == "" {
				generation = id(16)
				m["generation"] = generation
				items[rel] = m
				changed = true
			}
			old := object(p[rel])
			if !exists {
				generation = text(old["generation"])
			}
			stone := Map{"op_id": id(16), "generation": generation, "scope": "asset", "remote": boolean(m["webdav"], false) || boolean(old["remote"], false), "requested_at": a.server.now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02 15:04:05")}
			p[rel], planned[rel] = stone, stone
		}
		if changed {
			if e = a.storeCatalog(catalog, items); e != nil {
				return e
			}
		}
		pending["items"] = p
		return filepersist.WriteJSON(a.pendingName(), pending)
	})
	return planned, e
}
func tombstoneMatch(current, expected Map) bool {
	return text(current["op_id"]) != "" && text(current["op_id"]) == text(expected["op_id"])
}
func generationMatch(item any, expected Map) bool {
	return item == nil || text(object(item)["generation"]) == text(expected["generation"])
}

type imageDeleteResult struct{ completed, removed, retryRemote bool }

func (a *ImageAssets) DeleteMany(ctx context.Context, rels []string) (int, error) {
	safe := []string{}
	for _, raw := range unique(rels) {
		if rel, e := imageRelative(raw); e == nil {
			safe = append(safe, rel)
		}
	}
	removed := 0
	settings, e := a.settings(ctx)
	if e != nil {
		return 0, e
	}
	var client *http.Client
	var clientDone func()
	defer func() {
		if clientDone != nil {
			clientDone()
		}
	}()
	for start := 0; start < len(safe); start += 16 {
		batch := safe[start:min(start+16, len(safe))]
		planned, e := a.deletePrepare(ctx, batch)
		if e != nil {
			return removed, e
		}
		results := map[string]imageDeleteResult{}
		var terminal error
		for _, rel := range batch {
			stone := object(planned[rel])
			unlock, e := a.itemLock(ctx, rel)
			if e != nil {
				terminal = e
				break
			}
			var current any
			var durable Map
			e = a.catalog(ctx, func(_ Map, items Map) error {
				current = items[rel]
				p, e := loadFileMap(a.pendingName())
				durable = object(object(p["items"])[rel])
				return e
			})
			if e != nil {
				unlock()
				terminal = e
				break
			}
			if !tombstoneMatch(durable, stone) || !generationMatch(current, stone) {
				unlock()
				continue
			}
			result := imageDeleteResult{completed: true}
			if _, e = a.localInfo(rel); e == nil {
				e = a.root.Remove(rel)
				if e != nil && !errors.Is(e, os.ErrNotExist) {
					result.completed = false
					terminal = e
				} else {
					result.removed = true
				}
			}
			if terminal == nil && boolean(stone["remote"], false) {
				if client == nil {
					client, clientDone, e = a.server.configuredHTTP(ctx)
				}
				var remoteRemoved bool
				if e == nil {
					remoteRemoved, e = a.remoteDelete(ctx, client, settings, rel)
				}
				result.removed = result.removed || remoteRemoved
				if e != nil {
					result.retryRemote = true
					result.completed = result.removed
					if !result.removed {
						terminal = e
					}
				}
			}
			results[rel] = result
			unlock()
			if terminal != nil {
				break
			}
		}
		unlock, e := a.itemLocks(ctx, batch)
		if e != nil {
			return removed, e
		}
		e = a.catalog(ctx, func(catalog, items Map) error {
			p, e := loadFileMap(a.pendingName())
			if e != nil {
				return e
			}
			pending := object(p["items"])
			catalogChanged, pendingChanged := false, false
			for _, rel := range batch {
				stone := object(planned[rel])
				if !tombstoneMatch(object(pending[rel]), stone) {
					continue
				}
				matches := generationMatch(items[rel], stone)
				result, exists := results[rel]
				if exists && result.completed && matches {
					if _, found := items[rel]; found {
						delete(items, rel)
						catalogChanged = true
					}
				}
				if !exists || !matches || !result.retryRemote {
					delete(pending, rel)
					pendingChanged = true
				}
			}
			if catalogChanged {
				if e = a.storeCatalog(catalog, items); e != nil {
					return e
				}
			}
			if pendingChanged {
				p["items"] = pending
				return filepersist.WriteJSON(a.pendingName(), p)
			}
			return nil
		})
		unlock()
		for rel, result := range results {
			if result.removed {
				removed++
			}
			if result.completed {
				a.removeDerived(ctx, rel, true)
			}
		}
		if e != nil {
			return removed, e
		}
		if terminal != nil {
			return removed, terminal
		}
	}
	return removed, nil
}

type localRemoval struct {
	rel    string
	size   int64
	remote bool
}

func (a *ImageAssets) removeLocal(ctx context.Context, rels []string, required int64, dry bool) ([]localRemoval, error) {
	out := []localRemoval{}
	freed := int64(0)
	if required == 0 {
		return out, nil
	}
	for start := 0; start < len(rels); start += 16 {
		if required > 0 && freed >= required {
			break
		}
		batch := rels[start:min(start+16, len(rels))]
		unlock, e := a.itemLocks(ctx, batch)
		if e != nil {
			return out, e
		}
		e = a.catalog(ctx, func(catalog, items Map) error {
			changed := false
			for _, rel := range batch {
				if required > 0 && freed >= required {
					break
				}
				item := object(items[rel])
				if boolean(item["remote_sync_pending"], false) {
					continue
				}
				st, e := a.localInfo(rel)
				if e != nil {
					continue
				}
				if size, ok := number(item["size"]); !ok || int64(size) != st.Size() {
					continue
				}
				if !dry {
					if e = a.root.Remove(rel); e != nil {
						continue
					}
				}
				remote := boolean(item["webdav"], false)
				out = append(out, localRemoval{rel, st.Size(), remote})
				freed += st.Size()
				if !dry {
					if remote {
						item["local"], item["storage"] = false, "webdav"
						items[rel] = item
					} else {
						delete(items, rel)
					}
					changed = true
				}
			}
			if changed {
				return a.storeCatalog(catalog, items)
			}
			return nil
		})
		unlock()
		if e != nil {
			return out, e
		}
	}
	if !dry {
		for _, item := range out {
			a.removeDerived(ctx, item.rel, !item.remote)
		}
	}
	return out, nil
}
func (a *ImageAssets) Retention(ctx context.Context, hours int, dry bool) (Map, error) {
	hours = max(1, hours)
	raw, e := a.rawItems(ctx, "", "", "", true, true)
	if e != nil {
		return nil, e
	}
	rels := []string{}
	sizes := map[string]int64{}
	for _, item := range raw {
		local, _ := imageStorageFlags(item)
		created, ok := parseGalleryTime(text(item["created_at"]))
		if ok && local && !created.Add(time.Duration(hours)*time.Hour).After(a.server.now()) {
			rel := text(item["path"])
			rels = append(rels, rel)
			sizes[rel] = int64(integer(item["size"], 0))
		}
	}
	removed := len(rels)
	size := int64(0)
	if dry {
		for _, rel := range rels {
			size += sizes[rel]
		}
	} else {
		entries, e := a.removeLocal(ctx, rels, -1, false)
		if e != nil {
			return nil, e
		}
		removed = len(entries)
		for _, entry := range entries {
			size += entry.size
		}
		_, _ = a.PruneThumbnails(ctx)
	}
	return Map{"removed": removed, "removed_size_bytes": size, "retention_hours": hours, "dry_run": dry}, nil
}
func (a *ImageAssets) PruneThumbnails(ctx context.Context) (int, error) {
	dir := path.Join(path.Dir(a.index), "image_thumbnails")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return 0, e
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return 0, e
	}
	defer root.Close()
	registered := Map{}
	if e = a.catalog(ctx, func(_ Map, items Map) error {
		for rel, item := range items {
			registered[rel] = item
		}
		return nil
	}); e != nil {
		return 0, e
	}
	removed := 0
	e = fsWalk(root, func(rel string) error {
		source := strings.TrimSuffix(rel, ".png")
		if !strings.HasSuffix(rel, ".png") || registered[source] == nil {
			if e := root.Remove(rel); e == nil {
				removed++
			}
		}
		return ctx.Err()
	})
	return removed, e
}
func (a *ImageAssets) CleanupToTarget(ctx context.Context, target int, dry bool) (Map, error) {
	var stat unix.Statfs_t
	if e := unix.Statfs(a.root.Name(), &stat); e != nil {
		return nil, e
	}
	currentBytes := int64(stat.Bavail) * int64(stat.Bsize)
	targetBytes := int64(target) << 20
	rels, e := a.localPaths()
	if e != nil {
		return nil, e
	}
	pngs := []string{}
	for _, rel := range rels {
		if strings.HasSuffix(rel, ".png") {
			pngs = append(pngs, rel)
		}
	}
	sort.Slice(pngs, func(i, j int) bool {
		a1, e1 := a.localInfo(pngs[i])
		b, e2 := a.localInfo(pngs[j])
		return e1 == nil && e2 == nil && a1.ModTime().Before(b.ModTime())
	})
	entries, e := a.removeLocal(ctx, pngs, max(0, targetBytes-currentBytes), dry)
	if e != nil {
		return nil, e
	}
	freed := int64(0)
	for _, entry := range entries {
		freed += entry.size
	}
	free := (currentBytes + freed) >> 20
	return Map{"removed": len(entries), "freed_mb": freed >> 20, "target_free_mb": target, "current_free_mb": free, "done": currentBytes+freed >= targetBytes, "dry_run": dry}, nil
}
func (a *ImageAssets) Compress(ctx context.Context) (Map, error) {
	rels, e := a.localPaths()
	if e != nil {
		return nil, e
	}
	compressed, saved := 0, int64(0)
	updates := Map{}
	commit := func() error {
		if len(updates) == 0 {
			return nil
		}
		rels := make([]string, 0, len(updates))
		for rel := range updates {
			rels = append(rels, rel)
		}
		unlock, e := a.itemLocks(ctx, rels)
		if e != nil {
			return e
		}
		defer unlock()
		committed := []string{}
		e = a.catalog(ctx, func(catalog, items Map) error {
			for rel, raw := range updates {
				update := object(raw)
				st, e := a.localInfo(rel)
				if e != nil || st.Size() != int64(integer(update["size"], -1)) || st.ModTime().UnixNano() != update["mtime"].(int64) {
					continue
				}
				current := object(items[rel])
				item := a.localEntry(rel, current, nil)
				item["width"], item["height"] = update["width"], update["height"]
				if boolean(current["webdav"], false) || boolean(current["remote_sync_pending"], false) {
					item["remote_sync_pending"] = true
				} else {
					delete(item, "remote_sync_pending")
				}
				items[rel] = item
				committed = append(committed, rel)
			}
			if len(committed) > 0 {
				return a.storeCatalog(catalog, items)
			}
			return nil
		})
		if e != nil {
			return e
		}
		for _, rel := range committed {
			compressed++
			saved += int64(integer(object(updates[rel])["saved"], 0))
			a.removeDerived(ctx, rel, false)
		}
		updates = Map{}
		return nil
	}
	for _, rel := range rels {
		if !strings.HasSuffix(rel, ".png") {
			continue
		}
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		unlock, e := a.itemLock(ctx, rel)
		if e != nil {
			return nil, e
		}
		original, e := a.localData(rel)
		if e != nil {
			unlock()
			continue
		}
		if _, _, e = imageDimensions(original); e != nil {
			unlock()
			continue
		}
		source, format, e := image.Decode(bytes.NewReader(original))
		if e != nil {
			unlock()
			continue
		}
		orientation := imageEXIFOrientation(original, format)
		if orientation >= 2 && orientation <= 8 {
			source = orientedImage{Image: source, orientation: orientation}
		}
		var result bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		e = enc.Encode(&result, source)
		if e != nil || result.Len() >= len(original) {
			unlock()
			continue
		}
		temp := rel + "." + id(6) + ".tmp"
		e = a.root.WriteFile(temp, result.Bytes(), 0600)
		if e == nil {
			e = a.root.Rename(temp, rel)
		}
		a.root.Remove(temp)
		if e == nil {
			st, se := a.localInfo(rel)
			e = se
			if e == nil {
				updates[rel] = Map{"size": result.Len(), "saved": len(original) - result.Len(), "mtime": st.ModTime().UnixNano(), "width": source.Bounds().Dx(), "height": source.Bounds().Dy()}
			}
		}
		unlock()
		if e != nil {
			return nil, e
		}
		if len(updates) >= 16 {
			if e = commit(); e != nil {
				return nil, e
			}
		}
	}
	if e = commit(); e != nil {
		return nil, e
	}
	return Map{"compressed": compressed, "saved_bytes": saved, "saved_mb": saved >> 20, "message": fmt.Sprintf("压缩完成：处理 %d 张，节省 %s。", compressed, formatGallerySize(saved))}, nil
}
