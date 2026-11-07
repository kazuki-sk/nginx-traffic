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
	return Build(Input{Analysis: ar, HTTP: ar.HTTP, Stream: ar.Stream, ConfPrefix: res.ConfPrefix})
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
