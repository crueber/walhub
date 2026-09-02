package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reflect"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- auth.go: JWKS fetch / discover failure branches ---------------------------------

func TestJWKSFetchAndDiscoverFailures(t *testing.T) {
	iss := newStubIssuer(t)

	// discovery 500.
	iss.discStatus = http.StatusInternalServerError
	j := NewJWKS(iss.srvURL())
	if err := j.fetch(context.Background()); err == nil {
		t.Fatal("discovery 500 must fail")
	}
	// discovery malformed JSON.
	iss.discStatus = 0
	iss.discBody = "{bad"
	if err := j.fetch(context.Background()); err == nil {
		t.Fatal("discovery bad body must fail")
	}
	// discovery without jwks_uri.
	iss.discBody = ""
	iss.noJWKSURI = true
	if err := j.fetch(context.Background()); err == nil {
		t.Fatal("missing jwks_uri must fail")
	}
	// jwks 500.
	iss.noJWKSURI = false
	iss.jwksStatus = http.StatusInternalServerError
	if err := j.fetch(context.Background()); err == nil {
		t.Fatal("jwks 500 must fail")
	}
	// jwks malformed JSON.
	iss.jwksStatus = 0
	iss.jwksBody = "{bad"
	if err := j.fetch(context.Background()); err == nil {
		t.Fatal("jwks bad body must fail")
	}
	// Get surfaces the refresh failure as ErrUnavailable.
	if _, aerr := j.Get(context.Background(), "k1"); aerr == nil || aerr.Kind != auth.ErrUnavailable {
		t.Fatalf("get with broken issuer = %v", aerr)
	}
	// discoverDoc: non-200 status.
	iss.discStatus = http.StatusForbidden
	if _, err := j.discoverDoc(context.Background()); err == nil {
		t.Fatal("discoverDoc 403 must fail")
	}
	iss.discStatus = 0
}

// --- auth.go: JWT Verify branch coverage ----------------------------------------------

func TestJWKSVerifyBranches(t *testing.T) {
	iss := newStubIssuer(t)
	j := NewJWKS(iss.srvURL())
	// Seed discovery + keys once.
	if _, aerr := j.Get(context.Background(), "k1"); aerr != nil {
		t.Fatalf("seed keys: %v", aerr)
	}
	// An EC key under the same issuer for key-type mismatches.
	ecKey, _ := ecdsaGenerate()
	j.mu.Lock()
	ec := &jwk{Kty: "EC", Crv: "P-256", Kid: "ec1", Alg: "ES256",
		X: b64url(ecKey.X.Bytes()), Y: b64url(ecKey.Y.Bytes())}
	if err := ec.parse(); err != nil {
		t.Fatal(err)
	}
	j.keys["ec1"] = ec
	j.mu.Unlock()

	a := &config.Auth{Issuer: iss.srvURL(), OAuthClientID: "walhub",
		Audiences: []string{"extra"}, AllowedDomains: []string{"example.com"}}
	tv := true
	base := map[string]any{
		"iss": iss.srvURL(), "aud": "walhub",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": "alice@example.com", "email_verified": tv,
	}
	mintRS := func(claims any) string {
		hdr := b64url([]byte(`{"alg":"RS256","kid":"k1"}`))
		body, _ := json.Marshal(claims)
		payload := b64url(body)
		sum := sha256.Sum256([]byte(hdr + "." + payload))
		sig, err := rsa.SignPKCS1v15(rand.Reader, iss.rsaKey, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return hdr + "." + payload + "." + b64url(sig)
	}
	mustFail := func(name, tok string, want auth.AuthErrorKind) {
		t.Helper()
		_, aerr := j.Verify(context.Background(), tok, a, false)
		if aerr == nil || aerr.Kind != want {
			t.Fatalf("%s: got %v, want %v", name, aerr, want)
		}
	}
	inv := auth.ErrInvalid

	// Malformed header base64.
	mustFail("header b64", "!!!.e30.AAAA", inv)
	// Unsupported alg.
	hdr := b64url([]byte(`{"alg":"none","kid":"k1"}`))
	body := b64url([]byte(`{"iss":"` + iss.srvURL() + `","aud":"walhub","email":"a@example.com"}`))
	mustFail("unsupported alg", hdr+"."+body+".AAAA", inv)
	// Get failure (unknown kid + dead issuer): use a fresh JWKS on a dead issuer.
	dead := NewJWKS("http://127.0.0.1:1")
	tok := mintRS(base)
	parts := strings.Split(tok, ".")
	_, aerr := dead.Get(context.Background(), "k1")
	if aerr == nil || aerr.Kind != auth.ErrUnavailable {
		t.Fatalf("dead issuer get = %v", aerr)
	}
	// Signature b64 failure: keep the correct signature for a DIFFERENT payload,
	// then hand back an undecodable signature segment.
	mustFail("sig b64", parts[0]+"."+parts[1]+".!!!", inv)
	// Claims that are not JSON: sign "not-json" as the payload.
	hdrRs := b64url([]byte(`{"alg":"RS256","kid":"k1"}`))
	np := b64url([]byte("not-json"))
	sum := sha256.Sum256([]byte(hdrRs + "." + np))
	sig, err := rsa.SignPKCS1v15(rand.Reader, iss.rsaKey, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	mustFail("claims json", hdrRs+"."+np+"."+b64url(sig), inv)
	// aud as a JSON number (not string/array) → auds empty → audience mismatch.
	numAud := map[string]any{}
	for k, v := range base {
		numAud[k] = v
	}
	numAud["aud"] = 42
	mustFail("numeric aud", mintRS(numAud), inv)
	// Expired.
	expired := map[string]any{}
	for k, v := range base {
		expired[k] = v
	}
	expired["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	mustFail("expired", mintRS(expired), inv)
	// Issued in the future.
	future := map[string]any{}
	for k, v := range base {
		future[k] = v
	}
	future["iat"] = time.Now().Add(2 * time.Hour).Unix()
	mustFail("future iat", mintRS(future), inv)
	// EC key advertised, RS256 token → key type mismatch.
	j.mu.Lock()
	saved := j.keys["k1"]
	j.keys["k1"] = ec
	j.mu.Unlock()
	mustFail("key type", tok, inv)
	j.mu.Lock()
	j.keys["k1"] = saved
	j.mu.Unlock()
	// ES256 token against the RSA key → key type mismatch on the EC branch.
	_ = ecKey
	mustFail("ec on rsa", hdrES("k1", base)+"."+badSig(), inv)
	// Malformed claims JSON via a validly signed but non-object payload is
	// covered above; empty email → invalid.
	noEmail := map[string]any{}
	for k, v := range base {
		noEmail[k] = v
	}
	noEmail["email"] = ""
	mustFail("no email", mintRS(noEmail), inv)
}

func hdrES(kid string, claims map[string]any) string {
	hdr := b64url([]byte(`{"alg":"ES256","kid":"` + kid + `"}`))
	body, _ := json.Marshal(claims)
	return hdr + "." + b64url(body)
}

func badSig() string { return b64url(make([]byte, 64)) }

// --- auth_oidc.go: state/ticket decode branches ----------------------------------------

func TestVerifyStateB64Branches(t *testing.T) {
	s, _ := oidcServer(t)
	if _, ok := s.verifyState("!!!.???"); ok {
		t.Fatal("invalid base64 state must fail")
	}
	good := s.signState("/x", s.Now())
	parts := strings.Split(good, ".")
	if _, ok := s.verifyState(parts[0] + ".!!!"); ok {
		t.Fatal("invalid base64 mac must fail")
	}
	if _, ok := s.verifyStateTicket("!!!.???"); ok {
		t.Fatal("invalid base64 ticket must fail")
	}
	payload := b64url([]byte("one-line"))
	mac := b64url(hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte("one-line")))
	if _, ok := s.verifyStateTicket(payload + "." + mac); ok {
		t.Fatal("one-line ticket must fail")
	}
}

// --- bind_wal.go: Sync success, newLocalPack variants -----------------------------------

func TestWalEngineSyncSuccessReleases(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")
	if _, err := e.Repo(ctx, id, true, git.Sha1); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx, id, wal.LevelServe); err != nil {
		t.Fatalf("sync on live handle: %v", err)
	}
	if err := e.Sync(ctx, id, wal.LevelFull); err != nil {
		t.Fatalf("full sync: %v", err)
	}
}

func TestWalEngineNewLocalPackSkipsKnownAndDirs(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")
	h, err := reg.Create(ctx, id.String(), git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	packDir := h.Repo().PackDir()
	// A subdirectory and a non-idx file are skipped.
	if err := os.MkdirAll(filepath.Join(packDir, "subdir.idx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "stray.pack"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "aa.idx"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "bb.idx"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := e.newLocalPack(h)
	if p == nil || (p.Checksum != "aa" && p.Checksum != "bb") {
		t.Fatalf("newest candidate expected, got %+v", p)
	}
	// Removing the pack dir entirely → nil.
	if err := os.RemoveAll(packDir); err != nil {
		t.Fatal(err)
	}
	if p := e.newLocalPack(h); p != nil {
		t.Fatalf("missing pack dir must yield nil, got %+v", p)
	}
}

// --- lfs.go: dispatch PUT/verify, default cap, read-through branches --------------------

func TestLFSDispatchPutAndVerify(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	id := mustRepoID(t, "o/r")
	body := "via-dispatch"
	oid := lfsOID(t, body)
	// PUT through the dispatcher.
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsDispatch(rec, req, id, []string{"objects", oid})
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch put = %d", rec.Code)
	}
	// VERIFY through the dispatcher.
	req = httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/verify",
		strings.NewReader(`{"oid":"`+oid+`","size":`+fmt.Sprint(len(body))+`}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.lfsDispatch(rec, req, id, []string{"verify"})
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch verify = %d", rec.Code)
	}
}

func TestLFSPutDefaultCap(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.LFS.MaxObjectBytes = 0 // → default cap
	s.cacheRoot = t.TempDir()
	body := "small"
	oid := lfsOID(t, body)
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, mustRepoID(t, "o/r"), oid)
	if rec.Code != http.StatusOK {
		t.Fatalf("default cap put = %d", rec.Code)
	}
}

func TestLFSUpstreamResolveWithoutSizeParam(t *testing.T) {
	id := mustRepoID(t, "o/r")
	body := "no-size-param"
	oid := lfsOID(t, body)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		fmt.Fprintf(w, `{"transfer":["basic"],"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":"http://%s/objects/%s"}}}]}`,
			oid, len(body), r.Host, oid)
	}))
	defer up.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	size, ok := s.lfsUpstreamResolve(context.Background(), id, oid)
	if !ok || size != int64(len(body)) {
		t.Fatalf("resolve without ?size = %d %v", size, ok)
	}
}

func TestLFSReadThroughWithTokenAndClientError(t *testing.T) {
	id := mustRepoID(t, "o/r")
	body := "read-through-token"
	oid := lfsOID(t, body)
	up := upstreamLFS(t, oid, []byte(body))
	defer up.Close()
	t.Setenv("WALHUB_TEST_UPSTREAM_TOKEN", "up-tok")
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	s.cfg.Upstream.TokenEnv = "WALHUB_TEST_UPSTREAM_TOKEN"
	s.cacheRoot = t.TempDir()
	// A client writer that fails on the first write: io.Copy aborts with an
	// error, so the persist gate (cerr != nil) must NOT persist.
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := &failingRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.serveLFSObject(rec, req, id, oid)
	time.Sleep(80 * time.Millisecond)
	if meta, _ := s.store.Head(context.Background(), lfsKey(id, oid)); meta != nil {
		t.Fatal("failed client copy must not persist (cerr gate)")
	}
}

type failingRecorder struct {
	*httptest.ResponseRecorder
}

func (f *failingRecorder) Write(p []byte) (int, error) {
	return 0, errors.New("client went away")
}

// TestLFSReadThroughHashMismatch covers the no-persist-on-mismatch branch.
func TestLFSReadThroughHashMismatch(t *testing.T) {
	id := mustRepoID(t, "o/r")
	oid := lfsOID(t, "expected-content")
	up := upstreamLFS(t, oid, []byte("different-bytes-entirely"))
	defer up.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	s.cacheRoot = t.TempDir()
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsGet(rec, req, id, oid)
	if rec.Code != http.StatusOK {
		t.Fatalf("mismatched read-through = %d", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if meta, _ := s.store.Head(context.Background(), lfsKey(id, oid)); meta != nil {
		t.Fatal("hash mismatch must not persist")
	}
}

// --- middleware.go small branches --------------------------------------------------------

func TestRequestIDNoWriteHandler(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty handler = %d", rec.Code)
	}
}

func TestInflightWarnOverCap(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.inflight.high = -1 // force the advisory-cap warning
	h := s.requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("warn path = %d", rec.Code)
	}
}

func TestCanonicalBrowserHostTLS(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.tlsOn = true
	req := httptest.NewRequest("GET", "http://localhost:8443/wal", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.canonicalBrowserHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://") {
		t.Fatalf("tls redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRecoverPanicAfterWriteBranch(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
		panic("boom")
	}))
	inner := httptest.NewRecorder()
	rw := &recorder{ResponseWriter: inner}
	h.ServeHTTP(rw, httptest.NewRequest("GET", "/x", nil))
	if !strings.Contains(inner.Body.String(), "internal error") {
		t.Fatalf("touched-panic body = %q", inner.Body.String())
	}
}

func TestOriginAllowedPortAndLabel(t *testing.T) {
	allowed := []string{"*.example.com"}
	// The outer suffix check runs on the raw origin, so a port defeats the
	// wildcard match (only the inner host strip covers the label check).
	if originAllowed(allowed, "https://sub.example.com:8443") {
		t.Fatal("port variant must not match (raw-origin suffix check)")
	}
}

func TestMaybeRefreshSessionBranches(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	// Anonymous principal → no-op.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	s.maybeRefreshSession(rec, req, auth.Anonymous())
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("anonymous must not refresh")
	}
	// No cookie → no-op.
	rec = httptest.NewRecorder()
	s.maybeRefreshSession(rec, httptest.NewRequest("GET", "/x", nil), principalAlice)
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("missing cookie must not refresh")
	}
	// Corrupt cookie → no-op.
	req = httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: "junk.junk"})
	rec = httptest.NewRecorder()
	s.maybeRefreshSession(rec, req, principalAlice)
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("corrupt cookie must not refresh")
	}
}

// --- router.go gated / setup-only / lane-root branches -----------------------------------

func TestGatedMetricsInvalidToken(t *testing.T) {
	_, h := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/metrics", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gated metrics = %d", rec.Code)
	}
}

func TestSetupOnlyAPINotWiredAndFallbacks(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "setup_only"}})
	h := s.Handler()
	// setupAPIRouter fallback → 404.
	req := httptest.NewRequest("GET", "http://x/api/v1/setup/extra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("setup api fallback = %d", rec.Code)
	}
}

func TestRepoDispatchLaneRootAndUploadPack(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: true}
	// Lane root GET falls through to the gated repo page.
	s.api = nil
	req := httptest.NewRequest("GET", "http://x/o/r/api", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.repoDispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lane root = %d", rec.Code)
	}
	// git-upload-pack dispatched through repoDispatch.
	body := append(git.Pkt("command=ls-refs\n"), git.Flush()...)
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Git-Protocol", "version=2")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec = httptest.NewRecorder()
	s.repoDispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("router upload-pack = %d", rec.Code)
	}
}

// --- smart.go placement / forward / report branches ---------------------------------------

type nilPlacementEngine struct{ fakeEngine }

func (e *nilPlacementEngine) Placement(ctx context.Context, id git.RepoId) (Placement, error) {
	return Placement{}, errors.New("no heartbeat")
}

func TestPlacementOKFallbacks(t *testing.T) {
	// Engine Placement error → serve.
	ee := &errEngine{fakeEngine: fakeEngine{exists: true}}
	ee.placementErr = errors.New("heartbeat missing")
	s, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	if !s.placementOK(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack) {
		t.Fatal("placement error must allow serving")
	}
	// Not served + no ServedBy → "another host".
	s2, _ := newTestServer(t, nil)
	s2.engine.(*fakeEngine).placement = Placement{Serve: false}
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	if s2.placementOK(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack) {
		t.Fatal("not served must refuse")
	}
	if !strings.Contains(rec.Body.String(), "another host") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestGitServiceReceivePackDispatch(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	var b bytes2Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/none\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", b.reader())
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceReceivePack, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unpack ok") {
		t.Fatalf("gitService receive = %d %s", rec.Code, rec.Body.String())
	}
}

type bytes2Buffer struct{ b []byte }

func (x *bytes2Buffer) Write(p []byte) (int, error) { x.b = append(x.b, p...); return len(p), nil }
func (x *bytes2Buffer) reader() *strings.Reader     { return strings.NewReader(string(x.b)) }

func TestUploadPackCorruptGzipAndChildError(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	// Corrupt gzip → 400 from bodyReader.
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", strings.NewReader("junk"))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Content-Encoding", "gzip")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.uploadPack(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upload corrupt gzip = %d", rec.Code)
	}
	// v0 junk body → child fails → warn branch (no header change).
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", strings.NewReader("junk-body"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec = httptest.NewRecorder()
	s.uploadPack(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload child error = %d", rec.Code)
	}
}

func TestForwardToBrokerNewRequestError(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.WAL.PushBrokerURL = "http://bad url\x7f"
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", nil)
	if _, err := s.forwardToBroker(context.Background(), req, []byte("x"), "alice"); err == nil {
		t.Fatal("bad broker URL must error")
	}
}

func TestReceivePackBrokerNon200FallsBack(t *testing.T) {
	root := t.TempDir()
	fe := &fakeEngine{placement: Placement{Serve: false, Maintain: false}, exists: true}
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = fe
		o.Config.WAL.PushBrokerURL = "http://127.0.0.1:1"
		o.Config.WAL.PushBrokerBufferBytes = 1 << 20
	})
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	if _, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1); err != nil {
		t.Fatal(err)
	}
	var b bytes2Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/none\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", b.reader())
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackForward(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("broker non-200 fallback = %d", rec.Code)
	}
}

func TestReceivePackLocalWithBrokerSetButServed(t *testing.T) {
	fe := &fakeEngine{placement: Placement{Serve: true, Maintain: true}, exists: true}
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = fe
		o.Config.WAL.PushBrokerURL = "http://broker.test"
	})
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	var b bytes2Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/none\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", b.reader())
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePack(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("served placement local push = %d", rec.Code)
	}
}

func TestReceivePackPerRefErrorReport(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.pubPerRef = []wal.RefResult{{Name: "refs/heads/bad", Err: &wal.RefError{Kind: wal.RefErrConflict, Detail: "conflict"}}}
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	var b bytes2Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/none\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", b.reader())
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ng refs/heads/bad conflict") {
		t.Fatalf("per-ref error report = %d %s", rec.Code, rec.Body.String())
	}
}

func TestErrorsAsWrappedNonWal(t *testing.T) {
	var we *wal.WalError
	if errorsAs(fmt.Errorf("outer: %w", errors.New("inner")), &we) {
		t.Fatal("wrapped non-wal error must not resolve")
	}
}

// --- misc small branches -------------------------------------------------------------------

func TestSetupUIAssetsGone(t *testing.T) {
	// D-WEB-6: the standalone setup assets died with the vanilla page; /setup
	// is a SPA route (open in defaults mode, the API keeps its own gating).
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "defaults"}})
	rec := httptest.NewRecorder()
	s.setupUI(rec, httptest.NewRequest("GET", "/setup", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=\"root\"") {
		t.Fatalf("setup page = %d, want the SPA shell", rec.Code)
	}
}

func TestSecretKeyAndFmtAnyDirect(t *testing.T) {
	if !secretKey("key") || !secretKey("my_password") || secretKey("listen") {
		t.Fatal("secretKey truth table broken")
	}
	if fmtAny(reflectFloatValue()) != 3.5 {
		t.Fatal("fmtAny default branch broken")
	}
}

func TestStaticIfRangeMatch(t *testing.T) {
	s, _ := newTestServer(t, nil)
	meta := putObj(t, s, "repos/o/r/bundles/full/b.bundle", []byte("BODY123"))
	etag := `"` + string(meta.Version) + `"`
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Range", "bytes=0-3")
	req.Header.Set("If-Range", etag)
	rec := httptest.NewRecorder()
	s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 4 {
		t.Fatalf("if-range match = %d %d", rec.Code, rec.Body.Len())
	}
}

func TestWriteFileAtomicError(t *testing.T) {
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "no-such-dir", "f"), []byte("x")); err == nil {
		t.Fatal("unwritable path must error")
	}
}

func ecdsaGenerate() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func reflectFloatValue() reflect.Value {
	return reflect.ValueOf(struct{ V float64 }{V: 3.5}).Field(0)
}
