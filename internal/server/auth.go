package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// AuthService implements §8: credential resolution order (8.2), the per-mode
// decision trees (8.3), hand-rolled JWKS ID-token verification (8.4), and
// walgit-issued wgt_ tokens / sessions (8.5).
type AuthService struct {
	cfg *config.Auth
	now func() time.Time

	static map[string]auth.Principal // resolved token → principal (token_env resolved at startup)
	jwks   *JWKS
}

func NewAuthService(a *config.Auth, now func() time.Time) *AuthService {
	if now == nil {
		now = time.Now
	}
	s := &AuthService{cfg: a, now: now, static: map[string]auth.Principal{}}
	for _, t := range a.Tokens {
		tok := t.Token
		if t.TokenEnv != "" {
			tok = os.Getenv(t.TokenEnv) // env overrides the literal; empty-resolved dropped
		}
		if tok == "" {
			continue
		}
		s.static[tok] = auth.Principal{Name: t.Principal, Write: t.Write, Admin: t.Admin}
	}
	if a.Mode == "oidc" {
		s.jwks = NewJWKS(a.Issuer)
	}
	return s
}

// credKind is where the client credential came from (§8.2).
type credKind int

const (
	credNone credKind = iota
	credWalgitHeader
	credEdgeClientAuth
	credAuthorization
)

// resolveCredential applies the §8.2 order.
func resolveCredential(r *http.Request) (cred string, from credKind) {
	if v := r.Header.Get("X-Walgit-Authorization"); v != "" {
		return v, credWalgitHeader
	}
	if strings.Contains(r.Header.Get("X-Walgit-Capabilities"), "client-authorization") {
		return "", credEdgeClientAuth // the client sent none; Authorization is the edge hop's
	}
	return r.Header.Get("Authorization"), credAuthorization
}

// parseCredential splits Bearer (case-insensitive) / Basic; password = token.
func parseCredential(cred string) (token string, basic bool, err error) {
	if hasPrefixFold(strings.TrimSpace(cred), "bearer ") {
		return strings.TrimSpace(cred[len("Bearer "):]), false, nil
	}
	if hasPrefixFold(strings.TrimSpace(cred), "basic ") {
		dec, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(cred[len("Basic "):]))
		if derr != nil {
			return "", true, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed basic credentials"}
		}
		if i := strings.IndexByte(string(dec), ':'); i >= 0 {
			return string(dec[i+1:]), true, nil // user ignored (LFS conventions aside)
		}
		return string(dec), true, nil
	}
	if strings.TrimSpace(cred) == "" {
		return "", false, nil
	}
	return "", false, fmt.Errorf("unparseable credential")
}

// Authenticate resolves the request's principal per the §8.3 trees.
func (s *AuthService) Authenticate(r *http.Request, c *config.Config) (auth.Principal, *auth.AuthError) {
	switch s.cfg.Mode {
	case "none":
		return auth.None(), nil
	case "token":
		return s.authToken(r)
	case "oidc":
		return s.authOIDC(r)
	}
	return auth.Anonymous(), nil
}

// authToken is the §8.3 `token` tree.
func (s *AuthService) authToken(r *http.Request) (auth.Principal, *auth.AuthError) {
	cred, _ := resolveCredential(r)
	if cred == "" {
		return auth.Anonymous(), nil // read gated by server.auth.anonymous_read
	}
	tok, basic, perr := parseCredential(cred)
	if perr != nil {
		return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid credentials"}
	}
	if p, ok := s.static[tok]; ok {
		return p, nil
	}
	_ = basic
	return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid token"}
}

// authOIDC is the §8.3 `oidc` tree.
func (s *AuthService) authOIDC(r *http.Request) (auth.Principal, *auth.AuthError) {
	cred, _ := resolveCredential(r)
	if cred != "" {
		tok, basic, perr := parseCredential(cred)
		if perr != nil {
			return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid credentials"}
		}
		// Static tokens work in oidc mode.
		if p, ok := s.static[tok]; ok {
			return p, nil
		}
		if strings.HasPrefix(tok, "wgt_") {
			return s.wgtPrincipal(tok)
		}
		if basic {
			// Basic: static token or wgt_ only — no ID tokens over Basic.
			return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid token"}
		}
		// else verify as ID token via JWKS.
		return s.verifyIDToken(r.Context(), tok, false)
	}
	// No credential: session cookie → authenticated principal, else anonymous.
	if c, cerr := r.Cookie("walgit_session"); cerr == nil && c.Value != "" {
		st, terr := s.VerifyToken(c.Value)
		if terr == nil && st.Kind == "session" {
			p, aerr := s.principalFromEmail(st.Email)
			if aerr != nil {
				return auth.Principal{}, aerr
			}
			return p, nil
		}
	}
	return auth.Anonymous(), nil
}

// verifyIDToken verifies a JWT via the hand-rolled JWKS (§8.4). browserFlow
// pins the audience to exactly the oauth client id.
func (s *AuthService) verifyIDToken(ctx context.Context, raw string, browserFlow bool) (auth.Principal, *auth.AuthError) {
	claims, err := s.jwks.Verify(ctx, raw, s.cfg, browserFlow)
	if err != nil {
		return auth.Principal{}, err
	}
	return s.principalFromEmail(claims.Email)
}

// wgtPrincipal verifies a wgt_-prefixed token and derives the principal from
// the email claim (§8.3 oidc tree).
func (s *AuthService) wgtPrincipal(wire string) (auth.Principal, *auth.AuthError) {
	st, aerr := s.VerifyToken(wire)
	if aerr != nil {
		return auth.Principal{}, aerr
	}
	return s.principalFromEmail(st.Email)
}

// principalFromEmail applies the §8.4 email policy (7).
func (s *AuthService) principalFromEmail(email string) (auth.Principal, *auth.AuthError) {
	email = strings.ToLower(email)
	domain := ""
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		domain = email[i+1:]
	}
	allowed := false
	for _, d := range s.cfg.AllowedDomains {
		if strings.EqualFold(domain, d) {
			allowed = true
		}
	}
	for _, e := range s.cfg.AllowedEmails {
		if strings.EqualFold(email, e) {
			allowed = true
		}
	}
	if !allowed {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden,
			Why: "email not allowed"}
	}
	write := true
	if len(s.cfg.WriteDomains) > 0 {
		write = false
		for _, d := range s.cfg.WriteDomains {
			if strings.EqualFold(domain, d) {
				write = true
			}
		}
	}
	admin := false
	for _, e := range s.cfg.AdminEmails {
		if strings.EqualFold(email, e) {
			admin = true
		}
	}
	for _, d := range s.cfg.AdminDomains {
		if strings.EqualFold(domain, d) {
			admin = true
		}
	}
	return auth.Principal{Name: email, Write: write, Admin: admin}, nil
}

// requireRead/Write/Admin are the §8.3 checks.
func requireRead(p auth.Principal, anonymousRead bool) *auth.AuthError {
	if p.Anonymous && !anonymousRead {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	return nil
}

func requireWrite(p auth.Principal) *auth.AuthError {
	if !p.Write {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "write access required"}
	}
	return nil
}

func requireAdmin(p auth.Principal) *auth.AuthError {
	if !p.Admin {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "admin access required"}
	}
	return nil
}

// BrowserLoginEnabled reports §8.6 gating: mode oidc + session secret + client
// id + client secret.
func (s *AuthService) BrowserLoginEnabled() bool {
	return s.cfg.Mode == "oidc" && s.cfg.SessionSecret != "" &&
		s.cfg.OAuthClientID != "" && s.cfg.OAuthClientSecret != ""
}

// --- wgt_ tokens & sessions (§8.5) ---------------------------------------------------

// SessionToken is a verified walgit-issued token.
type SessionToken struct {
	Kind      string // "session" | "token"
	Email     string
	ExpiresAt time.Time
	IssuedAt  time.Time
	Wire      string // the full wire form (session tokens without prefix)
}

const (
	tokenPrefix = "wgt_"
	tokenKind   = "token"
	sessionKind = "session"
)

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64urlDecode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// MintToken issues an access token ("wgt_" prefix, kind token, TTL access_token_ttl).
func (s *AuthService) MintToken(email string) (SessionToken, error) {
	return s.mint(tokenKind, email, tokenPrefix)
}

// MintSession issues a session token (kind session, no prefix).
func (s *AuthService) MintSession(email string) (SessionToken, error) {
	return s.mint(sessionKind, email, "")
}

func (s *AuthService) mint(kind, email, prefix string) (SessionToken, error) {
	now := s.now()
	ttl := time.Duration(s.cfg.AccessTokenTTL)
	if kind == sessionKind {
		ttl = time.Duration(s.cfg.SessionTTL)
	}
	payload := fmt.Sprintf("%s\n%d\n%d\n%s", kind, now.Add(ttl).Unix(), now.Unix(), email)
	mac := hmacSHA256([]byte(s.cfg.SessionSecret), []byte(payload))
	wire := prefix + b64url([]byte(payload)) + "." + b64url(mac)
	return SessionToken{Kind: kind, Email: email, IssuedAt: now,
		ExpiresAt: now.Add(ttl), Wire: wire}, nil
}

// VerifyToken verifies the wire format
// `wgt_? + base64url(payload) + "." + base64url(mac)` with HMAC-SHA256 (§8.5).
func (s *AuthService) VerifyToken(wire string) (SessionToken, *auth.AuthError) {
	fail := func(k auth.AuthErrorKind, why string) (SessionToken, *auth.AuthError) {
		return SessionToken{}, &auth.AuthError{Kind: k, Why: why}
	}
	if s.cfg.SessionSecret == "" {
		return fail(auth.ErrUnavailable, "no session secret configured")
	}
	body := strings.TrimPrefix(wire, tokenPrefix)
	parts := strings.Split(body, ".")
	if len(parts) != 2 {
		return fail(auth.ErrInvalid, "malformed token")
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return fail(auth.ErrInvalid, "malformed token")
	}
	mac, err := b64urlDecode(parts[1])
	if err != nil {
		return fail(auth.ErrInvalid, "malformed token")
	}
	want := hmacSHA256([]byte(s.cfg.SessionSecret), payload)
	if subtle.ConstantTimeCompare(mac, want) != 1 {
		return fail(auth.ErrInvalid, "invalid token")
	}
	lines := strings.SplitN(string(payload), "\n", 4)
	if len(lines) != 4 {
		return fail(auth.ErrInvalid, "malformed token")
	}
	kind := lines[0]
	if kind != tokenKind && kind != sessionKind {
		return fail(auth.ErrInvalid, "malformed token")
	}
	exp, err1 := parseUnix(lines[1])
	iat, err2 := parseUnix(lines[2])
	if err1 != nil || err2 != nil {
		return fail(auth.ErrInvalid, "malformed token")
	}
	if s.now().After(exp) {
		return fail(auth.ErrInvalid, "token expired")
	}
	return SessionToken{Kind: kind, Email: lines[3], IssuedAt: iat, ExpiresAt: exp, Wire: wire}, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func parseUnix(s string) (time.Time, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

// --- hand-rolled JWKS (§8.4) -----------------------------------------------------------

// jwk is one JSON Web Key.
type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`

	pub   crypto.PublicKey
	ecdsa *ecdsa.PublicKey
	rsa   *rsa.PublicKey
}

// JWKS caches the issuer discovery + keys per §8.4: discovery once per
// process, keys honoring the JWKS Cache-Control max-age (default 300 s),
// stale-while-refresh on expiry, inline refresh on unknown kid (singleflight).
type JWKS struct {
	mu      sync.RWMutex
	issuer  string
	disc    oidcDiscovery
	discHas bool
	keys    map[string]*jwk
	fetched time.Time
	ttl     time.Duration
	client  *http.Client

	sfMu sync.Mutex
	sfIn bool // singleflight: one refresh leader at a time
	sfWg sync.WaitGroup
}

func NewJWKS(issuer string) *JWKS {
	return &JWKS{issuer: issuer, keys: map[string]*jwk{}, client: &http.Client{Timeout: 10 * time.Second}}
}

// nowKey carries a test clock into the JWKS fetch path.
type nowKey struct{}

// SetNow overrides the clock (tests).
func (j *JWKS) SetNow(t time.Time) { j.mu.Lock(); j.fetched = t; j.mu.Unlock() }

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// fetch (leader) refreshes discovery + keys with a 10 s context.
func (j *JWKS) fetch(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	j.mu.RLock()
	issuer := j.issuer
	j.mu.RUnlock()
	jwksURI, err := j.discover(cctx, issuer)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return err
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	ttl := 300 * time.Second
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		if d := maxAgeOf(cc); d > 0 {
			ttl = d
		}
	}
	j.mu.Lock()
	j.keys = map[string]*jwk{}
	for i := range doc.Keys {
		k := &doc.Keys[i]
		if err := k.parse(); err == nil {
			j.keys[k.Kid] = k
		}
	}
	j.fetched = j.nowOf(cctx)
	j.ttl = ttl
	j.mu.Unlock()
	return nil
}

func (j *JWKS) nowOf(ctx context.Context) time.Time {
	if t, ok := ctx.Value(nowKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

// Get returns the key for kid: stale-while-refresh on expiry, inline refresh
// on unknown kid; refresh failures → ErrUnavailable (§8.4).
func (j *JWKS) Get(ctx context.Context, kid string) (*jwk, *auth.AuthError) {
	j.mu.RLock()
	k := j.keys[kid]
	stale := j.ttl > 0 && time.Since(j.fetched) > j.ttl
	j.mu.RUnlock()
	if k != nil && !stale {
		return k, nil
	}
	if k == nil {
		// Inline refresh on unknown kid (thundering-herd safe: singleflight;
		// leader context has its own 10 s timeout).
		if err := j.refresh(ctx); err != nil {
			return nil, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "key refresh failed: " + err.Error()}
		}
		j.mu.RLock()
		k = j.keys[kid]
		j.mu.RUnlock()
		if k != nil {
			return k, nil
		}
		return nil, &auth.AuthError{Kind: auth.ErrInvalid, Why: "unknown key id"}
	}
	// Stale-while-refresh: serve stale keys while one background refresh runs.
	go func() {
		_ = j.refresh(context.Background())
	}()
	if k != nil {
		return k, nil
	}
	return nil, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "keys unavailable"}
}

// refresh runs at most one fetch at a time; followers share the outcome.
func (j *JWKS) refresh(ctx context.Context) error {
	j.mu.Lock()
	if j.sfIn {
		j.mu.Unlock()
		<-ctx.Done() // follower: never queue behind a slow issuer twice
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("refresh in flight")
	}
	j.sfIn = true
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.sfIn = false
		j.mu.Unlock()
	}()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return j.fetch(rctx)
}

// discoverDoc returns the parsed discovery document (cached once per
// process, §8.4).
func (j *JWKS) discoverDoc(ctx context.Context) (oidcDiscovery, error) {
	j.mu.RLock()
	cached := j.disc
	have := j.discHas
	j.mu.RUnlock()
	if have {
		return cached, nil
	}
	uri := strings.TrimSuffix(j.issuer, "/") + "/.well-known/openid-configuration"
	resp, err := j.client.Get(uri)
	if err != nil {
		return oidcDiscovery{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcDiscovery{}, fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	var doc oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return oidcDiscovery{}, err
	}
	j.mu.Lock()
	j.disc = doc
	j.discHas = true
	j.mu.Unlock()
	return doc, nil
}

// discover caches the discovery document once per process.
func (j *JWKS) discover(ctx context.Context, issuer string) (string, error) {
	uri := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	resp, err := j.client.Get(uri)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery: no jwks_uri")
	}
	return doc.JWKSURI, nil
}

func maxAgeOf(cc string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		p := strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(p), "max-age=") {
			var secs int
			if _, err := fmt.Sscanf(p, "max-age=%d", &secs); err == nil && secs >= 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return 0
}

// parse converts the JWK JSON into a crypto key.
func (k *jwk) parse() error {
	switch k.Kty {
	case "RSA":
		n, err := b64urlDecode(k.N)
		if err != nil {
			return err
		}
		e, err := b64urlDecode(k.E)
		if err != nil {
			return err
		}
		ei := new(big.Int).SetBytes(e).Int64()
		k.rsa = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(ei)}
		return nil
	case "EC":
		if k.Crv != "P-256" {
			return fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64urlDecode(k.X)
		if err != nil {
			return err
		}
		y, err := b64urlDecode(k.Y)
		if err != nil {
			return err
		}
		k.ecdsa = &ecdsa.PublicKey{Curve: elliptic.P256(),
			X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		return nil
	}
	return fmt.Errorf("unsupported kty %q", k.Kty)
}

// idClaims is the verified ID-token claim set.
type idClaims struct {
	Iss           string `json:"iss"`
	Sub           string `json:"sub"`
	Aud           string `json:"aud"`
	Exp           int64  `json:"exp"`
	Nbf           int64  `json:"nbf"`
	Iat           int64  `json:"iat"`
	Email         string `json:"email"`
	EmailVerified *bool  `json:"email_verified"`
}

// Verify checks alg/key/claims per §8.4 (leeway 30 s; iss with trailing slash
// stripped or its bare host; aud ∈ audiences ∪ {client_id}, pinned to the
// client id for the browser flow; email + email_verified required).
func (j *JWKS) Verify(ctx context.Context, raw string, a *config.Auth, browserFlow bool) (idClaims, *auth.AuthError) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed id token"}
	}
	hdrJSON, err := b64urlDecode(parts[0])
	if err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed id token"}
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed id token"}
	}
	// 1. alg MUST be RS256 or ES256.
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "unsupported alg"}
	}
	key, aerr := j.Get(ctx, hdr.Kid)
	if aerr != nil {
		return idClaims{}, aerr
	}
	// 2. The key's advertised alg must match the token alg.
	if key.Alg != "" && key.Alg != hdr.Alg {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "key alg mismatch"}
	}
	signing := raw[:len(raw)-len(parts[2])-1]
	sig, err := b64urlDecode(parts[2])
	if err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed signature"}
	}
	sum := sha256.Sum256([]byte(signing))
	switch hdr.Alg {
	case "RS256":
		if key.rsa == nil {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "key type mismatch"}
		}
		if err := rsa.VerifyPKCS1v15(key.rsa, crypto.SHA256, sum[:], sig); err != nil {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad signature"}
		}
	case "ES256":
		if key.ecdsa == nil {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "key type mismatch"}
		}
		if len(sig) != 64 {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad signature"}
		}
		r := new(big.Int).SetBytes(sig[:32])
		sv := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(key.ecdsa, sum[:], r, sv) {
			return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad signature"}
		}
	}
	payload, err := b64urlDecode(parts[1])
	if err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed claims"}
	}
	var c struct {
		Iss           string          `json:"iss"`
		Aud           json.RawMessage `json:"aud"`
		Exp           int64           `json:"exp"`
		Nbf           int64           `json:"nbf"`
		Iat           int64           `json:"iat"`
		Email         string          `json:"email"`
		EmailVerified *bool           `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "malformed claims"}
	}
	var auds []string
	if len(c.Aud) > 0 && c.Aud[0] == '"' {
		var one string
		if err := json.Unmarshal(c.Aud, &one); err == nil {
			auds = []string{one}
		}
	} else if len(c.Aud) > 0 {
		_ = json.Unmarshal(c.Aud, &auds)
	}
	now := j.nowOf(ctx)
	const leeway = 30 * time.Second // 3
	if c.Exp != 0 && now.After(time.Unix(c.Exp, 0).Add(leeway)) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "token expired"}
	}
	if c.Nbf != 0 && now.Add(leeway).Before(time.Unix(c.Nbf, 0)) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "token not yet valid"}
	}
	if c.Iat != 0 && now.Add(leeway).Before(time.Unix(c.Iat, 0)) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "token issued in the future"}
	}
	// 4. iss equals the configured issuer (trailing slash stripped) or its bare host.
	issWant := strings.TrimSuffix(a.Issuer, "/")
	issGot := strings.TrimSuffix(c.Iss, "/")
	if issGot != issWant && issGot != bareHost(issWant) {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "issuer mismatch"}
	}
	// 5. aud.
	audOK := containsString(auds, a.OAuthClientID)
	if !browserFlow {
		audOK = audOK || anyOf(auds, a.Audiences)
	}
	if !audOK {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "audience mismatch"}
	}
	// 6. email + email_verified required.
	if c.Email == "" || c.EmailVerified == nil || !*c.EmailVerified {
		return idClaims{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "email claim missing or unverified"}
	}
	claims := idClaims{Iss: c.Iss, Email: c.Email, Exp: c.Exp, Nbf: c.Nbf, Iat: c.Iat}
	return claims, nil
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func anyOf(list, allowed []string) bool {
	for _, x := range list {
		if containsString(allowed, x) {
			return true
		}
	}
	return false
}

func bareHost(iss string) string {
	s := strings.TrimSuffix(iss, "/")
	if i := strings.Index(s, "://"); i >= 0 {
		return s[i+3:]
	}
	return s
}

// --- /_auth endpoints shared with the OIDC flow ---------------------------------------

// meHandler answers GET /_auth/me → {principal, write} (§8.6).
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"principal": p.Name, "write": p.Write, "anonymous": p.Anonymous,
	})
}

// checkHandler answers /_auth/check (the edge's auth_request): 204 +
// X-Walgit-Principal + X-Walgit-Write + Cache-Control: private, max-age=300.
func (s *Server) authCheck(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	if p.Anonymous && !s.cfg.Server.Auth.AnonymousRead {
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		plainStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	h := w.Header()
	h.Set("X-Walgit-Principal", p.Name)
	if p.Write {
		h.Set("X-Walgit-Write", "1")
	} else {
		h.Set("X-Walgit-Write", "0")
	}
	h.Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusNoContent)
}

// identityForward resolves §8.6 identity forwarding (push broker): the hop's
// X-Walgit-Principal is honored only when the caller has write AND its name is
// in server.auth.trusted_forwarders; admin re-derived from policy.
func (s *AuthService) identityForward(r *http.Request, caller auth.Principal) auth.Principal {
	name := r.Header.Get("X-Walgit-Principal")
	if name == "" || !caller.Write || !containsString(s.cfg.TrustedForwarders, caller.Name) {
		return caller
	}
	p := caller
	p.Name = name // forwarded name replaces the principal, keeps write
	if em := principalEmail(name); em != "" {
		if fp, aerr := s.principalFromEmail(em); aerr == nil {
			p.Admin = fp.Admin
			p.Write = fp.Write || p.Write
		}
	}
	return p
}

func principalEmail(name string) string {
	if strings.Contains(name, "@") {
		return name
	}
	return ""
}

// randHex is a helper for nonces/tickets.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b64url(b)
}
