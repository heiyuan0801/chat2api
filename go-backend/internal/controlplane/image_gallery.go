package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/sys/unix"
)

func (a *ImageAssets) settings(ctx context.Context) (Map, error) {
	raw, e := a.server.DB.Document(ctx, "system_settings")
	return normalizeRoot("image_storage", object(raw["image_storage"])), e
}
func (a *ImageAssets) itemLock(ctx context.Context, rel string) (func(), error) {
	stripe := sha256.Sum256([]byte(rel))
	return filepersist.Lock(ctx, a.index+".item-locks/"+hex.EncodeToString(stripe[:1])+".lock")
}
func (a *ImageAssets) catalog(ctx context.Context, fn func(Map, Map) error) error {
	unlock, e := filepersist.Lock(ctx, a.index+".lock")
	if e != nil {
		return e
	}
	defer unlock()
	catalog, e := loadFileMap(a.index)
	if e != nil {
		return e
	}
	items := object(catalog["items"])
	if e = fn(catalog, items); e != nil {
		return e
	}
	return nil
}
func (a *ImageAssets) storeCatalog(catalog, items Map) error {
	catalog["items"] = items
	return filepersist.WriteJSON(a.index, catalog)
}
func (a *ImageAssets) localInfo(rel string) (fs.FileInfo, error) {
	st, e := a.root.Stat(rel)
	if e != nil || !st.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	return st, nil
}
func (a *ImageAssets) localData(rel string) ([]byte, error) {
	f, e := a.root.Open(rel)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > maxImageBytes {
		return nil, os.ErrNotExist
	}
	b, e := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	if len(b) > maxImageBytes {
		return nil, os.ErrNotExist
	}
	return b, e
}
func (a *ImageAssets) localPaths() ([]string, error) {
	out := []string{}
	e := fs.WalkDir(a.root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			if rel, e := imageRelative(name); e == nil {
				if _, e = a.localInfo(rel); e == nil {
					out = append(out, rel)
				}
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, e
}
func imageStorageFlags(item Map) (bool, bool) {
	storage := text(item["storage"])
	return boolean(item["local"], storage == "local" || storage == "both"), boolean(item["webdav"], storage == "webdav" || storage == "both")
}
func imageStorageName(local, remote bool) string {
	if local && remote {
		return "both"
	}
	if remote {
		return "webdav"
	}
	return "local"
}
func (a *ImageAssets) localEntry(rel string, prior Map, data []byte) Map {
	st, e := a.localInfo(rel)
	if e != nil {
		return prior
	}
	now := st.ModTime().In(time.FixedZone("Asia/Shanghai", 8*3600))
	day := now.Format("2006-01-02")
	if len(strings.Split(rel, "/")) >= 4 {
		day = strings.Join(strings.Split(rel, "/")[:3], "-")
	}
	item := copyMap(prior)
	item["rel"], item["path"], item["name"], item["date"], item["size"] = rel, rel, path.Base(rel), day, st.Size()
	if text(item["created_at"]) == "" {
		item["created_at"] = now.Format("2006-01-02 15:04:05")
	}
	if text(item["generation"]) == "" {
		item["generation"] = id(16)
	}
	remote := boolean(prior["webdav"], false)
	item["local"], item["webdav"], item["storage"] = true, remote, imageStorageName(true, remote)
	if data != nil {
		if w, h, e := imageDimensions(data); e == nil {
			item["width"], item["height"] = w, h
		}
	}
	return item
}
func (a *ImageAssets) rawItems(ctx context.Context, base, start, end string, refresh, verify bool) ([]Map, error) {
	out := []Map{}
	settings, e := a.settings(ctx)
	if e != nil {
		return nil, e
	}
	public := strings.TrimRight(text(settings["public_base_url"]), "/")
	if public == "" {
		public = strings.TrimRight(base, "/") + "/images"
	}
	e = a.catalog(ctx, func(catalog, items Map) error {
		changed := false
		if refresh {
			rels, e := a.localPaths()
			if e != nil {
				return e
			}
			for _, rel := range rels {
				if _, ok := items[rel]; !ok {
					b, _ := a.localData(rel)
					items[rel] = a.localEntry(rel, Map{}, b)
					changed = true
				}
			}
		}
		for rel, raw := range items {
			if _, e := imageRelative(rel); e != nil {
				delete(items, rel)
				changed = true
				continue
			}
			item := object(raw)
			local, remote := imageStorageFlags(item)
			if text(item["generation"]) == "" {
				item["generation"] = id(16)
				changed = true
			}
			if verify {
				st, e := a.localInfo(rel)
				local = e == nil
				if !local && !remote {
					delete(items, rel)
					changed = true
					continue
				}
				oldsize := integer(item["size"], -1)
				if local && oldsize != int(st.Size()) {
					if oldsize >= 0 && remote {
						item["remote_sync_pending"] = true
					}
					item["size"] = st.Size()
					changed = true
				}
				if item["local"] != local || text(item["storage"]) != imageStorageName(local, remote) {
					changed = true
				}
				item["local"], item["webdav"], item["storage"] = local, remote, imageStorageName(local, remote)
			}
			items[rel] = item
			day := text(item["date"])
			if start != "" && day < start || end != "" && day > end {
				continue
			}
			entry := maps.Clone(item)
			entry["path"], entry["rel"], entry["url"] = rel, rel, public+"/"+rel
			out = append(out, entry)
		}
		if changed {
			return a.storeCatalog(catalog, items)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		left, right := text(out[i]["created_at"]), text(out[j]["created_at"])
		if left == right {
			return text(out[i]["path"]) > text(out[j]["path"])
		}
		return left > right
	})
	return out, e
}
func (a *ImageAssets) tags(ctx context.Context) (Map, error) {
	name := filepath.Join(filepath.Dir(a.index), "image_tags.json")
	unlock, e := filepersist.Lock(ctx, name+".lock")
	if e != nil {
		return nil, e
	}
	defer unlock()
	return loadFileMap(name)
}
func (a *ImageAssets) SetTags(ctx context.Context, rel string, values []string) ([]string, error) {
	rel = strings.TrimLeft(strings.TrimSpace(rel), "/")
	if rel == "" {
		return nil, problem(400, "path is required")
	}
	clean := unique(values)
	name := filepath.Join(filepath.Dir(a.index), "image_tags.json")
	unlock, e := filepersist.Lock(ctx, name+".lock")
	if e != nil {
		return nil, e
	}
	defer unlock()
	tags, e := loadFileMap(name)
	if e != nil {
		return nil, e
	}
	if len(clean) > 0 {
		tags[rel] = clean
	} else {
		delete(tags, rel)
	}
	return clean, filepersist.WriteJSON(name, tags)
}
func (a *ImageAssets) removeDerived(ctx context.Context, rel string, removeTags bool) {
	root, e := os.OpenRoot(filepath.Join(filepath.Dir(a.index), "image_thumbnails"))
	if e == nil {
		root.Remove(rel + ".png")
		root.Remove(rel)
		root.Close()
	}
	if removeTags {
		_, _ = a.SetTags(ctx, rel, nil)
	}
}
func galleryGenboxState(value any) any {
	m := object(value)
	status := text(m["status"])
	labels := map[string]string{"imported": "已推送到 GenBox", "already-imported": "GenBox 已存在", "duplicate-local": "GenBox 本地重复"}
	label := labels[status]
	sha := strings.ToLower(text(m["sha256"]))
	raw, e := hex.DecodeString(sha)
	if label == "" || e != nil || len(raw) != 32 || text(m["updated_at"]) == "" {
		return nil
	}
	return Map{"status": status, "label": label, "sha256": sha, "updated_at": text(m["updated_at"])}
}
func galleryTags(value any) []string {
	out := []string{}
	for _, v := range list(value) {
		if truth(v) {
			out = append(out, text(v))
		}
	}
	return unique(out)
}
func galleryFilename(item Map) string {
	name := text(item["name"])
	if name == "" {
		name = text(item["filename"])
	}
	if name == "" {
		name = path.Base(text(item["path"]))
	}
	return name
}
func galleryStorage(item Map) (string, bool, bool) {
	local, remote := boolean(item["local"], true), boolean(item["webdav"], false)
	storage := text(item["storage"])
	if storage != "local" && storage != "webdav" && storage != "both" {
		storage = imageStorageName(local, remote)
	}
	return storage, local, remote
}
func (a *ImageAssets) Gallery(ctx context.Context, base, start, end string, page, pageSize int, media, tag, search string) (Map, error) {
	raw, e := a.rawItems(ctx, base, start, end, false, false)
	if e != nil {
		return nil, e
	}
	tags, e := a.tags(ctx)
	if e != nil {
		return nil, e
	}
	values, e := a.server.Settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	hours := max(1, integer(values["image_retention_hours"], 24))
	selected := make([]Map, 0, len(raw))
	totalSize := int64(0)
	needle := strings.ToLower(search)
	now := a.server.now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	for _, item := range raw {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		rel := text(item["path"])
		var itemTags []string
		if tag != "" && tag != "all" || needle != "" {
			itemTags = galleryTags(tags[rel])
		}
		if tag != "" && tag != "all" && !containsString(itemTags, tag) {
			continue
		}
		if needle != "" {
			storage, _, _ := galleryStorage(item)
			hay := strings.ToLower(strings.Join(append([]string{galleryFilename(item), rel, text(item["created_at"]), storage}, itemTags...), "\n"))
			if !strings.Contains(hay, needle) {
				continue
			}
		}
		totalSize += int64(max(0, integer(item["size"], 0)))
		selected = append(selected, item)
	}
	pageSize = max(1, pageSize)
	total := len(selected)
	pageCount := max(1, (total+pageSize-1)/pageSize)
	page = min(max(1, page), pageCount)
	offset := (page - 1) * pageSize
	rows := make([]Map, 0, min(pageSize, total))
	for _, item := range selected[offset:min(offset+pageSize, total)] {
		rel := text(item["path"])
		storage, local, remote := galleryStorage(item)
		var expires, remaining any
		expired := false
		if local {
			if created, ok := parseGalleryTime(text(item["created_at"])); ok {
				end := created.Add(time.Duration(hours) * time.Hour)
				secs := int(end.Sub(now).Seconds())
				expired = secs <= 0
				expires = end.Format("2006-01-02 15:04:05")
				remaining = max(0, secs)
			}
		}
		var width, height any
		if w := integer(item["width"], 0); w > 0 {
			width = w
		}
		if h := integer(item["height"], 0); h > 0 {
			height = h
		}
		rows = append(rows, Map{"id": rel, "path": rel, "filename": galleryFilename(item), "url": item["url"], "thumbnail_url": strings.TrimRight(base, "/") + "/image-thumbnails/" + rel, "size_bytes": max(0, integer(item["size"], 0)), "created_at": text(item["created_at"]), "date": text(item["date"]), "media_type": "image", "expired": expired, "expires_at": expires, "expires_in_seconds": remaining, "tags": galleryTags(tags[rel]), "storage": storage, "local": local, "webdav": remote, "available": local || remote, "width": width, "height": height, "genbox_push": galleryGenboxState(item["genbox_push"])})
	}
	allTags := []string{}
	for _, v := range tags {
		allTags = append(allTags, galleryTags(v)...)
	}
	allTags = unique(allTags)
	sort.Strings(allTags)
	return Map{"schema_version": 1, "generated_at": now.Format("2006-01-02T15:04:05-07:00"), "items": rows, "total": total, "total_size_bytes": totalSize, "retention_hours": hours, "capabilities": Map{"genbox_push": boolean(object(values["genbox_push"])["enabled"], false)}, "facets": Map{"media_types": Map{"all": total, "image": total}, "tags": allTags}, "media_type": media, "page": page, "page_size": pageSize, "page_count": pageCount, "has_more": page < pageCount}, nil
}

func containsString(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}
func parseGalleryTime(s string) (time.Time, bool) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, e := time.ParseInLocation(layout, s, loc); e == nil {
			return t.In(loc), true
		}
	}
	return time.Time{}, false
}
func (a *ImageAssets) Thumbnail(ctx context.Context, raw string) ([]byte, error) {
	rel, e := imageRelative(raw)
	if e != nil {
		return nil, os.ErrNotExist
	}
	unlock, e := a.itemLock(ctx, rel)
	if e != nil {
		return nil, e
	}
	defer unlock()
	dir := filepath.Join(filepath.Dir(a.index), "image_thumbnails")
	if e = os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	target := rel + ".png"
	// Registration is checked before cached data is served, including WebDAV-only assets.
	registered := false
	e = a.catalog(ctx, func(_ Map, items Map) error { _, registered = items[rel]; return nil })
	if e != nil {
		return nil, e
	}
	if !registered {
		return nil, os.ErrNotExist
	}
	local, le := a.localInfo(rel)
	if cached, e := root.Stat(target); e == nil && (le != nil || !cached.ModTime().Before(local.ModTime())) {
		if cached.Size() <= maxImageBytes {
			if data, e := root.ReadFile(target); e == nil {
				return data, nil
			}
		}
	}
	data, _, e := a.Read(ctx, rel)
	if e != nil {
		return nil, e
	}
	if _, _, e = imageDimensions(data); e != nil {
		return nil, problem(422, "failed to create thumbnail")
	}
	source, format, e := image.Decode(bytes.NewReader(data))
	if e != nil {
		return nil, problem(422, "failed to create thumbnail")
	}
	orientation := imageEXIFOrientation(data, format)
	if orientation >= 2 && orientation <= 8 {
		source = orientedImage{Image: source, orientation: orientation}
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	tw, th := width, height
	if width > 320 || height > 320 {
		aspect := float64(width) / float64(height)
		tw, th = 320, 320
		if aspect <= 1 {
			n := 320 * aspect
			low, high := int(math.Floor(n)), int(math.Ceil(n))
			tw = low
			if math.Abs(aspect-float64(high)/320) < math.Abs(aspect-float64(low)/320) {
				tw = high
			}
			tw = max(1, tw)
		} else {
			n := 320 / aspect
			low, high := int(math.Floor(n)), int(math.Ceil(n))
			score := func(v int) float64 {
				if v == 0 {
					return 0
				}
				return math.Abs(aspect - 320/float64(v))
			}
			th = low
			if score(high) < score(low) {
				th = high
			}
			th = max(1, th)
		}
	}
	out := image.NewNRGBA(image.Rect(0, 0, tw, th))
	kernel := xdraw.Kernel{Support: 3, At: lanczos}
	kernel.Scale(out, out.Bounds(), source, source.Bounds(), xdraw.Src, nil)
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if e = encoder.Encode(&encoded, out); e != nil {
		return nil, problem(422, "failed to create thumbnail")
	}
	if e = root.MkdirAll(path.Dir(target), 0700); e != nil {
		return nil, e
	}
	temp := target + "." + id(6) + ".tmp"
	if e = root.WriteFile(temp, encoded.Bytes(), 0600); e != nil {
		return nil, e
	}
	defer root.Remove(temp)
	if e = root.Rename(temp, target); e != nil {
		return nil, e
	}
	return encoded.Bytes(), nil
}
func (a *ImageAssets) StorageStats(ctx context.Context) (Map, error) {
	var st unix.Statfs_t
	if e := unix.Statfs(a.root.Name(), &st); e != nil {
		return nil, e
	}
	count, size := int64(0), int64(0)
	e := fs.WalkDir(a.root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			if info, e := a.root.Stat(name); e == nil && info.Mode().IsRegular() {
				count++
				size += info.Size()
			}
		}
		return ctx.Err()
	})
	if e != nil {
		return nil, e
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * uint64(st.Bsize)
	return Map{"disk_total_mb": int64(total >> 20), "disk_used_mb": int64(used >> 20), "disk_free_mb": int64(free >> 20), "image_count": count, "image_size_mb": size >> 20, "image_size_bytes": size}, nil
}
func formatGallerySize(size int64) string {
	if size <= 0 {
		return "0 B"
	}
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	if size < 1<<30 {
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", float64(size)/(1<<30))
}
