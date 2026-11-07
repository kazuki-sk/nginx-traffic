package app

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nginx-sitemap/internal/schemacheck"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixedNow() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

func ptr[T any](v T) *T { return &v }

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// runFixture analyses dir/nginx.conf (and dir/sitemap.conf when present).
func runFixture(t *testing.T, dir string) (*Outcome, []byte) {
	t.Helper()
	o := Options{
		NginxConf:    ptr(filepath.Join(dir, "nginx.conf")),
		OutputDir:    ptr(""),
		NginxVersion: ptr("1.30.0"),
		ToolVersion:  "test",
		Now:          fixedNow,
		LookupHost: func(ctx context.Context, h string) ([]string, error) {
			return []string{"192.0.2.1"}, nil
		},
	}
	if _, err := os.Stat(filepath.Join(dir, "sitemap.conf")); err == nil {
		o.ConfigFile = filepath.Join(dir, "sitemap.conf")
	}
	if _, err := os.Stat(filepath.Join(dir, "resolve")); err == nil {
		o.Resolve = ptr(true)
	}
	res, err := Run(o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var ind bytes.Buffer
	if err := json.Indent(&ind, res.Files["map.json"], "", "  "); err != nil {
		t.Fatal(err)
	}
	js := ind.Bytes()
	// Normalise machine-specific absolute paths.
	js = bytes.ReplaceAll(js, []byte(repoRoot(t)), []byte("<REPO>"))
	return res, js
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run go test ./internal/app -update): %v", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from golden; rerun with -update after reviewing.\n%s", path, firstDiff(string(want), string(got)))
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + itoa(i+1) + ":\n  want: " + x + "\n  got:  " + y
		}
	}
	return ""
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestGoldenValid(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join(repoRoot(t), "testdata", "fixtures", "*"))
	if len(dirs) == 0 {
		t.Fatal("no fixtures")
	}
	for _, d := range dirs {
		d := d
		t.Run(filepath.Base(d), func(t *testing.T) {
			res, js := runFixture(t, d)
			golden(t, filepath.Join(d, "golden.map.json"), js)
			if res.Code != ExitOK {
				t.Errorf("exit code %d, want 0 (fixtures must be nginx -t valid): %v", res.Code, res.Map.Errors)
			}
		})
	}
}

func TestGoldenInvalid(t *testing.T) {
	cases := map[string]string{"include_cycle": "cycle", "syntax_error": "parse-error"}
	for name, code := range cases {
		name, code := name, code
		t.Run(name, func(t *testing.T) {
			d := filepath.Join(repoRoot(t), "testdata", "invalid", name)
			res, js := runFixture(t, d)
			golden(t, filepath.Join(d, "golden.map.json"), js)
			if res.Code != ExitPartial {
				t.Errorf("exit code %d, want 1", res.Code)
			}
			found := false
			for _, e := range res.Map.Errors {
				if e.Code == code {
					found = true
				}
			}
			if !found {
				t.Errorf("no %q error in %+v", code, res.Map.Errors)
			}
		})
	}
}

func TestGoldensMatchSchema(t *testing.T) {
	schema, err := os.ReadFile(filepath.Join(repoRoot(t), "schema", "map.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(repoRoot(t), "testdata", "*", "*", "golden.map.json"))
	if len(files) == 0 {
		t.Fatal("no goldens")
	}
	for _, f := range files {
		doc, _ := os.ReadFile(f)
		if errs := schemacheck.Validate(schema, doc); len(errs) > 0 {
			if len(errs) > 10 {
				errs = errs[:10]
			}
			t.Errorf("%s:\n  %s", f, strings.Join(errs, "\n  "))
		}
	}
}
