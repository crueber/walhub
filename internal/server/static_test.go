package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- §5 static contract ---------------------------------------------------

func putObj(t *testing.T, s *Server, key string, body []byte) store.ObjectMeta {
	t.Helper()
	meta, err := s.store.Put(context.Background(), key,
		store.PutBody{Bytes: body},
		store.PutOptions{ContentType: "application/x-git-bundle", Immutable: true})
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestStaticContract(t *testing.T) {
	s, _ := newTestServer(t, nil)
	body := []byte(strings.Repeat("x", 100))
	meta := putObj(t, s, "repos/o/r/bundles/full/b.bundle", body)
	etag := `"` + string(meta.Version) + `"`

	t.Run("200 immutable + etag + accept-ranges", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.serveStatic(rec, httptest.NewRequest("GET", "/x", nil), "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
		if rec.Code != http.StatusOK || rec.Body.Len() != 100 {
			t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("ETag") != etag {
			t.Fatalf("etag = %q", rec.Header().Get("ETag"))
		}
		if rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("cc = %q", rec.Header().Get("Cache-Control"))
		}
		if rec.Header().Get("Accept-Ranges") != "bytes" {
			t.Fatal("accept-ranges missing")
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("nosniff missing")
		}
	})
	t.Run("304 on If-None-Match", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("If-None-Match", etag)
		rec := httptest.NewRecorder()
		s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
		}
	})
	t.Run("206 exact byte range", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Range", "bytes=10-19")
		rec := httptest.NewRecorder()
		s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
		if rec.Code != http.StatusPartialContent || rec.Body.Len() != 10 {
			t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
		}
		if cr := rec.Header().Get("Content-Range"); cr != "bytes 10-19/100" {
			t.Fatalf("content-range = %q", cr)
		}
	})
	t.Run("416 unsatisfiable", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Range", "bytes=200-300")
		rec := httptest.NewRecorder()
		s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d", rec.Code)
		}
		if cr := rec.Header().Get("Content-Range"); cr != "bytes */100" {
			t.Fatalf("content-range = %q", cr)
		}
	})
	t.Run("If-Range mismatch serves full 200", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Range", "bytes=10-19")
		req.Header.Set("If-Range", `"v999"`)
		rec := httptest.NewRecorder()
		s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
		if rec.Code != http.StatusOK || rec.Body.Len() != 100 {
			t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
		}
	})
	t.Run("HEAD headers only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.serveStatic(rec, httptest.NewRequest("HEAD", "/x", nil), "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("Content-Length") != "100" {
			t.Fatalf("cl = %q", rec.Header().Get("Content-Length"))
		}
	})
	t.Run("missing object 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.serveStatic(rec, httptest.NewRequest("GET", "/x", nil), "repos/o/r/none", "application/x-git-bundle")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

// accelStore wraps a store and always reports an accel target (test §5 edge contract).
type accelStore struct{ store.ObjectStore }

func (a accelStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return &store.AccelTarget{URL: "https://bucket.test/" + key, Authorization: "Bearer sig"}, nil
}

func TestAccelOffload(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.store = accelStore{s.store}
	s.cfg.Server.AccelRedirect = true
	putObj(t, s, "repos/o/r/bundles/full/b.bundle", []byte("B"))
	// Loopback peer + accel on → 200 empty + edge headers.
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status=%d len=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("X-Accel-Redirect") != "/_store/" {
		t.Fatalf("accel redirect = %q", rec.Header().Get("X-Accel-Redirect"))
	}
	if rec.Header().Get("X-Walgit-Store-Url") == "" || rec.Header().Get("X-Walgit-Store-Key") == "" {
		t.Fatal("store url/key missing")
	}
	// Non-loopback peer → streams bytes.
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	rec = httptest.NewRecorder()
	s.serveStatic(rec, req, "repos/o/r/bundles/full/b.bundle", "application/x-git-bundle")
	if rec.Body.Len() != 1 {
		t.Fatalf("direct hit must stream bytes, got len=%d", rec.Body.Len())
	}
}

// --- §6 LFS batch table -----------------------------------------------------

func TestLFSBatchTable(t *testing.T) {
	s, _ := newTestServer(t, nil)
	putObj(t, s, lfsKey(mustRepoID(t, "o/r"), "aa"), []byte("LFSBYTES"))

	call := func(op string, oids []string) (int, []lfsObject) {
		req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch", nil)
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		req.Header.Set("Authorization", "Bearer tok123")
		objs := []map[string]any{}
		for _, oid := range oids {
			objs = append(objs, map[string]any{"oid": oid, "size": 8})
		}
		b, _ := json.Marshal(map[string]any{"operation": op, "objects": objs})
		req.Body = http.NoBody
		req = httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		s.lfsBatch(rec, req, mustRepoID(t, "o/r"))
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var out struct {
			Objects []lfsObject `json:"objects"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Objects
	}

	t.Run("upload present → no actions", func(t *testing.T) {
		_, objs := call("upload", []string{"aa"})
		if objs[0].Actions != nil && len(objs[0].Actions) != 0 {
			t.Fatalf("present upload must have no actions, got %v", objs[0].Actions)
		}
	})
	t.Run("download present → our href", func(t *testing.T) {
		_, objs := call("download", []string{"aa"})
		if a, ok := objs[0].Actions["download"]; !ok || !strings.Contains(a.Href, "/info/lfs/objects/aa") {
			t.Fatalf("download href missing: %v", objs[0].Actions)
		}
	})
	t.Run("upload missing → upload + verify", func(t *testing.T) {
		_, objs := call("upload", []string{"bb"})
		if _, ok := objs[0].Actions["upload"]; !ok {
			t.Fatalf("upload action missing: %v", objs[0].Actions)
		}
		if _, ok := objs[0].Actions["verify"]; !ok {
			t.Fatalf("verify action missing: %v", objs[0].Actions)
		}
	})
	t.Run("download nowhere → 404 entry", func(t *testing.T) {
		_, objs := call("download", []string{"cc"})
		if objs[0].Err == nil || objs[0].Err.Code != 404 {
			t.Fatalf("want 404 entry, got %+v", objs[0].Err)
		}
	})
	t.Run("unauthenticated → 401", func(t *testing.T) {
		code, _ := call("download", []string{"aa"})
		if code != http.StatusOK {
			// call() reports non-200; make the unauthenticated call separately
		}
		req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch", strings.NewReader(`{"operation":"download","objects":[]}`))
		rec := httptest.NewRecorder()
		s.lfsBatch(rec, req, mustRepoID(t, "o/r"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestLFSPutSizeCap(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.LFS.MaxObjectBytes = config.ByteSize(8)
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte("too big for the cap!!")))
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid[:64],
		strings.NewReader("1234567890123456"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, mustRepoID(t, "o/r"), oid[:64])
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// --- §8 auth ----------------------------------------------------------------

func TestCredentialResolutionOrder(t *testing.T) {
	// 1. X-Walgit-Authorization wins.
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Walgit-Authorization", "Bearer wal-token")
	r.Header.Set("Authorization", "Bearer plain-token")
	cred, from := resolveCredential(r)
	if cred != "Bearer wal-token" || from != credWalgitHeader {
		t.Fatalf("got %q %v", cred, from)
	}
	// 2. client-authorization capability → the client sent none.
	r = httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer edge-hop-token")
	r.Header.Set("X-Walgit-Capabilities", "client-authorization")
	cred, from = resolveCredential(r)
	if cred != "" || from != credEdgeClientAuth {
		t.Fatalf("got %q %v", cred, from)
	}
	// 3. plain Authorization.
	r = httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Basic dXNlcjp0b2szNQ==") // user:tok35
	cred, from = resolveCredential(r)
	if cred != "Basic dXNlcjp0b2szNQ==" || from != credAuthorization {
		t.Fatalf("got %q %v", cred, from)
	}
	tok, basic, err := parseCredential(cred)
	if err != nil || !basic || tok != "tok35" {
		t.Fatalf("basic parse = %q %v %v", tok, basic, err)
	}
}

func TestTokenModeDecisionTree(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Static match → principal with flags from config.
	p, aerr := s.authSvc.Authenticate(bearerReq("tok123"), s.cfg)
	if aerr != nil || p.Name != "alice" || !p.Write || !p.Admin {
		t.Fatalf("p=%+v aerr=%v", p, aerr)
	}
	// Miss → Invalid → 401 mapping.
	_, aerr = s.authSvc.Authenticate(bearerReq("wrong"), s.cfg)
	if aerr == nil || aerr.Kind != auth.ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", aerr)
	}
	// None → anonymous.
	p, aerr = s.authSvc.Authenticate(httptest.NewRequest("GET", "/x", nil), s.cfg)
	if aerr != nil || !p.Anonymous {
		t.Fatalf("p=%+v aerr=%v", p, aerr)
	}
	// mode none → everyone is anon with write+admin (§8.1; the frozen
	// Principal.None carries Anonymous=false by design).
	s.cfg.Server.Auth.Mode = "none"
	p, _ = s.authSvc.Authenticate(httptest.NewRequest("GET", "/x", nil), s.cfg)
	if !p.Write || !p.Admin || p.Name != "anon" {
		t.Fatalf("mode none principal = %+v", p)
	}
}

func TestWgtTokenLifecycle(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	tok, err := s.authSvc.MintToken("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Wire, "wgt_") {
		t.Fatalf("access token must carry wgt_ prefix: %q", tok.Wire)
	}
	p, aerr := s.authSvc.wgtPrincipal(tok.Wire)
	if aerr != nil || p.Name != "alice@example.com" || !p.Write {
		t.Fatalf("p=%+v aerr=%v", p, aerr)
	}
	// Tampered payload → invalid.
	bad := tok.Wire[:len(tok.Wire)-2] + "xx"
	if _, aerr := s.authSvc.VerifyToken(bad); aerr == nil || aerr.Kind != auth.ErrInvalid {
		t.Fatalf("tampered token must be invalid, got %v", aerr)
	}
	// Expired → invalid (drives the real-401 rule).
	expired, _ := s.authSvc.mint(tokenKind, "alice@example.com", tokenPrefix)
	s.authSvc.now = func() time.Time { return time.Now().Add(200 * 24 * time.Hour) }
	if _, aerr := s.authSvc.VerifyToken(expired.Wire); aerr == nil {
		t.Fatal("expired token must be invalid")
	}
}

func TestOIDCTreeBasicAuthNoIDTokens(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	// Basic + static token → ok.
	req := httptest.NewRequest("GET", "/x", nil)
	req.SetBasicAuth("user", "tok123")
	p, aerr := s.authSvc.Authenticate(req, s.cfg)
	if aerr != nil || p.Name != "alice" {
		t.Fatalf("p=%+v aerr=%v", p, aerr)
	}
	// Basic + arbitrary bearer-shaped secret → invalid (no ID tokens over Basic).
	req = httptest.NewRequest("GET", "/x", nil)
	req.SetBasicAuth("user", "not-a-static-token")
	if _, aerr := s.authSvc.Authenticate(req, s.cfg); aerr == nil {
		t.Fatal("basic with unknown secret must be invalid")
	}
	// Session cookie path.
	sess, _ := s.authSvc.MintSession("alice@example.com")
	req = httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	p, aerr = s.authSvc.Authenticate(req, s.cfg)
	if aerr != nil || p.Name != "alice@example.com" {
		t.Fatalf("session principal = %+v aerr = %v", p, aerr)
	}
}

// TestJWKSVerify spins a real JWKS issuer and verifies RS256 ID tokens
// end-to-end through the hand-rolled verifier (§8.4).
func TestJWKSVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var issuerURL atomicStr
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 "https://issuer.test",
				"jwks_uri":               issuerURL.get() + "/jwks",
				"authorization_endpoint": "https://issuer.test/auth",
				"token_endpoint":         "https://issuer.test/token",
			})
		case "/jwks":
			w.Header().Set("Cache-Control", "max-age=300")
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	issuerURL.set(issuer.URL)
	defer issuer.Close()

	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	// cfg issuer is the LOGICAL issuer (the token iss); the JWKS discovery
	// hits the test server URL. §8.4 rule 4 also accepts the bare host.
	s.cfg.Server.Auth.Issuer = "https://issuer.test"
	s.cfg.Server.Auth.OAuthClientID = "walhub"
	s.cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	s.authSvc.jwks = NewJWKS(issuer.URL)

	mint := func(claims map[string]any) string {
		hdr := b64url([]byte(`{"alg":"RS256","kid":"k1"}`))
		body, _ := json.Marshal(claims)
		payload := b64url(body)
		sum := sha256.Sum256([]byte(hdr + "." + payload))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		return hdr + "." + payload + "." + b64url(sig)
	}
	emailVerified := true
	tok := mint(map[string]any{
		"iss": "https://issuer.test", "aud": "walhub",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": "alice@example.com", "email_verified": true,
	})
	claims, aerr := s.authSvc.jwks.Verify(context.Background(), tok, &s.cfg.Server.Auth, false)
	if aerr != nil {
		t.Fatalf("verify: %v", aerr)
	}
	if claims.Email != "alice@example.com" {
		t.Fatalf("claims = %+v", claims)
	}
	// Audience mismatch.
	bad := mint(map[string]any{"iss": "https://issuer.test", "aud": "other",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "alice@example.com", "email_verified": true})
	if _, aerr := s.authSvc.jwks.Verify(context.Background(), bad, &s.cfg.Server.Auth, false); aerr == nil {
		t.Fatal("audience mismatch must fail")
	}
	// Unverified email.
	bad = mint(map[string]any{"iss": "https://issuer.test", "aud": "walhub",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "alice@example.com", "email_verified": false})
	if _, aerr := s.authSvc.jwks.Verify(context.Background(), bad, &s.cfg.Server.Auth, false); aerr == nil {
		t.Fatal("unverified email must fail")
	}
	// Wrong issuer.
	bad = mint(map[string]any{"iss": "https://evil.test", "aud": "walhub",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "alice@example.com", "email_verified": true})
	if _, aerr := s.authSvc.jwks.Verify(context.Background(), bad, &s.cfg.Server.Auth, false); aerr == nil {
		t.Fatal("issuer mismatch must fail")
	}
	// Email policy: not-allowed domain → 403.
	bad = mint(map[string]any{"iss": "https://issuer.test", "aud": "walhub",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "mallory@evil.test", "email_verified": true})
	_, aerr = s.authSvc.verifyIDToken(context.Background(), bad, false)
	if aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Fatalf("want 403 for disallowed domain, got %v", aerr)
	}
	_ = emailVerified
}

// atomicStr is a tiny set-once string for handler closures.
type atomicStr struct {
	mu sync.Mutex
	v  string
}

func (a *atomicStr) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }

func (a *atomicStr) get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func bearerReq(tok string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}
