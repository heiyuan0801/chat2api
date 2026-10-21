package upstream

import (
	"context"
	"io"
	"net"
	stdhttp "net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AssetHTTPClient is a separate transport seam: signed assets never share the
// authenticated browser's cookies, bearer headers, or configured proxy.
type AssetHTTPClient interface {
	Do(*stdhttp.Request) (*stdhttp.Response, error)
}

type publicAssetClient struct{}

// DownloadGeneratedImage keeps authenticated ChatGPT content on the fixed API
// origin. Credentials must never accompany a signed third-party asset URL.
func (c *Client) DownloadGeneratedImage(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := validateAssetURL(rawURL, false)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > 100<<20 {
		return nil, failure("invalid_limits", "asset_download")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	release, err := acquireAssetSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if u.Host != "chatgpt.com" || u.EscapedPath() != "/backend-api/estuary/content" {
		data, _, err := fetchPublicImage(ctx, c.assets, rawURL, maxBytes, false)
		return data, err
	}
	path := u.RequestURI()
	headers := c.headers("/backend-api/estuary/content", true)
	headers["accept"] = []string{"image/*"}
	response, err := c.request(ctx, "GET", path, "asset_download", nil, headers)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength > maxBytes {
		return nil, failure("response_too_large", "asset_download")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, requestError(ctx, "asset_download", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, failure("response_too_large", "asset_download")
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if len(data) == 0 || (media != "" && !strings.HasPrefix(media, "image/") && media != "application/octet-stream") {
		return nil, failure("invalid_image_input", "asset_download")
	}
	return data, nil
}

// PublicAssetHTTPClient reuses the pinned-address, credential-free transport for
// other bounded public downloads. Callers own byte limits and redirect policy.
func PublicAssetHTTPClient() AssetHTTPClient { return publicAssetClient{} }

type closingAssetBody struct {
	io.ReadCloser
	transport *stdhttp.Transport
}

func (b *closingAssetBody) Close() error {
	e := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return e
}

var prohibitedAssetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"),
}

func publicAddress(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, allowed := range []string{"192.0.0.9/32", "192.0.0.10/32", "2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28", "2001:30::/28"} {
		if netip.MustParsePrefix(allowed).Contains(a) {
			return true
		}
	}
	for _, p := range prohibitedAssetPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
func validateAssetURL(raw string, allowHTTP bool) (*url.URL, error) {
	if len(raw) > 16384 || strings.ContainsAny(raw, "\r\n") {
		return nil, failure("invalid_asset_url", "asset_request")
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, failure("invalid_asset_url", "asset_request")
	}
	u.Fragment = ""
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || host == "localhost.localdomain" || strings.HasSuffix(host, ".local") || strings.Contains(host, "%") {
		return nil, failure("asset_host_not_public", "asset_request")
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return nil, failure("invalid_asset_url", "asset_request")
		}
	}
	return u, nil
}
func (publicAssetClient) Do(req *stdhttp.Request) (*stdhttp.Response, error) {
	// Every hop is resolved exactly once and the resulting validated IP is the
	// dial target. A rebinding answer cannot influence a second resolver in Dial.
	u, e := validateAssetURL(req.URL.String(), req.URL.Scheme == "http")
	if e != nil {
		return nil, e
	}
	addresses, e := net.DefaultResolver.LookupNetIP(req.Context(), "ip", u.Hostname())
	if e != nil || len(addresses) == 0 {
		return nil, failure("asset_dns_failed", "asset_request")
	}
	for _, a := range addresses {
		if !publicAddress(a) {
			return nil, failure("asset_host_not_public", "asset_request")
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	transport := &stdhttp.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 20 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			for _, a := range addresses {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, failure("asset_connection_failed", "asset_request")
		}}
	client := &stdhttp.Client{Transport: transport, CheckRedirect: func(*stdhttp.Request, []*stdhttp.Request) error { return stdhttp.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		transport.CloseIdleConnections()
		return nil, requestError(req.Context(), "asset_request", e)
	}
	resp.Body = &closingAssetBody{ReadCloser: resp.Body, transport: transport}
	return resp, nil
}

var publicFetchSlots = make(chan struct{}, 8)

func acquireAssetSlot(ctx context.Context) (func(), error) {
	select {
	case publicFetchSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-publicFetchSlots }) }, nil
	case <-ctx.Done():
		return nil, requestError(ctx, "asset_request", ctx.Err())
	}
}

// FetchPublicImage is the single URL security owner shared by public input
// parsing and signed output downloads. It never uses environment proxies.
func FetchPublicImage(ctx context.Context, rawURL string, maxBytes int64, allowHTTP bool) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	release, e := acquireAssetSlot(ctx)
	if e != nil {
		return nil, "", e
	}
	defer release()
	return fetchPublicImage(ctx, publicAssetClient{}, rawURL, maxBytes, allowHTTP)
}
func fetchPublicImage(ctx context.Context, client AssetHTTPClient, rawURL string, maxBytes int64, allowHTTP bool) ([]byte, string, error) {
	if maxBytes <= 0 || maxBytes > 100<<20 {
		return nil, "", failure("invalid_limits", "asset_download")
	}
	current := rawURL
	for redirects := 0; redirects <= 3; redirects++ {
		u, e := validateAssetURL(current, allowHTTP)
		if e != nil {
			return nil, "", e
		}
		req, e := stdhttp.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if e != nil {
			return nil, "", failure("invalid_asset_url", "asset_download")
		}
		req.Header.Set("Accept", "image/*,*/*;q=0.8")
		req.Header.Set("User-Agent", "chatgpt2api image fetcher")
		resp, e := client.Do(req)
		if e != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, "", assetRequestError(ctx, "asset_download", e)
		}
		if resp == nil || resp.Body == nil {
			return nil, "", failure("invalid_response", "asset_download")
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if redirects == 3 || location == "" {
				return nil, "", failure("asset_redirect_limit", "asset_download")
			}
			next, e := u.Parse(location)
			if e != nil {
				return nil, "", failure("invalid_asset_url", "asset_download")
			}
			current = next.String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, "", &Error{Kind: "image_download_failed", Stage: "asset_download", Status: resp.StatusCode}
		}
		if resp.ContentLength > maxBytes {
			resp.Body.Close()
			return nil, "", failure("response_too_large", "asset_download")
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		resp.Body.Close()
		if e != nil {
			return nil, "", assetRequestError(ctx, "asset_download", e)
		}
		if int64(len(data)) > maxBytes {
			return nil, "", failure("response_too_large", "asset_download")
		}
		if len(data) == 0 {
			return nil, "", failure("empty_asset", "asset_download")
		}
		mime := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		if mime != "" && !strings.HasPrefix(mime, "image/") && mime != "application/octet-stream" && mime != "binary/octet-stream" {
			return nil, "", failure("invalid_image_input", "asset_download")
		}
		return data, mime, nil
	}
	return nil, "", failure("asset_redirect_limit", "asset_download")
}

// NewWithAssetTransport supplies deterministic API and asset adapters for
// offline verification. Production New always uses the public pinned adapter.
func NewWithAssetTransport(cfg Config, api HTTPClient, assets AssetHTTPClient) (*Client, error) {
	if assets == nil {
		return nil, failure("asset_transport_required", "config")
	}
	c, e := NewWithTransport(cfg, api)
	if e != nil {
		return nil, e
	}
	c.assets = assets
	return c, nil
}
func (c *Client) FetchAsset(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	release, e := acquireAssetSlot(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	data, _, e := fetchPublicImage(ctx, c.assets, rawURL, 50<<20, false)
	return data, e
}
