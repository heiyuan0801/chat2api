package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var inlineReviewImage = regexp.MustCompile(`data:[\w/.+;\-]+;base64,[A-Za-z0-9+/=]+`)

func requestContentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, x := range v {
			parts = append(parts, requestContentText(x))
		}
		return strings.Join(parts, "\n")
	case []Map:
		parts := []string{}
		for _, x := range v {
			parts = append(parts, requestContentText(x))
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		parts := []string{}
		for _, key := range []string{"text", "input_text", "content", "input", "instructions", "system", "prompt", "messages", "tools"} {
			if x, ok := v[key]; ok {
				if s := requestContentText(x); strings.TrimSpace(s) != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
func (s *Server) checkContent(ctx context.Context, value any) error {
	content := requestContentText(value)
	if strings.TrimSpace(content) == "" {
		return nil
	}
	raw, err := s.DB.Document(ctx, "system_settings")
	if err != nil {
		return err
	}
	settings := s.Settings.effective(promote(raw))
	for _, word := range stringsOf(settings["sensitive_words"]) {
		if strings.Contains(content, word) {
			return problem(400, "检测到敏感词，拒绝本次任务")
		}
	}
	review := object(settings["ai_review"])
	if !boolean(review["enabled"], false) {
		return nil
	}
	base, key, model := strings.TrimRight(text(review["base_url"]), "/"), text(review["api_key"]), text(review["model"])
	if base == "" || key == "" || model == "" {
		return problem(400, "ai review config is incomplete")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return problem(400, "ai review config is incomplete")
	}
	failOpen := boolean(review["fail_open"], true)
	failure := func() error {
		if failOpen {
			return nil
		}
		return problem(503, "AI 审核服务暂时不可用，请稍后重试")
	}
	sanitized := []rune(inlineReviewImage.ReplaceAllString(content, "[image]"))
	marker := "\n…[truncated]…\n"
	if len(sanitized) > 100000 {
		half := (100000 - len([]rune(marker))) / 2
		sanitized = append(append(sanitized[:half:half], []rune(marker)...), sanitized[len(sanitized)-half:]...)
	}
	prompt := text(review["prompt"])
	if prompt == "" {
		prompt = "判断用户请求是否允许。只回答 ALLOW 或 REJECT。"
	}
	payload, _ := json.Marshal(Map{"model": model, "messages": []Map{{"role": "user", "content": prompt + "\n\n用户请求:\n" + string(sanitized) + "\n\n只回答 ALLOW 或 REJECT。"}}, "temperature": 0})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return failure()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client, closeClient, err := s.configuredHTTP(ctx)
	if err != nil {
		return failure()
	}
	defer closeClient()
	resp, err := client.Do(req)
	if err != nil {
		return failure()
	}
	defer resp.Body.Close()
	var response Map
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
	decoder.UseNumber()
	if decoder.Decode(&response) != nil {
		return failure()
	}
	choices := dicts(response["choices"])
	if len(choices) == 0 {
		return failure()
	}
	message := object(choices[0]["message"])
	decision := strings.ToLower(text(message["content"]))
	for _, prefix := range []string{"allow", "pass", "true", "yes", "通过", "允许", "安全"} {
		if strings.HasPrefix(decision, prefix) {
			return nil
		}
	}
	for _, prefix := range []string{"reject", "deny", "block", "false", "no", "拒绝", "不允许", "违规", "禁止"} {
		if strings.HasPrefix(decision, prefix) {
			return problem(400, "AI 审核未通过，拒绝本次任务")
		}
	}
	return failure()
}
func (s *Server) configuredHTTP(ctx context.Context) (*http.Client, func(), error) {
	if s.ManagementHTTP != nil {
		return s.ManagementHTTP, func() {}, nil
	}
	lease, err := s.Proxy.Resolve(ctx, nil, "", false, false, false)
	if err != nil {
		return nil, nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if lease.Profile.ProxyURL != "" {
		proxyURL, err := url.Parse(lease.Profile.ProxyURL)
		if err != nil {
			lease.Release()
			return nil, nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, func() { transport.CloseIdleConnections(); lease.Release() }, nil
}
