package analyze

import (
	"path/filepath"
	"regexp"
	"strings"

	"nginx-sitemap/internal/conf"
)

// Masked replaces secret values in the output.
const Masked = "***masked***"

// MaskOptions controls masking of secret values and file paths.
//
// The tool never opens files referenced by directives (only `include`
// targets are read), so key/password files are never read. Masking is
// about values written *inside* the configuration and about paths.
type MaskOptions struct {
	Disable bool     // disables the default value masking (not recommended)
	Paths   []string // glob patterns; matching path arguments are masked
	Headers []string // additional header names whose values are masked
}

var defaultMaskHeaders = []string{"authorization", "proxy-authorization", "cookie", "x-api-key", "x-auth-token"}

// Directives whose arguments are secret values.
var secretValueDirectives = map[string]bool{
	"secure_link_secret": true,
	"secure_link_md5":    true, // expression embeds the shared secret
	"auth_jwt_key_file":  false,
}

// Directives with file path arguments (subject to Paths masking; the
// "data:" form embeds key material and is always masked).
var pathDirectives = map[string]bool{
	"ssl_certificate": true, "ssl_certificate_key": true, "ssl_password_file": true,
	"ssl_trusted_certificate": true, "ssl_client_certificate": true, "ssl_crl": true,
	"ssl_dhparam": true, "ssl_session_ticket_key": true, "ssl_stapling_file": true,
	"auth_basic_user_file": true, "proxy_ssl_certificate": true, "proxy_ssl_certificate_key": true,
	"proxy_ssl_password_file": true, "proxy_ssl_trusted_certificate": true, "proxy_ssl_crl": true,
	"grpc_ssl_certificate": true, "grpc_ssl_certificate_key": true, "grpc_ssl_password_file": true,
	"grpc_ssl_trusted_certificate": true, "uwsgi_ssl_certificate": true, "uwsgi_ssl_certificate_key": true,
	"uwsgi_ssl_password_file": true, "uwsgi_ssl_trusted_certificate": true, "auth_jwt_key_file": true,
}

var secretVarName = regexp.MustCompile(`(?i)(secret|token|passw(or)?d|passwd|api_?key|private_?key|credential)`)
var secretParamName = regexp.MustCompile(`(?i)(HTTP_AUTHORIZATION|HTTP_COOKIE|PASSW|SECRET|TOKEN|API_?KEY)`)

func applyMask(root *conf.Node, mo MaskOptions) {
	headers := map[string]bool{}
	for _, h := range defaultMaskHeaders {
		headers[h] = true
	}
	for _, h := range mo.Headers {
		headers[strings.ToLower(h)] = true
	}
	var walk func(n *conf.Node)
	walk = func(n *conf.Node) {
		for _, c := range n.Block {
			maskNode(c, mo, headers)
			if c.IsBlock {
				walk(c)
			}
		}
	}
	walk(root)
}

func maskNode(n *conf.Node, mo MaskOptions, headers map[string]bool) {
	if n.MapParam {
		return
	}
	if pathDirectives[n.Name] {
		for i, a := range n.Args {
			if strings.HasPrefix(a, "data:") || strings.HasPrefix(a, "engine:") {
				n.Args[i] = Masked
				continue
			}
			if pathMatches(a, mo.Paths) {
				n.Args[i] = Masked
			}
		}
	}
	if mo.Disable {
		return
	}
	switch {
	case secretValueDirectives[n.Name]:
		for i := range n.Args {
			n.Args[i] = Masked
		}
	case strings.HasSuffix(n.Name, "_set_header") || n.Name == "add_header" || n.Name == "add_trailer" || n.Name == "more_set_headers":
		if len(n.Args) >= 2 && headers[strings.ToLower(n.Args[0])] {
			n.Args[1] = Masked
		}
	case n.Name == "fastcgi_param" || n.Name == "uwsgi_param" || n.Name == "scgi_param" || n.Name == "grpc_set_header":
		if len(n.Args) >= 2 && secretParamName.MatchString(n.Args[0]) {
			n.Args[1] = Masked
		}
	case n.Name == "set":
		if len(n.Args) >= 2 && secretVarName.MatchString(n.Args[0]) {
			n.Args[1] = Masked
		}
	}
}

func pathMatches(p string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, p); ok {
			return true
		}
		if strings.HasSuffix(g, "/**") && strings.HasPrefix(p, strings.TrimSuffix(g, "**")) {
			return true
		}
	}
	return false
}

// MaskPath is used for paths shown outside directive arguments (file list).
func MaskPath(p string, globs []string) (string, bool) {
	if pathMatches(p, globs) {
		return Masked, true
	}
	return p, false
}
