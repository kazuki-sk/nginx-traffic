package analyze

import (
	"strconv"
	"strings"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

// Rewrite-module directives executed as an imperative program
// (doc: https://nginx.org/en/docs/http/ngx_http_rewrite_module.html
// "directives are processed sequentially in the order of their appearance").
//
// Execution model, from nginx 1.30.0 source:
//   - server-level directives run in NGX_HTTP_SERVER_REWRITE_PHASE before the
//     location is searched; location-level ones in the location rewrite phase
//     (ngx_http_rewrite_handler).
//   - codes are NOT inherited by nested locations: ngx_http_rewrite_merge_loc_conf
//     leaves conf->codes NULL when the child has none, and the handler then
//     declines. So a nested location only executes its own directives.
//   - an "if" body is compiled into the *same* code array as its enclosing
//     level (ngx_http_rewrite_if: nlcf->codes = lcf->codes), i.e. it runs
//     inline; when the condition holds, the if-block's own location
//     configuration (e.g. a proxy_pass inside it) becomes current
//     (ngx_http_script_if_code sets r->loc_conf).
var rewriteDirectives = set("set", "if", "rewrite", "return", "break", "rewrite_log", "uninitialized_variable_warn")

type program struct {
	model      *model.Program
	terminated bool           // unconditional return/redirect/rewrite-last reached
	term       *model.Handler // the terminating return, if any
	termEdge   *model.Edge    // the terminating unconditional rewrite, if any
	ifBlocks   []*model.IfBlock
}

type runner struct {
	a       *analyzer
	ev      *evaluator
	inLoc   bool
	handler func(n *conf.Node, ev *evaluator) *model.Handler // content handler inside an if body
}

// run executes nodes and returns the program summary. ev.env is updated.
func (r *runner) run(nodes []*conf.Node, under string) *program {
	p := &program{model: &model.Program{}}
	r.exec(nodes, under, p)
	if len(p.model.Bindings) == 0 && len(p.model.Edges) == 0 {
		p.model = nil
	}
	return p
}

func (r *runner) exec(nodes []*conf.Node, under string, p *program) (stopped bool) {
	for _, n := range nodes {
		if !rewriteDirectives[n.Name] {
			continue
		}
		switch n.Name {
		case "set":
			if len(n.Args) != 2 {
				r.a.errorf("parse-error", n.Src, "invalid number of arguments in \"set\"")
				continue
			}
			name := strings.TrimPrefix(n.Args[0], "$")
			v := r.ev.eval(n.Args[1])
			r.ev.env[name] = v
			b := &model.Binding{Meta: meta(n.Src), Var: "$" + name, Value: v, Under: under}
			setStatus(&b.Meta, v)
			p.model.Bindings = append(p.model.Bindings, b)
		case "break":
			return true
		case "return":
			h := r.returnHandler(n)
			if under == "" {
				// unconditional: represented by the handler, not by an edge
				p.terminated = true
				p.term = h
				return true
			}
			e := &model.Edge{Meta: h.Meta, Kind: "return", Code: h.Code, Target: h.Target, Raw: n.Text(), Condition: under}
			if h.Kind == "redirect" {
				e.Kind = "redirect"
			}
			p.model.Edges = append(p.model.Edges, e)
			return true
		case "rewrite":
			if r.rewrite(n, under, p) {
				return true
			}
		case "if":
			if r.execIf(n, under, p) {
				return true
			}
		}
	}
	return false
}

func (r *runner) returnHandler(n *conf.Node) *model.Handler {
	h := &model.Handler{Meta: meta(n.Src), Kind: "return", Raw: n.Text()}
	switch len(n.Args) {
	case 1:
		if code, err := strconv.Atoi(n.Args[0]); err == nil {
			h.Code = code
		} else {
			// "return URL;" — 302 (doc: ngx_http_rewrite_module#return)
			h.Code = 302
			h.Target = r.ev.eval(n.Args[0])
			h.Kind = "redirect"
		}
	case 2:
		code, _ := strconv.Atoi(n.Args[0])
		h.Code = code
		h.Target = r.ev.eval(n.Args[1])
		if code == 301 || code == 302 || code == 303 || code == 307 || code == 308 {
			h.Kind = "redirect"
		}
	}
	if h.Target != nil {
		setStatus(&h.Meta, h.Target)
	}
	return h
}

// matchesEverything: rewrite regexes that match every URI. r->uri always
// begins with "/" for normal requests (ngx_http_parse_request_line /
// ngx_http_parse_complex_uri), so these always match.
var matchesEverything = set("^", "^.*", "^.*$", "^(.*)", "^(.*)$", ".*", "(.*)", "^/", "^/.*", "^/(.*)", "^/(.*)$", "^/.*$")

// rewrite handles `rewrite regex replacement [flag]`. Returns true when
// further rewrite processing at this level stops unconditionally.
func (r *runner) rewrite(n *conf.Node, under string, p *program) bool {
	if len(n.Args) < 2 {
		r.a.errorf("parse-error", n.Src, "invalid number of arguments in \"rewrite\"")
		return false
	}
	re, repl := n.Args[0], n.Args[1]
	flag := n.Arg(2)
	always := matchesEverything[re]
	cond := "$uri ~ " + re
	if always {
		cond = ""
	}
	cond = joinCond(under, cond)
	// The query string is dropped when the replacement ends with "?".
	target := r.ev.eval(strings.TrimSuffix(repl, "?"))
	e := &model.Edge{Meta: meta(n.Src), Condition: cond, Target: target, Raw: n.Text()}
	setStatus(&e.Meta, target)
	redirect := strings.HasPrefix(repl, "http://") || strings.HasPrefix(repl, "https://") || strings.HasPrefix(repl, "$scheme")
	switch {
	case flag == "permanent":
		e.Kind, e.Code = "redirect", 301
	case flag == "redirect" || redirect:
		e.Kind, e.Code = "redirect", 302
	case flag == "last":
		e.Kind = "rewrite-last"
	case flag == "break":
		e.Kind = "rewrite-break"
	default:
		e.Kind = "rewrite"
	}
	p.model.Edges = append(p.model.Edges, e)
	stops := always && under == "" && (e.Kind != "rewrite")
	if stops && e.Kind != "rewrite-break" {
		p.terminated = true
		p.termEdge = e
	}
	return stops
}

func joinCond(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " && " + b
}

func (r *runner) execIf(n *conf.Node, under string, p *program) (stopped bool) {
	cond := "(" + strings.Join(quoteArgs(n.Args), " ") + ")"
	val, known := r.evalCond(n.Args)
	ib := &model.IfBlock{Meta: meta(n.Src), Condition: cond}
	if known {
		ib.Status = model.StatusResolved
		if !val {
			ib.Reasons = []model.Reason{{Code: "never-true", Detail: "condition is statically false"}}
		}
	} else {
		ib.Status = model.StatusConditional
	}
	if r.handler != nil && (!known || val) {
		ib.Handler = r.handler(n, r.ev)
	}
	if r.inLoc || ib.Handler != nil {
		p.ifBlocks = append(p.ifBlocks, ib)
	}
	if known {
		if val {
			return r.exec(n.Block, under, p)
		}
		return false
	}
	// Unknown condition: run the body on a copy, then merge.
	before := r.ev.env
	r.ev.env = copyEnv(before)
	sub := &program{model: &model.Program{}}
	subStopped := r.exec(n.Block, joinCond(under, cond), sub)
	thenEnv := r.ev.env
	r.ev.env = before
	p.model.Bindings = append(p.model.Bindings, sub.model.Bindings...)
	p.model.Edges = append(p.model.Edges, sub.model.Edges...)
	if subStopped && endsWithTerminal(n.Block) {
		// then-branch left this level; continuation sees only the else env.
		return false
	}
	for k, tv := range thenEnv {
		ov, had := before[k]
		if had && ov == tv {
			continue
		}
		var def *model.Expr
		if had {
			def = ov
		} else {
			def = r.ev.resolve(k)
		}
		r.ev.env[k] = &model.Expr{Kind: "switch", Table: "if " + cond, On: model.Sym("if", model.ReasonCondition, cond),
			Cases: []model.Case{{Match: model.Matcher{Kind: "if", Pattern: cond}, Value: tv}}, Default: def, HadVars: true}
	}
	return false
}

func endsWithTerminal(nodes []*conf.Node) bool {
	for _, n := range nodes {
		if n.Name == "return" {
			return true
		}
		if n.Name == "rewrite" && (n.Arg(2) == "last" || n.Arg(2) == "redirect" || n.Arg(2) == "permanent") && matchesEverything[n.Arg(0)] {
			return true
		}
	}
	return false
}

func copyEnv(m map[string]*model.Expr) map[string]*model.Expr {
	n := make(map[string]*model.Expr, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t") {
			out[i] = strconv.Quote(a)
		} else {
			out[i] = a
		}
	}
	return out
}

// evalCond evaluates an if condition if possible.
// Grammar (doc: ngx_http_rewrite_module#if): a variable (false if "" or
// "0"); `=`/`!=` string comparison; `~`/`~*`/`!~`/`!~*` regex; `-f -d -e -x`
// and negations are file checks (filesystem-dependent, never evaluated).
func (r *runner) evalCond(args []string) (val, known bool) {
	switch len(args) {
	case 1:
		v := r.ev.eval(args[0])
		if v.IsLit() {
			return v.Value != "" && v.Value != "0", true
		}
		return false, false
	case 2:
		return false, false // file tests
	case 3:
		l := r.ev.eval(args[0])
		op := args[1]
		if !l.IsLit() {
			return false, false
		}
		switch op {
		case "=", "!=":
			rv := r.ev.eval(args[2])
			if !rv.IsLit() {
				return false, false
			}
			eq := l.Value == rv.Value
			return eq == (op == "="), true
		case "~", "~*", "!~", "!~*":
			re, ok := compileRegex(args[2], strings.Contains(op, "*"))
			if !ok {
				return false, false
			}
			m := re.MatchString(l.Value)
			return m == !strings.HasPrefix(op, "!"), true
		}
	}
	return false, false
}
