package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
)

// stubRepoExtra claims one sub-path family (the releases byte shape).
type stubRepoExtra struct {
	claimed int
}

func (s *stubRepoExtra) HandleRepo(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool {
	if len(sub) == 4 && sub[0] == "releases" && sub[2] == "assets" {
		s.claimed++
		w.WriteHeader(http.StatusTeapot)
		return true
	}
	return false
}

func TestRepoExtraByteRouteWins(t *testing.T) {
	s, h := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: true}
	stub := &stubRepoExtra{}
	s.ChainRepo(stub)

	// Byte-shaped path under a UI page prefix → the extra wins (uncompressed
	// static group; the SPA shell must not answer bytes).
	req := httptest.NewRequest("GET", "http://x/o/r/releases/v1/assets/tool", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || stub.claimed != 1 {
		t.Fatalf("byte route = %d claimed = %d", rec.Code, stub.claimed)
	}
	// Unclaimed family on a non-page prefix → 404 (extras consulted on
	// the default branch, core untouched).
	req = httptest.NewRequest("GET", "http://x/o/r/zzznope/v1/download", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unclaimed = %d, want 404", rec.Code)
	}
}

func TestRepoExtraNilSafe(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.ChainRepo() // no extras: nothing to front
	if s.repoDispatchRepoExtras(nil, nil, git.RepoId{}, []string{"releases"}) {
		t.Fatal("nil chain claimed")
	}
}

func TestUIPageRouteReleases(t *testing.T) {
	for _, page := range []string{"releases", "issues", "pulls", "checks"} {
		if !uiPageRoute(page) {
			t.Fatalf("%q is not a UI page", page)
		}
	}
	if uiPageRoute("releases-extra") || uiPageRoute("api") {
		t.Fatal("over-match")
	}
}
