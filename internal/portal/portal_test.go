package portal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nginx-sitemap/internal/analyze"
	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

func build(t *testing.T, cfg string) *model.Portal {
	t.Helper()
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "snip"), 0o755)
	os.WriteFile(filepath.Join(d, "snip", "proxy.inc"), []byte("proxy_set_header X-Real-IP $remote_addr;\n"), 0o644)
	p := filepath.Join(d, "nginx.conf")
	os.WriteFile(p, []byte(cfg), 0o644)
	res, err := conf.Load(p, conf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ar := analyze.Analyze(res, analyze.Options{AssumeSuperuser: true})
	pt, _ := Build(Input{Analysis: ar, HTTP: ar.HTTP, Stream: ar.Stream, ConfPrefix: res.ConfPrefix})
	return pt
}

func site(p *model.Portal, name string) *model.Site {
	for _, s := range p.Sites {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func TestSites(t *testing.T) {
	p := build(t, `events {} http {
  client_max_body_size 10m;
  upstream app { server 10.0.0.1:8080; server 10.0.0.2:8080 backup; }
  server { listen 443 ssl; listen 80; server_name shop.example.co.jp www.shop.example.co.jp;
    allow 10.0.0.0/16; allow 192.168.0.0/24; deny all; auth_basic "staff";
    location / { proxy_pass http://app; include snip/proxy.inc; }
    location /g/ { grpc_pass 10.0.0.9:50051; } }
  server { listen 8443 ssl; server_name *.preview.example.internal; location / { proxy_pass https://10.0.0.5; } }
  server { listen 80 default_server; return 444; }
}`)
	s := site(p, "shop.example.co.jp")
	if s == nil {
		t.Fatalf("sites: %+v", p.Sites)
	}
	if s.URL != "https://shop.example.co.jp/" || s.Group != "example.co.jp" || s.GroupSource != "domain" {
		t.Errorf("url/group: %s %s %s", s.URL, s.Group, s.GroupSource)
	}
	if len(s.Aliases) != 1 || s.Aliases[0] != "www.shop.example.co.jp" {
		t.Errorf("aliases %v", s.Aliases)
	}
	if s.Access.Summary != "10.0.0.0/16、192.168.0.0/24 のみ" || len(s.Access.Auth) != 1 || s.Access.Auth[0] != "Basic 認証" {
		t.Errorf("access %+v", s.Access)
	}
	want := "http://10.0.0.1:8080/ http://10.0.0.2:8080/ grpc://10.0.0.9:50051/"
	if got := strings.Join(s.InnerURLs, " "); got != want {
		t.Errorf("inner: %s", got)
	}
	var marker, inc bool
	for _, l := range s.Config {
		marker = marker || (l.Kind == "marker" && strings.Contains(l.Text, "snip/proxy.inc"))
		inc = inc || (l.Kind == "include" && l.Src == "snip/proxy.inc:1")
	}
	if !marker || !inc {
		t.Errorf("config lines: %+v", s.Config)
	}
	if len(p.HTTPConfig) != 1 || p.HTTPConfig[0].Text != "client_max_body_size 10m;" {
		t.Errorf("http config %+v", p.HTTPConfig)
	}
	if len(p.UpstreamConfig["http:app"]) != 4 {
		t.Errorf("upstream config %+v", p.UpstreamConfig)
	}

	w := site(p, "*.preview.example.internal")
	if w == nil || w.URL != "" || w.Group != "example.internal" || strings.Join(w.InnerURLs, " ") != "https://10.0.0.5/" {
		t.Errorf("wildcard site: %+v", w)
	}
	var def *model.Site
	for _, x := range p.Sites {
		if strings.HasPrefix(x.Name, "（名前なし）") {
			def = x
		}
	}
	if def == nil || def.Name != "（名前なし） *:80" || def.GroupSource != "default" {
		t.Errorf("default server site: %+v", def)
	}
}

func TestDefaultGroup(t *testing.T) {
	cases := map[string]string{
		"shop.example.co.jp": "example.co.jp", "example.co.jp": "example.co.jp", "a.b.example.com": "example.com",
		"example.com": "example.com", "wiki.corp.example.internal": "example.internal", "*.example.org": "example.org",
		"localhost": "localhost",
	}
	for in, want := range cases {
		g, _ := defaultGroup(&model.ServerName{Name: in, Kind: "exact"})
		if g != want {
			t.Errorf("%s → %s, want %s", in, g, want)
		}
	}
	if g, src := defaultGroup(&model.ServerName{Name: "~^x", Kind: "regex"}); src != "default" || g == "" {
		t.Errorf("regex: %s %s", g, src)
	}
}

func TestEntryURLPorts(t *testing.T) {
	ls := map[string]*model.Listen{
		"a": {ID: "a", Port: 8443, SSL: true}, "b": {ID: "b", Port: 80}, "c": {ID: "c", Port: 443, SSL: true, QUIC: true},
	}
	if u := entryURL("x.test", []string{"b", "a"}, ls); u != "https://x.test:8443/" {
		t.Errorf("got %s", u)
	}
	if u := entryURL("x.test", []string{"b"}, ls); u != "http://x.test/" {
		t.Errorf("got %s", u)
	}
	if u := entryURL("x.test", []string{"c"}, ls); u != "" {
		t.Errorf("quic-only listen must not produce a URL: %s", u)
	}
}

func TestSelfProxy(t *testing.T) {
	cfg, err := os.ReadFile("../../testdata/fixtures/self_proxy/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	p := filepath.Join(d, "nginx.conf")
	os.WriteFile(p, cfg, 0o644)
	res, _ := conf.Load(p, conf.Options{})
	ar := analyze.Analyze(res, analyze.Options{AssumeSuperuser: true})
	pt, warns := Build(Input{Analysis: ar, HTTP: ar.HTTP, ConfPrefix: res.ConfPrefix})

	www := site(pt, "www.example.test")
	got := map[string]string{}
	for _, h := range www.Hops {
		got[h.Via+"|"+h.Upstream] = site(pt, "cache.local").ID
		if h.TargetSite == site(pt, "auth.local").ID {
			got[h.Via+"|"+h.Upstream] = "auth"
		} else if h.TargetSite == site(pt, "cache.local").ID {
			got[h.Via+"|"+h.Upstream] = "cache"
		} else {
			got[h.Via+"|"+h.Upstream] = "other:" + h.TargetSite
		}
	}
	want := map[string]string{"127.0.0.1:8081|": "cache", "127.0.0.1:8082|": "auth", "127.0.0.1:8081|cache_layer": "cache"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("hop %s → %s, want %s (all: %v)", k, got[k], v, got)
		}
	}
	if len(www.Hops) != 3 {
		t.Errorf("hops: %d", len(www.Hops))
	}
	// final destinations: through the cache and auth layers, plus /direct/
	if g := strings.Join(www.InnerURLs, " "); g != "http://10.5.0.3:8080/ http://10.5.0.1:8080/ http://10.5.0.2:8080/ http://10.5.0.9:3000/ http://10.5.0.10:9000/" {
		t.Errorf("inner: %s", g)
	}
	cache := site(pt, "cache.local")
	if !cache.Internal || cache.URL != "" || cache.Group != "nginx 内部" || len(cache.Callers) != 1 || cache.Callers[0] != www.ID {
		t.Errorf("cache site: internal=%v group=%s callers=%v", cache.Internal, cache.Group, cache.Callers)
	}
	if site(pt, "other.local").Callers != nil {
		t.Error("other.local must not be selected for Host auth.local")
	}
	loop := false
	for _, w := range warns {
		loop = loop || w.Code == "self-proxy-loop"
	}
	if !loop {
		t.Errorf("loop not reported: %+v", warns)
	}
}

func TestListenLookup(t *testing.T) {
	ix := newListenIndex([]*model.Listen{
		{ID: "w", Proto: "http", Addr: "*", Port: 8081, Family: "inet"},
		{ID: "e", Proto: "http", Addr: "127.0.0.1", Port: 8081, Family: "inet"},
		{ID: "pub", Proto: "http", Addr: "10.0.0.10", Port: 443, Family: "inet", SSL: true},
		{ID: "w443", Proto: "http", Addr: "*", Port: 443, Family: "inet", SSL: true},
		{ID: "v6", Proto: "http", Addr: "[::]", Port: 8081, Family: "inet6"},
		{ID: "u", Proto: "http", Addr: "unix:/run/ng.sock", Family: "unix"},
	})
	cases := map[string]string{
		"127.0.0.1:8081": "e", "127.0.0.2:8081": "w", "localhost:8081": "e", "[::1]:8081": "v6",
		"10.0.0.10:443": "pub", "10.0.0.11:443": "", "10.9.9.9:8081": "", "unix:/run/ng.sock": "u", "app.example:8081": "",
	}
	for addr, want := range cases {
		got := ""
		if l := ix.lookup(addr); l != nil {
			got = l.ID
		}
		if got != want {
			t.Errorf("%s → %q, want %q", addr, got, want)
		}
	}
}
