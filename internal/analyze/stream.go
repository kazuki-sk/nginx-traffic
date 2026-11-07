package analyze

import (
	"fmt"
	"strconv"
	"strings"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

// Stream (TCP/UDP) analysis.
// doc: https://nginx.org/en/docs/stream/ngx_stream_core_module.html
//   - listen has no default; a server without listen is an error
//     (src ngx_stream_core_server: "no \"listen\" is defined for server").
//   - server_name (1.25.5) selects the server at the SSL/preread phase with
//     the same priority as http (exact, leading wildcard, trailing wildcard,
//     first regex).
//   - content: proxy_pass (ngx_stream_proxy_module), return
//     (ngx_stream_return_module), pass (ngx_stream_pass_module, 1.25.5).

type streamScope struct {
	ls      map[string]*model.Listen
	listens []string
	ssl     bool
}

func (s *streamScope) builtin(name string) *model.Expr {
	switch name {
	case "server_port", "server_addr":
		var first *model.Expr
		for _, id := range s.listens {
			l := s.ls[id]
			if l == nil || l.Family == "unix" {
				return nil
			}
			var v *model.Expr
			if name == "server_port" {
				v = model.Lit(strconv.Itoa(l.Port))
			} else if l.Addr == "*" || l.Addr == "[::]" || l.Family == "name" {
				v = model.Sym("$server_addr", model.ReasonConnection, "wildcard listen "+l.ID)
			} else {
				v = model.Lit(strings.Trim(l.Addr, "[]"))
			}
			if first == nil {
				first = v
			} else if first.String() != v.String() || first.Kind != v.Kind {
				return model.Sym("$"+name, model.ReasonConnection, "server has several listen addresses")
			}
		}
		return first
	}
	return nil
}

func (a *analyzer) analyzeStream(st *conf.Node) *model.Stream {
	out := &model.Stream{}
	tables := map[string]*tableDef{}
	ups := newUpstreams(a, "stream")
	labels := map[*conf.Node]string{st: "stream"}
	for _, n := range st.Block {
		switch n.Name {
		case "map", "geo", "split_clients":
			if td := buildTable(n, a); td != nil {
				tables[td.t.Var] = td
				out.Tables = append(out.Tables, td.t)
			}
		case "upstream":
			ups.addExplicit(n)
		}
	}
	setVars := map[string]bool{}
	collectSetVars(st, setVars)

	order := []string{}
	groups := map[string]*listenGroup{}
	names := map[string][]model.ServerName{}
	type pending struct {
		s    *model.StreamServer
		node *conf.Node
	}
	var srvs []pending
	for i, n := range st.Find("server") {
		s := &model.StreamServer{Meta: meta(n.Src), ID: fmt.Sprintf("t%d", i+1), Listens: []string{}}
		labels[n] = "server:" + s.ID
		lns := n.Find("listen")
		if len(lns) == 0 {
			a.errorf("parse-error", n.Src, "no \"listen\" is defined for server")
		}
		for _, ln := range lns {
			l, err := parseListen(ln, "stream-tcp")
			if err != nil {
				a.errorf("parse-error", ln.Src, "%v", err)
				continue
			}
			a.addListen(l, s.ID, &order, groups)
			s.Listens = appendUniq(s.Listens, l.key)
		}
		for _, sn := range n.Find("server_name") {
			if !versionAtLeast(a.opt.NginxVersion, "1.25.5") {
				a.errorf("parse-error", sn.Src, "stream server_name requires nginx 1.25.5 or later")
			}
			for _, arg := range sn.Args {
				kind, norm, e := classifyName(arg)
				nm := model.ServerName{Name: norm, Kind: kind, Src: srcp(sn.Src), Error: e}
				if kind == "regex" {
					nm.Name = "~" + norm
				}
				s.Names = append(s.Names, nm)
			}
		}
		names[s.ID] = s.Names
		srvs = append(srvs, pending{s, n})
		out.Servers = append(out.Servers, s)
	}
	out.Listens = a.groupListens(order, groups)
	ls := map[string]*model.Listen{}
	for _, l := range out.Listens {
		if hasNames(l.Servers, names) {
			l.NameIndex = a.buildNameIndex(l.Servers, names, l.ID)
		}
		ls[l.ID] = l
	}
	for _, p := range srvs {
		ev := &evaluator{tables: tables, sc: &streamScope{ls: ls, listens: p.s.Listens}, env: map[string]*model.Expr{},
			setVars: setVars, visiting: map[string]bool{}, captures: map[string]bool{}}
		for _, sn := range p.s.Names {
			if sn.Kind == "regex" {
				for _, c := range namedCaptures(sn.Name) {
					ev.captures[c] = true
				}
			}
		}
		// ngx_stream_set_module: `set` runs in the preaccess phase, in order.
		r := &runner{a: a, ev: ev}
		r.run(p.node.Block, "")
		switch {
		case p.node.Last("proxy_pass") != nil:
			pn := p.node.Last("proxy_pass")
			p.s.Handler = passHandlerCommon(a, ups, pn, ev, p.s.ID, hasResolver(pn))
		case p.node.Last("return") != nil:
			rn := p.node.Last("return")
			h := &model.Handler{Meta: meta(rn.Src), Kind: "return", Raw: rn.Text(), Target: ev.eval(rn.Arg(0))}
			setStatus(&h.Meta, h.Target)
			p.s.Handler = h
		case p.node.Last("pass") != nil:
			pn := p.node.Last("pass")
			h := &model.Handler{Meta: meta(pn.Src), Kind: "pass", Raw: pn.Text(), Target: ev.eval(pn.Arg(0))}
			setStatus(&h.Meta, h.Target)
			p.s.Handler = h
		default:
			p.s.Handler = &model.Handler{Meta: model.Meta{Origin: model.OriginNginx, Status: model.StatusStatic}, Kind: "none",
				Note: "no content handler (proxy_pass/return/pass)"}
		}
		p.s.Status = worst(p.s.Status, p.s.Handler.Status)
		p.s.Inherited = inheritedCommon(labels, p.node, []string{"proxy_timeout", "proxy_connect_timeout", "proxy_protocol", "ssl_preread", "allow", "deny", "resolver"}, p.s.ID, false)
	}
	out.Upstreams = ups.list()
	if out.Upstreams == nil {
		out.Upstreams = []*model.Upstream{}
	}
	if out.Tables == nil {
		out.Tables = []*model.Table{}
	}
	if out.Servers == nil {
		out.Servers = []*model.StreamServer{}
	}
	if out.Listens == nil {
		out.Listens = []*model.Listen{}
	}
	return out
}

func hasNames(ids []string, names map[string][]model.ServerName) bool {
	for _, id := range ids {
		if len(names[id]) > 0 {
			return true
		}
	}
	return false
}

// versionAtLeast compares dotted versions; an unknown version passes.
func versionAtLeast(v, min string) bool {
	if v == "" {
		return true
	}
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := 0; i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		y, _ = strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return true
}
