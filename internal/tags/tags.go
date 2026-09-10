// Package tags implements Forgejo #253/#263: create a lightweight or
// annotated tag at a given commit from the UI, server-side via the WAL.
//
// POST /{o}/{r}/api/tags (both lanes) with {name, sha, message?} publishes
// one transaction creating refs/tags/<name> (CAS old-oid zero = create,
// matching the RefTransaction contract). An empty/whitespace message takes
// the lightweight path: one EntryKindRefUpdate at the commit. A non-empty
// message takes the annotated path (#263, ruling (b)): the tag object is
// constructed server-side with `git mktag` (strict fsck), packed as a single
// object (`git pack-objects --stdout`), ingested through the existing
// Layer.Ingest path, and published as an EntryKindPush whose txn carries the
// refs/tags/<name> create with NewPeeled set. No new WAL kind is introduced
// (14 §14.11 rule 1): a lightweight tag is a pure ref move and an annotated
// tag is a single-object pack, both expressible on the existing funnel.
//
// Design decisions (see docs/go/14_extensibility.md Decisions):
//
//   - Annotated tags are never silently downgraded: a non-empty message
//     always mints a tag OBJECT (type/tagger/message visible in git); the
//     #253 lightweight-only v1 answered 422 instead.
//   - P6 gate is RoleWrite (push-equivalent: pushing a tag via receive-pack
//     requires push permission today), plus an explicit policy.json
//     EvaluateProtect check for (principal, refs/tags/<name>, create) —
//     the merge-task precedent (internal/pulls checkProtectedRef) — so
//     protect rules denying tag creation apply equally to the API path.
//   - Events need no new code: the WAL events bridge (internal/events)
//     derives ref events from PUSH and REF_UPDATE entries by ref kind, so an
//     API-created tag notifies/webhooks identically to a pushed tag (a test
//     proves the PUSH-shaped create emits the identical tag event).
//   - Conflict is CAS-decided: create-against-present fails in the publish
//     verify step and maps to 409, race-safe without a check-then-act.
//   - Tagger identity is server-rendered from the authed principal
//     (`<name> <<name>@walhub.local>`, wall-clock + local tz); messages are
//     capped at MaxTagMessageLen and normalized to one trailing newline.
//
// ### Concurrency
//
// Hazard: two creators racing the same tag name, or a push racing the API
// create. Avoidance (13_concurrency.md: CAS loops are the only tool; no
// locks): the publish funnel's verify step arbitrates (old-oid zero fails
// when the ref exists); this package holds no locks and spawns no
// goroutines. The three git spawns of one annotated create (resolve, mktag,
// pack-objects) run sequentially within the request (dependent), each
// pool-bounded with request-ctx cancellation. Handlers never hold repo locks
// across store calls; git runs go through the bounded pool, never bare on
// request goroutines.
package tags

import (
	"strings"
)

// Bounds (model shapes live in service.go).
const (
	// MaxTagLen bounds a tag name (same bound as release tags).
	MaxTagLen = 500
	// MaxTagMessageLen bounds an annotated-tag message (#263: tag messages
	// are annotations, not blobs — the resulting single-object pack stays
	// O(KiB), trivially inside server.max_push_bytes).
	MaxTagMessageLen = 64 * 1024
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
