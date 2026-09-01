package server

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// hasPrefixFold is a case-insensitive strings.HasPrefix.
func hasPrefixFold(s, p string) bool { return len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) }

// containsFold is a case-insensitive strings.Contains.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// trimPrefixFold strips a case-insensitive prefix.
func trimPrefixFold(s, p string) string {
	if hasPrefixFold(s, p) {
		return s[len(p):]
	}
	return s
}

// newRequestID mints a random 16-byte hex UUID (§2.2 #1).
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// remoteHost extracts the host part of r.RemoteAddr.
func remoteHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// isLoopbackHost reports loopback IPs and the literal "localhost".
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// canonicalHost is "walgit.localhost[:port]" for a loopback browser host.
func canonicalHost(host string) string {
	_, port, err := net.SplitHostPort(host)
	suffix := ""
	if err == nil && port != "" && port != "80" && port != "443" {
		suffix = ":" + port
	}
	return "walgit.localhost" + suffix
}

// plainStatus writes a plain-text body with a status (the Rust contract: never
// an HTML error page).
func plainStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if body == "" || body[len(body)-1] != '\n' {
		body += "\n"
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// pctEncode percent-encodes s for a header/path context (§5 accel key).
func pctEncode(s string) string {
	const hexU = "0123456789ABCDEF"
	var b strings.Builder
	for i := range s {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexU[c>>4])
		b.WriteByte(hexU[c&0xf])
	}
	return b.String()
}

// parseRange parses a single RFC 7233 bytes range into a closed [start,end]
// against size; ok=false → unsatisfiable or multiple ranges (serve full).
func parseRange(spec string, size int64) (start, end int64, ok bool) {
	if !hasPrefixFold(spec, "bytes=") {
		return 0, 0, false
	}
	spec = spec[len("bytes="):]
	if strings.Contains(spec, ",") { // multiple ranges: serve full
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if first == "" { // suffix form
		if last == "" {
			return 0, 0, false
		}
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 || size == 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	s0, err := strconv.ParseInt(first, 10, 64)
	if err != nil || s0 < 0 || s0 >= size {
		return 0, 0, false // unsatisfiable
	}
	if last == "" {
		return s0, size - 1, true
	}
	e0, err := strconv.ParseInt(last, 10, 64)
	if err != nil || e0 < s0 {
		return 0, 0, false
	}
	if e0 >= size {
		e0 = size - 1
	}
	return s0, e0, true
}

// netSplit wraps net.SplitHostPort.
func netSplit(s string) (string, string, error) { return net.SplitHostPort(s) }
