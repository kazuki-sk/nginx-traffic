package analyze

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

type httpCtx struct {
	a         *analyzer
	http      *conf.Node
	tables    map[string]*tableDef
	setVars   map[string]bool
	ups       *upstreams
	out       *model.HTTP
	servers   []*srvCtx
	nodeLabel map[*conf.Node]string
}

type srvCtx struct {
	h       *httpCtx
	m       *model.Server
	node    *conf.Node
	listens []*lsn
	locs    []*locCtx
	named   map[string]*locCtx
	env     map[string]*model.Expr // after the server rewrite phase
}

type locCtx struct {
	m        *model.Location
	node     *conf.Node
	srv      *srvCtx
	parent   *locCtx
	children []*locCtx
	re       *regexp.Regexp
	reBad    bool
}

func (a *analyzer) analyzeHTTP(h *conf.Node) *model.HTTP {
	hc := &httpCtx{a: a, http: h, tables: map[string]*tableDef{}, setVars: map[string]bool{},
		out: &model.HTTP{}, nodeLabel: map[*conf.Node]string{h: "http"}}
	hc.ups = newUpstreams(a, "http")

	for _, n := range h.Block {
		switch n.Name {
		case "map", "geo", "split_clients":
			if td := buildTable(n, a); td != nil {
				if _, dup := hc.tables[td.t.Var]; dup {
					a.errorf("parse-error", n.Src, "duplicate variable $%s", td.t.Var)
					continue
				}
				hc.tables[td.t.Var] = td
				hc.out.Tables = append(hc.out.Tables, td.t)
			}
		case "upstream":
			hc.ups.addExplicit(n)
		}
	}
	collectSetVars(h, hc.setVars)

	// Servers, in configuration order.
	order := []string{}
	groups := map[string]*listenGroup{}
	names := map[string][]model.ServerName{}
	for _, n := range h.Find("server") {
		sc := hc.newServer(n, len(hc.servers)+1)
		hc.servers = append(hc.servers, sc)
		a.nodes.Servers[sc.m.ID] = n
		hc.out.Servers = append(hc.out.Servers, sc.m)
		names[sc.m.ID] = sc.m.Names
		for _, l := range sc.listens {
			a.addListen(l, sc.m.ID, &order, groups)
			if !containsStr(sc.m.Listens, l.key) {
				sc.m.Listens = append(sc.m.Listens, l.key)
			}
		}
	}
	hc.out.Listens = a.groupListens(order, groups)
	for _, l := range hc.out.Listens {
		l.NameIndex = a.buildNameIndex(l.Servers, names, l.ID)
	}

	// Per-server evaluation (needs listens for $server_port etc.).
	listenByID := map[string]*model.Listen{}
	for _, l := range hc.out.Listens {
		listenByID[l.ID] = l
	}
	for _, sc := range hc.servers {
		sc.evaluate(listenByID)
	}
	hc.out.Upstreams = hc.ups.list()
	if hc.out.Upstreams == nil {
		hc.out.Upstreams = []*model.Upstream{}
	}
	if hc.out.Tables == nil {
		hc.out.Tables = []*model.Table{}
	}
	if hc.out.Servers == nil {
		hc.out.Servers = []*model.Server{}
	}
	if hc.out.Listens == nil {
		hc.out.Listens = []*model.Listen{}
	}
	return hc.out
}

func collectSetVars(n *conf.Node, out map[string]bool) {
	for _, c := range n.Block {
		if c.Name == "set" && len(c.Args) > 0 && !c.MapParam {
			out[strings.TrimPrefix(c.Args[0], "$")] = true
		}
		if c.IsBlock && c.Name != "map" && c.Name != "geo" && c.Name != "split_clients" {
			collectSetVars(c, out)
		}
	}
}

func (hc *httpCtx) newServer(n *conf.Node, idx int) *srvCtx {
	a := hc.a
	sc := &srvCtx{h: hc, node: n, named: map[string]*locCtx{}}
	sc.m = &model.Server{Meta: meta(n.Src), ID: fmt.Sprintf("s%d", idx), Locations: []*model.Location{}, Listens: []string{}}
	hc.nodeLabel[n] = "server"
	for _, ln := range n.Find("listen") {
		l, err := parseListen(ln, "http")
		if err != nil {
			a.errorf("parse-error", ln.Src, "%v", err)
			continue
		}
		sc.listens = append(sc.listens, l)
	}
	if len(n.Find("listen")) == 0 {
		il := a.implicitListen(n.Src)
		sc.listens = append(sc.listens, il)
		sc.m.ImplicitListen = il.implicit
	}
	for _, l := range sc.listens {
		if l.http2 && versionAtLeast(a.opt.NginxVersion, "1.25.1") && a.opt.NginxVersion != "" {
			a.warnf("deprecated", l.src, "the \"listen ... http2\" parameter is deprecated since 1.25.1, use the \"http2\" directive instead")
		}
	}
	for _, sn := range n.Find("server_name") {
		for _, arg := range sn.Args {
			kind, norm, err := classifyName(arg)
			nm := model.ServerName{Name: norm, Kind: kind, Src: srcp(sn.Src), Error: err}
			if kind == "regex" {
				nm.Name = "~" + norm
			}
			if err != "" {
				a.errorf("parse-error", sn.Src, "%s \"%s\"", err, arg)
			}
			sc.m.Names = append(sc.m.Names, nm)
		}
	}
	if len(sc.m.Names) == 0 {
		// default: server_name "" (doc: ngx_http_core_module#server_name)
		sc.m.Names = []model.ServerName{{Name: "", Kind: "empty"}}
	}
	for _, nm := range sc.m.Names {
		if nm.Kind == "hostname" {
			sc.m.Status = worst(sc.m.Status, model.StatusDynamic)
			sc.m.Reasons = append(sc.m.Reasons, model.Reason{Code: model.ReasonHost, Detail: "server_name $hostname is replaced by the host name at load time"})
		}
	}
	// Locations
	order := 0
	for _, c := range n.Block {
		if c.Name == "location" {
			order++
			if lc := sc.newLocation(c, nil, fmt.Sprintf("%s/L%d", sc.m.ID, order), order); lc != nil {
				sc.locs = append(sc.locs, lc)
				sc.m.Locations = append(sc.m.Locations, lc.m)
			}
		}
	}
	return sc
}

// parseLocation parses `location` arguments. A single argument may carry
// the modifier glued to the URI (src ngx_http_core_location: checks
// name[0] == '=', '^~', '~', '~*', '@').
// Predicate locations (`location $var`) appeared in 1.31.5; on earlier
// versions an argument starting with '$' is an ordinary prefix string.
func parseLocation(args []string) (mod, uri string, err error) {
	switch len(args) {
	case 2:
		switch args[0] {
		case "=", "~", "~*", "^~":
			return args[0], args[1], nil
		}
		return "", "", fmt.Errorf("invalid location modifier \"%s\"", args[0])
	case 1:
		a := args[0]
		switch {
		case strings.HasPrefix(a, "@"):
			return "@", a, nil
		case strings.HasPrefix(a, "="):
			return "=", a[1:], nil
		case strings.HasPrefix(a, "^~"):
			return "^~", a[2:], nil
		case strings.HasPrefix(a, "~*"):
			return "~*", a[2:], nil
		case strings.HasPrefix(a, "~"):
			return "~", a[1:], nil
		}
		return "prefix", a, nil
	}
	return "", "", fmt.Errorf("invalid number of arguments in \"location\"")
}

func (sc *srvCtx) newLocation(n *conf.Node, parent *locCtx, id string, order int) *locCtx {
	a := sc.h.a
	mod, uri, err := parseLocation(n.Args)
	if err != nil {
		a.errorf("parse-error", n.Src, "%v", err)
		return nil
	}
	lc := &locCtx{node: n, srv: sc, parent: parent}
	lc.m = &model.Location{Meta: meta(n.Src), ID: id, Modifier: mod, URI: uri, Order: order}
	sc.h.nodeLabel[n] = "location:" + id
	switch mod {
	case "=":
		lc.m.Group = "exact"
	case "^~":
		lc.m.Group = "prefix-noregex"
	case "prefix":
		lc.m.Group = "prefix"
	case "~", "~*":
		lc.m.Group = "regex"
		re, ok := compileRegex(uri, mod == "~*")
		if ok {
			lc.re = re
		} else {
			lc.reBad = true
			lc.m.Reasons = append(lc.m.Reasons, model.Reason{Code: model.ReasonUnsupportedRe, Detail: uri})
		}
	case "@":
		lc.m.Group = "named"
		if parent != nil {
			a.errorf("parse-error", n.Src, "named location \"%s\" can be on the server level only", uri)
		}
		sc.named[uri] = lc
	}
	if parent != nil {
		switch parent.m.Modifier {
		case "=":
			a.errorf("parse-error", n.Src, "location \"%s\" could not be inside the exact location \"%s\"", uri, parent.m.URI)
		case "@":
			a.errorf("parse-error", n.Src, "location \"%s\" could not be inside the named location \"%s\"", uri, parent.m.URI)
		}
		if (mod == "prefix" || mod == "^~" || mod == "=") && (parent.m.Modifier == "prefix" || parent.m.Modifier == "^~") && !strings.HasPrefix(uri, parent.m.URI) {
			a.errorf("parse-error", n.Src, "location \"%s\" is outside location \"%s\"", uri, parent.m.URI)
		}
	}
	// doc ngx_http_core_module#location: a prefix ending with "/" handled by
	// *_pass answers the URI without the slash with 301. The flag is set by
	// the *_pass directive itself (src ngx_http_proxy_pass etc.:
	// clcf->auto_redirect = 1 when the location name ends with '/').
	if (mod == "prefix" || mod == "^~") && strings.HasSuffix(uri, "/") {
		for _, d := range passDirectives {
			if n.First(d) != nil {
				lc.m.AutoRedirect = true
			}
		}
	}
	co := 0
	for _, c := range n.Block {
		if c.Name == "location" {
			co++
			if ch := sc.newLocation(c, lc, fmt.Sprintf("%s.%d", id, co), co); ch != nil {
				lc.children = append(lc.children, ch)
				lc.m.Children = append(lc.m.Children, ch.m)
			}
		}
	}
	return lc
}

// ---- evaluation ----

type srvScope struct {
	sc   *srvCtx
	ls   map[string]*model.Listen
	root *model.Expr
}

// builtin resolves variables whose value is fixed by the server context.
// doc: ngx_http_core_module "Embedded Variables".
func (s *srvScope) builtin(name string) *model.Expr {
	sc := s.sc
	switch name {
	case "server_port", "server_addr", "scheme", "https":
		vals := map[string]*model.Expr{}
		var ids []string
		for _, id := range sc.m.Listens {
			l := s.ls[id]
			if l == nil || l.Family == "unix" {
				return nil
			}
			var v *model.Expr
			switch name {
			case "server_port":
				v = model.Lit(strconv.Itoa(l.Port))
			case "server_addr":
				if l.Addr == "*" || l.Addr == "[::]" || l.Family == "name" {
					v = model.Sym("$server_addr", model.ReasonConnection, "wildcard listen "+l.ID)
				} else {
					v = model.Lit(strings.Trim(l.Addr, "[]"))
				}
			case "scheme":
				v = model.Lit(map[bool]string{true: "https", false: "http"}[l.SSL])
			case "https":
				v = model.Lit(map[bool]string{true: "on", false: ""}[l.SSL])
			}
			vals[id] = v
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil
		}
		same := true
		for _, id := range ids[1:] {
			if vals[id].String() != vals[ids[0]].String() || vals[id].Kind != vals[ids[0]].Kind {
				same = false
			}
		}
		if same {
			return vals[ids[0]]
		}
		sw := &model.Expr{Kind: "switch", On: model.Sym("$"+name, model.ReasonConnection, "listen that accepted the connection"), Table: "listen"}
		for _, id := range ids {
			sw.Cases = append(sw.Cases, model.Case{Match: model.Matcher{Kind: "listen", Pattern: id}, Value: vals[id]})
		}
		return sw
	case "server_name":
		// Decided only when exactly one explicit name exists (conservative
		// reading of "name of the server which accepted a request").
		explicit := 0
		var only model.ServerName
		for _, n := range sc.m.Names {
			if n.Src != nil {
				explicit++
				only = n
			}
		}
		if explicit == 1 && only.Kind != "regex" && only.Kind != "hostname" {
			return model.Lit(only.Name)
		}
		return model.Sym("$server_name", model.ReasonRequest, "server has several server_name values")
	case "document_root":
		if s.root != nil {
			return s.root
		}
		return model.Lit("html") // default root "html" relative to prefix
	case "nginx_version":
		if v := sc.h.a.opt.NginxVersion; v != "" {
			return model.Lit(v)
		}
		return model.Sym("$nginx_version", model.ReasonVersion, "set nginx_version in the tool settings")
	}
	return nil
}

func (sc *srvCtx) evaluator(ls map[string]*model.Listen, root *model.Expr, env map[string]*model.Expr, caps map[string]bool) *evaluator {
	return &evaluator{tables: sc.h.tables, sc: &srvScope{sc: sc, ls: ls, root: root}, env: copyEnv(env),
		setVars: sc.h.setVars, captures: caps, visiting: map[string]bool{}}
}

func (sc *srvCtx) evaluate(ls map[string]*model.Listen) {
	// server-name regex captures are available to the whole server
	caps := map[string]bool{}
	for _, n := range sc.m.Names {
		if n.Kind == "regex" {
			for _, c := range namedCaptures(n.Name) {
				caps[c] = true
			}
		}
	}
	ev := sc.evaluator(ls, sc.rootExpr(sc.node, nil), nil, caps)
	r := &runner{a: sc.h.a, ev: ev}
	p := r.run(sc.node.Block, "")
	sc.m.Rewrite = p.model
	sc.env = ev.env
	if p.model != nil {
		for _, e := range p.model.Edges {
			sc.linkEdge(e, nil)
		}
	}
	if p.term != nil {
		sc.m.Handler = p.term
		sc.m.Handler.Note = "server-level return: every request to this server ends here"
	} else if p.termEdge != nil {
		sc.m.Handler = edgeHandler(p.termEdge)
	}
	sc.m.Edges = sc.errorPageEdges(sc.node, ev, true)
	sc.m.Inherited = sc.h.inherited(sc.node, sc.h.a.inheritList(), "", false)

	// Handler used when no location matches the request.
	if !sc.hasCatchAll() && sc.m.Handler == nil {
		sc.m.Handler = sc.staticHandler(sc.node, ev)
		sc.m.Handler.Note = "used when no location matches"
	}
	for _, lc := range sc.locs {
		lc.evaluate(ls, caps)
	}
}

func (sc *srvCtx) hasCatchAll() bool {
	for _, lc := range sc.locs {
		if (lc.m.Modifier == "prefix" || lc.m.Modifier == "^~") && lc.m.URI == "/" {
			return true
		}
	}
	return false
}

// rootExpr returns the effective root/alias (root is inherited; alias is
// not — doc ngx_http_core_module#root/#alias).
func (sc *srvCtx) rootExpr(n *conf.Node, ev *evaluator) *model.Expr {
	if n.Name == "location" {
		if al := n.Last("alias"); al != nil {
			if ev != nil {
				return ev.eval(al.Arg(0))
			}
			return model.Lit(al.Arg(0))
		}
	}
	for cur := n; cur != nil; cur = cur.Parent {
		if r := cur.Last("root"); r != nil {
			if ev != nil {
				return ev.eval(r.Arg(0))
			}
			if !hasVars(r.Arg(0)) {
				return model.Lit(r.Arg(0))
			}
			return nil
		}
		if cur.Name == "http" {
			break
		}
	}
	return model.Lit("html")
}

var passDirectives = []string{"proxy_pass", "fastcgi_pass", "grpc_pass", "uwsgi_pass", "scgi_pass", "memcached_pass"}

// Other content-phase handlers (location-level directives that install
// clcf->handler).
var otherContent = []string{"stub_status", "empty_gif", "mp4", "flv", "js_content", "perl", "random_index"}

func (lc *locCtx) evaluate(ls map[string]*model.Listen, srvCaps map[string]bool) {
	sc := lc.srv
	caps := map[string]bool{}
	for k := range srvCaps {
		caps[k] = true
	}
	for p := lc; p != nil; p = p.parent {
		if p.re != nil || p.reBad {
			for _, c := range namedCaptures(p.m.URI) {
				caps[c] = true
			}
		}
	}
	// Variables set during the server rewrite phase stay visible.
	ev := sc.evaluator(ls, nil, sc.env, caps)
	root := sc.rootExpr(lc.node, ev)
	ev.sc.(*srvScope).root = root

	r := &runner{a: sc.h.a, ev: ev, inLoc: true, handler: func(n *conf.Node, ev2 *evaluator) *model.Handler {
		return lc.passHandler(n, ev2)
	}}
	p := r.run(lc.node.Block, "")
	lc.m.Rewrite = p.model
	lc.m.IfBlocks = p.ifBlocks
	if p.model != nil {
		for _, e := range p.model.Edges {
			sc.linkEdge(e, lc)
		}
	}

	if p.term != nil {
		lc.m.Handler = p.term
		if lc.passHandler(lc.node, ev) != nil {
			lc.m.Handler.Note = "unconditional return in the rewrite phase; the content handler of this location is never reached"
		}
	} else if p.termEdge != nil {
		lc.m.Handler = edgeHandler(p.termEdge)
	} else if h := lc.passHandler(lc.node, ev); h != nil {
		lc.m.Handler = h
	} else {
		lc.m.Handler = sc.staticHandler(lc.node, ev)
	}
	lc.m.Edges = append(lc.m.Edges, sc.errorPageEdges(lc.node, ev, false)...)
	lc.m.Internal = lc.node.First("internal") != nil
	if tf := lc.node.Last("try_files"); tf != nil {
		lc.m.Edges = append(lc.m.Edges, sc.tryFilesEdge(tf, ev, lc))
	}
	lc.m.Status = worst(lc.m.Status, lc.m.Handler.Status)
	if lc.m.Handler.Status == model.StatusDynamic {
		lc.m.Reasons = append(lc.m.Reasons, lc.m.Handler.Reasons...)
	}
	lc.m.Inherited = sc.h.inherited(lc.node, sc.h.a.inheritList(), lc.m.ID, isProxy(lc.m.Handler.Kind))

	for _, ch := range lc.children {
		ch.evaluate(ls, srvCaps)
	}
}

func isPass(kind string) bool {
	for _, p := range passDirectives {
		if p == kind {
			return true
		}
	}
	return false
}

func isProxy(kind string) bool { return kind == "proxy_pass" }

// passHandler returns the content handler defined directly in n, if any.
// *_pass is not inherited by nested locations (it installs clcf->handler on
// that location only).
func (lc *locCtx) passHandler(n *conf.Node, ev *evaluator) *model.Handler {
	for _, d := range passDirectives {
		if pn := n.Last(d); pn != nil {
			return lc.srv.h.passTarget(pn, ev, lc.srv)
		}
	}
	for _, d := range otherContent {
		if on := n.Last(d); on != nil {
			h := &model.Handler{Meta: meta(on.Src), Kind: d, Raw: on.Text()}
			if d == "js_content" || d == "perl" {
				h.Status = model.StatusDynamic
				h.Reasons = []model.Reason{{Code: model.ReasonModule, Detail: d}}
			}
			return h
		}
	}
	return nil
}

// staticHandler: try_files or static files from root/alias.
func (sc *srvCtx) staticHandler(n *conf.Node, ev *evaluator) *model.Handler {
	if tf := n.Last("try_files"); tf != nil {
		h := &model.Handler{Meta: meta(tf.Src), Kind: "try_files", Raw: tf.Text()}
		h.Target = sc.rootExpr(n, ev)
		setStatus(&h.Meta, h.Target)
		return h
	}
	root := sc.rootExpr(n, ev)
	h := &model.Handler{Meta: model.Meta{Origin: model.OriginNginx, Status: model.StatusStatic}, Kind: "static", Target: root}
	kind := "root"
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Name == "location" && cur == n {
			if al := cur.Last("alias"); al != nil {
				h.Src, h.Raw, kind = srcp(al.Src), al.Text(), "alias"
				break
			}
		}
		if r := cur.Last("root"); r != nil {
			h.Src, h.Raw = srcp(r.Src), r.Text()
			break
		}
		if cur.Name == "http" {
			h.Raw = "root html (default)"
			break
		}
	}
	h.Note = kind
	setStatus(&h.Meta, root)
	return h
}

// ---- internal redirects ----

// errorPageEdges returns error_page edges defined at level n. error_page is
// inherited only if not defined at the current level (doc
// ngx_http_core_module#error_page; src ngx_http_core_merge_loc_conf:
// conf->error_pages = prev->error_pages). Inherited ones are listed once on
// the level that defines them (server/http → on the server, withHTTP) and
// appear in each location's "inherited" list; the target is resolved within
// the server either way.
func (sc *srvCtx) errorPageEdges(n *conf.Node, ev *evaluator, withHTTP bool) []*model.Edge {
	defs := n.Find("error_page")
	if len(defs) == 0 && withHTTP && sc.h.http != nil {
		defs = sc.h.http.Find("error_page")
	}
	var out []*model.Edge
	for _, d := range defs {
		if len(d.Args) < 2 {
			continue
		}
		uri := d.Args[len(d.Args)-1]
		var codes []string
		resp := ""
		for _, a := range d.Args[:len(d.Args)-1] {
			if strings.HasPrefix(a, "=") {
				resp = a
				continue
			}
			codes = append(codes, a)
		}
		e := &model.Edge{Meta: meta(d.Src), Kind: "error_page", Condition: "status in (" + strings.Join(codes, " ") + ")", Raw: d.Text()}
		if resp != "" {
			e.Condition += " " + resp
		}
		e.Target = ev.eval(uri)
		setStatus(&e.Meta, e.Target)
		if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
			e.Kind = "redirect"
			e.Code = 302
		} else {
			sc.linkEdge(e, nil)
		}
		out = append(out, e)
	}
	return out
}

func (sc *srvCtx) tryFilesEdge(tf *conf.Node, ev *evaluator, lc *locCtx) *model.Edge {
	last := tf.Args[len(tf.Args)-1]
	e := &model.Edge{Meta: meta(tf.Src), Kind: "try_files", Raw: tf.Text(),
		Condition: "none of " + strings.Join(tf.Args[:len(tf.Args)-1], " ") + " exist"}
	e.Reasons = []model.Reason{{Code: model.ReasonFilesystem, Detail: "file existence is checked at request time"}}
	if strings.HasPrefix(last, "=") {
		e.Kind = "return"
		e.Code, _ = strconv.Atoi(last[1:])
		e.Status = model.StatusConditional
		return e
	}
	e.Target = ev.eval(last)
	e.Status = worst(model.StatusConditional, e.Target.Status())
	e.Reasons = append(e.Reasons, e.Target.Reasons()...)
	sc.linkEdge(e, nil)
	return e
}

// linkEdge resolves an internal redirect target to a location of this server.
func (sc *srvCtx) linkEdge(e *model.Edge, cur *locCtx) {
	switch e.Kind {
	case "redirect", "return":
		return
	case "rewrite-break":
		if cur != nil {
			e.TargetLocation = cur.m.ID
		}
		return
	}
	if e.Target == nil {
		return
	}
	if !e.Target.IsLit() {
		e.Status = worst(e.Status, e.Target.Status())
		if e.Status == model.StatusDynamic {
			e.Reasons = appendReason(e.Reasons, model.Reason{Code: model.ReasonRequest, Detail: "target location cannot be determined statically"})
		}
		return
	}
	uri := e.Target.Value
	if strings.HasPrefix(uri, "@") {
		if nl, ok := sc.named[uri]; ok {
			e.TargetLocation = nl.m.ID
		} else {
			sc.h.a.errorf("parse-error", *e.Src, "named location \"%s\" not found", uri)
		}
		return
	}
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	if !strings.HasPrefix(uri, "/") {
		return
	}
	loc, uncertain := findLocation(sc.locs, uri)
	switch {
	case uncertain:
		e.Reasons = appendReason(e.Reasons, model.Reason{Code: model.ReasonUnsupportedRe, Detail: "a PCRE-only location regex prevents static matching"})
		e.Status = worst(e.Status, model.StatusDynamic)
	case loc != nil:
		e.TargetLocation = loc.m.ID
	default:
		e.TargetLocation = sc.m.ID // server-level configuration (no location matched)
	}
}

func appendReason(rs []model.Reason, r model.Reason) []model.Reason {
	for _, x := range rs {
		if x == r {
			return rs
		}
	}
	return append(rs, r)
}

// findLocation emulates ngx_http_core_find_location /
// ngx_http_core_find_static_location (src/http/ngx_http_core_module.c 1.30.0):
//  1. exact "=" match wins immediately;
//  2. a prefix equal to the whole URI, else a "/"-terminated *_pass location
//     equal to URI+"/" (auto_redirect, answered with 301), else the longest
//     matching prefix;
//  3. nested locations of that prefix are searched recursively; an exact /
//     auto-redirect / regex result there wins;
//  4. unless the prefix has "^~", regex locations of this level are tried in
//     order of appearance (their nested locations are searched on match);
//  5. otherwise the remembered prefix is used.
//
// uncertain=true when an untranslatable (PCRE-only) regex had to be tested.
func findLocation(level []*locCtx, uri string) (res *locCtx, uncertain bool) {
	r, _, unc := findLevel(level, uri)
	return r, unc
}

const (
	rcDeclined = iota
	rcAgain
	rcOK
	rcDone
)

func findLevel(level []*locCtx, uri string) (*locCtx, int, bool) {
	var best *locCtx
	var autoRedir *locCtx
	for _, l := range level {
		switch l.m.Modifier {
		case "=":
			if uri == l.m.URI {
				return l, rcOK, false
			}
		case "prefix", "^~":
			if strings.HasPrefix(uri, l.m.URI) {
				if best == nil || len(l.m.URI) > len(best.m.URI) {
					best = l
				}
			} else if l.m.AutoRedirect && uri+"/" == l.m.URI {
				autoRedir = l
			}
		}
	}
	if autoRedir != nil && (best == nil || len(best.m.URI) < len(uri)) {
		return autoRedir, rcDone, false
	}
	res, rc := (*locCtx)(nil), rcDeclined
	noregex := false
	uncertain := false
	if best != nil {
		res, rc = best, rcAgain
		noregex = best.m.Modifier == "^~"
		if len(best.children) > 0 {
			sub, src, unc := findLevel(best.children, uri)
			uncertain = uncertain || unc
			if src == rcOK || src == rcDone {
				return sub, src, uncertain
			}
			if sub != nil {
				res = sub
			}
		}
	}
	if !noregex {
		for _, l := range level {
			if l.m.Modifier != "~" && l.m.Modifier != "~*" {
				continue
			}
			if l.reBad {
				return res, rc, true
			}
			if l.re.MatchString(uri) {
				if len(l.children) > 0 {
					if sub, _, unc := findLevel(l.children, uri); sub != nil {
						return sub, rcOK, uncertain || unc
					}
				}
				return l, rcOK, uncertain
			}
		}
	}
	return res, rc, uncertain
}

// edgeHandler represents an unconditional rewrite (last / redirect) as the
// effective handler of its level.
func edgeHandler(e *model.Edge) *model.Handler {
	h := &model.Handler{Meta: e.Meta, Kind: "internal-redirect", Raw: e.Raw, Target: e.Target, Code: e.Code}
	if e.Kind == "redirect" {
		h.Kind = "redirect"
	}
	if e.TargetLocation != "" {
		h.Note = "continues in " + e.TargetLocation
	}
	return h
}
