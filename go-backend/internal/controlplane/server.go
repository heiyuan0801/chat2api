package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Options struct {
	BootstrapKey, EnvironmentBaseURL, Version string
	Now                                       func() time.Time
	Upstream                                  AccountUpstream
	DataDir                                   string
	Text                                      TextExecutor
	TextCache                                 TextEventCache
	Images                                    ImageExecution
	ImageBackend                              ImageBackend
	ManagementHTTP                            *http.Client
	UpstreamClient                            func(upstream.Config) (*upstream.Client, error)
	EditableBackend                           EditableFileBackend
	Background                                bool
	RuntimeRoot, WebDir                       string
	ManagedUpdates                            bool
	Restart                                   func() error
	TaskWorkers, TaskQueueSize                int
	ProxyProbe                                func(context.Context, string) (Map, error)
}
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Access string `json:"access"`
	Model  string `json:"model,omitempty"`
}
type registeredRoute struct {
	Route
	handler http.HandlerFunc
}
type Server struct {
	DB               *repository.Database
	Auth             *Auth
	Settings         *Settings
	Proxy            *Proxy
	Accounts         *Accounts
	Maintenance      *Maintenance
	Operations       *Operations
	DataDir          string
	Text             TextExecutor
	Images           ImageExecution
	Assets           *ImageAssets
	ManagementHTTP   *http.Client
	NewUpstream      func(upstream.Config) (*upstream.Client, error)
	Tasks            *ImageTasks
	EditableTasks    *EditableTasks
	RemoteImports    *RemoteImports
	OAuth            *OAuthLogin
	Backups          *BackupService
	Retention        *RetentionCleanup
	Clearance        *ProxyClearance
	Updater          *GoUpdater
	webRoot          *os.Root
	webCloseOnce     sync.Once
	webCloseErr      error
	Prompts          *PromptLibrary
	Dashboard        *Dashboard
	Monitor          *RealtimeMonitor
	started          time.Time
	runtimeStats     runtimeProbe
	publicText       *publicTextState
	textCache        TextEventCache
	executionContext context.Context
	executionCancel  context.CancelFunc
	callsMu          sync.Mutex
	callsClosing     bool
	calls            sync.WaitGroup
	callsErr         error
	version          string
	now              func() time.Time
	proxyProbe       func(context.Context, string) (Map, error)
	routes           []registeredRoute
}

func NewServer(ctx context.Context, db *repository.Database, options Options) (*Server, error) {
	if strings.TrimSpace(options.BootstrapKey) == "" {
		return nil, errors.New("explicit bootstrap key required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Version == "" {
		options.Version = "0.0.0"
	}
	s := &Server{DB: db, Auth: NewAuth(db, options.BootstrapKey, options.Now), Settings: NewSettings(db, options.EnvironmentBaseURL, options.Now), Proxy: NewProxy(db, options.Now), version: options.Version, now: options.Now}
	s.started = s.now()
	s.textCache = options.TextCache
	s.Monitor = NewRealtimeMonitor(s)
	s.ManagementHTTP = options.ManagementHTTP
	s.NewUpstream = options.UpstreamClient
	if s.NewUpstream == nil {
		s.NewUpstream = upstream.New
	}
	s.Clearance = NewProxyClearance(s)
	s.proxyProbe = options.ProxyProbe
	if s.proxyProbe == nil {
		s.proxyProbe = nativeProxyProbe
	}
	s.Accounts = NewAccounts(db, s.Proxy, s.Settings, s.now)
	if e := s.Accounts.Initialize(ctx); e != nil {
		return nil, e
	}
	s.Maintenance = NewMaintenance(ctx, s.Accounts, options.Upstream)
	s.Operations = NewOperations(ctx, s.Accounts)
	s.register()
	if options.DataDir != "" {
		if !filepath.IsAbs(options.DataDir) {
			_ = s.Close(ctx)
			return nil, errors.New("explicit absolute data directory required")
		}
		s.executionContext, s.executionCancel = context.WithCancel(ctx)
		s.DataDir = filepath.Clean(options.DataDir)
		if e := s.Accounts.configureCumulative(ctx, s.DataDir); e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		var e error
		s.Assets, e = NewImageAssets(s, s.DataDir)
		if e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		backend := options.ImageBackend
		if backend == nil {
			backend = NativeImageBackend{newClient: s.NewUpstream}
		}
		s.Images = options.Images
		if s.Images == nil {
			s.Images = &NativeImageExecutor{server: s, backend: backend}
		}
		s.Text = options.Text
		if s.Text == nil {
			s.Text = NewNativeTextExecutor(s)
		}
		tasks, e := NewImageTasks(ctx, ImageTaskOptions{Path: filepath.Join(s.DataDir, "image_tasks.json"), Workers: options.TaskWorkers, QueueSize: options.TaskQueueSize, Now: s.now, RetentionHours: func() int {
			values, e := s.Settings.Values(context.Background())
			if e != nil {
				return 24
			}
			return max(1, integer(values["image_retention_hours"], 24))
		}, Execute: s.Images.Execute, Resume: s.Images.Resume, BeginCall: s.beginPublicCall, AfterSuccess: s.autoPushImageTask})
		if e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		s.Tasks = tasks
		s.registerPublicText()
		s.registerPublicImages()
		s.registerImageTasks()
		s.EditableTasks, e = NewEditableTasks(s.executionContext, s, options.EditableBackend)
		if e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		s.RemoteImports, e = NewRemoteImports(s.executionContext, s)
		if e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		s.OAuth = NewOAuthLogin(s)
		s.Backups, e = NewBackupService(s)
		if e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		s.Prompts = NewPromptLibrary(s)
		s.Dashboard = NewDashboard(s)
		s.Retention = NewRetentionCleanup(s)
		s.registerCleanup()
		s.registerRemainingAccounts()
		s.registerGallery()
		s.registerBackups()
		s.registerEditableTasks()
		s.registerPrompts()
		s.registerDashboard()
		s.Updater, e = NewGoUpdater(s, options.RuntimeRoot, options.ManagedUpdates, options.Restart)
		if e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		s.registerGoUpdates()
		if e = s.registerFramework(options.WebDir, options.RuntimeRoot); e != nil {
			_ = s.Close(ctx)
			return nil, e
		}
		s.registerLogs()
		s.registerMonitor()
		if options.Background {
			s.Backups.Start()
			s.Retention.Start()
			s.Maintenance.StartBackground(s.Dashboard)
		}
	}
	return s, nil
}
func (s *Server) Close(ctx context.Context) error {
	s.callsMu.Lock()
	s.callsClosing = true
	s.callsMu.Unlock()
	var taskErr, assetErr, executionErr, editableErr, remoteErr, backupErr, retentionErr, updateErr, webErr error
	if s.executionCancel != nil {
		s.executionCancel()
	}
	if s.Tasks != nil {
		taskErr = s.Tasks.Close(ctx)
	}
	if s.EditableTasks != nil {
		editableErr = s.EditableTasks.Close(ctx)
	}
	if s.RemoteImports != nil {
		remoteErr = s.RemoteImports.Close(ctx)
	}
	if s.Retention != nil {
		retentionErr = s.Retention.Close(ctx)
	}
	if s.Backups != nil {
		backupErr = s.Backups.Close(ctx)
	}
	if s.Updater != nil {
		updateErr = s.Updater.Close(ctx)
	}
	operationErr := s.Operations.Close(ctx)
	maintenanceErr := s.Maintenance.Close(ctx)
	if owner, ok := s.Images.(*NativeImageExecutor); ok {
		executionErr = owner.Close(ctx)
	}
	callsFinished, callErr := s.waitCalls(ctx)
	if s.Assets != nil && executionErr == nil && editableErr == nil && retentionErr == nil && backupErr == nil && remoteErr == nil && callsFinished {
		assetErr = s.Assets.Close()
	}
	if s.webRoot != nil {
		s.webCloseOnce.Do(func() { s.webCloseErr = s.webRoot.Close() })
		webErr = s.webCloseErr
	}
	return errors.Join(taskErr, editableErr, remoteErr, backupErr, retentionErr, updateErr, webErr, operationErr, maintenanceErr, executionErr, callErr, assetErr)
}
func (s *Server) Routes() []Route {
	out := make([]Route, 0, len(s.routes))
	for _, r := range s.routes {
		out = append(out, r.Route)
	}
	if s.DataDir != "" {
		out = append(out, Route{"GET", "/{full_path:path}", "public", ""}, Route{"HEAD", "/{full_path:path}", "public", ""})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Method < out[j].Method
		}
		return out[i].Path < out[j].Path
	})
	return out
}

type endpoint func(context.Context, *http.Request, Map, Map) (any, error)
type Download struct {
	Bytes       []byte
	Reader      io.ReadCloser
	ContentType string
	Headers     map[string]string
}
type detailedProblem struct {
	status int
	detail Map
}

func (p *detailedProblem) Error() string { return text(p.detail["error"]) }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(v)
}
func writeError(w http.ResponseWriter, e error) {
	var v *ValidationError
	if errors.As(e, &v) {
		writeJSON(w, 422, Map{"detail": v.Details})
		return
	}
	var p *Problem
	if errors.As(e, &p) {
		writeJSON(w, p.Status, Map{"detail": Map{"error": p.Message}})
		return
	}
	var d *detailedProblem
	if errors.As(e, &d) {
		writeJSON(w, d.status, Map{"detail": d.detail})
		return
	}
	if errors.Is(e, repository.ErrRevision) {
		writeJSON(w, 409, Map{"detail": Map{"error": "storage revision does not match current data"}})
		return
	}
	if errors.Is(e, context.DeadlineExceeded) {
		writeJSON(w, 504, Map{"detail": Map{"error": "request deadline exceeded"}})
		return
	}
	writeJSON(w, 500, Map{"detail": Map{"error": "Internal Server Error"}})
}
func decodeRequest(w http.ResponseWriter, r *http.Request, model string) (Map, error) {
	if model == "" {
		return Map{}, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	var raw any
	if e := d.Decode(&raw); e != nil {
		var max *http.MaxBytesError
		if errors.As(e, &max) {
			return nil, problem(413, "request body is too large")
		}
		if e == io.EOF {
			return nil, &ValidationError{[]Map{validationIssue("missing", "Field required", []any{"body"}, nil, nil)}}
		}
		return nil, &ValidationError{[]Map{validationIssue("json_invalid", "JSON decode error", []any{"body", 0}, Map{}, Map{"error": "Invalid JSON"})}}
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, &ValidationError{[]Map{validationIssue("json_invalid", "JSON decode error", []any{"body", 0}, Map{}, Map{"error": "Extra data"})}}
	}
	return Validate(model, raw)
}
func (s *Server) route(method, path, access, model string, fn endpoint) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeError(w, problem(500, "Internal Server Error"))
			}
		}()
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeJSON(w, 405, Map{"detail": "Method Not Allowed"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
		defer cancel()
		body, e := decodeRequest(w, r, model)
		if e != nil {
			writeError(w, e)
			return
		}
		var identity Map
		if access != "public" {
			identity, e = s.Auth.Identity(ctx, r.Header.Get("Authorization"))
			if e != nil {
				if access == "status" {
					identity = nil
				} else {
					writeError(w, e)
					return
				}
			}
			if access == "admin" && identity["role"] != "admin" {
				writeError(w, problem(403, "需要管理员权限才能执行这个操作"))
				return
			}
		}
		value, e := fn(ctx, r, body, identity)
		if e != nil {
			writeError(w, e)
			return
		}
		if download, ok := value.(*Download); ok {
			for k, v := range download.Headers {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", download.ContentType)
			w.WriteHeader(200)
			if download.Reader != nil {
				defer download.Reader.Close()
				_, _ = io.Copy(w, download.Reader)
			} else {
				_, _ = w.Write(download.Bytes)
			}
			return
		}
		writeJSON(w, 200, value)
	})
	s.routes = append(s.routes, registeredRoute{Route{method, path, access, model}, handler})
}
func routePathValues(pattern, path string) (map[string]string, bool) {
	a, b := strings.Split(pattern, "/"), strings.Split(path, "/")
	values := map[string]string{}
	for i, part := range a {
		if i >= len(b) {
			return nil, false
		}
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			key := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
			if strings.HasSuffix(key, ":path") && i == len(a)-1 {
				values[strings.TrimSuffix(key, ":path")] = strings.Join(b[i:], "/")
				return values, true
			}
			if b[i] == "" {
				return nil, false
			}
			values[key] = b[i]
		} else if part != b[i] {
			return nil, false
		}
	}
	return values, len(a) == len(b)
}
func routePathMatch(pattern, path string) bool { _, ok := routePathValues(pattern, path); return ok }
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	allow := []string{}
	for _, route := range s.routes {
		if !routePathMatch(route.Path, r.URL.Path) {
			continue
		}
		if route.Method == r.Method {
			values, _ := routePathValues(route.Path, r.URL.Path)
			for key, value := range values {
				r.SetPathValue(key, value)
			}
			route.handler(w, r)
			return
		}
		allow = append(allow, route.Method)
	}
	if s.DataDir != "" {
		if r.Method == "GET" || r.Method == "HEAD" {
			s.serveStatic(w, r)
			return
		}
		if len(allow) == 0 {
			allow = []string{"GET", "HEAD"}
		} else {
			allow = allow[:1]
		}
	}
	if len(allow) == 0 && strings.HasPrefix(r.URL.Path, "/v1/") && strings.HasSuffix(r.URL.Path, "/") {
		canonical := strings.TrimSuffix(r.URL.Path, "/")
		for _, route := range s.routes {
			if !routePathMatch(route.Path, canonical) {
				continue
			}
			target := *r.URL
			target.Path = canonical
			target.RawPath = ""
			if target.Scheme == "" {
				target.Scheme = "http"
				if r.TLS != nil {
					target.Scheme = "https"
				}
			}
			target.Host = r.Host
			if target.Host == "" {
				target.Host = r.URL.Host
			}
			w.Header().Set("Location", target.String())
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
	}

	if len(allow) > 0 {
		allow = unique(allow)
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
			writePublicError(w, r, problem(405, "Method Not Allowed"))
		} else {
			writeJSON(w, 405, Map{"detail": "Method Not Allowed"})
		}
	} else {
		if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
			writePublicError(w, r, problem(404, "Not Found"))
		} else {
			writeJSON(w, 404, Map{"detail": "Not Found"})
		}
	}
}
func queryInt(r *http.Request, key string, def, minValue, maxValue int) (int, error) {
	raw, ok := r.URL.Query()[key]
	if !ok {
		return def, nil
	}
	v := raw[len(raw)-1]
	n, e := strconv.Atoi(v)
	loc := []any{"query", key}
	if e != nil {
		return 0, &ValidationError{[]Map{validationIssue("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v, nil)}}
	}
	if n < minValue {
		return 0, &ValidationError{[]Map{validationIssue("greater_than_equal", "Input should be greater than or equal to "+strconv.Itoa(minValue), loc, v, Map{"ge": minValue})}}
	}
	if maxValue > 0 && n > maxValue {
		return 0, &ValidationError{[]Map{validationIssue("less_than_equal", "Input should be less than or equal to "+strconv.Itoa(maxValue), loc, v, Map{"le": maxValue})}}
	}
	return n, nil
}
func (s *Server) register() {
	s.route("POST", "/auth/login", "identity", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return AuthView(s.version, who), nil
	})
	s.route("GET", "/auth/status", "status", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return AuthView(s.version, who), nil
	})
	s.route("GET", "/version", "public", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return Map{"version": s.version}, nil
	})
	s.route("GET", "/api/auth/users", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		items, e := s.Auth.List(ctx)
		return Map{"items": items}, e
	})
	s.route("POST", "/api/auth/users", "admin", "UserKeyCreateRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		item, raw, e := s.Auth.Create(ctx, text(b["name"]))
		return Map{"item": item, "raw_key": raw}, e
	})
	s.route("POST", "/api/auth/users/{key_id}", "admin", "UserKeyUpdateRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		updates := Map{}
		for k, v := range b {
			if v != nil {
				updates[k] = v
			}
		}
		if len(updates) == 0 {
			return nil, problem(400, "还没有检测到改动，请修改后再保存")
		}
		item, e := s.Auth.Update(ctx, r.PathValue("key_id"), updates)
		return Map{"item": item}, e
	})
	s.route("DELETE", "/api/auth/users/{key_id}", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		keyID := r.PathValue("key_id")
		e := s.Auth.Delete(ctx, keyID)
		return Map{"deleted_id": keyID}, e
	})
	s.route("GET", "/api/settings", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Settings.View(ctx) })
	s.route("PATCH", "/api/settings", "admin", "SettingsPatch", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		result, e := s.Settings.Update(ctx, b)
		if e == nil && s.Retention != nil {
			for _, field := range stringsOf(result["changed_fields"]) {
				if field == "log_retention_hours" || field == "image_retention_hours" {
					e = s.Retention.Notify(ctx)
					break
				}
			}
		}
		return result, e
	})
	s.route("GET", "/api/third-party-apps", "identity", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Settings.Public(ctx) })
	s.route("GET", "/api/model-catalog", "identity", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.modelCatalog(ctx) })
	s.registerAccounts()
	s.registerProxy()
}
