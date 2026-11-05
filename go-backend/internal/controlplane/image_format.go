package controlplane

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"image"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type imageFormat struct {
	MIME, Extension string
	Width, Height   int
}

// ImageAssets owns format validation for generated files, including vector
// output. Raster input validation remains separate: SVG is not an edit input.
func inspectImage(data []byte) (imageFormat, error) {
	bad := func() (imageFormat, error) {
		return imageFormat{}, imageProblem("invalid_image_input", 400, "image dimensions or format are invalid", nil)
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return bad()
	}
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		ext := map[string]string{"png": ".png", "jpeg": ".jpg", "gif": ".gif", "webp": ".webp"}[format]
		if ext == "" || !validImageDimensions(cfg.Width, cfg.Height) {
			return bad()
		}
		return imageFormat{"image/" + format, ext, cfg.Width, cfg.Height}, nil
	}
	w, h, err := svgDimensions(data)
	if err != nil {
		return bad()
	}
	return imageFormat{"image/svg+xml", ".svg", w, h}, nil
}

func validImageDimensions(w, h int) bool {
	return w > 0 && h > 0 && w <= 32768 && h <= 32768 && int64(w)*int64(h) <= maxImagePixels
}

var svgElements = strings.Fields("svg g defs title desc path rect circle ellipse line polyline polygon text tspan textPath linearGradient radialGradient stop clipPath mask pattern symbol use marker")
var svgAttributes = strings.Fields("id class version width height viewBox preserveAspectRatio x y x1 y1 x2 y2 cx cy r rx ry d points transform fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity clip-path clip-rule mask display visibility font-family font-size font-weight font-style text-anchor dominant-baseline alignment-baseline letter-spacing word-spacing dx dy rotate textLength lengthAdjust offset stop-color stop-opacity gradientUnits gradientTransform spreadMethod fx fy fr patternUnits patternContentUnits patternTransform clipPathUnits maskUnits maskContentUnits markerWidth markerHeight markerUnits refX refY orient overflow vector-effect paint-order color href startOffset method spacing role aria-label aria-labelledby focusable")
var svgLocalURL = regexp.MustCompile(`(?i)^url\(\s*['"]?#[A-Za-z_][A-Za-z0-9_.:-]*['"]?\s*\)$`)

// Accept a bounded, passive SVG subset. Reject active content rather than
// silently modifying the generated document or publishing executable markup.
func svgDimensions(data []byte) (int, int, error) {
	invalid := imageProblem("invalid_image_input", 400, "unsupported or unsafe SVG document", nil)
	if len(data) > 2<<20 {
		return 0, 0, invalid
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	depth, nodes, roots := 0, 0, 0
	w, h := 0.0, 0.0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, invalid
		}
		switch v := token.(type) {
		case xml.StartElement:
			depth++
			nodes++
			if depth > 64 || nodes > 50000 || v.Name.Space != "http://www.w3.org/2000/svg" || !containsString(svgElements, v.Name.Local) {
				return 0, 0, invalid
			}
			attrs := map[string]string{}
			for _, a := range v.Attr {
				if a.Name.Space == "xmlns" || a.Name.Space == "" && a.Name.Local == "xmlns" {
					if a.Value != "http://www.w3.org/2000/svg" && a.Value != "http://www.w3.org/1999/xlink" {
						return 0, 0, invalid
					}
					continue
				}
				if a.Name.Space == "http://www.w3.org/XML/1998/namespace" && a.Name.Local == "space" {
					continue
				}
				if a.Name.Space != "" && !(a.Name.Space == "http://www.w3.org/1999/xlink" && a.Name.Local == "href") || !containsString(svgAttributes, a.Name.Local) {
					return 0, 0, invalid
				}
				value := strings.TrimSpace(a.Value)
				if strings.ContainsAny(value, "\\\x00") || strings.Contains(strings.ToLower(value), "url(") && !svgLocalURL.MatchString(value) || a.Name.Local == "href" && (!strings.HasPrefix(value, "#") || len(value) < 2) {
					return 0, 0, invalid
				}
				attrs[a.Name.Local] = value
			}
			if depth == 1 {
				roots++
				if roots != 1 || v.Name.Local != "svg" {
					return 0, 0, invalid
				}
				length := func(s string) float64 {
					n, _ := strconv.ParseFloat(strings.TrimSuffix(s, "px"), 64)
					return n
				}
				w, h = length(attrs["width"]), length(attrs["height"])
				view := strings.Fields(strings.ReplaceAll(attrs["viewBox"], ",", " "))
				if len(view) == 4 {
					if w <= 0 {
						w = length(view[2])
					}
					if h <= 0 {
						h = length(view[3])
					}
				}
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return 0, 0, invalid
		case xml.ProcInst:
			if v.Target != "xml" || roots != 0 {
				return 0, 0, invalid
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return 0, 0, invalid
			}
		}
	}
	if roots != 1 || depth != 0 || math.IsNaN(w) || math.IsNaN(h) || math.IsInf(w, 0) || math.IsInf(h, 0) || w <= 0 || h <= 0 || w > 32768 || h > 32768 {
		return 0, 0, invalid
	}
	width, height := int(math.Ceil(w)), int(math.Ceil(h))
	if !validImageDimensions(width, height) {
		return 0, 0, invalid
	}
	return width, height, nil
}

func imageBase64MIME(encoded string) string {
	// Raster MIME needs only magic bytes; avoid decoding large payloads again on
	// every task projection. SVG requires the same XML validation as publication.
	prefix := encoded[:min(len(encoded), 64)]
	if head, err := base64.StdEncoding.DecodeString(prefix); err == nil {
		switch mime := http.DetectContentType(head); mime {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return mime
		}
	}
	if len(encoded) <= base64.StdEncoding.EncodedLen(2<<20) {
		if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			if info, err := inspectImage(data); err == nil {
				return info.MIME
			}
		}
	}
	return "image/png" // Compatibility for historical task projections.
}

func imageResponseHeaders(mime string, headers map[string]string) map[string]string {
	headers["X-Content-Type-Options"] = "nosniff"
	if mime == "image/svg+xml" {
		headers["Content-Security-Policy"] = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
	}
	return headers
}
