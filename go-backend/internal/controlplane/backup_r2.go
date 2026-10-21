package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type r2Client struct {
	settings Map
	client   *http.Client
	now      func() time.Time
}

func r2Missing(settings Map) error {
	missing := []string{}
	for _, p := range [][2]string{{"account_id", "Account ID"}, {"access_key_id", "Access Key ID"}, {"secret_access_key", "Secret Access Key"}, {"bucket", "Bucket"}} {
		if text(settings[p[0]]) == "" {
			missing = append(missing, p[1])
		}
	}
	if len(missing) > 0 {
		return problem(400, "R2 配置不完整：缺少 "+strings.Join(missing, "、"))
	}
	account := text(settings["account_id"])
	if strings.ContainsAny(account, "/:@?#\\ ") || strings.ContainsAny(text(settings["bucket"]), "/\\?#") {
		return problem(400, "R2 配置无效")
	}
	return nil
}
func shaHex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func hmac256(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	return h.Sum(nil)
}
func (r *r2Client) request(ctx context.Context, method, key string, query url.Values, file *os.File, extra map[string]string) (*http.Response, error) {
	if e := r2Missing(r.settings); e != nil {
		return nil, e
	}
	host := text(r.settings["account_id"]) + ".r2.cloudflarestorage.com"
	escaped := "/" + url.PathEscape(text(r.settings["bucket"]))
	if key != "" {
		segments := strings.Split(strings.TrimLeft(key, "/"), "/")
		for i := range segments {
			segments[i] = url.PathEscape(segments[i])
		}
		escaped += "/" + strings.Join(segments, "/")
	}
	encoded := query.Encode()
	now := r.now().UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	payloadHash := shaHex(nil)
	var body io.Reader
	size := int64(0)
	if file != nil {
		if _, e := file.Seek(0, io.SeekStart); e != nil {
			return nil, e
		}
		h := sha256.New()
		n, e := io.Copy(h, file)
		if e != nil {
			return nil, e
		}
		size = n
		payloadHash = hex.EncodeToString(h.Sum(nil))
		if _, e = file.Seek(0, io.SeekStart); e != nil {
			return nil, e
		}
		body = file
	}
	headers := map[string]string{"host": host, "x-amz-content-sha256": payloadHash, "x-amz-date": amzDate}
	for key, value := range extra {
		headers[strings.ToLower(key)] = strings.Join(strings.Fields(value), " ")
	}
	names := []string{}
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name + ":" + headers[name] + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{method, escaped, encoded, canonical.String(), signed, payloadHash}, "\n")
	scope := date + "/auto/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + shaHex([]byte(request))
	signing := hmac256(hmac256(hmac256(hmac256([]byte("AWS4"+text(r.settings["secret_access_key"])), date), "auto"), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmac256(signing, toSign))
	authorization := "AWS4-HMAC-SHA256 Credential=" + text(r.settings["access_key_id"]) + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + signature
	target := "https://" + host + escaped
	if encoded != "" {
		target += "?" + encoded
	}
	req, e := http.NewRequestWithContext(ctx, method, target, body)
	if e != nil {
		return nil, problem(400, "R2 配置无效")
	}
	req.ContentLength = size
	for name, value := range headers {
		if name != "host" {
			req.Header.Set(name, value)
		}
	}
	req.Header.Set("Authorization", authorization)
	resp, e := r.client.Do(req)
	if e != nil {
		return nil, problem(400, "R2 请求失败或超时")
	}
	return resp, nil
}
func (r *r2Client) test(ctx context.Context) (Map, error) {
	resp, e := r.request(ctx, "GET", "", url.Values{"list-type": {"2"}, "max-keys": {"1"}}, nil, nil)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, problem(400, fmt.Sprintf("连接 R2 失败：HTTP %d", resp.StatusCode))
	}
	return Map{"ok": true, "status": resp.StatusCode}, nil
}
func (r *r2Client) delete(ctx context.Context, key string) error {
	resp, e := r.request(ctx, "DELETE", key, nil, nil, nil)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && resp.StatusCode != 404 {
		return problem(400, fmt.Sprintf("删除备份失败：HTTP %d", resp.StatusCode))
	}
	return nil
}
func (r *r2Client) list(ctx context.Context) ([]Map, error) {
	out := []Map{}
	continuation := ""
	seen := map[string]bool{}
	for pages := 0; pages < 10000; pages++ {
		query := url.Values{"list-type": {"2"}, "prefix": {strings.TrimRight(text(r.settings["prefix"]), "/") + "/"}, "max-keys": {"1000"}}
		if continuation != "" {
			query.Set("continuation-token", continuation)
		}
		resp, e := r.request(ctx, "GET", "", query, nil, nil)
		if e != nil {
			return nil, e
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, problem(400, fmt.Sprintf("获取备份列表失败：HTTP %d", resp.StatusCode))
		}
		raw, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
		resp.Body.Close()
		if e != nil || len(raw) > 16<<20 {
			return nil, problem(400, "R2 返回的备份列表过大或无效")
		}
		var data struct {
			Contents []struct {
				Key          string
				Size         int64
				LastModified string
			}
			IsTruncated           bool
			NextContinuationToken string
		}
		if e = xml.Unmarshal(raw, &data); e != nil {
			return nil, problem(400, "R2 返回的备份列表无效")
		}
		for _, item := range data.Contents {
			if item.Key != "" {
				out = append(out, Map{"key": item.Key, "size": max(0, item.Size), "updated_at": item.LastModified})
			}
		}
		if !data.IsTruncated {
			sort.SliceStable(out, func(i, j int) bool { return text(out[i]["updated_at"]) > text(out[j]["updated_at"]) })
			return out, nil
		}
		continuation = data.NextContinuationToken
		if continuation == "" || seen[continuation] {
			return nil, problem(400, "R2 分页令牌无效")
		}
		seen[continuation] = true
	}
	return nil, problem(400, "R2 备份列表超过分页限制")
}
func (r *r2Client) upload(ctx context.Context, key string, file *os.File, metadata map[string]string) error {
	headers := map[string]string{"content-type": "application/octet-stream"}
	for k, v := range metadata {
		headers["x-amz-meta-"+k] = v
	}
	resp, e := r.request(ctx, "PUT", key, nil, file, headers)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return problem(400, fmt.Sprintf("上传备份失败：HTTP %d", resp.StatusCode))
	}
	return nil
}
