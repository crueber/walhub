package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

type putFailStore struct{ store.ObjectStore }

func (p *putFailStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errors.New("put boom")
}

// TestLFSPutStorePutFailure covers the store-write failure branch.
func TestLFSPutStorePutFailure(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	s.store = failingStore{ObjectStore: &putFailStore{ObjectStore: s.store}}
	body := "persist-fails"
	oid := lfsOID(t, body)
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, mustRepoID(t, "o/r"), oid)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("put failure = %d %s", rec.Code, rec.Body.String())
	}
}

// TestLFSReadThroughUpstream500 covers the non-200 upstream branch.
func TestLFSReadThroughUpstream500(t *testing.T) {
	id := mustRepoID(t, "o/r")
	body := "never-served"
	oid := lfsOID(t, body)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	s.cacheRoot = t.TempDir()
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveLFSObject(rec, req, id, oid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upstream 500 = %d", rec.Code)
	}
}

// TestLFSReadThroughSpoolCreateTempFail covers the read-through CreateTemp
// failure (unwritable spool dir).
func TestLFSReadThroughSpoolCreateTempFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	id := mustRepoID(t, "o/r")
	body := "read-through-blocked"
	oid := lfsOID(t, body)
	up := upstreamLFS(t, oid, []byte(body))
	defer up.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	root := t.TempDir()
	if err := os.MkdirAll(root+"/lfs-spool", 0o500); err != nil {
		t.Fatal(err)
	}
	s.cacheRoot = root
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveLFSObject(rec, req, id, oid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read-through temp fail = %d", rec.Code)
	}
}

// TestSetupValidateError422 drives the config.Validate failure branch of
// setupTest/setupPut with a negative size.
func TestSetupValidateError422(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.DataDir = t.TempDir()
	req := httptest.NewRequest("POST", "/api/v1/setup/test",
		strings.NewReader(`{"overrides": {"server.max_push_bytes": "-1"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validate 422 = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_push_bytes") {
		t.Fatalf("error naming key expected: %s", rec.Body.String())
	}
	req = httptest.NewRequest("PUT", "/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.max_push_bytes": "-1"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("put validate 422 = %d", rec.Code)
	}
}

// TestEventsNotifyAndSetupJSONInvalidToken cover the mapAuthStatus branches.
func TestEventsNotifyAndSetupJSONInvalidToken(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "/_events/notify", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.eventsNotify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("notify invalid = %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/services/setup.json", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec = httptest.NewRecorder()
	s.setupJSON(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("setup.json invalid = %d", rec.Code)
	}
}

// TestPrewarmTimedOutTrue covers the elapsed-timeout branch.
func TestPrewarmTimedOutTrue(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Cache.PrewarmReadyTimeout = config.Duration(time.Millisecond)
	time.Sleep(2 * time.Millisecond)
	if !s.prewarmTimedOut() {
		t.Fatal("elapsed timeout must report true")
	}
}

// TestWriteJSONBodyUnencodable covers the marshal-failure branch (NaN).
func TestWriteJSONBodyUnencodable(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONBody(rec, http.StatusOK, map[string]any{"n": math.NaN()})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unencodable = %d", rec.Code)
	}
}

// TestGitInfoRefsAutoCreateRepoFail covers the auto-create inner Repo failure
// (the push falls through to the plain 404).
func TestGitInfoRefsAutoCreateRepoFail(t *testing.T) {
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}, exists: false},
		repoErr: errors.New("create boom")}
	s, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("auto-create repo fail = %d", rec.Code)
	}
}
