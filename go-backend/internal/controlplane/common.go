package controlplane

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
)

type Map = repository.Document
type Problem struct {
	Status  int
	Message string
}

func (p *Problem) Error() string         { return p.Message }
func problem(status int, s string) error { return &Problem{status, s} }
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return string(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
func object(v any) Map {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return Map{}
}
func list(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []Map:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = v
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = v
		}
		return out
	case []int:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = v
		}
		return out
	}
	return []any{}
}
func dicts(v any) []Map {
	out := []Map{}
	for _, x := range list(v) {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
func stringsOf(v any) []string {
	out := []string{}
	for _, x := range list(v) {
		if s := text(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}
func unique(xs []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range xs {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, e := x.Float64()
		return f, e == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	case float64:
		return x, !math.IsInf(x, 0) && !math.IsNaN(x)
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, e := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, e == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	}
	return 0, false
}
func integer(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case json.Number:
		if n, e := strconv.Atoi(string(x)); e == nil {
			return n
		}
	case string:
		if n, e := strconv.Atoi(strings.TrimSpace(x)); e == nil {
			return n
		}
	}
	f, ok := number(v)
	if !ok || f > float64(math.MaxInt) || f < float64(math.MinInt) {
		return def
	}
	return int(f)
}
func boolean(v any, def bool) bool {
	if v == nil {
		return def
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		switch s {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off", "":
			return false
		}
		return def
	default:
		f, ok := number(v)
		if ok {
			return f != 0
		}
	}
	return def
}
func truth(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		f, ok := number(v)
		return !ok || f != 0
	}
}
func defaultValue(v, def any) any {
	if v == nil {
		return def
	}
	return v
}
func copyMap(m Map) Map { return repository.Clone(m) }
func merge(base, patch Map) Map {
	out := copyMap(base)
	for k, v := range patch {
		if m, ok := v.(map[string]any); ok {
			out[k] = merge(object(out[k]), m)
		} else {
			out[k] = v
		}
	}
	return out
}
func pathGet(m Map, path string) (any, bool) {
	bits := strings.Split(path, ".")
	var v any = m
	for _, k := range bits {
		p, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = p[k]
		if !ok {
			return nil, false
		}
	}
	return v, true
}
func pathSet(m Map, path string, v any) {
	bits := strings.Split(path, ".")
	for _, k := range bits[:len(bits)-1] {
		child, ok := m[k].(map[string]any)
		if !ok {
			child = Map{}
			m[k] = child
		}
		m = child
	}
	m[bits[len(bits)-1]] = v
}
func pathDelete(m Map, path string) {
	bits := strings.Split(path, ".")
	for _, k := range bits[:len(bits)-1] {
		child, ok := m[k].(map[string]any)
		if !ok {
			return
		}
		m = child
	}
	delete(m, bits[len(bits)-1])
}
func leafPaths(m Map, prefix string) []string {
	out := []string{}
	for k, v := range m {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			out = append(out, leafPaths(child, p)...)
		} else {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
func slug(v any) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text(v)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if unicode.IsSpace(r) {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_")
}
func id(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic("system random source unavailable")
	}
	return hex.EncodeToString(b)
}
func digest(s string) string         { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func generated(now time.Time) string { return now.UTC().Format("2006-01-02T15:04:05.999999Z") }
func iso(now time.Time) string       { return now.UTC().Format("2006-01-02T15:04:05.999999") + "+00:00" }
func urlText(v any) string           { return strings.TrimRight(text(v), "/") }

// canonical reproduces Python json.dumps(sort_keys=True, ensure_ascii=False,
// separators=(',', ':')) for the JSON values used by revision contracts.
func canonical(v any) string {
	var b bytes.Buffer
	var emit func(any)
	emit = func(v any) {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case string:
			b.WriteByte('"')
			for _, r := range x {
				switch r {
				case '"', '\\':
					b.WriteByte('\\')
					b.WriteRune(r)
				case '\b':
					b.WriteString(`\b`)
				case '\f':
					b.WriteString(`\f`)
				case '\n':
					b.WriteString(`\n`)
				case '\r':
					b.WriteString(`\r`)
				case '\t':
					b.WriteString(`\t`)
				default:
					if r < 32 {
						fmt.Fprintf(&b, `\u%04x`, r)
					} else {
						b.WriteRune(r)
					}
				}
			}
			b.WriteByte('"')
		case json.Number:
			b.WriteString(string(x))
		case float64:
			a := math.Abs(x)
			format := byte('f')
			if a != 0 && (a < 1e-4 || a >= 1e16) {
				format = 'g'
			}
			s := strconv.FormatFloat(x, format, -1, 64)
			if !strings.ContainsAny(s, ".eE") {
				s += ".0"
			}
			b.WriteString(s)
		case int:
			b.WriteString(strconv.Itoa(x))
		case int64:
			b.WriteString(strconv.FormatInt(x, 10))
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				emit(k)
				b.WriteByte(':')
				emit(x[k])
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, v := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				emit(v)
			}
			b.WriteByte(']')
		default:
			raw, e := json.Marshal(v)
			if e != nil {
				panic("non-JSON revision")
			}
			var decoded any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if dec.Decode(&decoded) != nil {
				panic("non-JSON revision")
			}
			emit(decoded)
		}
	}
	emit(v)
	return b.String()
}
