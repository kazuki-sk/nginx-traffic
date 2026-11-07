// Package app wires loading, analysis, manual entries and output together.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"nginx-sitemap/internal/analyze"
	"nginx-sitemap/internal/atomicfs"
	"nginx-sitemap/internal/certs"
	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/emit"
	"nginx-sitemap/internal/model"
	"nginx-sitemap/internal/portal"
	"nginx-sitemap/internal/sitecfg"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitPartial = 1 // output written, but errors were found
	ExitFatal   = 2
)

// Options for one run. Pointer fields override sitemap.conf when non-nil.
type Options struct {
	ConfigFile         string // sitemap.conf; "" = none
	ConfigFileOptional bool   // missing ConfigFile is not an error

	NginxConf       *string
	OutputDir       *string
	Resolve         *bool
	SkipUnchanged   *bool
	AssumeSuperuser *bool
	NginxVersion    *string
	Dot             *bool

	ToolVersion string
	Now         func() time.Time
	LookupHost  func(ctx context.Context, host string) ([]string, error)
	Log         io.Writer
}

// Outcome of a run.
type Outcome struct {
	Code    int
	Skipped bool
	Map     *model.Map
	Files   map[string][]byte // name → content (also written to OutputDir unless empty)
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// Run executes one generation. It never modifies nginx configuration and
// never signals nginx.
func Run(o Options) (*Outcome, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ToolVersion == "" {
		o.ToolVersion = "dev"
	}
	// Bound the heap well below the 50MB RSS target.
	debug.SetMemoryLimit(40 << 20)

	sc := &sitecfg.File{Settings: sitecfg.Defaults()}
	if o.ConfigFile != "" {
		if _, err := os.Stat(o.ConfigFile); err != nil && o.ConfigFileOptional && errors.Is(err, os.ErrNotExist) {
			o.logf("settings file %s not found; using defaults", o.ConfigFile)
		} else {
			f, err := sitecfg.Load(o.ConfigFile)
			if err != nil {
				return &Outcome{Code: ExitFatal}, fmt.Errorf("settings: %w", err)
			}
			sc = f
		}
	}
	st := &sc.Settings
	if o.NginxConf != nil {
		st.NginxConf = *o.NginxConf
	}
	if o.OutputDir != nil {
		st.OutputDir = *o.OutputDir
	}
	if o.Resolve != nil {
		st.Resolve = *o.Resolve
	}
	if o.SkipUnchanged != nil {
		st.SkipUnchanged = *o.SkipUnchanged
	}
	if o.AssumeSuperuser != nil {
		st.AssumeSuperuser = *o.AssumeSuperuser
	}
	if o.NginxVersion != nil {
		st.NginxVersion = *o.NginxVersion
	}
	if o.Dot != nil {
		st.Dot = *o.Dot
	}

	res, err := conf.Load(st.NginxConf, conf.Options{})
	if err != nil {
		return &Outcome{Code: ExitFatal}, err
	}

	// Certificates are read before masking (which rewrites data: arguments)
	// and are part of the hash, so a renewed certificate regenerates the map.
	var cr *certs.Result
	if st.ReadCertificates {
		cr = certs.Read(res.Main, res.ConfPrefix, nil)
	}

	optKey := fmt.Sprintf("su=%v ver=%s maskp=%v maskh=%v maskv=%v show=%v hide=%v stale=%v dot=%v certs=%v",
		st.AssumeSuperuser, st.NginxVersion, st.MaskPaths, st.MaskHeaders, st.MaskValues, st.InheritShow, st.InheritHide, st.StaleAfter, st.Dot, st.ReadCertificates)
	allFiles := append(append([]conf.File(nil), res.Files...), prefixFiles("sitemap.conf:", sc.Files)...)
	if cr != nil {
		allFiles = append(allFiles, cr.Files...)
	}
	hash := conf.Hash(allFiles, "tool="+o.ToolVersion, optKey)

	if st.SkipUnchanged && !st.Resolve && st.OutputDir != "" {
		if prev, err := os.ReadFile(filepath.Join(st.OutputDir, "map.json")); err == nil {
			var pm struct {
				ConfigHash string `json:"config_hash"`
				Errors     []any  `json:"errors"`
			}
			if json.Unmarshal(prev, &pm) == nil && pm.ConfigHash == hash {
				o.logf("configuration hash unchanged (%s); skipping", hash)
				code := ExitOK
				if len(pm.Errors) > 0 {
					code = ExitPartial
				}
				return &Outcome{Code: code, Skipped: true}, nil
			}
		}
	}

	ar := analyze.Analyze(res, analyze.Options{
		AssumeSuperuser: st.AssumeSuperuser,
		NginxVersion:    st.NginxVersion,
		InheritShow:     st.InheritShow,
		InheritHide:     st.InheritHide,
		Mask:            analyze.MaskOptions{Disable: !st.MaskValues, Paths: st.MaskPaths, Headers: st.MaskHeaders},
	})

	now := o.Now().UTC()
	m := &model.Map{
		SchemaVersion: model.SchemaVersion,
		GeneratedAt:   now.Format(time.RFC3339),
		ToolVersion:   o.ToolVersion,
		ConfigHash:    hash,
		Nginx: model.NginxInfo{Main: absPath(st.NginxConf), ConfPrefix: res.ConfPrefix, AssumedVersion: st.NginxVersion,
			AssumeSuperuser: st.AssumeSuperuser, ManualFile: o.ConfigFileIfLoaded(sc)},
		DNS:      model.DNSInfo{Mode: "names"},
		HTTP:     ar.HTTP,
		Stream:   ar.Stream,
		External: sc.External,
		Errors:   append([]model.Diag{}, ar.Errors...),
		Warnings: append([]model.Diag{}, ar.Warnings...),
	}
	for _, f := range res.Files {
		if p, masked := analyze.MaskPath(f.Path, st.MaskPaths); masked {
			f.Path = p
		}
		m.Files = append(m.Files, f)
	}
	if m.External == nil {
		m.External = []model.External{}
	}
	m.Errors = append(m.Errors, sc.Errors...)
	m.Warnings = append(m.Warnings, sc.Validate(now)...)
	m.Warnings = append(m.Warnings, applyAnnotations(m, sc.Annotations)...)
	var pw []model.Diag
	m.Portal, pw = portal.Build(portal.Input{Analysis: ar, HTTP: m.HTTP, Stream: m.Stream, Certs: cr, ConfPrefix: res.ConfPrefix})
	m.Warnings = append(m.Warnings, pw...)
	for _, s := range m.Portal.Sites {
		for _, c := range s.Certs {
			if p, masked := analyze.MaskPath(c.Path, st.MaskPaths); masked {
				c.Path = p
			}
		}
	}

	if st.Resolve {
		lookup := o.LookupHost
		if lookup == nil {
			lookup = net.DefaultResolver.LookupHost
		}
		snapshotDNS(m, lookup, now)
	}

	out := &Outcome{Map: m, Files: map[string][]byte{}}
	type product struct {
		name  string
		write func(io.Writer, *model.Map) error
	}
	// map.json last: it carries the hash used by skip_unchanged, so a crash
	// mid-way never leaves a "complete" marker next to partial output.
	products := []product{{"index.html", emit.WriteHTML}}
	if st.Dot {
		products = append(products, product{"map.dot", emit.WriteDOT})
	}
	products = append(products, product{"map.json", emit.WriteJSON})
	if st.OutputDir != "" {
		// Under systemd StateDirectory= creates it; for manual runs create it
		// here (0755 so that the nginx worker can traverse it).
		if err := os.MkdirAll(st.OutputDir, 0o755); err != nil {
			return &Outcome{Code: ExitFatal}, fmt.Errorf("output directory: %w", err)
		}
	}
	for _, p := range products {
		if st.OutputDir == "" {
			var b bytes.Buffer
			if err := p.write(&b, m); err != nil {
				return &Outcome{Code: ExitFatal}, err
			}
			out.Files[p.name] = b.Bytes()
			continue
		}
		// Stream straight into the temporary file (bounded memory).
		if err := atomicfs.WriteFunc(filepath.Join(st.OutputDir, p.name), 0o644, func(w io.Writer) error { return p.write(w, m) }); err != nil {
			return &Outcome{Code: ExitFatal}, fmt.Errorf("write %s: %w", p.name, err)
		}
	}
	out.Code = ExitOK
	if len(m.Errors) > 0 {
		out.Code = ExitPartial
	}
	return out, nil
}

// ConfigFileIfLoaded returns the settings path when it was actually read.
func (o *Options) ConfigFileIfLoaded(sc *sitecfg.File) string {
	if len(sc.Files) > 0 {
		return absPath(o.ConfigFile)
	}
	return ""
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func prefixFiles(prefix string, fs []conf.File) []conf.File {
	out := make([]conf.File, len(fs))
	for i, f := range fs {
		f.Path = prefix + f.Path
		out[i] = f
	}
	return out
}

func applyAnnotations(m *model.Map, ans []sitecfg.AnnotationTarget) []model.Diag {
	var warn []model.Diag
	for _, at := range ans {
		hit := false
		switch at.Kind {
		case "server":
			if m.HTTP != nil {
				for _, s := range m.HTTP.Servers {
					for _, n := range s.Names {
						if n.Name == strings.ToLower(at.Name) || n.Name == at.Name {
							s.Annotations = append(s.Annotations, at.A)
							hit = true
							break
						}
					}
				}
			}
		case "stream_server":
			if m.Stream != nil {
				for _, s := range m.Stream.Servers {
					for _, n := range s.Names {
						if n.Name == at.Name || s.ID == at.Name {
							s.Annotations = append(s.Annotations, at.A)
							hit = true
							break
						}
					}
				}
			}
		case "upstream", "stream_upstream":
			var ups []*model.Upstream
			if at.Kind == "upstream" && m.HTTP != nil {
				ups = m.HTTP.Upstreams
			}
			if at.Kind == "stream_upstream" && m.Stream != nil {
				ups = m.Stream.Upstreams
			}
			for _, u := range ups {
				if strings.EqualFold(u.Name, at.Name) {
					u.Annotations = append(u.Annotations, at.A)
					hit = true
				}
			}
		}
		if !hit {
			warn = append(warn, model.Diag{Code: "stale-annotation",
				Message: fmt.Sprintf("annotate %s %q: no such element in the nginx configuration", at.Kind, at.Name), Src: at.A.Src})
		}
	}
	return warn
}

// snapshotDNS resolves host names once, at generation time. The result is
// labelled as a snapshot: nginx resolved (or will resolve) on its own
// schedule — at configuration load for static names, per request for
// variable targets, periodically for `server ... resolve`.
func snapshotDNS(m *model.Map, lookup func(context.Context, string) ([]string, error), now time.Time) {
	m.DNS = model.DNSInfo{Mode: "snapshot", SnapshotAt: now.Format(time.RFC3339)}
	cache := map[string]*model.Snapshot{}
	snap := func(hostport string) *model.Snapshot {
		host := hostport
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host = h
		}
		if host == "" || strings.HasPrefix(host, "unix:") || net.ParseIP(strings.Trim(host, "[]")) != nil {
			return nil
		}
		if s, ok := cache[host]; ok {
			return s
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		addrs, err := lookup(ctx, host)
		s := &model.Snapshot{At: m.DNS.SnapshotAt, Note: "生成時点のスナップショット（nginx 自身の解決結果ではない）"}
		if err != nil {
			s.Error = err.Error()
		} else {
			sort.Strings(addrs)
			s.Addrs = addrs
		}
		cache[host] = s
		return s
	}
	hostOf := func(url string) string {
		u := url
		if i := strings.Index(u, "://"); i >= 0 {
			u = u[i+3:]
		}
		if i := strings.IndexByte(u, '/'); i >= 0 {
			u = u[:i]
		}
		return u
	}
	visitH := func(h *model.Handler) {
		if h == nil {
			return
		}
		for _, b := range h.Backends {
			if b.Kind == "name" {
				b.Snapshot = snap(hostOf(b.URL))
			}
		}
	}
	var visitL func(ls []*model.Location)
	visitL = func(ls []*model.Location) {
		for _, l := range ls {
			visitH(l.Handler)
			for _, ib := range l.IfBlocks {
				visitH(ib.Handler)
			}
			visitL(l.Children)
		}
	}
	var ups []*model.Upstream
	if m.HTTP != nil {
		for _, s := range m.HTTP.Servers {
			visitH(s.Handler)
			visitL(s.Locations)
		}
		ups = append(ups, m.HTTP.Upstreams...)
	}
	if m.Stream != nil {
		for _, s := range m.Stream.Servers {
			visitH(s.Handler)
		}
		ups = append(ups, m.Stream.Upstreams...)
	}
	for _, u := range ups {
		for _, mb := range u.Members {
			mb.Snapshot = snap(mb.Server)
		}
	}
}
