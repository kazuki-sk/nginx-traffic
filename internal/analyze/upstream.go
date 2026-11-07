package analyze

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

type upstreams struct {
	a        *analyzer
	module   string                     // http | stream
	explicit map[string]*model.Upstream // lower-cased name
	implicit map[string]*model.Upstream // "host:port"
	order    []*model.Upstream
}

func newUpstreams(a *analyzer, module string) *upstreams {
	return &upstreams{a: a, module: module, explicit: map[string]*model.Upstream{}, implicit: map[string]*model.Upstream{}}
}

// addExplicit parses an `upstream name { ... }` block
// (doc: ngx_http_upstream_module, ngx_stream_upstream_module).
func (u *upstreams) addExplicit(n *conf.Node) {
	name := n.Arg(0)
	key := strings.ToLower(name)
	if _, dup := u.explicit[key]; dup {
		u.a.errorf("parse-error", n.Src, "duplicate upstream \"%s\"", name)
		return
	}
	up := &model.Upstream{Meta: meta(n.Src), Name: name, Kind: "explicit", Balancer: "round_robin", Members: []*model.Member{}, UsedBy: []string{}}
	for _, c := range n.Block {
		switch c.Name {
		case "server":
			up.Members = append(up.Members, parseMember(c))
		case "least_conn", "ip_hash", "random", "hash", "least_time", "ntlm", "sticky":
			if c.Name != "ntlm" && c.Name != "sticky" {
				up.Balancer = strings.TrimSpace(c.Text())
			}
			up.Directives = append(up.Directives, model.Inherited{Directive: c.Name, Args: c.Args, From: "here", Src: srcp(c.Src)})
		default:
			up.Directives = append(up.Directives, model.Inherited{Directive: c.Name, Args: c.Args, From: "here", Src: srcp(c.Src)})
		}
	}
	for _, m := range up.Members {
		if m.Status == model.StatusDynamic {
			up.Status = worst(up.Status, model.StatusDynamic)
			up.Reasons = appendReason(up.Reasons, model.Reason{Code: model.ReasonRuntimeDNS, Detail: "member with \"resolve\" is re-resolved at run time"})
		}
	}
	if len(up.Members) == 0 {
		u.a.errorf("parse-error", n.Src, "no servers are inside upstream")
	}
	u.explicit[key] = up
	u.order = append(u.order, up)
}

// parseMember parses `server address [parameters]`. Defaults per doc
// ngx_http_upstream_module#server: weight=1, max_fails=1, fail_timeout=10s,
// max_conns=0.
func parseMember(c *conf.Node) *model.Member {
	m := &model.Member{Meta: meta(c.Src), Server: c.Arg(0), Weight: 1, MaxFails: 1, FailTimeout: "10s"}
	for _, p := range c.Args[1:] {
		k, v, hasV := strings.Cut(p, "=")
		switch {
		case k == "weight" && hasV:
			m.Weight, _ = strconv.Atoi(v)
		case k == "max_fails" && hasV:
			m.MaxFails, _ = strconv.Atoi(v)
		case k == "fail_timeout" && hasV:
			m.FailTimeout = v
		case k == "max_conns" && hasV:
			m.MaxConns, _ = strconv.Atoi(v)
		case p == "backup":
			m.Backup = true
		case p == "down":
			m.Down = true
		case p == "resolve":
			m.Resolve = true
		default:
			m.Params = append(m.Params, p)
		}
	}
	if m.Resolve {
		// `resolve` (OSS since 1.27.3): addresses are re-resolved at run time.
		m.Status = model.StatusDynamic
		m.Reasons = []model.Reason{{Code: model.ReasonRuntimeDNS, Detail: "server ... resolve"}}
	}
	return m
}

func (u *upstreams) list() []*model.Upstream {
	out := append([]*model.Upstream(nil), u.order...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return false
	})
	return out
}

// passURL is a parsed *_pass destination.
type passURL struct {
	scheme string
	host   string // lower-cased host, or "unix:/path"
	port   int
	noPort bool
	uri    string
	unix   bool
}

var defaultPorts = map[string]int{"http": 80, "https": 443, "grpc": 80, "grpcs": 443, "uwsgi": 0, "suwsgi": 0}

// parsePass parses a *_pass argument (doc: proxy_pass, grpc_pass, fastcgi_pass, ...).
func parsePass(directive, s string) (*passURL, error) {
	p := &passURL{}
	rest := s
	if i := strings.Index(s, "://"); i >= 0 {
		p.scheme = strings.ToLower(s[:i])
		rest = s[i+3:]
	}
	switch directive {
	case "proxy_pass":
		if p.scheme != "http" && p.scheme != "https" {
			return nil, fmt.Errorf("invalid URL prefix in \"%s\"", s)
		}
	case "grpc_pass":
		if p.scheme != "" && p.scheme != "grpc" && p.scheme != "grpcs" {
			return nil, fmt.Errorf("invalid URL prefix in \"%s\"", s)
		}
	case "uwsgi_pass":
		if p.scheme != "" && p.scheme != "uwsgi" && p.scheme != "suwsgi" {
			return nil, fmt.Errorf("invalid URL prefix in \"%s\"", s)
		}
	}
	if strings.HasPrefix(rest, "unix:") {
		p.unix = true
		// proxy_pass http://unix:/path:/uri — the socket path ends at ':'
		path := rest[5:]
		if i := strings.IndexByte(path, ':'); i >= 0 {
			p.uri = path[i+1:]
			path = path[:i]
		}
		p.host = "unix:" + path
		return p, nil
	}
	hostport := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		hostport, p.uri = rest[:i], rest[i:]
	}
	host := hostport
	portStr := ""
	if strings.HasPrefix(hostport, "[") {
		if i := strings.IndexByte(hostport, ']'); i >= 0 {
			host = hostport[:i+1]
			if strings.HasPrefix(hostport[i+1:], ":") {
				portStr = hostport[i+2:]
			}
		}
	} else if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		host, portStr = hostport[:i], hostport[i+1:]
	}
	p.host = strings.ToLower(host)
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port in \"%s\"", s)
		}
		p.port = n
	} else {
		p.noPort = true
		p.port = defaultPorts[p.scheme]
	}
	return p, nil
}

func isIP(h string) bool {
	_, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil
}

// resolveStatic implements ngx_http_upstream_add (src/http/ngx_http_upstream.c
// 1.30.0) for a pass target without variables: an explicit upstream with the
// same (case-insensitive) name is used regardless of definition order; giving
// a port then is an error ("upstream ... may not have port"). Otherwise an
// implicit upstream host:port is created whose address is resolved once when
// the configuration is loaded.
func (u *upstreams) resolveStatic(p *passURL, raw string, src conf.Src, usedBy string) *model.Backend {
	b := &model.Backend{Meta: meta(src), URL: raw}
	if p.unix {
		b.Kind = "unix"
		return b
	}
	if up, ok := u.explicit[p.host]; ok {
		if !p.noPort {
			u.a.errorf("parse-error", src, "upstream \"%s\" may not have port %d", up.Name, p.port)
		}
		b.Kind, b.Upstream = "upstream", up.Name
		up.UsedBy = appendUniq(up.UsedBy, usedBy)
		b.Status, b.Reasons = up.Status, up.Reasons
		return b
	}
	key := fmt.Sprintf("%s:%d", p.host, p.port)
	up, ok := u.implicit[key]
	if !ok {
		up = &model.Upstream{Meta: meta(src), Name: key, Kind: "implicit", Balancer: "round_robin", UsedBy: []string{},
			Members: []*model.Member{{Meta: meta(src), Server: key, Weight: 1, MaxFails: 1, FailTimeout: "10s"}}}
		u.implicit[key] = up
		u.order = append(u.order, up)
	}
	up.UsedBy = appendUniq(up.UsedBy, usedBy)
	b.Upstream = up.Name
	if isIP(p.host) {
		b.Kind = "address"
	} else {
		b.Kind = "name"
		b.DNS = "startup"
	}
	return b
}

// resolveRuntime implements the run-time lookup for pass targets with
// variables (src ngx_http_upstream_init_request: search upstream groups by
// host with matching port — explicit groups have port 0 and only match a
// URL without port — then fall back to the resolver). doc proxy_pass:
// "the name is searched among the described server groups, and, if not
// found, is determined using a resolver".
func (u *upstreams) resolveRuntime(p *passURL, raw string, src conf.Src, usedBy string, hasResolver bool) *model.Backend {
	b := &model.Backend{Meta: meta(src), URL: raw}
	b.Status = model.StatusResolved
	if p.unix {
		b.Kind = "unix"
		return b
	}
	if up, ok := u.explicit[p.host]; ok && p.noPort {
		b.Kind, b.Upstream = "upstream", up.Name
		up.UsedBy = appendUniq(up.UsedBy, usedBy)
		return b
	}
	for _, up := range u.order {
		if up.Kind == "implicit" && up.Name == fmt.Sprintf("%s:%d", p.host, p.port) {
			b.Kind, b.Upstream = "upstream", up.Name
			up.UsedBy = appendUniq(up.UsedBy, usedBy)
			return b
		}
	}
	if isIP(p.host) {
		b.Kind = "address"
		return b
	}
	b.Kind = "name"
	b.DNS = "runtime-resolver"
	b.Status = model.StatusDynamic
	b.Reasons = []model.Reason{{Code: model.ReasonRuntimeDNS, Detail: "resolved per request by the configured resolver"}}
	if !hasResolver {
		u.a.warnf("no-resolver", src, "%s resolves to host name %q at run time but no resolver is defined; nginx would fail such requests", raw, p.host)
	}
	return b
}

func appendUniq(xs []string, x string) []string {
	if x == "" {
		return xs
	}
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

// passTarget builds the handler for a *_pass directive.
func (hc *httpCtx) passTarget(n *conf.Node, ev *evaluator, sc *srvCtx) *model.Handler {
	return passHandlerCommon(hc.a, hc.ups, n, ev, ownerID(hc.nodeLabel, n), hasResolver(n))
}

func ownerID(labels map[*conf.Node]string, n *conf.Node) string {
	for cur := n.Parent; cur != nil; cur = cur.Parent {
		if l, ok := labels[cur]; ok {
			return strings.TrimPrefix(l, "location:")
		}
	}
	return ""
}

// hasResolver: `resolver` is inherited from enclosing levels.
func hasResolver(n *conf.Node) bool {
	for cur := n.Parent; cur != nil; cur = cur.Parent {
		if cur.First("resolver") != nil {
			return true
		}
	}
	return false
}

func passHandlerCommon(a *analyzer, ups *upstreams, n *conf.Node, ev *evaluator, owner string, resolver bool) *model.Handler {
	h := &model.Handler{Meta: meta(n.Src), Kind: n.Name, Raw: n.Text()}
	if len(n.Args) == 0 {
		a.errorf("parse-error", n.Src, "invalid number of arguments in \"%s\"", n.Name)
		return h
	}
	raw := n.Args[0]
	h.Target = ev.eval(raw)
	setStatus(&h.Meta, h.Target)
	directive := n.Name
	if directive == "proxy_pass" && ups.module == "stream" {
		directive = "stream_proxy_pass"
	}
	if !hasVars(raw) {
		p, err := parsePass(directive, raw)
		if err != nil {
			a.errorf("parse-error", n.Src, "%v", err)
			return h
		}
		b := ups.resolveStatic(p, raw, n.Src, owner)
		h.Backends = []*model.Backend{b}
		h.Status = worst(h.Status, b.Status)
		h.Reasons = append(h.Reasons, b.Reasons...)
		return h
	}
	// Variables: nginx resolves at run time (u->resolved). Enumerate the
	// statically known outcomes.
	leaves, ok := h.Target.Leaves(64)
	if !ok {
		h.Status = model.StatusDynamic
		h.Reasons = appendReason(h.Reasons, model.Reason{Code: model.ReasonBranchLimit, Detail: "more than 64 possible targets"})
		return h
	}
	for _, lf := range leaves {
		when := strings.Join(lf.When, " && ")
		if !lf.Value.IsLit() {
			b := &model.Backend{Meta: meta(n.Src), When: when, URL: lf.Value.String(), Kind: "dynamic"}
			b.Status = model.StatusDynamic
			b.Reasons = lf.Value.Reasons()
			h.Backends = append(h.Backends, b)
			continue
		}
		p, err := parsePass(directive, lf.Value.Value)
		if err != nil {
			b := &model.Backend{Meta: meta(n.Src), When: when, URL: lf.Value.Value, Kind: "dynamic"}
			b.Status = model.StatusDynamic
			b.Reasons = []model.Reason{{Code: model.ReasonParse, Detail: err.Error()}}
			h.Backends = append(h.Backends, b)
			continue
		}
		b := ups.resolveRuntime(p, lf.Value.Value, n.Src, owner, resolver)
		b.When = when
		if len(leaves) > 1 && b.Status == model.StatusResolved {
			b.Status = model.StatusConditional
		}
		h.Backends = append(h.Backends, b)
		h.Status = worst(h.Status, b.Status)
		for _, r := range b.Reasons {
			h.Reasons = appendReason(h.Reasons, r)
		}
	}
	return h
}

// ---- inheritance ----

// Default list of directives whose effective (inherited) value is shown.
// Rule for all of them: the value of the nearest enclosing level that
// defines the directive applies; array-type directives such as
// proxy_set_header / add_header / allow/deny / error_page are inherited
// "if and only if there are no ... directives defined on the current level"
// (doc ngx_http_proxy_module#proxy_set_header, ngx_http_headers_module#add_header).
var defaultInherit = []string{"proxy_set_header", "proxy_hide_header", "proxy_pass_header", "add_header",
	"proxy_http_version", "proxy_redirect", "proxy_ssl_server_name", "proxy_ssl_name", "proxy_ssl_verify",
	"root", "index", "client_max_body_size", "allow", "deny", "auth_basic", "auth_request", "error_page", "resolver", "http2", "http3"}

func (a *analyzer) inheritList() []string {
	hide := set(a.opt.InheritHide...)
	var out []string
	for _, d := range append(append([]string(nil), defaultInherit...), a.opt.InheritShow...) {
		if !hide[d] && !containsStr(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func (hc *httpCtx) inherited(n *conf.Node, names []string, selfID string, proxy bool) []model.Inherited {
	return inheritedCommon(hc.nodeLabel, n, names, selfID, proxy)
}

func inheritedCommon(labels map[*conf.Node]string, n *conf.Node, names []string, selfID string, proxy bool) []model.Inherited {
	var out []model.Inherited
	for _, name := range names {
		found := false
		for cur := n; cur != nil && cur.Name != "#main"; cur = cur.Parent {
			defs := cur.Find(name)
			// allow/deny form one rule list (ngx_http_access_module)
			if name == "allow" || name == "deny" {
				if len(cur.Find("allow"))+len(cur.Find("deny")) == 0 {
					continue
				}
			}
			if len(defs) == 0 {
				if name == "allow" || name == "deny" {
					break
				}
				continue
			}
			from := labels[cur]
			if cur == n {
				from = "here"
			}
			for _, d := range defs {
				out = append(out, model.Inherited{Directive: name, Args: d.Args, From: from, Src: srcp(d.Src)})
			}
			found = true
			break
		}
		if !found && proxy && name == "proxy_set_header" {
			// doc proxy_set_header: "By default, only two fields are redefined"
			out = append(out,
				model.Inherited{Directive: name, Args: []string{"Host", "$proxy_host"}, From: "default"},
				model.Inherited{Directive: name, Args: []string{"Connection", "close"}, From: "default"})
		}
	}
	return out
}
