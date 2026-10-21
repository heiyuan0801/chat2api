package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	http "github.com/bogdanfinn/fhttp"
	"io"
	"strings"
)

// ExchangeAuthorizationCode uses the browser-profile transport at the fixed
// OAuth origin. Codes/verifiers and response bodies never enter diagnostics.
func (c *Client) ExchangeAuthorizationCode(ctx context.Context, code, verifier, redirect string) (CredentialTokens, error) {
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return CredentialTokens{}, err
	}
	defer end()
	payload := map[string]string{"client_id": "app_2SKx67EdpoN0G6j64rFvigXD", "grant_type": "authorization_code", "code": code, "code_verifier": verifier, "redirect_uri": redirect}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://auth.openai.com/api/accounts/oauth/token", bytes.NewReader(body))
	if err != nil {
		return CredentialTokens{}, failure("invalid_request", "oauth_login")
	}
	req.Header = http.Header{"accept": {"application/json"}, "content-type": {"application/json"}, "origin": {"https://platform.openai.com"}, "referer": {"https://platform.openai.com/"}, "auth0-client": {"eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"}, "user-agent": {c.cfg.UserAgent}, "sec-ch-ua": {`"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"`}}
	resp, err := c.http.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return CredentialTokens{}, requestError(ctx, "oauth_login", err)
	}
	if resp == nil || resp.Body == nil {
		return CredentialTokens{}, failure("invalid_response", "oauth_login")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return CredentialTokens{}, failure("invalid_response", "oauth_login")
	}
	var data map[string]any
	valid := json.Unmarshal(raw, &data) == nil && data != nil
	if resp.StatusCode != 200 || !valid || text(data["access_token"]) == "" {
		return CredentialTokens{}, fmt.Errorf("OpenAI 拒绝换 token (HTTP %d)", resp.StatusCode)
	}
	access, refresh := strings.TrimSpace(text(data["access_token"])), strings.TrimSpace(text(data["refresh_token"]))
	if refresh == "" {
		return CredentialTokens{}, fmt.Errorf("OpenAI 没有返回 refresh_token（可能 scope 未包含 offline_access 或 code 已使用过）")
	}
	return CredentialTokens{AccessToken: access, RefreshToken: refresh, IDToken: strings.TrimSpace(text(data["id_token"]))}, nil
}
