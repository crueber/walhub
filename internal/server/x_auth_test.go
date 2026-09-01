package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// stubIssuer is a full OIDC issuer: discovery + JWKS + token endpoint.
type stubIssuer struct {
	srv        *httptest.Server
	rsaKey     *rsa.PrivateKey
	idTokens   chan string // token endpoint hands out one id_token per request
	failExpr   bool
	discStatus int    // when non-zero, discovery answers with this status
	discBody   string // when set, discovery answers with this raw body
	jwksStatus int    // when non-zero, jwks answers with this status
	jwksBody   string // when set, jwks answers with this raw body
	noJWKSURI  bool   // discovery omits jwks_uri
}

func newStubIssuer(t *testing.T) *stubIssuer {
	t.Helper()
	iss := &stubIssuer{rsaKey: mustRSA(t), idTokens: make(chan string, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if iss.discStatus != 0 {
			w.WriteHeader(iss.discStatus)
			return
		}
		if iss.discBody != "" {
			_, _ = w.Write([]byte(iss.discBody))
			return
		}
		jwks := iss.srvURL() + "/jwks"
		if iss.noJWKSURI {
			jwks = ""
		}
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 iss.srvURL(),
			"jwks_uri":               jwks,
			"authorization_endpoint": iss.srvURL() + "/auth",
			"token_endpoint":         iss.srvURL() + "/token",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if iss.jwksStatus != 0 {
			w.WriteHeader(iss.jwksStatus)
			return
		}
		if iss.jwksBody != "" {
			_, _ = w.Write([]byte(iss.jwksBody))
			return
		}
		w.Header().Set("Cache-Control", "max-age=300")
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(iss.rsaKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(iss.rsaKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if iss.failExpr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		select {
		case tok := <-iss.idTokens:
			json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	iss.srv = httptest.NewServer(mux)
	t.Cleanup(iss.srv.Close)
	return iss
}

func (i *stubIssuer) srvURL() string {
	if i.srv == nil {
		return "https://pending"
	}
	return i.srv.URL
}

func (i *stubIssuer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = i.srvURL()
	}
	hdr := b64url([]byte(`{"alg":"RS256","kid":"k1"}`))
	body, _ := json.Marshal(claims)
	payload := b64url(body)
	sum := sha256.Sum256([]byte(hdr + "." + payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.rsaKey, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return hdr + "." + payload + "." + b64url(sig)
}

func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// oidcFull wires an oidc-mode server against the stub issuer.
func oidcFull(t *testing.T) (*Server, http.Handler, *stubIssuer) {
	t.Helper()
	iss := newStubIssuer(t)
	s, h := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	s.cfg.Server.Auth.Issuer = iss.srvURL()
	s.cfg.Server.Auth.OAuthClientID = "walhub"
	s.cfg.Server.Auth.OAuthClientSecret = "sekrit"
	s.cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	svc := NewAuthService(&s.cfg.Server.Auth, s.Now)
	s.authSvc = svc
	return s, h, iss
}

func TestAuthMeAndCheck(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// /_auth/me anonymous → anonymous principal JSON.
	rec := httptest.NewRecorder()
	s.authMe(rec, httptest.NewRequest("GET", "/_auth/me", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"anonymous":true`) {
		t.Fatalf("me anon = %d %s", rec.Code, rec.Body.String())
	}
	// Authenticated.
	req := httptest.NewRequest("GET", "/_auth/me", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.authMe(rec, req)
	if !strings.Contains(rec.Body.String(), `"principal":"alice"`) || !strings.Contains(rec.Body.String(), `"write":true`) {
		t.Fatalf("me authed = %s", rec.Body.String())
	}
	// /_auth/check anonymous + no anonymous read → 401.
	rec = httptest.NewRecorder()
	s.authCheck(rec, httptest.NewRequest("GET", "/_auth/check", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("check anon = %d", rec.Code)
	}
	// Authenticated → 204 + identity headers.
	req = httptest.NewRequest("GET", "/_auth/check", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.authCheck(rec, req)
	if rec.Code != http.StatusNoContent ||
		rec.Header().Get("X-Walgit-Principal") != "alice" ||
		rec.Header().Get("X-Walgit-Write") != "1" ||
		rec.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("check authed = %d %v", rec.Code, rec.Header())
	}
	// Auth error → mapped.
	s.cfg.Server.Auth.AnonymousRead = true
	rec = httptest.NewRecorder()
	s.authCheck(rec, httptest.NewRequest("GET", "/_auth/check", nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Walgit-Write") != "0" {
		t.Fatalf("check anon-read = %d", rec.Code)
	}
}

func TestRequireAdminAndIdentityForward(t *testing.T) {
	if aerr := requireAdmin(principalAlice); aerr != nil {
		t.Fatalf("admin check = %v", aerr)
	}
	if aerr := requireAdmin(auth.Principal{Name: "ro"}); aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Fatalf("non-admin = %v", aerr)
	}

	// identityForward: only honored for trusted forwarders with write.
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.TrustedForwarders = []string{"edge"}
	p := auth.Principal{Name: "edge", Write: true}
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set("X-Walgit-Principal", "alice@example.com")
	got := s.authSvc.identityForward(req, p)
	if got.Name != "alice@example.com" {
		t.Fatalf("forwarded principal = %+v", got)
	}
	// Untrusted forwarder → caller kept.
	got = s.authSvc.identityForward(req, auth.Principal{Name: "stranger", Write: true})
	if got.Name != "stranger" {
		t.Fatalf("untrusted forward = %+v", got)
	}
	// Read-only caller → kept.
	got = s.authSvc.identityForward(req, auth.Principal{Name: "edge"})
	if got.Name != "edge" {
		t.Fatalf("read-only forward = %+v", got)
	}
	if principalEmail("bob") != "" || principalEmail("b@x") == "" {
		t.Fatal("principalEmail truth table broken")
	}
}

func TestVerifyTokenMalformedVariants(t *testing.T) {
	s, _ := newTestServer(t, nil)
	mk := func(payload string) string {
		mac := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte(payload))
		return b64url([]byte(payload)) + "." + b64url(mac)
	}
	cases := map[string]string{
		"too many parts":   mk("session\n1\n1\na\nextra"),
		"bad kind":         mk("cookie\n1\n1\na"),
		"bad exp":          mk("session\nNaN\n1\na"),
		"bad iat":          mk("session\n9999999999\nNaN\na"),
		"no secret":        "anything",
		"wrong part count": "onlyonepart",
	}
	for name, wire := range cases {
		if _, aerr := s.authSvc.VerifyToken(wire); aerr == nil {
			t.Fatalf("%s must fail", name)
		}
	}
	// Valid session token still verifies.
	sess, err := s.authSvc.MintSession("a@b")
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := s.authSvc.VerifyToken(sess.Wire); aerr != nil {
		t.Fatalf("valid session = %v", aerr)
	}
	// parseUnix via a bad token; parseCredential garbage.
	if _, _, err := parseCredential("Digest abc"); err == nil {
		t.Fatal("unsupported credential scheme must fail")
	}
	if _, _, err := parseCredential("Basic !!!not-base64!!!"); err == nil {
		t.Fatal("malformed basic must fail")
	}
	if _, basic, err := parseCredential("Basic " + base64.StdEncoding.EncodeToString([]byte("noseparator"))); err != nil || !basic {
		t.Fatalf("basic without colon = %v", err)
	}
}

// TestJWKSFetchAndRefresh covers discovery + key fetch + unknown-kid refresh
// through a live issuer.
func TestJWKSFetchAndRefresh(t *testing.T) {
	iss := newStubIssuer(t)
	j := NewJWKS(iss.srvURL())
	// Unknown kid → inline refresh fetches discovery + jwks, then invalid kid.
	if _, aerr := j.Get(context.Background(), "missing"); aerr == nil || aerr.Kind != auth.ErrInvalid {
		t.Fatalf("unknown kid after refresh = %v", aerr)
	}
	// Known kid now resolves.
	k, aerr := j.Get(context.Background(), "k1")
	if aerr != nil || k == nil || k.rsa == nil {
		t.Fatalf("k1 = %v %v", k, aerr)
	}
	// maxAgeOf truth table.
	if maxAgeOf("max-age=42") != 42*time.Second || maxAgeOf("no-store") != 0 || maxAgeOf("max-age=-1") != 0 {
		t.Fatal("maxAgeOf truth table broken")
	}
	// bareHost.
	if bareHost("https://issuer.test/") != "issuer.test" || bareHost("plain") != "plain" {
		t.Fatal("bareHost truth table broken")
	}
	// anyOf / containsString.
	if !anyOf([]string{"a", "b"}, []string{"z", "b"}) || containsString([]string{"a"}, "b") {
		t.Fatal("aud helpers truth table broken")
	}
	// SetNow overrides the fetch clock (test hook).
	j.SetNow(time.Now())
}

// TestOIDCCallbackFullFlow drives the browser callback end-to-end: state,
// code exchange at the stub issuer, JWKS verification, session cookie.
func TestOIDCCallbackFullFlow(t *testing.T) {
	s, h, iss := oidcFull(t)
	// Login → capture state.
	req := httptest.NewRequest("GET", "http://localhost:8080/_auth/login?next=/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	// The redirect_uri uses localhost for loopback hosts (§8.6).
	q := loc.Query()
	if ru := q.Get("redirect_uri"); !strings.HasPrefix(ru, "http://localhost:8080") {
		t.Fatalf("redirect_uri = %q", ru)
	}
	// The issuer hands out a valid ID token for alice.
	tv := true
	iss.idTokens <- iss.mint(t, map[string]any{
		"aud": "walhub", "exp": time.Now().Add(time.Hour).Unix(),
		"email": "alice@example.com", "email_verified": tv,
	})
	// Callback on the loopback host bounces through /_auth/claimed.
	req = httptest.NewRequest("GET", "http://localhost:8080/_auth/callback?code=good&state="+url.QueryEscape(state), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body.String())
	}
	target := rec.Header().Get("Location")
	if !strings.Contains(target, "/_auth/claimed?ticket=") || !strings.Contains(target, "next=%2Fsettings") {
		t.Fatalf("callback target = %q", target)
	}
	// The claimed hop sets the cookie (the target is already absolute).
	req = httptest.NewRequest("GET", target, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("claimed = %d", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("session cookie missing after claimed")
	}
	if _, aerr := s.authSvc.VerifyToken(cookie.Value); aerr != nil {
		t.Fatalf("claimed cookie token invalid: %v", aerr)
	}
}

func TestOIDCCallbackFailurePaths(t *testing.T) {
	s, h, iss := oidcFull(t)
	// Missing code.
	state := s.signState("/next", s.Now())
	req := httptest.NewRequest("GET", "http://x/_auth/callback?state="+url.QueryEscape(state), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "missing code") {
		t.Fatalf("missing code = %d", rec.Code)
	}
	// Token exchange failure → 503.
	iss.failExpr = true
	req = httptest.NewRequest("GET", "http://x/_auth/callback?code=c&state="+url.QueryEscape(state), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "token exchange failed") {
		t.Fatalf("exchange fail = %d", rec.Code)
	}
	iss.failExpr = false
	// No id_token handed out → exchange gets a bad status (400 stub) → 503.
	req = httptest.NewRequest("GET", "http://x/_auth/callback?code=c&state="+url.QueryEscape(state), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no token = %d", rec.Code)
	}
	// Forged state → 400 (already covered; keep the exact mapping).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_auth/callback?code=c&state=forged", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d", rec.Code)
	}
	// sanitizeNext truth table.
	for next, want := range map[string]string{"//evil": "/", "https://x": "/", "/ok?1": "/ok?1"} {
		if got := sanitizeNext(next); got != want {
			t.Fatalf("sanitizeNext(%q) = %q, want %q", next, got, want)
		}
	}
	if s.scheme() != "http" {
		t.Fatal("scheme must be http without TLS")
	}
	s.tlsOn = true
	if s.scheme() != "https" {
		t.Fatal("scheme must be https with TLS")
	}
	s.tlsOn = false
	if hostOnlyPortSuffix("x:80") != "localhost:80" || hostOnlyPortSuffix("x") != "localhost" {
		t.Fatal("hostOnlyPortSuffix truth table broken")
	}
}

func TestOIDCLogoutTokensPageAndMint(t *testing.T) {
	s, h, _ := oidcFull(t)
	sess, _ := s.authSvc.MintSession("alice@example.com")

	// Logout clears the cookie and honors ?next=.
	req := httptest.NewRequest("GET", "http://x/_auth/logout?next=/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("logout = %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout must clear the session cookie")
	}

	// Tokens page requires a session → anonymous gets 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_auth/tokens", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokens page anon = %d", rec.Code)
	}
	// With a session → HTML mint page.
	req = httptest.NewRequest("GET", "http://x/_auth/tokens", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice@example.com") {
		t.Fatalf("tokens page = %d", rec.Code)
	}

	// Mint without a session → 401.
	req = httptest.NewRequest("POST", "http://x/_auth/tokens", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("mint anon = %d", rec.Code)
	}
	// Mint with a session: assert no-store + expires_at shape directly.
	req = httptest.NewRequest("POST", "http://x/_auth/tokens", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec = httptest.NewRecorder()
	s.authTokensMint(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "expires_at") {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(tokensPageHTML("http://x", principalAlice), "Mint token") {
		t.Fatal("tokens page HTML missing the mint button")
	}
}

func TestAuthRedirectURIForms(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.PublicURL = "https://pub.test"
	req := httptest.NewRequest("GET", "http://x/_auth/login", nil)
	if got := s.authRedirectURI(req); got != "https://pub.test/_auth/callback" {
		t.Fatalf("public redirect = %q", got)
	}
	s.cfg.Server.PublicURL = ""
	req = httptest.NewRequest("GET", "http://localhost:9099/_auth/login", nil)
	if got := s.authRedirectURI(req); got != "http://localhost:9099/_auth/callback" {
		t.Fatalf("loopback redirect = %q", got)
	}
	s.tlsOn = true
	req = httptest.NewRequest("GET", "http://localhost/_auth/login", nil)
	if got := s.authRedirectURI(req); got != "https://localhost/_auth/callback" {
		t.Fatalf("tls loopback redirect = %q", got)
	}
	s.tlsOn = false
	// Non-loopback host falls back to baseURL from the request.
	req = httptest.NewRequest("GET", "http://host.example/_auth/login", nil)
	if got := s.authRedirectURI(req); got != "http://host.example/_auth/callback" {
		t.Fatalf("host redirect = %q", got)
	}
}

func TestExchangeCodeRetriesOnce(t *testing.T) {
	s, _, iss := oidcFull(t)
	tv := true
	iss.idTokens <- iss.mint(t, map[string]any{
		"aud": "walhub", "exp": time.Now().Add(time.Hour).Unix(),
		"email": "alice@example.com", "email_verified": tv,
	})
	tok := s.exchangeCode(context.Background(), iss.srvURL()+"/token", "c", "http://localhost/cb")
	if tok == "" {
		t.Fatal("exchange must return the id token")
	}
	if _, aerr := s.authSvc.verifyIDToken(context.Background(), tok, true); aerr != nil {
		t.Fatalf("verify exchanged token: %v", aerr)
	}
	// failExpr breaks both tries → "".
	iss.failExpr = true
	if got := s.exchangeCode(context.Background(), iss.srvURL()+"/token", "c", "http://localhost/cb"); got != "" {
		t.Fatalf("failed exchange = %q", got)
	}
}
