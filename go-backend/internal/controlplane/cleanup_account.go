package controlplane

import (
	"context"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
)

// cleanupAccounts owns selection and mutation in one Account Repository write
// transaction. A preview reads the same status rule without consuming a slot.
func (s *Server) cleanupAccounts(ctx context.Context, body Map, dry bool) (Map, error) {
	settings, err := s.Settings.Values(ctx)
	if err != nil {
		return nil, err
	}
	invalid := boolean(body["auto_remove_invalid_accounts"], boolean(settings["auto_remove_invalid_accounts"], true))
	limited := boolean(body["auto_remove_rate_limited_accounts"], boolean(settings["auto_remove_rate_limited_accounts"], false))
	result := Map{"dry_run": dry, "invalid": 0, "rate_limited": 0, "total_removed": 0, "auto_remove_invalid_accounts": invalid, "auto_remove_rate_limited_accounts": limited}
	selectItems := func(items []Map) []Map {
		out := make([]Map, 0, len(items))
		bad, rate := 0, 0
		for _, m := range items {
			switch {
			case invalid && m["status"] == "异常":
				bad++
			case limited && m["status"] == "限流":
				rate++
			default:
				out = append(out, m)
			}
		}
		result["invalid"], result["rate_limited"], result["total_removed"] = bad, rate, bad+rate
		return out
	}
	if dry {
		items, err := s.Accounts.List(ctx)
		if err != nil {
			return nil, err
		}
		selectItems(items)
		return result, nil
	}
	callCtx, release, err := s.Accounts.gate.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	_, err = s.DB.Modify(callCtx, "accounts", nil, func(items []Map) ([]Map, error) { return selectItems(normalizeAccounts(items, s.now(), false)), nil })
	if err != nil {
		return nil, err
	}
	s.Accounts.notify()
	if integer(result["total_removed"], 0) > 0 {
		detail := Map{"removed": result["total_removed"], "invalid": result["invalid"], "rate_limited": result["rate_limited"]}
		item := Map{"id": id(16), "time": s.now().In(imageTaskZone).Format("2006-01-02 15:04:05"), "type": "account", "summary": "按账号自动移除策略清理账号", "detail": detail}
		projection := CallRecordProjection(item)
		_, err = repository.NewCallRecords(s.DB).Append(ctx, repository.CallRecordWrite{Payload: item, Outcome: text(projection["outcome"]), BusinessKind: text(projection["business_kind"]), SearchText: text(projection["search_text"])})
	}
	return result, err
}
