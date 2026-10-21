package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/controlplane"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var buildVersion = "0.0.0"

func run() error {
	flags := flag.NewFlagSet("control-server", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbURL := flags.String("database-url", os.Getenv("DATABASE_URL"), "explicit SQLite/PostgreSQL rehearsal database URL")
	redisURL := flags.String("redis-url", os.Getenv("REDIS_URL"), "optional shared public text cache URL")
	redisNamespace := flags.String("redis-namespace", os.Getenv("CHATGPT2API_REDIS_NAMESPACE"), "isolated text cache namespace; defaults to a database/bootstrap fingerprint")
	listen := flags.String("listen", "127.0.0.1:18080", "HTTP listen address (loopback by default)")
	dataDir := flags.String("data-dir", os.Getenv("CHATGPT2API_DATA_DIR"), "explicit isolated data directory enabling public API and durable image tasks")
	webDir := flags.String("web-dir", os.Getenv("CHATGPT2API_WEB_DIR"), "explicit built Vue distribution directory")
	runtimeRoot := flags.String("runtime-root", os.Getenv("CHATGPT2API_RUNTIME_ROOT"), "explicit marked Go runtime with releases/current/previous")
	managedUpdates := flags.Bool("managed-updates", false, "explicitly enable verified Go bundle installation and graceful process replacement")
	background := flags.Bool("background", false, "explicitly enable account, metric, retention and backup schedules")
	initialize := flags.Bool("init-schema", false, "explicitly initialize compatible rehearsal tables")
	initOnly := flags.Bool("init-only", false, "initialize schema and exit without starting workers or HTTP; requires -init-schema")
	healthURL := flags.String("healthcheck-url", "", "check an existing server's authenticated settings endpoint and exit")
	version := flags.String("version", buildVersion, "application version reported to Vue")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("invalid command-line flags")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	key := strings.TrimSpace(os.Getenv("CHATGPT2API_AUTH_KEY"))
	if *healthURL != "" {
		if key == "" {
			return fmt.Errorf("health check requires CHATGPT2API_AUTH_KEY")
		}
		endpoint, err := url.Parse(*healthURL)
		if err != nil || endpoint.Scheme != "http" || !net.ParseIP(endpoint.Hostname()).IsLoopback() || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/api/settings" {
			return fmt.Errorf("health check requires a loopback HTTP settings URL")
		}
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, err := http.NewRequest(http.MethodGet, *healthURL, nil)
		if err != nil {
			return fmt.Errorf("invalid health check URL")
		}
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("HTTP health check failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP health check failed")
		}
		return nil
	}
	if *initOnly && !*initialize {
		return fmt.Errorf("-init-only requires -init-schema")
	}
	if strings.TrimSpace(*dbURL) == "" || key == "" {
		return fmt.Errorf("explicit database URL and CHATGPT2API_AUTH_KEY are required")
	}
	if *runtimeRoot != "" {
		if !filepath.IsAbs(*runtimeRoot) {
			return fmt.Errorf("explicit absolute Go runtime directory required")
		}
		value, err := os.ReadFile(filepath.Join(*runtimeRoot, "current", "VERSION"))
		if err != nil || strings.TrimSpace(string(value)) == "" {
			return fmt.Errorf("Go runtime VERSION is unavailable")
		}
		*version = strings.TrimSpace(string(value))
	}
	if *managedUpdates && (*runtimeRoot == "" || *dataDir == "") {
		return fmt.Errorf("managed updates require explicit runtime and data directories")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if *initialize && strings.HasPrefix(*dbURL, "sqlite:///") && *dbURL != "sqlite:///:memory:" {
		if e := repository.CreateSQLiteFile(*dbURL); e != nil {
			return e
		}
	}
	db, e := repository.Open(connectCtx, *dbURL)
	if e != nil {
		return e
	}
	defer db.Close()
	if *initialize {
		if e = db.Initialize(connectCtx); e != nil {
			return fmt.Errorf("schema initialization failed")
		}
	}
	if *initOnly {
		return nil
	}
	var textCache *controlplane.RedisTextCache
	if strings.TrimSpace(*redisURL) != "" {
		if *dataDir == "" {
			return fmt.Errorf("Redis text cache requires an explicit data directory")
		}
		if *redisNamespace == "" {
			fingerprint := sha256.Sum256([]byte(key + "\x00" + *dbURL))
			*redisNamespace = fmt.Sprintf("chatgpt2api:%x", fingerprint[:12])
		}
		textCache, e = controlplane.NewRedisTextCache(connectCtx, *redisURL, *redisNamespace)
		if e != nil {
			return e
		}
		defer textCache.Close()
	}
	restartRequested := make(chan struct{}, 1)
	var restart func() error
	if *managedUpdates {
		restart = func() error {
			select {
			case restartRequested <- struct{}{}:
			default:
			}
			return nil
		}
	}
	options := controlplane.Options{BootstrapKey: key, EnvironmentBaseURL: os.Getenv("CHATGPT2API_BASE_URL"), Version: *version, DataDir: *dataDir, WebDir: *webDir, RuntimeRoot: *runtimeRoot, ManagedUpdates: *managedUpdates, Restart: restart, Background: *background}
	// Do not place a typed nil in the optional interface.
	if textCache != nil {
		options.TextCache = textCache
	}
	s, e := controlplane.NewServer(ctx, db, options)
	if e != nil {
		return fmt.Errorf("control-plane initialization failed")
	}
	listener, e := net.Listen("tcp", *listen)
	if e != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Close(closeCtx)
		return fmt.Errorf("HTTP listener failed")
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	cacheBackend := "memory"
	if textCache != nil {
		cacheBackend = "redis"
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "listening", "address": listener.Addr().String(), "backend": db.Backend(), "text_cache": cacheBackend, "routes": len(s.Routes())})
	updating := false
	select {
	case e = <-done:
	case <-ctx.Done():
	case <-restartRequested:
		updating = true
	}
	shutdown, finish := context.WithTimeout(context.Background(), 10*time.Second)
	defer finish()
	closeErr := s.Close(shutdown)
	shutdownErr := server.Shutdown(shutdown)
	if e != nil && e != http.ErrServerClosed {
		return errors.Join(fmt.Errorf("HTTP server stopped"), closeErr, shutdownErr)
	}
	if updating && closeErr == nil && shutdownErr == nil {
		binary := filepath.Join(*runtimeRoot, "current", "bin", "control-server")
		arguments := append([]string(nil), os.Args...)
		arguments[0] = binary
		if err := syscall.Exec(binary, arguments, os.Environ()); err != nil {
			return fmt.Errorf("Go update installed but process replacement failed; use the previous release pointer to roll back")
		}
	}
	return errors.Join(closeErr, shutdownErr)
}
func main() {
	if e := run(); e != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": e.Error()})
		os.Exit(1)
	}
}
