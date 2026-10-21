package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/filepersist"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The release build binds the artifact to its publishing repository. Source
// builds retain the upstream default; no HTTP request controls this identity.
var goReleaseRepository = "yukkcat/chatgpt2api"
var goReleaseAPI = "https://api.github.com/repos/" + goReleaseRepository + "/releases/latest"
var goReleasePage = "https://github.com/" + goReleaseRepository + "/releases"

var goBundlePaths = []string{"bin/control-server", "bin/upstream-probe", "VERSION", "web_dist"}
var goVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z.-]+)?$`)

type GoUpdater struct {
	s           *Server
	root        string
	managed     bool
	restart     func() error
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	task, cache Map
	cached      time.Time
	flight      chan struct{}
	workers     sync.WaitGroup
	closing     bool
	persistErr  error
}

func goTag(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 0 && strings.EqualFold(raw[:1], "v") {
		raw = raw[1:]
	}
	if raw == "" {
		return "unknown"
	}
	return "v" + raw
}
func compareGoVersions(a, b string) (int, bool) {
	left, right := goVersionPattern.FindStringSubmatch(strings.TrimSpace(a)), goVersionPattern.FindStringSubmatch(strings.TrimSpace(b))
	if left == nil || right == nil {
		return 0, false
	}
	for n := 1; n <= 3; n++ {
		l, _ := new(big.Int).SetString(left[n], 10)
		r, _ := new(big.Int).SetString(right[n], 10)
		if c := l.Cmp(r); c != 0 {
			return c, true
		}
	}
	l, r := strings.Split(left[4], "."), strings.Split(right[4], ".")
	if left[4] == "" || right[4] == "" {
		if left[4] == right[4] {
			return 0, true
		}
		if left[4] == "" {
			return 1, true
		}
		return -1, true
	}
	for n := 0; n < max(len(l), len(r)); n++ {
		if n >= len(l) {
			return -1, true
		}
		if n >= len(r) {
			return 1, true
		}
		ln, lok := new(big.Int).SetString(l[n], 10)
		rn, rok := new(big.Int).SetString(r[n], 10)
		if lok && rok {
			if c := ln.Cmp(rn); c != 0 {
				return c, true
			}
		} else if lok != rok {
			if lok {
				return -1, true
			}
			return 1, true
		} else if c := strings.Compare(l[n], r[n]); c != 0 {
			return c, true
		}
	}
	return 0, true
}
func goArchiveName() string {
	return "chatgpt2api-go-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
}
func NewGoUpdater(s *Server, root string, managed bool, restart func() error) (*GoUpdater, error) {
	if root != "" && !filepath.IsAbs(root) {
		return nil, errors.New("explicit absolute Go runtime directory required")
	}
	ctx, cancel := context.WithCancel(s.executionContext)
	u := &GoUpdater{s: s, root: filepath.Clean(root), managed: managed, restart: restart, ctx: ctx, cancel: cancel}
	if root == "" {
		u.root = ""
	}
	if managed && !u.managedReady() {
		cancel()
		return nil, errors.New("managed Go updates require an explicit marked runtime, current release and restart owner")
	}
	raw, err := loadFileMap(filepath.Join(s.DataDir, "update_task.json"))
	if err != nil {
		cancel()
		return nil, err
	}
	var reconcileErr error
	if validGoUpdateTask(raw) {
		u.task = raw
		if importStatus := text(raw["state"]); importStatus == "queued" || importStatus == "running" {
			if raw["stage"] == "restarting" && raw["latest_tag"] == goTag(s.version) {
				reconcileErr = u.recordLocked("succeeded", "completed", 6, "更新完成", "服务已更新到 "+goTag(s.version)+"。", "success", "")
			} else {
				message := "系统更新被服务重启中断，请重新开始。"
				if raw["stage"] == "restarting" {
					message = "服务重启后版本未生效，请查看服务日志并重新升级。"
				}
				reconcileErr = u.recordLocked("failed", "failed", integer(raw["current"], 0), "更新失败", message, "danger", message)
			}
		}
	}
	if reconcileErr != nil {
		cancel()
		return nil, reconcileErr
	}
	if u.task == nil {
		u.task = Map{"task_id": "", "state": "idle", "stage": "idle", "current": 0, "total": 6, "status_label": "未开始", "message": "当前没有系统更新任务。", "tone": "info", "busy": false, "current_tag": goTag(s.version), "latest_tag": "", "error": "", "updated_at": s.now().UTC().Format(time.RFC3339), "events": []Map{}}
	}
	return u, nil
}
func (u *GoUpdater) managedReady() bool {
	if !u.managed || u.root == "" || u.restart == nil {
		return false
	}
	marker, err := os.Lstat(filepath.Join(u.root, ".chatgpt2api-go-runtime"))
	if err != nil || !marker.Mode().IsRegular() {
		return false
	}
	target, err := os.Readlink(filepath.Join(u.root, "current"))
	if err != nil || !strings.HasPrefix(filepath.ToSlash(target), "releases/") || !filepath.IsLocal(target) || hasDotPath(target) {
		return false
	}
	directory, err := os.Lstat(filepath.Join(u.root, "releases"))
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(u.root, target))
	if err != nil {
		return false
	}
	base, err := filepath.EvalSymlinks(filepath.Join(u.root, "releases"))
	if err != nil {
		return false
	}
	if rel, err := filepath.Rel(base, resolved); err != nil || !filepath.IsLocal(rel) {
		return false
	}
	for _, name := range []string{"bin/control-server", "VERSION"} {
		info, err := os.Stat(filepath.Join(u.root, "current", name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}
func (u *GoUpdater) Close(ctx context.Context) error {
	u.mu.Lock()
	u.closing = true
	u.cancel()
	u.mu.Unlock()
	done := make(chan struct{})
	go func() { u.workers.Wait(); close(done) }()
	select {
	case <-done:
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.persistErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (u *GoUpdater) View() Map { u.mu.Lock(); defer u.mu.Unlock(); return copyMap(u.task) }
func (u *GoUpdater) recordLocked(state, stage string, current int, label, message, tone, errorText string) error {
	events := dicts(u.task["events"])
	events = append(events, Map{"id": text(u.task["task_id"]) + ":" + fmt.Sprint(len(events)+1), "timestamp": u.s.now().UTC().Format(time.RFC3339), "label": label, "message": message, "tone": tone})
	if len(events) > 64 {
		events = events[len(events)-64:]
	}
	next := merge(u.task, Map{"state": state, "stage": stage, "current": current, "total": 6, "status_label": label, "message": message, "tone": tone, "busy": state == "queued" || state == "running", "error": errorText, "updated_at": u.s.now().UTC().Format(time.RFC3339), "events": events, "current_tag": goTag(u.s.version)})
	u.persistErr = filepersist.WriteJSON(filepath.Join(u.s.DataDir, "update_task.json"), next)
	if u.persistErr != nil {
		return u.persistErr
	}
	u.task = next
	return nil
}
func (u *GoUpdater) progress(stage string, current int, label, message string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.recordLocked("running", stage, current, label, message, "info", "")
}
func (u *GoUpdater) publicClient() upstream.AssetHTTPClient {
	if u.s.ManagementHTTP != nil {
		client := *u.s.ManagementHTTP
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &client
	}
	return upstream.PublicAssetHTTPClient()
}
func updateURL(raw string, metadata bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.Port() != "" {
		return nil, errors.New("更新包下载地址无效。")
	}
	host := strings.ToLower(parsed.Hostname())
	allowed := host == "github.com" || host == "objects.githubusercontent.com" || host == "release-assets.githubusercontent.com"
	if metadata {
		allowed = host == "api.github.com"
	}
	if !allowed {
		return nil, errors.New("更新包下载地址无效。")
	}
	return parsed, nil
}
func (u *GoUpdater) fetch(ctx context.Context, raw string, maxBytes int64, metadata bool, destination io.Writer) error {
	current := raw
	for hop := 0; hop < 4; hop++ {
		parsed, err := updateURL(current, metadata)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, "GET", parsed.String(), nil)
		if err != nil {
			return errors.New("更新请求无效。")
		}
		request.Header.Set("User-Agent", "chatgpt2api-go-updater")
		request.Header.Set("Accept", "application/vnd.github+json")
		if strings.Contains(parsed.Path, "/contents/CHANGELOG.md") {
			request.Header.Set("Accept", "application/vnd.github.raw+json")
		}
		response, err := u.publicClient().Do(request)
		if err != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			return errors.New("更新包下载失败。")
		}
		if response == nil || response.Body == nil {
			return errors.New("更新包响应无效。")
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := response.Header.Get("Location")
			response.Body.Close()
			if metadata || location == "" {
				return errors.New("更新包下载重定向无效。")
			}
			next, err := parsed.Parse(location)
			if err != nil {
				return errors.New("更新包下载重定向无效。")
			}
			current = next.String()
			continue
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return errors.New("更新包下载失败。")
		}
		if response.ContentLength > maxBytes {
			response.Body.Close()
			return errors.New("更新包超过允许大小。")
		}
		n, err := io.Copy(destination, io.LimitReader(response.Body, maxBytes+1))
		response.Body.Close()
		if err != nil {
			return errors.New("更新包下载失败。")
		}
		if n > maxBytes {
			return errors.New("更新包超过允许大小。")
		}
		return nil
	}
	return errors.New("更新包下载重定向次数过多。")
}
func (u *GoUpdater) release(ctx context.Context) (Map, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var body bytes.Buffer
	if err := u.fetch(ctx, goReleaseAPI, 256<<10, true, &body); err != nil {
		return nil, err
	}
	var release Map
	if json.Unmarshal(body.Bytes(), &release) != nil || release == nil {
		return nil, errors.New("远程版本信息无效。")
	}
	return release, nil
}
func goReleaseAssets(release Map) (string, string, error) {
	archive, checksum := "", ""
	for _, asset := range dicts(release["assets"]) {
		switch text(asset["name"]) {
		case goArchiveName():
			archive = text(asset["browser_download_url"])
		case "checksums.txt":
			checksum = text(asset["browser_download_url"])
		}
	}
	if archive == "" || checksum == "" {
		return "", "", errors.New("该版本没有当前平台的 Go 更新包，请打开发布页手动更新；Python 更新包不能用于 Go 服务。")
	}
	if _, err := updateURL(archive, false); err != nil {
		return "", "", err
	}
	if _, err := updateURL(checksum, false); err != nil {
		return "", "", err
	}
	return archive, checksum, nil
}
func (u *GoUpdater) Status(ctx context.Context, force bool) Map {
	for {
		u.mu.Lock()
		if !force && u.cache != nil && u.s.now().Sub(u.cached) < 300*time.Second {
			out := copyMap(u.cache)
			u.mu.Unlock()
			return out
		}
		if flight := u.flight; flight != nil {
			u.mu.Unlock()
			select {
			case <-flight:
				force = false
				continue
			case <-ctx.Done():
				return u.failedStatus()
			}
		}
		u.flight = make(chan struct{})
		u.mu.Unlock()
		break
	}
	result := u.failedStatus()
	callCtx, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	type releaseResult struct {
		release Map
		err     error
	}
	releases := make(chan releaseResult, 1)
	changelogs := make(chan string, 1)
	go func() { release, err := u.release(callCtx); releases <- releaseResult{release, err} }()
	go func() {
		var content bytes.Buffer
		if u.fetch(callCtx, "https://api.github.com/repos/"+goReleaseRepository+"/contents/CHANGELOG.md?ref=main", 256<<10, true, &content) != nil {
			changelogs <- ""
			return
		}
		changelogs <- strings.TrimSpace(content.String())
	}()
	loaded := <-releases
	release, err := loaded.release, loaded.err
	changelog := <-changelogs
	if err == nil {
		latest, current := goTag(text(release["tag_name"])), goTag(u.s.version)
		comparison, ok := compareGoVersions(latest, current)
		if ok {
			if changelog == "" {
				changelog = goReleaseChangelog(release, latest)
			}
			result = Map{"current_tag": current, "latest_tag": latest, "update_available": comparison > 0, "release_url": goReleasePage + "/tag/" + url.QueryEscape(latest), "status_label": "已是最新", "status_message": "当前版本 " + current + " 无需更新。", "tone": "muted", "changelog": changelog, "can_update": false}
			if comparison > 0 {
				_, _, assetErr := goReleaseAssets(release)
				can := u.managedReady() && assetErr == nil
				result["can_update"], result["tone"], result["status_label"] = can, "warning", "发现新版本"
				message := "发现新版本 " + latest + "，当前未启用受管 Go 在线更新，请使用部署工具升级。"
				if assetErr != nil {
					message = "发现新版本 " + latest + "，该版本缺少当前平台的 Go 更新包，不能安装 Python 更新包。"
				}
				if can {
					result["tone"], result["status_label"] = "success", "可更新"
					message = "发现新版本 " + latest + "，可以直接更新。"
				}
				result["status_message"] = message
			}
		}
	}
	u.mu.Lock()
	u.cache = copyMap(result)
	u.cached = u.s.now()
	close(u.flight)
	u.flight = nil
	u.mu.Unlock()
	return result
}
func (u *GoUpdater) failedStatus() Map {
	return Map{"current_tag": goTag(u.s.version), "latest_tag": "", "update_available": false, "release_url": goReleasePage, "status_label": "检查失败", "status_message": "暂时无法检查最新版本，请稍后重试。", "tone": "warning", "changelog": "", "can_update": false}
}
func (u *GoUpdater) Start() (Map, error) {
	if !u.managedReady() {
		return nil, problem(409, "当前部署不支持持久化 Go 在线更新，请使用部署工具升级。")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closing {
		return nil, problem(409, "系统正在关闭，不能开始更新。")
	}
	if truth(u.task["busy"]) {
		return copyMap(u.task), nil
	}
	previous := u.task
	u.task = Map{"task_id": id(16), "latest_tag": "", "events": []Map{}}
	if err := u.recordLocked("queued", "queued", 0, "等待更新", "系统更新任务已进入队列。", "info", ""); err != nil {
		u.task = previous
		return nil, err
	}
	out := copyMap(u.task)
	u.workers.Add(1)
	go func() { defer u.workers.Done(); u.perform() }()
	return out, nil
}
func (u *GoUpdater) perform() {
	var err error
	defer func() {
		if recover() != nil {
			err = errors.New("在线更新失败，请查看服务日志后重试。")
		}
		if err != nil {
			u.mu.Lock()
			defer u.mu.Unlock()
			_ = u.recordLocked("failed", "failed", integer(u.task["current"], 0), "更新失败", err.Error(), "danger", err.Error())
		}
	}()
	ctx, cancel := context.WithTimeout(u.ctx, 15*time.Minute)
	defer cancel()
	if err = u.progress("checking", 1, "确认目标版本", "正在读取最新发布版本与 Go 更新资产。"); err != nil {
		return
	}
	release, fetchErr := u.release(ctx)
	if fetchErr != nil {
		err = fetchErr
		return
	}
	latest := goTag(text(release["tag_name"]))
	comparison, ok := compareGoVersions(latest, goTag(u.s.version))
	if !ok {
		err = errors.New("远程版本号无效。")
		return
	}
	u.mu.Lock()
	u.task["latest_tag"] = latest
	u.mu.Unlock()
	if comparison <= 0 {
		u.mu.Lock()
		err = u.recordLocked("succeeded", "completed", 6, "无需更新", "当前已经是最新版本。", "success", "")
		u.mu.Unlock()
		return
	}
	rollback, installErr := u.install(ctx, release, latest)
	if installErr != nil {
		err = installErr
		return
	}
	if err = u.progress("restarting", 5, "重启服务", latest+" 已安装，正在重启服务使更新生效。"); err != nil {
		_ = rollback()
		return
	}
	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		_ = rollback()
		err = errors.New("系统更新已取消，已恢复原版本。")
		return
	}
	if err = u.restart(); err != nil {
		_ = rollback()
		err = errors.New("无法重启 Go 服务，已恢复原版本。")
	}
}
func (u *GoUpdater) install(ctx context.Context, release Map, tag string) (func() error, error) {
	archiveURL, checksumURL, err := goReleaseAssets(release)
	if err != nil {
		return nil, err
	}
	unlock, err := filepersist.TryLock(filepath.Join(u.root, ".update.install.lock"))
	if err != nil {
		return nil, errors.New("无法锁定 Go 运行目录。")
	}
	if unlock == nil {
		return nil, errors.New("另一个系统更新正在运行。")
	}
	defer unlock()
	temp, err := os.MkdirTemp(u.root, ".chatgpt2api-go-update-")
	if err != nil {
		return nil, errors.New("无法创建更新临时目录。")
	}
	defer os.RemoveAll(temp)
	if err = u.progress("downloading", 2, "下载更新包", "正在下载 Go 发布包与校验文件。"); err != nil {
		return nil, err
	}
	archive, err := os.OpenFile(filepath.Join(temp, "bundle.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	err = u.fetch(ctx, archiveURL, 256<<20, false, io.MultiWriter(archive, hash))
	closeErr := archive.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, errors.New("保存更新包失败。")
	}
	var checksums bytes.Buffer
	if err = u.fetch(ctx, checksumURL, 1<<20, false, &checksums); err != nil {
		return nil, err
	}
	expected := ""
	for _, line := range strings.Split(checksums.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == goArchiveName() {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if len(expected) != 64 || expected != hex.EncodeToString(hash.Sum(nil)) {
		return nil, errors.New("更新包完整性校验失败。")
	}
	if err = u.progress("verifying", 3, "校验更新包", "正在校验 Go 平台、可执行文件和更新清单。"); err != nil {
		return nil, err
	}
	bundle, err := extractGoBundle(ctx, filepath.Join(temp, "bundle.tar.gz"), filepath.Join(temp, "extracted"), tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if err = u.progress("installing", 4, "安装运行文件", "正在原子切换受管 Go 运行目录。"); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.New("系统更新已取消。")
	}
	old, err := os.Readlink(filepath.Join(u.root, "current"))
	if err != nil || !filepath.IsLocal(old) || !strings.HasPrefix(filepath.ToSlash(old), "releases/") {
		return nil, errors.New("Go 当前版本指针无效。")
	}
	releaseName := strings.TrimPrefix(tag, "v") + "-" + id(6)
	relative := filepath.Join("releases", releaseName)
	if err = os.MkdirAll(filepath.Join(u.root, "releases"), 0700); err != nil {
		return nil, err
	}
	if err = os.Rename(bundle, filepath.Join(u.root, relative)); err != nil {
		return nil, errors.New("安装 Go 更新文件失败。")
	}
	if err = replaceGoLink(u.root, "previous", old); err != nil {
		return nil, err
	}
	if err = replaceGoLink(u.root, "current", relative); err != nil {
		return nil, err
	}
	return func() error { return replaceGoLink(u.root, "current", old) }, nil
}
func replaceGoLink(root, name, target string) error {
	temp := filepath.Join(root, "."+name+"-"+id(6))
	if err := os.Symlink(target, temp); err != nil {
		return errors.New("创建 Go 版本指针失败。")
	}
	defer os.Remove(temp)
	if err := os.Rename(temp, filepath.Join(root, name)); err != nil {
		return errors.New("切换 Go 版本指针失败。")
	}
	if directory, err := os.Open(root); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
func extractGoBundle(ctx context.Context, archivePath, destination, tag, goos, goarch string) (string, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	reader, err := gzip.NewReader(archive)
	if err != nil {
		return "", errors.New("Go 更新包格式无效。")
	}
	defer reader.Close()
	if err = os.MkdirAll(destination, 0700); err != nil {
		return "", err
	}
	tarReader := tar.NewReader(reader)
	count, total := 0, int64(0)
	for {
		if ctx.Err() != nil {
			return "", errors.New("系统更新已取消。")
		}
		entry, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("Go 更新包不完整。")
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if !filepath.IsLocal(name) || strings.ContainsAny(name, "\\\x00") || hasDotPath(name) || !(name == "chatgpt2api-go" || strings.HasPrefix(name, "chatgpt2api-go/")) {
			return "", errors.New("更新包包含不安全路径。")
		}
		relative := strings.TrimPrefix(name, "chatgpt2api-go/")
		allowed := name == "chatgpt2api-go" || relative == "update-manifest.json" || relative == "bin"
		for _, p := range goBundlePaths {
			if relative == p || strings.HasPrefix(relative, p+"/") {
				allowed = true
			}
		}
		if !allowed {
			return "", errors.New("更新包包含非 Go 运行文件，拒绝 Python 更新包。")
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return "", err
			}
			continue
		}
		if entry.Typeflag != tar.TypeReg && entry.Typeflag != tar.TypeRegA {
			return "", errors.New("更新包包含不支持的文件类型。")
		}
		if (relative == "update-manifest.json" && entry.Size > 1<<20) || (relative == "VERSION" && entry.Size > 256) {
			return "", errors.New("Go 更新包清单或版本文件过大。")
		}
		count++
		total += max(0, entry.Size)
		if count > 10000 || total > 512<<20 || entry.Size < 0 {
			return "", errors.New("更新包解压内容超过允许大小。")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", errors.New("更新包包含重复或无效文件。")
		}
		_, copyErr := io.CopyN(file, tarReader, entry.Size)
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			return "", errors.New("更新包内容写入失败。")
		}
		if strings.HasPrefix(relative, "bin/") {
			if err = os.Chmod(target, 0755); err != nil {
				return "", err
			}
		}
	}
	remaining := int64(512<<20) - total
	if n, err := io.Copy(io.Discard, io.LimitReader(reader, remaining+1)); err != nil || n > remaining {
		return "", errors.New("Go 更新包压缩校验失败。")
	}
	bundle := filepath.Join(destination, "chatgpt2api-go")
	manifestRaw, err := os.ReadFile(filepath.Join(bundle, "update-manifest.json"))
	if err != nil || len(manifestRaw) > 1<<20 {
		return "", errors.New("Go 更新包清单无效。")
	}
	var manifest Map
	if json.Unmarshal(manifestRaw, &manifest) != nil || manifest["runtime"] != "go" || integer(manifest["format"], 0) != 1 || goTag(text(manifest["version"])) != tag || manifest["goos"] != goos || manifest["goarch"] != goarch || canonical(manifest["paths"]) != canonical(goBundlePaths) {
		return "", errors.New("Go 更新包清单与目标版本或平台不匹配。")
	}
	version, err := os.ReadFile(filepath.Join(bundle, "VERSION"))
	if err != nil || goTag(string(version)) != tag {
		return "", errors.New("Go 更新包版本无效。")
	}
	for _, name := range []string{"bin/control-server", "bin/upstream-probe"} {
		info, err := buildinfo.ReadFile(filepath.Join(bundle, name))
		if err != nil || info.Main.Path != "github.com/yukkcat/chatgpt2api/go-backend" {
			return "", errors.New("Go 更新包缺少有效的原生可执行文件。")
		}
		settings := map[string]string{}
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		if settings["GOOS"] != goos || settings["GOARCH"] != goarch {
			return "", errors.New("Go 更新包可执行文件平台不匹配。")
		}
	}
	if info, err := os.Stat(filepath.Join(bundle, "web_dist", "index.html")); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Go 更新包缺少 Vue 前端。")
	}
	return bundle, nil
}
func (s *Server) registerGoUpdates() {
	s.route("GET", "/api/system/update-status", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) {
		input := ""
		if values := r.URL.Query()["force"]; len(values) > 0 {
			input = values[len(values)-1]
		}
		raw := strings.ToLower(input)
		force := false
		switch raw {
		case "", "0", "false", "f", "no", "n", "off":
		case "1", "true", "t", "yes", "y", "on":
			force = true
		default:
			return nil, &ValidationError{[]Map{validationIssue("bool_parsing", "Input should be a valid boolean, unable to interpret input", []any{"query", "force"}, input, nil)}}
		}
		return s.Updater.Status(ctx, force), nil
	})
	s.route("GET", "/api/system/update-task", "admin", "", func(ctx context.Context, r *http.Request, b, who Map) (any, error) { return s.Updater.View(), nil })
	s.routes = append(s.routes, registeredRoute{Route{"POST", "/api/system/update", "admin", ""}, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
		defer cancel()
		who, err := s.Auth.Identity(ctx, r.Header.Get("Authorization"))
		if err == nil && who["role"] != "admin" {
			err = problem(403, "需要管理员权限才能执行这个操作")
		}
		if err != nil {
			writeError(w, err)
			return
		}
		task, err := s.Updater.Start()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 202, task)
	}})
}

func hasDotPath(value string) bool {
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}
func goReleaseChangelog(release Map, tag string) string {
	body := text(release["body"])
	if body == "" {
		return ""
	}
	items, fallback := []string{}, []string{}
	hasItem := false
	pattern := regexp.MustCompile(`^[+*-]\s+\[[^]\n]+\]\s+\S`)
	bullet := regexp.MustCompile(`^[+*-]\s+`)
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			continue
		}
		fallback = append(fallback, line)
		if pattern.MatchString(line) {
			items = append(items, line)
			hasItem = true
		} else if hasItem && !bullet.MatchString(line) {
			items = append(items, line)
		}
	}
	if !hasItem {
		summary := strings.Join(fallback, " ")
		if summary == "" {
			return ""
		}
		items = []string{"- [更新] " + summary}
	}
	heading := "## " + strings.TrimPrefix(tag, "v")
	published := text(release["published_at"])
	if len(published) >= 10 {
		if _, err := time.Parse("2006-01-02", published[:10]); err == nil {
			heading += " - " + published[:10]
		}
	}
	return heading + "\n\n" + strings.Join(items, "\n")
}

func validGoUpdateTask(raw Map) bool {
	if len(raw) == 0 {
		return false
	}
	state, stage := text(raw["state"]), text(raw["stage"])
	validState := map[string]bool{"idle": true, "queued": true, "running": true, "succeeded": true, "failed": true}
	validStage := map[string]bool{"idle": true, "queued": true, "checking": true, "downloading": true, "verifying": true, "installing": true, "syncing": true, "restarting": true, "completed": true, "failed": true}
	if !validState[state] || !validStage[stage] || text(raw["current_tag"]) == "" || text(raw["status_label"]) == "" || text(raw["message"]) == "" || text(raw["updated_at"]) == "" {
		return false
	}
	if (state == "idle") != (text(raw["task_id"]) == "") {
		return false
	}
	busy, ok := raw["busy"].(bool)
	if !ok || busy != (state == "queued" || state == "running") {
		return false
	}
	current, total := integer(raw["current"], -1), integer(raw["total"], 0)
	if current < 0 || total < 1 || current > total {
		return false
	}
	if state == "failed" && text(raw["error"]) == "" {
		return false
	}
	allowed := map[string]bool{"task_id": true, "state": true, "stage": true, "current": true, "total": true, "status_label": true, "message": true, "tone": true, "busy": true, "current_tag": true, "latest_tag": true, "error": true, "updated_at": true, "events": true}
	for key := range raw {
		if !allowed[key] {
			return false
		}
	}
	return true
}
