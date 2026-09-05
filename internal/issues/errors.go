package issues

import (
	"errors"
	"net/http"
)

// Sentinel errors mapped to HTTP statuses (plain-text bodies, 07 §2).
var (
	// ErrConflict maps to 409: CAS version mismatch after bounded retries,
	// duplicate label, milestone delete with open issues.
	ErrConflict = errors.New("conflict")
	// ErrInvalid maps to 400: bad title/body/label/color/role/state
	// transitions, unknown request keys, unknown reaction content.
	ErrInvalid = errors.New("invalid")
	// ErrNotFound maps to 404: unknown issue, event, label, milestone.
	ErrNotFound = errors.New("not found")
	// ErrForbidden maps to 403: authenticated but insufficient role.
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthorized maps to 401 + WWW-Authenticate: Bearer.
	ErrUnauthorized = errors.New("authentication required")
	// ErrCorrupt maps to 503: a stored object fails to parse (the repair
	// path, not the client, owns the fix).
	ErrCorrupt = errors.New("store unavailable")
	// ErrTooLarge maps to 413: an attachment upload over
	// attachments.max_image_bytes (§12).
	ErrTooLarge = errors.New("attachment too large")
	// ErrUnsupportedMedia maps to 415: an attachment upload whose
	// magic bytes are not in the PNG/JPEG/GIF/WebP allowlist (§12;
	// SVG is rejected, never sanitized).
	ErrUnsupportedMedia = errors.New("unsupported image type")
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
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnsupportedMedia):
		return http.StatusUnsupportedMediaType
	}
	return http.StatusServiceUnavailable
}
