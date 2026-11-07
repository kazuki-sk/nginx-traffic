package model

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Enumerations exposed in the schema.
var schemaEnums = map[string][]string{
	"status":               {StatusStatic, StatusResolved, StatusConditional, StatusDynamic, StatusDeclared},
	"origin":               {OriginNginx, OriginManual},
	"Expr.kind":            {"lit", "sym", "concat", "switch"},
	"Listen.proto":         {"http", "stream-tcp", "stream-udp"},
	"Listen.family":        {"inet", "inet6", "unix", "name"},
	"Location.modifier":    {"=", "^~", "~", "~*", "prefix", "@"},
	"Location.match_group": {"exact", "prefix-noregex", "prefix", "regex", "named"},
	"Upstream.kind":        {"explicit", "implicit"},
	"Table.kind":           {"map", "geo", "split_clients"},
	"DNSInfo.mode":         {"names", "snapshot"},
}

var schemaDescriptions = map[string]string{
	"Map":        "nginx-sitemap output. Every element carries src (file:line), origin and status. Listens reference servers by id; handlers reference upstreams by name.",
	"Meta":       "",
	"Expr":       "Partially evaluated nginx string expression. lit: literal; sym: unknown value (var + reason); concat: parts; switch: branch table (first matching case, else default).",
	"Listen":     "One address:port pair with the servers bound to it, its default server and the server_name selection order.",
	"Location":   "location block; match_group/order express the nginx search order.",
	"Edge":       "Internal redirect / transition (rewrite, error_page, try_files fallback, conditional return/redirect).",
	"External":   "Manually declared service that does not go through nginx (origin=manual, status=declared).",
	"Annotation": "Manual note attached to an nginx-derived element; never changes nginx-derived facts.",
}

// Schema returns the JSON Schema (draft 2020-12) of Map, derived from the
// Go types so it cannot drift from the implementation.
func Schema() []byte {
	g := &schemaGen{defs: map[string]any{}}
	root := g.typ(reflect.TypeOf(Map{}), "")
	s := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         "https://example.invalid/nginx-sitemap/map.schema.json",
		"title":       "nginx-sitemap map.json",
		"description": schemaDescriptions["Map"],
		"$defs":       g.defs,
	}
	for k, v := range root.(map[string]any) {
		s[k] = v
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

type schemaGen struct{ defs map[string]any }

func (g *schemaGen) typ(t reflect.Type, field string) any {
	switch t.Kind() {
	case reflect.Ptr:
		inner := g.typ(t.Elem(), field)
		return map[string]any{"anyOf": []any{inner, map[string]any{"type": "null"}}}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": g.typ(t.Elem(), field)}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		name := t.Name()
		if name == "Src" {
			return map[string]any{"type": "string", "description": "origin \"file:line\" (file relative to the conf prefix when inside it)"}
		}
		if _, ok := g.defs[name]; !ok {
			g.defs[name] = nil // reserve (recursion)
			g.defs[name] = g.object(t)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	}
	return map[string]any{}
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var req []string
	var addFields func(t reflect.Type)
	addFields = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if f.Anonymous && tag == "" {
				addFields(f.Type)
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" {
				name = f.Name
			}
			p := g.typ(f.Type, name)
			if en, ok := schemaEnums[t.Name()+"."+name]; ok {
				p = map[string]any{"type": "string", "enum": en}
			} else if en, ok := schemaEnums[name]; ok && f.Type.Kind() == reflect.String {
				p = map[string]any{"type": "string", "enum": en}
			}
			props[name] = p
			if !strings.Contains(opts, "omitempty") {
				req = append(req, name)
			}
		}
	}
	addFields(t)
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		o["required"] = req
	}
	if d := schemaDescriptions[t.Name()]; d != "" {
		o["description"] = d
	}
	return o
}
