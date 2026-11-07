// Package model defines the map.json document (see schema/map.schema.json).
package model

import "nginx-sitemap/internal/conf"

const SchemaVersion = "1"

// Resolution status of an element. Derived from the element's expressions.
const (
	StatusStatic      = "static"      // contains no variables
	StatusResolved    = "resolved"    // contained variables, all folded into literals
	StatusConditional = "conditional" // finite branch table; every leaf is a literal
	StatusDynamic     = "dynamic"     // depends on something unknown at generation time
	StatusDeclared    = "declared"    // value written by the user in the manual file
)

// Origin of an element.
const (
	OriginNginx  = "nginx"
	OriginManual = "manual"
)

// Reason codes for anything that is not static.
const (
	ReasonRequest       = "request-dependent"
	ReasonConnection    = "connection-dependent"
	ReasonFilesystem    = "filesystem-dependent"
	ReasonHost          = "host-dependent"
	ReasonRuntimeDNS    = "runtime-dns"
	ReasonUnsupported   = "unsupported-directive"
	ReasonUnsupportedRe = "unsupported-regex"
	ReasonModule        = "unsupported-module"
	ReasonCycle         = "cycle"
	ReasonBranchLimit   = "branch-limit"
	ReasonRedirectLimit = "redirect-limit"
	ReasonUnknownVar    = "unknown-variable"
	ReasonParse         = "parse-error"
	ReasonMasked        = "masked"
	ReasonCondition     = "condition-dependent"
	ReasonVersion       = "version-dependent"
)

type Reason struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Meta is embedded (flattened) into every element.
type Meta struct {
	Src     *conf.Src `json:"src,omitempty"`
	Origin  string    `json:"origin"`
	Status  string    `json:"status"`
	Reasons []Reason  `json:"reasons,omitempty"`
}

type Map struct {
	SchemaVersion string      `json:"schema_version"`
	GeneratedAt   string      `json:"generated_at"`
	ToolVersion   string      `json:"tool_version"`
	ConfigHash    string      `json:"config_hash"`
	Nginx         NginxInfo   `json:"nginx"`
	DNS           DNSInfo     `json:"dns"`
	Files         []conf.File `json:"files"`
	HTTP          *HTTP       `json:"http"`
	Stream        *Stream     `json:"stream"`
	External      []External  `json:"external"`
	Errors        []Diag      `json:"errors"`
	Warnings      []Diag      `json:"warnings"`
}

type NginxInfo struct {
	Main            string `json:"main"`
	ConfPrefix      string `json:"conf_prefix"`
	AssumedVersion  string `json:"assumed_version,omitempty"`
	AssumeSuperuser bool   `json:"assume_superuser"`
	ManualFile      string `json:"manual_file,omitempty"`
}

type DNSInfo struct {
	// "names": proxy targets are shown as written. "snapshot": additionally
	// resolved by this tool at SnapshotAt (generation time; NOT what nginx
	// resolved at its own startup).
	Mode       string `json:"mode"`
	SnapshotAt string `json:"snapshot_at,omitempty"`
}

type Diag struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Src     *conf.Src `json:"src,omitempty"`
}

type HTTP struct {
	Listens   []*Listen   `json:"listens"`
	Servers   []*Server   `json:"servers"`
	Upstreams []*Upstream `json:"upstreams"`
	Tables    []*Table    `json:"tables"`
}

type Stream struct {
	Listens   []*Listen       `json:"listens"`
	Servers   []*StreamServer `json:"servers"`
	Upstreams []*Upstream     `json:"upstreams"`
	Tables    []*Table        `json:"tables"`
}

// Listen is one address:port pair (nginx "ngx_http_conf_addr_t").
type Listen struct {
	Meta
	ID            string      `json:"id"`
	Proto         string      `json:"proto"` // http | stream-tcp | stream-udp
	Addr          string      `json:"addr"`  // "*", "10.0.0.1", "[::]", "unix:/path", or a host name
	Port          int         `json:"port"`
	Family        string      `json:"family"` // inet | inet6 | unix | name
	Implicit      *Implicit   `json:"implicit,omitempty"`
	SSL           bool        `json:"ssl"`
	HTTP2         bool        `json:"http2"`
	QUIC          bool        `json:"quic"`
	ProxyProtocol bool        `json:"proxy_protocol"`
	Bind          bool        `json:"bind"`
	Params        []string    `json:"params,omitempty"`
	Socket        string      `json:"socket,omitempty"` // note on which socket accepts it
	DefaultServer *DefaultRef `json:"default_server"`
	Servers       []string    `json:"servers"`     // server ids, configuration order
	NameIndex     []NameEntry `json:"name_index"`  // server_name selection order
	Decls         []conf.Src  `json:"declared_at"` // listen directives contributing
}

type Implicit struct {
	Value string `json:"value"`
	Basis string `json:"basis"`
}

type DefaultRef struct {
	ServerID string    `json:"server_id"`
	Explicit bool      `json:"explicit"`
	Src      *conf.Src `json:"src,omitempty"`
}

// NameEntry is one row of the virtual-server selection table of a Listen.
type NameEntry struct {
	Rank     int       `json:"rank"`
	Group    int       `json:"group"` // 1 exact, 2 leading wildcard, 3 trailing wildcard, 4 regex
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	ServerID string    `json:"server_id"`
	Src      *conf.Src `json:"src,omitempty"`
	Ignored  string    `json:"ignored,omitempty"` // e.g. "conflicting server name"
}

type ServerName struct {
	Name  string    `json:"name"`
	Kind  string    `json:"kind"` // exact | leading-wildcard | dot-wildcard | trailing-wildcard | regex | empty | hostname
	Src   *conf.Src `json:"src,omitempty"`
	Error string    `json:"error,omitempty"`
}

type Server struct {
	Meta
	ID             string        `json:"id"`
	Names          []ServerName  `json:"names"`
	Listens        []string      `json:"listens"`
	ImplicitListen *Implicit     `json:"implicit_listen,omitempty"`
	Rewrite        *Program      `json:"rewrite,omitempty"` // server-level rewrite phase
	Edges          []*Edge       `json:"edges,omitempty"`   // error_page defined at server level
	Handler        *Handler      `json:"handler,omitempty"` // when no location matches
	Locations      []*Location   `json:"locations"`
	Inherited      []Inherited   `json:"inherited,omitempty"`
	Annotations    []*Annotation `json:"annotations,omitempty"`
}

type Location struct {
	Meta
	ID           string      `json:"id"`
	Modifier     string      `json:"modifier"` // "=" "^~" "~" "~*" "prefix" "@"
	URI          string      `json:"uri"`
	Group        string      `json:"match_group"` // exact | prefix-noregex | prefix | regex | named
	Order        int         `json:"order"`       // appearance order within its level
	AutoRedirect bool        `json:"auto_redirect,omitempty"`
	Internal     bool        `json:"internal,omitempty"`
	Rewrite      *Program    `json:"rewrite,omitempty"`
	Handler      *Handler    `json:"handler"`
	IfBlocks     []*IfBlock  `json:"if_blocks,omitempty"`
	Edges        []*Edge     `json:"edges,omitempty"`
	Inherited    []Inherited `json:"inherited,omitempty"`
	Children     []*Location `json:"children,omitempty"`
}

// Program is the abstractly executed sequence of rewrite-module directives.
type Program struct {
	Bindings []*Binding `json:"bindings,omitempty"`
	Edges    []*Edge    `json:"edges,omitempty"`
}

type Binding struct {
	Meta
	Var   string `json:"var"`
	Value *Expr  `json:"value"`
	Under string `json:"under,omitempty"` // enclosing if-condition, if any
}

type IfBlock struct {
	Meta
	Condition string   `json:"condition"`
	Handler   *Handler `json:"handler,omitempty"`
}

type Handler struct {
	Meta
	Kind     string     `json:"kind"`
	Raw      string     `json:"raw,omitempty"`
	Target   *Expr      `json:"target,omitempty"`
	Code     int        `json:"code,omitempty"`
	Backends []*Backend `json:"backends,omitempty"`
	Note     string     `json:"note,omitempty"`
}

// Backend is one possible destination of a *_pass handler.
type Backend struct {
	Meta
	When     string    `json:"when,omitempty"` // branch condition (conditional targets)
	URL      string    `json:"url"`
	Upstream string    `json:"upstream,omitempty"` // matched upstream block name
	Kind     string    `json:"kind"`               // upstream | address | name | unix | dynamic
	DNS      string    `json:"dns,omitempty"`      // startup | runtime-resolver | upstream-resolve
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

type Snapshot struct {
	At    string   `json:"at"`
	Addrs []string `json:"addrs,omitempty"`
	Error string   `json:"error,omitempty"`
	Note  string   `json:"note"`
}

type Edge struct {
	Meta
	Kind           string `json:"kind"` // rewrite-last | rewrite-break | rewrite | redirect | return | error_page | try_files | if
	Condition      string `json:"condition,omitempty"`
	Target         *Expr  `json:"target,omitempty"`
	Code           int    `json:"code,omitempty"`
	TargetLocation string `json:"target_location,omitempty"`
	Raw            string `json:"raw"`
}

type Inherited struct {
	Directive string    `json:"directive"`
	Args      []string  `json:"args"`
	From      string    `json:"from"` // "here" | "server" | "http" | "location:<id>" | "default"
	Src       *conf.Src `json:"src,omitempty"`
}

type Upstream struct {
	Meta
	Name        string        `json:"name"`
	Kind        string        `json:"kind"` // explicit | implicit
	Balancer    string        `json:"balancer"`
	Members     []*Member     `json:"members"`
	Directives  []Inherited   `json:"directives,omitempty"`
	UsedBy      []string      `json:"used_by"`
	Annotations []*Annotation `json:"annotations,omitempty"`
}

type Member struct {
	Meta
	Server      string    `json:"server"`
	Weight      int       `json:"weight"`
	MaxFails    int       `json:"max_fails"`
	FailTimeout string    `json:"fail_timeout"`
	MaxConns    int       `json:"max_conns"`
	Backup      bool      `json:"backup"`
	Down        bool      `json:"down"`
	Resolve     bool      `json:"resolve"`
	Params      []string  `json:"params,omitempty"`
	Snapshot    *Snapshot `json:"snapshot,omitempty"`
}

type Table struct {
	Meta
	Kind      string       `json:"kind"` // map | geo | split_clients
	Var       string       `json:"var"`
	Source    string       `json:"source"`
	Hostnames bool         `json:"hostnames,omitempty"`
	Volatile  bool         `json:"volatile,omitempty"`
	Entries   []TableEntry `json:"entries"`
	Default   *TableEntry  `json:"default,omitempty"`
}

type TableEntry struct {
	Match Matcher   `json:"match"`
	Value string    `json:"value"`
	Src   *conf.Src `json:"src,omitempty"`
}

type StreamServer struct {
	Meta
	ID          string        `json:"id"`
	Names       []ServerName  `json:"names,omitempty"`
	Listens     []string      `json:"listens"`
	Handler     *Handler      `json:"handler"`
	Inherited   []Inherited   `json:"inherited,omitempty"`
	Annotations []*Annotation `json:"annotations,omitempty"`
}

// External is a manually declared service that does not go through nginx.
type External struct {
	Meta
	Name      string   `json:"name"`
	Group     string   `json:"group,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
	Listens   []string `json:"listens,omitempty"`
	Backends  []string `json:"backends,omitempty"`
	Depends   []string `json:"depends,omitempty"`
	Owner     string   `json:"owner,omitempty"`
	Note      string   `json:"note,omitempty"`
	Reviewed  string   `json:"reviewed,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

// Annotation is a manual note attached to an nginx-derived element.
type Annotation struct {
	Meta
	Owner  string   `json:"owner,omitempty"`
	Note   string   `json:"note,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Expect []string `json:"expect,omitempty"`
}
