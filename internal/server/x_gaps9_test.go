// x_gaps9_test.go — Forgejo #289 (internal/server coverage gate): real
// behavioral tests for the uncovered auth/OIDC/setup branches, in the
// issue's value order (OIDC error branches → wgtPrincipal/Authenticate →
// setup auth-test/body/coerce edges). Every test pins a contract — error
// kinds, wire shapes, or fail-closed behavior — never bare statement hits.
package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// --- OIDC: wgtPrincipal + Authenticate ---------------------------------------

func TestWGTPrincipalRoundTripAndReject(t *testing.T) {
	s, _ := oidcServer(t)
	tok, err := s.authSvc.MintToken("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// A minted wgt_ token authenticates to its email principal (§8.3).
	req := httptest.NewRequest("GET", "http://x/_auth/check", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Wire)
	p, aerr := s.authSvc.Authenticate(req, s.cfg)
	if aerr != nil || p.Name != "alice" || p.Email != "alice@example.com" {
		t.Fatalf("wgt auth = %+v %v", p, aerr)
	}
	// A well-formed but forged wgt_ wire fails closed with ErrInvalid.
	forged := "wgt_" + b64url([]byte("token\n9999999999\n9999999999\nmallory@evil.test")) +
		"." + b64url([]byte("wrong-mac-wrong-mac-wrong-mac-12"))
	req = httptest.NewRequest("GET", "http://x/_auth/check", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	if _, aerr := s.authSvc.Authenticate(req, s.cfg); aerr == nil || aerr.Kind != auth.ErrInvalid {
		t.Fatalf("forged wgt = %v, want ErrInvalid", aerr)
	}
}

func TestAuthenticateUnknownModeIsAnonymous(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "bogus-mode"
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	p, aerr := s.authSvc.Authenticate(httptest.NewRequest("GET", "http://x/", nil), s.cfg)
	if aerr != nil || !p.Anonymous {
		t.Fatalf("unknown mode = %+v %v, want anonymous", p, aerr)
	}
}

func TestPrincipalForNameOIDCAdmission(t *testing.T) {
	s, _ := oidcServer(t)
	p, err := s.authSvc.PrincipalForName("alice@example.com")
	if err != nil || p.Name != "alice" || p.Email != "alice@example.com" || !p.Write {
		t.Fatalf("allowed email = %+v %v", p, err)
	}
	// Outside every allowlist the SSH-key principal is denied (§8.4 email
	// policy — the same gate the browser login applies).
	if _, err := s.authSvc.PrincipalForName("intruder@evil.test"); err == nil {
		t.Fatal("disallowed email must be denied a principal")
	} else {
		var ae *auth.AuthError
		if !errors.As(err, &ae) || ae.Kind != auth.ErrForbidden {
			t.Fatalf("denied email err = %v, want ErrForbidden", err)
		}
	}
}

// --- VerifyToken wire negatives (§8.5) ---------------------------------------

func TestVerifyTokenWireNegatives(t *testing.T) {
	s, _ := oidcServer(t)
	secret := s.cfg.Server.Auth.SessionSecret
	macOf := func(payload string) string {
		return b64url(hmacSHA256([]byte(secret), []byte(payload)))
	}
	now := time.Now()
	valid, err := s.authSvc.MintToken("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	expiredPayload := fmt.Sprintf("token\n%d\n%d\nalice@example.com",
		now.Add(-time.Hour).Unix(), now.Add(-2*time.Hour).Unix())
	cases := []struct {
		name string
		wire string
		kind auth.AuthErrorKind
		why  string
	}{
		{"no dot", "wgt_abc", auth.ErrInvalid, "malformed"},
		{"bad payload b64", "wgt_!!!." + b64url([]byte("x")), auth.ErrInvalid, "malformed"},
		{"bad mac b64", "wgt_" + b64url([]byte("a\nb\nc\nd")) + ".!!!", auth.ErrInvalid, "malformed"},
		{"short payload", "wgt_" + b64url([]byte("a\nb")) + "." + macOf("a\nb"), auth.ErrInvalid, "malformed"},
		{"unknown kind", "wgt_" + b64url([]byte("x\ny\nz\ntoken\nq")) + "." + macOf("x\ny\nz\ntoken\nq"), auth.ErrInvalid, "malformed"},
		{"tampered mac", valid.Wire[:len(valid.Wire)-1] + "A", auth.ErrInvalid, "invalid token"},
		{"expired", "wgt_" + b64url([]byte(expiredPayload)) + "." + macOf(expiredPayload), auth.ErrInvalid, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, aerr := s.authSvc.VerifyToken(tc.wire); aerr == nil || aerr.Kind != tc.kind ||
				!strings.Contains(aerr.Why, tc.why) {
				t.Fatalf("wire %q = %v, want %v %q", tc.wire, aerr, tc.kind, tc.why)
			}
		})
	}
	// The minted token verifies with its kind, email, and TTL intact.
	got, aerr := s.authSvc.VerifyToken(valid.Wire)
	if aerr != nil || got.Kind != tokenKind || got.Email != "alice@example.com" || !got.ExpiresAt.After(now) {
		t.Fatalf("valid token = %+v %v", got, aerr)
	}
	// Without a session secret every verification is unavailable (§8.5).
	bare := NewAuthService(&config.Auth{Mode: "oidc"}, time.Now)
	if _, aerr := bare.VerifyToken(valid.Wire); aerr == nil || aerr.Kind != auth.ErrUnavailable {
		t.Fatalf("no-secret verify = %v, want ErrUnavailable", aerr)
	}
}

// --- jwk.parse + JWKS.Verify negatives (§8.4 fail-closed) ---------------------

func TestJWKParseNegatives(t *testing.T) {
	goodN := b64url([]byte{0x01, 0x02})
	cases := []struct {
		name string
		k    jwk
	}{
		{"rsa bad N", jwk{Kty: "RSA", N: "!!!", E: b64url([]byte{1})}},
		{"rsa bad E", jwk{Kty: "RSA", N: goodN, E: "!!!"}}, // auth.go parse E branch
		{"ec wrong curve", jwk{Kty: "EC", Crv: "P-384"}},
		{"ec bad X", jwk{Kty: "EC", Crv: "P-256", X: "!!!", Y: goodN}}, // X branch
		{"ec bad Y", jwk{Kty: "EC", Crv: "P-256", X: goodN, Y: "!!!"}}, // Y branch
		{"unknown kty", jwk{Kty: "oct"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.k.parse(); err == nil {
				t.Fatalf("%s must not parse", tc.name)
			}
		})
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := jwk{Kty: "EC", Crv: "P-256",
		X: b64url(priv.PublicKey.X.Bytes()), Y: b64url(priv.PublicKey.Y.Bytes())}
	if err := k.parse(); err != nil || k.ecdsa == nil {
		t.Fatalf("valid P-256 must parse: %v", err)
	}
}

func b64json(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b64url(b)
}

func pad32(x *big.Int) []byte {
	b := x.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func TestJWKSVerifyRejects(t *testing.T) {
	s, _ := oidcServer(t)
	a := &s.cfg.Server.Auth
	mk := func(hdr, pay, sig string) string { return hdr + "." + pay + "." + sig }

	// Unknown kid against a dead issuer → unavailable, never anonymous (§8.4).
	dead := NewJWKS("http://127.0.0.1:1")
	tok := mk(b64json(t, map[string]string{"alg": "RS256", "kid": "nope"}), b64url([]byte("{}")), b64url([]byte("x")))
	if _, aerr := dead.Verify(context.Background(), tok, a, false); aerr == nil ||
		aerr.Kind != auth.ErrUnavailable || !strings.Contains(aerr.Why, "key refresh failed") {
		t.Fatalf("dead-issuer verify = %v, want ErrUnavailable/refresh-failed", aerr)
	}

	// RS256 against a key entry without an RSA key → type mismatch.
	j := NewJWKS("https://issuer.test")
	j.keys["rsa-mismatch"] = &jwk{Kid: "rsa-mismatch", Alg: "RS256"}
	tok = mk(b64json(t, map[string]string{"alg": "RS256", "kid": "rsa-mismatch"}), b64url([]byte("{}")), b64url([]byte("x")))
	if _, aerr := j.Verify(context.Background(), tok, a, false); aerr == nil || aerr.Why != "key type mismatch" {
		t.Fatalf("rsa mismatch = %v", aerr)
	}

	// RS256 with a real key but a forged signature → rejected.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	j.keys["rsa"] = &jwk{Kid: "rsa", Alg: "RS256", rsa: &priv.PublicKey}
	sig := make([]byte, 256)
	for i := range sig {
		sig[i] = 0x7f
	}
	tok = mk(b64json(t, map[string]string{"alg": "RS256", "kid": "rsa"}), b64url([]byte("{}")), b64url(sig))
	if _, aerr := j.Verify(context.Background(), tok, a, false); aerr == nil || aerr.Why != "bad signature" {
		t.Fatalf("forged rsa = %v", aerr)
	}

	// ES256 against a key entry without an EC key → type mismatch.
	j.keys["ec-mismatch"] = &jwk{Kid: "ec-mismatch", Alg: "ES256"}
	tok = mk(b64json(t, map[string]string{"alg": "ES256", "kid": "ec-mismatch"}), b64url([]byte("{}")), b64url([]byte("x")))
	if _, aerr := j.Verify(context.Background(), tok, a, false); aerr == nil || aerr.Why != "key type mismatch" {
		t.Fatalf("ec mismatch = %v", aerr)
	}

	// ES256 with a real key but a truncated signature → rejected.
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	j.keys["ec"] = &jwk{Kid: "ec", Alg: "ES256", ecdsa: &ecPriv.PublicKey}
	tok = mk(b64json(t, map[string]string{"alg": "ES256", "kid": "ec"}), b64url([]byte("{}")), b64url([]byte("short")))
	if _, aerr := j.Verify(context.Background(), tok, a, false); aerr == nil || aerr.Why != "bad signature" {
		t.Fatalf("short ec sig = %v", aerr)
	}

	// ES256 with a VALID signature but undecodable claims → malformed claims.
	hdr := b64json(t, map[string]string{"alg": "ES256", "kid": "ec"})
	signing := hdr + ".!!!"
	sum := sha256.Sum256([]byte(signing))
	r, sv, err := ecdsa.Sign(rand.Reader, ecPriv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	tok = signing + "." + b64url(append(pad32(r), pad32(sv)...))
	if _, aerr := j.Verify(context.Background(), tok, a, false); aerr == nil || aerr.Why != "malformed claims" {
		t.Fatalf("bad claims = %v", aerr)
	}
}

// --- JWKS fetch/discovery negatives -------------------------------------------

func TestJWKSFetchNegatives(t *testing.T) {
	ctx := context.Background()
	discovery := func(jwksURI string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/openid-configuration" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jwks_uri":%q}`, jwksURI)
		}))
	}
	// Malformed jwks_uri → the fetch request cannot be built (§8.4).
	d := discovery("://bad-jwks-uri")
	defer d.Close()
	if err := NewJWKS(d.URL).fetch(ctx); err == nil {
		t.Fatal("bad jwks_uri must fail the fetch")
	}
	// Unreachable jwks_uri → fetch error (fail closed, no partial keys).
	d2 := discovery("http://127.0.0.1:1/keys")
	defer d2.Close()
	if err := NewJWKS(d2.URL).fetch(ctx); err == nil {
		t.Fatal("refused jwks_uri must fail the fetch")
	}
	// Truncated body (Content-Length lie) → read error, not partial keys.
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"keys":`))
	}))
	defer jwks.Close()
	d3 := discovery(jwks.URL)
	defer d3.Close()
	if err := NewJWKS(d3.URL).fetch(ctx); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("truncated jwks = %v, want unexpected EOF", err)
	}
	// A healthy endpoint round-trips (control: success path stays green).
	var okURL string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "openid-configuration") {
			_, _ = fmt.Fprintf(w, `{"jwks_uri":%q}`, okURL+"/keys")
			return
		}
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer ok.Close()
	okURL = ok.URL
	j := NewJWKS(ok.URL) // healthy discovery + empty key set
	if err := j.fetch(ctx); err != nil {
		t.Fatalf("healthy fetch = %v", err)
	}
	// Torn discovery document → discoverDoc surfaces the decode error.
	torn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{{{not json`))
	}))
	defer torn.Close()
	if _, err := NewJWKS(torn.URL).discoverDoc(ctx); err == nil {
		t.Fatal("torn discovery doc must error")
	}
}

// --- exchangeCode negatives (§8.6: one retry, then "") -------------------------
func TestExchangeCodeNegatives(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ctx := context.Background()
	// Unparseable endpoint → both tries fail → "".
	if got := s.exchangeCode(ctx, "://bad endpoint", "c", "http://localhost/cb"); got != "" {
		t.Fatalf("bad endpoint = %q, want empty", got)
	}
	// Refused endpoint → both tries fail → "".
	if got := s.exchangeCode(ctx, "http://127.0.0.1:1/token", "c", "http://localhost/cb"); got != "" {
		t.Fatalf("refused endpoint = %q, want empty", got)
	}
	// Non-200 → retry → "".
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fail.Close()
	if got := s.exchangeCode(ctx, fail.URL+"/token", "c", "http://localhost/cb"); got != "" {
		t.Fatalf("500 endpoint = %q, want empty", got)
	}
	// Malformed JSON body → "" (no partial token).
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{oops`))
	}))
	defer junk.Close()
	if got := s.exchangeCode(ctx, junk.URL+"/token", "c", "http://localhost/cb"); got != "" {
		t.Fatalf("junk body = %q, want empty", got)
	}
	// Control: a token endpoint round-trips the id_token.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id_token":"tok-abc"}`))
	}))
	defer ok.Close()
	if got := s.exchangeCode(ctx, ok.URL+"/token", "c", "http://localhost/cb"); got != "tok-abc" {
		t.Fatalf("healthy exchange = %q, want tok-abc", got)
	}
}

// --- setupAuthTest edges (§3.4) -------------------------------------------------

func TestSetupAuthTestEdgeCases(t *testing.T) {
	post := func(s *Server, body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/v1/setup/auth/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.setupAuthTest(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	msgOf := func(out map[string]any) string {
		var msgs []string
		if errs, ok := out["errors"].([]any); ok {
			for _, e := range errs {
				if m, ok := e.(map[string]any)["message"].(string); ok {
					msgs = append(msgs, m)
				}
			}
		}
		return strings.Join(msgs, "; ")
	}
	s, _ := setupMergeServer(t, t.TempDir())

	// Malformed JSON → 422 naming the expected shape.
	if code, out := post(s, "{not json"); code != http.StatusUnprocessableEntity ||
		!strings.Contains(msgOf(out), "body must be JSON") {
		t.Fatalf("bad json = %d %v", code, out)
	}
	// Non-http(s) redirect_uri → 422 keyed to redirect_uri.
	if code, out := post(s, `{"issuer":"https://id.example.com","redirect_uri":"ftp://x/cb"}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(msgOf(out), "redirect_uri") {
		t.Fatalf("bad redirect = %d %v", code, out)
	}
	// Discovery 404 → 422 naming the status (not a 500 leak).
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer missing.Close()
	if code, out := post(s, `{"issuer":`+strconv.Quote(missing.URL)+`}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(msgOf(out), "discovery returned 404") {
		t.Fatalf("404 discovery = %d %v", code, out)
	}
	// Discovery 200 with a torn body → 422 naming the document.
	torn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{{{`))
	}))
	defer torn.Close()
	if code, out := post(s, `{"issuer":`+strconv.Quote(torn.URL)+`}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(msgOf(out), "not a valid OIDC discovery document") {
		t.Fatalf("torn discovery = %d %v", code, out)
	}
	// Gated: in normal mode with token auth, anonymous callers get 403.
	s.cfg.Server.Auth.Mode = "token"
	if code, _ := post(s, `{"issuer":"https://id.example.com"}`); code != http.StatusForbidden {
		t.Fatalf("anonymous probe = %d, want 403", code)
	}
}

// --- parseSetupBody + configCoerce edges ----------------------------------------

type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("torn body") }

func parseBody(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/setup/test", strings.NewReader(body))
	return parseSetupBody(req, config.Defaults())
}

func TestParseSetupBodyEdges(t *testing.T) {
	// Torn body → the read error surfaces (never a half-parsed config).
	req := &http.Request{Body: io.NopCloser(errBody{})}
	if _, err := parseSetupBody(req, config.Defaults()); err == nil {
		t.Fatal("torn body must error")
	}
	// Raw TOML spellings: bare ints ride the int64 branch, floats the
	// float64 branch (no exponents on the wire).
	c, err := parseBody(t, "[server]\nmax_push_bytes = 123")
	if err != nil || int64(c.Server.MaxPushBytes) != 123 {
		t.Fatalf("toml int = %+v %v", c.Server.MaxPushBytes, err)
	}
	c, err = parseBody(t, "[server]\nmax_push_bytes = 1.5e3")
	if err != nil || int64(c.Server.MaxPushBytes) != 1500 {
		t.Fatalf("toml float = %+v %v", c.Server.MaxPushBytes, err)
	}
	// Inline tables are not scalar spellings → typed error, not a panic.
	if _, err := parseBody(t, "[server]\nrequest_timeout = {a = 1}"); err == nil ||
		!strings.Contains(err.Error(), "not a duration") {
		t.Fatalf("inline table = %v, want duration error", err)
	}
	// Unknown keys fail with the key named.
	if _, err := parseBody(t, "[server]\nno_such_key = 1"); err == nil ||
		!strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key = %v", err)
	}
	// A JSON object where a scalar belongs → the section guard fires.
	if _, err := parseBody(t, `{"overrides": {"server.auth": {"mode": "x"}}}`); err == nil ||
		!strings.Contains(err.Error(), "is a section, not a value") {
		t.Fatalf("nested object = %v", err)
	}
	// A key without a section → the dotted-key guard fires.
	if _, err := parseBody(t, `{"overrides": {"mode": "x"}}`); err == nil ||
		!strings.Contains(err.Error(), "must be section.field") {
		t.Fatalf("bare key = %v", err)
	}
	// Unknown dotted key → unknown-key error through the JSON channel.
	if _, err := parseBody(t, `{"overrides": {"server.nope": "x"}}`); err == nil ||
		!strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown dotted = %v", err)
	}
}

func TestConfigCoerceNegatives(t *testing.T) {
	strategyOf := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Bundles.Strategy).Elem()
	}
	cases := []struct {
		name string
		fv   func() reflect.Value
		raw  string
		path string
		want string // "" = success expected
	}{
		{"int32 ok", func() reflect.Value { v := int32(0); return reflect.ValueOf(&v).Elem() }, "5", "p.n", ""},
		{"int32 overflow", func() reflect.Value { v := int32(0); return reflect.ValueOf(&v).Elem() }, "99999999999", "p.n", "out of range"},
		{"int bad", func() reflect.Value { v := 0; return reflect.ValueOf(&v).Elem() }, "abc", "p.n", "not an int"},
		{"uint ok", func() reflect.Value { v := uint32(0); return reflect.ValueOf(&v).Elem() }, "7", "p.n", ""},
		{"uint bad", func() reflect.Value { v := uint32(0); return reflect.ValueOf(&v).Elem() }, "abc", "p.n", "not an int"},
		{"uint negative", func() reflect.Value { v := uint32(0); return reflect.ValueOf(&v).Elem() }, "-5", "p.n", "not an int"},
		{"uint overflow", func() reflect.Value { v := uint32(0); return reflect.ValueOf(&v).Elem() }, "4294967296", "p.n", "out of range"},
		{"float ok", func() reflect.Value { v := 0.0; return reflect.ValueOf(&v).Elem() }, "1.5", "p.f", ""},
		{"float bad", func() reflect.Value { v := 0.0; return reflect.ValueOf(&v).Elem() }, "abc", "p.f", "not a number"},
		{"duration ok", func() reflect.Value { v := config.Duration(0); return reflect.ValueOf(&v).Elem() }, "2h", "p.d", ""},
		{"duration bad", func() reflect.Value { v := config.Duration(0); return reflect.ValueOf(&v).Elem() }, "zzz", "p.d", "not a duration"},
		{"bytesize bad", func() reflect.Value { v := config.ByteSize(0); return reflect.ValueOf(&v).Elem() }, "zzz", "p.b", "not a size"},
		{"bool bad", func() reflect.Value { v := false; return reflect.ValueOf(&v).Elem() }, "yes", "p.b", "not a bool"},
		{"strategy wrong table", strategyOf, "[[wrong]]\nname = \"x\"", "bundles.strategy", "must define [[strategy]]"},
		{"strategy bad field", strategyOf, "[[strategy]]\nkeep = \"many\"", "bundles.strategy", "bundles.strategy"},
		{"non-string list", func() reflect.Value { v := []int{}; return reflect.ValueOf(&v).Elem() }, "a,b", "p.l", "unsupported list element"},
		{"unsupported kind", func() reflect.Value { v := make(chan int); return reflect.ValueOf(&v).Elem() }, "x", "p.c", "unsupported type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := configCoerce(tc.fv(), tc.raw, tc.path)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("coerce %q = %v, want success", tc.raw, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("coerce %q = %v, want %q", tc.raw, err, tc.want)
			}
		})
	}
	// A valid strategy fragment decodes by the field's toml name (§3.4).
	fv := strategyOf()
	frag := "[[strategy]]\nname = \"weekly\"\nkind = \"full\"\nschedule = \"0 0 * * * *\"\nkeep = 2"
	if err := configCoerce(fv, frag, "bundles.strategy"); err != nil {
		t.Fatalf("valid fragment = %v", err)
	}
	if fv.Len() != 1 || fv.Index(0).FieldByName("Keep").Int() != 2 {
		t.Fatalf("fragment decoded = %+v", fv)
	}
}

// --- fmtAny + schema skip -------------------------------------------------------

func TestFmtAnyAndSchemaSkip(t *testing.T) {
	// Invalid reflection → nil (never a panic on schema edges).
	if got := fmtAny(reflect.Value{}); got != nil {
		t.Fatalf("fmtAny(invalid) = %v, want nil", got)
	}
	if got := fmtAny(reflect.ValueOf(config.Duration(90 * time.Second))); got != "1m30s" {
		t.Fatalf("fmtAny(duration) = %v", got)
	}
	// Untagged struct fields never become schema keys.
	type tagged struct {
		SkipMe string
		Keep   string `toml:"keep"`
	}
	g := &setupSchemaGroup{}
	appendSchemaKeys(g, "s.", reflect.ValueOf(tagged{}), reflect.ValueOf(tagged{}))
	if len(g.Keys) != 1 || g.Keys[0].Key != "s.keep" {
		t.Fatalf("schema keys = %+v, want [s.keep]", g.Keys)
	}
}
