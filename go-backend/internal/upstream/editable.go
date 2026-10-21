package upstream

import (
	"context"
	"errors"
	http "github.com/bogdanfinn/fhttp"
	"os"
	"strings"
	"time"
)

const EditableFileModel = "gpt-5-5-thinking"
const editablePPTPrompt = `我需要你根据用户的需求，来制作一个可以编辑的PPT，你可以使用Agent来做，你不要再继续询问用户问题，内容风格、版式、配色、内容结构和页面信息你可以自行补充并直接执行。整体的流程如下：
1. 用生图的方式，帮我生成一个精美的产品介绍ppt，5-6个页面
2. 帮我把以上涉及到的所有图像和形状素材拆分成单独png，每个素材单独一张图片，不要有遗漏，让我可以直接在ppt里拼接素材还原，不要文字
3. 利用以上所有图片和形状素材，帮我还原你第一次生成的展示ppt，我需要是可编辑的ppt格式，主要部分需要你单独还原插入，文字需要可以编辑
最后只需要给我生成一个PPT文件，以及生成中遇到的各种素材压缩包zip文件就行。`
const editablePSDPrompt = "帮我生成这个图像，把这张海报分成若干图像，包括背景图，每个元素不要改位置，这样子我可以直接在 平时里无需拖动，底色为白色，不要伪透明底。再帮我将以上拆分的图像拼合成一个psd文件，去除白色底，不要改变每个图层的相应位置，保留每个元素所在图层的相应位置，保留每个元素的图层，最后只需要给我输出psd文件，以及每个图层的zip文件"

type EditableFileRequest struct {
	Kind, Prompt string
	Images       []ImageInput
	Progress     func(string)
}
type EditableFileExport struct {
	ConversationID, PrimaryName, ArchiveName string
	AuthInvalid                              bool
}
type editableArtifact struct {
	AttachmentID, FileID, Name, MIME, SandboxPath, MessageID string
	Created                                                  float64
}
type editableTransport struct{ HTTPClient }

func (t editableTransport) Do(r *http.Request) (*http.Response, error) {
	r.Header["oai-client-version"] = []string{"prod-bede35f9dcd856d080e012478f0c1031faa2588e"}
	r.Header["oai-client-build-number"] = []string{"6631702"}
	return t.HTTPClient.Do(r)
}

// ExportEditable executes one billable conversation and writes two bounded
// artifacts into a caller-owned private staging directory. Only the task owner
// can publish that directory; polling never starts another conversation.
func (client *Client) ExportEditable(ctx context.Context, in EditableFileRequest, directory string) (EditableFileExport, error) {
	if client.cfg.AccessToken == "" {
		return EditableFileExport{}, failure("auth_invalid", "editable_validation")
	}
	if in.Kind != "ppt" && in.Kind != "psd" {
		return EditableFileExport{}, failure("invalid_request", "editable_validation")
	}
	if in.Kind == "psd" && len(in.Images) == 0 {
		return EditableFileExport{}, failure("invalid_image_input", "editable_validation")
	}
	if e := validateImageInputs(in.Images); e != nil {
		return EditableFileExport{}, e
	}
	if len(in.Prompt) > 1<<20 {
		return EditableFileExport{}, failure("input_too_large", "editable_validation")
	}
	ctx, end, e := client.begin(ctx)
	if e != nil {
		return EditableFileExport{}, e
	}
	defer end()
	local := *client
	local.http = editableTransport{client.http}
	c := &local
	prompt := editablePPTPrompt
	if in.Kind == "psd" {
		prompt = editablePSDPrompt
	}
	if extra := strings.TrimSpace(in.Prompt); extra != "" {
		prompt += "\n\n以下是用户补充需求，请直接结合执行：\n" + extra
	}
	if len(in.Images) > 0 {
		reportProgress(in.Progress, "uploading")
	}
	content, metadata, mimes, e := c.editableContent(ctx, prompt, in.Images)
	if e != nil {
		return EditableFileExport{}, e
	}
	reportProgress(in.Progress, "preparing_conversation")
	preparePath := "/backend-api/f/conversation/prepare"
	payload := map[string]any{"action": "next", "fork_from_shared_post": false, "parent_message_id": "client-created-root", "model": EditableFileModel, "client_prepare_state": "success", "timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"}, "system_hints": []any{}, "partial_query": map[string]any{"id": uuid(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}}, "supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]any{"app_name": "chatgpt.com"}, "thinking_effort": "extended"}
	if len(mimes) > 0 {
		payload["attachment_mime_types"] = mimes
	}
	h := c.headers(preparePath, true)
	h["x-conduit-token"] = []string{"no-token"}
	prepared, e := c.json(ctx, "POST", preparePath, "editable_prepare", payload, h)
	if e != nil {
		return EditableFileExport{}, e
	}
	conduit := text(prepared["conduit_token"])
	if conduit == "" {
		return EditableFileExport{}, failure("missing_conduit_token", "editable_prepare")
	}
	reportProgress(in.Progress, "bootstrapping")
	requirements, e := c.prepare(ctx)
	if e != nil {
		return EditableFileExport{}, e
	}
	message := map[string]any{"id": uuid(), "author": map[string]any{"role": "user"}, "create_time": float64(time.Now().UnixNano()) / 1e9, "content": content}
	if len(metadata) > 0 {
		message["metadata"] = metadata
	}
	payload = map[string]any{"action": "next", "messages": []any{message}, "parent_message_id": "client-created-root", "model": EditableFileModel, "client_prepare_state": "sent", "timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"}, "enable_message_followups": true, "system_hints": []any{}, "supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]any{"is_dark_mode": false, "time_since_loaded": 401, "page_height": 1138, "page_width": 803, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"}, "paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto", "thinking_effort": "extended"}
	apiPath := "/backend-api/f/conversation"
	h = c.conversationHeaders(apiPath, requirements)
	h["x-conduit-token"] = []string{conduit}
	reportProgress(in.Progress, "starting_generation")
	resp, e := c.request(ctx, "POST", apiPath, "editable_stream", payload, h)
	if e != nil {
		return EditableFileExport{}, e
	}
	reportProgress(in.Progress, "generating")
	conversation := ""
	e = ReadSSE(ctx, resp.Body, c.cfg.MaxEventBytes, c.cfg.MaxStreamBytes, func(ev Event) error {
		if ev.Data == "[DONE]" {
			return errStreamDone
		}
		var value any
		if jsonDecodeEditable(ev.Data, &value) != nil {
			return nil
		}
		if code := structuredImageFailure(value); code != "" && code != "upstream_text_reply" {
			return failure(code, "editable_stream")
		}
		if conversation == "" {
			conversation = findSearchValue(value, "conversation_id")
		}
		return nil
	})
	resp.Body.Close()
	if e != nil {
		return EditableFileExport{}, e
	}
	if !idPattern.MatchString(conversation) || len(conversation) > 128 {
		return EditableFileExport{}, failure("missing_conversation_id", "editable_stream")
	}
	reportProgress(in.Progress, "image_stream_resolve_start")
	for {
		apiPath = "/backend-api/conversation/" + conversation
		h = c.headers(apiPath, true)
		h["referer"] = []string{origin + "/c/" + conversation}
		h["x-openai-target-route"] = []string{"/backend-api/conversation/{conversation_id}"}
		doc, e := c.json(ctx, "GET", apiPath, "editable_poll", nil, h)
		if e != nil {
			var up *Error
			if errors.As(e, &up) && searchRetryStatus(up.Status) {
				if e = waitImage(ctx, c.cfg.PollInterval); e != nil {
					return EditableFileExport{}, e
				}
				continue
			}
			return EditableFileExport{}, e
		}
		artifacts := editableTargets(doc, in.Kind)
		if len(artifacts) == 2 {
			reportProgress(in.Progress, "receiving_image")
			root, e := os.OpenRoot(directory)
			if e != nil {
				return EditableFileExport{}, failure("asset_write_failed", "editable_download")
			}
			defer root.Close()
			result := EditableFileExport{ConversationID: conversation, AuthInvalid: editableConversationFailure(doc) == "auth_invalid"}
			for i, a := range artifacts {
				name, e := c.downloadEditable(ctx, root, conversation, a, in.Kind)
				if e != nil {
					return EditableFileExport{}, e
				}
				if i == 0 {
					result.PrimaryName = name
				} else {
					result.ArchiveName = name
				}
			}
			reportProgress(in.Progress, "image_download_done")
			return result, nil
		}
		if code := editableConversationFailure(doc); code != "" && code != "upstream_text_reply" {
			return EditableFileExport{}, failure(code, "editable_poll")
		}
		if e = waitImage(ctx, c.cfg.PollInterval); e != nil {
			return EditableFileExport{}, e
		}
	}
}

func editableConversationFailure(doc map[string]any) string {
	state := &conversationState{}
	var problem error
	for _, message := range currentTurnMessages(doc) {
		problem = mergeUpstreamImageError(problem, state.pollMessage(message))
	}
	if len(state.Pointers) > 0 {
		return ""
	}
	var up *Error
	if errors.As(problem, &up) {
		return up.Kind
	}
	return ""
}
