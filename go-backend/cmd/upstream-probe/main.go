package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
)

func run(ctx context.Context, args []string, input io.Reader, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("upstream-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "handshake", "handshake, models, text, image or image-poll")
	conversationID := flags.String("conversation-id", "", "existing conversation to resume with image-poll")
	prompt := flags.String("prompt", "", "prompt; otherwise read from stdin")
	model := flags.String("model", "", "upstream text slug or supported Web image name")
	profile := flags.String("profile", "chrome_110", "TLS profile; defaults to the Python transport baseline")
	proxy := flags.String("proxy", "", "explicit proxy; otherwise GO_UPSTREAM_PROXY")
	timeout := flags.Duration("timeout", 300*time.Second, "deadline for the entire operation")
	stream := flags.Bool("stream", false, "emit text snapshots as NDJSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(out)
			flags.PrintDefaults()
			return 0
		}
		_ = json.NewEncoder(diagnostics).Encode(map[string]any{"error": map[string]string{"kind": "invalid_arguments"}})
		return 2
	}
	fail := func(err error) int {
		_ = json.NewEncoder(diagnostics).Encode(map[string]any{"error": upstream.SafeError(err)})
		return 1
	}
	if flags.NArg() != 0 || (*mode != "handshake" && *mode != "models" && *mode != "text" && *mode != "image" && *mode != "image-poll") {
		return fail(&upstream.Error{Kind: "invalid_mode_or_arguments", Stage: "validation"})
	}
	if *proxy == "" {
		*proxy = os.Getenv("GO_UPSTREAM_PROXY")
	}
	client, err := upstream.New(upstream.Config{
		AccessToken: os.Getenv("GO_UPSTREAM_ACCESS_TOKEN"), ProxyURL: *proxy, Profile: *profile, Timeout: *timeout,
		DeviceID: os.Getenv("GO_UPSTREAM_DEVICE_ID"), SessionID: os.Getenv("GO_UPSTREAM_SESSION_ID"), UserAgent: os.Getenv("GO_UPSTREAM_USER_AGENT"),
	})
	if err != nil {
		return fail(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if *mode == "text" || *mode == "image" {
		if *prompt == "" {
			type inputResult struct {
				data []byte
				err  error
			}
			ready := make(chan inputResult, 1)
			go func() { data, err := io.ReadAll(io.LimitReader(input, (1<<20)+1)); ready <- inputResult{data, err} }()
			var result inputResult
			select {
			case result = <-ready:
			case <-ctx.Done():
				if closer, ok := input.(io.Closer); ok {
					_ = closer.Close()
				}
				return fail(&upstream.Error{Kind: "canceled", Stage: "prompt_input"})
			}
			if result.err != nil || len(result.data) > 1<<20 {
				return fail(&upstream.Error{Kind: "invalid_prompt_input", Stage: "validation"})
			}
			*prompt = string(result.data)
		}
		if strings.TrimSpace(*prompt) == "" || len(*prompt) > 1<<20 {
			return fail(&upstream.Error{Kind: "prompt_required_or_too_large", Stage: "validation"})
		}
	}
	var result any
	switch *mode {
	case "handshake":
		result, err = client.Handshake(ctx)
	case "models":
		result, err = client.Models(ctx)
	case "text":
		var callback func(string) error
		if *stream {
			callback = func(snapshot string) error {
				return json.NewEncoder(out).Encode(map[string]string{"type": "text_snapshot", "text": snapshot})
			}
		}
		result, err = client.StreamText(ctx, []upstream.Message{{Role: "user", Content: *prompt}}, *model, callback)
	case "image":
		result, err = client.GenerateImage(ctx, *prompt, *model)
	case "image-poll":
		result, err = client.PollImage(ctx, *conversationID)
	}
	if err != nil {
		return fail(err)
	}
	if err = json.NewEncoder(out).Encode(result); err != nil {
		return fail(&upstream.Error{Kind: "output_failed", Stage: "probe"})
	}
	return 0
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}
