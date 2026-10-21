package controlplane

import (
	"math"
	"strings"
	"time"
)

var imageTaskStages = map[string]string{
	"queued": "排队中", "running": "生成中", "getting_account": "等待账号",
	"image_egress_waiting": "等待出口", "image_egress_ready": "出口就绪", "uploading": "上传图片",
	"bootstrapping": "初始化上游", "getting_token": "获取令牌", "preparing_conversation": "准备会话",
	"starting_generation": "等待上游首包", "generating": "上游生成中", "image_stream_resolve_start": "等待图片结果", "receiving_image": "接收图片",
}

func imageTaskNullableInteger(v any) any {
	if v == nil || v == "" {
		return nil
	}
	if _, ok := v.(bool); ok {
		return nil
	}
	f, ok := number(v)
	if !ok || f > math.MaxInt64 {
		return nil
	}
	return max(int64(0), int64(f))
}
func imageTaskDimension(v any) any {
	n := imageTaskNullableInteger(v)
	if n == nil || n.(int64) == 0 {
		return nil
	}
	return n
}
func imageTaskAssets(raw Map) []Map {
	source := list(raw["data"])
	switch value := raw["results"].(type) {
	case []any:
		source = value
	case []Map:
		source = list(value)
	}
	out := []Map{}
	for _, value := range source {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		asset := Map{}
		for _, field := range []string{"url", "path", "b64_json", "revised_prompt"} {
			v, _ := item[field].(string)
			asset[field] = strings.TrimSpace(v)
		}
		if asset["url"] == "" && asset["b64_json"] == "" {
			continue
		}
		asset["width"] = imageTaskDimension(item["width"])
		asset["height"] = imageTaskDimension(item["height"])
		out = append(out, asset)
	}
	return out
}

// ImageTaskRow is the single task projection consumed by Vue. Raw durable
// statuses intentionally remain compatible with Python's task JSON format.
func ImageTaskRow(raw Map, now time.Time) Map {
	requested, present := raw["requested_count"]
	if !present {
		requested = raw["n"]
	}
	count := imageTaskCount(requested)
	results := imageTaskAssets(raw)
	if len(results) > count {
		results = results[:count]
	}
	succeeded := len(results)
	status := "failed"
	switch strings.ToLower(text(raw["status"])) {
	case "queued":
		status = "queued"
	case "running":
		status = "running"
	case "success":
		if succeeded >= count {
			status = "success"
		} else if succeeded > 0 {
			status = "partial_success"
		}
	case "error":
		if integer(raw["status_code"], 0) == 400 || imageProblem(text(raw["error_code"]), 0, "", nil).Status == 400 {
			status = "text_review"
		}
	}
	terminal := status != "queued" && status != "running"
	failed, pending := 0, 0
	if terminal && status != "text_review" {
		failed = count - succeeded
	}
	if !terminal {
		pending = count - succeeded
	}
	stage, label := status, imageTaskStages[status]
	if terminal {
		label = map[string]string{"success": "成功", "partial_success": "部分成功", "failed": "失败", "text_review": "文本"}[status]
	} else if progress := strings.ToLower(text(raw["progress"])); progress != "" {
		stage = progress
		if v := imageTaskStages[progress]; v != "" {
			label = v
		}
	}
	duration := imageTaskNullableInteger(raw["duration_ms"])
	elapsed := duration
	if !terminal {
		elapsed = nil
		base := raw["started_ts"]
		if status == "queued" {
			base = raw["created_ts"]
		}
		_, isString := base.(string)
		if _, isBool := base.(bool); !isBool && !isString {
			if f, ok := number(base); ok && f > 0 {
				elapsed = max(int64(0), int64((float64(now.UnixNano())/1e9-f)*1000))
			}
		}
	}
	mode := "generate"
	if strings.ToLower(text(raw["mode"])) == "edit" {
		mode = "edit"
	}
	model := text(raw["model"])
	if model == "" {
		model = "gpt-image-2"
	}
	quality := text(raw["quality"])
	if quality == "" {
		quality = "auto"
	}
	publicError := text(raw["public_error"])
	if publicError == "" {
		publicError = text(raw["error"])
	}
	return Map{"id": text(raw["id"]), "status": status, "terminal": terminal, "mode": mode, "model": model, "size": text(raw["size"]), "quality": quality, "stage_code": stage, "stage_label": label, "created_at": text(raw["created_at"]), "updated_at": text(raw["updated_at"]), "requested_count": count, "succeeded_count": succeeded, "failed_count": failed, "pending_count": pending, "duration_ms": duration, "elapsed_ms": elapsed, "error_code": text(raw["error_code"]), "public_error": publicError, "results": results, "actions": Map{"resume_poll": status == "failed" && raw["can_resume_poll"] == true}}
}
