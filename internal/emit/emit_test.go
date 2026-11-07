package emit

import (
	"regexp"
	"strings"
	"testing"

	"nginx-sitemap/internal/model"
)

func sample() *model.Map {
	return &model.Map{SchemaVersion: "1", GeneratedAt: "2026-11-07T12:00:00Z", ConfigHash: "sha256:x",
		HTTP:   &model.HTTP{Listens: []*model.Listen{}, Servers: []*model.Server{}, Upstreams: []*model.Upstream{}, Tables: []*model.Table{}},
		Errors: []model.Diag{{Code: "parse-error", Message: "</script><script>alert(1)</script>"}}, Warnings: []model.Diag{}}
}

func TestHTMLSelfContained(t *testing.T) {
	b, err := HTML(sample())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if regexp.MustCompile(`(?i)<(script|link|img|iframe)[^>]+(src|href)=`).MatchString(s) {
		t.Error("index.html must not load external resources")
	}
	if strings.Count(s, "</script>") != 2 {
		t.Error("data block must not be able to close the script element")
	}
	// CSP hashes must match the inline script/style exactly.
	if !strings.Contains(s, cspHash(appJS)) || !strings.Contains(s, "<script>"+appJS+"</script>") {
		t.Error("script CSP hash/content mismatch")
	}
	if !strings.Contains(s, cspHash(appCSS)) || !strings.Contains(s, "<style>"+appCSS+"</style>") {
		t.Error("style CSP hash/content mismatch")
	}
}

func TestDOTQuoting(t *testing.T) {
	m := sample()
	m.External = []model.External{{Name: `we"ird`, Group: "g", Backends: []string{"10.0.0.1:1"}}}
	d := string(DOT(m))
	if !strings.Contains(d, `we\"ird`) || !strings.HasPrefix(d, "digraph") {
		t.Fatal(d)
	}
}
