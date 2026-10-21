package controlplane

import (
	"context"
	"net/http"
)

func (s *Server) registerRemainingAccounts() {
	s.route("POST", "/api/accounts/{account_id}/test", "admin", "AccountTestRequest", s.testAccount)
	s.route("POST", "/api/accounts/oauth/start", "admin", "OAuthLoginStartRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		return s.OAuth.Start(text(b["email_hint"])), nil
	})
	s.route("POST", "/api/accounts/oauth/finish", "admin", "OAuthLoginFinishRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		tokens, err := s.OAuth.Finish(ctx, text(b["session_id"]), text(b["callback"]))
		if err != nil {
			return nil, err
		}
		payload := Map{"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken, "id_token": tokens.IDToken, "source_type": "web"}
		if b["target_group_id"] != nil {
			gid := slug(b["target_group_id"])
			if err := s.Accounts.ValidateGroup(ctx, gid, 400); err != nil {
				return nil, err
			}
			payload["group_id"] = gid
		}
		add, err := s.Accounts.Add(ctx, []Map{payload}, false)
		if err != nil {
			return nil, err
		}
		items, err := s.Accounts.List(ctx)
		if err != nil {
			return nil, err
		}
		account := byToken(items, tokens.AccessToken)
		if account == nil {
			return nil, problem(500, "created account could not be resolved")
		}
		return s.Accounts.MutationPayload(ctx, Map{"added": add["added"], "skipped": add["skipped"], "synced": 0}, []string{text(account["management_id"])}, nil, nil, nil, true, false)
	})
	for _, provider := range []string{"cpa", "sub2api"} {
		prefix, plural, singular, key, createModel, updateModel, importModel := "/api/cpa/pools", "pools", "pool", "pool_id", "CPAPoolCreateRequest", "CPAPoolUpdateRequest", "CPAImportRequest"
		if provider == "sub2api" {
			prefix, plural, singular, key, createModel, updateModel, importModel = "/api/sub2api/servers", "servers", "server", "server_id", "Sub2APIServerCreateRequest", "Sub2APIServerUpdateRequest", "Sub2APIImportRequest"
		}
		list := func(ctx context.Context) ([]Map, error) {
			items, err := s.RemoteImports.list(ctx, provider)
			out := []Map{}
			for _, m := range items {
				out = append(out, sanitizeImportConfig(provider, m))
			}
			return out, err
		}
		s.route("GET", prefix, "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			items, err := list(ctx)
			return Map{plural: items}, err
		})
		s.route("POST", prefix, "admin", createModel, func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			if text(b["base_url"]) == "" {
				return nil, problem(400, "base_url is required")
			}
			if provider == "cpa" && text(b["secret_key"]) == "" {
				return nil, problem(400, "secret_key is required")
			}
			if provider == "sub2api" && (text(b["email"]) == "" || text(b["password"]) == "") && text(b["api_key"]) == "" {
				return nil, problem(400, "email+password or api_key is required")
			}
			item, err := s.RemoteImports.save(ctx, provider, "", b, true, false)
			if err != nil {
				return nil, err
			}
			items, err := list(ctx)
			return Map{singular: sanitizeImportConfig(provider, item), plural: items}, err
		})
		base := prefix + "/{" + key + "}"
		s.route("POST", base, "admin", updateModel, func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			item, err := s.RemoteImports.save(ctx, provider, r.PathValue(key), b, false, false)
			if err != nil {
				return nil, err
			}
			items, err := list(ctx)
			return Map{singular: sanitizeImportConfig(provider, item), plural: items}, err
		})
		s.route("DELETE", base, "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			_, err := s.RemoteImports.save(ctx, provider, r.PathValue(key), nil, false, true)
			if err != nil {
				return nil, err
			}
			items, err := list(ctx)
			return Map{plural: items}, err
		})
		require := func(ctx context.Context, r *http.Request) (Map, error) {
			source, err := s.RemoteImports.get(ctx, provider, r.PathValue(key))
			if err != nil {
				return nil, err
			}
			if source == nil {
				return nil, problem(404, singular+" not found")
			}
			return source, nil
		}
		if provider == "cpa" {
			s.route("GET", base+"/files", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
				source, err := require(ctx, r)
				if err != nil {
					return nil, err
				}
				files, err := s.RemoteImports.cpaFiles(ctx, source)
				return Map{"pool_id": r.PathValue(key), "files": files}, err
			})
		} else {
			s.route("GET", base+"/groups", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
				source, err := require(ctx, r)
				if err != nil {
					return nil, err
				}
				groups, err := s.RemoteImports.subList(ctx, source, true)
				if err != nil {
					return nil, problem(502, importDiagnostic(err.Error(), source))
				}
				return Map{"server_id": r.PathValue(key), "groups": groups}, nil
			})
			s.route("GET", base+"/accounts", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
				source, err := require(ctx, r)
				if err != nil {
					return nil, err
				}
				if values, ok := r.URL.Query()["group_id"]; ok {
					source["group_id"] = values[len(values)-1]
				}
				accounts, err := s.RemoteImports.subList(ctx, source, false)
				if err != nil {
					return nil, problem(502, importDiagnostic(err.Error(), source))
				}
				return Map{"server_id": r.PathValue(key), "accounts": accounts}, nil
			})
		}
		s.route("POST", base+"/import", "admin", importModel, func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			source, err := require(ctx, r)
			if err != nil {
				return nil, err
			}
			job, err := s.RemoteImports.start(ctx, provider, source, b)
			return Map{"import_job": job}, err
		})
		s.route("GET", base+"/import", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
			source, err := require(ctx, r)
			if err != nil {
				return nil, err
			}
			return Map{"import_job": source["import_job"]}, nil
		})
	}
	s.route("POST", "/api/proxy/clearance/test", "admin", "ClearanceTestRequest", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		result, err := s.testClearance(ctx, text(b["target_url"]))
		return Map{"result": result}, err
	})
}
