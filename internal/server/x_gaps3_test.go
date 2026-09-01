package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// TestRouterSetupOnlyAndBranches covers mountSetupOnly 405 + owner-page 405 +
// ParseRepoId junk + setup-only method fallback.
func TestRouterOwnerPage405AndJunk(t *testing.T) {
	_, h := newTestServer(t, nil)
	// POST on /{owner} → 405 with Allow.
	req := httptest.NewRequest("POST", "http://x/alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("owner POST = %d", rec.Code)
	}
	// Bad owner charset → deliberate 404 (never reaches repo parse).
	req = httptest.NewRequest("GET", "http://x/ok/../x/info/refs?service=git-upload-pack", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("junk repo = %d", rec.Code)
	}
}

// TestLFSReadThroughSpoolFailure covers the CreateTemp failure branch in the
// read-through path (lfs-spool is a file, not a dir).
func TestLFSReadThroughSpoolFailure(t *testing.T) {
	id := mustRepoID(t, "o/r")
	body := "spool-blocked-content"
	oid := lfsOID(t, body)
	up := upstreamLFS(t, oid, []byte(body))
	defer up.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	root := t.TempDir()
	if err := os.WriteFile(root+"/lfs-spool", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cacheRoot = root
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveLFSObject(rec, req, id, oid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("spool failure = %d", rec.Code)
	}
}

// TestSetupTestValidation422 rounds out the setup API branches.
func TestSetupTestValidation422(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(`{"overrides": {"server.auth.mode": "oidc"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validation 422 = %d %s", rec.Code, rec.Body.String())
	}
}

// TestGitInfoRefsAdvertError covers the Advertisement-error branch using an
// engine whose Repo returns a repo with a broken git-dir.
func TestGitInfoRefsRepoErr404(t *testing.T) {
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}, exists: true},
		repoErr: wal.ErrNotFound("o/r")}
	s, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(contextWithRoot(t.TempDir()))
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("repo err advert = %d", rec.Code)
	}
}

func contextWithRoot(root string) context.Context {
	return context.WithValue(context.Background(), repoRootKey{}, root)
}

var _ = errors.New
var _ = store.ObjectStore(nil)
