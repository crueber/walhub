// Package cachepolicy is the single source of truth for the HTTP
// Cache-Control classes (docs/go/07_api.md §4, Forgejo #382). Every
// handler — core (internal/api) and feature (internal/issues,
// internal/social, internal/releases, internal/pulls, internal/identity)
// — aliases these values; nothing redeclares the header strings.
//
// The rule the values encode (the #382 amendment to the §4 "two cache
// classes" framing): addressability does not decide the class —
// MUTABILITY does. SWR's stale-serve window is only safe for content
// whose staleness is bounded by ref movement (git content: refs move
// rarely, seconds-old is fine). Any GET whose resource can change via a
// direct user action (issue/PR threads, social counters, releases, the
// pull view, identity profiles/orgs/teams/invites/access docs, the repo
// summary, the repos/detailed listing, the owner profile) revalidates on
// EVERY read: no-cache still caches in the browser, and the version ETag
// still makes unchanged responses 304 with zero body — only the
// stale-serve window is lost, which is exactly the #259/#280/#381
// flip-flop (refresh 1 painting pre-mutation state while revalidation
// lands, refresh 2 painting post-mutation state). A version-keyed ETag
// gives correct revalidation; only the cache class fixes stale-serve.
//
// Classes:
//
//	Immutable — sha-addressed git content (full 40/64-hex in the rev
//	  position). The bytes can never change at that URL.
//	SWR — ref-dependent git views (name-addressed tree/blob/commits,
//	  resolve, refs, the pull diff): staleness bounded by ref movement,
//	  ETag is the resolved sha (or empty for unversioned listings —
//	  owners, ownerRepos, owners/activity stay SWR per the §4 listings
//	  boundary: membership/activity listings carry no version token and
//	  have no sibling endpoint serving the same data under a different
//	  class, so there is no disagreement hazard; gaining a version ETag
//	  moves them to Mutable).
//	Mutable — user-mutable state with a version ETag (or no ETag for
//	  tokenless collections): revalidate every read, never serve stale.
//	NoStore — mutations, auth-gated single reads with no revalidation
//	  story, SSE/ops surfaces: never store at all.
//	NoCache — bare no-cache for public-informational documents
//	  (GET /api/v1 discovery) that vary by caller but carry no ETag.
//
// Check enforces the machine-checkable half of the rule for tests: a
// response served with a stale-serve window must not carry a
// version-derived ETag.
package cachepolicy

import (
	"errors"
	"strings"
)

// Header values (exact wire strings; 07_api.md §4 pins them byte-for-byte).
const (
	// Immutable is sha-addressed git content.
	Immutable = "private, max-age=31536000, immutable"
	// SWR is ref-dependent git views (staleness bounded by ref movement).
	SWR = "private, max-age=0, stale-while-revalidate=60"
	// Mutable is user-mutable state: revalidate every read, never stale.
	Mutable = "private, no-cache"
	// NoStore is mutations and uncacheable reads.
	NoStore = "no-store"
	// NoCache is bare no-cache for ETag-less informational documents.
	NoCache = "no-cache"
)

// isSHA reports whether e is a full 40/64-hex git sha (the §4
// sha-addressed test): the only ETag shape allowed under SWR.
func isSHA(e string) bool {
	if len(e) != 40 && len(e) != 64 {
		return false
	}
	for i := 0; i < len(e); i++ {
		c := e[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Check enforces the mutability rule on one served (Cache-Control, ETag)
// pair: a response served with a stale-serve window (SWR) must not carry
// a version-derived ETag — the #259 → #280 → #381 recurrence, where a
// version-keyed ETag made revalidation correct while SWR still licensed
// painting the pre-mutation body. Sha ETags (or no ETag, the §4 listings
// boundary) pass; anything else under SWR fails. Non-SWR classes always
// pass: no-cache/no-store/immutable never serve stale by construction,
// whatever the ETag says.
func Check(cc, etag string) error {
	if !strings.Contains(cc, "stale-while-revalidate") {
		return nil
	}
	e := strings.TrimSpace(etag)
	e = strings.TrimPrefix(e, "W/")
	e = strings.TrimPrefix(e, "w/")
	e = strings.Trim(e, `"`)
	if e == "" || isSHA(e) {
		return nil
	}
	return errors.New("cachepolicy: stale-while-revalidate with version-derived ETag " + etag)
}
