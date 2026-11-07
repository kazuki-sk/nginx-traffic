// Package schemacheck is a minimal JSON Schema validator covering the
// subset used by schema/map.schema.json ($ref, type, properties, required,
// additionalProperties:false, items, enum, anyOf). Used by tests only.
package schemacheck

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Validator struct {
	root map[string]any
	errs []string
}

func Validate(schemaJSON, doc []byte) []string {
	var s map[string]any
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return []string{"schema: " + err.Error()}
	}
	var d any
	if err := json.Unmarshal(doc, &d); err != nil {
		return []string{"doc: " + err.Error()}
	}
	v := &Validator{root: s}
	v.check(s, d, "$")
	return v.errs
}

func (v *Validator) resolve(s map[string]any) map[string]any {
	for {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		name := strings.TrimPrefix(ref, "#/$defs/")
		s = v.root["$defs"].(map[string]any)[name].(map[string]any)
	}
}

func typeOf(d any) string {
	switch x := d.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "?"
}

func (v *Validator) ok(s map[string]any, d any) bool {
	sub := &Validator{root: v.root}
	sub.check(s, d, "")
	return len(sub.errs) == 0
}

func (v *Validator) check(s map[string]any, d any, path string) {
	s = v.resolve(s)
	if alts, ok := s["anyOf"].([]any); ok {
		for _, alt := range alts {
			if v.ok(alt.(map[string]any), d) {
				return
			}
		}
		// Report the errors of the first non-null alternative.
		for _, alt := range alts {
			if m := alt.(map[string]any); m["type"] != "null" {
				v.check(m, d, path)
				return
			}
		}
		v.errs = append(v.errs, fmt.Sprintf("%s: matches no alternative (%s)", path, typeOf(d)))
		return
	}
	if t, ok := s["type"].(string); ok {
		got := typeOf(d)
		if !(got == t || (t == "number" && got == "integer")) {
			v.errs = append(v.errs, fmt.Sprintf("%s: type %s, want %s", path, got, t))
			return
		}
	}
	if en, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if e == d {
				found = true
			}
		}
		if !found {
			v.errs = append(v.errs, fmt.Sprintf("%s: %v not in enum", path, d))
		}
	}
	switch x := d.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range asList(s["required"]) {
			if _, ok := x[r]; !ok {
				v.errs = append(v.errs, fmt.Sprintf("%s: missing required %q", path, r))
			}
		}
		for k, val := range x {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if ap, ok := s["additionalProperties"].(bool); ok && !ap {
					v.errs = append(v.errs, fmt.Sprintf("%s: unexpected property %q", path, k))
				}
				continue
			}
			v.check(ps, val, path+"."+k)
		}
	case []any:
		if it, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				v.check(it, e, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
}

func asList(x any) []string {
	l, _ := x.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		out = append(out, e.(string))
	}
	return out
}
