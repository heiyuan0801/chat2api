package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/controlplane"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func main() {
	module := flag.String("module-dir", "", "absolute Go module directory")
	web := flag.String("web-dir", "", "absolute built Vue distribution directory")
	output := flag.String("output-dir", "", "absolute output directory (local only)")
	version := flag.String("version", "", "release version")
	goos := flag.String("goos", runtime.GOOS, "target operating system")
	goarch := flag.String("goarch", runtime.GOARCH, "target architecture")
	flag.Parse()
	if *module == "" {
		*module, _ = filepath.Abs(".")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	archive, err := controlplane.BuildGoReleaseBundle(ctx, *module, *web, *output, *version, *goos, *goarch)
	if err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"archive": archive, "runtime": "go", "goos": *goos, "goarch": *goarch})
}
