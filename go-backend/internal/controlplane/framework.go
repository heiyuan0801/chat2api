package controlplane

import (
	"crypto/md5"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed framework_spec.json
var frameworkSpecBytes []byte
var frameworkParameters = func() map[string]Map {
	var meta map[string]Map
	if json.Unmarshal(frameworkSpecBytes, &meta) != nil {
		panic("invalid framework parameter metadata")
	}
	return meta
}()

func (s *Server) registerFramework(webDir, runtimeRoot string) error {
	if webDir == "" && runtimeRoot != "" {
		webDir = filepath.Join(runtimeRoot, "current", "web_dist")
	}
	if webDir != "" {
		if !filepath.IsAbs(webDir) {
			return fmt.Errorf("explicit absolute Vue distribution directory required")
		}
		root, err := os.OpenRoot(webDir)
		if err != nil {
			return fmt.Errorf("Vue distribution directory is unavailable")
		}
		s.webRoot = root
	}
	return nil
}

type headOnlyWriter struct{ http.ResponseWriter }

func (w headOnlyWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w headOnlyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "HEAD" {
		w = headOnlyWriter{w}
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		if r.Method == "OPTIONS" && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT")
			w.Header().Set("Access-Control-Max-Age", "600")
			if headers := r.Header.Get("Access-Control-Request-Headers"); headers != "" {
				w.Header().Set("Access-Control-Allow-Headers", headers)
			}
			method := r.Header.Get("Access-Control-Request-Method")
			if !strings.Contains(",DELETE,GET,HEAD,OPTIONS,PATCH,POST,PUT,", ","+method+",") {
				w.WriteHeader(400)
				_, _ = w.Write([]byte("Disallowed CORS method"))
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "X-Export-Requested, X-Exported, X-Skipped")
	}
	if s.DataDir != "" && (r.Method == "GET" || r.Method == "HEAD") && s.serveFramework(w, r) {
		return
	}
	s.serveHTTP(w, r)
}
func (s *Server) serveFramework(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/openapi.json":
		writeJSON(w, 200, s.OpenAPI())
		return true
	case "/docs":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>chatgpt2api - Swagger UI</title><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"></head><body><div id="swagger-ui"></div><script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script><script>SwaggerUIBundle({url:'/openapi.json',dom_id:'#swagger-ui',deepLinking:true,oauth2RedirectUrl:window.location.origin+'/docs/oauth2-redirect'})</script></body></html>`)
		return true
	case "/redoc":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>chatgpt2api - ReDoc</title></head><body><redoc spec-url="/openapi.json"></redoc><script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script></body></html>`)
		return true
	case "/docs/oauth2-redirect":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>Swagger OAuth redirect</title></head><body><script>if(window.opener&&window.opener.swaggerUIRedirectOauth2){var o=window.opener.swaggerUIRedirectOauth2,p=new URLSearchParams(window.location.search||window.location.hash.slice(1));if(p.get('state')!==o.state){o.errCb({authId:o.auth.name,source:'auth',level:'warning',message:'OAuth state mismatch'});}else{o.callback({auth:o.auth,token:Object.fromEntries(p),isValid:true,redirectUrl:o.redirectUrl});}window.close();}</script></body></html>`)
		return true
	}
	return false
}
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	clean := strings.Trim(r.URL.Path, "/")
	if s.webRoot != nil && !strings.ContainsAny(clean, "\\\x00") && (clean == "" || filepath.IsLocal(clean)) {
		candidates := []string{"index.html"}
		if clean != "" {
			candidates = []string{clean, clean + "/index.html", clean + ".html"}
		}
		for _, candidate := range candidates {
			file, err := s.webRoot.Open(candidate)
			if err != nil {
				continue
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				file.Close()
				continue
			}
			sum := md5.Sum([]byte(fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())))
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
			http.ServeContent(w, r, candidate, info.ModTime(), file)
			file.Close()
			return
		}
	}
	if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
		writePublicError(w, r, problem(404, "Not Found"))
	} else {
		writeJSON(w, 404, Map{"detail": "Not Found"})
	}
}
func (s *Server) OpenAPI() Map {
	schemas, paths := Map{}, Map{}
	var convert func(any) any
	convert = func(raw any) any {
		switch v := raw.(type) {
		case map[string]any:
			out := Map{}
			for key, value := range v {
				if key == "$defs" || key == "x-field-order" {
					continue
				}
				if key == "$ref" {
					value = strings.ReplaceAll(text(value), "#/$defs/", "#/components/schemas/")
				}
				out[key] = convert(value)
			}
			return out
		case []any:
			out := []any{}
			for _, value := range v {
				out = append(out, convert(value))
			}
			return out
		default:
			return raw
		}
	}
	for _, source := range []map[string]Map{baseline.Models, publicSpec.Models, stage4Models} {
		for name, schema := range source {
			for def, body := range object(schema["$defs"]) {
				if _, ok := schemas[def]; !ok {
					schemas[def] = convert(body)
				}
			}
			schemas[name] = convert(schema)
		}
	}
	for _, route := range s.routes {
		if strings.Contains(route.Path, ":path}") && route.Path == "/{full_path:path}" {
			continue
		}
		path := route.Path
		for _, part := range strings.Split(path, "/") {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, ":path}") {
				path = strings.ReplaceAll(path, part, strings.ReplaceAll(part, ":path}", "}"))
			}
		}
		ops := object(paths[path])
		operation := Map{"operationId": strings.ToLower(route.Method) + "_" + strings.NewReplacer("/", "_", "{", "", "}", "", ":", "_").Replace(route.Path), "responses": Map{"200": Map{"description": "Successful Response"}, "422": Map{"description": "Validation Error"}}}
		if route.Method == "POST" && route.Path == "/api/system/update" {
			operation["responses"] = Map{"202": Map{"description": "Accepted update task"}, "409": Map{"description": "Update unavailable"}}
		}
		if route.Model != "" {
			media := "application/json"
			operation["requestBody"] = Map{"required": true, "content": Map{media: Map{"schema": Map{"$ref": "#/components/schemas/" + route.Model}}}}
		}
		parameters := []Map{}
		for _, part := range strings.Split(route.Path, "/") {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				name := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
				name = strings.TrimSuffix(name, ":path")
				parameters = append(parameters, Map{"name": name, "in": "path", "required": true, "schema": Map{"type": "string"}})
			}
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		if meta, ok := frameworkParameters[route.Method+" "+route.Path]; ok && len(list(meta["parameters"])) > 0 {
			operation["parameters"] = copyMap(meta)["parameters"]
		}
		if route.Access != "public" && route.Access != "status" {
			operation["security"] = []Map{{"BearerAuth": []string{}}}
		}
		ops[strings.ToLower(route.Method)] = operation
		paths[path] = ops
	}
	return Map{"openapi": "3.1.0", "info": Map{"title": "chatgpt2api", "version": s.version}, "paths": paths, "components": Map{"schemas": schemas, "securitySchemes": Map{"BearerAuth": Map{"type": "http", "scheme": "bearer"}}}}
}
