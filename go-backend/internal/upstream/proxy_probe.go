package upstream

import (
	"context"
	http "github.com/bogdanfinn/fhttp"
	"time"
)

// ProxyStatus probes only the fixed upstream CSRF endpoint. The baseline treats
// HTTP 4xx as a reachable proxy and only HTTP 5xx/network errors as failure.
func (c *Client) ProxyStatus(ctx context.Context) (int, error) {
	ctx, end, e := c.begin(ctx)
	if e != nil {
		return 0, e
	}
	defer end()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/api/auth/csrf", nil)
	req.Header = http.Header{"user-agent": {"Mozilla/5.0 (chatgpt2api proxy test)"}}
	r, e := c.http.Do(req)
	if e != nil {
		return 0, requestError(ctx, "proxy_test", e)
	}
	if r == nil || r.Body == nil {
		return 0, failure("invalid_response", "proxy_test")
	}
	r.Body.Close()
	return r.StatusCode, nil
}
