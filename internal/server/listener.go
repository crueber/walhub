package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// This file is the §10.4 listener: plain TCP with TCP_NODELAY per connection
// plus h2c (HTTP/2 prior knowledge) + HTTP/1.1 via x/net/http2/h2c wrapped
// around the chi router. TLS termination belongs on the reverse proxy in
// front of walhub (#165) — the server never wraps the listener.

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

// BuildListener builds the §10.4 listener: plain TCP with TCP_NODELAY per
// connection; h2c is applied at the http.Server layer (NewHTTPServer).
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

// noDelayListener sets TCP_NODELAY on every accepted connection (§10.4).
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
