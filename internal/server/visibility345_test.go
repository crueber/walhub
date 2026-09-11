package server

// visibility345_test.go — Forgejo #345: visibility as the read authority
// on the git/LFS/bundle/SSH transports and the repo SPA shell.
//
// Matrix (anonymous_read=false throughout — the motivating instance shape):
//   - gateRepoRead: wired allow-gate + anonymous → pass (the flag no
//     longer short-circuits public repos); nil gate + anonymous → 401;
//     wired deny → mapped status (401 anon / 403 authed).
//   - repo shell: anonymous reaches the public shell (200), private keeps
//     the legacy gated outcome (401; the #344 login page for browsers).
//   - SSH fetch: the read gate runs before any transport gate (deny
//     surfaces its reason even when drained).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/sshd"
)

func TestGateRepoReadVisibilityFirst(t *testing.T) {
	mkReq := func() *http.Request {
		return httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	}
	id := git.RepoId{Owner: "o", Name: "r"}
	// Wired allow-gate admits anonymous despite anonymous_read=false.
	s, _ := newTestServer(t, func(o *Options) { o.ReadGate = &stubGate{} })
	rec := httptest.NewRecorder()
	if !s.gateRepoRead(rec, mkReq(), git.ServiceUploadPack, id, auth.Anonymous()) {
		t.Fatalf("allow-gate anon = %d, want pass", rec.Code)
	}
	// Nil gate keeps the legacy flag check: anonymous → 401.
	s.readGate = nil
	rec = httptest.NewRecorder()
	if s.gateRepoRead(rec, mkReq(), git.ServiceUploadPack, id, auth.Anonymous()) {
		t.Error("nil gate anon with flag off must fail")
	}
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("nil gate anon = %d, want 401+Bearer", rec.Code)
	}
	// Denials map through §4.2: 401 anon, 403 authed.
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}}
	rec = httptest.NewRecorder()
	if s.gateRepoRead(rec, mkReq(), git.ServiceUploadPack, id, auth.Anonymous()) {
		t.Error("deny-gate anon must fail")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deny-gate anon = %d, want 401", rec.Code)
	}
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}}
	authed := mkReq()
	authed.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	if s.gateRepoRead(rec, authed, git.ServiceUploadPack, id, auth.Principal{Name: "bob"}) {
		t.Error("deny-gate authed must fail")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("deny-gate authed = %d, want 403", rec.Code)
	}
}

func TestRepoPageGatedVisibility(t *testing.T) {
	// Public (allow-gate): anonymous reaches the shell with the flag off.
	s, h := newTestServer(t, func(o *Options) { o.ReadGate = &stubGate{} })
	req := httptest.NewRequest("GET", "http://x/o/r", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("public shell anon = %d, want the SPA shell", rec.Code)
	}
	// Private (deny-gate): anonymous curl keeps the legacy 401.
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}}
	req = httptest.NewRequest("GET", "http://x/o/r", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("private shell anon curl = %d, want 401", rec.Code)
	}
	// ...and an anonymous browser keeps the #344 login page (not a bare
	// 401, not the shell).
	req = httptest.NewRequest("GET", "http://x/o/r", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Log in with OIDC") {
		t.Fatalf("private shell anon browser = %d, want the #344 login page", rec.Code)
	}
	// Authenticated callers are unaffected by the visibility branch.
	req = httptest.NewRequest("GET", "http://x/o/r", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed shell = %d, want 200", rec.Code)
	}
	// Nil gate: legacy behavior — anonymous stays out with the flag off.
	s.readGate = nil
	req = httptest.NewRequest("GET", "http://x/o/r", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil-gate shell anon = %d, want 401", rec.Code)
	}
	_ = s
}

func TestSSHUploadReadGate(t *testing.T) {
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, nil)
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	id := mustRepoID(t, "o/r")
	mkCall := func() error {
		return s.SSHUploadPack(ctx, id, "", sshd.Principal{Name: "ada"}, strings.NewReader(""), nilWriter{}, nilWriter{})
	}
	// Deny surfaces its reason — even when drained (the read gate runs
	// before every transport gate).
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}}
	s.Drain().Begin2()
	if err := mkCall(); err == nil || !strings.Contains(err.Error(), "read access required") {
		t.Fatalf("deny-gate ssh fetch = %v, want the gate reason", err)
	}
	// Allow passes the read gate: the DRAIN refusal proves the ordering
	// (a gate failure would surface its own reason instead).
	s.readGate = &stubGate{}
	if err := mkCall(); err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("allow-gate drained ssh fetch = %v, want the drain refusal", err)
	}
}

// nilWriter discards writes (SSHUploadPack takes io.Writers, not httptest).
type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }
