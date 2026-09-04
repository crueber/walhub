package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// stubGate is a scripted ReadGate.
type stubGate struct{ err *auth.AuthError }

func (s *stubGate) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return s.err
}

// principalCapturingAPI records the injected principal for apiServe tests.
type principalCapturingAPI struct {
	fakeAPI
	got auth.Principal
	ok  bool
}

func (p *principalCapturingAPI) Serve(w http.ResponseWriter, r *http.Request) {
	if v, ok := r.Context().Value(ctxPrincipalKey{}).(auth.Principal); ok {
		p.got, p.ok = v, true
	}
	w.WriteHeader(http.StatusTeapot)
}

func TestApiServeAuth(t *testing.T) {
	var cap *principalCapturingAPI
	s, h := newTestServer(t, func(o *Options) {
		cap = &principalCapturingAPI{}
		o.API = cap
	})
	_ = s
	// Invalid token → real 401 (previously anonymous-treated).
	req := httptest.NewRequest("GET", "http://x/api/v1/owners", nil)
	req.Header.Set("Authorization", "Bearer bogus")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token api lane = %d", rec.Code)
	}
	// Valid token → delegates with the principal injected.
	req = httptest.NewRequest("GET", "http://x/api/v1/owners", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("good token api lane = %d", rec.Code)
	}
	if !cap.ok || cap.got.Name != "alice" {
		t.Fatalf("injected principal = %+v ok=%v", cap.got, cap.ok)
	}
	// Anonymous passes through untouched (legacy read behavior).
	req = httptest.NewRequest("GET", "http://x/api/v1/owners", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("anon api lane = %d", rec.Code)
	}
	// Repo lane also authenticates before dispatch.
	req = httptest.NewRequest("GET", "http://x/o/r/api/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer bogus")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token repo lane = %d", rec.Code)
	}
}

func TestChainExtra(t *testing.T) {
	primary := &fakeAPI{}
	matched := false
	extra := stubExtra{fn: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/extra" {
			matched = true
			w.WriteHeader(http.StatusOK)
			return true
		}
		return false
	}}
	chained := ChainAPI(primary, extra)
	req := httptest.NewRequest("GET", "http://x/api/v1/extra", nil)
	rec := httptest.NewRecorder()
	chained.Serve(rec, req)
	if rec.Code != http.StatusOK || !matched || primary.served != 0 {
		t.Fatalf("extra match: code=%d matched=%v served=%d", rec.Code, matched, primary.served)
	}
	req = httptest.NewRequest("GET", "http://x/api/v1/other", nil)
	rec = httptest.NewRecorder()
	chained.Serve(rec, req)
	if rec.Code != http.StatusTeapot || primary.served != 1 {
		t.Fatalf("fallthrough: code=%d served=%d", rec.Code, primary.served)
	}
	owners, _ := chained.Owners(req)
	_ = owners
	// Nil api seam: ChainExtra is a no-op.
	s, _ := newTestServer(t, nil)
	s.api = nil
	s.ChainExtra(extra) // must not panic
	if s.api != nil {
		t.Error("nil api must stay nil")
	}
}

type stubExtra struct {
	fn func(w http.ResponseWriter, r *http.Request) bool
}

func (s stubExtra) Handle(w http.ResponseWriter, r *http.Request) bool { return s.fn(w, r) }

func TestCheckReadGate(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if aerr := s.checkReadGate(context.Background(), "o", "r", auth.Anonymous()); aerr != nil {
		t.Fatalf("nil gate must allow: %v", aerr)
	}
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}}
	if aerr := s.checkReadGate(context.Background(), "o", "r", auth.Anonymous()); aerr == nil {
		t.Error("deny gate must deny")
	}
	s.readGate = &stubGate{}
	if aerr := s.checkReadGate(context.Background(), "o", "r", auth.Anonymous()); aerr != nil {
		t.Errorf("allow gate must allow: %v", aerr)
	}
}

func TestReadGateGitPaths(t *testing.T) {
	// info/refs upload-pack with a denying gate → pkt ERR (git-ish eligible).
	s, _ := newTestServer(t, func(o *Options) {
		o.ReadGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}}
	})
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: false} // allow-through stops at placement → 503
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("gated info/refs = %d", rec.Code)
	}
	if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, "private") {
		t.Fatalf("gated body = %q", rec.Body.String())
	}
	// Allow-through continues to placement (fake denies with a pkt ERR for
	// git-eligible callers) — reaching placement proves the gate passed.
	s.readGate = &stubGate{}
	rec = httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("allow-through info/refs = %d", rec.Code)
	}
	if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, "served by") {
		t.Fatalf("allow-through body = %q", rec.Body.String())
	}

	// upload-pack POST with a denying gate → plain 403 (non-git UA).
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", nil)
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Set("Authorization", "Bearer tok123")
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}}
	rec = httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gated upload-pack = %d", rec.Code)
	}
	// Allow-through continues to placement → 503.
	s.readGate = &stubGate{}
	rec = httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("allow-through upload-pack = %d", rec.Code)
	}

	// Bundles list with a denying gate → 403.
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}}
	req = httptest.NewRequest("GET", "http://x/o/r/bundles/list", nil)
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, mustRepoID(t, "o/r"), []string{"list"}, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gated bundles = %d", rec.Code)
	}
}

func TestLfsReadGate(t *testing.T) {
	s, _ := newTestServer(t, nil)
	id := mustRepoID(t, "o/r")
	mkReq := func() *http.Request {
		req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/abc", nil)
		req.Header.Set("User-Agent", "git-lfs/3.0")
		req.Header.Set("Authorization", "Bearer tok123")
		return req
	}
	// Deny on reads.
	s.readGate = &stubGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}}
	rec := httptest.NewRecorder()
	if _, ok := s.lfsAuth(rec, mkReq(), id, false); ok {
		t.Error("gated LFS read must fail")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gated LFS read = %d", rec.Code)
	}
	// Writes skip the read gate.
	rec = httptest.NewRecorder()
	if _, ok := s.lfsAuth(rec, mkReq(), id, true); !ok {
		t.Fatalf("LFS write with write flag = %d", rec.Code)
	}
	// Allow-through on reads.
	s.readGate = &stubGate{}
	rec = httptest.NewRecorder()
	if _, ok := s.lfsAuth(rec, mkReq(), id, false); !ok {
		t.Fatalf("allowed LFS read = %d", rec.Code)
	}
}
