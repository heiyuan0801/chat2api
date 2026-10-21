package controlplane

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var managementPattern = regexp.MustCompile(`^acct_[0-9a-f]{24}$`)
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func managementID(token string, preferred any) string {
	s := strings.ToLower(text(preferred))
	if managementPattern.MatchString(s) {
		return s
	}
	return "acct_" + digest(token)[:24]
}
func uniqueManagement(token string, preferred any, used map[string]bool) string {
	s := managementID(token, preferred)
	for n := 1; used[s]; n++ {
		s = "acct_" + digest(fmt.Sprintf("%s\x00%d", token, n))[:24]
	}
	return s
}
func fingerprints(v any, token string) []string {
	xs := []string{}
	for _, s := range stringsOf(v) {
		s = strings.ToLower(s)
		if fingerprintPattern.MatchString(s) {
			xs = append(xs, s)
		}
	}
	xs = unique(xs)
	current := digest(token)
	first := current
	if len(xs) > 0 {
		first = xs[0]
	}
	recent := []string{}
	for _, s := range xs[min(1, len(xs)):] {
		if s != current {
			recent = append(recent, s)
		}
	}
	if current != first {
		recent = append(recent, current)
	}
	if len(recent) > 7 {
		recent = recent[len(recent)-7:]
	}
	return append([]string{first}, recent...)
}
func plan(v any) any {
	s := strings.ToLower(text(v))
	s = strings.NewReplacer("-", "", "_", "", " ", "").Replace(s)
	return map[string]any{"free": "free", "plus": "Plus", "pro": "Pro", "prolite": "ProLite", "team": "Team", "business": "Team", "enterprise": "Enterprise"}[s]
}
func source(v any) string {
	switch strings.ToLower(text(v)) {
	case "codex", "cpa", "cpa_json", "remote_cpa", "sub2api":
		return "codex"
	}
	return "web"
}
func accountBool(v any, def bool) bool {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "y":
			return true
		case "n", "none", "null", "":
			return false
		}
	}
	return boolean(v, def)
}
func normalizedStatus(v any, m Map) string {
	if accountBool(m["auto_disabled"], false) {
		return "禁用"
	}
	if v, ok := m["enabled"]; ok && v != nil && !accountBool(v, true) {
		return "禁用"
	}
	s := strings.ToLower(text(v))
	if s == "" {
		return "正常"
	}
	status := map[string]string{"正常": "正常", "normal": "正常", "ready": "正常", "限流": "限流", "limited": "限流", "rate_limited": "限流", "cooling": "限流", "backoff": "限流", "异常": "异常", "abnormal": "异常", "invalid": "异常", "error": "异常", "incomplete": "异常", "禁用": "禁用", "disabled": "禁用", "auto_disabled": "禁用"}[s]
	if status == "" {
		status = "正常"
	}
	return status
}
func nonempty(v any) bool { return v != nil && text(v) != "" }
func orValue(v any, fallback any) any {
	if !truth(v) {
		return fallback
	}
	return v
}
func deriveQuota(limits []any) (*int, any, *bool) {
	if len(limits) == 0 {
		return nil, nil, nil
	}
	for _, v := range limits {
		m := object(v)
		feature := ""
		for _, k := range []string{"feature_name", "feature", "name", "type"} {
			if text(m[k]) != "" {
				feature = strings.ToLower(text(m[k]))
				break
			}
		}
		if feature == "image_gen" || feature == "image_generation" || feature == "image" || feature == "images" {
			n := max(0, integer(m["remaining"], 0))
			var restore any
			for _, k := range []string{"reset_after", "restore_at", "reset_at"} {
				if text(m[k]) != "" {
					restore = text(m[k])
					break
				}
			}
			unknown := false
			return &n, restore, &unknown
		}
	}
	unknown := true
	return nil, nil, &unknown
}

var diagnosticURL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+(?::[^\s/@]*)?@`)
var diagnosticBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
var diagnosticBasic = regexp.MustCompile(`(?i)(\b(?:Proxy-)?Authorization\s*:\s*Basic)\s+[A-Za-z0-9._~+/=-]+`)
var diagnosticQuery = regexp.MustCompile(`(?i)([?&](?:(?:proxy[_-]?)?(?:user(?:name)?|pass(?:word)?)|access_token|refresh_token|id_token)=)[^&#\s]+`)
var diagnosticKey = regexp.MustCompile(`(?i)((?:^|[^a-z0-9_])["']?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|password)["']?\s*[:=]\s*["']?)[^\s,;"'&}]+`)

func diagnostic(v any, m Map, limit int) string {
	s := text(v)
	proxy := text(m["proxy"])
	if proxy != "" && proxy != "direct" && proxy != "default" && proxy != "inherit" && proxy != "system" {
		s = strings.ReplaceAll(s, proxy, "[proxy]")
	}
	secrets := []string{text(m["access_token"]), text(m["refresh_token"]), text(m["id_token"])}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[credential]")
		}
	}
	s = diagnosticURL.ReplaceAllString(s, "${1}***@")
	s = diagnosticBasic.ReplaceAllString(s, "${1} [credential]")
	s = diagnosticBearer.ReplaceAllString(s, "Bearer [credential]")
	s = diagnosticQuery.ReplaceAllString(s, "${1}***")
	s = diagnosticKey.ReplaceAllString(s, "${1}[credential]")
	r := []rune(s)
	if limit > 0 && len(r) > limit {
		s = string(r[:limit]) + "..."
	}
	return s
}
func NormalizeAccount(item Map, now time.Time) Map {
	token := text(orValue(item["access_token"], item["accessToken"]))
	if token == "" {
		return nil
	}
	m := copyMap(item)
	delete(m, "accessToken")
	m["access_token"] = token
	m["management_id"] = managementID(token, m["management_id"])
	m["access_token_fingerprints"] = fingerprints(m["access_token_fingerprints"], token)
	if strings.EqualFold(text(m["type"]), "codex") {
		m["export_type"] = "codex"
		delete(m, "type")
	}
	limits := list(m["limits_progress"])
	derived, restore, unknown := deriveQuota(limits)
	m["type"] = plan(m["type"])
	m["status"] = normalizedStatus(m["status"], m)
	delete(m, "enabled")
	delete(m, "auto_disabled")
	m["email"], m["user_id"] = orValue(m["email"], nil), orValue(m["user_id"], nil)
	m["proxy"] = text(m["proxy"])
	sourceValue := m["source_type"]
	if !truth(sourceValue) && strings.EqualFold(text(m["export_type"]), "codex") {
		sourceValue = "codex"
	}
	m["source_type"] = source(sourceValue)
	if !nonempty(m["quota"]) && derived != nil {
		m["quota"] = *derived
	}
	m["quota"] = max(0, integer(m["quota"], 0))
	if !nonempty(m["image_quota_unknown"]) {
		m["image_quota_unknown"] = true
		if unknown != nil {
			m["image_quota_unknown"] = *unknown
		}
	}
	m["image_quota_unknown"] = accountBool(m["image_quota_unknown"], true)
	confirmed := (unknown != nil && !*unknown) || (truth(m["last_remote_checked_at"]) && !boolean(m["image_quota_unknown"], true))
	if m["status"] == "正常" && (!confirmed || integer(m["quota"], 0) == 0) {
		m["image_quota_unknown"] = true
	} else if m["status"] == "限流" {
		m["quota"], m["image_quota_unknown"] = 0, false
	}
	m["limits_progress"] = limits
	m["default_model_slug"] = orValue(m["default_model_slug"], nil)
	if truth(restore) && !truth(m["restore_at"]) {
		m["restore_at"] = restore
	}
	m["restore_at"] = orValue(m["restore_at"], nil)
	for _, k := range []string{"success", "fail", "invalid_count"} {
		m[k] = integer(m[k], 0)
	}
	m["last_used_at"] = m["last_used_at"]
	for _, k := range []string{"last_invalid_at", "last_refresh_error", "last_refresh_error_at", "last_remote_checked_at", "last_remote_check_attempt_at", "last_remote_check_error", "last_remote_check_error_at", "last_remote_check_event", "last_token_refresh_at", "last_token_refresh_error", "last_token_refresh_error_at", "refresh_token_invalid_at"} {
		m[k] = orValue(m[k], nil)
	}
	result := strings.ToLower(text(m["last_remote_check_result"]))
	switch result {
	case "pending", "ok", "error", "invalid":
		m["last_remote_check_result"] = result
	default:
		m["last_remote_check_result"] = nil
	}
	if result == "pending" {
		if v := m["pending_auth_remove_invalid"]; v != nil {
			m["pending_auth_remove_invalid"] = accountBool(v, false)
		} else {
			m["pending_auth_remove_invalid"] = nil
		}
		m["pending_auth_scope"] = "account"
		if strings.EqualFold(text(item["pending_auth_scope"]), "image") {
			m["pending_auth_scope"] = "image"
		}
		if v := text(m["pending_auth_verification_id"]); v != "" {
			m["pending_auth_verification_id"] = v
		} else {
			delete(m, "pending_auth_verification_id")
		}
	} else {
		m["pending_auth_remove_invalid"], m["pending_auth_scope"] = nil, nil
		delete(m, "pending_auth_verification_id")
	}
	for _, k := range []string{"last_remote_check_error", "last_refresh_error", "last_token_refresh_error"} {
		if truth(m[k]) {
			m[k] = diagnostic(m[k], m, 0)
		}
	}
	for _, k := range []string{"capability_cooldowns", "capability_failure_counts", "capability_failure_codes", "capability_failed_at"} {
		delete(m, k)
	}
	m["created_at"] = orValue(m["created_at"], now.UTC().Format("2006-01-02 15:04:05"))
	return m
}
func normalizeAccounts(items []Map, now time.Time, recover bool) []Map {
	out := []Map{}
	used := map[string]bool{}
	by := map[string]int{}
	for _, item := range items {
		m := NormalizeAccount(item, now)
		if m == nil {
			continue
		}
		token := text(m["access_token"])
		if index, ok := by[token]; ok {
			delete(used, text(out[index]["management_id"]))
		}
		mid := uniqueManagement(token, m["management_id"], used)
		m["management_id"] = mid
		used[mid] = true
		if recover && m["last_remote_check_result"] == "pending" && m["pending_auth_scope"] != "image" {
			m["last_remote_check_result"] = "error"
			m["last_remote_check_error"] = orValue(m["last_remote_check_error"], "Account verification was interrupted by a service restart.")
			m["last_remote_check_error_at"] = iso(now)
			m["pending_auth_remove_invalid"], m["pending_auth_scope"] = nil, nil
			delete(m, "pending_auth_verification_id")
		}
		if i, ok := by[token]; ok {
			out[i] = m
		} else {
			by[token] = len(out)
			out = append(out, m)
		}
	}
	return out
}
func jwtPayload(token string) Map {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || len(parts[1]) > 1<<20 {
		return Map{}
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if e != nil {
		return Map{}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m Map
	if d.Decode(&m) != nil {
		return Map{}
	}
	return object(m)
}
func positiveTimestamp(v any) any {
	n := integer(v, 0)
	if n > 0 {
		return n
	}
	return nil
}
func timestamp(v any) any {
	switch v.(type) {
	case json.Number, float64, int, int64:
		n := integer(v, 0)
		if n > 10000000000 {
			n /= 1000
		}
		return n
	}
	s := text(v)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05.999999", "2006-01-02T15:04:05.999999", "2006-01-02"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.Unix()
		}
	}
	return nil
}
func CredentialLifecycle(m Map, now time.Time) Map {
	token := text(m["access_token"])
	claims := jwtPayload(token)
	issued, expires := positiveTimestamp(claims["iat"]), positiveTimestamp(claims["exp"])
	access := "valid"
	if token == "" {
		access = "invalid"
		issued, expires = nil, nil
	} else if m["last_remote_check_result"] == "invalid" || (expires != nil && int64(integer(expires, 0)) <= now.Unix()) {
		access = "invalid"
	} else if expires != nil && int64(integer(expires, 0))-now.Unix() <= 86400 {
		access = "expiring"
	}
	refresh := "valid"
	if text(m["refresh_token"]) == "" {
		refresh = "missing"
	} else if truth(m["refresh_token_invalid_at"]) {
		refresh = "invalid"
	}
	availability := "usable"
	if access == "invalid" {
		availability = "unavailable"
		if refresh == "valid" {
			availability = "recoverable"
		}
	}
	al, at := map[string]string{"valid": "AT 有效", "expiring": "AT 临期", "invalid": "AT 失效"}[access], map[string]string{"valid": "success", "expiring": "warning", "invalid": "error"}[access]
	rl, rt := map[string]string{"valid": "RT 有效", "missing": "RT 缺失", "invalid": "RT 失效"}[refresh], map[string]string{"valid": "success", "missing": "neutral", "invalid": "error"}[refresh]
	vl, vt := map[string]string{"usable": "可用", "recoverable": "可恢复", "unavailable": "不可用"}[availability], map[string]string{"usable": "success", "recoverable": "warning", "unavailable": "error"}[availability]
	err := diagnostic(m["last_token_refresh_error"], m, 2000)
	return Map{"access_token_status": access, "access_token_label": al, "access_token_tone": at, "access_token_issued_at": issued, "access_token_expires_at": expires, "refresh_token_status": refresh, "refresh_token_label": rl, "refresh_token_tone": rt, "can_refresh_access_token": refresh == "valid", "credential_availability": availability, "credential_availability_label": vl, "credential_availability_tone": vt, "refresh_token_invalid_at": timestamp(m["refresh_token_invalid_at"]), "last_token_refresh_at": timestamp(m["last_token_refresh_at"]), "last_token_refresh_error": orValue(err, nil), "last_token_refresh_error_at": timestamp(m["last_token_refresh_error_at"])}
}
func backendCategory(m Map) string {
	c := map[string]string{"正常": "normal", "限流": "limited", "异常": "abnormal", "禁用": "disabled"}[text(m["status"])]
	if c == "" {
		c = "normal"
	}
	return c
}
func statusCategory(m Map, now time.Time) string {
	c := backendCategory(m)
	if c != "disabled" && c != "abnormal" && CredentialLifecycle(m, now)["credential_availability"] == "unavailable" {
		return "abnormal"
	}
	return c
}
func selectable(m Map, now time.Time, allowLimited, allowImagePending bool) bool {
	status := text(m["status"])
	if status != "正常" && !(allowLimited && status == "限流") {
		return false
	}
	if CredentialLifecycle(m, now)["credential_availability"] == "unavailable" {
		return false
	}
	return m["last_remote_check_result"] != "pending" || (allowImagePending && m["pending_auth_scope"] == "image")
}
func imageAvailable(m Map, now time.Time) bool { return selectable(m, now, false, false) }
func unlimited(m Map) bool {
	p := strings.ToLower(text(plan(m["type"])))
	return truth(m["image_quota_unknown"]) && (p == "pro" || p == "prolite")
}
func AccountRow(m Map, groupName string, now time.Time) Map {
	creds := CredentialLifecycle(m, now)
	backend, category := backendCategory(m), statusCategory(m, now)
	label := map[string]string{"normal": "正常", "limited": "限流", "abnormal": "异常", "disabled": "禁用"}
	tone := map[string]string{"normal": "success", "limited": "warning", "abnormal": "error", "disabled": "neutral"}
	rawError := ""
	for _, k := range []string{"last_remote_check_error", "last_token_refresh_error", "last_refresh_error"} {
		if truth(m[k]) {
			rawError = diagnostic(m[k], m, 500)
			break
		}
	}
	code, reason := "", "账号已启用，当前凭据可调用"
	switch {
	case backend == "disabled":
		code, reason = "disabled", "账号已手动禁用"
	case backend == "abnormal":
		code, reason = "auth_invalid", "远程确认账号登录态已失效"
	case category == "abnormal":
		code, reason = "credentials_unavailable", "AT 已失效，且没有可用 RT，账号无法参与调度"
	case backend == "limited":
		code, reason = "image_quota_exhausted", "远程确认图片额度已用完"
	case m["last_remote_check_result"] == "pending":
		code, reason = "verification_pending", "正在核验账号状态"
	case m["last_remote_check_result"] == "error" || rawError != "":
		code, reason = "remote_check_failed", "最近一次账号检测失败，尚未确认账号失效"
	case creds["credential_availability"] == "recoverable":
		code, reason = "access_token_refresh_required", "AT 已失效，将使用 RT 自动刷新"
	}
	planValue := text(m["type"])
	planLabel := map[string]string{"free": "Free", "plus": "Plus", "pro": "Pro", "prolite": "ProLite", "team": "Team", "business": "Team", "enterprise": "Enterprise"}[strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(planValue))]
	if planLabel == "" {
		planLabel = planValue
	}
	if planLabel == "" {
		planLabel = "未知"
	}
	src := text(m["source_type"])
	if src != "codex" {
		src = "web"
	}
	sourceLabel := map[string]string{"web": "Web", "codex": "Codex"}[src]
	projection := assignment(m["proxy"], m["proxy_group_id"], nil)
	proxyMode, safeRef := projection["mode"], projection["reference"]
	if proxyMode == "custom" {
		safeRef = "custom"
	}
	if proxyMode == "profile" {
		proxyMode = "custom"
	}
	quota := max(0, integer(m["quota"], 0))
	unknown := truth(m["image_quota_unknown"])
	unlimited := unlimited(m)
	qstate, qlabel := "available", fmt.Sprint(quota)
	if unlimited {
		qstate, qlabel = "unlimited", "无限"
	} else if unknown {
		qstate, qlabel = "unknown", "未知"
	} else if quota <= 0 {
		qstate, qlabel = "exhausted", "0"
	}
	mid, email, user := text(m["management_id"]), text(m["email"]), text(m["user_id"])
	display := email
	if display == "" {
		display = user
	}
	if display == "" {
		display = mid
	}
	enabled := backend != "disabled"
	action, actionLabel := "disable", "停用账号"
	if !enabled {
		action, actionLabel = "enable", "恢复启用"
	}
	out := Map{"id": mid, "email": email, "user_id": user, "display_name": display, "plan": planValue, "plan_label": planLabel, "source": src, "source_label": sourceLabel, "source_plan_label": sourceLabel + " / " + planLabel, "backend_status": label[backend], "status_category": category, "status_label": label[category], "status_tone": tone[category], "status_reason_code": code, "status_reason": reason, "status_raw_error": rawError, "enabled": enabled, "enabled_action": action, "enabled_action_label": actionLabel, "available": imageAvailable(m, now), "quota_remaining": quota, "quota_unknown": unknown, "quota_unlimited": unlimited, "quota_state": qstate, "quota_label": qlabel, "quota_reset_at": timestamp(m["restore_at"]), "group_id": text(m["group_id"]), "group_name": groupName, "proxy": safeRef, "proxy_mode": proxyMode, "proxy_group_id": projection["group_id"], "proxy_label": projection["label"], "success_count": max(0, integer(m["success"], 0)), "failure_count": max(0, integer(m["fail"], 0)), "image_inflight": max(0, integer(m["image_inflight"], 0)), "last_remote_check_result": text(m["last_remote_check_result"]), "last_remote_check_attempt_at": timestamp(m["last_remote_check_attempt_at"]), "last_remote_checked_at": timestamp(m["last_remote_checked_at"]), "created_at": timestamp(m["created_at"]), "last_used_at": timestamp(m["last_used_at"])}
	for k, v := range creds {
		out[k] = v
	}
	return out
}
func AccountDetail(m Map, groupName string, now time.Time) Map {
	out := AccountRow(m, groupName, now)
	out["configuration"] = Map{"type": text(m["type"]), "source_type": text(m["source_type"]), "quota": max(0, integer(m["quota"], 0)), "proxy": text(m["proxy"]), "group_id": text(m["group_id"])}
	out["diagnostics"] = Map{"remote_check_error": diagnostic(m["last_remote_check_error"], m, 2000), "refresh_error": diagnostic(m["last_refresh_error"], m, 2000), "token_refresh_error": diagnostic(m["last_token_refresh_error"], m, 2000), "last_invalid_at": timestamp(m["last_invalid_at"]), "last_refresh_error_at": timestamp(m["last_refresh_error_at"]), "last_token_refresh_at": timestamp(m["last_token_refresh_at"])}
	return out
}
