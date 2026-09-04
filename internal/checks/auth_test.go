package checks

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Seam 2 shape tests: credential extraction, shape resolution, and the
// composition wrapper — no store access anywhere in this file.

func TestExtractToken(t *testing.T) {
	mk := func(header, value string) string {
		req := httptest.NewRequest("GET", "/o/r/api/checks", nil)
		if header != "" {
			req.Header.Set(header, value)
		}
		return extractToken(req)
	}
	if got := mk("Authorization", "Bearer abc123"); got != "abc123" {
		t.Fatalf("bearer: %q", got)
	}
	if got := mk("Authorization", "bearer abc123"); got != "abc123" {
		t.Fatalf("lower bearer: %q", got)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("ci:wct_abcd1234.secret"))
	if got := mk("Authorization", "Basic "+basic); got != "wct_abcd1234.secret" {
		t.Fatalf("basic: %q", got)
	}
	if got := mk("X-Walgit-Authorization", "Bearer edge-token"); got != "edge-token" {
		t.Fatalf("edge header: %q", got)
	}
	if got := mk("", ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := mk("Authorization", "Basic !!!"); got != "" {
		t.Fatalf("bad basic: %q", got)
	}
}

func TestShapePrincipal(t *testing.T) {
	mk := func(cred string) (auth.Principal, *auth.AuthError, bool) {
		req := httptest.NewRequest("GET", "/o/r/api/checks", nil)
		if cred != "" {
			req.Header.Set("Authorization", cred)
		}
		return ShapePrincipal(req)
	}
	// Non-wct_ falls through (claimed=false) — the server chain decides.
	if _, _, claimed := mk("Bearer wgt_abc"); claimed {
		t.Fatal("wgt claimed")
	}
	if _, _, claimed := mk(""); claimed {
		t.Fatal("empty claimed")
	}
	// Well-formed shape resolves the unprivileged principal.
	p, aerr, claimed := mk("Bearer wct_abcd1234." + strings.Repeat("s", 64))
	if !claimed || aerr != nil {
		t.Fatalf("shape: %+v %v", p, aerr)
	}
	if p.Name != "ci:abcd1234" || p.Write || p.Admin || p.Anonymous {
		t.Fatalf("principal = %+v (must be unprivileged)", p)
	}
	// Malformed wct_ is a real 401 (never a fall-through).
	_, aerr, claimed = mk("Bearer wct_short")
	if !claimed || aerr == nil || aerr.Kind != auth.ErrInvalid {
		t.Fatalf("malformed: claimed=%v err=%v", claimed, aerr)
	}
}

func TestWrapAuth(t *testing.T) {
	chain := func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "jane@example.com", Write: true}, nil
	}
	wrapped := WrapAuth(chain)
	// wct_ resolves through the shape (the chain never sees it).
	req := httptest.NewRequest("GET", "/o/r/api/checks", nil)
	req.Header.Set("Authorization", "Bearer wct_abcd1234.secret")
	p, aerr := wrapped(req)
	if aerr != nil || p.Name != "ci:abcd1234" {
		t.Fatalf("wct: %+v %v", p, aerr)
	}
	// Anything else falls through to the chain.
	req2 := httptest.NewRequest("GET", "/o/r/api/checks", nil)
	req2.Header.Set("Authorization", "Bearer static-token")
	p, aerr = wrapped(req2)
	if aerr != nil || p.Name != "jane@example.com" {
		t.Fatalf("chain: %+v %v", p, aerr)
	}
	// Nil chain falls back to anonymous.
	anon := WrapAuth(nil)
	p, aerr = anon(req2)
	if aerr != nil || !p.Anonymous {
		t.Fatalf("nil chain: %+v %v", p, aerr)
	}
}

func TestCISecretOf(t *testing.T) {
	req := httptest.NewRequest("GET", "/o/r/api/checks", nil)
	req.Header.Set("Authorization", "Bearer wct_abcd1234.mysecret")
	id, secret, ok := CISecretOf(req)
	if !ok || id != "abcd1234" || secret != "mysecret" {
		t.Fatalf("secret: %q %q %v", id, secret, ok)
	}
	req2 := httptest.NewRequest("GET", "/o/r/api/checks", nil)
	if _, _, ok := CISecretOf(req2); ok {
		t.Fatal("empty claimed")
	}
}
