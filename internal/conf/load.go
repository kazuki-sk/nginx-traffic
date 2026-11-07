package conf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	crossplane "github.com/nginxinc/nginx-go-crossplane"
)

// File is a configuration file that was actually opened and read.
type File struct {
	Path   string `json:"path"` // display path (relative to conf prefix when inside it)
	Abs    string `json:"-"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
	// IncludedFrom lists the include chains ("a:1 > b:2") that pulled this
	// file in; empty for the main file.
	IncludedFrom []string `json:"included_from,omitempty"`
}

// Error is a load/parse diagnostic.
type Error struct {
	Code string // parse-error | include-error | cycle
	Msg  string
	Src  Src
}

// Result of loading a configuration tree.
type Result struct {
	ConfPrefix string
	Main       *Node // synthetic root; Block holds the main-context directives
	Files      []File
	Errors     []Error
}

// Options for Load.
type Options struct {
	// ReadFile, if set, replaces os.ReadFile (tests).
	ReadFile func(string) ([]byte, error)
}

// Load parses mainFile and all files it includes.
//
// Each file is parsed separately by crossplane (SingleFile mode); include
// resolution is done here so that (a) an include cycle does not abort the
// whole parse (crossplane returns no payload at all in that case), and
// (b) a file included from several contexts is spliced into each of them.
//
// Path semantics (source: nginx 1.30.0 src/core/ngx_conf_file.c
// ngx_conf_include → ngx_conf_full_name(cycle, &file, 1)): relative include
// paths are resolved against the *configuration prefix* (cycle->conf_prefix),
// i.e. the directory of the file given with -c / --conf-path — not against
// the directory of the including file.
//
// Glob semantics (src/os/unix/ngx_files.c ngx_open_glob): glob(3) with
// flags=0, i.e. sorted results and wildcards that do not match a leading
// '.'. nginx does not call setlocale(), so sorting is byte order. A pattern
// without "*?[" must name a readable file; a glob may match nothing.
func Load(mainFile string, opt Options) (*Result, error) {
	abs, err := filepath.Abs(mainFile)
	if err != nil {
		return nil, err
	}
	readFile := opt.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	res := &Result{ConfPrefix: filepath.Dir(abs)}
	l := &loader{res: res, readFile: readFile, cache: map[string]*parsedFile{}, files: map[string]*File{}}

	main := l.parse(abs)
	if main.readErr != nil {
		return nil, fmt.Errorf("cannot read main configuration %s: %w", abs, main.readErr)
	}
	res.Main = &Node{Name: "#main", IsBlock: true, Src: Src{File: res.display(abs), Line: 0}}
	res.Main.Block = l.convert(main.dirs, res.display(abs), res.Main, nil, []string{abs})

	for _, f := range l.files {
		res.Files = append(res.Files, *f)
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
	return res, nil
}

type parsedFile struct {
	dirs    crossplane.Directives
	readErr error
}

type loader struct {
	res      *Result
	readFile func(string) ([]byte, error)
	cache    map[string]*parsedFile
	files    map[string]*File
}

func (l *loader) parse(path string) *parsedFile {
	if pf, ok := l.cache[path]; ok {
		return pf
	}
	pf := &parsedFile{}
	l.cache[path] = pf
	data, err := l.readFile(path)
	if err != nil {
		pf.readErr = err
		return pf
	}
	sum := sha256.Sum256(data)
	l.files[path] = &File{Path: l.res.display(path), Abs: path, SHA256: hex.EncodeToString(sum[:]), Size: len(data)}
	payload, err := crossplane.Parse(path, &crossplane.ParseOptions{
		SingleFile: true,
		Open: func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
		StopParsingOnError:        false,
		ErrorOnUnknownDirectives:  false,
		SkipDirectiveContextCheck: true,
		SkipDirectiveArgsCheck:    true,
	})
	if err != nil && payload == nil {
		l.res.Errors = append(l.res.Errors, Error{Code: "parse-error", Msg: err.Error(), Src: Src{File: l.res.display(path)}})
		return pf
	}
	for _, pe := range payload.Errors {
		line := 0
		if pe.Line != nil {
			line = *pe.Line
		}
		msg := ""
		if pe.Error != nil {
			msg = pe.Error.Error()
		}
		l.res.Errors = append(l.res.Errors, Error{Code: "parse-error", Msg: msg, Src: Src{File: l.res.display(path), Line: line}})
	}
	if len(payload.Config) > 0 {
		pf.dirs = payload.Config[0].Parsed
	}
	if len(payload.Errors) > 0 && len(pf.dirs) == 0 {
		// crossplane drops the whole file on a syntax error; salvage the
		// part before the error so that the rest of the map is still useful.
		errLine := 0
		if pe := payload.Errors[0]; pe.Line != nil {
			errLine = *pe.Line
		}
		if dirs, upto := recoverPrefix(path, data, errLine); len(dirs) > 0 {
			pf.dirs = dirs
			l.res.Errors = append(l.res.Errors, Error{Code: "parse-error",
				Msg: fmt.Sprintf("recovered the part of %s before line %d; everything after it is ignored", l.res.display(path), upto),
				Src: Src{File: l.res.display(path), Line: upto}})
		}
	}
	return pf
}

// recoverPrefix re-parses the tokens of data that precede errLine, dropping
// the last incomplete statement and closing open blocks. Line numbers are
// preserved by padding with newlines.
func recoverPrefix(path string, data []byte, errLine int) (crossplane.Directives, int) {
	var toks []crossplane.NgxToken
	for t := range crossplane.Lex(bytes.NewReader(data)) {
		if t.Error != nil || (errLine > 0 && t.Line >= errLine) {
			break
		}
		toks = append(toks, t)
	}
	cut := -1
	depth, depthAtCut := 0, 0
	for i, t := range toks {
		if t.IsQuoted {
			continue
		}
		switch t.Value {
		case "{":
			depth++
		case "}":
			depth--
		case ";":
		default:
			continue
		}
		if depth < 0 {
			break
		}
		cut, depthAtCut = i, depth
	}
	if cut < 0 {
		return nil, errLine
	}
	var b strings.Builder
	line := 1
	for _, t := range toks[:cut+1] {
		if !t.IsQuoted && strings.HasPrefix(t.Value, "#") {
			continue
		}
		for line < t.Line {
			b.WriteByte('\n')
			line++
		}
		b.WriteByte(' ')
		if t.IsQuoted {
			b.WriteString(`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(t.Value) + `"`)
		} else {
			b.WriteString(t.Value)
		}
	}
	b.WriteString(strings.Repeat(" }", depthAtCut))
	text := b.String()
	payload, err := crossplane.Parse(path, &crossplane.ParseOptions{
		SingleFile:                true,
		Open:                      func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(text)), nil },
		SkipDirectiveContextCheck: true,
		SkipDirectiveArgsCheck:    true,
	})
	if err != nil || payload == nil || len(payload.Config) == 0 || len(payload.Errors) > 0 {
		return nil, errLine
	}
	return payload.Config[0].Parsed, toks[cut].Line
}

func (l *loader) convert(ds crossplane.Directives, file string, parent *Node, chain []string, stack []string) []*Node {
	var out []*Node
	for _, d := range ds {
		src := Src{File: file, Line: d.Line, Chain: chain}
		if d.Directive == "include" && !d.IsMapBlockParameter {
			out = append(out, l.include(d, src, parent, chain, stack)...)
			continue
		}
		n := &Node{
			Name:     d.Directive,
			Args:     append([]string{}, d.Args...),
			Src:      src,
			IsBlock:  d.IsBlock(),
			Parent:   parent,
			MapParam: d.IsMapBlockParameter,
		}
		if d.IsBlock() {
			n.Block = l.convert(d.Block, file, n, chain, stack)
		}
		out = append(out, n)
	}
	return out
}

func (l *loader) include(d *crossplane.Directive, src Src, parent *Node, chain, stack []string) []*Node {
	if len(d.Args) != 1 {
		l.res.Errors = append(l.res.Errors, Error{Code: "parse-error", Msg: "invalid number of arguments in \"include\" directive", Src: src})
		return nil
	}
	pattern := d.Args[0]
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(l.res.ConfPrefix, pattern)
	}
	var paths []string
	if strings.ContainsAny(pattern, "*?[") {
		m, err := globNginx(pattern)
		if err != nil {
			l.res.Errors = append(l.res.Errors, Error{Code: "include-error", Msg: fmt.Sprintf("glob \"%s\" failed: %v", pattern, err), Src: src})
			return nil
		}
		paths = m
	} else {
		paths = []string{pattern}
	}
	nchain := append(append([]string(nil), chain...), src.String())
	var out []*Node
	for _, p := range paths {
		if containsStr(stack, p) {
			// nginx has no cycle check; it recurses until it crashes or runs
			// out of file descriptors (1.30.0 `nginx -t` dies with SIGSEGV).
			l.res.Errors = append(l.res.Errors, Error{Code: "cycle",
				Msg: fmt.Sprintf("include cycle: %s is already being included (%s)", l.res.display(p), strings.Join(nchain, " → ")), Src: src})
			continue
		}
		pf := l.parse(p)
		if f := l.files[p]; f != nil {
			f.IncludedFrom = appendUniq(f.IncludedFrom, strings.Join(nchain, " > "))
		}
		if pf.readErr != nil {
			l.res.Errors = append(l.res.Errors, Error{Code: "include-error", Msg: fmt.Sprintf("open() \"%s\" failed: %v", p, pf.readErr), Src: src})
			continue
		}
		out = append(out, l.convert(pf.dirs, l.res.display(p), parent, nchain, append(append([]string(nil), stack...), p))...)
	}
	return out
}

func appendUniq(xs []string, x string) []string {
	if containsStr(xs, x) {
		return xs
	}
	return append(xs, x)
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (r *Result) display(path string) string {
	if path == "" {
		return path
	}
	if rel, err := filepath.Rel(r.ConfPrefix, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// Hash returns the configuration hash over (path, size, sha256) of every
// file read, plus any extra strings (tool version, options).
func Hash(files []File, extra ...string) string {
	h := sha256.New()
	for _, e := range extra {
		fmt.Fprintf(h, "x\x00%s\x00", e)
	}
	fs := append([]File(nil), files...)
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	for _, f := range fs {
		fmt.Fprintf(h, "f\x00%s\x00%d\x00%s\x00", f.Path, f.Size, f.SHA256)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// globNginx emulates glob(3) with flags=0 as used by nginx: results are
// sorted in byte order and a wildcard never matches a leading '.' in a
// path component (POSIX glob rule; Go's filepath.Glob does match it).
func globNginx(pattern string) ([]string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	pseg := strings.Split(filepath.ToSlash(pattern), "/")
	var out []string
	for _, m := range matches {
		mseg := strings.Split(filepath.ToSlash(m), "/")
		if len(mseg) != len(pseg) {
			out = append(out, m)
			continue
		}
		ok := true
		for i := range pseg {
			if strings.HasPrefix(mseg[i], ".") && !strings.HasPrefix(pseg[i], ".") && strings.ContainsAny(pseg[i], "*?[") {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}
