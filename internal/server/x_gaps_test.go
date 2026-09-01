package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- smart.go remaining branches ---------------------------------------------------

// errEngine wraps fakeEngine to inject generic (non-not-found) errors.
type errEngine struct {
	fakeEngine
	syncErr      error
	repoErr      error
	bundlesErr   error
	placementErr error
}

func (e *errEngine) Placement(ctx context.Context, id git.RepoId) (Placement, error) {
	if e.placementErr != nil {
		return Placement{}, e.placementErr
	}
	return e.fakeEngine.Placement(ctx, id)
}

func (e *errEngine) Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error {
	if e.syncErr != nil {
		return e.syncErr
	}
	return e.fakeEngine.Sync(ctx, id, level)
}

func (e *errEngine) Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error) {
	if e.repoErr != nil {
		return nil, e.repoErr
	}
	return e.fakeEngine.Repo(ctx, id, create, format)
}

func TestUploadPackSyncGenericError(t *testing.T) {
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}}, syncErr: errors.New("disk exploded")}
	s, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.uploadPack(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "disk exploded") {
		t.Fatalf("sync error = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReceivePackRepoGenericError(t *testing.T) {
	ee := &errEngine{fakeEngine: fakeEngine{placement: Placement{Serve: true}, exists: true, noCreate: true},
		repoErr: errors.New("store offline")}
	s, _ := newTestServer(t, func(o *Options) { o.Engine = ee })
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("repo error = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReceivePackTooLarge(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	s.cfg.Server.MaxPushBytes = 8
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", strings.NewReader(strings.Repeat("x", 64)))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large = %d", rec.Code)
	}
}

func TestReceivePackIngestFailureBand2(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	repo, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo
	// Valid command line, garbage pack → ingest failure → band2 "pack rejected".
	var b bytes.Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/x\000report-status\n"))
	b.Write(git.Flush())
	b.Write([]byte("PACK-not-a-real-pack"))
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(b.Bytes()))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pack rejected") {
		t.Fatalf("ingest failure = %d %s", rec.Code, rec.Body.String())
	}
}

func TestGitServiceGates(t *testing.T) {
	// Placement refusal inside gitService (non-git client → 503).
	s, _ := newTestServer(t, nil)
	s.engine.(*fakeEngine).placement = Placement{Serve: false, ServedBy: "host-b"}
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("placement gate = %d", rec.Code)
	}

	// Busy repo → 503 with Retry-After.
	s2, _ := newTestServer(t, nil)
	s2.sem = NewRepoSemaphores(1)
	rel := s2.sem.TryAcquire("o/r")
	if rel == nil {
		t.Fatal("acquire must succeed")
	}
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s2.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy gate = %d", rec.Code)
	}
	rel()

	// Write-denied principal → pkt-ERR shape (git client, carried auth, §4.2).
	s3, _ := newTestServer(t, nil)
	s3.cfg.Server.Auth.Tokens = append(s3.cfg.Server.Auth.Tokens,
		config.StaticToken{Principal: "ro", Token: "ro-token", Write: false, Admin: false})
	s3.authSvc = NewAuthService(&s3.cfg.Server.Auth, s3.Now)
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack?service=git-receive-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer ro-token")
	rec = httptest.NewRecorder()
	s3.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceReceivePack, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("write-denied = %d", rec.Code)
	}
	if _, ok := pktErrOf(rec.Body.String()); !ok {
		t.Fatalf("want pkt ERR, got %q", rec.Body.String())
	}
}

func TestInfoRefsAnonUploadPack401(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon info/refs = %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("WWW-Authenticate missing")
	}
}

// TestInfoRefsAutoCreateAdvert covers the unborn-repo receive-pack advert.
func TestInfoRefsAutoCreateAdvert(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = false // sync → not-found → auto-create advertises empty refs
	fe.noCreate = false
	root := t.TempDir()
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-create advert = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "# service=git-receive-pack") {
		t.Fatalf("advert body = %q", rec.Body.String())
	}
}

// --- receivePack forward decision ---------------------------------------------------

func TestReceivePackForwardDecision(t *testing.T) {
	fe := &fakeEngine{placement: Placement{Serve: false, Maintain: false}, exists: true}
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = fe
		o.Config.WAL.PushBrokerURL = "http://127.0.0.1:1"
		o.Config.WAL.PushBrokerBufferBytes = 1 << 20
	})
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	var b bytes.Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/none\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(b.Bytes()))
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePack(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("forward decision = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReceivePackForwardSpill413(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.WAL.PushBrokerURL = "http://broker.test"
	s.cfg.WAL.PushBrokerBufferBytes = 16
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", strings.NewReader(strings.Repeat("x", 64)))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.receivePackForward(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("spill = %d", rec.Code)
	}
}

// --- bundlesDispatch remaining gates ------------------------------------------------

func TestBundlesDispatchBusyAndErrors(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	id := mustRepoID(t, "o/r")
	// Busy → 503.
	s.sem = NewRepoSemaphores(1)
	rel := s.sem.TryAcquire("o/r")
	if rel == nil {
		t.Fatal("acquire must succeed")
	}
	req := authedGet("http://x/o/r.git/bundles/list")
	rec := httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"list"}, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy = %d", rec.Code)
	}
	rel()
	// filter=blob:none is accepted.
	req = authedGet("http://x/o/r.git/bundles/list?filter=blob:none")
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"list"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob:none = %d", rec.Code)
	}
	// Unauthenticated → 401.
	req = httptest.NewRequest("GET", "http://x/o/r.git/bundles/list", nil)
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"list"}, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon bundles = %d", rec.Code)
	}
}

// --- WalEngine.newLocalPack via a real ingested pack ---------------------------------

func TestWalEngineNewLocalPackFindsIngestedPack(t *testing.T) {
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
	repo := h.Repo()

	// Build a commit in a scratch repo and fetch its objects into the local repo.
	scratch := t.TempDir()
	pack, oid := gitCommitPack(t, scratch)
	cmd := exec.Command("git", "fetch", "-q", scratch, "main:refs/heads/main")
	cmd.Dir = repo.Path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + repo.Path}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed fetch: %v: %s", err, out)
	}

	// The fetched pack objects are loose/alternates; force a pack via
	// pack-objects so the pack dir holds an idx the manifest doesn't know.
	packDir := repo.PackDir()
	packName := filepath.Join(packDir, "gen")
	cmd = exec.Command("git", "pack-objects", "-q", packName)
	cmd.Dir = repo.Path
	cmd.Stdin = strings.NewReader(oid + "\n")
	var nameBuf bytes.Buffer
	cmd.Stdout = &nameBuf
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + repo.Path}
	if err := cmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	sum := strings.TrimSpace(nameBuf.String())
	entries, err := os.ReadDir(packDir)
	if err != nil {
		t.Fatal(err)
	}
	foundIdx := ""
	for _, f := range entries {
		if strings.HasSuffix(f.Name(), ".idx") && strings.Contains(f.Name(), sum) {
			foundIdx = strings.TrimSuffix(f.Name(), ".idx")
			break
		}
	}
	if foundIdx == "" {
		t.Fatalf("idx for %s missing in %v", sum, entries)
	}
	_ = pack

	p := e.newLocalPack(h)
	if p == nil {
		t.Fatal("newLocalPack must find the fresh idx")
	}
	if p.Checksum != foundIdx {
		t.Fatalf("checksum = %q want %q", p.Checksum, foundIdx)
	}
	if p.IdxSize == 0 || p.PackSize == 0 {
		t.Fatalf("sizes = %d %d", p.PackSize, p.IdxSize)
	}

	// And a full publish through the real funnel succeeds with the pack.
	req := &git.PushRequest{Commands: []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: oid, Ref: "refs/heads/main"}}}
	if _, err := e.Publish(ctx, id, req, "alice", wal.ObjectAccess{Local: repo}); err != nil {
		t.Fatalf("publish with pack: %v", err)
	}
}

// --- middleware requireAuth / refresh / compress bypass / flush ----------------------

func TestRequireAuthMatrix(t *testing.T) {
	s, _ := newTestServer(t, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := s.requireAuth(next)
	// Token → next runs.
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("authed = %d", rec.Code)
	}
	// Anonymous → 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon = %d", rec.Code)
	}
	// Invalid token → 401.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid = %d", rec.Code)
	}
	// Aged session → sliding refresh sets a new cookie. Sessions are only
	// honored in oidc mode (§8.3), so flip the mode.
	s.cfg.Server.Auth.Mode = "oidc"
	s.cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	s.cfg.Server.Auth.OAuthClientID = "walhub"
	s.cfg.Server.Auth.OAuthClientSecret = "sekrit"
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	svc := s.authSvc
	sess, err := svc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	half := time.Duration(30 * 24 * time.Hour / 2)
	s.Now = func() time.Time { return time.Now().Add(half) }
	req = httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("session path = %d", rec.Code)
	}
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_session" {
			found = true // re-issued (same-second mint yields the same wire)
		}
	}
	if !found {
		t.Fatal("aged session must be re-issued via maybeRefreshSession")
	}
}

func TestTraceIDOfDirect(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	if TraceIDOf(req) != "" {
		t.Fatal("no trace in context")
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxTraceIDKey{}, "trace-1"))
	if TraceIDOf(req) != "trace-1" {
		t.Fatal("trace value lost")
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxRequestIDKey{}, "req-1"))
	if RequestIDOf(req) != "req-1" {
		t.Fatal("request id value lost")
	}
}

func TestCompressBypassPrecompressedAndFlush(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Precompressed passthrough: the handler sets Content-Encoding itself.
	pre := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("precompressed"))
	})
	req := httptest.NewRequest("GET", "/_ui/x.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.compress(pre).ServeHTTP(rec, req)
	if rec.Body.String() != "precompressed" {
		t.Fatalf("precompressed body = %q", rec.Body.String())
	}
	// Flush path: a streaming handler flushes through both writers.
	flusher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("chunk"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.compress(flusher).ServeHTTP(rec, req)
	if rec.Body.Len() == 0 {
		t.Fatal("flushed body missing")
	}
}

// --- router helpers -----------------------------------------------------------------

func TestRouterErrorHelpers(t *testing.T) {
	rec := httptest.NewRecorder()
	notFound(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("notFound = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	methodNotAllowed(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed ||
		rec.Header().Get("Allow") != "GET, HEAD, POST, PUT, DELETE, OPTIONS" {
		t.Fatalf("methodNotAllowed = %d %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// --- jwk parse branches + JWKS refresh follower + nowOf ------------------------------

func TestJWKParseBranches(t *testing.T) {
	// RSA parses.
	rk := jwk{Kty: "RSA", N: base64.RawURLEncoding.EncodeToString(big.NewInt(12345).Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(65537).Bytes())}
	if err := rk.parse(); err != nil || rk.rsa == nil {
		t.Fatalf("rsa parse = %v", err)
	}
	// EC P-256 parses.
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ek := jwk{Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(priv.X.Bytes()),
		Y: base64.RawURLEncoding.EncodeToString(priv.Y.Bytes())}
	if err := ek.parse(); err != nil || ek.ecdsa == nil {
		t.Fatalf("ec parse = %v", err)
	}
	// Wrong curve and unknown kty fail.
	bad := ek
	bad.Crv = "P-384"
	if err := bad.parse(); err == nil {
		t.Fatal("P-384 must fail")
	}
	oct := jwk{Kty: "oct"}
	if err := oct.parse(); err == nil {
		t.Fatal("oct must fail")
	}
	badb64 := jwk{Kty: "RSA", N: "!!", E: "!!"}
	if err := badb64.parse(); err == nil {
		t.Fatal("bad base64 must fail")
	}
}

func TestJWKSRefreshFollower(t *testing.T) {
	iss := newStubIssuer(t)
	j := NewJWKS(iss.srvURL())
	j.mu.Lock()
	j.sfIn = true // pretend a leader is in flight
	j.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := j.refresh(ctx); err == nil {
		t.Fatal("follower must fail while a leader is in flight")
	}
	j.mu.Lock()
	j.sfIn = false
	j.mu.Unlock()

	// nowOf honors the test clock injected via the context.
	j2 := NewJWKS(iss.srvURL())
	stamp := time.Now().Add(-time.Hour)
	if got := j2.nowOf(context.WithValue(context.Background(), nowKey{}, stamp)); !got.Equal(stamp) {
		t.Fatalf("nowOf = %v", got)
	}
}

// --- Verify: ES256 + claim guards -----------------------------------------------------

func TestJWKSVerifyES256AndClaims(t *testing.T) {
	iss := newStubIssuer(t)
	mux := iss.srv.Config.Handler.(*http.ServeMux)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mux.HandleFunc("/jwks-ec", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(w, map[string]any{"keys": []map[string]string{{
			"kty": "EC", "kid": "ec1", "alg": "ES256", "use": "sig", "crv": "P-256",
			"x": base64.RawURLEncoding.EncodeToString(ecKey.X.Bytes()),
			"y": base64.RawURLEncoding.EncodeToString(ecKey.Y.Bytes()),
		}}})
	})
	j := NewJWKS(iss.srvURL())
	// Seed discovery + keys from the EC endpoint.
	j.mu.Lock()
	j.disc = oidcDiscovery{AuthEndpoint: "x", TokenEndpoint: "x"}
	j.discHas = true
	j.keys = map[string]*jwk{}
	k := &jwk{Kty: "EC", Crv: "P-256", Kid: "ec1", Alg: "ES256",
		X: base64.RawURLEncoding.EncodeToString(ecKey.X.Bytes()),
		Y: base64.RawURLEncoding.EncodeToString(ecKey.Y.Bytes())}
	if err := k.parse(); err != nil {
		t.Fatal(err)
	}
	j.keys["ec1"] = k
	j.ttl = 300 * time.Second
	j.fetched = time.Now()
	j.mu.Unlock()

	a := &config.Auth{
		Issuer: iss.srvURL(), OAuthClientID: "walhub",
		Audiences: []string{"extra"}, AllowedDomains: []string{"example.com"},
	}
	mintES := func(claims map[string]any) string {
		hdr := b64url([]byte(`{"alg":"ES256","kid":"ec1"}`))
		body, _ := jsonMarshal(claims)
		payload := b64url(body)
		sum := sha256.Sum256([]byte(hdr + "." + payload))
		r, s, err := ecdsa.Sign(rand.Reader, ecKey, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		sigRaw := append(leftPad(r, 32), leftPad(s, 32)...)
		return hdr + "." + payload + "." + b64url(sigRaw)
	}
	tv := true
	base := map[string]any{
		"iss": iss.srvURL(), "aud": "walhub",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": "alice@example.com", "email_verified": tv,
	}
	if _, aerr := j.Verify(context.Background(), mintES(base), a, false); aerr != nil {
		t.Fatalf("ES256 verify: %v", aerr)
	}
	// Bad signature → invalid.
	bad := base
	bad["email"] = "mallory@example.com"
	tok := mintES(bad)
	// Tamper the payload → signature no longer matches.
	parts := strings.Split(tok, ".")
	parts[1] = b64url(jsonMustMarshal(map[string]any{
		"iss": iss.srvURL(), "aud": "walhub", "exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(), "email": "eve@example.com", "email_verified": tv}))
	if _, aerr := j.Verify(context.Background(), strings.Join(parts, "."), a, false); aerr == nil {
		t.Fatal("tampered payload must fail")
	}
	// nbf in the future → invalid.
	future := map[string]any{}
	for kk, vv := range base {
		future[kk] = vv
	}
	future["nbf"] = time.Now().Add(time.Hour).Unix()
	if _, aerr := j.Verify(context.Background(), mintES(future), a, false); aerr == nil {
		t.Fatal("future nbf must fail")
	}
	// aud as array.
	arr := map[string]any{}
	for kk, vv := range base {
		arr[kk] = vv
	}
	arr["aud"] = []string{"walhub", "other"}
	if _, aerr := j.Verify(context.Background(), mintES(arr), a, false); aerr != nil {
		t.Fatalf("array aud: %v", aerr)
	}
	// iss with trailing slash matches (§8.4 rule 4 slash strip).
	slash := map[string]any{}
	for kk, vv := range base {
		slash[kk] = vv
	}
	slash["iss"] = iss.srvURL() + "/"
	if _, aerr := j.Verify(context.Background(), mintES(slash), a, false); aerr != nil {
		t.Fatalf("trailing slash iss: %v", aerr)
	}
	// Malformed token → invalid.
	if _, aerr := j.Verify(context.Background(), "not-a-jwt", a, false); aerr == nil {
		t.Fatal("malformed token must fail")
	}
	// key alg mismatch (RSA key advertised with ES256 alg).
	j.mu.Lock()
	j.keys["ec1"].Alg = "RS256"
	j.mu.Unlock()
	if _, aerr := j.Verify(context.Background(), mintES(base), a, false); aerr == nil {
		t.Fatal("key alg mismatch must fail")
	}
	j.mu.Lock()
	j.keys["ec1"].Alg = "ES256"
	j.mu.Unlock()
}

func leftPad(n *big.Int, size int) []byte {
	b := n.Bytes()
	for len(b) < size {
		b = append([]byte{0}, b...)
	}
	return b
}

func jsonWrite(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonMustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// --- authMe error path -----------------------------------------------------------------

func TestAuthMeInvalidToken(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/_auth/me", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.authMe(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me invalid = %d", rec.Code)
	}
}

// --- auth_oidc claimed ticket malformed -------------------------------------------------

func TestAuthClaimedMalformedTicket(t *testing.T) {
	s, _ := oidcServer(t)
	// Validly signed but without the "|" separator → 400.
	ticket := s.signState("no-separator", s.Now())
	req := httptest.NewRequest("GET", "http://x/_auth/claimed?ticket="+ticket, nil)
	rec := httptest.NewRecorder()
	s.authClaimed(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed ticket = %d", rec.Code)
	}
	// Unsigned ticket → 400.
	req = httptest.NewRequest("GET", "http://x/_auth/claimed?ticket=garbage", nil)
	rec = httptest.NewRecorder()
	s.authClaimed(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage ticket = %d", rec.Code)
	}
}

// --- drain RunPhase2 with a real server + semaphore default ------------------------------

func TestRunPhase2ShutdownAndSemDefault(t *testing.T) {
	s, _ := newTestServer(t, nil)
	srv := &http.Server{Handler: http.NewServeMux()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	done := make(chan struct{})
	go func() { s.RunPhase2(srv); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunPhase2 with a real server must return")
	}
	if sem := NewRepoSemaphores(0); sem == nil {
		t.Fatal("zero capacity must fall back to a default")
	}
}

// --- setup API: setupTest gated + setupPut save failure -----------------------------------

func TestSetupTestAndPutFailurePaths(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Unauthenticated → 403 (admin required).
	rec := httptest.NewRecorder()
	s.setupTest(rec, httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader("{}")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gated test = %d", rec.Code)
	}
	// Validation failure → 422 with the key named.
	req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(`{"overrides": {"server.auth.mode": "weird"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validation = %d %s", rec.Code, rec.Body.String())
	}

	// Save failure → 500 (data dir is a file).
	fileDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.DataDir = fileDir
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": {"server.listen": "127.0.0.1:1"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("save failure = %d %s", rec.Code, rec.Body.String())
	}
}

// --- bind_api Owners -----------------------------------------------------------------------

type fakeRegistry struct{}

func (fakeRegistry) Owners(ctx context.Context) ([]string, error) {
	return []string{"alice"}, nil
}
func (fakeRegistry) Repos(ctx context.Context, owner string) ([]string, error) {
	return nil, nil
}
func (fakeRegistry) Exists(ctx context.Context, id git.RepoId) (bool, error) {
	return false, nil
}
func (fakeRegistry) Create(ctx context.Context, id git.RepoId, format git.ObjectFormat) error {
	return nil
}
func (fakeRegistry) Delete(ctx context.Context, id git.RepoId) error { return nil }

func TestAPIProviderOwners(t *testing.T) {
	env := api.NewEnv(store.NewMemory(), fakeRegistry{}, config.Defaults(), nil, "v", "h")
	p := NewAPIProvider(env)
	owners, err := p.Owners(httptest.NewRequest("GET", "/x", nil))
	if err != nil || len(owners) != 1 || owners[0] != "alice" {
		t.Fatalf("owners = %v %v", owners, err)
	}
	rec := httptest.NewRecorder()
	p.Serve(rec, httptest.NewRequest("GET", "/api/v1/repos", nil))
	if rec.Code == 0 {
		t.Fatal("mux must answer")
	}
}
