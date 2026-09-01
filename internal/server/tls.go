package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// tlsFiles resolves the §11 paths: <cache.dir>/tls/{cert,key}.pem + cert.sans.
func (s *Server) tlsFiles() (cert, key, sans string) {
	dir := filepath.Join(s.cacheRoot, "tls")
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.sans")
}

// loadCACert reads the published cert PEM (§11: published at
// /services/public/ca.pem).
func (s *Server) loadCACert() ([]byte, error) {
	certPath, _, _ := s.tlsFiles()
	return os.ReadFile(certPath)
}

// EnsureSelfSigned implements the SAN-stable regeneration contract: read
// cert.sans, compare with the desired SAN set (default localhost, *.localhost,
// 127.0.0.1, ::1 + the public_url host + server.tls.hostnames), rewrite all
// three files only on mismatch (§11).
func (s *Server) EnsureSelfSigned() error {
	certPath, keyPath, sansPath := s.tlsFiles()
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return err
	}
	want := s.desiredSANs()
	if prev, err := os.ReadFile(sansPath); err == nil {
		if sansEqual(string(prev), want) {
			return nil // regenerated only when the SAN list changes
		}
	}
	certPEM, keyPEM, err := selfSignedPEM(want)
	if err != nil {
		return err
	}
	body := joinSans(want)
	if err := writeFileAtomic(sansPath, []byte(body)); err != nil {
		return err
	}
	if err := writeFileAtomic(certPath, certPEM); err != nil {
		return err
	}
	return writeFileAtomic(keyPath, keyPEM)
}

// desiredSANs is the §11 SAN list.
func (s *Server) desiredSANs() []string {
	out := []string{"localhost", "*.localhost", "127.0.0.1", "::1"}
	if h := hostOnly(s.cfg.Server.PublicURL); h != "" && h != "localhost" {
		out = append(out, h)
	}
	return append(out, s.cfg.Server.TLS.Hostnames...)
}

// selfSignedPEM generates a P-256 self-signed cert (crypto/x509 + crypto/ecdsa
// replaces the Rust rcgen).
func selfSignedPEM(hosts []string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "walhub"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func sansEqual(prev string, want []string) bool {
	cur := splitSans(prev)
	if len(cur) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, w := range want {
		set[w] = true
	}
	for _, c := range cur {
		if !set[c] {
			return false
		}
	}
	return true
}

func splitSans(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func joinSans(list []string) string {
	out := ""
	for i, v := range list {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out + "\n"
}

func writeFileAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sdkETag is a cheap strong validator for the SDK file.
func sdkETag(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// BuildListener builds the §10.4 listener: TLS off → plain TCP with
// TCP_NODELAY per connection + h2c (prior knowledge) + HTTP/1.1 via
// x/net/http2/h2c; TLS on → crypto/tls in-process with ALPN h2, http/1.1.
func (s *Server) BuildListener(ln net.Listener) net.Listener {
	return &noDelayListener{Listener: ln}
}

// Contexter supplies the app context (composition; §10.4 BaseContext).
type Contexter interface {
	Context() context.Context
}

// NewHTTPServer wires the one http.Server with BaseContext carrying the app
// context; h2c wraps the chi router (§10.4).
func (s *Server) NewHTTPServer(h http.Handler, appCtx Contexter) *http.Server {
	srv := &http.Server{Handler: h2c.NewHandler(h, &http2.Server{})}
	if appCtx != nil {
		srv.BaseContext = func(net.Listener) context.Context { return appCtx.Context() }
	}
	return srv
}

// TLSServerConfig is the §11 TLS config: ALPN h2 + http/1.1.
func (s *Server) TLSServerConfig() (*tls.Config, error) {
	certPath, keyPath, _ := s.tlsFiles()
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

// noDelayListener sets TCP_NODELAY on every accepted connection (§11).
type noDelayListener struct{ net.Listener }

func (l *noDelayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return c, nil
}
