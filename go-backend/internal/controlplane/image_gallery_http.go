package controlplane

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

func galleryNotFound(e error) error {
	if os.IsNotExist(e) {
		return &detailedProblem{404, Map{"error": "Image not found"}}
	}
	return e
}
func galleryCORS() map[string]string {
	return map[string]string{"Access-Control-Allow-Origin": "*", "Access-Control-Allow-Methods": "GET, OPTIONS", "Access-Control-Allow-Headers": "*"}
}
func (s *Server) registerGallery() {
	s.route("GET", "/api/images", "admin", "", func(ctx context.Context, r *http.Request, _ Map, _ Map) (any, error) {
		page, e := queryInt(r, "page", 1, 1, 0)
		if e != nil {
			return nil, e
		}
		size, e := queryInt(r, "page_size", 24, 1, 500)
		if e != nil {
			return nil, e
		}
		media := r.URL.Query().Get("media_type")
		if media == "" {
			media = "all"
		}
		if media != "all" && media != "image" {
			return nil, &ValidationError{[]Map{validationIssue("literal_error", "Input should be 'all' or 'image'", []any{"query", "media_type"}, media, Map{"expected": "'all' or 'image'"})}}
		}
		return s.Assets.Gallery(ctx, s.resolveImageBaseURL(r), strings.TrimSpace(r.URL.Query().Get("start_date")), strings.TrimSpace(r.URL.Query().Get("end_date")), page, size, media, strings.TrimSpace(r.URL.Query().Get("tag")), strings.TrimSpace(r.URL.Query().Get("search")))
	})
	s.route("GET", "/image-thumbnails/{image_path:path}", "public", "", func(ctx context.Context, r *http.Request, _ Map, _ Map) (any, error) {
		data, e := s.Assets.Thumbnail(ctx, r.PathValue("image_path"))
		if e != nil {
			return nil, galleryNotFound(e)
		}
		return &Download{Bytes: data, ContentType: "image/png", Headers: galleryCORS()}, nil
	})
	s.route("GET", "/api/images/download/{image_path:path}", "admin", "", func(ctx context.Context, r *http.Request, _ Map, _ Map) (any, error) {
		rel, e := imageRelative(r.PathValue("image_path"))
		if e != nil {
			return nil, galleryNotFound(e)
		}
		data, mime, e := s.Assets.Read(ctx, rel)
		if e != nil {
			return nil, galleryNotFound(e)
		}
		headers := galleryCORS()
		headers["Content-Disposition"] = `attachment; filename="` + path.Base(rel) + `"`
		return &Download{Bytes: data, ContentType: mime, Headers: headers}, nil
	})
	s.route("POST", "/api/images/download", "admin", "ImageDownloadRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
		return s.Assets.zip(ctx, stringsOf(body["paths"]))
	})
	s.route("POST", "/api/images/delete", "admin", "ImageDeleteRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
		rels := stringsOf(body["paths"])
		if boolean(body["all_matching"], false) {
			raw, e := s.Assets.rawItems(ctx, "", text(body["start_date"]), text(body["end_date"]), true, true)
			if e != nil {
				return nil, e
			}
			rels = []string{}
			for _, item := range raw {
				rels = append(rels, text(item["path"]))
			}
		}
		removed, e := s.Assets.DeleteMany(ctx, rels)
		return Map{"removed": removed}, e
	})
	s.route("POST", "/api/images/tags", "admin", "ImageTagsRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
		tags, e := s.Assets.SetTags(ctx, text(body["path"]), stringsOf(body["tags"]))
		return Map{"ok": true, "tags": tags}, e
	})
	s.route("POST", "/api/images/retention-cleanup", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		values, e := s.Settings.Values(ctx)
		if e != nil {
			return nil, e
		}
		result, e := s.Retention.RunImages(ctx, integer(values["image_retention_hours"], 24), false)
		if e != nil {
			return nil, e
		}
		delete(result, "dry_run")
		result["message"] = fmt.Sprintf("已清理 %d 个过期本地副本；仍有 WebDAV 副本的图库记录已保留。", integer(result["removed"], 0))
		return result, nil
	})
	s.route("GET", "/api/images/storage", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		return s.Assets.StorageStats(ctx)
	})
	s.route("POST", "/api/images/storage/compress", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) { return s.Assets.Compress(ctx) })
	s.route("POST", "/api/images/storage/cleanup-to-target", "admin", "", func(ctx context.Context, r *http.Request, _ Map, _ Map) (any, error) {
		target, e := queryInt(r, "target_free_mb", 500, 1, 0)
		if e != nil {
			return nil, e
		}
		dry := false
		if raw := r.URL.Query().Get("dry_run"); raw != "" {
			switch strings.ToLower(raw) {
			case "true", "1", "on", "yes":
				dry = true
			case "false", "0", "off", "no":
			default:
				return nil, &ValidationError{[]Map{validationIssue("bool_parsing", "Input should be a valid boolean, unable to interpret input", []any{"query", "dry_run"}, raw, nil)}}
			}
		}
		result, e := s.Retention.CleanupToTarget(ctx, target, dry)
		if e != nil {
			return nil, e
		}
		result["message"] = galleryTargetMessage(result)
		return result, nil
	})
	s.route("POST", "/api/images/genbox-push", "admin", "GalleryGenBoxPushRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
		return s.Assets.PushGenBox(ctx, text(body["path"]), nil)
	})
	s.route("POST", "/api/image-storage/test", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		result, e := s.Assets.TestWebDAV(ctx)
		return Map{"result": result}, e
	})
	s.route("POST", "/api/image-storage/sync", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		result, e := s.Assets.Sync(ctx)
		return Map{"result": result}, e
	})
}
func (s *Server) registerBackups() {
	s.route("POST", "/api/backup/test", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		result, e := s.Backups.Test(ctx)
		return Map{"result": result}, e
	})
	s.route("GET", "/api/backups", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		items, e := s.Backups.List(ctx)
		if e != nil {
			return nil, e
		}
		state, e := s.Backups.State(ctx)
		return Map{"items": items, "state": state}, e
	})
	s.route("POST", "/api/backups/run", "admin", "", func(ctx context.Context, _ *http.Request, _ Map, _ Map) (any, error) {
		result, e := s.Backups.Run(ctx, "manual")
		return Map{"result": result}, e
	})
	s.route("POST", "/api/backups/delete", "admin", "BackupDeleteRequest", func(ctx context.Context, _ *http.Request, body Map, _ Map) (any, error) {
		e := s.Backups.Delete(ctx, text(body["key"]))
		return Map{"ok": true}, e
	})
}
func galleryTargetMessage(result Map) string {
	removed := integer(result["removed"], 0)
	free := formatGallerySize(int64(integer(result["current_free_mb"], 0)) << 20)
	target := formatGallerySize(int64(integer(result["target_free_mb"], 1)) << 20)
	freed := formatGallerySize(int64(integer(result["freed_mb"], 0)) << 20)
	status := fmt.Sprintf("当前剩余 %s / 目标 %s。", free, target)
	done := boolean(result["done"], false)
	dry := boolean(result["dry_run"], false)
	if !done {
		status = fmt.Sprintf("当前剩余 %s / 目标 %s，仍未达到目标。", free, target)
	}
	if dry && removed > 0 {
		return fmt.Sprintf("预估会清理 %d 张，预计释放 %s。%s", removed, freed, status)
	}
	if dry && done {
		return fmt.Sprintf("无需清理：当前剩余 %s，已达到目标 %s。", free, target)
	}
	if removed > 0 {
		return fmt.Sprintf("已清理 %d 张，释放 %s。%s", removed, freed, status)
	}
	if done {
		return fmt.Sprintf("没有需要清理的图片。当前剩余 %s / 目标 %s。", free, target)
	}
	return "没有可清理的图片。" + status
}

type temporaryZip struct {
	*os.File
	name string
}

func (t *temporaryZip) Close() error { e := t.File.Close(); os.Remove(t.name); return e }
func (a *ImageAssets) zip(ctx context.Context, rels []string) (*Download, error) {
	out, e := os.CreateTemp(a.server.DataDir, ".gallery-*.zip")
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	archive := zip.NewWriter(out)
	used := map[string]bool{}
	count := 0
	for _, raw := range rels {
		if e = ctx.Err(); e != nil {
			archive.Close()
			return nil, e
		}
		rel, e := imageRelative(raw)
		if e != nil {
			archive.Close()
			return nil, problem(404, "Image not found")
		}
		data, _, e := a.Read(ctx, rel)
		if e != nil {
			continue
		}
		name := path.Base(rel)
		if used[name] {
			extension := path.Ext(name)
			stem := strings.TrimSuffix(name, extension)
			for suffix := 2; ; suffix++ {
				name = stem + "_" + strconv.Itoa(suffix) + extension
				if !used[name] {
					break
				}
			}
		}
		used[name] = true
		entry, e := archive.Create(name)
		if e != nil {
			archive.Close()
			return nil, e
		}
		if _, e = entry.Write(data); e != nil {
			archive.Close()
			return nil, e
		}
		count++
	}
	if e = archive.Close(); e != nil {
		return nil, e
	}
	if count == 0 {
		return nil, problem(404, "no images found")
	}
	if _, e = out.Seek(0, io.SeekStart); e != nil {
		return nil, e
	}
	ok = true
	return &Download{Reader: &temporaryZip{out, out.Name()}, ContentType: "application/zip", Headers: map[string]string{"Content-Disposition": `attachment; filename="images.zip"`}}, nil
}
