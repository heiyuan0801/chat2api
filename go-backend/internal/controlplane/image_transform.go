package controlplane

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/upstream"
	xdraw "golang.org/x/image/draw"
)

// Image masks replace alpha, rather than blending source RGB. Lanczos3 is
// separable and bounded by the validated decoded dimensions.
func compositeImageMasks(ctx context.Context, inputs, masks []upstream.ImageInput) ([]upstream.ImageInput, error) {
	out := make([]upstream.ImageInput, 0, len(inputs))
	for i, input := range inputs {
		if _, _, e := imageDimensions(input.Data); e != nil {
			return nil, e
		}
		maskInput := masks[min(i, len(masks)-1)]
		if _, _, e := imageDimensions(maskInput.Data); e != nil {
			return nil, e
		}
		source, _, e := image.Decode(bytes.NewReader(input.Data))
		if e != nil {
			return nil, imageProblem("invalid_image_input", 0, "invalid image input", nil)
		}
		mask, maskFormat, e := image.Decode(bytes.NewReader(maskInput.Data))
		if e != nil {
			return nil, imageProblem("invalid_image_input", 0, "invalid image mask", nil)
		}
		bounds := source.Bounds()
		target := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		alpha := image.NewGray(image.Rect(0, 0, mask.Bounds().Dx(), mask.Bounds().Dy()))
		useAlpha := false
		switch mask.(type) {
		case *image.NRGBA, *image.NRGBA64, *image.RGBA, *image.RGBA64:
			useAlpha = true
		}
		// Pillow reads JPEG/RGB PNG as RGB, not RGBA. Go's PNG decoder represents
		// opaque RGB as RGBA too; alpha masks apply only if the format carries alpha.
		if maskFormat == "jpeg" {
			useAlpha = false
		}
		if maskFormat == "png" && len(maskInput.Data) > 25 {
			ct := maskInput.Data[25]
			useAlpha = ct == 6
		}
		for y := 0; y < alpha.Bounds().Dy(); y++ {
			if y%128 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			for x := 0; x < alpha.Bounds().Dx(); x++ {
				c := mask.At(x+mask.Bounds().Min.X, y+mask.Bounds().Min.Y)
				value := uint8(0)
				if useAlpha {
					_, _, _, a := c.RGBA()
					value = uint8(a >> 8)
				} else {
					rgb := color.NRGBAModel.Convert(c).(color.NRGBA)
					value = uint8((19595*uint32(rgb.R) + 38470*uint32(rgb.G) + 7471*uint32(rgb.B) + 32768) >> 16)
				}
				alpha.SetGray(x, y, color.Gray{Y: value})
			}
		}
		resized, e := resizeAlphaLanczos(ctx, alpha, target.Bounds().Dx(), target.Bounds().Dy())
		if e != nil {
			return nil, e
		}
		for y := 0; y < target.Bounds().Dy(); y++ {
			if y%128 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			for x := 0; x < target.Bounds().Dx(); x++ {
				c := color.NRGBAModel.Convert(source.At(x+bounds.Min.X, y+bounds.Min.Y)).(color.NRGBA)
				c.A = resized.GrayAt(x, y).Y
				target.SetNRGBA(x, y, c)
			}
		}
		var encoded bytes.Buffer
		if e = png.Encode(&encoded, target); e != nil {
			return nil, e
		}
		if encoded.Len() > maxImageBytes {
			return nil, imageProblem("invalid_image_input", 0, "image input exceeds 50MB limit", nil)
		}
		out = append(out, upstream.ImageInput{Data: encoded.Bytes(), MIME: "image/png", Name: input.Name})
	}
	return out, nil
}
func lanczos(x float64) float64 {
	x = math.Abs(x)
	if x == 0 {
		return 1
	}
	if x >= 3 {
		return 0
	}
	return 3 * math.Sin(math.Pi*x) * math.Sin(math.Pi*x/3) / (math.Pi * math.Pi * x * x)
}

type alphaWeight struct {
	index  int
	weight float64
}

func alphaWeights(source, target int) [][]alphaWeight {
	weights := make([][]alphaWeight, target)
	scale := float64(source) / float64(target)
	filterScale := math.Max(1, scale)
	for x := range target {
		center := (float64(x)+.5)*scale - .5
		sum := 0.0
		lo := max(0, int(math.Ceil(center-3*filterScale)))
		hi := min(source-1, int(math.Floor(center+3*filterScale)))
		for i := lo; i <= hi; i++ {
			w := lanczos((float64(i) - center) / filterScale)
			weights[x] = append(weights[x], alphaWeight{i, w})
			sum += w
		}
		if sum != 0 {
			for i := range weights[x] {
				weights[x][i].weight /= sum
			}
		}
	}
	return weights
}
func resizeAlphaLanczos(ctx context.Context, source *image.Gray, width, height int) (*image.Gray, error) {
	if source.Bounds().Dx() == width && source.Bounds().Dy() == height {
		return source, nil
	}
	// Independently safe source and destination areas can still produce an
	// enormous cross-shaped intermediate. Bound that allocation before weights
	// or buffers are built.
	if int64(width)*int64(source.Bounds().Dy()) > maxImagePixels {
		return nil, imageProblem("invalid_image_input", 0, "image resize intermediate exceeds pixel limit", nil)
	}
	horizontal := image.NewGray(image.Rect(0, 0, width, source.Bounds().Dy()))
	wx, wy := alphaWeights(source.Bounds().Dx(), width), alphaWeights(source.Bounds().Dy(), height)
	clamp := func(v float64) uint8 { return uint8(max(0, math.Min(255, math.Floor(v+.5)))) }
	for y := 0; y < source.Bounds().Dy(); y++ {
		if y%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		for x, weights := range wx {
			v := 0.0
			for _, w := range weights {
				v += float64(source.GrayAt(w.index, y).Y) * w.weight
			}
			horizontal.SetGray(x, y, color.Gray{Y: clamp(v)})
		}
	}
	out := image.NewGray(image.Rect(0, 0, width, height))
	for y, weights := range wy {
		if y%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		for x := range width {
			v := 0.0
			for _, w := range weights {
				v += float64(horizontal.GrayAt(x, w.index).Y) * w.weight
			}
			out.SetGray(x, y, color.Gray{Y: clamp(v)})
		}
	}
	return out, nil
}

var exactImageSize = regexp.MustCompile(`(?i)^(\d{2,5})\s*x\s*(\d{2,5})$`)

// Generated-image upscaling uses the existing optional Sharp helper first for
// sharp_lanczos3; pillow_lanczos and Sharp failures use bounded native Lanczos.
// Native encoders retain JPEG/WebP/PNG containers, but are not byte-identical to
// Pillow/libwebp: JPEG uses Go quality 95 and WebP uses lossless VP8L.
var imageUpscaleSlot = make(chan struct{}, 1)

func resizeGeneratedImage(ctx context.Context, data []byte, size string, enabled bool, engine string) ([]byte, error) {
	w, h, e := imageDimensions(data)
	if e != nil {
		return data, nil
	}
	if !enabled {
		return data, nil
	}
	parts := exactImageSize.FindStringSubmatch(strings.TrimSpace(size))
	if len(parts) != 3 {
		return data, nil
	}
	tw, _ := strconv.Atoi(parts[1])
	th, _ := strconv.Atoi(parts[2])
	if tw < 1 || th < 1 || tw > 8192 || th > 8192 || (w >= tw && h >= th) {
		return data, nil
	}
	select {
	case imageUpscaleSlot <- struct{}{}:
		defer func() { <-imageUpscaleSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	_, format, _ := image.DecodeConfig(bytes.NewReader(data))
	orientation := imageEXIFOrientation(data, format)
	orientedHeight := h
	if orientation >= 5 && orientation <= 8 {
		orientedHeight = w
	}
	if int64(tw)*int64(orientedHeight) > maxImagePixels {
		return data, nil
	}
	if engine == "sharp_lanczos3" {
		if resized, err := sharpResize(ctx, data, tw, th); err == nil {
			return resized, nil
		}
		if e = ctx.Err(); e != nil {
			return nil, e
		}
	}
	source, _, e := image.Decode(bytes.NewReader(data))
	if e != nil {
		return data, nil
	}
	if orientation >= 2 && orientation <= 8 {
		source = orientedImage{Image: source, orientation: orientation}
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	out := image.NewNRGBA(image.Rect(0, 0, tw, th))
	kernel := xdraw.Kernel{Support: 3, At: lanczos}
	kernel.Scale(out, out.Bounds(), source, source.Bounds(), xdraw.Src, nil)
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	encoded := &boundedImageBuffer{ctx: ctx, limit: maxImageBytes}
	switch format {
	case "jpeg":
		e = jpeg.Encode(encoded, out, &jpeg.Options{Quality: 95})
	case "webp":
		e = nativewebp.Encode(encoded, out, &nativewebp.Options{CompressionLevel: nativewebp.DefaultCompression})
	default:
		e = png.Encode(encoded, out)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if e != nil || encoded.Len() == 0 {
		return data, nil
	}
	return encoded.Bytes(), nil
}

type boundedImageBuffer struct {
	buffer  bytes.Buffer
	ctx     context.Context
	limit   int
	onLimit context.CancelFunc
}

func (b *boundedImageBuffer) Len() int      { return b.buffer.Len() }
func (b *boundedImageBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedImageBuffer) Write(p []byte) (int, error) {
	if e := b.ctx.Err(); e != nil {
		return 0, e
	}
	if len(p) > b.limit-b.Len() {
		if b.onLimit != nil {
			b.onLimit()
		}
		return 0, errors.New("image output exceeds limit")
	}
	return b.buffer.Write(p)
}

func sharpResize(ctx context.Context, data []byte, width, height int) ([]byte, error) {
	node, e := exec.LookPath("node")
	if e != nil {
		return nil, e
	}
	helper := sharpImageHelper()
	if helper == "" {
		return nil, errors.New("Sharp helper unavailable")
	}
	child, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	command := exec.CommandContext(child, node, helper, strconv.Itoa(width), strconv.Itoa(height))
	command.Stdin = bytes.NewReader(data)
	command.Stderr = io.Discard
	output := &boundedImageBuffer{ctx: child, limit: maxImageBytes, onLimit: cancel}
	command.Stdout = output
	if e = command.Run(); e != nil {
		return nil, e
	}
	w, h, e := imageDimensions(output.Bytes())
	if e != nil || w != width || h != height {
		return nil, errors.New("invalid Sharp output")
	}
	return output.Bytes(), nil
}
func sharpImageHelper() string {
	valid := func(path string) bool { stat, e := os.Stat(path); return e == nil && stat.Mode().IsRegular() }
	if explicit := os.Getenv("CHATGPT2API_SHARP_SCRIPT"); explicit != "" {
		if filepath.IsAbs(explicit) && valid(explicit) {
			return explicit
		}
		return ""
	}
	roots := []string{}
	if cwd, e := os.Getwd(); e == nil {
		roots = append(roots, cwd)
	}
	if executable, e := os.Executable(); e == nil {
		roots = append(roots, filepath.Dir(executable))
	}
	home, _ := os.UserHomeDir()
	for _, root := range roots {
		for depth := 0; depth < 3; depth++ {
			if root == home {
				break
			}
			candidate := filepath.Join(root, "scripts", "image_upscale", "upscale.mjs")
			if valid(candidate) {
				return candidate
			}
			parent := filepath.Dir(root)
			if parent == root {
				break
			}
			root = parent
		}
	}
	return ""
}

// Only IFD0's inline Orientation SHORT is needed. Container and TIFF offsets
// are checked before every read; metadata is capped and no IFD graph is walked.
func imageEXIFOrientation(data []byte, format string) int {
	metadata := []byte(nil)
	switch format {
	case "jpeg":
		for offset := 2; offset+4 <= len(data); {
			if data[offset] != 0xff {
				break
			}
			marker := data[offset+1]
			offset += 2
			if marker == 0xda || marker == 0xd9 {
				break
			}
			if marker == 0xff {
				offset--
				continue
			}
			if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd8) {
				continue
			}
			length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			if length < 2 || length > len(data)-offset {
				break
			}
			body := data[offset+2 : offset+length]
			if marker == 0xe1 && bytes.HasPrefix(body, []byte("Exif\x00\x00")) {
				metadata = body
				break
			}
			offset += length
		}
	case "webp":
		if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
			for offset := 12; offset+8 <= len(data); {
				n := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
				if n > uint64(len(data)-offset-8) {
					break
				}
				if string(data[offset:offset+4]) == "EXIF" {
					metadata = data[offset+8 : offset+8+int(n)]
					break
				}
				offset += 8 + int(n) + (int(n) & 1)
			}
		}
	case "png":
		for offset := 8; offset+12 <= len(data); {
			n := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
			if n > uint64(len(data)-offset-12) {
				break
			}
			if string(data[offset+4:offset+8]) == "eXIf" {
				metadata = data[offset+8 : offset+8+int(n)]
				break
			}
			offset += 12 + int(n)
		}
	}
	if bytes.HasPrefix(metadata, []byte("Exif\x00\x00")) {
		metadata = metadata[6:]
	}
	if len(metadata) < 8 || len(metadata) > 64<<10 {
		return 1
	}
	var order binary.ByteOrder
	switch string(metadata[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(metadata[2:4]) != 42 {
		return 1
	}
	offset := uint64(order.Uint32(metadata[4:8]))
	if offset > uint64(len(metadata)-2) {
		return 1
	}
	count := int(order.Uint16(metadata[int(offset) : int(offset)+2]))
	offset += 2
	if uint64(count) > uint64(len(metadata)-int(offset))/12 {
		return 1
	}
	for i := 0; i < count; i++ {
		entry := metadata[int(offset)+12*i : int(offset)+12*(i+1)]
		if order.Uint16(entry[:2]) == 0x112 && order.Uint16(entry[2:4]) == 3 && order.Uint32(entry[4:8]) == 1 {
			value := int(order.Uint16(entry[8:10]))
			if value >= 1 && value <= 8 {
				return value
			}
			return 1
		}
	}
	return 1
}

type orientedImage struct {
	image.Image
	orientation int
}

func (o orientedImage) Bounds() image.Rectangle {
	b := o.Image.Bounds()
	if o.orientation >= 5 {
		return image.Rect(0, 0, b.Dy(), b.Dx())
	}
	return image.Rect(0, 0, b.Dx(), b.Dy())
}
func (o orientedImage) At(x, y int) color.Color {
	b := o.Image.Bounds()
	w, h := b.Dx(), b.Dy()
	switch o.orientation {
	case 2:
		x = w - 1 - x
	case 3:
		x, y = w-1-x, h-1-y
	case 4:
		y = h - 1 - y
	case 5:
		x, y = y, x
	case 6:
		x, y = y, h-1-x
	case 7:
		x, y = w-1-y, h-1-x
	case 8:
		x, y = w-1-y, x
	}
	return o.Image.At(x+b.Min.X, y+b.Min.Y)
}
