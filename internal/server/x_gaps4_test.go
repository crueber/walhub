package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// sha1Sum renders the hex sha1 of body (a 40-hex LFS oid variant).
func sha1Sum(body string) string {
	sum := sha1.Sum([]byte(body))
	return hex.EncodeToString(sum[:])
}

// errReader is a body that always fails to read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }
func (errReader) Close() error             { return nil }

// failingBodyReq wraps a request with a body that errors mid-read.
func failingBodyReq(method, url string) *http.Request {
	req := httptest.NewRequest(method, url, nil)
	req.Body = errReader{}
	return req
}

func TestSmallHelpersDirect(t *testing.T) {
	// sameSiteFor truth table.
	if sameSiteFor([]string{"x"}) != http.SameSiteNoneMode || sameSiteFor(nil) != http.SameSiteLaxMode {
		t.Fatal("sameSiteFor truth table broken")
	}
	// truncate + trimPrefixFold partial branches.
	if truncate("abcdef", 3) != "abc" || truncate("ab", 3) != "ab" {
		t.Fatal("truncate truth table broken")
	}
	if trimPrefixFold("plain", "git/") != "plain" {
		t.Fatal("trimPrefixFold non-match must keep input")
	}
	// ReqLog returns the request-scoped logger when present.
	s, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.serverHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ReqLog(r) == nil {
			t.Error("ReqLog must never be nil inside the chain")
		}
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	// newRequestID returns the zero id when the RNG fails (hard to force) —
	// assert the success shape only.
	if got := newRequestID(); len(got) != 32 {
		t.Fatalf("newRequestID = %q", got)
	}
}

// TestAuthFailureUnavailable covers the Retry-After branch of authFailure.
func TestAuthFailureUnavailable(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.authFailure(rec, httptest.NewRequest("GET", "/x", nil),
		&auth.AuthError{Kind: auth.ErrUnavailable, Why: "overloaded"})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("unavailable = %d", rec.Code)
	}
	// Forbidden branch.
	rec = httptest.NewRecorder()
	s.authFailure(rec, httptest.NewRequest("GET", "/x", nil),
		&auth.AuthError{Kind: auth.ErrForbidden, Why: "denied"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden = %d", rec.Code)
	}
}

// TestJWKSStaleWhileRefresh covers the stale-key branch of Get.
func TestJWKSStaleWhileRefresh(t *testing.T) {
	iss := newStubIssuer(t)
	j := NewJWKS(iss.srvURL())
	// Prime keys via a refresh.
	if _, aerr := j.Get(context.Background(), "k1"); aerr != nil {
		t.Fatalf("prime: %v", aerr)
	}
	// Age the fetch timestamp past ttl → stale-while-refresh serves the key.
	j.mu.Lock()
	j.fetched = time.Now().Add(-10 * time.Minute)
	j.mu.Unlock()
	k, aerr := j.Get(context.Background(), "k1")
	if aerr != nil || k == nil {
		t.Fatalf("stale get = %v %v", k, aerr)
	}
	// The background refresh re-fetched; the fetch clock is fresh again.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j.mu.RLock()
		fresh := time.Since(j.fetched) < j.ttl
		j.mu.RUnlock()
		if fresh {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background refresh did not run")
}

// TestGitInfoRefsSyncGenericError covers the non-not-found sync failure.
func TestGitInfoRefsSyncGenericError(t *testing.T) {
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}, exists: true},
		syncErr: errors.New("sync boom")}
	s, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("generic sync via gitish = %d", rec.Code)
	}
	if _, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(rec.Body.String(), "sync boom") {
		t.Fatalf("want pkt ERR naming the error, got %q", rec.Body.String())
	}
	// Non-git client → 503.
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("generic sync non-git = %d", rec.Code)
	}
}

// TestReceivePackBodyReadFailure covers the ReadAll failure branch.
func TestReceivePackBodyReadFailure(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	req := failingBodyReq("POST", "http://x/o/r.git/git-receive-pack")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body read failure = %d", rec.Code)
	}
}

// TestLFSPutReadFailure covers copyTee's read-error branch.
func TestLFSPutReadFailure(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	oid := lfsOID(t, "anything")
	req := failingBodyReq("PUT", "http://x/o/r.git/info/lfs/objects/"+oid)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, mustRepoID(t, "o/r"), oid)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("lfs read failure = %d", rec.Code)
	}
}

// TestLFSPutSpoolDirUnwritable covers the CreateTemp failure branch.
func TestLFSPutSpoolDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, _ := newTestServer(t, nil)
	root := t.TempDir()
	if err := os.MkdirAll(root+"/lfs-spool", 0o500); err != nil {
		t.Fatal(err)
	}
	s.cacheRoot = root
	body := "unwritable"
	oid := lfsOID(t, body)
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, mustRepoID(t, "o/r"), oid)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwritable spool = %d", rec.Code)
	}
}

// TestLFSPutShortOidAccepted covers the 40-hex (sha1-style) oid path.
func TestLFSPutShortOidAccepted(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	body := "sha1-style-object"
	sum := sha1Sum(body)
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+sum, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, mustRepoID(t, "o/r"), sum)
	if rec.Code != http.StatusOK {
		t.Fatalf("sha1-style put = %d %s", rec.Code, rec.Body.String())
	}
	// The object is stored under the short oid key.
	meta, _ := s.store.Head(context.Background(), lfsKey(mustRepoID(t, "o/r"), sum))
	if meta == nil {
		t.Fatal("short-oid object must be stored")
	}
}

// TestLFSBatchInvalidToken covers lfsAuth's Authenticate-error branch.
func TestLFSBatchInvalidToken(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch",
		strings.NewReader(`{"operation":"download","objects":[]}`))
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.lfsBatch(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("lfs invalid token = %d", rec.Code)
	}
	// Download with a read-denied (anonymous) principal → 401.
	req = httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch",
		strings.NewReader(`{"operation":"download","objects":[]}`))
	rec = httptest.NewRecorder()
	s.lfsBatch(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("lfs anon batch = %d", rec.Code)
	}
}

// TestLFSVerifyMalformed covers the verify decode-failure branch.
func TestLFSVerifyMalformed(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/verify", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsVerify(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed verify = %d", rec.Code)
	}
}

// TestSetupOnlyMethodFallback covers mountSetupOnly's 405 fallback.
func TestSetupOnlyMethodFallback(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "setup_only"}})
	h := s.Handler()
	req := httptest.NewRequest("POST", "http://x/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("setup-only 405 fallback = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/setup") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestRepoDispatchBadRepoName covers the ParseRepoId 404 via an invalid repo name.
func TestRepoDispatchBadRepoName(t *testing.T) {
	_, h := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/o/r%20bad/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad repo name = %d", rec.Code)
	}
}

// TestSetupJSONTLSAndOIDCOnRealTLS covers the tlsOn variant of setupJSON.
func TestSetupJSONTLSOn(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.tlsOn = true
	req := httptest.NewRequest("GET", "http://x/services/setup.json?repo=o/r", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupJSON(rec, req)
	if !strings.Contains(rec.Body.String(), "ca_url") {
		t.Fatalf("tls setup.json missing ca_url: %s", rec.Body.String())
	}
}

// TestRenderInstallErrorPaths are unreachable template failures; exercise the
// happy path of credentialHelper directly instead.
func TestCredentialHelperRenders(t *testing.T) {
	out, err := credentialHelper(installTmplData{
		Host: "wal.example.com", Slug: "wal-example-com", AuthNone: false,
	})
	if err != nil || !strings.Contains(out, "wal-example-com") {
		t.Fatalf("credential helper = %q %v", out, err)
	}
}

// TestWgtPrincipalDenied covers the wgt path with a disallowed email.
func TestWgtPrincipalDenied(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	svc := NewAuthService(&s.cfg.Server.Auth, s.Now)
	tok, err := svc.MintToken("mallory@evil.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := svc.wgtPrincipal(tok.Wire); aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Fatalf("disallowed email = %v", aerr)
	}
}

// TestStatic503HeadUnreachable documents that serveStatic's generic Head-error
// 503 branch is shadowed by the meta==nil 404 branch; the stream-failure path
// is covered in x_gaps2_test.go.
func TestStaticStreamRangeErrorLogged(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.store = failingStore{ObjectStore: s.store, getErr: errors.New("get boom")}
	rec := httptest.NewRecorder()
	s.streamRange(context.Background(), rec, "repos/o/r/none", store.GetOptions{})
	if rec.Body.Len() != 0 {
		t.Fatalf("streamRange error body = %q", rec.Body.String())
	}
}
