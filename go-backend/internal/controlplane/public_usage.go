package controlplane

import (
	"bytes"
	"encoding/base64"
	"github.com/tiktoken-go/tokenizer"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
	"sync"
)

var publicTokenizers struct {
	sync.Mutex
	codecs map[string]tokenizer.Codec
}

func countPublicTextTokens(value, model string) int {
	encoding := publicSpec.ModelEncodings[model]
	if encoding == "" {
		longest := 0
		for prefix, name := range publicSpec.PrefixEncodings {
			if strings.HasPrefix(model, prefix) && len(prefix) > longest {
				longest, encoding = len(prefix), name
			}
		}
	}
	if encoding == "gpt2" {
		encoding = "r50k_base"
	}
	if encoding == "" {
		encoding = "o200k_base"
	}
	if encoding == "o200k_harmony" {
		encoding = "o200k_base"
	}
	publicTokenizers.Lock()
	defer publicTokenizers.Unlock()
	if publicTokenizers.codecs == nil {
		publicTokenizers.codecs = map[string]tokenizer.Codec{}
	}
	codec := publicTokenizers.codecs[encoding]
	if codec == nil {
		var e error
		codec, e = tokenizer.Get(tokenizer.Encoding(encoding))
		if e != nil {
			panic("tokenizer unavailable")
		}
		publicTokenizers.codecs[encoding] = codec
	}
	n, e := codec.Count(value)
	if e != nil {
		panic("tokenizer failed")
	}
	return n
}
func publicTokenUsage(inputText, inputImage, outputText, outputImage int) Map {
	inputText = max(0, inputText)
	inputImage = max(0, inputImage)
	outputText = max(0, outputText)
	outputImage = max(0, outputImage)
	input, output := inputText+inputImage, outputText+outputImage
	return Map{"input_tokens": input, "output_tokens": output, "total_tokens": input + output, "input_tokens_details": Map{"text_tokens": inputText, "image_tokens": inputImage, "cached_tokens": 0}, "output_tokens_details": Map{"text_tokens": outputText, "image_tokens": outputImage, "reasoning_tokens": 0}}
}
func publicChatUsage(inputText, inputImage, outputText, outputImage int) Map {
	u := publicTokenUsage(inputText, inputImage, outputText, outputImage)
	return Map{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": u["total_tokens"], "prompt_tokens_details": u["input_tokens_details"], "completion_tokens_details": u["output_tokens_details"]}
}
func publicInputImageTokens(images []upstream.ImageInput) int {
	total := 0
	for _, input := range images {
		size, _, e := image.DecodeConfig(bytes.NewReader(input.Data))
		if e != nil || size.Width <= 0 || size.Height <= 0 {
			continue
		}
		total += publicImageGeometryTokens(size.Width, size.Height, "auto")
	}
	return total
}
func countPublicMessages(messages []publicMessage, model string) (int, int) {
	total, images := 3, 0
	for _, m := range messages {
		total += 3 + countPublicTextTokens(m.Role, model) + countPublicTextTokens(m.Content, model)
		images += publicInputImageTokens(m.Images)
	}
	return total, images
}

func publicRawResponseImageTokens(input any) int {
	parts := []Map{}
	if m, ok := input.(map[string]any); ok {
		parts = append(parts, dicts(m["content"])...)
	} else {
		items := dicts(input)
		allTyped := len(items) == len(list(input))
		for _, m := range items {
			if !truth(m["type"]) {
				allTyped = false
			}
		}
		if allTyped {
			parts = items
		} else {
			for _, m := range items {
				parts = append(parts, dicts(m["content"])...)
			}
		}
	}
	total := 0
	for _, part := range parts {
		typ := text(part["type"])
		if typ != "image" && typ != "image_url" && typ != "input_image" && !truth(part["source"]) {
			continue
		}
		width, height := integer(part["width"], 0), integer(part["height"], 0)
		if width <= 0 || height <= 0 {
			var data []byte
			source := part["image_url"]
			if m, ok := source.(map[string]any); ok {
				source = orValue(m["url"], m["image_url"])
			}
			if encoded, ok := source.(string); ok && strings.HasPrefix(encoded, "data:") {
				_, encoded, _ = strings.Cut(encoded, ",")
				data, _ = base64.StdEncoding.DecodeString(encoded)
			}
			if source := object(part["source"]); source["type"] == "base64" {
				data, _ = base64.StdEncoding.DecodeString(text(source["data"]))
			}
			if len(data) > 0 {
				config, _, e := image.DecodeConfig(bytes.NewReader(data))
				if e == nil {
					width, height = config.Width, config.Height
				}
			}
		}
		total += publicImageGeometryTokens(width, height, text(part["detail"]))
	}
	return total
}

func publicImageGeometryTokens(width, height int, detail string) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(detail), "low") {
		return 415
	}
	w, h := float64(width), float64(height)
	scale := math.Min(1, 2048/math.Max(w, h))
	w *= scale
	h *= scale
	patch := func(w, h float64) float64 { return math.Ceil(w/32) * math.Ceil(h/32) }
	if patch(w, h) > 1536 {
		shrink := math.Sqrt(32 * 32 * 1536 / (w * h))
		wu, hu := w*shrink/32, h*shrink/32
		shrink *= math.Min(math.Floor(wu)/wu, math.Floor(hu)/hu)
		w *= shrink
		h *= shrink
	}
	return int(math.Ceil(math.Min(patch(math.Max(1, w), math.Max(1, h)), 1536) * 1.62))
}
