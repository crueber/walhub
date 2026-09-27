package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// statePayload is the HMAC-signed anti-forgery state: "{now+600}\n{nonce}\n{next}".
type statePayload struct {
	ExpiresAt time.Time
	Nonce     string
	Next      string
}

// signState produces base64url(payload) + "." + base64url(mac) with the
// session secret and a 600 s window (the state HMAC is the anti-forgery;
// the carried nonce binds the browser login to its ID token — F1).
func (s *Server) signState(next string, now time.Time) string {
	state, _ := s.signStateTTL(next, now, 600*time.Second)
	return state
}

// signStateTTL signs "{now+ttl}\n{nonce}\n{next}" and also returns the carried
// nonce so the login flow can bind it into the OIDC request (F1) and the
// claimed-ticket hop can use its own 60 s window (F8).
func (s *Server) signStateTTL(next string, now time.Time, ttl time.Duration) (string, string) {
	nonce := randHex(16)
	payload := fmt.Sprintf("%d\n%s\n%s", now.Add(ttl).Unix(), nonce, next)
	mac := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte(payload))
	return b64url([]byte(payload)) + "." + b64url(mac), nonce
}

// parseState verifies the HMAC and the embedded window; returns the carried
// nonce and the raw next slot.
func (s *Server) parseState(state string) (nonce, next string, ok bool) {
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return "", "", false
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return "", "", false
	}
	mac, err := b64urlDecode(parts[1])
	if err != nil {
		return "", "", false
	}
	want := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), payload)
	if !hmac.Equal(mac, want) {
		return "", "", false
	}
	lines := strings.SplitN(string(payload), "\n", 3)
	if len(lines) != 3 {
		return "", "", false
	}
	exp, err1 := parseUnix(lines[0])
	if err1 != nil || s.Now().After(exp) {
		return "", "", false
	}
	return lines[1], lines[2], true
}

// verifyState checks the HMAC and the 600 s window; returns the sanitized next.
func (s *Server) verifyState(state string) (string, bool) {
	_, next, ok := s.parseState(state)
	if !ok {
		return "", false
	}
	return sanitizeNext(next), true
}

// verifyLoginState checks the state like verifyState and additionally returns
// the carried nonce so the callback can bind the ID token to this login (F1).
func (s *Server) verifyLoginState(state string) (next, nonce string, ok bool) {
	nonce, raw, ok := s.parseState(state)
	if !ok {
		return "", "", false
	}
	return sanitizeNext(raw), nonce, true
}

// sanitizeNext requires the redirect target to start with a single "/" (no
// "//host", no scheme).
func sanitizeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// authLogin answers GET /_auth/login?next= (§8.6): fetch discovery → signed
// state → 302 to the issuer's authorization endpoint with response_type=code,
// scope=openid email, prompt=select_account, &hd= = first allowed domain, the
// state-carried nonce (verified against the ID token at the callback — F1),
// and PKCE S256 (the verifier rides a short-lived HttpOnly cookie — F1).
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authSvc.BrowserLoginEnabled() {
		// #344: the 501 string must not be the end-user experience — a
		// browser gets the rendered login page (with the disabled
		// explanation); API clients keep the plain status.
		if browserLooks(r) {
			next := sanitizeNext(r.URL.Query().Get("next"))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(loginUnavailableHTML(next)))
			return
		}
		plainStatus(w, http.StatusNotImplemented, "browser login is not enabled")
		return
	}
	next := sanitizeNext(r.URL.Query().Get("next"))
	disc, err := s.authSvc.jwks.discoverDoc(r.Context())
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "issuer discovery failed")
		return
	}
	state, nonce := s.signStateTTL(next, s.Now(), 600*time.Second)
	verifier := randHex(32)
	chalSum := sha256.Sum256([]byte(verifier))
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_pkce", Value: verifier, Path: "/_auth/callback",
		MaxAge: 600, HttpOnly: true, Secure: len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
	redirect := s.authRedirectURI(r)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("scope", "openid email")
	q.Set("prompt", "select_account")
	q.Set("nonce", nonce)
	q.Set("code_challenge", b64url(chalSum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("client_id", s.cfg.Server.Auth.OAuthClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	if len(s.cfg.Server.Auth.AllowedDomains) > 0 {
		q.Set("hd", s.cfg.Server.Auth.AllowedDomains[0])
	}
	target := disc.AuthEndpoint + "?" + q.Encode()
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// loginUnavailableHTML renders the #344 login entry page: a working
// "Log in with OIDC" button (GET /_auth/login?next=…, which starts the
// provider flow whenever browser login is enabled) plus a clear explanation
// for the disabled state. Served in two places: GET /_auth/login itself
// (501) when the trio is incomplete, and the gated-group 401 path for
// browser-ish GETs (middleware.go authFailure) so an unauthenticated browser
// never faces a bare "authentication required" with no path forward. The
// next target travels in the query string (url.QueryEscape — the only
// interpolation, so no HTML escaping hazard even when a call site passes a
// hostile RequestURI); the enabled /_auth/login flow additionally confines
// it via sanitizeNext before signing it into state.
func loginUnavailableHTML(next string) string {
	if next == "" {
		next = "/"
	}
	loginURL := "/_auth/login?next=" + url.QueryEscape(next)
	return `<!doctype html><html><head><title>walhub — log in</title></head><body>
<h1>Log in with OIDC</h1>
<p>Browser login is not enabled on this instance: the server's OIDC
configuration is incomplete (it needs <code>server.auth.session_secret</code>,
<code>server.auth.oauth_client_id</code> and
<code>server.auth.oauth_client_secret</code> alongside
<code>auth.mode = "oidc"</code>). The button below starts the sign-in flow
and works as soon as an administrator completes that configuration and
restarts the server.</p>
<p><a href="` + loginURL + `"><button>Log in with OIDC</button></a></p>
<p>If you are the administrator: run <code>walhub config check</code> (it
names the missing keys) or open <code>/setup</code> — the OIDC section
flags the incomplete trio before you save.</p>
</body></html>`
}

// authRedirectURI is {public_url}/_auth/callback; loopback origins use
// http(s)://localhost[:port] (§8.6 — the claimed-ticket hop then sets the
// cookie on walgit.localhost, a different cookie host).
func (s *Server) authRedirectURI(r *http.Request) string {
	base := s.baseURL(r)
	if isLoopbackHost(hostOnly(r.Host)) {
		scheme := requestScheme(r)
		_, port, _ := netSplit(r.Host)
		if port != "" {
			base = scheme + "://localhost:" + port
		} else {
			base = scheme + "://localhost"
		}
	}
	return base + "/_auth/callback"
}

type oidcDiscovery struct {
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
}

// authCallback answers GET /_auth/callback?code&state (§8.6): verify state,
// require the PKCE verifier cookie, exchange the code (one retry on transport
// errors and 5xx only), verify the ID token (aud exactly the client id, nonce
// bound to this login, then domain policy), set the session cookie, redirect
// to next.
func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	if !s.authSvc.BrowserLoginEnabled() {
		plainStatus(w, http.StatusNotImplemented, "browser login is not enabled")
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	next, nonce, ok := s.verifyLoginState(state)
	if !ok {
		plainStatus(w, http.StatusBadRequest, "invalid state")
		return
	}
	if code == "" {
		plainStatus(w, http.StatusBadRequest, "missing code")
		return
	}
	// The PKCE verifier is one-time: consume and clear it before the
	// exchange so a replayed callback cannot reuse it.
	verifier := ""
	if c, cerr := r.Cookie("walgit_pkce"); cerr != nil || c.Value == "" {
		plainStatus(w, http.StatusBadRequest, "missing PKCE verifier")
		return
	} else {
		verifier = c.Value
	}
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_pkce", Value: "", Path: "/_auth/callback",
		MaxAge: -1, HttpOnly: true, Secure: len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
	disc, err := s.authSvc.jwks.discoverDoc(r.Context())
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "issuer discovery failed")
		return
	}
	idToken := s.exchangeCode(r.Context(), disc.TokenEndpoint, code, s.authRedirectURI(r), verifier)
	if idToken == "" {
		plainStatus(w, http.StatusServiceUnavailable, "token exchange failed")
		return
	}
	p, aerr := s.authSvc.verifyIDToken(r.Context(), idToken, true, nonce)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	// The session wire carries the verified EMAIL (existing sessions
	// stay valid; law 5) — the username resolves from it on every
	// request via principalFromEmail.
	sess, merr := s.authSvc.MintSession(emailOf(p))
	if merr != nil {
		plainStatus(w, http.StatusServiceUnavailable, "session mint failed")
		return
	}
	// Forgejo #376: enqueue a background avatar generation for users
	// without one (never blocks the login response — the hook only
	// enqueues; generation + install happen on a per-principal
	// single-flight goroutine in the identity service).
	if s.avatarHook != nil {
		s.avatarHook(p.Name, emailOf(p))
	}
	if isLoopbackHost(hostOnly(r.Host)) {
		// Loopback: bounce through /_auth/claimed with a 60 s signed ticket so
		// the cookie lands on walgit.localhost (different cookie host).
		// The ticket names the verified email (the cookie itself only
		// carries the wire, whose payload re-resolves it).
		ticket, _ := s.signStateTTL(emailOf(p)+"|"+sess.Wire, s.Now(), 60*time.Second)
		ticket = strings.ReplaceAll(ticket, "\n", "") // wire form is url-safe already
		target := requestScheme(r) + "://walgit." + hostOnlyPortSuffix(r.Host) + "/_auth/claimed?ticket=" +
			url.QueryEscape(ticket) + "&next=" + url.QueryEscape(next)
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
		return
	}
	s.setSessionCookie(w, sess)
	w.Header().Set("Location", next)
	w.WriteHeader(http.StatusFound)
}

// authClaimed answers GET /_auth/claimed?ticket= — the loopback hop that sets
// the cookie on walgit.localhost (60 s ticket).
func (s *Server) authClaimed(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	raw, ok := s.verifyStateTicket(ticket)
	if !ok {
		plainStatus(w, http.StatusBadRequest, "invalid ticket")
		return
	}
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		plainStatus(w, http.StatusBadRequest, "invalid ticket")
		return
	}
	sess := SessionToken{Kind: sessionKind, Email: parts[0], Wire: parts[1]}
	s.setSessionCookie(w, sess)
	next := sanitizeNext(r.URL.Query().Get("next"))
	w.Header().Set("Location", next)
	w.WriteHeader(http.StatusFound)
}

// verifyStateTicket reuses the state HMAC for claimed tickets (60 s window
// enforced inside verifyState via the embedded expiry).
func (s *Server) verifyStateTicket(ticket string) (string, bool) {
	next, ok := s.verifyState(ticket)
	if !ok {
		return "", false
	}
	// The payload's third line is the "next" slot carrying "name|wire".
	parts := strings.Split(ticket, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return "", false
	}
	lines := strings.SplitN(string(payload), "\n", 3)
	if len(lines) != 3 {
		return "", false
	}
	return lines[2], ok && next != ""
}

func hostOnlyPortSuffix(host string) string {
	_, port, err := netSplit(host)
	if err != nil || port == "" {
		return "localhost"
	}
	return "localhost:" + port
}

// exchangeCode POSTs the code for tokens (§8.6): one retry on transport
// errors and 5xx only (4xx and malformed bodies fail fast — F6), with a 10 s
// per-attempt timeout so a hung provider cannot stall callback workers.
func (s *Server) exchangeCode(ctx context.Context, tokenEndpoint, code, redirect, verifier string) string {
	a := s.cfg.Server.Auth
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", a.OAuthClientID)
	form.Set("client_secret", a.OAuthClientSecret)
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	try := func() (string, bool) { // token, retryable
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint,
			strings.NewReader(form.Encode()))
		if err != nil {
			return "", false
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			return "", true
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", resp.StatusCode >= 500
		}
		var out struct {
			IDToken string `json:"id_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
			return "", false
		}
		return out.IDToken, false
	}
	tok, retryable := try()
	if tok == "" && retryable { // one retry
		tok, _ = try()
	}
	return tok
}

// authLogout clears the cookie (§8.6).
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
	target := sanitizeNext(r.URL.Query().Get("next"))
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// authTokensPage renders GET /_auth/tokens (session required; §8.6).
func (s *Server) authTokensPage(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil || p.Anonymous {
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		plainStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(tokensPageHTML(s.baseURL(r), p)))
}

// tokensPageHTML is the mint page: explains the token lifecycle (rotating the
// secret revokes everything; nothing can be listed or revoked individually —
// §8.5) and mints via the POST button.
func tokensPageHTML(base string, p auth.Principal) string {
	return `<!doctype html><html><head><title>walgit tokens</title></head><body>
<h1>API token</h1>
<p>Principal: ` + p.Name + `</p>
<p>Tokens are HMAC-signed, valid until expiry, and cannot be listed or
revoked individually — rotating the session secret revokes all of them.</p>
<button onclick="mint()">Mint token</button>
<pre id="out"></pre>
<script>
async function mint(){
  const r = await fetch("` + base + `/_auth/tokens",{method:"POST",credentials:"include"});
  const j = await r.json();
  document.getElementById("out").textContent = r.ok ? j.token : ("error: "+(j.message||r.status));
}
</script></body></html>`
}

// authTokensMint answers POST /_auth/tokens: session required, same-origin
// CSRF guard (Sec-Fetch-Site must be same-origin), returns
// {token, principal, write, expires_at} (no-store) (§8.6).
func (s *Server) authTokensMint(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		plainStatus(w, http.StatusForbidden, "cross-site token mint refused")
		return
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	if p.Anonymous {
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		plainStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Mint against the verified email when the principal carries one
	// (OIDC — the token wire keeps the email, the username resolves
	// from it); else the name (static-token behavior, unchanged).
	tok, merr := s.authSvc.MintToken(emailOf(p))
	if merr != nil {
		plainStatus(w, http.StatusServiceUnavailable, "token mint failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONBody(w, http.StatusOK, map[string]any{
		"token":      tok.Wire,
		"principal":  p.Name,
		"write":      p.Write,
		"expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339),
	})
}
