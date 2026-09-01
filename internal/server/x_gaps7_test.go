package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

func TestNewAuthServiceVariants(t *testing.T) {
	// nil clock → time.Now; TokenEnv resolution; empty-resolved dropped.
	t.Setenv("WALHUB_TEST_TOKEN_A", "env-token")
	svc := NewAuthService(&config.Auth{
		Mode: "token",
		Tokens: []config.StaticToken{
			{Principal: "env", TokenEnv: "WALHUB_TEST_TOKEN_A", Write: true},
			{Principal: "dropped", TokenEnv: "WALHUB_TEST_TOKEN_MISSING"},
			{Principal: "empty"},
		},
	}, nil)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer env-token")
	p, aerr := svc.Authenticate(req, &config.Config{Server: config.Server{Auth: config.Auth{Mode: "token"}}})
	if aerr != nil || p.Name != "env" || !p.Write {
		t.Fatalf("env token principal = %+v %v", p, aerr)
	}
	// The missing-env and empty tokens must not authenticate anything.
	req2 := httptest.NewRequest("GET", "/x", nil)
	req2.Header.Set("Authorization", "Bearer dropped")
	if _, aerr := svc.Authenticate(req2, svc.cfg2()); aerr == nil {
		t.Fatal("dropped token must not authenticate")
	}
}

func (s *AuthService) cfg2() *config.Config {
	return &config.Config{Server: config.Server{Auth: config.Auth{
		Mode: "token", Tokens: []config.StaticToken{
			{Principal: "env", TokenEnv: "WALHUB_TEST_TOKEN_A"},
			{Principal: "dropped", TokenEnv: "WALHUB_TEST_TOKEN_MISSING"},
			{Principal: "empty"},
		},
		SessionSecret: "0123456789abcdef0123456789abcdef"}}}
}

func TestParseCredentialEdges(t *testing.T) {
	if tok, basic, err := parseCredential(""); err != nil || tok != "" || basic {
		t.Fatalf("empty credential = %q %v %v", tok, basic, err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Digest abc")
	s := NewAuthService(&config.Auth{Mode: "token", SessionSecret: "0123456789abcdef0123456789abcdef"}, nil)
	if _, aerr := s.Authenticate(req, s.cfg2()); aerr == nil || aerr.Kind != auth.ErrInvalid {
		t.Fatalf("unsupported scheme = %v", aerr)
	}
}

func TestOIDCModeCredentialVariants(t *testing.T) {
	cfg := &config.Auth{
		Mode: "oidc", Issuer: "https://issuer.test",
		SessionSecret:  "0123456789abcdef0123456789abcdef",
		AccessTokenTTL: config.Duration(24 * time.Hour),
		SessionTTL:     config.Duration(24 * time.Hour),
		Tokens:         []config.StaticToken{{Principal: "alice", Token: "static-tok", Write: true}},
		AllowedDomains: []string{"example.com"},
	}
	svc := NewAuthService(cfg, nil)

	// Static token in oidc mode.
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer static-tok")
	p, aerr := svc.Authenticate(req, oidcConfig(cfg))
	if aerr != nil || p.Name != "alice" {
		t.Fatalf("oidc static = %+v %v", p, aerr)
	}
	// Garbage scheme → invalid credentials.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Digest x")
	if _, aerr := svc.Authenticate(req, oidcConfig(cfg)); aerr == nil {
		t.Fatal("garbage scheme must fail")
	}
	// wgt_ token for a disallowed email → Forbidden.
	tok, err := svc.MintToken("mallory@evil.test")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Wire)
	if _, aerr := svc.Authenticate(req, oidcConfig(cfg)); aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Fatalf("disallowed wgt = %v", aerr)
	}
	// Raw garbage ID token → invalid.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	if _, aerr := svc.Authenticate(req, oidcConfig(cfg)); aerr == nil {
		t.Fatal("garbage id token must fail")
	}
	// Session cookie for a disallowed email → Forbidden.
	sess, _ := svc.MintSession("mallory@evil.test")
	req = httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	if _, aerr := svc.Authenticate(req, oidcConfig(cfg)); aerr == nil {
		t.Fatal("disallowed session must fail")
	}
}

func oidcConfig(a *config.Auth) *config.Config {
	return &config.Config{Server: config.Server{Auth: *a}}
}

func TestPrincipalFromEmailPolicies(t *testing.T) {
	cfg := &config.Auth{
		AllowedDomains: []string{"example.com", "admins.test"},
		AllowedEmails:  []string{"special@other.test"},
		WriteDomains:   []string{"example.com", "writers.test"},
		AdminEmails:    []string{"boss@example.com"},
		AdminDomains:   []string{"admins.test"},
	}
	svc := NewAuthService(cfg, nil)
	// allowed_emails grants access even outside allowed domains.
	p, aerr := svc.principalFromEmail("special@other.test")
	if aerr != nil || p.Write {
		t.Fatalf("allowed email = %+v %v", p, aerr)
	}
	// write domain grants write.
	p, _ = svc.principalFromEmail("alice@example.com")
	if !p.Write {
		t.Fatal("write domain must grant write")
	}
	// admin email → admin.
	p, _ = svc.principalFromEmail("boss@example.com")
	if !p.Admin {
		t.Fatal("admin email must grant admin")
	}
	// admin domain → admin.
	p, _ = svc.principalFromEmail("root@admins.test")
	if !p.Admin {
		t.Fatalf("admin domain = %+v", p)
	}
}

func TestVerifyStateBranches(t *testing.T) {
	s, _ := oidcServer(t)
	// Expired state.
	expired := s.signState("/x", s.Now().Add(-700*time.Second))
	if _, ok := s.verifyState(expired); ok {
		t.Fatal("expired state must fail")
	}
	// Corrupted MAC.
	good := s.signState("/x", s.Now())
	parts := strings.Split(good, ".")
	bad := parts[0] + ".AAAA"
	if _, ok := s.verifyState(bad); ok {
		t.Fatal("bad mac must fail")
	}
	// Payload that isn't 3 lines.
	payload := b64url([]byte("only-one-line"))
	mac := b64url(hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte("only-one-line")))
	if _, ok := s.verifyState(payload + "." + mac); ok {
		t.Fatal("malformed payload must fail")
	}
	// verifyStateTicket with 3 dot-parts → fail.
	if _, ok := s.verifyStateTicket("a.b.c"); ok {
		t.Fatal("3-part ticket must fail")
	}
	// Ticket carrying "name|wire" with an unparseable wire → still accepted
	// (cookie set with garbage wire)? The claimed handler splits on "|".
	ticket := s.signState("alice@example.com|not-a-real-token", s.Now())
	rec := httptest.NewRecorder()
	s.authClaimed(rec, httptest.NewRequest("GET", "/_auth/claimed?ticket="+ticket, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("claimed with bad wire = %d", rec.Code)
	}
}

func TestWalRegistryStoreError(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := failingStore{ObjectStore: store.NewMemory(), getErr: errors.New("get boom")}
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	// Open surfaces the store error (not not-found) through Sync.
	err := e.Sync(ctx, mustRepoID(t, "o/r"), wal.LevelRefs)
	if err == nil || isNotFound(err) {
		t.Fatalf("store error = %v", err)
	}
	// Bundles: GetBytes failure → empty list (never an error).
	bl, berr := e.Bundles(ctx, mustRepoID(t, "o/r"), "")
	if berr != nil || len(bl.Fulls) != 0 {
		t.Fatalf("bundles store error = %v %v", bl, berr)
	}
	// Garbage bundle-list bytes → empty list.
	id := mustRepoID(t, "o/r")
	_ = id
}

func TestWalEngineBundlesGarbageList(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")
	if _, err := st.Put(ctx, id.StorePrefix()+store.BundleList,
		store.PutBody{Bytes: []byte("not-a-proto")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	bl, err := e.Bundles(ctx, id, "")
	if err != nil || len(bl.Fulls) != 0 {
		t.Fatalf("garbage list = %+v %v", bl, err)
	}
}

func TestWalEngineNewLocalPackMissingPackFile(t *testing.T) {
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
	// A stray idx without its pack: candidate found, PackPath cleared.
	idx := filepath.Join(packDir, "orphan.idx")
	if err := os.WriteFile(idx, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := e.newLocalPack(h)
	if p == nil || p.Checksum != "orphan" {
		t.Fatalf("pack = %+v", p)
	}
	if p.PackPath != "" || p.IdxSize == 0 {
		t.Fatalf("missing pack file must clear PackPath: %+v", p)
	}
}

func TestWalEnginePublishPushOptionsAndAtomic(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")
	if _, err := e.Repo(ctx, id, true, git.Sha1); err != nil {
		t.Fatal(err)
	}
	req := &git.PushRequest{
		Caps:        []string{"push-options", "atomic", "report-status"},
		PushOptions: []string{"option-a"},
	}
	if _, err := e.Publish(ctx, id, req, "alice", wal.ObjectAccess{}); err != nil {
		t.Fatalf("publish with options: %v", err)
	}
}

func TestRunPhase2DeadlineFires(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.DrainTimeout = config.Duration(30 * time.Millisecond)
	s.drain.Begin2()
	s.Inflight().n.Store(5) // never drains → deadline must fire
	done := make(chan struct{})
	go func() { s.RunPhase2(nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline must break the drain wait")
	}
}

func TestMiddlewareSmallBranches(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if s.middlewareByName("nope") != nil {
		t.Fatal("unknown middleware must resolve nil")
	}
	// browserLooks via Sec-Fetch-Dest.
	req := httptest.NewRequest("GET", "http://localhost/x", nil)
	req.Header.Set("Sec-Fetch-Dest", "document")
	if !browserLooks(req) {
		t.Fatal("document fetch dest must look like a browser")
	}
	// canonicalBrowserHost skips walgit.-prefixed hosts.
	passed := false
	h := s.canonicalBrowserHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passed = true
	}))
	req = httptest.NewRequest("GET", "http://walgit.localhost/x", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !passed {
		t.Fatal("walgit.localhost must skip the canonical redirect")
	}
	// recoverPanic after a partial write writes the tail instead of 500.
	w := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("panic after write = %d %q", rec.Code, rec.Body.String())
	}
	// isCanonicalOrigin: the configured public_url origin is allowed.
	s.cfg.Server.PublicURL = "https://app.example.com"
	if !s.isCanonicalOrigin("https://app.example.com/") {
		t.Fatal("public_url origin must be allowed")
	}
	// refreshSession ignores a corrupt cookie.
	sess := "garbage.cookie"
	req2 := httptest.NewRequest("GET", "/x", nil)
	req2.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess})
	rec2 := httptest.NewRecorder()
	s.refreshSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec2, req2)
	if rec2.Header().Get("Set-Cookie") != "" {
		t.Fatal("corrupt session cookie must not be refreshed")
	}
}

func TestTLSServerConfigMissingCert(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir() // no certs generated
	if _, err := s.TLSServerConfig(); err == nil {
		t.Fatal("missing certs must error")
	}
}

func TestParseRangeAndServerDefaults(t *testing.T) {
	if _, _, ok := parseRange("bytes=1-2,3-4", 10); ok {
		t.Fatal("multi-range must serve full")
	}
	// New with nil Config/Log/Now/Kind fills defaults.
	s := New(Options{Store: newFakeStore(), Engine: &fakeEngine{}})
	if s.Config() == nil || s.Version() != "dev" {
		t.Fatal("default wiring broken")
	}
	if got := s.serverHeaderValue(); !strings.Contains(got, "walgit/dev") {
		t.Fatalf("server header = %q", got)
	}
}
