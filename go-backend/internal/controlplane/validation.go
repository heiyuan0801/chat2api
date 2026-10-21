package controlplane

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

//go:embed stage4_spec.json
var stage4SpecBytes []byte
var stage4Models = func() map[string]Map {
	var v struct {
		Models map[string]Map `json:"models"`
	}
	if json.Unmarshal(stage4SpecBytes, &v) != nil {
		panic("invalid management contracts")
	}
	return v.Models
}()

type ValidationError struct{ Details []Map }

func (v *ValidationError) Error() string { return "request validation failed" }
func validationIssue(kind, msg string, loc []any, input any, ctx Map) Map {
	out := Map{"type": kind, "loc": loc, "msg": msg, "input": input}
	if len(ctx) > 0 {
		out["ctx"] = ctx
	}
	return out
}
func appendLoc(loc []any, key any) []any { return append(append([]any(nil), loc...), key) }
func secretLoc(loc []any) bool {
	for _, v := range loc {
		s := strings.ToLower(text(v))
		if strings.Contains(s, "token") || s == "key" || strings.Contains(s, "password") || s == "api_key" || s == "authorization" {
			return true
		}
	}
	return false
}
func validateSchema(value any, schema, defs Map, loc []any) (any, []Map) {
	if ref := text(schema["$ref"]); ref != "" {
		schema = object(defs[strings.TrimPrefix(ref, "#/$defs/")])
	}
	issues := []Map{}
	fail := func(kind, msg string, ctx Map) (any, []Map) {
		input := value
		if secretLoc(loc) {
			input = "[credential]"
		}
		return value, []Map{validationIssue(kind, msg, loc, input, ctx)}
	}
	if choices := list(schema["anyOf"]); len(choices) > 0 {
		var all []Map
		nullable := false
		nonNull := []any{}
		for _, branch := range choices {
			if object(branch)["type"] == "null" {
				nullable = true
			} else {
				nonNull = append(nonNull, branch)
			}
		}
		if nullable {
			if value == nil {
				return nil, nil
			}
			choices = nonNull
		}
		for _, branch := range choices {
			v, errs := validateSchema(value, object(branch), defs, loc)
			if len(errs) == 0 {
				return v, nil
			}
			all = append(all, errs...)
		}
		return value, all
	}
	if choices := list(schema["enum"]); len(choices) > 0 {
		valid := false
		for _, v := range choices {
			if canonical(v) == canonical(value) {
				valid = true
			}
		}
		if !valid {
			labels := []string{}
			for _, v := range choices {
				labels = append(labels, "'"+text(v)+"'")
			}
			expected := ""
			if len(labels) == 1 {
				expected = labels[0]
			} else {
				expected = strings.Join(labels[:len(labels)-1], ", ") + " or " + labels[len(labels)-1]
			}
			return fail("literal_error", "Input should be "+expected, Map{"expected": expected})
		}
	}
	switch text(schema["type"]) {
	case "null":
		if value != nil {
			return fail("none_required", "Input should be None", nil)
		}
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return fail("model_attributes_type", "Input should be a valid dictionary or object to extract fields from", nil)
		}
		out := Map{}
		properties := object(schema["properties"])
		required := map[string]bool{}
		for _, key := range stringsOf(schema["required"]) {
			required[key] = true
		}
		order := stringsOf(schema["x-field-order"])
		if len(order) == 0 {
			for key := range properties {
				order = append(order, key)
			}
			sort.Strings(order)
		}
		for _, key := range order {
			field, known := properties[key]
			if !known {
				continue
			}
			v, present := m[key]
			if !present {
				if required[key] {
					issues = append(issues, validationIssue("missing", "Field required", appendLoc(loc, key), value, nil))
				}
				continue
			}
			next, errs := validateSchema(v, object(field), defs, appendLoc(loc, key))
			out[key] = next
			issues = append(issues, errs...)
		}
		extras := []string{}
		for key := range m {
			if _, known := properties[key]; !known {
				extras = append(extras, key)
			}
		}
		sort.Strings(extras)
		for _, key := range extras {
			v := m[key]
			if schema["additionalProperties"] == false {
				input := v
				if secretLoc(appendLoc(loc, key)) {
					input = "[credential]"
				}
				issues = append(issues, validationIssue("extra_forbidden", "Extra inputs are not permitted", appendLoc(loc, key), input, nil))
			} else if schema["additionalProperties"] != nil {
				out[key] = v
			}
		}
		return out, issues
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fail("list_type", "Input should be a valid list", nil)
		}
		if n, ok := number(schema["maxItems"]); ok && len(array) > int(n) {
			return fail("too_long", fmt.Sprintf("List should have at most %d items after validation, not %d", int(n), len(array)), Map{"field_type": "List", "max_length": int(n), "actual_length": len(array)})
		}
		out := []any{}
		for i, v := range array {
			next, errs := validateSchema(v, object(schema["items"]), defs, appendLoc(loc, i))
			out = append(out, next)
			issues = append(issues, errs...)
		}
		return out, issues
	case "string":
		s, ok := value.(string)
		if !ok {
			return fail("string_type", "Input should be a valid string", nil)
		}
		length := utf8.RuneCountInString(s)
		if n, ok := number(schema["minLength"]); ok && length < int(n) {
			suffix := "characters"
			if int(n) == 1 {
				suffix = "character"
			}
			return fail("string_too_short", fmt.Sprintf("String should have at least %d %s", int(n), suffix), Map{"min_length": int(n)})
		}
		if n, ok := number(schema["maxLength"]); ok && length > int(n) {
			return fail("string_too_long", fmt.Sprintf("String should have at most %d characters", int(n)), Map{"max_length": int(n)})
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			if s, ok := value.(string); ok {
				switch strings.ToLower(s) {
				case "1", "true", "t", "yes", "y", "on":
					value = true
				case "0", "false", "f", "no", "n", "off":
					value = false
				default:
					return fail("bool_parsing", "Input should be a valid boolean, unable to interpret input", nil)
				}
			} else if f, ok := number(value); ok && (f == 0 || f == 1) {
				value = f == 1
			} else {
				return fail("bool_type", "Input should be a valid boolean", nil)
			}
		}
	case "integer", "number":
		f, ok := number(value)
		if !ok {
			kind, msg := "float_parsing", "Input should be a valid number, unable to parse string as a number"
			if schema["type"] == "integer" {
				kind, msg = "int_parsing", "Input should be a valid integer, unable to parse string as an integer"
			}
			if _, isString := value.(string); !isString {
				if schema["type"] == "integer" {
					kind, msg = "int_type", "Input should be a valid integer"
				} else {
					kind, msg = "float_type", "Input should be a valid number"
				}
			}
			return fail(kind, msg, nil)
		}
		if schema["type"] == "integer" {
			if math.Trunc(f) != f {
				return fail("int_from_float", "Input should be a valid integer, got a number with a fractional part", nil)
			}
			if f > math.MaxInt64 || f < math.MinInt64 {
				return fail("int_parsing_size", "Unable to parse input string as an integer, exceeded maximum size", nil)
			}
			raw := text(value)
			if n, e := strconv.ParseInt(raw, 10, 64); e == nil {
				value = json.Number(strconv.FormatInt(n, 10))
			} else {
				value = json.Number(strconv.FormatFloat(f, 'f', 0, 64))
			}
		} else {
			value = json.Number(canonical(f))
		}
		if n, ok := number(schema["minimum"]); ok && f < n {
			return fail("greater_than_equal", "Input should be greater than or equal to "+text(schema["minimum"]), Map{"ge": schema["minimum"]})
		}
		if n, ok := number(schema["maximum"]); ok && f > n {
			return fail("less_than_equal", "Input should be less than or equal to "+text(schema["maximum"]), Map{"le": schema["maximum"]})
		}
	}
	return value, issues
}
func Validate(model string, body any) (Map, error) {
	schema, ok := baseline.Models[model]
	if !ok {
		schema, ok = stage4Models[model]
	}
	if !ok {
		return nil, problem(500, "request contract unavailable")
	}
	value, errs := validateSchema(body, schema, object(schema["$defs"]), []any{"body"})
	if len(errs) > 0 {
		return nil, &ValidationError{errs}
	}
	m := object(value)
	if model == "ProxyDefaultsRequest" {
		for _, name := range []string{"default_reference", "fallback_reference"} {
			if m[name] == nil {
				continue
			}
			ref := object(m[name])
			mode := text(ref["mode"])
			gid, u := text(ref["group_id"]), text(ref["url"])
			msg := ""
			switch mode {
			case "direct":
				if gid != "" || u != "" {
					msg = "direct proxy reference cannot include group_id or url"
				}
			case "group":
				if gid == "" {
					msg = "group proxy reference requires group_id"
				} else if u != "" {
					msg = "group proxy reference cannot include url"
				}
			case "custom":
				if gid != "" {
					msg = "custom proxy reference cannot include group_id"
				} else if u == "" {
					msg = "custom proxy reference requires url"
				} else if strings.EqualFold(u, "direct") || strings.EqualFold(u, "global") || strings.HasPrefix(strings.ToLower(u), "group:") {
					msg = "custom proxy reference uses a reserved value"
				}
			}
			if msg != "" {
				return nil, &ValidationError{[]Map{validationIssue("value_error", "Value error, "+msg, []any{"body", name}, ref, Map{"error": Map{}})}}
			}
		}
	}
	if model == "SettingsPatch" {
		raw := text(m["revision"])
		if raw == "" {
			return nil, &ValidationError{[]Map{validationIssue("value_error", "Value error, revision must not be blank", []any{"body", "revision"}, m["revision"], Map{"error": Map{}})}}
		}
		m["revision"] = raw
	}
	return m, nil
}
