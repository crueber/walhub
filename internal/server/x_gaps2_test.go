package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// failingStore wraps a store with injected Head/Get failures.
type failingStore struct {
	store.ObjectStore
	headErr error
	getErr  error
}

func (f failingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return f.ObjectStore.Head(ctx, key)
}

func (f failingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func TestGitInfoRefsAuthAndGateBranches(t *testing.T) {
	// Authenticate error → real 401.
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token = %d", rec.Code)
	}

	// Write-denied receive-pack advertisement → pkt-ERR.
	s2, _ := newTestServer(t, nil)
	s2.cfg.Server.Auth.Tokens = append(s2.cfg.Server.Auth.Tokens,
		config.StaticToken{Principal: "ro", Token: "ro-token", Write: false, Admin: false})
	s2.authSvc = NewAuthService(&s2.cfg.Server.Auth, s2.Now)
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer ro-token")
	rec = httptest.NewRecorder()
	s2.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("write-denied advert = %d", rec.Code)
	}
	if _, ok := pktErrOf(rec.Body.String()); !ok {
		t.Fatalf("want pkt ERR, got %q", rec.Body.String())
	}

	// Busy repo → 503 Retry-After.
	s3, _ := newTestServer(t, nil)
	s3.engine.(*fakeEngine).exists = true
	s3.sem = NewRepoSemaphores(1)
	rel := s3.sem.TryAcquire("o/r")
	if rel == nil {
		t.Fatal("acquire must succeed")
	}
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s3.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy advert = %d", rec.Code)
	}
	rel()

	// Missing repo upload-pack advertisement → 404.
	s4, _ := newTestServer(t, nil)
	s4.engine.(*fakeEngine).exists = false
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s4.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing advert = %d", rec.Code)
	}

	// v0 advertisement: "# service=" prefix + flush before the body.
	s5, _ := newTestServer(t, nil)
	s5.engine.(*fakeEngine).exists = true
	root := t.TempDir()
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec = httptest.NewRecorder()
	s5.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# service=git-upload-pack") {
		t.Fatalf("v0 advert = %d %q", rec.Code, rec.Body.String())
	}
}

func TestGitServiceAndUploadPackBranches(t *testing.T) {
	// gitService: Authenticate error → 401.
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gitService invalid = %d", rec.Code)
	}

	// gitService happy upload-pack dispatch (streams the child's answer).
	s2, _ := newTestServer(t, nil)
	fe := s2.engine.(*fakeEngine)
	fe.exists = true
	root := t.TempDir()
	body := append(git.Pkt("command=ls-refs\n"), git.Flush()...)
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Git-Protocol", "version=2")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec = httptest.NewRecorder()
	s2.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("gitService upload = %d %s", rec.Code, rec.Body.String())
	}

	// uploadPack: Repo error after successful Sync → 404.
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}, exists: true},
		repoErr: wal.ErrNotFound("o/r")}
	s3, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec = httptest.NewRecorder()
	s3.uploadPack(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("uploadPack repo error = %d", rec.Code)
	}
}

// TestReceivePackForwardBrokerOK covers the broker-success passthrough.
func TestReceivePackForwardBrokerOK(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("broker-report"))
	}))
	defer broker.Close()

	s, _ := newTestServer(t, nil)
	s.cfg.WAL.PushBrokerURL = broker.URL
	s.cfg.WAL.PushBrokerBufferBytes = 1 << 20
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", strings.NewReader("PUSHBODY"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.receivePackForward(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK || rec.Body.String() != "broker-report" {
		t.Fatalf("broker ok = %d %q", rec.Code, rec.Body.String())
	}
}

// TestReceivePackLocalPublishFailed covers the pkt-ERR publish failure shape.
func TestReceivePackLocalPublishFailed(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.pubErr = errors.New("wal closed")
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	var b bytes.Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/x\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(b.Bytes()))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "publish failed") {
		t.Fatalf("publish failed = %d %s", rec.Code, rec.Body.String())
	}
}

// TestReceivePackLocalCorruptGzipBody drives the ReadAll failure after a
// valid gzip header: bodyReader succeeds, the full read fails → 400.
func TestReceivePackLocalCorruptGzipBody(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	// A truncated gzip stream: valid 2-byte magic, then garbage.
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack",
		bytes.NewReader([]byte{0x1f, 0x8b, 0x00, 0x01}))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Content-Encoding", "gzip")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("corrupt gzip body = %d", rec.Code)
	}
}

// TestBundlesDispatchErrorBranches covers auth failure and generic engine error.
func TestBundlesDispatchErrorBranches(t *testing.T) {
	// Authenticate error → 401.
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/o/r.git/bundles/list", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.bundlesDispatch(rec, req, mustRepoID(t, "o/r"), []string{"list"}, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bundles invalid = %d", rec.Code)
	}
	// Generic Bundles error → 503.
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}, exists: true},
		bundlesErr: errors.New("store down")}
	s2, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req = httptest.NewRequest("GET", "http://x/o/r.git/bundles/list", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s2.bundlesDispatch(rec, req, mustRepoID(t, "o/r"), []string{"list"}, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bundles error = %d", rec.Code)
	}
}

func (e *errEngine) Bundles(ctx context.Context, id git.RepoId, filter string) (BundleList, error) {
	if e.bundlesErr != nil {
		return BundleList{}, e.bundlesErr
	}
	return e.fakeEngine.Bundles(ctx, id, filter)
}

// TestStaticStoreFailures covers the stream-failure path (status already sent).
func TestStaticStoreFailures(t *testing.T) {
	s2, _ := newTestServer(t, nil)
	// Get failure mid-stream → logged, empty body, status already sent.
	rec := httptest.NewRecorder()
	if _, err := s2.store.Put(context.Background(), "repos/o/r/bundles/full/b.bundle",
		store.PutBody{Bytes: []byte("DATA")}, store.PutOptions{Immutable: true}); err != nil {
		t.Fatal(err)
	}
	s2.store = failingStore{ObjectStore: s2.store, getErr: errors.New("get boom")}
	rec = httptest.NewRecorder()
	s2.serveStatic(rec, httptest.NewRequest("GET", "/x", nil), "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
	if rec.Code != http.StatusOK {
		t.Fatalf("get failure status = %d", rec.Code)
	}
}

// TestRouterBareRoot405 covers POST on the bare repo root when no api seam.
func TestRouterBareRoot405(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.api = nil // PUT/DELETE fall through; POST hits the UI-page 405
	req := httptest.NewRequest("POST", "http://x/o/r.git", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("bare POST = %d", rec.Code)
	}
}

// TestOIDCCallbackNonLoopbackCookie covers the direct cookie-set branch.
func TestOIDCCallbackNonLoopbackCookie(t *testing.T) {
	s, _, iss := oidcFull(t)
	tv := true
	iss.idTokens <- iss.mint(t, map[string]any{
		"aud": "walhub", "exp": time.Now().Add(time.Hour).Unix(),
		"email": "alice@example.com", "email_verified": tv,
	})
	state := s.signState("/settings", s.Now())
	req := httptest.NewRequest("GET", "http://wal.example.com/_auth/callback?code=c&state="+state, nil)
	rec := httptest.NewRecorder()
	s.authCallback(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("non-loopback callback = %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("session cookie must be set directly on non-loopback hosts")
	}
}

// TestSetupAccessInvalidToken covers the mapAuthStatus path inside setupAccess.
func TestSetupAccessInvalidToken(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/api/v1/setup", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.setupGet(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("setup invalid token = %d", rec.Code)
	}
}

// TestSetupValidationErrorsAndListCoerce covers the 422 validation branches and
// the TOML array coercion path.
func TestSetupValidationErrorsAndListCoerce(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.DataDir = t.TempDir()
	// oidc without client id/secret → validation error → 422 (both test+put).
	for _, body := range []string{
		`{"overrides": {"server.auth.mode": "oidc"}}`,
	} {
		req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		s.setupTest(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("oidc validation = %d %s", rec.Code, rec.Body.String())
		}
	}
	// TOML array values coerce into a comma-joined list.
	req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(
		"[server]\ncors_origins = [\"a.test\", \"b.test\"]\n"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("toml array save = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "server.cors_origins") {
		t.Fatalf("changed key missing: %s", rec.Body.String())
	}
}
