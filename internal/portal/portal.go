// Package portal builds the site-oriented view (model.Portal) shown by
// index.html: one entry per virtual server with its entry URL, group,
// access summary, inner URLs, certificates and configuration text.
package portal

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"nginx-sitemap/internal/analyze"
	"nginx-sitemap/internal/certs"
	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

// Input of Build.
type Input struct {
	Analysis   *analyze.Result
	HTTP       *model.HTTP
	Stream     *model.Stream
	Certs      *certs.Result // nil when read_certificates is off
	ConfPrefix string
}

// Build creates the portal view. It must run after masking and after
// manual annotations have been attached to servers.
func Build(in Input) *model.Portal {
	p := &model.Portal{Sites: []*model.Site{}, HTTPConfig: []model.ConfigLine{}, UpstreamConfig: map[string][]model.ConfigLine{}}
	nodes := in.Analysis.Nodes
	if in.HTTP != nil {
		listens := map[string]*model.Listen{}
		for _, l := range in.HTTP.Listens {
			listens[l.ID] = l
		}
		for _, s := range in.HTTP.Servers {
			p.Sites = append(p.Sites, httpSite(s, nodes.Servers[s.ID], listens, in))
		}
		if nodes.HTTP != nil {
			p.HTTPConfig = httpLevel(nodes.HTTP)
		}
	}
	if in.Stream != nil {
		listens := map[string]*model.Listen{}
		for _, l := range in.Stream.Listens {
			listens[l.ID] = l
		}
		for _, s := range in.Stream.Servers {
			p.Sites = append(p.Sites, streamSite(s, nodes.Servers[s.ID], listens, in))
		}
	}
	for key, n := range nodes.Upstreams {
		p.UpstreamConfig[key] = render(n, n.Src.File, 0, nil)
	}
	sort.SliceStable(p.Sites, func(i, j int) bool {
		a, b := p.Sites[i], p.Sites[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Name < b.Name
	})
	return p
}

func newSite(id, proto string, meta model.Meta, listens []string) *model.Site {
	return &model.Site{Meta: meta, ID: "site-" + id, ServerID: id, Proto: proto, Listens: append([]string{}, listens...),
		InnerURLs: []string{}, Upstreams: []string{}, Config: []model.ConfigLine{}}
}

func httpSite(s *model.Server, node *conf.Node, listens map[string]*model.Listen, in Input) *model.Site {
	site := newSite(s.ID, "http", s.Meta, s.Listens)
	site.Status, site.Reasons = model.StatusStatic, nil
	primary, aliases := primaryName(s.Names)
	site.Aliases = aliases
	ssl := false
	for _, id := range s.Listens {
		if l := listens[id]; l != nil && l.SSL {
			ssl = true
		}
	}
	switch {
	case primary == nil:
		site.Name = "（名前なし） " + listenLabels(s.Listens, listens)
	default:
		site.Name = primary.Name
		if primary.Kind == "exact" {
			site.URL = entryURL(primary.Name, s.Listens, listens)
		}
	}
	applyAnnotations(site, s.Annotations)
	if site.Group == "" {
		site.Group, site.GroupSource = defaultGroup(primary)
	}
	if node != nil {
		site.Access = access(node)
		if ssl {
			site.Certs = certificates(node, in)
		}
		site.Config = render(node, node.Src.File, 0, nil)
	}
	collectBackends(site, s.Handler)
	var walk func([]*model.Location)
	walk = func(ls []*model.Location) {
		for _, l := range ls {
			collectBackends(site, l.Handler)
			for _, ib := range l.IfBlocks {
				collectBackends(site, ib.Handler)
			}
			walk(l.Children)
		}
	}
	walk(s.Locations)
	site.InnerURLs = innerURLs(site, in)
	for _, l := range s.Locations {
		site.Status = worst(site.Status, l.Status)
	}
	return site
}

func streamSite(s *model.StreamServer, node *conf.Node, listens map[string]*model.Listen, in Input) *model.Site {
	site := newSite(s.ID, "stream", s.Meta, s.Listens)
	primary, aliases := primaryName(s.Names)
	site.Aliases = aliases
	if primary != nil {
		site.Name = primary.Name
	} else {
		site.Name = listenLabels(s.Listens, listens)
	}
	applyAnnotations(site, s.Annotations)
	if site.Group == "" {
		site.Group, site.GroupSource = "TCP / UDP", "default"
	}
	ssl := false
	for _, id := range s.Listens {
		if l := listens[id]; l != nil && l.SSL {
			ssl = true
		}
	}
	if node != nil {
		site.Access = access(node)
		if ssl {
			site.Certs = certificates(node, in)
		}
		site.Config = render(node, node.Src.File, 0, nil)
	}
	collectBackends(site, s.Handler)
	site.InnerURLs = innerURLs(site, in)
	return site
}

// primaryName picks the name shown for a site: the first exact name, else
// the first wildcard/regex name. The empty name "" is never primary.
func primaryName(names []model.ServerName) (*model.ServerName, []string) {
	var primary *model.ServerName
	for i := range names {
		if names[i].Kind == "exact" {
			primary = &names[i]
			break
		}
	}
	if primary == nil {
		for i := range names {
			if names[i].Kind != "empty" && names[i].Kind != "invalid" {
				primary = &names[i]
				break
			}
		}
	}
	var aliases []string
	for _, n := range names {
		if primary != nil && n.Name == primary.Name || n.Kind == "empty" || n.Kind == "invalid" {
			continue
		}
		aliases = append(aliases, n.Name)
	}
	return primary, aliases
}

// entryURL builds the URL users open: https when the server has an ssl
// listen, and the port only when it is not the scheme's default.
func entryURL(host string, ids []string, listens map[string]*model.Listen) string {
	var best *model.Listen
	for _, id := range ids {
		l := listens[id]
		if l == nil || l.Family == "unix" || l.QUIC {
			continue
		}
		if best == nil || l.SSL && !best.SSL {
			best = l
		}
	}
	if best == nil {
		return ""
	}
	scheme, def := "http", 80
	if best.SSL {
		scheme, def = "https", 443
	}
	if best.Port == def {
		return scheme + "://" + host + "/"
	}
	return fmt.Sprintf("%s://%s:%d/", scheme, host, best.Port)
}

func applyAnnotations(site *model.Site, as []*model.Annotation) {
	for _, a := range as {
		site.Annotations = append(site.Annotations, a)
		if a.Group != "" {
			site.Group, site.GroupSource = a.Group, "annotate"
		}
		if a.Owner != "" {
			site.Owner = a.Owner
		}
		if a.Note != "" {
			site.Note = a.Note
		}
		site.Tags = append(site.Tags, a.Tags...)
		site.Links = append(site.Links, a.Links...)
	}
}

// Second-level labels under which registrations are made (a pragmatic
// subset of the Public Suffix List; no external dependency).
var secondLevel = map[string]bool{
	"co.jp": true, "ne.jp": true, "or.jp": true, "ac.jp": true, "go.jp": true, "ad.jp": true, "ed.jp": true, "gr.jp": true, "lg.jp": true,
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true, "com.au": true, "net.au": true, "org.au": true,
	"co.kr": true, "or.kr": true, "com.cn": true, "net.cn": true, "com.tw": true, "com.hk": true, "co.nz": true, "com.sg": true, "co.in": true, "com.br": true,
}

// defaultGroup groups by registrable domain when no group is annotated:
// shop.example.co.jp → example.co.jp, wiki.corp.example.internal → example.internal.
func defaultGroup(primary *model.ServerName) (string, string) {
	if primary == nil || primary.Kind == "regex" || primary.Kind == "hostname" {
		return "（ドメインなし）", "default"
	}
	host := strings.TrimPrefix(strings.TrimPrefix(primary.Name, "*."), ".")
	host = strings.TrimSuffix(host, ".*")
	if net.ParseIP(host) != nil {
		return "（IP アドレス）", "default"
	}
	labels := strings.Split(host, ".")
	n := 2
	if len(labels) >= 3 && secondLevel[strings.Join(labels[len(labels)-2:], ".")] {
		n = 3
	}
	if len(labels) <= n {
		return host, "domain"
	}
	return strings.Join(labels[len(labels)-n:], "."), "domain"
}

// access summarises the effective allow/deny rules (inherited from the
// nearest level that defines any, ngx_http_access_module) and the
// authentication directives in effect.
func access(node *conf.Node) *model.Access {
	a := &model.Access{Summary: "制限なし"}
	for cur := node; cur != nil && cur.Name != "#main"; cur = cur.Parent {
		var rules []*conf.Node
		for _, c := range cur.Block {
			if (c.Name == "allow" || c.Name == "deny") && !c.MapParam {
				rules = append(rules, c)
			}
		}
		if len(rules) == 0 {
			continue
		}
		var allows []string
		for _, r := range rules {
			a.Rules = append(a.Rules, r.Name+" "+r.Arg(0))
			if r.Name == "allow" {
				allows = append(allows, r.Arg(0))
			}
		}
		s := rules[0].Src
		a.Src = &s
		last := rules[len(rules)-1]
		switch {
		case last.Name == "deny" && last.Arg(0) == "all" && len(allows) > 0 && len(allows) == len(rules)-1:
			a.Summary = strings.Join(allows, "、") + " のみ"
		case last.Name == "deny" && last.Arg(0) == "all" && len(allows) == 0:
			a.Summary = "すべて拒否"
		default:
			a.Summary = fmt.Sprintf("ルール %d 件", len(rules))
		}
		break
	}
	for _, d := range []struct{ name, label string }{
		{"auth_basic", "Basic 認証"}, {"auth_request", "auth_request"}, {"auth_jwt", "JWT"}, {"ssl_verify_client", "クライアント証明書"},
	} {
		for cur := node; cur != nil && cur.Name != "#main"; cur = cur.Parent {
			if n := cur.Last(d.name); n != nil {
				if v := n.Arg(0); v != "off" && v != "" {
					label := d.label
					if d.name == "auth_request" {
						label += " " + v
					}
					a.Auth = append(a.Auth, label)
				}
				break
			}
		}
	}
	return a
}

// certificates returns the certificates in effect for a server
// (ssl_certificate is inherited from http/stream when not set).
func certificates(node *conf.Node, in Input) []*model.Cert {
	if in.Certs == nil {
		return nil // read_certificates off: show nothing rather than "unreadable"
	}
	for cur := node; cur != nil && cur.Name != "#main"; cur = cur.Parent {
		defs := cur.Find("ssl_certificate")
		if len(defs) == 0 {
			continue
		}
		var out []*model.Cert
		for _, d := range defs {
			if c := in.Certs.Lookup(d.Arg(0), in.ConfPrefix); c != nil {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

func collectBackends(site *model.Site, h *model.Handler) {
	if h == nil {
		return
	}
	for _, b := range h.Backends {
		if b.Upstream != "" && !contains(site.Upstreams, b.Upstream) {
			site.Upstreams = append(site.Upstreams, b.Upstream)
		}
		if b.Kind == "address" || b.Kind == "name" || b.Kind == "unix" {
			u := b.URL
			if !strings.Contains(u, "://") && !strings.HasPrefix(u, "unix:") {
				sch := strings.TrimSuffix(h.Kind, "_pass")
				if site.Proto == "stream" {
					sch = streamScheme(site)
				}
				u = sch + "://" + u
			}
			site.InnerURLs = appendUniq(site.InnerURLs, directURL(u))
		}
	}
	site.Status = worst(site.Status, h.Status)
}

// directURL keeps scheme and authority of a *_pass URL and drops the URI
// part (it is rewritten per request).
func directURL(u string) string {
	if strings.HasPrefix(u, "unix:") {
		return u
	}
	rest, scheme := u, ""
	if i := strings.Index(u, "://"); i >= 0 {
		scheme, rest = u[:i+3], u[i+3:]
	}
	if strings.HasPrefix(rest, "unix:") {
		return scheme + rest
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return scheme + rest + "/"
}

// innerURLs lists URLs that reach the backends directly (bypassing nginx):
// upstream members with the scheme of the *_pass that uses them.
func innerURLs(site *model.Site, in Input) []string {
	out := []string{}
	ups := map[string]*model.Upstream{}
	if site.Proto == "http" && in.HTTP != nil {
		for _, u := range in.HTTP.Upstreams {
			ups[u.Name] = u
		}
	}
	if site.Proto == "stream" && in.Stream != nil {
		for _, u := range in.Stream.Upstreams {
			ups[u.Name] = u
		}
	}
	for _, name := range site.Upstreams {
		u := ups[name]
		if u == nil || u.Kind != "explicit" {
			continue // implicit upstreams are listed from the *_pass URL itself
		}
		sch := upstreamScheme(site, name)
		for _, m := range u.Members {
			out = appendUniq(out, memberURL(sch, m.Server))
		}
	}
	for _, d := range site.InnerURLs {
		out = appendUniq(out, d)
	}
	return out
}

// upstreamScheme finds the scheme used to reach upstream name from this site.
func upstreamScheme(site *model.Site, name string) string {
	if site.Proto == "stream" {
		return streamScheme(site)
	}
	for _, l := range site.Config {
		t := strings.TrimSpace(l.Text)
		for _, d := range []string{"proxy_pass", "grpc_pass", "fastcgi_pass", "uwsgi_pass", "scgi_pass", "memcached_pass"} {
			if !strings.HasPrefix(t, d+" ") {
				continue
			}
			arg := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(t, d)), ";")
			host := arg
			if i := strings.Index(host, "://"); i >= 0 {
				host = host[i+3:]
			}
			if i := strings.IndexAny(host, "/:"); i >= 0 {
				host = host[:i]
			}
			if !strings.EqualFold(host, name) && !strings.EqualFold(host+":"+portOf(arg), name) {
				continue
			}
			if i := strings.Index(arg, "://"); i >= 0 {
				return strings.ToLower(arg[:i])
			}
			return strings.TrimSuffix(d, "_pass")
		}
	}
	return "http"
}

func portOf(arg string) string {
	a := arg
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	if i := strings.IndexByte(a, '/'); i >= 0 {
		a = a[:i]
	}
	if i := strings.LastIndexByte(a, ':'); i >= 0 {
		return a[i+1:]
	}
	return ""
}

func memberURL(scheme, server string) string {
	if strings.HasPrefix(server, "unix:") {
		return server
	}
	host := server
	if _, _, err := net.SplitHostPort(server); err != nil {
		def := map[string]int{"http": 80, "https": 443, "grpc": 80, "grpcs": 443}[scheme]
		if def != 0 {
			host = net.JoinHostPort(strings.Trim(server, "[]"), strconv.Itoa(def))
		}
	}
	return scheme + "://" + host + "/"
}

// ---- configuration text ----

// render reconstructs the configuration of a block from the (masked) AST.
// Lines from included files are marked, preceded by a marker naming the
// include directive. Closing braces carry no source.
func render(n *conf.Node, baseFile string, depth int, out []model.ConfigLine) []model.ConfigLine {
	if out == nil {
		out = []model.ConfigLine{}
	}
	indent := strings.Repeat("    ", depth)
	out = append(out, model.ConfigLine{Src: n.Src.String(), Text: indent + n.Text() + " {", Kind: kindOf(n, baseFile)})
	prevChain := ""
	for _, c := range n.Block {
		chain := strings.Join(c.Src.Chain, " > ")
		if c.Src.File != baseFile && chain != prevChain && len(c.Src.Chain) > 0 {
			out = append(out, model.ConfigLine{Src: c.Src.Chain[len(c.Src.Chain)-1], Text: indent + "    # include " + c.Src.File, Kind: "marker"})
		}
		prevChain = chain
		if c.IsBlock {
			out = render(c, baseFile, depth+1, out)
			continue
		}
		out = append(out, model.ConfigLine{Src: c.Src.String(), Text: indent + "    " + c.Text() + ";", Kind: kindOf(c, baseFile)})
	}
	out = append(out, model.ConfigLine{Text: indent + "}", Kind: kindOf(n, baseFile)})
	return out
}

func kindOf(n *conf.Node, baseFile string) string {
	if n.Src.File != baseFile {
		return "include"
	}
	return ""
}

// httpLevel lists the http-level directives that servers inherit (blocks
// such as server, upstream, map and types are left out).
func httpLevel(h *conf.Node) []model.ConfigLine {
	out := []model.ConfigLine{}
	for _, c := range h.Block {
		if c.IsBlock || c.MapParam {
			continue
		}
		out = append(out, model.ConfigLine{Src: c.Src.String(), Text: c.Text() + ";", Kind: "inherited"})
	}
	return out
}

func worst(a, b string) string {
	rank := map[string]int{model.StatusStatic: 0, model.StatusResolved: 1, model.StatusConditional: 2, model.StatusDynamic: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func appendUniq(xs []string, x string) []string {
	if contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

// streamScheme is "udp" for servers listening with the udp parameter.
func streamScheme(site *model.Site) string {
	for _, id := range site.Listens {
		if strings.HasPrefix(id, "stream-udp:") {
			return "udp"
		}
	}
	return "tcp"
}

// listenLabels formats listen ids for display: "TCP *:5432", "*:80".
func listenLabels(ids []string, listens map[string]*model.Listen) string {
	var out []string
	for _, id := range ids {
		l := listens[id]
		if l == nil {
			out = append(out, id)
			continue
		}
		addr := fmt.Sprintf("%s:%d", l.Addr, l.Port)
		if l.Family == "unix" {
			addr = l.Addr
		}
		switch l.Proto {
		case "stream-tcp":
			addr = "TCP " + addr
		case "stream-udp":
			addr = "UDP " + addr
		}
		out = append(out, addr)
	}
	return strings.Join(out, ", ")
}
