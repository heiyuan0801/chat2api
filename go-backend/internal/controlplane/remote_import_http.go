package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Explicit administrator-configured management endpoints may be private hosts.
// Protocols, credentials-in-URL, redirect count, response bytes and deadlines
// are bounded; user-supplied image fetches use the separate pinned-public owner.
func (i *RemoteImports) request(ctx context.Context, method, base, path string, query url.Values, headers map[string]string, payload any) (any, error) {
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("invalid remote management URL")
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("invalid remote request")
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client, closeClient, err := i.server.configuredHTTP(ctx)
	if err != nil {
		return nil, fmt.Errorf("remote connection configuration failed")
	}
	defer closeClient()
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := boundedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil || len(raw) > 64<<20 {
		return nil, fmt.Errorf("remote response is too large or incomplete")
	}
	var out any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&out) != nil {
		return nil, fmt.Errorf("remote response payload is invalid")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("remote response payload is invalid")
	}
	return out, nil
}
func (i *RemoteImports) cpaFiles(ctx context.Context, source Map) ([]Map, error) {
	if text(source["base_url"]) == "" || text(source["secret_key"]) == "" {
		return []Map{}, nil
	}
	raw, err := i.request(ctx, "GET", text(source["base_url"]), "/v0/management/auth-files", nil, map[string]string{"Authorization": "Bearer " + text(source["secret_key"])}, nil)
	if err != nil {
		return nil, fmt.Errorf("remote list failed: %w", err)
	}
	files, ok := object(raw)["files"].([]any)
	if !ok {
		return nil, fmt.Errorf("remote list payload is invalid")
	}
	out := []Map{}
	for _, v := range files {
		m := object(v)
		name := text(m["name"])
		if name != "" {
			out = append(out, Map{"name": name, "email": text(orValue(m["email"], m["account"]))})
		}
	}
	return out, nil
}
func credentialSection(raw Map, keys ...string) Map {
	for _, key := range keys {
		if m := object(raw[key]); len(m) > 0 {
			return m
		}
		if s, ok := raw[key].(string); ok {
			var m Map
			if json.Unmarshal([]byte(s), &m) == nil && len(m) > 0 {
				return m
			}
		}
	}
	return Map{}
}
func importCredentials(raw Map) Map {
	sources := []Map{}
	for _, key := range []string{"credentials", "credential", "tokens", "auth"} {
		if m := object(raw[key]); len(m) > 0 {
			sources = append(sources, m)
		}
	}
	sources = append(sources, raw)
	out := Map{}
	for target, aliases := range map[string][]string{"access_token": {"access_token", "accessToken", "token"}, "refresh_token": {"refresh_token", "refreshToken"}, "id_token": {"id_token", "idToken"}} {
		for _, source := range sources {
			for _, key := range aliases {
				if value := text(source[key]); value != "" {
					out[target] = value
					break
				}
			}
			if out[target] != nil {
				break
			}
		}
	}
	return out
}
func (i *RemoteImports) cpaFetch(ctx context.Context, source Map, name string) (Map, error) {
	raw, err := i.request(ctx, "GET", text(source["base_url"]), "/v0/management/auth-files/download", url.Values{"name": {name}}, map[string]string{"Authorization": "Bearer " + text(source["secret_key"])}, nil)
	if err != nil {
		return nil, err
	}
	m := object(raw)
	if _, ok := raw.(map[string]any); !ok {
		return nil, fmt.Errorf("invalid payload")
	}
	credentials := importCredentials(m)
	if text(credentials["access_token"]) == "" {
		return nil, fmt.Errorf("missing access_token")
	}
	credentials["source_type"] = "codex"
	return credentials, nil
}
func subUnwrap(raw any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	if _, has := m["data"]; !has {
		return raw
	}
	_, code := m["code"]
	allEnvelope := true
	for key := range m {
		switch key {
		case "code", "message", "msg", "data", "success", "status":
		default:
			allEnvelope = false
		}
	}
	if code || allEnvelope {
		return m["data"]
	}
	return raw
}
func subPage(raw any) ([]Map, int) {
	inner := subUnwrap(raw)
	if items, ok := inner.([]any); ok {
		return dicts(items), len(items)
	}
	m := object(inner)
	candidates := []Map{m}
	if nested := object(m["data"]); len(nested) > 0 {
		candidates = append(candidates, nested)
	} else if items, ok := m["data"].([]any); ok {
		return dicts(items), integer(orValue(m["total"], len(items)), len(items))
	}
	for _, candidate := range candidates {
		for _, key := range []string{"items", "list", "accounts", "records", "rows"} {
			if items, ok := candidate[key].([]any); ok {
				return dicts(items), integer(orValue(candidate["total"], orValue(m["total"], len(items))), len(items))
			}
		}
	}
	return []Map{}, 0
}
func subAccount(raw any) Map {
	m := object(subUnwrap(raw))
	for range 3 {
		for _, key := range []string{"account", "item", "record"} {
			if n := object(m[key]); len(n) > 0 {
				return n
			}
		}
		if n := object(m["data"]); len(n) > 0 {
			m = n
		} else {
			break
		}
	}
	return m
}
func subFirst(sources []Map, keys ...string) string {
	for _, source := range sources {
		for _, key := range keys {
			if value := text(source[key]); value != "" {
				return value
			}
		}
	}
	return ""
}
func subAccountID(account, credentials Map) string {
	return subFirst([]Map{account, credentials}, "id", "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId")
}
func subEmail(account, credentials, extra Map) string {
	v := subFirst([]Map{credentials, extra, account}, "email", "email_address", "emailAddress", "account_email", "accountEmail")
	if v == "" {
		v = text(account["name"])
	}
	return v
}
func subPlan(account, credentials, extra Map) string {
	return subFirst([]Map{credentials, extra, account}, "plan_type", "planType", "subscription_tier", "subscriptionTier")
}
func subGroupValue(value any) (string, string) {
	if m, ok := value.(map[string]any); ok {
		return subFirst([]Map{m}, "id", "group_id", "groupId", "account_group_id", "accountGroupId"), subFirst([]Map{m}, "name", "group_name", "groupName", "title", "label")
	}
	switch value.(type) {
	case string, json.Number, int, float64:
		return text(value), ""
	}
	return "", ""
}
func subGroup(account, extra Map) (string, string) {
	for _, source := range []Map{account, extra} {
		for _, key := range []string{"group", "group_info", "groupInfo", "account_group", "accountGroup"} {
			gid, name := subGroupValue(source[key])
			if gid != "" || name != "" {
				return gid, name
			}
		}
		for _, group := range list(source["groups"]) {
			gid, name := subGroupValue(group)
			if gid != "" || name != "" {
				return gid, name
			}
		}
	}
	return subFirst([]Map{account, extra}, "group_id", "groupId", "account_group_id", "accountGroupId"), subFirst([]Map{account, extra}, "group_name", "groupName", "account_group_name", "accountGroupName")
}
func (i *RemoteImports) subHeaders(ctx context.Context, source Map) (map[string]string, error) {
	if key := text(source["api_key"]); key != "" {
		return map[string]string{"x-api-key": key}, nil
	}
	if text(source["email"]) == "" || text(source["password"]) == "" {
		return nil, fmt.Errorf("sub2api server requires email+password or api_key")
	}
	key := text(source["id"]) + ":" + digest(canonical(Map{"base_url": source["base_url"], "email": source["email"], "password": source["password"]}))
	i.mu.Lock()
	cached, ok := i.tokens[key]
	i.mu.Unlock()
	if ok && cached.expires.After(i.server.now()) {
		return map[string]string{"Authorization": "Bearer " + cached.value}, nil
	}
	raw, err := i.request(ctx, "POST", text(source["base_url"]), "/api/v1/auth/login", nil, nil, Map{"email": source["email"], "password": source["password"]})
	if err != nil {
		return nil, fmt.Errorf("sub2api login failed: %w", err)
	}
	body := object(subUnwrap(raw))
	token := text(body["access_token"])
	if token == "" {
		return nil, fmt.Errorf("sub2api login did not return access_token")
	}
	expires := i.server.now().Add(time.Duration(max(60, integer(body["expires_in"], 3600))-300) * time.Second)
	i.mu.Lock()
	if len(i.tokens) >= 128 {
		for key, value := range i.tokens {
			if !value.expires.After(i.server.now()) {
				delete(i.tokens, key)
			}
		}
		if len(i.tokens) >= 128 {
			for key := range i.tokens {
				delete(i.tokens, key)
				break
			}
		}
	}
	i.tokens[key] = remoteLoginToken{value: token, expires: expires}
	i.mu.Unlock()
	return map[string]string{"Authorization": "Bearer " + token}, nil
}
func (i *RemoteImports) subList(ctx context.Context, source Map, groups bool) ([]Map, error) {
	if text(source["base_url"]) == "" {
		return []Map{}, nil
	}
	headers, err := i.subHeaders(ctx, source)
	if err != nil {
		return nil, err
	}
	out := []Map{}
	seen := map[string]bool{}
	path := "/api/v1/admin/accounts"
	if groups {
		path = "/api/v1/admin/groups"
	}
	for page := 1; page <= 10000; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "page_size": {"200"}}
		if !groups {
			query.Set("platform", "openai")
			query.Set("type", "oauth")
			query.Set("account_type", "oauth")
			if gid := text(source["group_id"]); gid != "" {
				query.Set("group", gid)
				query.Set("group_id", gid)
			}
		}
		raw, err := i.request(ctx, "GET", text(source["base_url"]), path, query, headers, nil)
		if err != nil {
			return nil, fmt.Errorf("sub2api list failed: %w", err)
		}
		items, total := subPage(raw)
		if len(items) == 0 {
			break
		}
		newIDs := 0
		for _, account := range items {
			if groups {
				if account["id"] == nil {
					continue
				}
				out = append(out, Map{"id": text(account["id"]), "name": text(account["name"]), "description": text(account["description"]), "platform": text(account["platform"]), "status": text(account["status"]), "account_count": integer(account["account_count"], 0), "active_account_count": integer(account["active_account_count"], 0)})
				continue
			}
			credentials, extra := credentialSection(account, "credentials", "credential"), credentialSection(account, "extra", "metadata", "meta")
			accountID := subAccountID(account, credentials)
			if accountID == "" || seen[accountID] {
				continue
			}
			seen[accountID] = true
			newIDs++
			gid, name := subGroup(account, extra)
			if gid == "" {
				gid = text(source["group_id"])
			}
			if name == "" {
				name = text(source["group_name"])
			}
			out = append(out, Map{"id": accountID, "name": text(account["name"]), "email": subEmail(account, credentials, extra), "plan_type": subPlan(account, credentials, extra), "status": text(account["status"]), "expires_at": text(credentials["expires_at"]), "has_access_token": subFirst([]Map{credentials, account}, "access_token", "accessToken", "token") != "", "has_refresh_token": text(credentials["refresh_token"]) != "", "remote_group_id": gid, "remote_group_name": name})
		}
		if page*200 >= total || len(items) < 200 {
			break
		}
		if !groups && newIDs == 0 {
			return nil, fmt.Errorf("sub2api pagination did not advance")
		}
		if page == 10000 {
			return nil, fmt.Errorf("sub2api pagination limit exceeded")
		}
	}
	return out, nil
}
func subExportAccounts(raw any) []Map {
	m := object(subUnwrap(raw))
	for _, candidate := range []Map{m, object(m["data"])} {
		if items, ok := candidate["accounts"].([]any); ok {
			return dicts(items)
		}
	}
	return []Map{}
}
func subImportPayload(account Map) Map {
	credentials := importCredentials(account)
	nested := credentialSection(account, "credentials", "credential", "tokens", "auth")
	for key, value := range importCredentials(nested) {
		credentials[key] = value
	}
	extra := credentialSection(account, "extra", "metadata", "meta")
	payload := Map{"source_type": "codex"}
	for key, value := range credentials {
		payload[key] = value
	}
	if email := subEmail(account, nested, extra); email != "" {
		payload["email"] = email
	}
	if plan := subPlan(account, nested, extra); plan != "" {
		payload["type"] = plan
	}
	return payload
}
func (i *RemoteImports) subExport(ctx context.Context, source Map, ids []string) (map[string]Map, map[string]string, error) {
	results, errs := map[string]Map{}, map[string]string{}
	headers, err := i.subHeaders(ctx, source)
	if err != nil {
		return results, errs, err
	}
	raw, err := i.request(ctx, "GET", text(source["base_url"]), "/api/v1/admin/accounts/data", url.Values{"ids": {strings.Join(ids, ",")}, "timezone": {"Asia/Shanghai"}, "include_proxies": {"false"}}, headers, nil)
	if err != nil {
		return results, errs, fmt.Errorf("data export %w", err)
	}
	accounts := subExportAccounts(raw)
	if len(accounts) == 0 {
		return results, errs, fmt.Errorf("data export returned no accounts")
	}
	requested := map[string]bool{}
	for _, v := range ids {
		requested[v] = true
	}
	for index, account := range accounts {
		accountID := subAccountID(account, credentialSection(account, "credentials", "credential"))
		if !requested[accountID] && index < len(ids) {
			accountID = ids[index]
		}
		payload := subImportPayload(account)
		if text(payload["access_token"]) == "" {
			errs[accountID] = "data export missing access_token"
			continue
		}
		if accountID == "" {
			errs["unknown"] = "data export missing account id"
			continue
		}
		results[accountID] = payload
	}
	for _, accountID := range ids {
		if results[accountID] == nil && errs[accountID] == "" {
			errs[accountID] = "data export did not return this account"
		}
	}
	return results, errs, nil
}
func (i *RemoteImports) subFetch(ctx context.Context, source Map, accountID string) (Map, error) {
	results, _, exportErr := i.subExport(ctx, source, []string{accountID})
	if results[accountID] != nil {
		return results[accountID], nil
	}
	headers, err := i.subHeaders(ctx, source)
	if err != nil {
		return nil, err
	}
	raw, err := i.request(ctx, "GET", text(source["base_url"]), "/api/v1/admin/accounts/"+url.PathEscape(accountID), nil, headers, nil)
	if err != nil {
		return nil, err
	}
	payload := subImportPayload(subAccount(raw))
	if text(payload["access_token"]) == "" {
		message := "missing access_token"
		if exportErr != nil {
			message += "; export fallback: " + exportErr.Error()
		}
		return nil, fmt.Errorf("%s", message)
	}
	return payload, nil
}
