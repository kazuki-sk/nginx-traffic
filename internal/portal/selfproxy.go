package portal

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"nginx-sitemap/internal/model"
)

// Self-proxy: a *_pass destination that points back to this nginx, so the
// request is received again by another server block (a cache or auth layer
// on 127.0.0.1:8081, for example).
//
// A destination is "this nginx" when it is
//   - a loopback address (127.0.0.0/8, ::1, localhost) and a listen exists on
//     that port (on the address itself or on the wildcard of its family),
//   - an address:port that a listen names explicitly, or
//   - a unix socket that a listen names.
//
// The receiving listen follows nginx's address matching: an explicit
// address:port wins over the wildcard on the same port (src
// ngx_http_init_listening / ngx_http_add_addrs; see docs/spec-basis.md).
// The receiving server is then chosen by the Host header that nginx sends
// (proxy_set_header Host, default $proxy_host — doc ngx_http_proxy_module
// #proxy_set_header), using the same selection order as for clients.

type listenIndex struct {
	byID  map[string]*model.Listen
	order []*model.Listen
}

func newListenIndex(ls []*model.Listen) *listenIndex {
	ix := &listenIndex{byID: map[string]*model.Listen{}}
	for _, l := range ls {
		if l.Proto != "http" || l.QUIC {
			continue
		}
		ix.byID[l.ID] = l
		ix.order = append(ix.order, l)
	}
	return ix
}

// lookup returns the listen that receives a connection to addr
// ("127.0.0.1:8081", "[::1]:80", "localhost:8081", "unix:/run/x.sock").
func (ix *listenIndex) lookup(addr string) *model.Listen {
	if strings.HasPrefix(addr, "unix:") {
		for _, l := range ix.order {
			if l.Family == "unix" && l.Addr == addr {
				return l
			}
		}
		return nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil
	}
	host = strings.ToLower(host)
	var ip netip.Addr
	if host == "localhost" {
		ip = netip.MustParseAddr("127.0.0.1")
	} else if ip, err = netip.ParseAddr(host); err != nil {
		return nil // host names are not followed (DNS is not consulted)
	}
	explicit := ""
	if ip.Is4() {
		explicit = ip.String()
	} else {
		explicit = "[" + ip.String() + "]"
	}
	var wildcard *model.Listen
	for _, l := range ix.order {
		if l.Port != port {
			continue
		}
		if l.Addr == explicit {
			return l
		}
		if ip.Is4() && l.Addr == "*" || ip.Is6() && l.Addr == "[::]" {
			wildcard = l
		}
	}
	if wildcard == nil {
		return nil
	}
	// The wildcard only receives loopback traffic or traffic to an address
	// that this nginx is known to own (named by another listen).
	if ip.IsLoopback() || ix.ownsIP(explicit) {
		return wildcard
	}
	return nil
}

func (ix *listenIndex) ownsIP(addr string) bool {
	for _, l := range ix.order {
		if l.Addr == addr {
			return true
		}
	}
	return false
}

// selectServer emulates virtual-server selection on listen l for a Host
// value (port stripped, lower-cased): exact, longest leading wildcard,
// longest trailing wildcard, first regex, else the default server. The
// listen's NameIndex is already ordered that way.
func selectServer(l *model.Listen, host string) (serverID string, certain bool) {
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	h = strings.TrimSuffix(h, ".")
	certain = true
	for _, group := range []int{1, 2, 3, 4} {
		for _, e := range l.NameIndex {
			if e.Group != group || e.Ignored != "" {
				continue
			}
			switch e.Kind {
			case "exact", "empty":
				if e.Name == h {
					return e.ServerID, true
				}
			case "leading-wildcard": // *.example.com
				if strings.HasSuffix(h, e.Name[1:]) && len(h) > len(e.Name)-1 {
					return e.ServerID, true
				}
			case "dot-wildcard": // .example.com
				if h == e.Name[1:] || strings.HasSuffix(h, e.Name) {
					return e.ServerID, true
				}
			case "trailing-wildcard": // www.example.*
				if strings.HasPrefix(h, e.Name[:len(e.Name)-1]) && len(h) > len(e.Name)-1 {
					return e.ServerID, true
				}
			case "regex":
				re, err := regexp.Compile("(?i)" + strings.TrimPrefix(e.Name, "~"))
				if err != nil {
					certain = false // PCRE-only pattern: cannot decide
					continue
				}
				if re.MatchString(h) {
					return e.ServerID, certain
				}
			}
		}
	}
	if l.DefaultServer == nil {
		return "", false
	}
	return l.DefaultServer.ServerID, certain
}

// hostCandidates evaluates the Host header nginx sends to the hop.
// $host / $http_host / $server_name stand for the names of the calling
// server; $proxy_host is the host part of the *_pass URL.
func hostCandidates(expr string, caller *model.Server, proxyHost string) (hosts []string, known bool) {
	switch expr {
	case "$proxy_host":
		return []string{proxyHost}, true
	case "$host", "$http_host", "$server_name":
		for _, n := range caller.Names {
			if n.Kind == "exact" {
				hosts = append(hosts, n.Name)
			}
		}
		return hosts, len(hosts) > 0
	}
	if !strings.Contains(expr, "$") {
		return []string{expr}, true
	}
	return nil, false
}

// hostHeader returns the effective Host header expression for a location
// (proxy_set_header is inherited only when the level defines none; the
// default is $proxy_host).
func hostHeader(inh []model.Inherited) string {
	for _, d := range inh {
		if d.Directive == "proxy_set_header" && len(d.Args) == 2 && strings.EqualFold(d.Args[0], "host") {
			return d.Args[1]
		}
	}
	return "$proxy_host"
}

type hopCtx struct {
	in       Input
	ix       *listenIndex
	byServer map[string]*model.Site
	servers  map[string]*model.Server
	ups      map[string]*model.Upstream
	warnings []model.Diag
}

// linkHops finds self-proxies of every http site, links the receiving
// sites, marks internal sites, and replaces inner URLs by the final
// destinations reached through the hops.
func linkHops(p *model.Portal, in Input) []model.Diag {
	if in.HTTP == nil {
		return nil
	}
	c := &hopCtx{in: in, ix: newListenIndex(in.HTTP.Listens), byServer: map[string]*model.Site{}, servers: map[string]*model.Server{}, ups: map[string]*model.Upstream{}}
	for _, s := range p.Sites {
		c.byServer[s.ServerID] = s
	}
	for _, s := range in.HTTP.Servers {
		c.servers[s.ID] = s
	}
	for _, u := range in.HTTP.Upstreams {
		c.ups[u.Name] = u
	}
	for _, s := range in.HTTP.Servers {
		site := c.byServer[s.ID]
		if site == nil {
			continue
		}
		site.Internal = internalOnly(site.Listens, c.ix)
		if site.Internal {
			site.URL = "" // reachable only from this host: no entry URL for users
		}
		c.scanHandler(site, s, "", s.Handler, s.Inherited)
		var walk func([]*model.Location)
		walk = func(ls []*model.Location) {
			for _, l := range ls {
				c.scanHandler(site, s, l.ID, l.Handler, l.Inherited)
				for _, ib := range l.IfBlocks {
					c.scanHandler(site, s, l.ID, ib.Handler, l.Inherited)
				}
				walk(l.Children)
			}
		}
		walk(s.Locations)
	}
	// callers, grouping of internal sites, final destinations
	for _, site := range p.Sites {
		for _, h := range site.Hops {
			if t := siteByID(p, h.TargetSite); t != nil && !contains(t.Callers, site.ID) {
				t.Callers = append(t.Callers, site.ID)
			}
		}
	}
	for _, site := range p.Sites {
		if site.Internal && site.GroupSource != "annotate" {
			site.Group, site.GroupSource = "nginx 内部", "default"
		}
	}
	final := map[string][]string{}
	for _, site := range p.Sites {
		site.InnerURLs = c.finalDestinations(p, site, final, map[string]bool{}, nil)
	}
	for _, site := range p.Sites {
		sort.Strings(site.Callers)
	}
	return c.warnings
}

func siteByID(p *model.Portal, id string) *model.Site {
	for _, s := range p.Sites {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func internalOnly(ids []string, ix *listenIndex) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		l := ix.byID[id]
		if l == nil {
			return false
		}
		if l.Family == "unix" {
			continue
		}
		a := strings.Trim(l.Addr, "[]")
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

func (c *hopCtx) scanHandler(site *model.Site, s *model.Server, locID string, h *model.Handler, inh []model.Inherited) {
	if h == nil || h.Kind != "proxy_pass" {
		return // the Host-based server choice below is proxy_pass semantics
	}
	host := hostHeader(inh)
	for _, b := range h.Backends {
		if b.Kind == "dynamic" {
			continue
		}
		scheme, hostport := splitURL(b.URL)
		if u := c.ups[b.Upstream]; u != nil && u.Kind == "explicit" {
			for _, m := range u.Members {
				c.tryHop(site, s, locID, h, b, scheme, m.Server, u.Name, host, b.Upstream)
			}
			continue
		}
		c.tryHop(site, s, locID, h, b, scheme, hostport, "", host, hostport)
	}
}

// splitURL returns the scheme and host:port (default port filled in) of a
// proxy_pass URL; unix sockets are returned as "unix:/path".
func splitURL(u string) (scheme, hostport string) {
	scheme, rest := "http", u
	if i := strings.Index(u, "://"); i >= 0 {
		scheme, rest = strings.ToLower(u[:i]), u[i+3:]
	}
	if strings.HasPrefix(rest, "unix:") {
		p := rest
		if i := strings.IndexByte(p[5:], ':'); i >= 0 {
			p = p[:5+i]
		}
		return scheme, p
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if _, _, err := net.SplitHostPort(rest); err != nil {
		port := "80"
		if scheme == "https" {
			port = "443"
		}
		rest = net.JoinHostPort(strings.Trim(rest, "[]"), port)
	}
	return scheme, rest
}

func (c *hopCtx) tryHop(site *model.Site, s *model.Server, locID string, h *model.Handler, b *model.Backend, scheme, addr, upstream, hostExpr, proxyHost string) {
	l := c.ix.lookup(addr)
	if l == nil {
		return
	}
	src := h.Src
	hop := &model.Hop{Meta: model.Meta{Src: src, Origin: model.OriginNginx, Status: model.StatusStatic},
		Location: locID, Via: addr, Upstream: upstream, Listen: l.ID, Host: hostExpr}
	if hostExpr == "$proxy_host" {
		hop.Host = "$proxy_host (" + strings.TrimSuffix(strings.TrimSuffix(proxyHost, ":80"), ":443") + ")"
		proxyHost = strings.TrimSuffix(strings.TrimSuffix(proxyHost, ":80"), ":443")
	}
	if (scheme == "https") != l.SSL && l.Family != "unix" {
		hop.Note = fmt.Sprintf("%s で %s に送っているが、listen は %s", scheme, addr, map[bool]string{true: "ssl", false: "ssl なし"}[l.SSL])
		if src != nil {
			c.warnings = append(c.warnings, model.Diag{Code: "self-proxy-scheme", Message: site.Name + ": " + hop.Note, Src: src})
		}
	}
	hosts, known := hostCandidates(hostExpr, s, proxyHost)
	targets := map[string]bool{}
	certain := known
	if known {
		for _, hh := range hosts {
			id, ok := selectServer(l, hh)
			if id != "" {
				targets[id] = true
			}
			certain = certain && ok
		}
	}
	if len(targets) == 0 && l.DefaultServer != nil {
		targets[l.DefaultServer.ServerID] = true
		certain = false
		hop.Note = joinNote(hop.Note, "Host が決まらないため、既定の server を示す")
	}
	if !certain || len(targets) > 1 {
		hop.Status = model.StatusConditional
	}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := c.byServer[id]
		if t == nil {
			continue
		}
		x := *hop
		x.TargetSite = t.ID
		site.Hops = append(site.Hops, &x)
	}
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "。" + b
}

// finalDestinations replaces addresses that point back to nginx by the
// destinations of the receiving site, following hops transitively.
func (c *hopCtx) finalDestinations(p *model.Portal, site *model.Site, memo map[string][]string, visiting map[string]bool, chain []string) []string {
	if v, ok := memo[site.ID]; ok {
		return v
	}
	if visiting[site.ID] {
		c.warnings = append(c.warnings, model.Diag{Code: "self-proxy-loop",
			Message: "nginx 内部の転送がループしています: " + strings.Join(append(chain, site.Name), " → "), Src: site.Src})
		return nil
	}
	visiting[site.ID] = true
	defer delete(visiting, site.ID)
	via := map[string]bool{}
	for _, h := range site.Hops {
		via[h.Via] = true
	}
	out := []string{}
	for _, u := range site.InnerURLs {
		if _, hp := splitURL(u); via[hp] || via[u] {
			continue
		}
		out = appendUniq(out, u)
	}
	for _, h := range site.Hops {
		if t := siteByID(p, h.TargetSite); t != nil && t.ID != site.ID {
			for _, u := range c.finalDestinations(p, t, memo, visiting, append(chain, site.Name)) {
				out = appendUniq(out, u)
			}
		} else if t != nil {
			c.warnings = append(c.warnings, model.Diag{Code: "self-proxy-loop", Message: "nginx 内部の転送が自分自身に戻っています: " + site.Name, Src: h.Src})
		}
	}
	memo[site.ID] = out
	return out
}
