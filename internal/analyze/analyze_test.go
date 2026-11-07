package analyze

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

func analyzeString(t *testing.T, cfg string, opt Options) *Result {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := conf.Load(p, conf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return Analyze(res, opt)
}

func httpCfg(body string) string { return "events {}\nhttp {\n" + body + "\n}\n" }

func TestParseValue(t *testing.T) {
	cases := map[string]string{
		"plain":           "L(plain)",
		"$host":           "V(host)",
		"a${b}c":          "L(a)V(b)L(c)",
		"/x/$1/$22":       "L(/x/)V(1)L(/)V(2)L(2)",
		"price $ 5":       "L(price $ 5)",
		"$http_x_a-b":     "V(http_x_a)L(-b)",
		"http://$up:8080": "L(http://)V(up)L(:8080)",
	}
	for in, want := range cases {
		var b strings.Builder
		for _, s := range parseValue(in) {
			if s.v != "" {
				b.WriteString("V(" + s.v + ")")
			} else {
				b.WriteString("L(" + s.lit + ")")
			}
		}
		if b.String() != want {
			t.Errorf("parseValue(%q) = %s, want %s", in, b.String(), want)
		}
	}
}

func TestClassifyName(t *testing.T) {
	cases := map[string]string{
		"Example.COM": "exact", "*.example.com": "leading-wildcard", ".example.com": "dot-wildcard",
		"www.example.*": "trailing-wildcard", "~^a\\.b$": "regex", "": "empty", "$hostname": "hostname",
		"w*w.example.com": "invalid",
	}
	for in, want := range cases {
		if k, _, _ := classifyName(in); k != want {
			t.Errorf("classifyName(%q) = %s, want %s", in, k, want)
		}
	}
	if _, n, _ := classifyName("Example.COM"); n != "example.com" {
		t.Errorf("names must be lower-cased, got %s", n)
	}
}

func TestParseListen(t *testing.T) {
	cases := []struct{ args, key string }{
		{"80", "http:*:80"},
		{"127.0.0.1", "http:127.0.0.1:80"},
		{"0.0.0.0:8080", "http:*:8080"},
		{"[::]:443 ssl", "http:[::]:443"},
		{"[::1]", "http:[::1]:80"},
		{"unix:/run/x.sock", "http:unix:/run/x.sock:0"},
		{"443 quic reuseport", "http:*:443/quic"},
		{"localhost:81", "http:localhost:81"},
	}
	for _, c := range cases {
		l, err := parseListen(&conf.Node{Name: "listen", Args: strings.Fields(c.args)}, "http")
		if err != nil {
			t.Fatalf("%s: %v", c.args, err)
		}
		if l.key != c.key {
			t.Errorf("listen %s → %s, want %s", c.args, l.key, c.key)
		}
	}
	if _, err := parseListen(&conf.Node{Name: "listen", Args: []string{"10.0.0.1"}}, "stream-tcp"); err == nil {
		t.Error("stream listen without port must be rejected")
	}
}

func TestImplicitListenNonSuperuser(t *testing.T) {
	r := analyzeString(t, httpCfg("server { server_name a; }"), Options{AssumeSuperuser: false})
	l := r.HTTP.Listens[0]
	if l.ID != "http:*:8000" || l.Implicit == nil || l.Implicit.Value != "*:8000" {
		t.Fatalf("got %+v", l)
	}
	if r.HTTP.Servers[0].ImplicitListen == nil {
		t.Error("server must record its implicit listen")
	}
}

func TestDuplicateDefaultServer(t *testing.T) {
	r := analyzeString(t, httpCfg("server { listen 80 default_server; } server { listen 80 default_server; }"), Options{AssumeSuperuser: true})
	if len(r.Errors) == 0 || !strings.Contains(r.Errors[0].Message, "duplicate default server") {
		t.Fatalf("errors: %+v", r.Errors)
	}
}

func locByURI(ls []*model.Location, uri string) *model.Location {
	for _, l := range ls {
		if l.URI == uri {
			return l
		}
		if c := locByURI(l.Children, uri); c != nil {
			return c
		}
	}
	return nil
}

func TestFindLocationOrder(t *testing.T) {
	r := analyzeString(t, httpCfg(`server {
  listen 80;
  location = /exact { return 200; }
  location / { return 200; }
  location /a/ { location ~ \.php$ { return 200; } }
  location ^~ /img/ { return 200; }
  location ~ \.(png|php)$ { return 200; }
  location /b/ { proxy_pass http://127.0.0.1:1; }
  location @named { return 200; }
}`), Options{})
	cases := map[string]string{
		"/exact":     "= /exact",
		"/exact/x":   "prefix /",
		"/a/x.php":   "~ \\.php$",       // nested regex wins
		"/a/x.png":   "~ \\.(png|php)$", // outer regex after nested search
		"/img/x.png": "^~ /img/",        // ^~ stops regex search
		"/other.png": "~ \\.(png|php)$", // regex beats prefix "/"
		"/b":         "prefix /b/",      // auto_redirect (301) for *_pass with trailing slash
		"/zzz":       "prefix /",
	}
	locs := rebuild(t, r)
	for uri, want := range cases {
		got, unc := findLocation(locs, uri)
		if unc || got == nil {
			t.Errorf("%s: no match (uncertain=%v)", uri, unc)
			continue
		}
		if g := got.m.Modifier + " " + got.m.URI; g != want {
			t.Errorf("%s → %s, want %s", uri, g, want)
		}
	}
}

// rebuild reconstructs locCtx trees from the model (regexes recompiled).
func rebuild(t *testing.T, r *Result) []*locCtx {
	var conv func(ls []*model.Location, parent *locCtx) []*locCtx
	conv = func(ls []*model.Location, parent *locCtx) []*locCtx {
		var out []*locCtx
		for _, l := range ls {
			lc := &locCtx{m: l, parent: parent}
			if l.Modifier == "~" || l.Modifier == "~*" {
				re, ok := compileRegex(l.URI, l.Modifier == "~*")
				lc.re, lc.reBad = re, !ok
			}
			lc.children = conv(l.Children, lc)
			out = append(out, lc)
		}
		return out
	}
	return conv(r.HTTP.Servers[0].Locations, nil)
}

func handlerOf(t *testing.T, r *Result, uri string) *model.Handler {
	t.Helper()
	l := locByURI(r.HTTP.Servers[0].Locations, uri)
	if l == nil {
		t.Fatalf("location %s not found", uri)
	}
	return l.Handler
}

func TestMapSemantics(t *testing.T) {
	r := analyzeString(t, httpCfg(`
map "WWW.Example.COM" $ci { default no; www.example.com yes; }
map "x.sub.example" $wc { hostnames; default no; *.sub.example yes; }
map "abc" $re { default no; ~^a(b)c$ yes; }
map $a_loop $b_loop { default $c_loop; }
map $b_loop $c_loop { default x; }
map $c_loop $a_loop { default y; }
server { listen 80;
  location /ci { return 200 $ci; }
  location /wc { return 200 $wc; }
  location /re { return 200 $re; }
  location /loop { return 200 $a_loop; }
}`), Options{})
	for uri, want := range map[string]string{"/ci": "yes", "/wc": "yes", "/re": "yes"} {
		h := handlerOf(t, r, uri)
		if !h.Target.IsLit() || h.Target.Value != want || h.Status != model.StatusResolved {
			t.Errorf("%s: %s (%s), want %s resolved", uri, h.Target, h.Status, want)
		}
	}
	h := handlerOf(t, r, "/loop")
	found := false
	for _, rs := range h.Reasons {
		if rs.Code == model.ReasonCycle {
			found = true
		}
	}
	if !found {
		t.Errorf("map cycle not reported: %+v", h.Reasons)
	}
}

func TestPCREOnlyRegexIsUnsupported(t *testing.T) {
	r := analyzeString(t, httpCfg(`map "abc" $v { default no; ~^(?=a)abc yes; } server { listen 80; location / { return 200 $v; } }`), Options{})
	h := handlerOf(t, r, "/")
	if h.Status == model.StatusResolved {
		t.Fatalf("lookahead must not be evaluated with RE2: %s", h.Target)
	}
	if r.HTTP.Tables[0].Reasons[0].Code != model.ReasonUnsupportedRe {
		t.Errorf("table reasons: %+v", r.HTTP.Tables[0].Reasons)
	}
}

func TestIfStatic(t *testing.T) {
	r := analyzeString(t, httpCfg(`server { listen 80; set $a 1;
  location /f { if ($a = 2) { return 403; } return 200 ok; }
  location /t { if ($a) { return 403; } return 200 ok; }
}`), Options{})
	if h := handlerOf(t, r, "/f"); h.Code != 200 {
		t.Errorf("/f: statically false if must be skipped, got %d", h.Code)
	}
	if h := handlerOf(t, r, "/t"); h.Code != 403 {
		t.Errorf("/t: statically true if must apply, got %d", h.Code)
	}
}

func TestUpstreamPortError(t *testing.T) {
	r := analyzeString(t, httpCfg(`upstream app { server 10.0.0.1:80; } server { listen 80; location / { proxy_pass http://app:8080; } }`), Options{})
	if len(r.Errors) == 0 || !strings.Contains(r.Errors[0].Message, "may not have port") {
		t.Fatalf("errors: %+v", r.Errors)
	}
}

func TestNoResolverWarning(t *testing.T) {
	r := analyzeString(t, httpCfg(`server { listen 80; location / { set $h api.example; proxy_pass http://$h; } }`), Options{})
	if len(r.Warnings) == 0 || r.Warnings[0].Code != "no-resolver" {
		t.Fatalf("warnings: %+v", r.Warnings)
	}
}

func TestBranchLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("map $http_x $big {\n default d;\n")
	for i := 0; i < 400; i++ {
		b.WriteString("  k")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(itoa(i))
		b.WriteString(" v;\n")
	}
	b.WriteString("}\nserver { listen 80; location / { return 200 $big; } }")
	r := analyzeString(t, httpCfg(b.String()), Options{})
	h := handlerOf(t, r, "/")
	if h.Status != model.StatusDynamic || h.Reasons[0].Code != model.ReasonBranchLimit {
		t.Fatalf("got %s %+v", h.Status, h.Reasons)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestMasking(t *testing.T) {
	r := analyzeString(t, httpCfg(`server { listen 80;
  ssl_certificate_key data:-----BEGIN PRIVATE KEY-----xyz;
  ssl_certificate /etc/nginx/private/cert.pem;
  location / { proxy_set_header Cookie "s=1"; fastcgi_param HTTP_AUTHORIZATION "x"; set $db_password hunter2; return 200 $db_password; }
}`), Options{Mask: MaskOptions{Paths: []string{"/etc/nginx/private/*"}}})
	h := handlerOf(t, r, "/")
	if strings.Contains(h.Target.String(), "hunter2") || h.Target.String() != Masked {
		t.Errorf("set value not masked: %s", h.Target)
	}
}

func TestVersionAtLeast(t *testing.T) {
	if !versionAtLeast("1.30.0", "1.25.5") || versionAtLeast("1.24.0", "1.25.5") || !versionAtLeast("", "9") {
		t.Fatal("versionAtLeast")
	}
}
