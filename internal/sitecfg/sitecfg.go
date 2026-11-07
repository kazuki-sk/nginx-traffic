// Package sitecfg reads the tool's own configuration file (sitemap.conf):
// tool settings plus manually declared entries for things that do not go
// through nginx (e.g. an independent monitoring stack).
//
// The file uses nginx-like syntax and is parsed with the same parser, so
// every manual entry carries file:line. It must live outside the nginx
// configuration tree so nginx never includes it.
package sitecfg

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

type Settings struct {
	NginxConf       string
	AssumeSuperuser bool
	NginxVersion    string
	OutputDir       string
	SkipUnchanged   bool
	Resolve         bool
	MaskPaths       []string
	MaskHeaders     []string
	MaskValues      bool
	InheritShow     []string
	InheritHide     []string
	StaleAfter      time.Duration
	Dot             bool
	// ReadCertificates enables reading the files named by ssl_certificate
	// (certificates only, never keys) to show expiry dates.
	ReadCertificates bool
}

func Defaults() Settings {
	return Settings{
		NginxConf:       "/etc/nginx/nginx.conf",
		AssumeSuperuser: true,
		OutputDir:       "/var/lib/nginx-sitemap",
		MaskValues:      true,
		StaleAfter:      180 * 24 * time.Hour,
		Dot:             true,
	}
}

type AnnotationTarget struct {
	Kind string // server | upstream | stream_server | stream_upstream
	Name string
	A    *model.Annotation
}

type File struct {
	Settings    Settings
	External    []model.External
	Annotations []AnnotationTarget
	Files       []conf.File
	Errors      []model.Diag
}

func (f *File) errorf(src conf.Src, format string, args ...any) {
	s := src
	f.Errors = append(f.Errors, model.Diag{Code: "manual-error", Message: fmt.Sprintf(format, args...), Src: &s})
}

// Load reads path. A missing file is not an error when optional is true.
func Load(path string) (*File, error) {
	f := &File{Settings: Defaults()}
	res, err := conf.Load(path, conf.Options{})
	if err != nil {
		return nil, err
	}
	f.Files = res.Files
	for _, e := range res.Errors {
		s := e.Src
		f.Errors = append(f.Errors, model.Diag{Code: e.Code, Message: e.Msg, Src: &s})
	}
	for _, n := range res.Main.Block {
		switch n.Name {
		case "settings":
			f.settings(n)
		case "external":
			f.external(n)
		case "annotate":
			f.annotate(n)
		default:
			f.errorf(n.Src, "unknown top-level directive %q (expected settings, external, annotate, include)", n.Name)
		}
	}
	return f, nil
}

func onOff(n *conf.Node, f *File) bool {
	switch n.Arg(0) {
	case "on":
		return true
	case "off":
		return false
	}
	f.errorf(n.Src, "%s: expected on|off, got %q", n.Name, n.Arg(0))
	return false
}

func (f *File) settings(b *conf.Node) {
	s := &f.Settings
	for _, n := range b.Block {
		switch n.Name {
		case "nginx_conf":
			s.NginxConf = n.Arg(0)
		case "assume_superuser":
			s.AssumeSuperuser = onOff(n, f)
		case "nginx_version":
			s.NginxVersion = n.Arg(0)
		case "output_dir":
			s.OutputDir = n.Arg(0)
		case "skip_unchanged":
			s.SkipUnchanged = onOff(n, f)
		case "resolve":
			s.Resolve = onOff(n, f)
		case "mask_paths":
			s.MaskPaths = append(s.MaskPaths, n.Args...)
		case "mask_header":
			s.MaskHeaders = append(s.MaskHeaders, n.Args...)
		case "mask_values":
			s.MaskValues = onOff(n, f)
		case "inherited_show":
			s.InheritShow = append(s.InheritShow, n.Args...)
		case "inherited_hide":
			s.InheritHide = append(s.InheritHide, n.Args...)
		case "stale_after":
			d, err := parseDays(n.Arg(0))
			if err != nil {
				f.errorf(n.Src, "stale_after: %v", err)
			}
			s.StaleAfter = d
		case "dot":
			s.Dot = onOff(n, f)
		case "read_certificates":
			s.ReadCertificates = onOff(n, f)
		default:
			f.errorf(n.Src, "unknown setting %q", n.Name)
		}
	}
}

// parseDays accepts "180d", "12h", or "0" (disabled).
func parseDays(v string) (time.Duration, error) {
	if v == "0" || v == "off" {
		return 0, nil
	}
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

func manualMeta(src conf.Src) model.Meta {
	return model.Meta{Src: &src, Origin: model.OriginManual, Status: model.StatusDeclared}
}

func (f *File) external(b *conf.Node) {
	if len(b.Args) != 1 || !b.IsBlock {
		f.errorf(b.Src, "usage: external <name> { ... }")
		return
	}
	e := model.External{Meta: manualMeta(b.Src), Name: b.Args[0]}
	for _, n := range b.Block {
		switch n.Name {
		case "group":
			e.Group = n.Arg(0)
		case "endpoint":
			e.Endpoints = append(e.Endpoints, n.Args...)
		case "listen":
			e.Listens = append(e.Listens, strings.Join(n.Args, " "))
		case "backend":
			e.Backends = append(e.Backends, n.Args...)
		case "depends":
			e.Depends = append(e.Depends, n.Args...)
		case "owner":
			e.Owner = strings.Join(n.Args, " ")
		case "note":
			e.Note = strings.Join(n.Args, " ")
		case "tag":
			e.Tags = append(e.Tags, n.Args...)
		case "link":
			if l, ok := f.link(n); ok {
				e.Links = append(e.Links, l)
			}
		case "reviewed":
			if _, err := time.Parse("2006-01-02", n.Arg(0)); err != nil {
				f.errorf(n.Src, "reviewed: expected YYYY-MM-DD, got %q", n.Arg(0))
			}
			e.Reviewed = n.Arg(0)
		default:
			f.errorf(n.Src, "unknown key %q in external %q", n.Name, e.Name)
		}
	}
	for _, x := range f.External {
		if x.Name == e.Name {
			f.errorf(b.Src, "duplicate external %q", e.Name)
			return
		}
	}
	f.External = append(f.External, e)
}

// link parses `link <name> <url>;`.
func (f *File) link(n *conf.Node) (model.Link, bool) {
	if len(n.Args) != 2 {
		f.errorf(n.Src, "usage: link <name> <url>")
		return model.Link{}, false
	}
	u := n.Args[1]
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		f.errorf(n.Src, "link %q: URL must start with http:// or https://", n.Args[0])
		return model.Link{}, false
	}
	return model.Link{Name: n.Args[0], URL: u}, true
}

var annotateKinds = map[string]bool{"server": true, "upstream": true, "stream_server": true, "stream_upstream": true}

func (f *File) annotate(b *conf.Node) {
	if len(b.Args) != 2 || !annotateKinds[b.Args[0]] {
		f.errorf(b.Src, "usage: annotate server|upstream|stream_server|stream_upstream <name> { ... }")
		return
	}
	a := &model.Annotation{Meta: manualMeta(b.Src)}
	for _, n := range b.Block {
		switch n.Name {
		case "owner":
			a.Owner = strings.Join(n.Args, " ")
		case "note":
			a.Note = strings.Join(n.Args, " ")
		case "tag":
			a.Tags = append(a.Tags, n.Args...)
		case "expect":
			a.Expect = append(a.Expect, n.Args...)
		case "group":
			a.Group = strings.Join(n.Args, " ")
		case "link":
			if l, ok := f.link(n); ok {
				a.Links = append(a.Links, l)
			}
		default:
			f.errorf(n.Src, "unknown key %q in annotate", n.Name)
		}
	}
	f.Annotations = append(f.Annotations, AnnotationTarget{Kind: b.Args[0], Name: b.Args[1], A: a})
}

// Validate cross-checks manual entries against the analysis and returns
// warnings (stale annotations, overdue reviews, unknown depends).
func (f *File) Validate(now time.Time) []model.Diag {
	var w []model.Diag
	names := map[string]bool{}
	for _, e := range f.External {
		names[e.Name] = true
	}
	for _, e := range f.External {
		for _, d := range e.Depends {
			if !names[d] {
				w = append(w, model.Diag{Code: "manual-unknown-depends", Message: fmt.Sprintf("external %q depends on unknown external %q", e.Name, d), Src: e.Src})
			}
		}
		if e.Reviewed != "" && f.Settings.StaleAfter > 0 {
			if t, err := time.Parse("2006-01-02", e.Reviewed); err == nil && now.Sub(t) > f.Settings.StaleAfter {
				w = append(w, model.Diag{Code: "stale-manual-entry", Message: fmt.Sprintf("external %q was last reviewed %s (older than stale_after)", e.Name, e.Reviewed), Src: e.Src})
			}
		}
	}
	return w
}
