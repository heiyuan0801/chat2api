package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"io"
	"mime"
	stdhttp "net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func jsonDecodeEditable(value string, out *any) error {
	dec := json.NewDecoder(strings.NewReader(value))
	dec.UseNumber()
	return dec.Decode(out)
}

var editableID = regexp.MustCompile(`\b(file[-_][A-Za-z0-9_-]+)\b`)
var editablePointer = regexp.MustCompile(`(?:file-service|sediment)://([A-Za-z0-9_-]+)`)
var editablePPTPath = regexp.MustCompile(`(?i)(?:sandbox:)?(/mnt/data/[^\s"'\)\]]+\.(?:pptx?|zip))`)
var editablePSDPath = regexp.MustCompile(`(?i)(?:sandbox:)?(/mnt/data/[^\s"'\)\]]+\.(?:psd|zip))`)

func editableFileID(value string) string {
	for _, m := range editableID.FindAllStringSubmatch(value, -1) {
		if !strings.HasPrefix(m[1], "file-service") {
			return m[1]
		}
	}
	return ""
}
func editablePath(value string, kind string) string {
	re := editablePPTPath
	if kind == "psd" {
		re = editablePSDPath
	}
	if m := re.FindStringSubmatch(value); len(m) > 1 {
		return m[1]
	}
	return ""
}
func editableExtension(kind, mimeType string) string {
	mimeType = strings.ToLower(strings.Split(mimeType, ";")[0])
	if strings.Contains(mimeType, "zip") {
		return ".zip"
	}
	if kind == "psd" && (strings.Contains(mimeType, "photoshop")) {
		return ".psd"
	}
	if kind == "ppt" && (strings.Contains(mimeType, "presentationml.presentation") || strings.Contains(mimeType, "ms-powerpoint")) {
		return ".pptx"
	}
	return ""
}

// SafeEditableFilename is the shared filename boundary for export and public
// download. It matches the baseline cross-platform filename rules.
func SafeEditableFilename(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 240 || name == "." || name == ".." || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, r := range name {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" {
		return false
	}
	for i := 1; i < 10; i++ {
		if stem == "COM"+strconv.Itoa(i) || stem == "LPT"+strconv.Itoa(i) {
			return false
		}
	}
	return true
}
func sanitizeEditableFilename(value string) string {
	value = path.Base(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	runes := []rune(value)
	for i, r := range runes {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			runes[i] = '_'
		}
	}
	value = strings.TrimRight(strings.TrimSpace(string(runes)), " .")
	if value == "" {
		return ""
	}
	if !SafeEditableFilename(value) {
		stem := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || regexp.MustCompile(`^(?:COM|LPT)[1-9]$`).MatchString(stem) {
			value = "_" + value
		}
	}
	runes = []rune(value)
	if len(runes) > 240 {
		ext := path.Ext(value)
		if len([]rune(ext)) > 16 {
			ext = ""
		}
		value = strings.TrimRight(string(runes[:240-len([]rune(ext))]), " .") + ext
	}
	if !SafeEditableFilename(value) {
		return ""
	}
	return value
}

func editableTargets(doc map[string]any, kind string) []editableArtifact {
	nodes := []map[string]any{}
	for _, value := range object(doc["mapping"]) {
		if m := object(object(value)["message"]); m != nil {
			nodes = append(nodes, m)
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := numeric(nodes[i]["create_time"]), numeric(nodes[j]["create_time"])
		if a == b {
			return text(nodes[i]["id"]) < text(nodes[j]["id"])
		}
		return a < b
	})
	ordered := []string{}
	by := map[string]editableArtifact{}
	merge := func(key string, a editableArtifact) {
		if key == "" {
			return
		}
		if prior, ok := by[key]; ok {
			if a.AttachmentID == "" {
				a.AttachmentID = prior.AttachmentID
			}
			if a.FileID == "" {
				a.FileID = prior.FileID
			}
			if a.Name == "" {
				a.Name = prior.Name
			}
			if a.MIME == "" {
				a.MIME = prior.MIME
			}
			if a.SandboxPath == "" {
				a.SandboxPath = prior.SandboxPath
			}
			if a.MessageID == "" {
				a.MessageID = prior.MessageID
			}
			a.Created = max(a.Created, prior.Created)
		} else {
			ordered = append(ordered, key)
		}
		by[key] = a
	}
	for _, m := range nodes {
		role := text(object(m["author"])["role"])
		if role != "assistant" && role != "tool" {
			continue
		}
		created := numeric(m["create_time"])
		messageID := text(m["id"])
		walkSearch(m, func(v map[string]any) {
			has := false
			for _, key := range []string{"id", "file_id", "asset_pointer", "name", "file_name", "filename", "mime_type", "mimeType"} {
				if _, ok := v[key]; ok {
					has = true
					break
				}
			}
			if !has {
				return
			}
			a := editableArtifact{AttachmentID: editableFileID(text(v["id"])), FileID: editableFileID(text(v["file_id"])), Name: sanitizeEditableFilename(firstText(v["name"], v["file_name"], v["filename"], v["title"])), MIME: strings.ToLower(strings.Split(firstText(v["mime_type"], v["mimeType"]), ";")[0]), Created: created, MessageID: messageID}
			for _, match := range editablePointer.FindAllStringSubmatch(text(v["asset_pointer"]), -1) {
				if a.AttachmentID == "" {
					a.AttachmentID = match[1]
				}
				if a.FileID == "" {
					a.FileID = match[1]
				}
			}
			raw := string(compactJSON(v))
			if a.AttachmentID == "" {
				a.AttachmentID = editableFileID(raw)
			}
			if a.FileID == "" {
				a.FileID = editableFileID(raw)
			}
			if a.AttachmentID == "" && a.FileID == "" {
				return
			}
			a.SandboxPath = editablePath(raw, kind)
			key := a.AttachmentID
			if key == "" {
				key = a.FileID
			}
			if key == "" {
				key = a.Name
			}
			merge(key, a)
		}, nil)
		re := editablePPTPath
		if kind == "psd" {
			re = editablePSDPath
		}
		for _, match := range re.FindAllStringSubmatch(searchMessageText(m), -1) {
			p := match[1]
			merge(p, editableArtifact{Name: path.Base(p), SandboxPath: p, MessageID: messageID, Created: created})
		}
	}
	artifacts := make([]editableArtifact, 0, len(ordered))
	for _, key := range ordered {
		artifacts = append(artifacts, by[key])
	}
	sort.SliceStable(artifacts, func(i, j int) bool { return artifacts[i].Created < artifacts[j].Created })
	var primary, archive *editableArtifact
	for i := len(artifacts) - 1; i >= 0; i-- {
		a := artifacts[i]
		ext := editableExtension(kind, a.MIME)
		zip := strings.HasSuffix(strings.ToLower(a.Name), ".zip") || strings.HasSuffix(strings.ToLower(a.SandboxPath), ".zip") || ext == ".zip"
		if archive == nil && zip {
			copy := a
			archive = &copy
		}
		isPrimary := ext != "" && ext != ".zip"
		for _, suffix := range []string{".ppt", ".pptx"} {
			if kind == "ppt" && (strings.HasSuffix(strings.ToLower(a.Name), suffix) || strings.HasSuffix(strings.ToLower(a.SandboxPath), suffix)) {
				isPrimary = true
			}
		}
		if kind == "psd" && (strings.HasSuffix(strings.ToLower(a.Name), ".psd") || strings.HasSuffix(strings.ToLower(a.SandboxPath), ".psd")) {
			isPrimary = true
		}
		if primary == nil && isPrimary {
			copy := a
			primary = &copy
		}
	}
	if primary != nil && archive != nil {
		return []editableArtifact{*primary, *archive}
	}
	return nil
}

func (c *Client) editableContent(ctx context.Context, prompt string, images []ImageInput) (map[string]any, map[string]any, []string, error) {
	if len(images) == 0 {
		return map[string]any{"content_type": "text", "parts": []string{prompt}}, nil, nil, nil
	}
	parts, attachments := []any{}, []any{}
	mimes := []string{}
	for i, in := range images {
		conf, format, e := image.DecodeConfig(bytes.NewReader(in.Data))
		if e != nil {
			return nil, nil, nil, failure("invalid_image_input", "editable_upload")
		}
		mimeType := "image/" + format
		extension := "." + format
		if format == "jpeg" {
			extension = ".jpg"
		}
		name := "image_" + strconv.Itoa(i+1) + extension
		apiPath := "/backend-api/files"
		meta, e := c.json(ctx, "POST", apiPath, "editable_upload_prepare", map[string]any{"file_name": name, "file_size": len(in.Data), "use_case": "multimodal", "timezone_offset_min": -480, "reset_rate_limits": false, "store_in_library": true, "library_persistence_mode": "opportunistic"}, c.headers(apiPath, true))
		if e != nil {
			return nil, nil, nil, e
		}
		fileID := text(meta["file_id"])
		if !idPattern.MatchString(fileID) || len(fileID) > 128 {
			return nil, nil, nil, failure("invalid_file_id", "editable_upload_prepare")
		}
		u, e := validateAssetURL(text(meta["upload_url"]), false)
		if e != nil {
			return nil, nil, nil, e
		}
		release, e := acquireAssetSlot(ctx)
		if e != nil {
			return nil, nil, nil, e
		}
		req, e := stdhttp.NewRequestWithContext(ctx, "PUT", u.String(), bytes.NewReader(in.Data))
		if e != nil {
			release()
			return nil, nil, nil, failure("invalid_asset_url", "editable_upload")
		}
		req.Header = stdhttp.Header{"Content-Type": {mimeType}, "x-ms-blob-type": {"BlockBlob"}, "x-ms-version": {"2020-04-08"}, "Origin": {origin}, "Referer": {origin + "/"}, "User-Agent": {c.cfg.UserAgent}}
		resp, e := c.assets.Do(req)
		if e != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			release()
			return nil, nil, nil, assetRequestError(ctx, "editable_upload", e)
		}
		if resp == nil || resp.Body == nil {
			release()
			return nil, nil, nil, failure("invalid_response", "editable_upload")
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		release()
		if readErr != nil {
			return nil, nil, nil, assetRequestError(ctx, "editable_upload", readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, nil, nil, &Error{Kind: "image_upload_failed", Stage: "editable_upload", Status: resp.StatusCode}
		}
		apiPath = "/backend-api/files/" + fileID + "/uploaded"
		if _, e = c.json(ctx, "POST", apiPath, "editable_upload_finish", map[string]any{}, c.headers(apiPath, true)); e != nil {
			return nil, nil, nil, e
		}
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + fileID, "size_bytes": len(in.Data), "width": conf.Width, "height": conf.Height})
		attachments = append(attachments, map[string]any{"id": fileID, "size": len(in.Data), "name": name, "mime_type": mimeType, "width": conf.Width, "height": conf.Height, "source": "library", "library_file_id": text(meta["library_file_id"]), "is_big_paste": false})
		mimes = append(mimes, mimeType)
	}
	parts = append(parts, prompt)
	return map[string]any{"content_type": "multimodal_text", "parts": parts}, map[string]any{"attachments": attachments, "developer_mode_connector_ids": []any{}, "selected_sources": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}}, mimes, nil
}
func (c *Client) editableDownloadURL(ctx context.Context, conversation string, a editableArtifact) (string, error) {
	type candidate struct{ path, route string }
	values := []candidate{}
	if a.SandboxPath != "" && a.MessageID != "" {
		values = append(values, candidate{"/backend-api/conversation/" + conversation + "/interpreter/download?" + url.Values{"message_id": {a.MessageID}, "sandbox_path": {a.SandboxPath}}.Encode(), "/backend-api/conversation/{conversation_id}/interpreter/download"})
	}
	ids := []string{}
	for _, id := range []string{a.AttachmentID, a.FileID} {
		if id != "" && (len(ids) == 0 || ids[0] != id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		values = append(values, candidate{"/backend-api/conversation/" + conversation + "/attachment/" + url.PathEscape(id) + "/download", "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download"})
	}
	for _, id := range ids {
		values = append(values, candidate{"/backend-api/files/download/" + url.PathEscape(id) + "?post_id=&inline=false", "/backend-api/files/download/{file_id}"})
	}
	for _, id := range ids {
		values = append(values, candidate{"/backend-api/files/" + url.PathEscape(id) + "/download", "/backend-api/files/download/{file_id}"})
	}
	for _, candidate := range values {
		h := c.headers(strings.Split(candidate.path, "?")[0], true)
		h["referer"] = []string{origin + "/c/" + conversation}
		h["x-openai-target-route"] = []string{candidate.route}
		doc, e := c.json(ctx, "GET", candidate.path, "editable_download_url", nil, h)
		if e != nil {
			var up *Error
			if errors.As(e, &up) && up.Status == 404 {
				continue
			}
			return "", e
		}
		if value := firstText(doc["download_url"], doc["url"]); value != "" {
			return value, nil
		}
	}
	return "", failure("image_download_failed", "editable_download_url")
}
func (c *Client) downloadEditable(ctx context.Context, root *os.Root, conversation string, a editableArtifact, kind string) (string, error) {
	raw, e := c.editableDownloadURL(ctx, conversation, a)
	if e != nil {
		return "", e
	}
	release, e := acquireAssetSlot(ctx)
	if e != nil {
		return "", e
	}
	defer release()
	for redirects := 0; redirects <= 3; redirects++ {
		u, e := validateAssetURL(raw, false)
		if e != nil {
			return "", e
		}
		req, e := stdhttp.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if e != nil {
			return "", failure("invalid_asset_url", "editable_download")
		}
		req.Header.Set("Accept", "*/*")
		req.Header.Set("User-Agent", "chatgpt2api editable-file fetcher")
		resp, e := c.assets.Do(req)
		if e != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return "", assetRequestError(ctx, "editable_download", e)
		}
		if resp == nil || resp.Body == nil {
			return "", failure("invalid_response", "editable_download")
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if redirects == 3 || location == "" {
				return "", failure("asset_redirect_limit", "editable_download")
			}
			next, e := u.Parse(location)
			if e != nil {
				return "", failure("invalid_asset_url", "editable_download")
			}
			raw = next.String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return "", &Error{Kind: "image_download_failed", Stage: "editable_download", Status: resp.StatusCode}
		}
		if resp.ContentLength > 100<<20 {
			resp.Body.Close()
			return "", failure("response_too_large", "editable_download")
		}
		mimeType := firstText(resp.Header.Get("Content-Type"), a.MIME)
		name := sanitizeEditableFilename(a.Name)
		if name == "" && a.SandboxPath != "" {
			name = sanitizeEditableFilename(path.Base(a.SandboxPath))
		}
		if name == "" {
			_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
			name = sanitizeEditableFilename(params["filename"])
		}
		if name == "" {
			name = sanitizeEditableFilename(path.Base(u.Path))
		}
		ext := editableExtension(kind, mimeType)
		if name == "" {
			name = "artifact" + ext
		} else if path.Ext(name) == "" {
			name += ext
		}
		if !SafeEditableFilename(name) {
			resp.Body.Close()
			return "", failure("image_download_failed", "editable_filename")
		}
		base, extension := strings.TrimSuffix(name, path.Ext(name)), path.Ext(name)
		var file *os.File
		for i := 0; i < 1000; i++ {
			candidate := name
			if i > 0 {
				candidate = base + "_" + strconv.Itoa(i) + extension
			}
			file, e = root.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e == nil {
				name = candidate
				break
			}
			if !os.IsExist(e) {
				break
			}
		}
		if e != nil {
			resp.Body.Close()
			return "", failure("asset_write_failed", "editable_download")
		}
		n, copyErr := io.Copy(file, io.LimitReader(resp.Body, (100<<20)+1))
		resp.Body.Close()
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n == 0 || n > 100<<20 {
			_ = root.Remove(name)
			if ctx.Err() != nil {
				return "", requestError(ctx, "editable_download", ctx.Err())
			}
			return "", failure("image_download_failed", "editable_download")
		}
		return name, nil
	}
	return "", failure("asset_redirect_limit", "editable_download")
}
