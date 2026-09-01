package server

import (
	"context"
	"crypto/hmac"
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
// session secret (the state HMAC is the anti-forgery; the nonce is carried
// but not verified, §8.6).
func (s *Server) signState(next string, now time.Time) string {
	nonce := randHex(16)
	payload := fmt.Sprintf("%d\n%s\n%s", now.Add(600*time.Second).Unix(), nonce, next)
	mac := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), []byte(payload))
	return b64url([]byte(payload)) + "." + b64url(mac)
}

// verifyState checks the HMAC and the 600 s window; returns the sanitized next.
func (s *Server) verifyState(state string) (string, bool) {
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := b64urlDecode(parts[0])
	if err != nil {
		return "", false
	}
	mac, err := b64urlDecode(parts[1])
	if err != nil {
		return "", false
	}
	want := hmacSHA256([]byte(s.cfg.Server.Auth.SessionSecret), payload)
	if !hmac.Equal(mac, want) {
		return "", false
	}
	lines := strings.SplitN(string(payload), "\n", 3)
	if len(lines) != 3 {
		return "", false
	}
	exp, err1 := parseUnix(lines[0])
	if err1 != nil || s.Now().After(exp) {
		return "", false
	}
	next := sanitizeNext(lines[2])
	return next, true
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
// scope=openid email, prompt=select_account, &hd= = first allowed domain; no
// PKCE.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authSvc.BrowserLoginEnabled() {
		plainStatus(w, http.StatusNotImplemented, "browser login is not enabled")
		return
	}
	next := sanitizeNext(r.URL.Query().Get("next"))
	disc, err := s.authSvc.jwks.discoverDoc(r.Context())
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "issuer discovery failed")
		return
	}
	state := s.signState(next, s.Now())
	redirect := s.authRedirectURI(r)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("scope", "openid email")
	q.Set("prompt", "select_account")
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

// authRedirectURI is {public_url}/_auth/callback; loopback origins use
// http(s)://localhost[:port] (§8.6 — the claimed-ticket hop then sets the
// cookie on walgit.localhost, a different cookie host).
func (s *Server) authRedirectURI(r *http.Request) string {
	base := s.baseURL(r)
	if isLoopbackHost(hostOnly(r.Host)) {
		scheme := "http"
		if s.tlsOn || r.TLS != nil {
			scheme = "https"
		}
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
// exchange the code (one retry), verify the ID token (aud exactly the client
// id, then domain policy), set the session cookie, redirect to next.
func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	if !s.authSvc.BrowserLoginEnabled() {
		plainStatus(w, http.StatusNotImplemented, "browser login is not enabled")
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	next, ok := s.verifyState(state)
	if !ok {
		plainStatus(w, http.StatusBadRequest, "invalid state")
		return
	}
	if code == "" {
		plainStatus(w, http.StatusBadRequest, "missing code")
		return
	}
	disc, err := s.authSvc.jwks.discoverDoc(r.Context())
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "issuer discovery failed")
		return
	}
	idToken := s.exchangeCode(r.Context(), disc.TokenEndpoint, code, s.authRedirectURI(r))
	if idToken == "" {
		plainStatus(w, http.StatusServiceUnavailable, "token exchange failed")
		return
	}
	p, aerr := s.authSvc.verifyIDToken(r.Context(), idToken, true)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	sess, merr := s.authSvc.MintSession(p.Name)
	if merr != nil {
		plainStatus(w, http.StatusServiceUnavailable, "session mint failed")
		return
	}
	if isLoopbackHost(hostOnly(r.Host)) {
		// Loopback: bounce through /_auth/claimed with a 60 s signed ticket so
		// the cookie lands on walgit.localhost (different cookie host).
		ticket := s.signState(p.Name+"|"+sess.Wire, s.Now())
		ticket = strings.ReplaceAll(ticket, "\n", "") // wire form is url-safe already
		target := s.scheme() + "://walgit." + hostOnlyPortSuffix(r.Host) + "/_auth/claimed?ticket=" +
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

func (s *Server) scheme() string {
	if s.tlsOn {
		return "https"
	}
	return "http"
}

func hostOnlyPortSuffix(host string) string {
	_, port, err := netSplit(host)
	if err != nil || port == "" {
		return "localhost"
	}
	return "localhost:" + port
}

// exchangeCode POSTs the code for tokens (one retry, §8.6).
func (s *Server) exchangeCode(ctx context.Context, tokenEndpoint, code, redirect string) string {
	a := s.cfg.Server.Auth
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", a.OAuthClientID)
	form.Set("client_secret", a.OAuthClientSecret)
	try := func() (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint,
			strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("token endpoint status %d", resp.StatusCode)
		}
		var out struct {
			IDToken string `json:"id_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
			return "", err
		}
		return out.IDToken, nil
	}
	tok, err := try()
	if err != nil { // one retry
		tok, err = try()
	}
	return tok
}

// authLogout clears the cookie (§8.6).
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: "walgit_session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.tlsOn || len(s.cfg.Server.CorsOrigins) > 0,
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
	tok, merr := s.authSvc.MintToken(p.Name)
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
