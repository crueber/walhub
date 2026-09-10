// Package tags implements Forgejo #253: create a lightweight tag at a given
// commit from the UI, server-side via the WAL ref-update path.
//
// POST /{o}/{r}/api/tags (both lanes) with {name, sha, message?} publishes
// one EntryKindRefUpdate transaction creating refs/tags/<name> at sha (CAS
// old-oid zero = create, matching the RefTransaction contract). No new WAL
// kind is introduced (14 §14.11 rule 1): a lightweight tag is a pure ref
// move, expressible as a RefUpdate today.
//
// Design decisions (see docs/go/14_extensibility.md Decisions):
//
//   - Lightweight-only v1: a non-empty message requests an annotated tag,
//     which needs a tag OBJECT the WAL entry kinds don't carry (proto is
//     append-only and frozen). The server answers 422 with a documented
//     message — never a silent lightweight downgrade. Annotated support is
//     the tracked follow-up.
//   - P6 gate is RoleWrite (push-equivalent: pushing a tag via receive-pack
//     requires push permission today), plus an explicit policy.json
//     EvaluateProtect check for (principal, refs/tags/<name>, create) —
//     the merge-task precedent (internal/pulls checkProtectedRef) — so
//     protect rules denying tag creation apply equally to the API path.
//   - Events need no new code: the WAL events bridge (internal/events)
//     derives ref events from REF_UPDATE entries by ref kind, so an
//     API-created tag notifies/webhooks identically to a pushed tag.
//   - Conflict is CAS-decided: create-against-present fails in the publish
//     verify step and maps to 409, race-safe without a check-then-act.
//
// ### Concurrency
//
// Hazard: two creators racing the same tag name, or a push racing the API
// create. Avoidance (13_concurrency.md: CAS loops are the only tool; no
// locks): the publish funnel's verify step arbitrates (old-oid zero fails
// when the ref exists); this package holds no locks and spawns no
// goroutines. Handlers never hold repo locks across store calls; git runs
// go through the bounded pool, never bare on request goroutines.
package tags

import (
	"strings"
)

// Bounds (model shapes live in service.go).
const (
	// MaxTagLen bounds a tag name (same bound as release tags).
	MaxTagLen = 500
)

// Sentinel errors (mapped to plain-text statuses in http.go).
var (
	ErrNotFound     = errNotFound
	ErrInvalid      = errInvalid
	ErrUnauthorized = errUnauthorized
	ErrForbidden    = errForbidden
	ErrConflict     = errConflict
	ErrUnavailable  = errUnavailable
	ErrCorrupt      = errCorrupt
	ErrUnsupported  = errUnsupported
)

// tagError is the package sentinel family (errors.Is-compatible with the
// exported aliases above).
type tagError string

func (e tagError) Error() string { return string(e) }

const (
	errNotFound     tagError = "not found"
	errInvalid      tagError = "invalid tag"
	errUnauthorized tagError = "authentication required"
	errForbidden    tagError = "forbidden"
	errConflict     tagError = "conflict"
	errUnavailable  tagError = "temporarily unavailable"
	errCorrupt      tagError = "corrupt object"
	errUnsupported  tagError = "unsupported"
)

// statusFor maps a service error onto its HTTP status.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case isErr(err, errNotFound):
		return 404
	case isErr(err, errInvalid):
		return 400
	case isErr(err, errUnauthorized):
		return 401
	case isErr(err, errForbidden):
		return 403
	case isErr(err, errConflict):
		return 409
	case isErr(err, errUnavailable):
		return 503
	case isErr(err, errUnsupported):
		return 422
	default:
		return 500
	}
}

func isErr(err error, target tagError) bool {
	return err != nil && (err == error(target) || strings.Contains(err.Error(), string(target)))
}
