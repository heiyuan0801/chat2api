package upstream

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	stdhttp "net/http"
	"path"
	"strings"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

type ImageInput struct {
	Data       []byte
	MIME, Name string
}
type ImageRequest struct {
	Prompt, Model, Size, Quality string
	Images                       []ImageInput
}

func validateImageInputs(images []ImageInput) error {
	if len(images) > 16 {
		return failure("too_many_images", "validation")
	}
	total := 0
	for _, in := range images {
		total += len(in.Data)
		if len(in.Data) == 0 || len(in.Data) > 50<<20 || total > 100<<20 {
			return failure("invalid_image_input", "validation")
		}
		if config, _, e := image.DecodeConfig(bytes.NewReader(in.Data)); e != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 100_000_000 {
			return failure("invalid_image_input", "validation")
		}
	}
	return nil
}
func reportProgress(progress func(string), stage string) {
	if progress != nil {
		progress(stage)
	}
}
func imageMessageMetadata(metadata map[string]any) map[string]any {
	metadata["developer_mode_connector_ids"] = []any{}
	metadata["selected_github_repos"] = []any{}
	metadata["selected_all_github_repos"] = false
	metadata["system_hints"] = []string{"picture_v2"}
	metadata["serialization_metadata"] = map[string]any{"custom_symbol_offsets": []any{}}
	return metadata
}
func (c *Client) messageContent(ctx context.Context, prompt string, images []ImageInput) (map[string]any, map[string]any, error) {
	if len(images) == 0 {
		return map[string]any{"content_type": "text", "parts": []string{prompt}}, map[string]any{}, nil
	}
	parts := make([]any, 0, len(images)+1)
	attachments := make([]any, 0, len(images))
	for i, in := range images {
		ref, e := c.uploadImage(ctx, in, i+1)
		if e != nil {
			return nil, nil, e
		}
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + ref.ID, "width": ref.Width, "height": ref.Height, "size_bytes": ref.Size})
		attachments = append(attachments, map[string]any{"id": ref.ID, "mimeType": ref.MIME, "name": ref.Name, "size": ref.Size, "width": ref.Width, "height": ref.Height})
	}
	if prompt != "" {
		parts = append(parts, prompt)
	}
	return map[string]any{"content_type": "multimodal_text", "parts": parts}, map[string]any{"attachments": attachments}, nil
}

type uploadReference struct {
	ID, Name, MIME      string
	Width, Height, Size int
}

func (c *Client) uploadImage(ctx context.Context, in ImageInput, index int) (uploadReference, error) {
	config, format, e := image.DecodeConfig(bytes.NewReader(in.Data))
	if e != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 100_000_000 {
		return uploadReference{}, failure("invalid_image_input", "upload")
	}
	mime := "image/" + format
	if format == "jpg" {
		mime = "image/jpeg"
	}
	name := path.Base(strings.ReplaceAll(in.Name, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = "image.png"
	}
	if len(name) > 255 || strings.ContainsAny(name, "\r\n\x00") {
		return uploadReference{}, failure("invalid_image_input", "upload")
	}
	apiPath := "/backend-api/files"
	meta, e := c.json(ctx, "POST", apiPath, "image_upload_prepare", map[string]any{"file_name": name, "file_size": len(in.Data), "use_case": "multimodal", "width": config.Width, "height": config.Height}, c.headers(apiPath, true))
	if e != nil {
		return uploadReference{}, e
	}
	id := text(meta["file_id"])
	if !idPattern.MatchString(id) || len(id) > 128 {
		return uploadReference{}, failure("invalid_file_id", "image_upload_prepare")
	}
	uploadURL := text(meta["upload_url"])
	u, e := validateAssetURL(uploadURL, false)
	if e != nil {
		return uploadReference{}, e
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	release, e := acquireAssetSlot(uploadCtx)
	if e != nil {
		return uploadReference{}, e
	}
	defer release()
	req, e := stdhttp.NewRequestWithContext(uploadCtx, "PUT", u.String(), bytes.NewReader(in.Data))
	if e != nil {
		return uploadReference{}, failure("invalid_asset_url", "image_upload")
	}
	req.Header = stdhttp.Header{"Content-Type": {mime}, "x-ms-blob-type": {"BlockBlob"}, "x-ms-version": {"2020-04-08"}, "Origin": {origin}, "Referer": {origin + "/"}, "User-Agent": {c.cfg.UserAgent}, "Accept": {"application/json, text/plain, */*"}, "Accept-Language": {"en-US,en;q=0.8"}}
	resp, e := c.assets.Do(req)
	if e != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return uploadReference{}, assetRequestError(uploadCtx, "image_upload", e)
	}
	if resp == nil || resp.Body == nil {
		return uploadReference{}, failure("invalid_response", "image_upload")
	}
	// The response may contain a signed URL; consume only a small bounded body
	// and expose controlled HTTP diagnostics. Never follow a PUT redirect.
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if readErr != nil {
		return uploadReference{}, requestError(uploadCtx, "image_upload", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return uploadReference{}, &Error{Kind: "image_upload_failed", Stage: "image_upload", Status: resp.StatusCode}
	}
	apiPath = "/backend-api/files/" + id + "/uploaded"
	if _, e = c.json(ctx, "POST", apiPath, "image_upload_finish", map[string]any{}, c.headers(apiPath, true)); e != nil {
		return uploadReference{}, e
	}
	return uploadReference{ID: id, Name: name, MIME: mime, Width: config.Width, Height: config.Height, Size: len(in.Data)}, nil
}
func assetRequestError(ctx context.Context, stage string, e error) error {
	if controlled, ok := e.(*Error); ok {
		return controlled
	}
	return requestError(ctx, stage, e)
}
