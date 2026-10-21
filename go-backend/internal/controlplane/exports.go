package controlplane

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var exportName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func prettyJSON(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	_ = e.Encode(v)
	return b.Bytes()
}

var fullExportFields = []string{"management_id", "access_token_fingerprints", "refresh_token", "id_token", "password", "type", "plan_type", "export_type", "source_type", "status", "quota", "image_quota_unknown", "limits_progress", "default_model_slug", "email", "user_id", "account_id", "proxy", "group_id", "created_at", "last_token_refresh_at", "last_token_refresh_error", "last_token_refresh_error_at", "refresh_token_invalid_at", "last_remote_checked_at", "last_remote_check_attempt_at", "last_remote_check_error", "last_remote_check_error_at", "last_remote_check_event", "last_remote_check_result"}

func exportItems(items []Map, selected map[string]bool, full bool) []Map {
	out := []Map{}
	for _, m := range items {
		token := text(m["access_token"])
		if token == "" || !selected[text(m["management_id"])] {
			continue
		}
		if full {
			v := Map{"access_token": token}
			for _, k := range fullExportFields {
				if value, ok := m[k]; ok && value != nil && value != "" {
					v[k] = value
				}
			}
			out = append(out, v)
			continue
		}
		claims, idClaims := jwtPayload(token), jwtPayload(text(m["id_token"]))
		auth, profile := object(claims["https://api.openai.com/auth"]), object(claims["https://api.openai.com/profile"])
		first := func(values ...any) string {
			for _, v := range values {
				if s := text(v); s != "" {
					return s
				}
			}
			return ""
		}
		ts := func(v any) string {
			n := integer(v, 0)
			if n == 0 {
				return ""
			}
			return time.Unix(int64(n), 0).In(time.FixedZone("+08:00", 8*3600)).Format(time.RFC3339)
		}
		v := Map{"type": orValue(m["export_type"], "codex"), "email": first(m["email"], profile["email"], idClaims["email"]), "account_id": first(m["account_id"], auth["chatgpt_account_id"], m["user_id"]), "access_token": token, "refresh_token": text(m["refresh_token"]), "id_token": text(m["id_token"]), "expired": ts(claims["exp"]), "last_refresh": ts(claims["iat"])}
		if password := text(m["password"]); password != "" {
			v["password"] = password
		}
		out = append(out, v)
	}
	return out
}
func (s *Server) exportAccounts(ctx context.Context, r *http.Request, b, who Map) (any, error) {
	targets, missing, e := s.targets(ctx, b, "access_tokens", true)
	if e != nil {
		return nil, e
	}
	all, e := s.Accounts.List(ctx)
	if e != nil {
		return nil, e
	}
	errs := notFoundErrors(missing)
	for _, token := range unique(stringsOf(b["access_tokens"])) {
		if byToken(all, token) == nil {
			errs = append(errs, Map{"id": "token:" + digest(token)[:10], "code": "account_not_found", "message": "legacy access token was not found"})
		}
	}
	if len(errs) > 0 {
		return nil, &detailedProblem{400, Map{"error": "one or more accounts were not found", "errors": errs}}
	}
	if len(targets) == 0 {
		return nil, problem(400, "no valid accounts selected")
	}
	selected := map[string]bool{}
	for _, m := range targets {
		selected[text(m["management_id"])] = true
	}
	format := text(b["format"])
	if format == "" {
		format = "json"
	}
	items := exportItems(all, selected, format != "zip")
	if len(items) == 0 {
		return nil, problem(400, "没有可导出的账号")
	}
	headers := map[string]string{"X-Export-Requested": strconv.Itoa(len(targets)), "X-Exported": strconv.Itoa(len(items)), "X-Skipped": strconv.Itoa(max(0, len(targets)-len(items)))}
	stamp := s.now().Local().Format("20060102-150405")
	filename, contentType := "codex-accounts-"+stamp+".json", "application/json"
	var content []byte
	switch format {
	case "txt":
		filename, contentType = "codex-access-tokens-"+stamp+".txt", "text/plain; charset=utf-8"
		var out strings.Builder
		for _, m := range items {
			out.WriteString(text(m["access_token"]))
			out.WriteByte('\n')
		}
		content = []byte(out.String())
	case "zip":
		filename, contentType = "codex-accounts-"+stamp+".zip", "application/zip"
		var out bytes.Buffer
		archive := zip.NewWriter(&out)
		used := map[string]bool{}
		for i, m := range items {
			fallback := fmt.Sprintf("account-%03d", i+1)
			name := text(orValue(m["email"], orValue(m["account_id"], fallback)))
			base := strings.Trim(exportName.ReplaceAllString(name, "-"), "-._")
			if base == "" {
				base = fallback
			}
			if len(base) > 80 {
				base = base[:80]
			}
			name = base
			for suffix := 2; used[name]; suffix++ {
				name = base + "-" + strconv.Itoa(suffix)
			}
			used[name] = true
			header := &zip.FileHeader{Name: name + ".json", Method: zip.Deflate}
			header.SetModTime(s.now())
			file, e := archive.CreateHeader(header)
			if e != nil {
				return nil, e
			}
			if _, e = file.Write(prettyJSON(m)); e != nil {
				return nil, e
			}
		}
		if e = archive.Close(); e != nil {
			return nil, e
		}
		content = out.Bytes()
	default:
		var payload any = items
		if len(items) == 1 {
			payload = items[0]
		}
		content = prettyJSON(payload)
	}
	headers["Content-Disposition"] = "attachment; filename=\"" + filename + "\""
	return &Download{Bytes: content, ContentType: contentType, Headers: headers}, nil
}
func (s *Server) importCleanup(ctx context.Context, r *http.Request, b, who Map) (any, error) {
	ids, tokens := stringsOf(b["account_ids"]), stringsOf(b["access_tokens"])
	if len(ids) == 0 && len(tokens) == 0 {
		return nil, problem(400, "account_ids is required")
	}
	targets, missing, e := s.Accounts.Targets(ctx, nil, ids, tokens, false)
	if e != nil {
		return nil, e
	}
	abnormal := []Map{}
	for _, m := range targets {
		if statusCategory(m, s.now()) == "abnormal" {
			abnormal = append(abnormal, m)
		}
	}
	removed := []string{}
	events := []Map{}
	errs := notFoundErrors(missing)
	if boolean(b["remove"], false) && len(abnormal) > 0 {
		result, e := s.Accounts.Batch(ctx, targetIDs(abnormal), nil, true)
		if e != nil {
			return nil, e
		}
		removed = stringsOf(result["removed_ids"])
		events = operationEvents("delete_account", nil, removed, errs, labelsFor(abnormal), "账号已删除", "导入后异常账号已删除", s.now())
	}
	return s.Accounts.MutationPayload(ctx, Map{"checked": len(targets), "abnormal": len(abnormal), "removed": len(removed)}, nil, removed, errs, events, false, false)
}
