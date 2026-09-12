// username.go — OIDC email → username derivation (Forgejo #370).
//
// The principal name is a username, never a raw email: emails must never
// be exposed to users other than their owner, and the repo-id owner
// segment ([A-Za-z0-9._-], no @) cannot carry one. Derivation is a pure
// function of the verified email — deterministic, so every login,
// session-mint, and wgt_ token resolves the same base without I/O —
// while collision-uniqueness (crueber, crueber2, …) and immutability live
// in the store-backed registry (internal/identity users/<username>/
// user.json, CAS on creation): the AuthService.UsernameResolver hook
// consults it when wired, and falls back to this base otherwise.
package auth

import (
	"strconv"
	"strings"
)

// Reserved usernames: the synthetic principals (Anonymous, None) — a
// derived username must never equal one, or an OIDC user named e.g.
// "anonymous@example.com" would inherit (or collide with) the synthetic
// identity.
var reservedUsernames = map[string]bool{"anonymous": true, "anon": true}

// maxUsernameLen bounds the derived base so numeric collision suffixes
// still fit the owner-segment budget (git validPart allows 100; 64
// leaves room and matches the team-slug scale).
const maxUsernameLen = 64

// ValidUsername reports whether s is a usable username: lowercase
// [a-z0-9._-], 1..64 chars, no leading dot, not ".." — the owner-segment
// subset of the repo-id charset, so every username is a valid ID part
// (git.ParseRepoId accepts "<username>/<repo>").
func ValidUsername(s string) bool {
	if len(s) == 0 || len(s) > maxUsernameLen || s == ".." || s[0] == '.' {
		return false
	}
	for i := range s {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// DeriveUsername maps a verified email to its stable username base:
// the lowercased local part (before the last @) with every character
// outside [a-z0-9._-] folded to '-', leading dots trimmed, truncated to
// 64 chars, falling back to "user" when nothing survives. Reserved names
// gain a "-user" suffix. Pure (no I/O): the same email always derives
// the same base; the registry uniquifies collisions with numeric
// suffixes (base, base2, base3, …).
func DeriveUsername(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	local := email
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	var b strings.Builder
	for i := 0; i < len(local); i++ {
		c := local[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > maxUsernameLen {
		out = out[:maxUsernameLen]
	}
	out = strings.TrimRight(out, ".")
	if out == "" || out == ".." {
		out = "user"
	}
	if reservedUsernames[out] {
		out += "-user"
	}
	return out
}

// Synthetic reports whether name is a synthetic principal
// (anonymous, anon — auth modes' everyone/nobody identities): never a
// real user. Synthetic names are never bound (no user: binding), never
// minted as usernames (DeriveUsername suffixes them), and never
// resolve as usernames. Case-insensitive, like all principal matching.
func Synthetic(name string) bool {
	return reservedUsernames[strings.ToLower(strings.TrimSpace(name))]
}

// WithCollisionSuffix returns the i-th uniquification candidate for
// base: i=0 is the base itself, then base2, base3, … Truncates the base
// so the candidate never exceeds maxUsernameLen.
func WithCollisionSuffix(base string, i int) string {
	if i <= 0 {
		return base
	}
	suf := strconv.Itoa(i + 1)
	if len(base)+len(suf) > maxUsernameLen {
		base = base[:maxUsernameLen-len(suf)]
		base = strings.TrimRight(base, ".")
	}
	return base + suf
}
