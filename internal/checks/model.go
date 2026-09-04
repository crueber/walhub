package checks

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"git.packden.us/crueber/walhub/internal/policy"
)

// Sentinel errors (07 §2: mapped to plain-text statuses in http.go).
var (
	// ErrNotFound marks an unknown sha/thread/token (→ 404).
	ErrNotFound = errors.New("unknown check")
	// ErrInvalid marks a bad request body/field (→ 400).
	ErrInvalid = errors.New("invalid check")
	// ErrUnauthorized marks anonymous-denied access and bad/revoked CI
	// credentials (→ 401 + Bearer; git-style: the client erases it).
	ErrUnauthorized = errors.New("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = errors.New("forbidden")
	// ErrConflict marks state conflicts: stale CAS, duplicate token id
	// (→ 409).
	ErrConflict = errors.New("conflict")
	// ErrUnprocessable marks a well-formed but unusable sha (not a commit)
	// (→ 422).
	ErrUnprocessable = errors.New("unprocessable")
	// ErrUnavailable marks a down dependency (git pool, store, identity)
	// (→ 503 + Retry-After: 15).
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrCorrupt marks an unreadable bucket object (→ 500-class).
	ErrCorrupt = errors.New("corrupt object")
)

// statusFor maps a service error onto its HTTP status.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrInvalid):
		return 400
	case errors.Is(err, ErrUnauthorized):
		return 401
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrConflict):
		return 409
	case errors.Is(err, ErrUnprocessable):
		return 422
	case errors.Is(err, ErrInvalidState):
		return 409
	case errors.Is(err, ErrUnavailable):
		return 503
	default:
		return 500
	}
}

// Status states (the v1 enum; 409 on anything else per the §4 table).
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// NotifyClass is the 06 §5.3 activity-log action this package emits
// (transitions into failure or error ONLY).
const NotifyClass = "check_reported"

// StreamName is the repo collaboration SSE stream event name (§7).
const StreamName = "check"

// StatusDoc is one checks/<sha>/<context>.json record (§2 schema).
type StatusDoc struct {
	SHA         string  `json:"sha"`
	Context     string  `json:"context"`
	State       string  `json:"state"`
	TargetURL   string  `json:"target_url,omitempty"`
	Description string  `json:"description,omitempty"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
	Creator     string  `json:"creator"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	Version     int     `json:"version"`
}

// IndexContext is one context row of a sha entry in checks/index.json.
type IndexContext struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
}

// IndexSHA is one sha entry of checks/index.json (newest-sha first).
type IndexSHA struct {
	SHA       string         `json:"sha"`
	State     string         `json:"state"`
	Contexts  []IndexContext `json:"contexts"`
	UpdatedAt string         `json:"updated_at"`
}

// IndexDoc is the checks/index.json projection (§2, P4 discipline): a
// newest-first hot window capped at 256 KiB / 500 shas. It is a
// projection — the table page and the PR head-sha fast path read it; the
// combined view always reads the canonical per-context objects.
type IndexDoc struct {
	CompactedThrough string     `json:"compacted_through"`
	SHAs             []IndexSHA `json:"shas"`
	Version          int        `json:"version"`
}

// CITokenDoc is one meta/ci_tokens/<id>.json record (§3 schema). Only
// token_hash is stored; the secret is shown once at creation.
type CITokenDoc struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	TokenHash string   `json:"token_hash"`
	Scopes    []string `json:"scopes"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	RevokedAt *string  `json:"revoked_at,omitempty"`
	Version   int      `json:"version"`
}

// CITokenScope is the only v1 capability; the field exists for growth
// (granting repo-write or admin via a CI token is explicitly rejected).
const CITokenScope = "checks:write"

// shaRe pins full commit shas (40/64 lowercase hex — stored lowercase;
// input is lowercased before validation).
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// normalizeSHA lowercases and validates a full commit sha.
func normalizeSHA(sha string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(sha))
	if !shaRe.MatchString(lower) {
		return "", fmt.Errorf("%w: sha must be a full 40/64-hex commit sha, got %q", ErrInvalid, sha)
	}
	return lower, nil
}

// validState reports the v1 state enum.
func validState(state string) bool {
	switch state {
	case StatePending, StateSuccess, StateFailure, StateError:
		return true
	}
	return false
}

// ValidContext validates one CI context name — the same grammar as
// policy.ValidCheckContext (one rule in both places; policy owns the
// canonical implementation so the gate and the writer can never drift).
func ValidContext(c string) error {
	return policy.ValidCheckContext(c)
}

// validTargetURL validates the optional target_url (≤ 2 KiB, absolute
// http(s)).
func validTargetURL(u string) error {
	if u == "" {
		return nil
	}
	if len(u) > 2048 {
		return fmt.Errorf("%w: target_url must be ≤ 2 KiB", ErrInvalid)
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("%w: target_url must be absolute http(s), got %q", ErrInvalid, u)
	}
	return nil
}

// validDescription validates the optional description (≤ 256 chars).
func validDescription(d string) error {
	if len([]rune(d)) > 256 {
		return fmt.Errorf("%w: description must be ≤ 256 chars", ErrInvalid)
	}
	return nil
}

// combinedState aggregates worst-of over contexts: error > failure >
// pending > success. Zero contexts ⇒ pending (a caller cannot
// distinguish "not started" from "in flight" — exactly what a gate
// needs).
func combinedState(states []string) string {
	worst := StateSuccess
	rank := map[string]int{StateSuccess: 0, StatePending: 1, StateFailure: 2, StateError: 3}
	for _, st := range states {
		if rank[st] > rank[worst] {
			worst = st
		}
	}
	if len(states) == 0 {
		return StatePending
	}
	return worst
}

// --- CI tokens (§3) ----------------------------------------------------------

// TokenPrefix is the CI-token wire prefix. It keeps the wgt_ HMAC-token
// prefix convention distinct (D-NAME-1 keeper family); startup validates
// no overlap (see AssertPrefixDisjoint, called from composition).
const TokenPrefix = "wct_"

// tokenIDRe pins token ids: 8 chars [a-z0-9].
var tokenIDRe = regexp.MustCompile(`^[a-z0-9]{8}$`)

// ParseCIToken splits wct_<id>.<secret> into its parts. Malformed input
// is a 401-class error (the client erases it), never a 400: to every
// other auth step this credential is simply invalid.
func ParseCIToken(wire string) (id, secret string, err error) {
	if !strings.HasPrefix(wire, TokenPrefix) {
		return "", "", fmt.Errorf("%w: not a CI token", ErrUnauthorized)
	}
	rest := strings.TrimPrefix(wire, TokenPrefix)
	id, secret, ok := strings.Cut(rest, ".")
	if !ok || !tokenIDRe.MatchString(id) || secret == "" {
		return "", "", fmt.Errorf("%w: malformed CI token", ErrUnauthorized)
	}
	return id, secret, nil
}

// ClaimToken reports whether wire claims the wct_ prefix (the Seam 2
// Claim shape test — cheap, no I/O). Startup validates this prefix
// overlaps no other provider's (AssertPrefixDisjoint).
func ClaimToken(wire string) bool {
	return strings.HasPrefix(wire, TokenPrefix)
}

// AssertPrefixDisjoint panics when the CI-token prefix overlaps another
// credential prefix (Seam 2 startup rule: two providers MUST NOT claim
// overlapping prefixes). Composition calls it once at startup.
func AssertPrefixDisjoint(others ...string) {
	for _, o := range others {
		if o == "" {
			continue
		}
		if strings.HasPrefix(TokenPrefix, o) || strings.HasPrefix(o, TokenPrefix) {
			panic(fmt.Sprintf("checks: CI token prefix %q overlaps credential prefix %q", TokenPrefix, o))
		}
	}
}

// CIPrincipalName renders the unprivileged principal name for a token id.
// The frozen Principal is NOT extended: {name: "ci:<id>", write: false,
// admin: false} — the scoped capability is checked handler-side.
func CIPrincipalName(id string) string { return "ci:" + id }

// IsCIPrincipal reports a ci:<id> principal name.
func IsCIPrincipal(name string) (string, bool) {
	id, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(name)), "ci:")
	if !ok || !tokenIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// hashSecret hex-encodes sha-256(secret) — the only stored form.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// verifySecret compares a candidate secret against a stored hash in
// constant time.
func verifySecret(candidate, storedHash string) bool {
	cand := hashSecret(candidate)
	if len(cand) != len(storedHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cand), []byte(storedHash)) == 1
}

// mintTokenID mints a random 8-char [a-z0-9] token id.
func mintTokenID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// mintSecret mints a random 32-byte hex secret (shown once).
func mintSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// nonNilStr normalizes a nil slice to [] (wire rule: [] never null).
func nonNilStr(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
