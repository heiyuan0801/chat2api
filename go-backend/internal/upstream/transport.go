// Package upstream implements a standalone ChatGPT Web protocol prototype.
// It does not own the public API, account scheduling, or persistent application data.
package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const origin = "https://chatgpt.com"

// HTTPClient is the transport seam used by the browser client and offline fixtures.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

type Config struct {
	CookieHeader              string
	SkipSSLVerify             bool
	AccessToken               string
	ProxyURL                  string
	Profile                   string
	UserAgent                 string
	DeviceID                  string
	SessionID                 string
	Timeout                   time.Duration
	StreamTimeout             time.Duration
	PollInitialWait           time.Duration
	PollInitialWaitConfigured bool
	PollInterval              time.Duration
	MaxBodyBytes              int64
	MaxEventBytes             int
	MaxStreamBytes            int64
	ProofAttempts             int
	PollTimeout               time.Duration
	CheckBeforeHit            *bool
	SettleEnabled             *bool
	SettleTime                time.Duration
	// ImageTiming receives synchronous, credential-free poll phase measurements.
	ImageTiming func(string, time.Duration)
}

// Error deliberately omits response bodies, URLs, tokens and proxy credentials.
type Error struct {
	Kind              string `json:"kind"`
	Stage             string `json:"stage"`
	Status            int    `json:"status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	RetryAfterPresent bool   `json:"-"`
	cause             error
}

func (e *Error) Error() string { return fmt.Sprintf("%s at %s (HTTP %d)", e.Kind, e.Stage, e.Status) }
func (e *Error) Unwrap() error { return e.cause }

func failure(kind, stage string) error { return &Error{Kind: kind, Stage: stage} }

func requestError(ctx context.Context, stage string, err error) error {
	if cause := ctx.Err(); cause != nil {
		return &Error{Kind: "canceled", Stage: stage, cause: cause}
	}
	// Avoid retaining transport errors containing authenticated proxy URLs.
	return failure("network_error", stage)
}

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.Profile == "" {
		cfg.Profile = "chrome_110"
	}
	if _, ok := profiles.MappedTLSClients[cfg.Profile]; !ok {
		return cfg, failure("invalid_profile", "config")
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = uuid()
	}
	if cfg.SessionID == "" {
		cfg.SessionID = uuid()
	}
	for _, v := range []string{cfg.AccessToken, cfg.UserAgent, cfg.DeviceID, cfg.SessionID, cfg.CookieHeader} {
		if strings.ContainsAny(v, "\r\n") {
			return cfg, failure("invalid_header", "config")
		}
	}
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return cfg, failure("invalid_proxy", "config")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return cfg, failure("invalid_proxy", "config")
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 300 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.StreamTimeout == 0 {
		cfg.StreamTimeout = 90 * time.Second
	}
	if cfg.PollInitialWait == 0 && !cfg.PollInitialWaitConfigured {
		cfg.PollInitialWait = 5 * time.Second
	}
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = 60 * time.Second
	}
	if cfg.SettleTime == 0 {
		cfg.SettleTime = 5 * time.Second
	}
	if cfg.PollTimeout <= 0 || cfg.PollTimeout > 30*time.Minute || cfg.SettleTime < 0 || cfg.SettleTime > time.Minute {
		return cfg, failure("invalid_limits", "config")
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	if cfg.MaxEventBytes == 0 {
		cfg.MaxEventBytes = 1 << 20
	}
	if cfg.MaxStreamBytes == 0 {
		cfg.MaxStreamBytes = 32 << 20
	}
	if cfg.ProofAttempts == 0 {
		cfg.ProofAttempts = 500000
	}
	if cfg.Timeout <= 0 || cfg.Timeout > 30*time.Minute || cfg.StreamTimeout <= 0 || cfg.StreamTimeout > 30*time.Minute || cfg.PollInitialWait < 0 || cfg.PollInitialWait > time.Minute || cfg.PollInterval <= 0 || cfg.MaxBodyBytes <= 0 || cfg.MaxBodyBytes > 64<<20 || cfg.MaxEventBytes <= 0 || cfg.MaxEventBytes > 8<<20 || cfg.MaxStreamBytes <= 0 || cfg.MaxStreamBytes > 256<<20 || cfg.ProofAttempts < 1 || cfg.ProofAttempts > 500000 {
		return cfg, failure("invalid_limits", "config")
	}
	return cfg, nil
}

// New creates a browser-profile-aware, certificate-verifying, nonredirecting client.
func New(cfg Config) (*Client, error) {
	var err error
	cfg, err = normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	transport, err := newHTTPTransport(cfg)
	if err != nil {
		return nil, err
	}
	return NewWithTransport(cfg, transport)
}

func newHTTPTransport(cfg Config) (HTTPClient, error) {
	idle := 90 * time.Second
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profiles.MappedTLSClients[cfg.Profile]),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
		tlsclient.WithTimeoutMilliseconds(int(cfg.Timeout.Milliseconds())),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{IdleConnTimeout: &idle, MaxIdleConns: 16, MaxIdleConnsPerHost: 8}),
	}
	if cfg.SkipSSLVerify {
		options = append(options, tlsclient.WithInsecureSkipVerify())
	}
	if cfg.ProxyURL != "" {
		options = append(options, tlsclient.WithProxyUrl(cfg.ProxyURL))
	}
	transport, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, failure("transport_configuration", "config")
	}
	return transport, nil
}

// NewWithTransport allows fixtures or another transport adapter to exercise the
// same protocol implementation. Requests still target the fixed ChatGPT origin.
func NewWithTransport(cfg Config, transport HTTPClient) (*Client, error) {
	var err error
	cfg, err = normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		return nil, errors.New("upstream transport is required")
	}
	return &Client{cfg: cfg, http: transport, assets: publicAssetClient{}, operation: make(chan struct{}, 1)}, nil
}
