package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	http "github.com/bogdanfinn/fhttp"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type CredentialTokens struct{ AccessToken, RefreshToken, IDToken string }
type CredentialError struct {
	Status   int
	Code     string
	Terminal bool
}

func (e *CredentialError) Error() string {
	suffix := ""
	if e.Code != "" {
		suffix = ": " + e.Code
	}
	return fmt.Sprintf("oauth_refresh_http_%d%s", e.Status, suffix)
}

var oauthCode = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// RefreshCredentials uses the baseline OAuth client and fixed official origin;
// it neither fetches account metadata nor records credentials in diagnostics.
func (c *Client) RefreshCredentials(ctx context.Context, refresh string) (CredentialTokens, error) {
	ctx, end, e := c.begin(ctx)
	if e != nil {
		return CredentialTokens{}, e
	}
	defer end()
	if refresh == "" || len(refresh) > 1<<20 {
		return CredentialTokens{}, failure("refresh_token_missing", "credential_refresh")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {"app_2SKx67EdpoN0G6j64rFvigXD"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://auth.openai.com/oauth/token", strings.NewReader(form.Encode()))
	req.Header = http.Header{"accept": {"application/json"}, "content-type": {"application/x-www-form-urlencoded"}, "user-agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"}}
	resp, e := c.http.Do(req)
	if e != nil {
		return CredentialTokens{}, requestError(ctx, "credential_refresh", e)
	}
	if resp == nil || resp.Body == nil {
		return CredentialTokens{}, failure("invalid_response", "credential_refresh")
	}
	body, e := c.readBody(ctx, resp, "credential_refresh")
	if e != nil {
		return CredentialTokens{}, e
	}
	var data map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	valid := dec.Decode(&data) == nil && data != nil
	var trailing any
	if valid {
		valid = dec.Decode(&trailing) == io.EOF
	}
	if resp.StatusCode != 200 || !valid || text(data["access_token"]) == "" {
		nested := object(data["error"])
		code := text(nested["code"])
		if code == "" {
			code = text(data["code"])
		}
		if code == "" {
			code = text(data["error"])
		}
		description := text(data["error_description"])
		if description == "" {
			description = text(nested["message"])
		}
		if description == "" {
			description = text(data["message"])
		}
		if description == "" {
			description = text(nested["description"])
		}
		lower := strings.ToLower(code)
		terminal := false
		if resp.StatusCode != 408 && resp.StatusCode != 429 && resp.StatusCode < 500 {
			terminal = lower == "invalid_grant" || lower == "invalid_refresh_token" || lower == "refresh_token_invalidated" || (resp.StatusCode >= 400 && strings.Contains(strings.ToLower(description), "session has ended"))
		}
		if !oauthCode.MatchString(code) {
			code = ""
		}
		return CredentialTokens{}, &CredentialError{Status: resp.StatusCode, Code: code, Terminal: terminal}
	}
	rt := strings.TrimSpace(text(data["refresh_token"]))
	if rt == "" {
		rt = refresh
	}
	return CredentialTokens{AccessToken: strings.TrimSpace(text(data["access_token"])), RefreshToken: rt, IDToken: strings.TrimSpace(text(data["id_token"]))}, nil
}

type AccountInfo struct{ Me, Conversation, Account map[string]any }

// AccountInformation performs the same three independent calls as Python and
// gives authentication failures precedence. No generation is submitted.
func (c *Client) AccountInformation(ctx context.Context) (AccountInfo, error) {
	ctx, end, e := c.begin(ctx)
	if e != nil {
		return AccountInfo{}, e
	}
	defer end()
	if c.cfg.AccessToken == "" {
		return AccountInfo{}, failure("access_token_required", "account_info")
	}
	paths := []string{"/backend-api/me", "/backend-api/conversation/init", "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480"}
	values := make([]map[string]any, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			method := "GET"
			var payload any
			if i == 1 {
				method = "POST"
				payload = map[string]any{"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": -480}
			}
			h := c.headers(strings.Split(path, "?")[0], true)
			h["accept"] = []string{"application/json"}
			values[i], errs[i] = c.json(callCtx, method, path, "account_info", payload, h)
		}(i, path)
	}
	wg.Wait()
	for _, err := range errs {
		if controlled, ok := err.(*Error); ok && controlled.Status == 401 {
			return AccountInfo{}, err
		}
	}
	for _, err := range errs {
		if err != nil {
			return AccountInfo{}, err
		}
	}
	account := object(object(object(values[2]["accounts"])["default"])["account"])
	return AccountInfo{Me: values[0], Conversation: values[1], Account: account}, nil
}
