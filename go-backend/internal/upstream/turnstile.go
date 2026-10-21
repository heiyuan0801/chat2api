package upstream

// This VM ports utils/turnstile.py. It interprets only the baseline's closed
// instruction set: it cannot execute JavaScript, access files or make requests.
// Fractional register identifiers must remain distinct (for example 66.41).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	turnstileMaxBytes  = 1 << 20
	turnstileMaxSteps  = 20000
	turnstileMaxDepth  = 64
	turnstileMaxCells  = 8192
	turnstileWorkBytes = 16 << 20
)

var errVMOperation = errors.New("unsupported turnstile operation")
var errVMLimit = errors.New("turnstile execution limit")

type tsList struct{ items []any }
type tsObject struct {
	keys   []string
	values map[string]any
	native bool
}
type tsFunction func([]any) (any, error)
type tsVM struct {
	ctx                context.Context
	cells              map[string]any
	steps, depth, work int
	started            time.Time
	result             string
}

func tsKey(v any) (string, error) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return "", errVMOperation
		}
		return "n:" + strconv.FormatFloat(f, 'g', -1, 64), nil
	case float64:
		return tsKey(json.Number(strconv.FormatFloat(n, 'g', -1, 64)))
	case int:
		return "n:" + strconv.Itoa(n), nil
	case string:
		if len(n) > 4096 {
			return "", errVMLimit
		}
		return "s:" + n, nil
	case bool:
		if n {
			return "n:1", nil
		}
		return "n:0", nil
	case nil:
		return "nil", nil
	}
	return "", errVMOperation
}

func (v *tsVM) get(key any) any {
	k, err := tsKey(key)
	if err != nil {
		return nil
	}
	return v.cells[k]
}
func (v *tsVM) put(key, value any) error {
	k, err := tsKey(key)
	if err != nil {
		return err
	}
	if _, ok := v.cells[k]; !ok && len(v.cells) >= turnstileMaxCells {
		return errVMLimit
	}
	size := 0
	switch x := value.(type) {
	case string:
		size = len(x)
	case *tsList:
		if x != nil {
			size = len(x.items) * 8
		}
	}
	if size > turnstileMaxBytes {
		return errVMLimit
	}
	v.work += size
	if v.work > turnstileWorkBytes {
		return errVMLimit
	}
	v.cells[k] = value
	return nil
}

func tsDecode(raw []byte) (any, error) {
	if len(raw) > turnstileMaxBytes {
		return nil, errVMLimit
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var parse func(int) (any, error)
	parse = func(depth int) (any, error) {
		nodes++
		if depth > turnstileMaxDepth || nodes > 65536 {
			return nil, errVMLimit
		}
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return token, nil
		}
		switch delim {
		case '[':
			list := &tsList{}
			for d.More() {
				child, err := parse(depth + 1)
				if err != nil {
					return nil, err
				}
				list.items = append(list.items, child)
			}
			if _, err = d.Token(); err != nil {
				return nil, err
			}
			return list, nil
		case '{':
			obj := &tsObject{values: map[string]any{}}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				s, ok := key.(string)
				if !ok {
					return nil, errVMOperation
				}
				child, err := parse(depth + 1)
				if err != nil {
					return nil, err
				}
				if _, ok := obj.values[s]; !ok {
					obj.keys = append(obj.keys, s)
				}
				obj.values[s] = child
			}
			if _, err = d.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		}
		return nil, errVMOperation
	}
	value, err := parse(0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errVMOperation
	}
	return value, nil
}

func tsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
func tsFloat(v any) (float64, bool) {
	if s, ok := v.(string); ok {
		f, e := strconv.ParseFloat(s, 64)
		return f, e == nil
	}
	return tsNumber(v)
}
func tsIsFloat(v any) bool {
	switch n := v.(type) {
	case float64:
		return true
	case json.Number:
		return strings.ContainsAny(string(n), ".eE")
	}
	return false
}

func tsString(value any) string { return tsStringDepth(value, 0) }
func tsStringDepth(value any, depth int) string {
	if depth > turnstileMaxDepth {
		panic(errVMLimit)
	}
	if value == nil {
		return "undefined"
	}
	switch v := value.(type) {
	case string:
		if len(v) > turnstileMaxBytes {
			panic(errVMLimit)
		}
		special := map[string]string{
			"window.Math": "[object Math]", "window.Reflect": "[object Reflect]", "window.performance": "[object Performance]", "window.localStorage": "[object Storage]",
			"window.Object": "function Object() { [native code] }", "window.Reflect.set": "function set() { [native code] }",
			"window.performance.now": "function () { [native code] }", "window.Object.create": "function create() { [native code] }",
			"window.Object.keys": "function keys() { [native code] }", "window.Math.random": "function random() { [native code] }",
		}
		if s, ok := special[v]; ok {
			return s
		}
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		if tsIsFloat(v) {
			f, _ := v.Float64()
			return tsString(f)
		}
		return string(v)
	case float64:
		s := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") && !math.IsInf(v, 0) && !math.IsNaN(v) {
			s += ".0"
		}
		return s
	case int:
		return strconv.Itoa(v)
	case *tsList:
		if v == nil {
			return "None"
		}
		parts := make([]string, 0, len(v.items))
		allStrings := true
		total := 0
		for _, item := range v.items {
			s, ok := item.(string)
			if !ok {
				allStrings = false
				break
			}
			total += len(s) + 2
			if total > turnstileMaxBytes {
				panic(errVMLimit)
			}
			parts = append(parts, s)
		}
		if allStrings {
			return strings.Join(parts, ",")
		}
		parts = nil
		total = 0
		for _, item := range v.items {
			s := tsReprDepth(item, depth+1)
			total += len(s) + 2
			if total > turnstileMaxBytes {
				panic(errVMLimit)
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return tsReprDepth(value, depth+1)
}

func tsRepr(v any) string { return tsReprDepth(v, 0) }
func tsReprDepth(v any, depth int) string {
	if depth > turnstileMaxDepth {
		panic(errVMLimit)
	}
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		x = strings.ReplaceAll(x, `\`, `\\`)
		x = strings.ReplaceAll(x, q, `\`+q)
		x = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(x)
		return q + x + q
	case *tsObject:
		parts := make([]string, 0, len(x.keys))
		total := 0
		for _, key := range x.keys {
			s := tsReprDepth(key, depth+1) + ": " + tsReprDepth(x.values[key], depth+1)
			total += len(s) + 2
			if total > turnstileMaxBytes {
				panic(errVMLimit)
			}
			parts = append(parts, s)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *tsVM:
		return "<process map>"
	case tsFunction:
		return "<function>"
	default:
		return tsStringDepth(v, depth+1)
	}
}

func tsXOR(value, key string) string {
	if key == "" {
		return value
	}
	runes, secret := []rune(value), []rune(key)
	for i := range runes {
		runes[i] ^= secret[i%len(secret)]
	}
	return string(runes)
}

func tsJSONString(value string) string {
	quoted := compactJSON(value)
	var out strings.Builder
	for _, r := range string(quoted) {
		if r <= 127 {
			out.WriteRune(r)
		} else if r <= 65535 {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			a, b := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, a, b)
		}
	}
	return out.String()
}

func tsJSON(value any, depth int) (string, error) {
	if depth > turnstileMaxDepth {
		return "", errVMLimit
	}
	switch v := value.(type) {
	case nil:
		return "null", nil
	case string:
		return tsJSONString(v), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return string(v), nil
	case float64:
		return tsString(v), nil
	case int:
		return strconv.Itoa(v), nil
	case *tsList:
		parts := make([]string, 0, len(v.items))
		total := 0
		for _, item := range v.items {
			s, err := tsJSON(item, depth+1)
			if err != nil {
				return "", err
			}
			total += len(s) + 2
			if total > turnstileMaxBytes {
				return "", errVMLimit
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case *tsObject:
		// Python's OrderedMap is not JSON-serializable; parsed JSON dicts are.
		if v.native {
			return "", errVMOperation
		}
		parts := make([]string, 0, len(v.keys))
		total := 0
		for _, key := range v.keys {
			s, err := tsJSON(v.values[key], depth+1)
			if err != nil {
				return "", err
			}
			s = tsJSONString(key) + ": " + s
			total += len(s) + 2
			if total > turnstileMaxBytes {
				return "", errVMLimit
			}
			parts = append(parts, s)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	return "", errVMOperation
}

func tsEqual(a, b any) bool {
	if x, ok := tsNumber(a); ok {
		if y, ok := tsNumber(b); ok {
			return x == y
		}
	}
	return reflect.DeepEqual(a, b)
}
func tsProperty(obj, key any) any {
	switch o := obj.(type) {
	case *tsObject:
		return o.values[tsString(key)]
	case *tsList:
		if index, ok := tsFloat(key); ok && index >= 0 && index < float64(len(o.items)) {
			return o.items[int(index)]
		}
	case *tsVM:
		return o.get(key)
	case string:
		name := tsString(key)
		if name == "location" && o == "window.document" {
			return origin + "/"
		}
		if name != "" && name != "undefined" && name != "None" {
			return o + "." + name
		}
	}
	return nil
}

func (v *tsVM) call(target any, args []any) (any, error) {
	if err := v.ctx.Err(); err != nil {
		return nil, err
	}
	if fn, ok := target.(tsFunction); ok {
		return fn(args)
	}
	if name, ok := target.(string); ok {
		switch name {
		case "window.performance.now":
			return float64(time.Since(v.started).Nanoseconds())/1e6 + rand.Float64()/1e6, nil
		case "window.Math.random":
			return rand.Float64(), nil
		case "window.Object.create":
			return &tsObject{values: map[string]any{}, native: true}, nil
		case "window.Object.keys":
			if len(args) == 0 {
				return nil, nil
			}
			if s, ok := args[0].(string); ok && s == "window.localStorage" {
				return &tsList{items: []any{"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4", "STATSIG_LOCAL_STORAGE_STABLE_ID", "client-correlated-secret", "oai/apps/capExpiresAt", "oai-did", "STATSIG_LOCAL_STORAGE_LOGGING_REQUEST", "UiState.isNavigationCollapsed.1"}}, nil
			}
			if obj, ok := args[0].(*tsObject); ok {
				list := &tsList{}
				for _, key := range obj.keys {
					list.items = append(list.items, key)
				}
				return list, nil
			}
		case "window.Reflect.set":
			if len(args) < 3 {
				return false, nil
			}
			if obj, ok := args[0].(*tsObject); ok {
				key := tsString(args[1])
				if len(key) > turnstileMaxBytes || len(obj.keys) >= turnstileMaxCells {
					return nil, errVMLimit
				}
				if _, exists := obj.values[key]; !exists {
					obj.keys = append(obj.keys, key)
				}
				obj.values[key] = args[2]
				return true, nil
			}
			return false, nil
		}
	}
	return nil, nil
}

func tsArgs(args []any, n int) error {
	if len(args) < n {
		return errVMOperation
	}
	return nil
}
func (v *tsVM) resolved(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = v.get(a)
	}
	return out
}

func (v *tsVM) instruction(op int, args []any) (any, error) {
	minimum := map[int]int{1: 2, 2: 2, 3: 1, 5: 2, 6: 3, 7: 1, 8: 2, 11: 2, 12: 1, 13: 2, 14: 2, 15: 2, 17: 2, 18: 1, 19: 1, 20: 3, 21: 4, 22: 2, 23: 2, 24: 3, 27: 2, 29: 3, 30: 3, 33: 3, 34: 2}
	if err := tsArgs(args, minimum[op]); err != nil {
		return nil, err
	}
	put := func(value any) (any, error) { return nil, v.put(args[0], value) }
	switch op {
	case 1:
		return put(tsXOR(tsString(v.get(args[0])), tsString(v.get(args[1]))))
	case 2:
		return put(args[1])
	case 3:
		s, ok := args[0].(string)
		if !ok {
			return nil, errVMOperation
		}
		if len(s) > turnstileMaxBytes*3/4 {
			return nil, errVMLimit
		}
		v.result = base64.StdEncoding.EncodeToString([]byte(s))
		return nil, nil
	case 5:
		current, incoming := v.get(args[0]), v.get(args[1])
		if list, ok := current.(*tsList); ok {
			return put(&tsList{items: append(append([]any(nil), list.items...), incoming)})
		}
		_, currentString := current.(string)
		_, incomingString := incoming.(string)
		if currentString || incomingString || tsIsFloat(current) || tsIsFloat(incoming) {
			if len(tsString(current))+len(tsString(incoming)) > turnstileMaxBytes {
				return nil, errVMLimit
			}
			return put(tsString(current) + tsString(incoming))
		}
		return put("NaN")
	case 6, 24:
		return put(tsProperty(v.get(args[1]), v.get(args[2])))
	case 7:
		return v.call(v.get(args[0]), v.resolved(args[1:]))
	case 8, 34:
		return put(v.get(args[1]))
	case 11:
		return put(nil)
	case 12:
		return put(v)
	case 13:
		_, err := v.call(v.get(args[1]), args[2:])
		if err != nil {
			if v.controlError(err) {
				return nil, err
			}
			return put(err.Error())
		}
		return nil, nil
	case 14:
		s, ok := v.get(args[1]).(string)
		if !ok {
			return nil, errVMOperation
		}
		value, err := tsDecode([]byte(s))
		if err != nil {
			return nil, err
		}
		return put(value)
	case 15:
		s, err := tsJSON(v.get(args[1]), 0)
		if err != nil {
			return nil, err
		}
		return put(s)
	case 17:
		value, err := v.call(v.get(args[1]), v.resolved(args[2:]))
		if err != nil {
			return nil, err
		}
		return put(value)
	case 18:
		decoded, err := base64.StdEncoding.DecodeString(tsString(v.get(args[0])))
		if err != nil || !utf8.Valid(decoded) {
			return nil, errVMOperation
		}
		return put(string(decoded))
	case 19:
		s := tsString(v.get(args[0]))
		if len(s) > turnstileMaxBytes*3/4 {
			return nil, errVMLimit
		}
		return put(base64.StdEncoding.EncodeToString([]byte(s)))
	case 20:
		if tsEqual(v.get(args[0]), v.get(args[1])) {
			return v.call(v.get(args[2]), args[3:])
		}
	case 21:
		x, xok := tsFloat(v.get(args[0]))
		y, yok := tsFloat(v.get(args[1]))
		threshold, _ := tsFloat(v.get(args[2]))
		delta := 0.0
		if xok && yok {
			delta = x - y
		}
		if math.Abs(delta) > math.Abs(threshold) {
			return v.call(v.get(args[3]), args[4:])
		}
	case 22:
		queue, ok := args[1].(*tsList)
		if !ok {
			return nil, errVMOperation
		}
		if err := v.runNested(queue, nil, nil); err != nil {
			return nil, err
		}
		return put("None")
	case 23:
		if v.get(args[0]) != nil {
			return v.call(v.get(args[1]), args[2:])
		}
	case 27:
		current, incoming := v.get(args[0]), v.get(args[1])
		if list, ok := current.(*tsList); ok {
			for i, item := range list.items {
				if tsEqual(item, incoming) {
					list.items = append(list.items[:i], list.items[i+1:]...)
					break
				}
			}
			return nil, nil
		}
		x, a := tsNumber(current)
		y, b := tsNumber(incoming)
		if a && b {
			return put(x - y)
		}
		return put(0)
	case 29:
		a, b := v.get(args[1]), v.get(args[2])
		if x, ok := a.(string); ok {
			y, valid := b.(string)
			return put(valid && x < y)
		}
		x, xok := tsNumber(a)
		y, yok := tsNumber(b)
		return put(xok && yok && x < y)
	case 30:
		var captures *tsList
		queue, ok := args[2].(*tsList)
		if len(args) > 3 {
			if q, valid := args[3].(*tsList); valid {
				queue, ok = q, true
				captures, _ = args[2].(*tsList)
			}
		}
		if !ok {
			return nil, errVMOperation
		}
		saved := &tsList{items: append([]any(nil), queue.items...)}
		return put(tsFunction(func(callArgs []any) (any, error) { return nil, v.runNested(saved, captures, callArgs) }))
	case 33:
		x, xok := tsFloat(v.get(args[1]))
		y, yok := tsFloat(v.get(args[2]))
		if xok && yok {
			return put(x * y)
		}
		return put(0)
	case 25, 26, 28:
		return nil, nil
	}
	return nil, nil
}

func (v *tsVM) controlError(err error) bool {
	return errors.Is(err, errVMLimit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
func (v *tsVM) runNested(queue, captures *tsList, args []any) error {
	previous, _ := v.get(9).(*tsList)
	if captures != nil {
		for i, key := range captures.items {
			if i < len(args) {
				if err := v.put(key, args[i]); err != nil {
					return err
				}
			}
		}
	}
	if err := v.put(9, &tsList{items: append([]any(nil), queue.items...)}); err != nil {
		return err
	}
	defer func() { v.cells["n:9"] = previous }()
	return v.run()
}

func (v *tsVM) run() error {
	v.depth++
	defer func() { v.depth-- }()
	if v.depth > turnstileMaxDepth {
		return errVMLimit
	}
	for {
		if err := v.ctx.Err(); err != nil {
			return err
		}
		queue, ok := v.get(9).(*tsList)
		if !ok || queue == nil || len(queue.items) == 0 {
			return nil
		}
		v.steps++
		if v.steps > turnstileMaxSteps {
			return errVMLimit
		}
		raw := queue.items[0]
		queue.items = queue.items[1:]
		instruction, ok := raw.(*tsList)
		if !ok || len(instruction.items) == 0 {
			continue
		}
		fn, ok := v.get(instruction.items[0]).(tsFunction)
		if !ok {
			continue
		}
		if _, err := fn(instruction.items[1:]); err != nil && v.controlError(err) {
			return err
		}
	}
}

// SolveTurnstile executes the baseline VM against a single requirements token.
// The challenge and solution remain local to the operation and are never logged.
func SolveTurnstile(ctx context.Context, dx, p string) (solution string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			solution = ""
			if recovered == errVMLimit {
				err = failure("turnstile_execution_limit", "turnstile")
			} else {
				err = failure("turnstile_invalid_input", "turnstile")
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(dx) == 0 || len(dx) > turnstileMaxBytes || len(p) > turnstileMaxBytes {
		return "", failure("turnstile_invalid_input", "turnstile")
	}
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil || !utf8.Valid(decoded) {
		return "", failure("turnstile_invalid_input", "turnstile")
	}
	program, err := tsDecode([]byte(tsXOR(string(decoded), p)))
	if err != nil {
		return "", failure("turnstile_invalid_input", "turnstile")
	}
	queue, ok := program.(*tsList)
	if !ok {
		return "", failure("turnstile_invalid_input", "turnstile")
	}
	vm := &tsVM{ctx: ctx, cells: map[string]any{}, started: time.Now()}
	for _, op := range []int{1, 2, 3, 5, 6, 7, 8, 11, 12, 13, 14, 15, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 33, 34} {
		code := op
		_ = vm.put(code, tsFunction(func(args []any) (any, error) { return vm.instruction(code, args) }))
	}
	_ = vm.put(9, queue)
	_ = vm.put(10, "window")
	_ = vm.put(16, p)
	if err := vm.run(); err != nil {
		if errors.Is(err, errVMLimit) {
			return "", failure("turnstile_execution_limit", "turnstile")
		}
		return "", err
	}
	if vm.result == "" {
		return "", failure("turnstile_unsolved", "turnstile")
	}
	return vm.result, nil
}
