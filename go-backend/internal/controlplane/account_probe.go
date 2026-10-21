package controlplane

import (
	"context"
	"errors"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func accountQuotaLabel(account Map) string {
	if unlimited(account) {
		return "无限"
	}
	if truth(account["image_quota_unknown"]) {
		return "未知"
	}
	return strconv.Itoa(max(0, integer(account["quota"], 0)))
}
func (s *Server) testAccount(ctx context.Context, r *http.Request, payload, identity Map) (any, error) {
	accountID := r.PathValue("account_id")
	account, err := s.Accounts.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, problem(404, "account not found")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.executionContext != nil {
		stop := context.AfterFunc(s.executionContext, cancel)
		defer stop()
	}
	mode, model, prompt := text(payload["mode"]), text(payload["model"]), text(payload["prompt"])
	label := text(orValue(account["email"], orValue(account["name"], accountID)))
	quotaBefore := accountQuotaLabel(account)
	started := time.Now()
	result := Map{"account_id": accountID, "account_label": label, "mode": mode, "mode_label": map[string]string{"chat": "对话", "image": "画图"}[mode], "model": model, "duration_ms": 0, "content": "", "quota_before_label": quotaBefore, "quota_after_label": quotaBefore, "quota_deducted": false, "error_code": "", "error_message": ""}
	call := s.beginPublicCall(ctx, "/api/accounts/{account_id}/test", payload, identity)
	if call == nil {
		return nil, problem(503, "server is shutting down")
	}
	defer func() { _ = call.Finish(nil, problem(500, "account test was interrupted")) }()
	ctx = context.WithValue(ctx, publicCallContextKey{}, call)
	execute := func() error {
		fresh, _, err := s.Maintenance.Refresh(ctx, accountID, false)
		if err != nil {
			return err
		}
		if fresh != nil {
			account = fresh
		}
		lease, err := s.Proxy.Resolve(ctx, account, "", false, true, false)
		if err != nil {
			return err
		}
		defer lease.Release()
		callCtx, release, err := s.Accounts.gate.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		cfg := upstream.Config{AccessToken: text(account["access_token"]), ProxyURL: lease.Profile.ProxyURL, Timeout: 300 * time.Second}
		settings, err := s.Settings.runtimeValues(callCtx)
		if err != nil {
			return err
		}
		if mode == "image" {
			cfg = nativeImageConfig(account, lease.Profile, settings)
		}
		applyProxySession(lease.Profile, &cfg)
		client, err := s.NewUpstream(cfg)
		if err != nil {
			return err
		}
		defer client.Close()
		if mode == "chat" {
			response, err := client.StreamTextWithOptions(callCtx, []upstream.Message{{Role: "user", Content: prompt}}, model, "", func(string) error { return nil })
			if err != nil {
				return err
			}
			result["content"] = strings.TrimSpace(response.Text)
			if response.ConversationID != "" && call != nil {
				call.Observe(Map{"_conversation_id": response.ConversationID})
			}
			return s.Accounts.RecordTextUse(callCtx, accountID)
		}
		generated, err := client.GenerateImageRequest(callCtx, upstream.ImageRequest{Prompt: prompt, Model: model}, func(stage string) { recordPublicStage(callCtx, stage, Map{"index": 1, "total": 1}) })
		if err != nil {
			return err
		}
		if len(generated.Assets) == 0 {
			return errors.New("上游没有返回图片")
		}
		executor := &NativeImageExecutor{server: s, backend: NativeImageBackend{}}
		formatted, err := executor.format(callCtx, generated, Map{"prompt": prompt, "model": model, "response_format": "b64_json", "base_url": s.resolveImageBaseURL(r)}, settings)
		if err != nil {
			return err
		}
		urls := stringsOf(formatted["_image_urls"])
		if len(urls) == 0 {
			for _, item := range dicts(formatted["data"]) {
				if value := text(item["url"]); value != "" {
					urls = append(urls, value)
				} else if value := text(item["b64_json"]); value != "" {
					urls = append(urls, "data:image/png;base64,"+value)
				}
			}
		}
		urls = unique(urls)
		if len(urls) == 0 {
			return errors.New("上游没有返回图片")
		}
		if err := s.Accounts.recordImageOutcome(callCtx, account, true, true, nil); err != nil {
			return errors.New("图片已生成，但账号额度更新失败")
		}
		updated, err := s.Accounts.Get(callCtx, accountID)
		if err != nil || updated == nil {
			return errors.New("图片已生成，但账号额度更新失败")
		}
		quotaAfter := accountQuotaLabel(updated)
		result["quota_after_label"] = quotaAfter
		before, bErr := strconv.Atoi(quotaBefore)
		after, aErr := strconv.Atoi(quotaAfter)
		result["quota_deducted"] = bErr == nil && aErr == nil && after < before
		markdown := []string{}
		for index, value := range urls {
			markdown = append(markdown, fmt.Sprintf("![image_%d](%s)", index+1, value))
		}
		result["content"] = strings.Join(markdown, "\n\n")
		if call != nil {
			call.Observe(formatted)
		}
		return nil
	}
	err = execute()
	result["duration_ms"] = max(int64(0), time.Since(started).Milliseconds())
	logResult := copyMap(result)
	logResult["_account_email"] = label
	if err != nil {
		result["status"], result["status_label"], result["tone"] = "failed", "测试失败", "danger"
		result["error_code"] = "RuntimeError"
		result["error_message"] = diagnostic(err.Error(), account, 2000)
		logResult = merge(logResult, result)
		logResult["_call_status"], logResult["_call_error"] = "failed", result["error_message"]
	} else {
		result["status"], result["status_label"], result["tone"] = "success", "测试通过", "success"
		logResult = merge(logResult, result)
	}
	if call != nil {
		_ = call.Finish(logResult, nil)
	}
	return result, nil
}
