package controlplane

import (
	"context"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"time"
)

// TextExecutor is a replaceable native conversation boundary. The callback emits
// complete snapshots; protocol adapters convert them to public deltas.
type TextExecutor interface {
	Stream(context.Context, []upstream.Message, string, string, func(string) error) (upstream.TextResult, error)
	Search(context.Context, string) (Map, error)
}
type NativeTextExecutor struct {
	s         *Server
	newClient func(upstream.Config) (*upstream.Client, error)
}

func NewNativeTextExecutor(s *Server) TextExecutor {
	return NewNativeTextExecutorWithClient(s, s.NewUpstream)
}

// NewNativeTextExecutorWithClient reuses the upstream transport seam for offline
// protocol and account-lifecycle verification. Production uses NewNativeTextExecutor.
func NewNativeTextExecutorWithClient(s *Server, factory func(upstream.Config) (*upstream.Client, error)) TextExecutor {
	if factory == nil {
		factory = upstream.New
	}
	return &NativeTextExecutor{s: s, newClient: factory}
}
func (n *NativeTextExecutor) client(ctx context.Context, excluded []string, search bool) (*upstream.Client, *AccountLease, error) {
	lease, e := n.s.Accounts.AcquireText(ctx, excluded)
	if e != nil {
		var pe *PoolError
		if !search && errors.As(e, &pe) && pe.Kind == "unavailable" {
			egress, resolveErr := n.s.Proxy.Resolve(ctx, nil, "", false, true, false)
			if resolveErr != nil {
				return nil, nil, resolveErr
			}
			lease = &AccountLease{Account: Map{}, Egress: egress}
		} else {
			if search && errors.As(e, &pe) && pe.Kind == "unavailable" {
				e = problem(502, "no available text account")
			}
			return nil, nil, e
		}
	}
	cfg := upstream.Config{AccessToken: text(lease.Account["access_token"]), Timeout: 300 * time.Second}
	if search {
		cfg.Timeout = 600 * time.Second
	}
	if lease.Egress != nil {
		cfg.ProxyURL = lease.Egress.Profile.ProxyURL
		applyProxySession(lease.Egress.Profile, &cfg)
	}
	client, e := n.newClient(cfg)
	if e != nil {
		lease.Release()
		return nil, nil, e
	}
	return client, lease, nil
}
func authTextFailure(e error) bool {
	var up *upstream.Error
	return errors.As(e, &up) && (up.Kind == "auth_invalid" || up.Status == 401)
}
func (n *NativeTextExecutor) Stream(ctx context.Context, messages []upstream.Message, model, effort string, emit func(string) error) (upstream.TextResult, error) {
	excluded := []string{}
	emitted := false
	for attempt := 0; attempt < 2; attempt++ {
		client, lease, e := n.client(ctx, excluded, false)
		if e != nil {
			return upstream.TextResult{}, e
		}
		token := text(lease.Account["access_token"])
		result, e := client.StreamTextWithOptions(ctx, messages, model, effort, func(snapshot string) error {
			if snapshot != "" {
				emitted = true
			}
			return emit(snapshot)
		})
		client.Close()
		lease.Release()
		if e == nil {
			observePublicAccount(ctx, lease.Account)
			if mid := text(lease.Account["management_id"]); mid != "" {
				if e = n.s.Accounts.RecordTextUse(ctx, mid); e != nil {
					return result, e
				}
			}
			return result, nil
		}
		if token != "" && authTextFailure(e) {
			_ = n.s.Maintenance.scheduleVerification(ctx, lease.Account, "text_stream")
			if !emitted && attempt == 0 {
				excluded = append(excluded, token)
				continue
			}
		}
		observePublicAccount(ctx, lease.Account)
		return result, e
	}
	return upstream.TextResult{}, problem(502, "no available text account")
}
func (n *NativeTextExecutor) Search(ctx context.Context, query string) (Map, error) {
	ctx, cancel := context.WithTimeout(ctx, 600*time.Second)
	defer cancel()
	excluded := []string{}
	for attempt := 0; attempt < 2; attempt++ {
		client, lease, e := n.client(ctx, excluded, true)
		if e != nil {
			return nil, e
		}
		result, e := client.Search(ctx, query)
		client.Close()
		lease.Release()
		if e == nil {
			observePublicAccount(ctx, lease.Account)
			if e = n.s.Accounts.RecordTextUse(ctx, text(lease.Account["management_id"])); e != nil {
				return nil, e
			}
			return result, nil
		}
		if authTextFailure(e) {
			_ = n.s.Maintenance.scheduleVerification(ctx, lease.Account, "search")
			if attempt == 0 {
				excluded = append(excluded, text(lease.Account["access_token"]))
				continue
			}
		}
		return nil, e
	}
	return nil, problem(502, "no available text account")
}
