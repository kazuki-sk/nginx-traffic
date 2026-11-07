// Package analyze turns a loaded nginx configuration into a model.Map by
// static analysis and partial evaluation. It never contacts nginx and never
// sends network traffic; optional DNS snapshots are done by the caller.
package analyze

import (
	"fmt"
	"sort"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

// Options control spec-dependent assumptions.
type Options struct {
	// AssumeSuperuser selects the implicit "listen" default: *:80 when nginx
	// runs with superuser privileges, *:8000 otherwise
	// (doc: ngx_http_core_module#listen; src: ngx_http_core_server,
	// `(getuid() == 0) ? 80 : 8000`). Not decidable statically.
	AssumeSuperuser bool
	// NginxVersion, e.g. "1.30.0". Enables version-dependent checks.
	NginxVersion string
	// InheritShow overrides/extends the list of inherited directives shown.
	InheritShow []string
	InheritHide []string
	Mask        MaskOptions
}

type analyzer struct {
	opt      Options
	errors   []model.Diag
	warnings []model.Diag
	nodes    Nodes
}

// Nodes gives later passes (the portal view) access to the AST behind
// model elements. Arguments are already masked.
type Nodes struct {
	HTTP      *conf.Node
	Stream    *conf.Node
	Servers   map[string]*conf.Node // server id → server block
	Upstreams map[string]*conf.Node // "http:<name>" / "stream:<name>" → upstream block (explicit only)
}

func (a *analyzer) errorf(code string, src conf.Src, format string, args ...any) {
	s := src
	a.errors = append(a.errors, model.Diag{Code: code, Message: fmt.Sprintf(format, args...), Src: &s})
}

func (a *analyzer) warnf(code string, src conf.Src, format string, args ...any) {
	s := src
	a.warnings = append(a.warnings, model.Diag{Code: code, Message: fmt.Sprintf(format, args...), Src: &s})
}

// Result of the analysis.
type Result struct {
	HTTP     *model.HTTP
	Stream   *model.Stream
	Errors   []model.Diag
	Warnings []model.Diag
	// Internal handles used by the manual-annotation pass.
	ServerByName   map[string][]string // server_name → server ids (http and stream)
	UpstreamByName map[string]*model.Upstream
	Nodes          Nodes
}

// Analyze runs the analysis over the loaded configuration.
func Analyze(res *conf.Result, opt Options) *Result {
	a := &analyzer{opt: opt, nodes: Nodes{Servers: map[string]*conf.Node{}, Upstreams: map[string]*conf.Node{}}}
	applyMask(res.Main, opt.Mask)

	out := &Result{ServerByName: map[string][]string{}, UpstreamByName: map[string]*model.Upstream{}}
	for _, e := range res.Errors {
		s := e.Src
		a.errors = append(a.errors, model.Diag{Code: e.Code, Message: e.Msg, Src: &s})
	}

	var httpBlocks, streamBlocks []*conf.Node
	for _, n := range res.Main.Block {
		switch n.Name {
		case "http":
			httpBlocks = append(httpBlocks, n)
		case "stream":
			streamBlocks = append(streamBlocks, n)
		}
	}
	// nginx rejects a second http/stream block ("is duplicate",
	// src: ngx_http_block / ngx_stream_block); analyse only the first.
	if len(httpBlocks) > 0 {
		for _, extra := range httpBlocks[1:] {
			a.errorf("parse-error", extra.Src, "\"http\" directive is duplicate")
		}
		a.nodes.HTTP = httpBlocks[0]
		out.HTTP = a.analyzeHTTP(httpBlocks[0])
		for _, s := range out.HTTP.Servers {
			for _, n := range s.Names {
				out.ServerByName[n.Name] = append(out.ServerByName[n.Name], s.ID)
			}
		}
		for _, u := range out.HTTP.Upstreams {
			out.UpstreamByName["http:"+u.Name] = u
		}
	}
	if len(streamBlocks) > 0 {
		for _, extra := range streamBlocks[1:] {
			a.errorf("parse-error", extra.Src, "\"stream\" directive is duplicate")
		}
		a.nodes.Stream = streamBlocks[0]
		out.Stream = a.analyzeStream(streamBlocks[0])
		for _, s := range out.Stream.Servers {
			for _, n := range s.Names {
				out.ServerByName[n.Name] = append(out.ServerByName[n.Name], s.ID)
			}
		}
		for _, u := range out.Stream.Upstreams {
			out.UpstreamByName["stream:"+u.Name] = u
		}
	}
	sortDiags(a.errors)
	sortDiags(a.warnings)
	out.Nodes = a.nodes
	out.Errors = a.errors
	out.Warnings = a.warnings
	return out
}

func sortDiags(ds []model.Diag) {
	sort.SliceStable(ds, func(i, j int) bool {
		si, sj := ds[i].Src, ds[j].Src
		if si == nil || sj == nil {
			return si != nil
		}
		if si.File != sj.File {
			return si.File < sj.File
		}
		return si.Line < sj.Line
	})
}

func srcp(s conf.Src) *conf.Src { return &s }

func meta(src conf.Src) model.Meta {
	return model.Meta{Src: srcp(src), Origin: model.OriginNginx, Status: model.StatusStatic}
}

// setStatus copies the status/reasons of an expression into m.
func setStatus(m *model.Meta, e *model.Expr) {
	m.Status = e.Status()
	m.Reasons = e.Reasons()
}

// worst combines statuses (dynamic > conditional > resolved > static).
func worst(a, b string) string {
	rank := map[string]int{model.StatusStatic: 0, model.StatusResolved: 1, model.StatusConditional: 2, model.StatusDynamic: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
