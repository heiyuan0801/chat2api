package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type genboxJob struct {
	rel      string
	metadata Map
}
type assetJobs struct {
	ctx    context.Context
	cancel context.CancelFunc
	queue  chan genboxJob
	wg     sync.WaitGroup
}

func (a *ImageAssets) startJobs() {
	ctx, cancel := context.WithCancel(a.server.executionContext)
	workers := max(1, integer(os.Getenv("CHATGPT2API_GENBOX_PUSH_CONCURRENCY"), 8))
	queue := max(1, integer(os.Getenv("CHATGPT2API_GENBOX_PUSH_QUEUE_SIZE"), 64))
	a.jobs = &assetJobs{ctx: ctx, cancel: cancel, queue: make(chan genboxJob, queue)}
	for range workers {
		a.jobs.wg.Add(1)
		go func() {
			defer a.jobs.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-a.jobs.queue:
					if ctx.Err() != nil {
						return
					}
					_, _ = a.PushGenBox(ctx, job.rel, job.metadata)
				}
			}
		}()
	}
}
func genboxError(code string) error {
	spec := map[string]struct {
		status  int
		message string
	}{"genbox_not_configured": {409, "GenBox 尚未配置，请先在设置的外部服务中填写并启用"}, "genbox_invalid_receipt": {502, "GenBox 返回的数据无效"}, "genbox_probe_rejected": {502, "GenBox 连接验证未通过"}, "genbox_unavailable": {502, "GenBox 服务当前不可用"}, "genbox_rejected": {502, "GenBox 未接受该图片"}, "genbox_request_failed": {504, "GenBox 请求失败或超时"}}
	s := spec[code]
	return &detailedProblem{s.status, Map{"code": code, "message": s.message}}
}

// strictReceipt rejects duplicate keys, including nested JSON objects.
func strictReceipt(reader io.Reader) (Map, error) {
	data, e := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if e != nil || len(data) > 1<<20 {
		return nil, genboxError("genbox_invalid_receipt")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var parse func() (any, error)
	parse = func() (any, error) {
		tok, e := dec.Token()
		if e != nil {
			return nil, e
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return tok, nil
		}
		if delim == '{' {
			out := Map{}
			for dec.More() {
				key, e := dec.Token()
				if e != nil {
					return nil, e
				}
				s, ok := key.(string)
				if !ok {
					return nil, io.ErrUnexpectedEOF
				}
				if _, exists := out[s]; exists {
					return nil, io.ErrUnexpectedEOF
				}
				value, e := parse()
				if e != nil {
					return nil, e
				}
				out[s] = value
			}
			_, e = dec.Token()
			return out, e
		}
		if delim == '[' {
			out := []any{}
			for dec.More() {
				v, e := parse()
				if e != nil {
					return nil, e
				}
				out = append(out, v)
			}
			_, e = dec.Token()
			return out, e
		}
		return nil, io.ErrUnexpectedEOF
	}
	value, e := parse()
	if e != nil {
		return nil, genboxError("genbox_invalid_receipt")
	}
	if _, e = dec.Token(); e != io.EOF {
		return nil, genboxError("genbox_invalid_receipt")
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, genboxError("genbox_invalid_receipt")
	}
	return m, nil
}
func (a *ImageAssets) PushGenBox(parent context.Context, raw string, metadata Map) (Map, error) {
	values, e := a.server.DB.Document(parent, "system_settings")
	if e != nil {
		return nil, e
	}
	settings := normalizeRoot("genbox_push", object(values["genbox_push"]))
	if !boolean(settings["enabled"], false) || text(settings["base_url"]) == "" || text(settings["source_id"]) == "" || text(settings["push_key"]) == "" {
		return nil, genboxError("genbox_not_configured")
	}
	base, e := url.Parse(text(settings["base_url"]))
	if e != nil || base.Host == "" || base.User != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Fragment != "" || base.RawQuery != "" {
		return nil, genboxError("genbox_not_configured")
	}
	rel, e := imageRelative(raw)
	if e != nil {
		return nil, problem(404, "Gallery image not registered or not a valid image")
	}
	data, _, e := a.Read(parent, rel)
	if e != nil {
		return nil, problem(404, "Gallery image not registered or not a valid image")
	}
	if _, _, e = imageDimensions(data); e != nil {
		return nil, &detailedProblem{404, Map{"code": "genbox_source_not_found", "message": "Gallery image not registered or not a valid image"}}
	}
	if _, _, e = image.Decode(bytes.NewReader(data)); e != nil {
		return nil, &detailedProblem{404, Map{"code": "genbox_source_not_found", "message": "Gallery image not registered or not a valid image"}}
	}
	hash := sha256.Sum256(data)
	sha := hex.EncodeToString(hash[:])
	ctx, cancel := context.WithTimeout(parent, time.Duration(max(1, integer(settings["timeout_secs"], 60)))*time.Second)
	defer cancel()
	client, done, e := a.server.configuredHTTP(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	send := func(method, route string, body io.Reader, contentType string) (*http.Response, error) {
		req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(base.String(), "/")+route, body)
		if e != nil {
			return nil, genboxError("genbox_request_failed")
		}
		req.Header.Set("X-GenBox-Source", text(settings["source_id"]))
		req.Header.Set("X-GenBox-Key", text(settings["push_key"]))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, e := client.Do(req)
		if e != nil {
			return nil, genboxError("genbox_request_failed")
		}
		return resp, nil
	}
	probe, e := send("GET", "/api/sync/push/status", nil, "")
	if e != nil {
		return nil, e
	}
	defer probe.Body.Close()
	if probe.StatusCode < 200 || probe.StatusCode >= 300 {
		return nil, genboxError("genbox_unavailable")
	}
	value, e := strictReceipt(probe.Body)
	if e != nil {
		return nil, e
	}
	limit, isInt := value["max_image_bytes"].(json.Number)
	maxBytes, ne := limit.Int64()
	if value["ok"] != true || value["contract_version"] != "v1" || value["source_id"] != settings["source_id"] || !isInt || ne != nil || maxBytes < int64(len(data)) {
		return nil, genboxError("genbox_probe_rejected")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, e := form.CreateFormFile("image", path.Base(rel))
	if e != nil {
		return nil, e
	}
	if _, e = part.Write(data); e != nil {
		return nil, e
	}
	form.WriteField("remote_path", rel)
	form.WriteField("source_sha256", sha)
	for _, key := range []string{"prompt", "created_at", "date", "model"} {
		if text(metadata[key]) != "" {
			form.WriteField(key, text(metadata[key]))
		}
	}
	form.Close()
	resp, e := send("POST", "/api/sync/push", &body, form.FormDataContentType())
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, genboxError("genbox_rejected")
	}
	receipt, e := strictReceipt(resp.Body)
	if e != nil {
		return nil, e
	}
	status := text(receipt["status"])
	if receipt["ok"] != true || receipt["contract_version"] != "v1" || receipt["source_id"] != settings["source_id"] || receipt["sha256"] != sha || (status != "imported" && status != "already-imported" && status != "duplicate-local") {
		return nil, genboxError("genbox_invalid_receipt")
	}
	state := Map{"status": status, "sha256": sha, "updated_at": a.server.now().UTC().Format(time.RFC3339Nano)}
	unlock, e := a.itemLock(ctx, rel)
	if e != nil {
		return nil, e
	}
	defer unlock()
	e = a.catalog(ctx, func(catalog, items Map) error {
		item, exists := items[rel]
		if !exists {
			return problem(404, "Gallery image not registered or not a valid image")
		}
		m := object(item)
		m["genbox_push"] = state
		items[rel] = m
		return a.storeCatalog(catalog, items)
	})
	if e != nil {
		return nil, e
	}
	result := object(galleryGenboxState(state))
	result["path"], result["source_retained"] = rel, true
	return result, nil
}
func (s *Server) AutoPushGalleryURLs(urls []string, metadata Map, baseURL string) {
	if s.Assets == nil || s.Assets.jobs == nil {
		return
	}
	a := s.Assets
	ctx := a.jobs.ctx
	raw, e := s.DB.Document(ctx, "system_settings")
	if e != nil {
		return
	}
	settings := normalizeRoot("genbox_push", object(raw["genbox_push"]))
	if !boolean(settings["enabled"], false) || !boolean(settings["auto_push_after_studio"], false) || text(settings["base_url"]) == "" || text(settings["source_id"]) == "" || text(settings["push_key"]) == "" {
		return
	}
	storage := normalizeRoot("image_storage", object(raw["image_storage"]))
	values, e := s.Settings.Values(ctx)
	if e != nil {
		return
	}
	origins := map[string]bool{}
	bases := []string{text(values["base_url"]), text(storage["public_base_url"]), baseURL}
	for _, base := range bases {
		if u, e := url.Parse(base); e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			origins[u.Scheme+"://"+strings.ToLower(u.Host)] = true
		}
	}
	seen := map[string]bool{}
	for _, rawURL := range urls {
		u, e := url.Parse(rawURL)
		if e != nil || !origins[u.Scheme+"://"+strings.ToLower(u.Host)] {
			continue
		}
		rel := ""
		if i := strings.Index(u.Path, "/images/"); i >= 0 {
			rel = u.Path[i+8:]
		} else {
			for _, base := range bases {
				if base != "" && strings.HasPrefix(rawURL, strings.TrimRight(base, "/")+"/") {
					rel = strings.TrimPrefix(rawURL, strings.TrimRight(base, "/")+"/")
					break
				}
			}
		}
		rel, e = imageRelative(rel)
		if e != nil || seen[rel] {
			continue
		}
		seen[rel] = true
		terminal := false
		_ = a.catalog(ctx, func(_ Map, items Map) error {
			terminal = galleryGenboxState(object(items[rel])["genbox_push"]) != nil
			return nil
		})
		if terminal {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case a.jobs.queue <- genboxJob{rel, copyMap(metadata)}:
		default:
		}
	}
}

func (s *Server) autoPushImageTask(result, payload Map, resumed bool) {
	urls := []string{}
	var collect func(any)
	collect = func(v any) {
		switch value := v.(type) {
		case map[string]any:
			if u, ok := value["url"].(string); ok && strings.TrimSpace(u) != "" {
				urls = append(urls, u)
			}
			urls = append(urls, stringsOf(value["_image_urls"])...)
			collect(value["data"])
		case []Map:
			for _, item := range value {
				collect(item)
			}
		case []any:
			for _, item := range value {
				collect(item)
			}
		}
	}
	collect(result)
	metadata := Map{}
	if !resumed && text(payload["prompt"]) != "" {
		metadata["prompt"] = payload["prompt"]
	}
	for _, key := range []string{"created_at", "date"} {
		if text(result[key]) != "" {
			metadata[key] = result[key]
		}
	}
	if metadata["created_at"] == nil && result["created"] != nil {
		metadata["created_at"] = result["created"]
	}
	if text(payload["model"]) != "" {
		metadata["model"] = payload["model"]
	}
	s.AutoPushGalleryURLs(unique(urls), metadata, text(payload["base_url"]))
}
