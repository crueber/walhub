package checks

import (
	"encoding/base64"
	"net/http"
	"strings"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// This file owns the Seam 2 surface (05 §3, 14_extensibility.md §14.4):
// the compiled-in wct_ credential shape. Because authentication is
// repo-scoped but the auth chain sees only the credential, verification
// splits in two — the chain (or the handler's Auth wrapper) resolves the
// *shape* to an unprivileged ci:<id> principal, and the report handler
// verifies the *secret* against that repo's token record (one conditional
// GET), requires checks:write in scopes, and requires revoked_at == null.
// Mismatch = 401 (the client erases it); valid but revoked = 401; valid
// but no checks:write = 403.

// extractToken pulls the bearer token (case-insensitive Bearer) or Basic
// password (= the token, username informational) from the request. Empty
// when the request carries no credential.
func extractToken(r *http.Request) string {
	cred := r.Header.Get("X-Walgit-Authorization")
	if cred == "" {
		cred = r.Header.Get("Authorization")
	}
	cred = strings.TrimSpace(cred)
	if cred == "" {
		return ""
	}
	if len(cred) >= 7 && strings.EqualFold(cred[:7], "bearer ") {
		return strings.TrimSpace(cred[7:])
	}
	if len(cred) >= 6 && strings.EqualFold(cred[:6], "basic ") {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred[6:]))
		if err != nil {
			return ""
		}
		if i := strings.IndexByte(string(dec), ':'); i >= 0 {
			return string(dec[i+1:])
		}
		return string(dec)
	}
	return ""
}

// CISecretOf returns the CI secret when the request carries a wct_
// credential ("", false otherwise). The report handler passes it to the
// service for the per-repo hash comparison.
func CISecretOf(r *http.Request) (id, secret string, ok bool) {
	tok := extractToken(r)
	if !ClaimToken(tok) {
		return "", "", false
	}
	id, secret, err := ParseCIToken(tok)
	if err != nil {
		return "", "", false
	}
	return id, secret, true
}

// ShapePrincipal resolves the wct_ *shape* to the unprivileged principal
// (no store access — the secret is verified handler-side per repo).
// ok=false when the credential is not a wct_ token (fall through to the
// server chain). A malformed wct_ token is a real 401 (invalid
// credential — the client erases it), never a fall-through: otherwise a
// typo'd CI token would silently authenticate as someone else.
func ShapePrincipal(r *http.Request) (auth.Principal, *auth.AuthError, bool) {
	tok := extractToken(r)
	if !ClaimToken(tok) {
		return auth.Principal{}, nil, false
	}
	id, _, err := ParseCIToken(tok)
	if err != nil {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid CI token"}, true
	}
	return auth.Principal{Name: CIPrincipalName(id)}, nil, true
}

// WrapAuth builds the handler Authenticator for composition: wct_
// credentials resolve through ShapePrincipal (above); everything else
// falls through to the server chain. Nil chain falls back to anonymous
// (tests that exercise pure paths).
func WrapAuth(chain func(r *http.Request) (auth.Principal, *auth.AuthError)) Authenticator {
	return func(r *http.Request) (auth.Principal, *auth.AuthError) {
		if p, aerr, claimed := ShapePrincipal(r); claimed {
			return p, aerr
		}
		if chain != nil {
			return chain(r)
		}
		return auth.Anonymous(), nil
	}
}
