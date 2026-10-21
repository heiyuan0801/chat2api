package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const oauthRedirect = "https://platform.openai.com/auth/callback"
const oauthAuth0 = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"

type oauthSession struct {
	verifier, state string
	created         time.Time
	busy            bool
}
type OAuthLogin struct {
	server   *Server
	mu       sync.Mutex
	sessions map[string]*oauthSession
}

func NewOAuthLogin(s *Server) *OAuthLogin {
	return &OAuthLogin{server: s, sessions: map[string]*oauthSession{}}
}
func oauthRandom(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("system random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (o *OAuthLogin) purgeLocked() {
	now := o.server.now()
	for key, session := range o.sessions {
		if now.Sub(session.created) > 10*time.Minute {
			delete(o.sessions, key)
		}
	}
	if len(o.sessions) > 64 {
		keys := []string{}
		for key := range o.sessions {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return o.sessions[keys[i]].created.Before(o.sessions[keys[j]].created) })
		for _, key := range keys[:len(keys)-64] {
			delete(o.sessions, key)
		}
	}
}
func (o *OAuthLogin) Start(email string) Map {
	verifier := oauthRandom(64)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	sid := id(16)
	state := sid + "." + oauthRandom(16)
	device := id(16)
	device = device[:8] + "-" + device[8:12] + "-" + device[12:16] + "-" + device[16:20] + "-" + device[20:]
	query := url.Values{"issuer": {"https://auth.openai.com"}, "client_id": {"app_2SKx67EdpoN0G6j64rFvigXD"}, "audience": {"https://api.openai.com/v1"}, "redirect_uri": {oauthRedirect}, "device_id": {device}, "screen_hint": {"login_or_signup"}, "max_age": {"0"}, "scope": {"openid profile email offline_access"}, "response_type": {"code"}, "response_mode": {"query"}, "state": {state}, "nonce": {oauthRandom(32)}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "auth0Client": {oauthAuth0}}
	if strings.TrimSpace(email) != "" {
		query.Set("login_hint", strings.TrimSpace(email))
	}
	o.mu.Lock()
	o.purgeLocked()
	o.sessions[sid] = &oauthSession{verifier: verifier, state: state, created: o.server.now()}
	o.purgeLocked()
	o.mu.Unlock()
	return Map{"session_id": sid, "authorize_url": "https://auth.openai.com/api/accounts/authorize?" + query.Encode(), "expires_in": "600", "redirect_uri_prefix": oauthRedirect}
}
func oauthCallback(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", problem(400, "无法解析 callback URL")
		}
		q := u.Query()
		code, state := strings.TrimSpace(q.Get("code")), strings.TrimSpace(q.Get("state"))
		if code == "" {
			message := q.Get("error_description")
			if message == "" {
				message = q.Get("error")
			}
			if message == "" {
				message = "callback URL 中没有 code 参数"
			}
			return "", "", problem(400, diagnostic(message, nil, 2000))
		}
		return code, state, nil
	}
	return raw, "", nil
}
func (o *OAuthLogin) Finish(ctx context.Context, sid, callback string) (upstream.CredentialTokens, error) {
	code, state, err := oauthCallback(callback)
	if err != nil {
		return upstream.CredentialTokens{}, err
	}
	if code == "" {
		return upstream.CredentialTokens{}, problem(400, "缺少 code 或 callback URL")
	}
	stateSID := ""
	if state != "" {
		stateSID = strings.SplitN(state, ".", 2)[0]
	}
	candidates := unique([]string{stateSID, strings.TrimSpace(sid)})
	if len(candidates) == 0 {
		return upstream.CredentialTokens{}, problem(400, "既未提供 session_id，callback URL 中也未携带 state")
	}
	o.mu.Lock()
	o.purgeLocked()
	var session *oauthSession
	picked := ""
	for _, candidate := range candidates {
		if current := o.sessions[candidate]; current != nil {
			picked, session = candidate, current
			break
		}
	}
	if session == nil {
		o.mu.Unlock()
		return upstream.CredentialTokens{}, problem(400, "OAuth 会话已过期或不存在，请回到导入对话框点\"重新生成\"再走一次")
	}
	if state != "" && state != session.state {
		o.mu.Unlock()
		return upstream.CredentialTokens{}, problem(400, "state 不匹配。常见原因：你点过两次\"打开授权页面\"，但浏览器里登录的还是前一次的窗口。请点\"重新生成\"重来。")
	}
	if session.busy {
		o.mu.Unlock()
		return upstream.CredentialTokens{}, problem(400, "OAuth 授权码正在兑换，请稍后重试")
	}
	session.busy = true
	verifier := session.verifier
	o.mu.Unlock()
	success := false
	defer func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.sessions[picked] == session {
			if success {
				delete(o.sessions, picked)
			} else {
				session.busy = false
			}
		}
	}()
	lease, err := o.server.Proxy.Resolve(ctx, nil, "", false, true, false)
	if err != nil {
		return upstream.CredentialTokens{}, problem(400, "换 token 网络配置异常")
	}
	defer lease.Release()
	client, err := o.server.NewUpstream(upstream.Config{ProxyURL: lease.Profile.ProxyURL, Timeout: 60 * time.Second})
	if err != nil {
		return upstream.CredentialTokens{}, problem(400, "换 token 网络配置异常")
	}
	defer client.Close()
	tokens, err := client.ExchangeAuthorizationCode(ctx, code, verifier, oauthRedirect)
	if err != nil {
		message := diagnostic(err.Error(), Map{"access_token": code, "refresh_token": verifier}, 2000)
		return upstream.CredentialTokens{}, problem(400, message)
	}
	success = true
	return tokens, nil
}
