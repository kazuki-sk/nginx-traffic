package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExitFatalOnMissingMain(t *testing.T) {
	res, err := Run(Options{NginxConf: ptr("/nonexistent/nginx.conf"), OutputDir: ptr(""), Now: fixedNow})
	if err == nil || res.Code != ExitFatal {
		t.Fatalf("code %v err %v", res, err)
	}
}

func TestWriteAndSkipUnchanged(t *testing.T) {
	out := t.TempDir()
	fx := filepath.Join(repoRoot(t), "testdata", "fixtures", "upstream", "nginx.conf")
	o := Options{NginxConf: ptr(fx), OutputDir: ptr(out), SkipUnchanged: ptr(true), Now: fixedNow, ToolVersion: "t"}
	r1, err := Run(o)
	if err != nil || r1.Skipped || r1.Code != ExitOK {
		t.Fatalf("first run: %+v %v", r1, err)
	}
	for _, n := range []string{"map.json", "index.html", "map.dot"} {
		st, err := os.Stat(filepath.Join(out, n))
		if err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("%s: %v %v", n, err, st)
		}
	}
	ents, _ := os.ReadDir(out)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	r2, _ := Run(o)
	if !r2.Skipped {
		t.Fatal("second run with identical configuration must be skipped")
	}
	o.Resolve = ptr(true)
	o.LookupHost = func(context.Context, string) ([]string, error) { return nil, nil }
	if r3, _ := Run(o); r3.Skipped {
		t.Fatal("--resolve must never be skipped")
	}
}

func TestResolveSnapshot(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "nginx.conf"), []byte(`events {} http {
  upstream u { server app.internal.example:8080; server 10.0.0.1:80; }
  server { listen 80; location / { proxy_pass http://u; } }
}`), 0o644)
	calls := map[string]int{}
	res, err := Run(Options{NginxConf: ptr(filepath.Join(d, "nginx.conf")), OutputDir: ptr(""), Resolve: ptr(true), Now: fixedNow,
		LookupHost: func(_ context.Context, h string) ([]string, error) {
			calls[h]++
			if h == "app.internal.example" {
				return []string{"10.9.9.2", "10.9.9.1"}, nil
			}
			return nil, fmt.Errorf("unexpected lookup %s", h)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Map.DNS.Mode != "snapshot" || res.Map.DNS.SnapshotAt == "" {
		t.Fatalf("dns %+v", res.Map.DNS)
	}
	m := res.Map.HTTP.Upstreams[0].Members
	if m[0].Snapshot == nil || strings.Join(m[0].Snapshot.Addrs, ",") != "10.9.9.1,10.9.9.2" || m[1].Snapshot != nil {
		t.Fatalf("snapshot %+v %+v", m[0].Snapshot, m[1].Snapshot)
	}
	if len(calls) != 1 {
		t.Fatalf("IP literals must not be looked up: %v", calls)
	}
}

// Target: a few hundred servers in well under a few seconds and < 50MB RSS.
// RSS is checked separately with the built binary (see README); here we
// bound time and Go heap.
func TestScale(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	d := t.TempDir()
	var b strings.Builder
	b.WriteString("events {}\nhttp {\n map $host $pool { hostnames; default 10.0.0.1:80;\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "  h%d.example 10.0.%d.%d:80;\n", i, i/250, i%250+1)
	}
	b.WriteString(" }\n upstream app { server 10.1.0.1:80; server 10.1.0.2:80 backup; }\n")
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, " server { listen 443 ssl; server_name s%d.example *.s%d.example; set $v %d;\n", i, i, i)
		b.WriteString("  location / { proxy_pass http://app; proxy_set_header Host $host; }\n")
		b.WriteString("  location ~ \\.php$ { fastcgi_pass unix:/run/php.sock; }\n")
		b.WriteString("  location /api/ { proxy_pass http://$pool; }\n")
		b.WriteString("  location = /health { return 200 ok; }\n")
		b.WriteString("  location /old { rewrite ^ /api/v1 last; }\n }\n")
	}
	b.WriteString("}\n")
	os.WriteFile(filepath.Join(d, "nginx.conf"), []byte(b.String()), 0o644)
	runtime.GC()
	start := time.Now()
	out := t.TempDir()
	res, err := Run(Options{NginxConf: ptr(filepath.Join(d, "nginx.conf")), OutputDir: ptr(out), Now: fixedNow})
	el := time.Since(start)
	if err != nil || res.Code != ExitOK {
		t.Fatalf("%v %+v", err, res.Map.Errors)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	js, _ := os.Stat(filepath.Join(out, "map.json"))
	t.Logf("600 servers / 3000 locations: %v, map.json %d KB, heap in use %d MB", el, js.Size()/1024, ms.HeapInuse>>20)
	if el > 5*time.Second {
		t.Errorf("too slow: %v", el)
	}
}
