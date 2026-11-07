package model

import (
	"sort"
	"strings"

	"nginx-sitemap/internal/conf"
)

// Expr is the partially evaluated value of an nginx string expression.
//
//	lit    — a literal string
//	sym    — an unknown value (Var + Reason)
//	concat — concatenation of Parts
//	switch — a branch table: first matching case wins, else Default
type Expr struct {
	Kind    string    `json:"kind"`
	Value   string    `json:"value,omitempty"`
	Var     string    `json:"var,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Parts   []*Expr   `json:"parts,omitempty"`
	On      *Expr     `json:"on,omitempty"`
	Table   string    `json:"table,omitempty"` // e.g. "map $http_host $backend"
	Cases   []Case    `json:"cases,omitempty"`
	Default *Expr     `json:"default,omitempty"`
	Src     *conf.Src `json:"src,omitempty"`
	// HadVars is true when the original text contained variables (used to
	// distinguish "static" from "resolved").
	HadVars bool `json:"had_vars,omitempty"`
}

type Case struct {
	Match Matcher `json:"match"`
	Value *Expr   `json:"value"`
}

// Matcher kinds: exact | leading-wildcard | trailing-wildcard | regex |
// cidr | range | percent | listen | if
type Matcher struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	CI      bool   `json:"ci,omitempty"`
}

func Lit(s string) *Expr { return &Expr{Kind: "lit", Value: s} }

func Sym(v, reason, detail string) *Expr {
	return &Expr{Kind: "sym", Var: v, Reason: reason, Detail: detail}
}

// IsLit reports whether e is a single literal.
func (e *Expr) IsLit() bool { return e != nil && e.Kind == "lit" }

// Status derives the resolution status.
func (e *Expr) Status() string {
	if e == nil {
		return StatusStatic
	}
	hasSym, hasSwitch := false, false
	e.walk(func(x *Expr) {
		switch x.Kind {
		case "sym":
			hasSym = true
		case "switch":
			hasSwitch = true
		}
	})
	switch {
	case hasSym:
		return StatusDynamic
	case hasSwitch:
		return StatusConditional
	case e.HadVars:
		return StatusResolved
	default:
		return StatusStatic
	}
}

// Reasons collects distinct reasons of unknown parts.
func (e *Expr) Reasons() []Reason {
	if e == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Reason
	e.walk(func(x *Expr) {
		if x.Kind == "sym" {
			k := x.Reason + "|" + x.Var
			if !seen[k] {
				seen[k] = true
				d := x.Var
				if x.Detail != "" {
					d += ": " + x.Detail
				}
				out = append(out, Reason{Code: x.Reason, Detail: d})
			}
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func (e *Expr) walk(f func(*Expr)) {
	if e == nil {
		return
	}
	f(e)
	for _, p := range e.Parts {
		p.walk(f)
	}
	// The switch subject only matters through the chosen branch; an unknown
	// subject with all-literal leaves is "conditional", not "dynamic".
	for _, c := range e.Cases {
		c.Value.walk(f)
	}
	e.Default.walk(f)
}

// Size counts nodes (used for the branch limit).
func (e *Expr) Size() int {
	n := 0
	e.walk(func(*Expr) { n++ })
	return n
}

// Leaf is one concrete outcome of an expression.
type Leaf struct {
	When  []string // conditions leading to this outcome
	Value *Expr    // a literal, or a concat containing syms
}

// Leaves enumerates outcomes (cartesian over switches), up to limit.
// ok is false when the limit was hit.
func (e *Expr) Leaves(limit int) (out []Leaf, ok bool) {
	ok = true
	var rec func(x *Expr) []Leaf
	rec = func(x *Expr) []Leaf {
		if !ok {
			return nil
		}
		switch x.Kind {
		case "lit", "sym":
			return []Leaf{{Value: x}}
		case "switch":
			var res []Leaf
			subj := x.On.String()
			if x.Table != "" {
				subj = x.Table
			}
			for _, c := range x.Cases {
				for _, l := range rec(c.Value) {
					l.When = append([]string{subj + " " + c.Match.String()}, l.When...)
					res = append(res, l)
				}
			}
			if x.Default != nil {
				for _, l := range rec(x.Default) {
					l.When = append([]string{subj + " default"}, l.When...)
					res = append(res, l)
				}
			}
			if len(res) > limit {
				ok = false
			}
			return res
		case "concat":
			acc := []Leaf{{}}
			for _, p := range x.Parts {
				sub := rec(p)
				var next []Leaf
				for _, a := range acc {
					for _, b := range sub {
						next = append(next, Leaf{
							When:  append(append([]string(nil), a.When...), b.When...),
							Value: concat2(a.Value, b.Value),
						})
						if len(next) > limit {
							ok = false
							return nil
						}
					}
				}
				acc = next
			}
			return acc
		}
		return nil
	}
	out = rec(e)
	return out, ok
}

func concat2(a, b *Expr) *Expr {
	if a == nil {
		return b
	}
	if a.IsLit() && b.IsLit() {
		return Lit(a.Value + b.Value)
	}
	var parts []*Expr
	if a.Kind == "concat" {
		parts = append(parts, a.Parts...)
	} else {
		parts = append(parts, a)
	}
	parts = append(parts, b)
	return &Expr{Kind: "concat", Parts: parts}
}

func (m Matcher) String() string {
	switch m.Kind {
	case "exact":
		return "= " + quote(m.Pattern)
	case "regex":
		if m.CI {
			return "~* " + m.Pattern
		}
		return "~ " + m.Pattern
	case "if":
		return "if " + m.Pattern
	default:
		return m.Kind + " " + m.Pattern
	}
}

func quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t") {
		return "\"" + s + "\""
	}
	return s
}

// String renders the expression in a compact, human-readable form.
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case "lit":
		return e.Value
	case "sym":
		return "${" + strings.TrimPrefix(e.Var, "$") + "}"
	case "concat":
		var b strings.Builder
		for _, p := range e.Parts {
			b.WriteString(p.String())
		}
		return b.String()
	case "switch":
		var b strings.Builder
		b.WriteString("⟨")
		if e.Table != "" {
			b.WriteString(e.Table)
		} else {
			b.WriteString(e.On.String())
		}
		b.WriteString(": ")
		for i, c := range e.Cases {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(c.Match.String())
			b.WriteString(" → ")
			b.WriteString(c.Value.String())
		}
		if e.Default != nil {
			b.WriteString(" | default → ")
			b.WriteString(e.Default.String())
		}
		b.WriteString("⟩")
		return b.String()
	}
	return ""
}
