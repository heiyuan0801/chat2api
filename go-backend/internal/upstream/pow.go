package upstream

import (
	"bytes"
	"context"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"regexp"
	"time"

	"golang.org/x/net/html"
)

const defaultScript = origin + "/backend-api/sentinel/sdk.js"

var scriptBuildPattern = regexp.MustCompile(`c/[^/]*/_`)

func compactJSON(value any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value) // all callers pass JSON-compatible primitive values
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

func powResources(document []byte) ([]string, string) {
	var scripts []string
	build := ""
	z := html.NewTokenizer(bytes.NewReader(document))
	for {
		t := z.Next()
		if t == html.ErrorToken {
			break
		}
		if t != html.StartTagToken && t != html.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		for _, a := range token.Attr {
			if token.Data == "html" && a.Key == "data-build" {
				build = a.Val
			}
			if token.Data == "script" && a.Key == "src" && a.Val != "" {
				scripts = append(scripts, a.Val)
			}
		}
	}
	if len(scripts) == 0 {
		scripts = []string{defaultScript}
	}
	// Python's ScriptSrcParser gives matching script resources precedence over
	// the HTML data-build fallback; the last matching script wins.
	for _, script := range scripts {
		if candidate := scriptBuildPattern.FindString(script); candidate != "" {
			build = candidate
		}
	}
	return scripts, build
}

func powConfig(userAgent string, scripts []string, build string) []any {
	now := time.Now()
	return []any{
		3000, now.In(time.FixedZone("EST", -5*3600)).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)",
		4294705152, 1, userAgent, scripts[rand.IntN(len(scripts))], build, "en-US", "en-US,es-US,en,es", rand.Float64(),
		"webdriver−false", "location", "window", float64(now.UnixNano()%1e9) / 1e6, uuid(), "", 32, now.UnixMilli(),
		0, 0, 0, 0, 0, 0, 0,
	}
}

func solveProof(ctx context.Context, seed, difficulty string, config []any, limit int) (string, error) {
	target, err := hex.DecodeString(difficulty)
	if err != nil || len(target) == 0 || len(target) > 64 {
		return "", failure("invalid_proof_challenge", "requirements")
	}
	// Match Python's JSON array positions and SHA3-512 lexicographic comparison.
	for i := 0; i < limit; i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		config[3] = i
		config[9] = i >> 1
		encoded := base64.StdEncoding.EncodeToString(compactJSON(config))
		digest := sha3.Sum512([]byte(seed + encoded))
		if bytes.Compare(digest[:len(target)], target) <= 0 {
			return "gAAAAAB" + encoded, nil
		}
	}
	return "", failure("proof_exhausted", "requirements")
}
