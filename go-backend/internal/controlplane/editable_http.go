package controlplane

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
)

func urlPathName(value string) string { return url.PathEscape(value) }
func (s *Server) editableRoute(method, path, model string, fn endpoint) {
	s.routes = append(s.routes, registeredRoute{Route{method, path, "identity", model}, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
		defer cancel()
		if s.executionContext != nil {
			stop := context.AfterFunc(s.executionContext, cancel)
			defer stop()
		}
		defer func() {
			if recover() != nil {
				writePublicError(w, r, problem(500, "Internal Server Error"))
			}
		}()
		body, e := decodePublic(w, r, model)
		if e != nil {
			writePublicError(w, r, e)
			return
		}
		who, e := s.Auth.Identity(ctx, r.Header.Get("Authorization"))
		if e != nil {
			writePublicError(w, r, e)
			return
		}
		result, e := fn(ctx, r, body, who)
		if e != nil {
			writePublicError(w, r, e)
			return
		}
		writeJSON(w, 200, result)
	}})
}
func (s *Server) registerEditableTasks() {
	s.editableRoute("GET", "/v1/editable-file-tasks", "", func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
		limit, e := queryInt(r, "limit", 0, 0, 100)
		if e != nil {
			return nil, e
		}
		return s.EditableTasks.List(ctx, who, strings.Split(r.URL.Query().Get("ids"), ","), limit)
	})
	submit := func(fixed string) endpoint {
		return func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
			kind := fixed
			if kind == "" {
				kind = strings.ToLower(text(body["kind"]))
				if kind == "" {
					kind = "ppt"
				}
				if kind != "ppt" && kind != "psd" {
					return nil, problem(400, "kind must be ppt or psd")
				}
			}
			if e := s.checkContent(ctx, body["prompt"]); e != nil {
				call := s.beginPublicCall(ctx, "/v1/"+kind+"/generations", Map{"model": upstream.EditableFileModel, "prompt": body["prompt"]}, who)
				if call != nil {
					_ = call.Finish(nil, e)
				}
				return nil, e
			}
			return s.EditableTasks.Submit(ctx, who, kind, text(body["prompt"]), editableInputStrings(body["base64_images"]), text(body["client_task_id"]), s.resolveImageBaseURL(r))
		}
	}
	s.editableRoute("POST", "/v1/editable-file-tasks", "EditableFileTaskRequest", submit(""))
	s.editableRoute("POST", "/v1/ppt/generations", "EditableFileTaskRequest", submit("ppt"))
	s.editableRoute("POST", "/v1/psd/generations", "EditableFileTaskRequest", submit("psd"))
	s.editableRoute("DELETE", "/v1/editable-file-tasks/{task_id}", "", func(ctx context.Context, r *http.Request, body, who Map) (any, error) {
		return s.EditableTasks.Delete(ctx, who, r.PathValue("task_id"))
	})
	s.routes = append(s.routes, registeredRoute{Route{"GET", "/files/{file_path:path}", "public", ""}, func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file_path")
		parts := strings.Split(strings.TrimLeft(name, "/"), "/")
		valid := len(parts) == 3 && !strings.ContainsAny(name, "\\\r\n\x00")
		for _, c := range name {
			if c < 32 {
				valid = false
			}
		}
		if valid {
			kind, storage, filename := parts[0], parts[1], parts[2]
			extension := strings.ToLower(path.Ext(filename))
			valid = (kind == "ppt" || kind == "psd") && editableStorageID.MatchString(storage) && upstream.SafeEditableFilename(filename) && (extension == ".zip" || (kind == "ppt" && (extension == ".ppt" || extension == ".pptx")) || (kind == "psd" && extension == ".psd"))
		}
		if !valid {
			writeError(w, problem(404, "file not found"))
			return
		}
		for i := 1; i <= 3; i++ {
			info, e := s.EditableTasks.root.Lstat(strings.Join(parts[:i], "/"))
			if e != nil || info.Mode()&os.ModeSymlink != 0 {
				writeError(w, problem(404, "file not found"))
				return
			}
		}
		directory, e := s.EditableTasks.root.OpenRoot(strings.Join(parts[:2], "/"))
		if e != nil {
			writeError(w, problem(404, "file not found"))
			return
		}
		defer directory.Close()
		file, e := directory.Open(parts[2])
		if e != nil {
			writeError(w, problem(404, "file not found"))
			return
		}
		defer file.Close()
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() {
			writeError(w, problem(404, "file not found"))
			return
		}
		contentTypes := map[string]string{".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation", ".psd": "image/vnd.adobe.photoshop", ".zip": "application/zip"}
		w.Header().Set("Content-Type", contentTypes[strings.ToLower(path.Ext(parts[2]))])
		digest := md5.Sum([]byte(strconv.FormatFloat(float64(info.ModTime().UnixNano())/1e9, 'f', -1, 64) + "-" + strconv.FormatInt(info.Size(), 10)))
		w.Header().Set("ETag", `"`+hex.EncodeToString(digest[:])+`"`)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": parts[2]}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, parts[2], info.ModTime(), file)
	}})
}

func editableInputStrings(value any) []string {
	out := []string{}
	for _, item := range list(value) {
		v, _ := item.(string)
		out = append(out, v)
	}
	return out
}
