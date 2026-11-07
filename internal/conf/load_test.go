package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func names(nodes []*Node) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name+":"+strings.Join(n.Args, ","))
	}
	return out
}

func TestIncludeRelativeToConfPrefix(t *testing.T) {
	d := t.TempDir()
	write(t, d, map[string]string{
		"nginx.conf":       "include sites/*.conf;",
		"sites/a.conf":     "include snippets/x.inc;", // relative to d, not d/sites
		"snippets/x.inc":   "set $x 1;",
		"sites/.skip.conf": "set $hidden 1;",
		"sites/b.conf":     "set $b 1;",
	})
	r, err := Load(filepath.Join(d, "nginx.conf"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(r.Main.Block), " ")
	if got != "set:$x,1 set:$b,1" {
		t.Fatalf("got %q (errors %+v)", got, r.Errors)
	}
	x := r.Main.Block[0]
	if x.Src.File != "snippets/x.inc" || len(x.Src.Chain) != 2 || x.Src.Chain[0] != "nginx.conf:1" {
		t.Fatalf("src %+v", x.Src)
	}
	for _, f := range r.Files {
		if f.Path == "snippets/x.inc" && (len(f.IncludedFrom) != 1 || f.IncludedFrom[0] != "nginx.conf:1 > sites/a.conf:1") {
			t.Fatalf("included_from %+v", f.IncludedFrom)
		}
	}
}

func TestIncludeErrorsAndCycle(t *testing.T) {
	d := t.TempDir()
	write(t, d, map[string]string{
		"nginx.conf": "include missing.conf; include nomatch/*.conf; include a.conf; set $after 1;",
		"a.conf":     "set $a 1; include a.conf;",
	})
	r, err := Load(filepath.Join(d, "nginx.conf"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]int{}
	for _, e := range r.Errors {
		codes[e.Code]++
	}
	if codes["include-error"] != 1 || codes["cycle"] != 1 {
		t.Fatalf("errors %+v", r.Errors)
	}
	if got := strings.Join(names(r.Main.Block), " "); got != "set:$a,1 set:$after,1" {
		t.Fatalf("partial result lost: %q", got)
	}
}

func TestSameFileInSeveralContexts(t *testing.T) {
	d := t.TempDir()
	write(t, d, map[string]string{
		"nginx.conf": "http { server { include p.inc; } server { include p.inc; } }",
		"p.inc":      "proxy_set_header A b;",
	})
	r, _ := Load(filepath.Join(d, "nginx.conf"), Options{})
	srv := r.Main.Block[0].Block
	for i, s := range srv {
		if len(s.Block) != 1 || s.Block[0].Parent != s {
			t.Fatalf("server %d: %+v", i, s.Block)
		}
	}
	if len(r.Files) != 2 {
		t.Fatalf("files %+v", r.Files)
	}
}

func TestMainUnreadable(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.conf"), Options{}); err == nil {
		t.Fatal("expected fatal error")
	}
}

func TestHashStable(t *testing.T) {
	a := []File{{Path: "b", SHA256: "2", Size: 1}, {Path: "a", SHA256: "1", Size: 1}}
	b := []File{a[1], a[0]}
	if Hash(a, "v1") != Hash(b, "v1") || Hash(a, "v1") == Hash(a, "v2") {
		t.Fatal("hash must be order-independent and include extras")
	}
}
