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

// lsn is one parsed listen directive.
type lsn struct {
	key      string // grouping key: proto|family|addr|port
	proto    string
	addr     string
	port     int
	family   string
	ssl      bool
	http2    bool
	quic     bool
	pp       bool
	bind     bool
	def      bool
	sockopt  bool // socket options given (lsopt->set): may appear once per address
	params   []string
	src      conf.Src
	implicit *model.Implicit
}

// Socket-level listen parameters; nginx sets lsopt->set for these and
// rejects them twice for the same address ("duplicate listen options",
// src: ngx_http_add_addresses). `bind` also implies set.
var sockoptParams = []string{"bind", "ipv6only=", "reuseport", "backlog=", "rcvbuf=", "sndbuf=",
	"setfib=", "fastopen=", "accept_filter=", "deferred", "so_keepalive="}

// Accepted listen parameters, from nginx 1.30.0 ngx_http_core_listen /
// ngx_stream_core_listen. Anything else is "invalid parameter" in nginx.
var (
	httpListenParams   = set("default_server", "default", "bind", "deferred", "reuseport", "multipath", "ssl", "quic", "http2", "proxy_protocol")
	streamListenParams = set("default_server", "udp", "bind", "deferred", "reuseport", "multipath", "ssl", "proxy_protocol")
	listenKV           = []string{"setfib=", "fastopen=", "backlog=", "rcvbuf=", "sndbuf=", "accept_filter=", "so_keepalive=", "ipv6only="}
)

func validListenParam(proto, p string) bool {
	if proto == "http" && httpListenParams[p] || proto != "http" && streamListenParams[p] {
		return true
	}
	for _, kv := range listenKV {
		if strings.HasPrefix(p, kv) {
			return true
		}
	}
	return false
}

// parseListen parses `listen` (doc: ngx_http_core_module#listen,
// ngx_stream_core_module#listen). An address without port uses port 80
// (http: "If only address is given, the port 80 is used").
func parseListen(n *conf.Node, proto string) (*lsn, error) {
	if len(n.Args) == 0 {
		return nil, fmt.Errorf("invalid number of arguments in \"listen\"")
	}
	l := &lsn{proto: proto, src: n.Src}
	a0 := n.Args[0]
	host := ""
	port := 80
	hasPort := false
	switch {
	case strings.HasPrefix(a0, "unix:"):
		l.family, l.addr, l.port = "unix", a0, 0
	case strings.HasPrefix(a0, "["):
		i := strings.IndexByte(a0, ']')
		if i < 0 {
			return nil, fmt.Errorf("invalid IPv6 address in \"listen %s\"", a0)
		}
		host = a0[1:i]
		rest := a0[i+1:]
		if strings.HasPrefix(rest, ":") {
			p, err := strconv.Atoi(rest[1:])
			if err != nil {
				return nil, fmt.Errorf("invalid port in \"listen %s\"", a0)
			}
			port, hasPort = p, true
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return nil, fmt.Errorf("invalid IPv6 address in \"listen %s\"", a0)
		}
		l.family = "inet6"
		if ip.IsUnspecified() {
			l.addr = "[::]"
		} else {
			l.addr = "[" + ip.String() + "]"
		}
	default:
		if p, err := strconv.Atoi(a0); err == nil {
			l.family, l.addr, port, hasPort = "inet", "*", p, true
			break
		}
		if i := strings.LastIndexByte(a0, ':'); i >= 0 {
			host = a0[:i]
			p, err := strconv.Atoi(a0[i+1:])
			if err != nil {
				return nil, fmt.Errorf("invalid port in \"listen %s\"", a0)
			}
			port, hasPort = p, true
		} else {
			host = a0
		}
		switch ip, err := netip.ParseAddr(host); {
		case host == "*":
			l.family, l.addr = "inet", "*"
		case err == nil && ip.Is4():
			l.family = "inet"
			if ip.IsUnspecified() {
				l.addr = "*"
			} else {
				l.addr = ip.String()
			}
		default:
			// A host name is resolved once by nginx at configuration load
			// and bound to every address (ngx_parse_url → ngx_inet_resolve_host).
			l.family, l.addr = "name", strings.ToLower(host)
		}
	}
	if l.family != "unix" {
		if proto != "http" && !hasPort {
			return nil, fmt.Errorf("no port in \"listen %s\"", a0)
		}
		l.port = port
	}
	for _, p := range n.Args[1:] {
		if !validListenParam(proto, p) {
			return nil, fmt.Errorf("invalid parameter \"%s\" in \"listen\"", p)
		}
		switch {
		case p == "default_server" || p == "default":
			l.def = true
		case p == "ssl":
			l.ssl = true
		case p == "http2":
			l.http2 = true
		case p == "quic":
			l.quic = true
		case p == "proxy_protocol":
			l.pp = true
		case p == "udp":
			if proto == "stream-tcp" {
				l.proto = "stream-udp"
			}
		}
		if p == "bind" {
			l.bind = true
		}
		for _, so := range sockoptParams {
			if p == so || (strings.HasSuffix(so, "=") && strings.HasPrefix(p, so)) {
				l.sockopt = true
			}
		}
		l.params = append(l.params, p)
	}
	// QUIC listens are UDP sockets distinct from TCP ones on the same port.
	q := ""
	if l.quic {
		q = "/quic"
	}
	l.key = fmt.Sprintf("%s:%s:%d%s", l.proto, l.addr, l.port, q)
	return l, nil
}

func (l *lsn) wildcard() bool { return l.addr == "*" || l.addr == "[::]" }

// listenGroup collects all servers bound to one address:port.
type listenGroup struct {
	m       *model.Listen
	first   *lsn
	servers []string
	setOpt  bool
	hasDef  bool
}

// groupListens builds the Listen table.
//
// Semantics, src/http/ngx_http.c (nginx 1.30.0):
//   - ngx_http_add_addresses: servers sharing an exact address:port share one
//     conf_addr; ssl/http2/quic/proxy_protocol are OR-ed across them; a second
//     explicit default_server is an error; without any, the first server added
//     (configuration order) is the default (ngx_http_add_address).
//   - ngx_http_init_listening: if "*:port" exists, only it (plus addresses with
//     `bind`) gets a socket; other addresses are dispatched per connection via
//     getsockname(). Each address keeps its *own* server list and default, so a
//     request to 10.0.0.1:80 is matched only against servers that listen on
//     10.0.0.1:80, never against "*:80" servers.
func (a *analyzer) groupListens(order []string, groups map[string]*listenGroup) []*model.Listen {
	var out []*model.Listen
	wild := map[string]bool{}
	for _, k := range order {
		g := groups[k]
		if g.first.wildcard() {
			wild[g.first.proto+"|"+g.first.family+"|"+strconv.Itoa(g.first.port)] = true
		}
	}
	for _, k := range order {
		g := groups[k]
		l := g.first
		if !l.wildcard() && l.family != "unix" && !g.m.Bind && wild[l.proto+"|"+l.family+"|"+strconv.Itoa(l.port)] {
			g.m.Socket = fmt.Sprintf("accepted on the %s:%d wildcard socket and dispatched by getsockname() (no separate bind)", map[string]string{"inet": "*", "inet6": "[::]"}[l.family], l.port)
		}
		if l.family == "name" {
			g.m.Status = model.StatusDynamic
			g.m.Reasons = append(g.m.Reasons, model.Reason{Code: model.ReasonRuntimeDNS, Detail: "listen host name is resolved by nginx at configuration load"})
		}
		out = append(out, g.m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// addListen registers server sid on listen l.
func (a *analyzer) addListen(l *lsn, sid string, order *[]string, groups map[string]*listenGroup) {
	g, ok := groups[l.key]
	if !ok {
		m := &model.Listen{
			Meta:      model.Meta{Src: srcp(l.src), Origin: model.OriginNginx, Status: model.StatusStatic},
			ID:        l.key,
			Proto:     l.proto,
			Addr:      l.addr,
			Port:      l.port,
			Family:    l.family,
			Servers:   []string{},
			NameIndex: []model.NameEntry{},
			Decls:     []conf.Src{},
		}
		if l.implicit != nil {
			m.Implicit = l.implicit
			m.Status = model.StatusResolved
			m.Reasons = []model.Reason{{Code: "implicit-default", Detail: l.implicit.Basis}}
		}
		g = &listenGroup{m: m, first: l}
		groups[l.key] = g
		*order = append(*order, l.key)
		// first server added becomes the default (ngx_http_add_address)
		m.DefaultServer = &model.DefaultRef{ServerID: sid, Explicit: false}
	} else if l.implicit == nil && g.m.Implicit != nil {
		// An explicit listen on the same address: no longer purely implicit.
		g.m.Implicit = nil
		g.m.Status = model.StatusStatic
		g.m.Reasons = nil
		g.m.Src = srcp(l.src)
	}
	m := g.m
	if l.sockopt {
		if g.setOpt {
			a.errorf("parse-error", l.src, "duplicate listen options for %s:%d", l.addr, l.port)
		}
		g.setOpt = true
		m.Params = l.params
	}
	m.SSL = m.SSL || l.ssl
	m.HTTP2 = m.HTTP2 || l.http2
	m.QUIC = m.QUIC || l.quic
	m.ProxyProtocol = m.ProxyProtocol || l.pp
	m.Bind = m.Bind || l.bind
	if l.def {
		if g.hasDef {
			a.errorf("parse-error", l.src, "a duplicate default server for %s:%d", l.addr, l.port)
		} else {
			g.hasDef = true
			m.DefaultServer = &model.DefaultRef{ServerID: sid, Explicit: true, Src: srcp(l.src)}
		}
	}
	if !containsStr(m.Servers, sid) {
		m.Servers = append(m.Servers, sid)
	}
	if l.implicit == nil {
		m.Decls = append(m.Decls, l.src)
	}
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// implicitListen returns the default listen for a server without one.
func (a *analyzer) implicitListen(srv conf.Src) *lsn {
	port := 8000
	why := "assume_superuser=off → *:8000"
	if a.opt.AssumeSuperuser {
		port = 80
		why = "assume_superuser=on → *:80"
	}
	l := &lsn{proto: "http", family: "inet", addr: "*", port: port, src: srv}
	l.key = fmt.Sprintf("http:*:%d", port)
	l.implicit = &model.Implicit{
		Value: fmt.Sprintf("*:%d", port),
		Basis: "no listen directive; " + why + " (doc: ngx_http_core_module#listen \"If the directive is not present then either *:80 is used if nginx runs with the superuser privileges, or *:8000 otherwise\"; src: ngx_http_core_server getuid()==0 ? 80 : 8000)",
	}
	return l
}

// ---- server names ----

// classifyName follows doc: https://nginx.org/en/docs/http/server_names.html
// and src ngx_http_core_server_name / ngx_http_server_names.
func classifyName(name string) (kind string, norm string, err string) {
	switch {
	case name == "":
		return "empty", "", ""
	case name == "$hostname":
		return "hostname", name, ""
	case strings.HasPrefix(name, "~"):
		return "regex", name[1:], ""
	}
	low := strings.ToLower(name) // ngx_strlow in ngx_http_core_server_name
	switch {
	case strings.HasPrefix(low, "*.") && !strings.Contains(low[1:], "*"):
		return "leading-wildcard", low, ""
	case strings.HasPrefix(low, ".") && !strings.Contains(low, "*"):
		return "dot-wildcard", low, ""
	case strings.HasSuffix(low, ".*") && !strings.Contains(low[:len(low)-1], "*"):
		return "trailing-wildcard", low, ""
	case strings.Contains(low, "*"):
		return "invalid", low, "invalid server name or wildcard"
	}
	return "exact", low, ""
}

func nameGroup(kind string) int {
	switch kind {
	case "exact", "empty", "hostname":
		return 1
	case "leading-wildcard", "dot-wildcard":
		return 2
	case "trailing-wildcard":
		return 3
	case "regex":
		return 4
	}
	return 9
}

// buildNameIndex orders virtual-server names for one address:port:
// exact → longest leading wildcard → longest trailing wildcard → first regex
// in order of appearance (doc server_names.html "Virtual server selection").
// Duplicate non-regex names on the same address are ignored after the first
// (src ngx_http_server_names: "conflicting server name ... ignored").
func (a *analyzer) buildNameIndex(serverIDs []string, names map[string][]model.ServerName, listenID string) []model.NameEntry {
	out := []model.NameEntry{}
	seen := map[string]string{}
	for _, sid := range serverIDs {
		for _, n := range names[sid] {
			if n.Kind == "invalid" {
				continue
			}
			e := model.NameEntry{Group: nameGroup(n.Kind), Kind: n.Kind, Name: n.Name, ServerID: sid, Src: n.Src}
			if n.Kind != "regex" {
				key := n.Name
				if n.Kind == "dot-wildcard" {
					key = "*" + n.Name
				}
				if other, dup := seen[key]; dup {
					if other != sid {
						e.Ignored = "conflicting server name, " + other + " wins"
						if n.Src != nil {
							a.warnf("conflicting-server-name", *n.Src, "conflicting server name %q on %s, ignored", n.Name, listenID)
						}
					} else {
						continue
					}
				} else {
					seen[key] = sid
				}
			}
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.Group != y.Group {
			return x.Group < y.Group
		}
		switch x.Group {
		case 1:
			return x.Name < y.Name
		case 2, 3:
			lx, ly := len(strings.TrimPrefix(x.Name, "*")), len(strings.TrimPrefix(y.Name, "*"))
			if lx != ly {
				return lx > ly
			}
			return x.Name < y.Name
		}
		return false // regex: keep order of appearance
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}
