package analyze

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

// BranchLimit bounds the size of an expression tree; larger ones degrade to
// dynamic(branch-limit) to keep memory/time bounded.
const BranchLimit = 256

// segment of an nginx complex value: literal text or a variable reference.
type segment struct {
	lit string
	v   string // variable name without '$' ("" for literal)
}

// parseValue splits an nginx string into literal and variable segments.
//
// Syntax (src/http/ngx_http_script.c ngx_http_script_compile):
// "$" followed by a digit 1-9 is a single-digit regex capture; "${name}" is
// a braced name; otherwise the name is [A-Za-z0-9_]+. A '$' not followed by
// a valid name is literal.
func parseValue(s string) []segment {
	var out []segment
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, segment{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' || i+1 >= len(s) {
			lit.WriteByte(c)
			continue
		}
		n := s[i+1]
		switch {
		case n >= '1' && n <= '9':
			flush()
			out = append(out, segment{v: string(n)})
			i++
		case n == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				lit.WriteByte(c)
				continue
			}
			flush()
			out = append(out, segment{v: s[i+2 : i+2+end]})
			i += 2 + end
		case isNameChar(n):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			flush()
			out = append(out, segment{v: s[i+1 : j]})
			i = j - 1
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	return out
}

func isNameChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func hasVars(s string) bool {
	for _, sg := range parseValue(s) {
		if sg.v != "" {
			return true
		}
	}
	return false
}

// tableDef is a map / geo / split_clients definition.
type tableDef struct {
	t       *model.Table
	src     string // source expression text
	entries []tableEntry
	def     *tableEntry
}

type tableEntry struct {
	m     model.Matcher
	value string
	re    *regexp.Regexp
	reErr bool
	pfx   netip.Prefix
	src   conf.Src
}

// scope supplies context-dependent built-in variables.
type scope interface {
	builtin(name string) *model.Expr
}

type evaluator struct {
	tables   map[string]*tableDef
	sc       scope
	env      map[string]*model.Expr
	setVars  map[string]bool // variables assigned by `set` anywhere in this module
	captures map[string]bool // named captures available in this context
	visiting map[string]bool
}

func (ev *evaluator) clone() *evaluator {
	n := *ev
	n.env = make(map[string]*model.Expr, len(ev.env))
	for k, v := range ev.env {
		n.env[k] = v
	}
	n.visiting = map[string]bool{}
	return &n
}

// eval evaluates an nginx string expression.
func (ev *evaluator) eval(s string) *model.Expr {
	segs := parseValue(s)
	var parts []*model.Expr
	had := false
	for _, sg := range segs {
		if sg.v == "" {
			parts = append(parts, model.Lit(sg.lit))
			continue
		}
		had = true
		parts = append(parts, ev.resolve(sg.v))
	}
	e := simplifyConcat(parts)
	if had {
		e = withHadVars(e)
	}
	if e.Size() > BranchLimit {
		return &model.Expr{Kind: "sym", Var: "$" + strings.TrimPrefix(s, "$"), Reason: model.ReasonBranchLimit,
			Detail: "expression expands to more than 256 nodes", HadVars: true}
	}
	return e
}

func withHadVars(e *model.Expr) *model.Expr {
	c := *e
	c.HadVars = true
	return &c
}

func simplifyConcat(parts []*model.Expr) *model.Expr {
	var flat []*model.Expr
	for _, p := range parts {
		if p.Kind == "concat" {
			flat = append(flat, p.Parts...)
		} else {
			flat = append(flat, p)
		}
	}
	var merged []*model.Expr
	for _, p := range flat {
		if p.IsLit() && len(merged) > 0 && merged[len(merged)-1].IsLit() {
			merged[len(merged)-1] = model.Lit(merged[len(merged)-1].Value + p.Value)
			continue
		}
		if p.IsLit() {
			p = model.Lit(p.Value) // drop HadVars on parts
		}
		merged = append(merged, p)
	}
	switch len(merged) {
	case 0:
		return model.Lit("")
	case 1:
		return merged[0]
	}
	return &model.Expr{Kind: "concat", Parts: merged}
}

func (ev *evaluator) resolve(name string) *model.Expr {
	if len(name) == 1 && name[0] >= '1' && name[0] <= '9' {
		return model.Sym("$"+name, model.ReasonRequest, "regex capture")
	}
	if v, ok := ev.env[name]; ok {
		return v
	}
	if ev.captures[name] {
		return model.Sym("$"+name, model.ReasonRequest, "named regex capture")
	}
	if t, ok := ev.tables[name]; ok {
		if ev.visiting[name] {
			return model.Sym("$"+name, model.ReasonCycle, "variable refers to itself through "+t.t.Kind)
		}
		ev.visiting[name] = true
		defer delete(ev.visiting, name)
		return ev.evalTable(t)
	}
	if ev.setVars[name] {
		// Assigned by `set` somewhere, but not on the path evaluated here.
		// Variables survive internal redirects (r->variables is kept by
		// ngx_http_internal_redirect), so the value depends on the path.
		return model.Sym("$"+name, model.ReasonCondition, "assigned by set on another execution path")
	}
	if ev.sc != nil {
		if e := ev.sc.builtin(name); e != nil {
			return e
		}
	}
	return genericVar(name)
}

func (ev *evaluator) evalTable(t *tableDef) *model.Expr {
	srcE := ev.eval(t.src)
	// nginx detects such loops at run time ("cycle while evaluating
	// variable", src ngx_http_get_indexed_variable) and the value is
	// invalid; propagate instead of hiding the cycle in a switch subject.
	if containsReason(srcE, model.ReasonCycle) {
		return model.Sym("$"+t.t.Var, model.ReasonCycle, "source "+t.src+" depends on $"+t.t.Var)
	}
	e := ev.through(t, srcE)
	if e.Size() > BranchLimit {
		return model.Sym("$"+t.t.Var, model.ReasonBranchLimit, t.t.Kind+" "+t.src+" $"+t.t.Var)
	}
	return e
}

// through applies table t to the (partially known) source value srcE.
// When srcE is itself a branch table (e.g. the result of another map —
// a "map chain"), t is pushed into every branch, so the result is
// expressed in terms of the original request condition:
//
//	map $host $tenant {a.example → alpha}; map $tenant $pool {alpha → pool_a}
//	$pool = ⟨map $host $tenant → $pool: = a.example → pool_a | default → …⟩
func (ev *evaluator) through(t *tableDef, srcE *model.Expr) *model.Expr {
	switch srcE.Kind {
	case "lit":
		if e, ok := ev.pick(t, srcE.Value); ok {
			return withHadVars(e)
		}
	case "switch":
		out := &model.Expr{Kind: "switch", On: srcE.On, Table: srcE.Table + " → $" + t.t.Var, Src: srcE.Src, HadVars: true}
		for _, c := range srcE.Cases {
			out.Cases = append(out.Cases, model.Case{Match: c.Match, Value: ev.through(t, c.Value)})
		}
		if srcE.Default != nil {
			out.Default = ev.through(t, srcE.Default)
		}
		return out
	}
	return ev.tableSwitch(t, srcE)
}

func (ev *evaluator) tableSwitch(t *tableDef, srcE *model.Expr) *model.Expr {
	label := t.t.Kind + " " + t.src + " $" + t.t.Var
	sw := &model.Expr{Kind: "switch", On: srcE, Table: label, Src: t.t.Src}
	for _, en := range t.entries {
		sw.Cases = append(sw.Cases, model.Case{Match: en.m, Value: ev.eval(en.value)})
	}
	if t.def != nil {
		sw.Default = ev.eval(t.def.value)
	} else {
		sw.Default = model.Lit("")
	}
	return sw
}

// pick selects the table entry for a known source value. ok=false means
// the choice cannot be made statically (e.g. PCRE-only regex).
func (ev *evaluator) pick(t *tableDef, v string) (*model.Expr, bool) {
	switch t.t.Kind {
	case "map":
		return ev.pickMap(t, v)
	case "geo":
		return ev.pickGeo(t, v)
	}
	return nil, false
}

// pickMap follows ngx_http_map_find (src/http/ngx_http_variables.c):
// the value is lower-cased and looked up in the combined hash (exact, then
// longest leading wildcard, then longest trailing wildcard — wildcards only
// with "hostnames"), then regexes are tried in order of appearance (only
// if the value is non-empty), then the default.
func (ev *evaluator) pickMap(t *tableDef, v string) (*model.Expr, bool) {
	low := strings.ToLower(v)
	if t.t.Hostnames {
		low = strings.TrimSuffix(low, ".")
	}
	for _, en := range t.entries {
		if en.m.Kind == "exact" && strings.ToLower(en.m.Pattern) == low {
			return ev.eval(en.value), true
		}
	}
	if t.t.Hostnames {
		if en := bestWildcard(t.entries, low, "leading-wildcard"); en != nil {
			return ev.eval(en.value), true
		}
		if en := bestWildcard(t.entries, low, "trailing-wildcard"); en != nil {
			return ev.eval(en.value), true
		}
	}
	if v != "" {
		for _, en := range t.entries {
			if en.m.Kind != "regex" {
				continue
			}
			if en.re == nil {
				return nil, false // cannot evaluate PCRE-only pattern
			}
			if en.re.MatchString(v) {
				return ev.eval(en.value), true
			}
		}
	}
	if t.def != nil {
		return ev.eval(t.def.value), true
	}
	return model.Lit(""), true
}

func bestWildcard(es []tableEntry, host, kind string) *tableEntry {
	var best *tableEntry
	bestLen := -1
	for i := range es {
		en := &es[i]
		if en.m.Kind != kind {
			continue
		}
		if wildcardMatch(kind, strings.ToLower(en.m.Pattern), host) && len(en.m.Pattern) > bestLen {
			best, bestLen = en, len(en.m.Pattern)
		}
	}
	return best
}

// wildcardMatch: "*.example.com" / ".example.com" (leading) and "www.*" (trailing).
// ".example.com" also matches "example.com" itself (ngx_hash_add_key
// NGX_HASH_WILDCARD_KEY semantics as documented for server_name).
func wildcardMatch(kind, pat, host string) bool {
	switch kind {
	case "leading-wildcard":
		if strings.HasPrefix(pat, "*.") {
			return strings.HasSuffix(host, pat[1:]) && len(host) > len(pat)-1
		}
		if strings.HasPrefix(pat, ".") {
			return host == pat[1:] || strings.HasSuffix(host, pat)
		}
	case "trailing-wildcard":
		if strings.HasSuffix(pat, ".*") {
			return strings.HasPrefix(host, pat[:len(pat)-1]) && len(host) > len(pat)-1
		}
	}
	return false
}

// pickGeo: the geo module stores CIDRs in a radix tree, so the most
// specific (longest) prefix wins (ngx_http_geo_cidr_value / ngx_radix32tree_find).
func (ev *evaluator) pickGeo(t *tableDef, v string) (*model.Expr, bool) {
	addr, err := netip.ParseAddr(v)
	if err != nil {
		return nil, false
	}
	var best *tableEntry
	for i := range t.entries {
		en := &t.entries[i]
		if en.m.Kind != "cidr" || !en.pfx.IsValid() {
			if en.m.Kind == "range" {
				return nil, false
			}
			continue
		}
		if en.pfx.Contains(addr) && (best == nil || en.pfx.Bits() > best.pfx.Bits()) {
			best = en
		}
	}
	if best != nil {
		return ev.eval(best.value), true
	}
	if t.def != nil {
		return ev.eval(t.def.value), true
	}
	return model.Lit(""), true
}

// compileRegex converts an nginx (PCRE) regex into Go RE2 if possible.
func compileRegex(pat string, ci bool) (*regexp.Regexp, bool) {
	p := pat
	if ci {
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, false
	}
	return re, true
}

// namedCaptures returns the (?<name>...) / (?P<name>...) groups of a regex.
func namedCaptures(pat string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`\(\?P?<([A-Za-z_][A-Za-z0-9_]*)>`).FindAllStringSubmatch(pat, -1) {
		out = append(out, m[1])
	}
	return out
}

// Generic classification of variables that are not context-determined.
// Reference: nginx docs "Alphabetical index of variables"
// (https://nginx.org/en/docs/varindex.html) and the variable tables in
// src/http/ngx_http_variables.c (ngx_http_core_variables).
var (
	requestPrefixes = []string{"http_", "cookie_", "arg_", "sent_http_", "sent_trailer_",
		"upstream_", "jwt_", "invalid_referer", "secure_link", "limit_", "uid_"}
	connPrefixes = []string{"ssl_", "proxy_protocol_", "tcpinfo_", "realip_"}
	requestVars  = set("host", "uri", "document_uri", "request_uri", "args", "query_string", "is_args",
		"request", "request_method", "request_body", "request_body_file", "request_length",
		"request_time", "request_id", "request_completion", "request_filename", "content_type",
		"content_length", "remote_user", "status", "bytes_sent", "body_bytes_sent", "time_local",
		"time_iso8601", "msec", "server_protocol", "gzip_ratio", "date_local", "date_gmt",
		"fastcgi_script_name", "fastcgi_path_info", "proxy_add_x_forwarded_for", "cookie",
		"modern_browser", "ancient_browser", "msie", "geoip_country_code", "http2", "http3",
		"proxy_host", "proxy_port", "limit_rate", "pipe", "session_time", "bytes_received",
		"upstream_addr", "spdy")
	connVars = set("remote_addr", "remote_port", "binary_remote_addr", "connection",
		"connection_requests", "connection_time", "server_addr", "server_port", "https", "scheme",
		"protocol", "ssl_preread_server_name", "ssl_preread_protocol", "ssl_preread_alpn_protocols")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func genericVar(name string) *model.Expr {
	lname := strings.ToLower(name)
	switch lname {
	case "hostname", "pid":
		return model.Sym("$"+name, model.ReasonHost, "value of the nginx host/process at run time")
	case "realpath_root":
		return model.Sym("$"+name, model.ReasonFilesystem, "symlinks resolved at run time")
	}
	if requestVars[lname] {
		return model.Sym("$"+name, model.ReasonRequest, "")
	}
	if connVars[lname] {
		return model.Sym("$"+name, model.ReasonConnection, "")
	}
	for _, p := range requestPrefixes {
		if strings.HasPrefix(lname, p) {
			return model.Sym("$"+name, model.ReasonRequest, "")
		}
	}
	for _, p := range connPrefixes {
		if strings.HasPrefix(lname, p) {
			return model.Sym("$"+name, model.ReasonConnection, "")
		}
	}
	return model.Sym("$"+name, model.ReasonUnknownVar, "not a known built-in, map, geo, split_clients or set variable")
}

// ---- table construction ----

// buildTable parses map / geo / split_clients blocks.
//
//	map:  https://nginx.org/en/docs/http/ngx_http_map_module.html
//	geo:  https://nginx.org/en/docs/http/ngx_http_geo_module.html
//	split_clients: https://nginx.org/en/docs/http/ngx_http_split_clients_module.html
func buildTable(n *conf.Node, a *analyzer) *tableDef {
	src := n.Src
	t := &model.Table{Meta: model.Meta{Src: &src, Origin: model.OriginNginx, Status: model.StatusStatic}, Kind: n.Name}
	td := &tableDef{t: t}
	switch n.Name {
	case "map":
		if len(n.Args) != 2 {
			a.errorf("parse-error", n.Src, "map requires 2 parameters")
			return nil
		}
		td.src = n.Args[0]
		t.Var = strings.TrimPrefix(n.Args[1], "$")
	case "geo":
		switch len(n.Args) {
		case 1:
			td.src = "$remote_addr"
			t.Var = strings.TrimPrefix(n.Args[0], "$")
		case 2:
			td.src = n.Args[0]
			t.Var = strings.TrimPrefix(n.Args[1], "$")
		default:
			a.errorf("parse-error", n.Src, "geo requires 1 or 2 parameters")
			return nil
		}
	case "split_clients":
		if len(n.Args) != 2 {
			a.errorf("parse-error", n.Src, "split_clients requires 2 parameters")
			return nil
		}
		td.src = n.Args[0]
		t.Var = strings.TrimPrefix(n.Args[1], "$")
	}
	t.Source = td.src
	geoRanges := false
	for _, c := range n.Block {
		csrc := c.Src
		key := c.Name
		val := c.Arg(0)
		switch n.Name {
		case "map":
			switch {
			case key == "default" && len(c.Args) == 1:
				td.def = &tableEntry{m: model.Matcher{Kind: "default"}, value: val, src: csrc}
				continue
			case key == "hostnames" && len(c.Args) == 0:
				t.Hostnames = true
				continue
			case key == "volatile" && len(c.Args) == 0:
				t.Volatile = true
				continue
			}
			en := tableEntry{value: val, src: csrc}
			switch {
			case strings.HasPrefix(key, "~*"):
				en.m = model.Matcher{Kind: "regex", Pattern: key[2:], CI: true}
			case strings.HasPrefix(key, "~"):
				en.m = model.Matcher{Kind: "regex", Pattern: key[1:]}
			case strings.HasPrefix(key, "\\"):
				en.m = model.Matcher{Kind: "exact", Pattern: key[1:]}
			default:
				en.m = model.Matcher{Kind: "exact", Pattern: key}
			}
			td.entries = append(td.entries, en)
		case "geo":
			switch key {
			case "default":
				td.def = &tableEntry{m: model.Matcher{Kind: "default"}, value: val, src: csrc}
				continue
			case "ranges":
				geoRanges = true
				continue
			case "proxy", "proxy_recursive", "delete":
				t.Reasons = append(t.Reasons, model.Reason{Code: model.ReasonConnection, Detail: "geo " + key})
				continue
			}
			en := tableEntry{value: val, src: csrc}
			if geoRanges {
				en.m = model.Matcher{Kind: "range", Pattern: key}
			} else {
				en.m = model.Matcher{Kind: "cidr", Pattern: key}
				if p, err := parsePrefix(key); err == nil {
					en.pfx = p
				}
			}
			td.entries = append(td.entries, en)
		case "split_clients":
			if key == "*" {
				td.def = &tableEntry{m: model.Matcher{Kind: "percent", Pattern: "*"}, value: val, src: csrc}
				continue
			}
			td.entries = append(td.entries, tableEntry{m: model.Matcher{Kind: "percent", Pattern: key}, value: val, src: csrc})
		}
	}
	for i := range td.entries {
		en := &td.entries[i]
		if en.m.Kind == "regex" {
			if re, ok := compileRegex(en.m.Pattern, en.m.CI); ok {
				en.re = re
			} else {
				en.reErr = true
				t.Reasons = append(t.Reasons, model.Reason{Code: model.ReasonUnsupportedRe, Detail: en.m.Pattern})
			}
		}
	}
	// Leading/trailing wildcard keys only exist with "hostnames".
	if t.Hostnames {
		for i := range td.entries {
			en := &td.entries[i]
			if en.m.Kind != "exact" {
				continue
			}
			switch {
			case strings.HasPrefix(en.m.Pattern, "*.") || strings.HasPrefix(en.m.Pattern, "."):
				en.m.Kind = "leading-wildcard"
			case strings.HasSuffix(en.m.Pattern, ".*"):
				en.m.Kind = "trailing-wildcard"
			}
		}
	}
	for _, en := range td.entries {
		s := en.src
		t.Entries = append(t.Entries, model.TableEntry{Match: en.m, Value: en.value, Src: &s})
	}
	if td.def != nil {
		s := td.def.src
		t.Default = &model.TableEntry{Match: td.def.m, Value: td.def.value, Src: &s}
	}
	if hasVars(td.src) {
		t.Status = model.StatusConditional
	}
	if len(t.Reasons) > 0 {
		sort.SliceStable(t.Reasons, func(i, j int) bool { return t.Reasons[i].Code < t.Reasons[j].Code })
	}
	return td
}

func parsePrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return p, err
	}
	return p.Masked(), nil
}

// containsReason walks the whole expression, including switch subjects.
func containsReason(e *model.Expr, code string) bool {
	if e == nil {
		return false
	}
	if e.Kind == "sym" && e.Reason == code {
		return true
	}
	if containsReason(e.On, code) || containsReason(e.Default, code) {
		return true
	}
	for _, p := range e.Parts {
		if containsReason(p, code) {
			return true
		}
	}
	for _, c := range e.Cases {
		if containsReason(c.Value, code) {
			return true
		}
	}
	return false
}
