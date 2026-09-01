// Package auth: shared identity types (06_server_http.md §8.8). The server owner
// implements the full auth chain in sibling files; server/api handlers call these helpers.
package auth

// Principal is the authenticated identity. Admin (repo delete, PUT/DELETE settings+policy)
// is INDEPENDENT of write (push + create).
type Principal struct {
	Name      string
	Write     bool
	Admin     bool
	Anonymous bool
}

// Anonymous is the unauthenticated principal (name "anonymous", no write, no admin).
func Anonymous() Principal { return Principal{Name: "anonymous", Anonymous: true} }

// None is the auth-mode-none principal: everyone is anon with write+admin.
func None() Principal { return Principal{Name: "anon", Write: true, Admin: true} }

// AuthError maps to HTTP: Invalid/Unauthorized → 401 (+WWW-Authenticate Bearer),
// Forbidden → 403, Unavailable → 503 (+Retry-After: 15).
type AuthError struct {
	Kind AuthErrorKind
	Why  string
}

type AuthErrorKind int

const (
	ErrUnauthorized AuthErrorKind = iota
	ErrInvalid
	ErrForbidden
	ErrUnavailable
)

func (e *AuthError) Error() string { return e.Why }

// Require helpers (doc 06 §8.8): implemented by the server owner; contract shapes here.
type Chain struct{}

// RequireRead: anonymous && !anonymous_read → ErrUnauthorized.
func (c *Chain) RequireRead(p Principal) *AuthError { panic("unimplemented") }

// RequireWrite: !p.Write → ErrForbidden.
func (c *Chain) RequireWrite(p Principal) *AuthError { panic("unimplemented") }

// RequireAdmin: !p.Admin → ErrForbidden.
func (c *Chain) RequireAdmin(p Principal) *AuthError { panic("unimplemented") }
