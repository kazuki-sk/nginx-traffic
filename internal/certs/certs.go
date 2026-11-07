// Package certs reads the certificates named by ssl_certificate to report
// their expiry. Only certificate files are opened; ssl_certificate_key and
// any other key material is never read.
package certs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nginx-sitemap/internal/conf"
	"nginx-sitemap/internal/model"
)

// Result maps the configured certificate path (as written, after relative
// path resolution) to what was read.
type Result struct {
	ByPath map[string]*model.Cert
	Files  []conf.File // for the configuration hash
	keys   map[string]bool
}

// Read collects every ssl_certificate under root and reads it.
// Must be called before masking, which rewrites "data:" arguments.
//
// Relative paths are resolved against the conf prefix
// (src: src/event/ngx_event_openssl_cache.c ngx_ssl_cache_init_key →
// ngx_get_full_name(pool, &ngx_cycle->conf_prefix, path)).
//
// A file that is also named by ssl_certificate_key (certificate and key in
// one PEM) is not read, so that key material is never loaded.
func Read(root *conf.Node, confPrefix string, readFile func(string) ([]byte, error)) *Result {
	if readFile == nil {
		readFile = os.ReadFile
	}
	res := &Result{ByPath: map[string]*model.Cert{}}
	keys := map[string]bool{}
	var keyWalk func(n *conf.Node)
	keyWalk = func(n *conf.Node) {
		for _, c := range n.Block {
			if strings.HasSuffix(c.Name, "certificate_key") && !c.MapParam && len(c.Args) == 1 {
				keys[resolve(c.Args[0], confPrefix)] = true
			}
			if c.IsBlock {
				keyWalk(c)
			}
		}
	}
	keyWalk(root)
	res.keys = keys
	var walk func(n *conf.Node)
	walk = func(n *conf.Node) {
		for _, c := range n.Block {
			if c.Name == "ssl_certificate" && !c.MapParam && len(c.Args) == 1 {
				p := c.Args[0]
				if _, seen := res.ByPath[resolve(p, confPrefix)]; !seen {
					res.read(p, confPrefix, c.Src, readFile)
				}
			}
			if c.IsBlock {
				walk(c)
			}
		}
	}
	walk(root)
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
	return res
}

func resolve(p, prefix string) string {
	if strings.HasPrefix(p, "data:") || strings.HasPrefix(p, "engine:") || strings.Contains(p, "$") || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(prefix, p)
}

// Lookup returns the certificate for a configured path.
func (r *Result) Lookup(p, prefix string) *model.Cert {
	if r == nil {
		return nil
	}
	return r.ByPath[resolve(p, prefix)]
}

func (r *Result) read(p, prefix string, src conf.Src, readFile func(string) ([]byte, error)) {
	full := resolve(p, prefix)
	s := src
	c := &model.Cert{Path: p, Src: &s}
	r.ByPath[full] = c
	switch {
	case strings.HasPrefix(p, "data:"):
		c.Path = "data:…"
		c.Error = "inline certificate (data:) is not read"
		return
	case strings.HasPrefix(p, "engine:"):
		c.Error = "certificate from an OpenSSL engine is not read"
		return
	case strings.Contains(p, "$"):
		// ssl_certificate with variables is loaded per handshake (1.15.9+).
		c.Error = "path contains variables; resolved per TLS handshake"
		return
	}
	if r.keys[full] {
		c.Error = "the same file is used as ssl_certificate_key; not read to avoid loading key material"
		return
	}
	data, err := readFile(full)
	if err != nil {
		c.Error = shortErr(err)
		return
	}
	sum := sha256.Sum256(data)
	r.Files = append(r.Files, conf.File{Path: "cert:" + full, Abs: full, SHA256: hex.EncodeToString(sum[:]), Size: len(data)})
	cert, err := firstCert(data)
	if err != nil {
		c.Error = err.Error()
		return
	}
	c.NotAfter = cert.NotAfter.UTC().Format(time.RFC3339)
	c.Subject = cert.Subject.CommonName
	c.Issuer = cert.Issuer.CommonName
	c.DNSNames = cert.DNSNames
}

// firstCert returns the leaf: nginx expects the server certificate first,
// followed by intermediates (doc: ngx_http_ssl_module#ssl_certificate).
func firstCert(data []byte) (*x509.Certificate, error) {
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, errors.New("no PEM certificate found")
		}
		if b.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %v", err)
			}
			return c, nil
		}
	}
}

func shortErr(err error) string {
	switch {
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, os.ErrNotExist):
		return "file not found"
	}
	return err.Error()
}
