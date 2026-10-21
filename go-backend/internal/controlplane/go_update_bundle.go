package controlplane

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// BuildGoReleaseBundle builds the two native commands and the unchanged Vue
// distribution into the same versioned format consumed by the managed updater.
// It only writes the explicitly selected output directory, never publishes.
func BuildGoReleaseBundle(ctx context.Context, moduleDir, webDir, outputDir, version, goos, goarch string) (string, error) {
	releaseRepository := strings.TrimSpace(os.Getenv("CHATGPT2API_RELEASE_REPOSITORY"))
	if releaseRepository == "" {
		releaseRepository = goReleaseRepository
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(releaseRepository) {
		return "", errors.New("valid owner/repository release identity required")
	}
	tag := goTag(version)
	if !goVersionPattern.MatchString(tag) || (goos != "linux" && goos != "darwin") || (goarch != "amd64" && goarch != "arm64") {
		return "", errors.New("valid version and supported Go target are required")
	}
	for _, dir := range []string{moduleDir, webDir, outputDir} {
		if !filepath.IsAbs(dir) {
			return "", errors.New("absolute module, Vue and output directories are required")
		}
	}
	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		return "", errors.New("built Vue distribution with index.html is required")
	}
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(outputDir, ".go-bundle-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	source := filepath.Join(temp, "chatgpt2api-go")
	if err = os.MkdirAll(filepath.Join(source, "bin"), 0700); err != nil {
		return "", err
	}
	for _, command := range []string{"control-server", "upstream-probe"} {
		build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -X main.buildVersion="+strings.TrimPrefix(tag, "v")+" -X github.com/yukkcat/chatgpt2api/go-backend/internal/controlplane.goReleaseRepository="+releaseRepository, "-o", filepath.Join(source, "bin", command), "./cmd/"+command)
		build.Dir = moduleDir
		env := []string{}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "GOOS=") && !strings.HasPrefix(v, "GOARCH=") && !strings.HasPrefix(v, "CGO_ENABLED=") {
				env = append(env, v)
			}
		}
		build.Env = append(env, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			_ = output
			return "", errors.New("native Go command build failed")
		}
	}
	if err = os.WriteFile(filepath.Join(source, "VERSION"), []byte(strings.TrimPrefix(tag, "v")+"\n"), 0600); err != nil {
		return "", err
	}
	manifest, _ := json.Marshal(Map{"format": 1, "runtime": "go", "version": strings.TrimPrefix(tag, "v"), "goos": goos, "goarch": goarch, "paths": goBundlePaths})
	if err = os.WriteFile(filepath.Join(source, "update-manifest.json"), manifest, 0600); err != nil {
		return "", err
	}
	count, total := 0, int64(0)
	if err = filepath.WalkDir(webDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(webDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(source, "web_dist", rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Vue distribution must not contain symlinks")
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("Vue distribution contains an unsupported file")
		}
		count++
		total += info.Size()
		if count > 9990 || total > 384<<20 {
			return errors.New("Vue distribution exceeds bundle limits")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}); err != nil {
		return "", err
	}
	name := "chatgpt2api-go-" + goos + "-" + goarch + ".tar.gz"
	archivePath := filepath.Join(temp, name)
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	gzipWriter := gzip.NewWriter(io.MultiWriter(file, hash))
	tarWriter := tar.NewWriter(gzipWriter)
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(temp, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.ModTime = time.Unix(0, 0)
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Mode = 0644
		if entry.IsDir() || strings.HasPrefix(filepath.ToSlash(rel), "chatgpt2api-go/bin/") {
			header.Mode = 0755
		}
		if err = tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		_, err = io.Copy(tarWriter, input)
		return err
	})
	tarErr, gzipErr := tarWriter.Close(), gzipWriter.Close()
	syncErr, closeErr := file.Sync(), file.Close()
	if err != nil || tarErr != nil || gzipErr != nil || syncErr != nil || closeErr != nil {
		return "", errors.New("writing Go release bundle failed")
	}
	// Validate the finished bytes through the installer's exact format boundary.
	if _, err = extractGoBundle(ctx, archivePath, filepath.Join(temp, "verify"), tag, goos, goarch); err != nil {
		return "", err
	}
	destination := filepath.Join(outputDir, name)
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		return "", errors.New("release archive already exists")
	}
	if err = os.Rename(archivePath, destination); err != nil {
		return "", err
	}
	checksumPath := filepath.Join(outputDir, "checksums.txt")
	previous, err := os.ReadFile(checksumPath)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	content := string(previous)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += hex.EncodeToString(hash.Sum(nil)) + "  " + name + "\n"
	temporary := filepath.Join(temp, "checksums.txt")
	if err = os.WriteFile(temporary, []byte(content), 0600); err != nil {
		return "", err
	}
	if err = os.Rename(temporary, checksumPath); err != nil {
		return "", err
	}
	return destination, nil
}
