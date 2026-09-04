package identity

import (
	"errors"
	"net/http"
)

// Sentinel errors mapped to HTTP statuses (plain-text bodies, 07 §2).
var (
	// ErrConflict maps to 409: CAS version mismatch after bounded retries,
	// duplicate org/team name, last-owner removal, org delete with repos,
	// accept of a non-pending invite.
	ErrConflict = errors.New("conflict")
	// ErrInvalid maps to 400: bad subject/role/visibility/org/slug/email,
	// duplicate binding subject in one PUT.
	ErrInvalid = errors.New("invalid")
	// ErrNotFound maps to 404.
	ErrNotFound = errors.New("not found")
	// ErrForbidden maps to 403: authenticated but insufficient role.
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthorized maps to 401 + WWW-Authenticate: Bearer.
	ErrUnauthorized = errors.New("authentication required")
)

// statusFor maps a sentinel to its HTTP status.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}
