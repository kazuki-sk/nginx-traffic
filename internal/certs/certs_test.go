package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nginx-sitemap/internal/conf"
)

func writeCert(t *testing.T, path string, notAfter time.Time, withKey bool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "www.example.test"},
		DNSNames: []string{"www.example.test", "example.test"}, NotBefore: notAfter.Add(-24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if withKey {
		kb, _ := x509.MarshalECPrivateKey(key)
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})...)
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, dir, cfg string) *conf.Result {
	t.Helper()
	p := filepath.Join(dir, "nginx.conf")
	os.WriteFile(p, []byte(cfg), 0o644)
	r, err := conf.Load(p, conf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReadCertificates(t *testing.T) {
	d := t.TempDir()
	exp := time.Date(2027, 1, 31, 12, 0, 0, 0, time.UTC)
	writeCert(t, filepath.Join(d, "tls", "www.crt"), exp, false)
	writeCert(t, filepath.Join(d, "tls", "combined.pem"), exp, true)
	r := load(t, d, `events {} http {
  server { listen 443 ssl; ssl_certificate tls/www.crt; ssl_certificate_key tls/www.key; }
  server { listen 443 ssl; ssl_certificate tls/combined.pem; ssl_certificate_key tls/combined.pem; }
  server { listen 443 ssl; ssl_certificate tls/missing.crt; ssl_certificate_key k; }
  server { listen 443 ssl; ssl_certificate $ssl_server_name.crt; ssl_certificate_key k; }
  server { listen 443 ssl; ssl_certificate data:$cert; ssl_certificate_key k; }
}`)
	var reads []string
	res := Read(r.Main, r.ConfPrefix, func(p string) ([]byte, error) {
		reads = append(reads, p)
		return os.ReadFile(p)
	})
	c := res.Lookup("tls/www.crt", r.ConfPrefix)
	if c == nil || c.Error != "" || c.NotAfter != "2027-01-31T12:00:00Z" || c.Subject != "www.example.test" || len(c.DNSNames) != 2 {
		t.Fatalf("www: %+v", c)
	}
	if c := res.Lookup("tls/combined.pem", r.ConfPrefix); c == nil || !strings.Contains(c.Error, "ssl_certificate_key") {
		t.Fatalf("combined file must not be read: %+v", c)
	}
	if c := res.Lookup("tls/missing.crt", r.ConfPrefix); c == nil || c.Error != "file not found" {
		t.Fatalf("missing: %+v", c)
	}
	if c := res.Lookup("$ssl_server_name.crt", r.ConfPrefix); c == nil || !strings.Contains(c.Error, "variables") {
		t.Fatalf("variable: %+v", c)
	}
	if c := res.Lookup("data:$cert", r.ConfPrefix); c == nil || c.Path != "data:…" {
		t.Fatalf("data: %+v", c)
	}
	for _, p := range reads {
		if strings.Contains(p, "combined") || strings.HasSuffix(p, ".key") {
			t.Fatalf("key material read: %s", p)
		}
	}
	if len(res.Files) != 1 || !strings.HasPrefix(res.Files[0].Path, "cert:") {
		t.Fatalf("hash files: %+v", res.Files)
	}
}
