// Package social implements docs/features/07_releases_stars.md §§4–6:
// stars, watches (records + counters), and the fork counter. It registers
// Seam 1 routes on both lanes (Handler.Handle fronts the core mux exactly
// like internal/identity and internal/notify).
//
// Bucket layout (all keys bucket-relative):
//
//	repos/<o>/<r>/meta/social.json            CAS'd counters (§§4–6)
//	users/<principal>/starred/<o>/<r>.json    star record, Create/Delete only (§4)
//	users/<principal>/watching/<o>/<r>.json   watch record: READ here (viewer
//	                                          flags), WRITTEN by internal/notify
//	                                          (06 §6 routes — see Decisions)
//
// social.json is dual-written: internal/notify CASes ONLY the watch fields
// (watcher_list, watchers) while this package CASes ONLY stars/forks — the
// canonical field-scoped loop of 07 §4.1, so interleavings converge with no
// cross-feature lock. Both shapes carry every field; neither writer drops
// the other's.
//
// The WAL stays git-only; core (internal/store, internal/wal,
// internal/git) never imports this package.
//
// ### Concurrency
//
// Hazard: star/unstar/watch/unwatch and 03's fork completion all CAS the
// same social.json. Avoidance: one canonical loop — read, mutate ONLY the
// caller's field, PutUpdate(version), retry on 412 (13_concurrency.md: the
// CAS IS the arbitrator; contention is human-rate, documented per P2
// reasoning). Star counts cannot derive from a list (no reverse index until
// a star index format is decided): star = Create-then-increment,
// unstar = Delete-then-decrement (floor 0); a 412 on the record Create is
// "already starred" (no count change), so concurrent stars converge.
// Watchers stays derived from watcher_list length (06 behavior, unchanged).
// The star resync decision (absent/zero counter + live record ⇒ one +1) is
// evaluated INSIDE the counter CAS loop, never from a pre-read (issue #69).
// Handlers hold no repo locks across store calls (13 §2 rule 4) — the one
// stated exception is the Star shard gate below, which serializes only
// same-shard Star calls (human-rate, bounded hold, never nested, issue #97).
package social

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Bounds and budgets (07 §§4–7).
const (
	// ListDefaultPage is the default starred list page (n default 50, max 100).
	ListDefaultPage = 50
	ListMaxPage     = 100
)

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// Sentinel errors (07 §2: mapped to plain-text statuses in http.go).
var (
	// ErrNotFound marks an unknown star/social object (→ 404).
	ErrNotFound = fmt.Errorf("not found")
	// ErrInvalid marks a bad request body/field (→ 400).
	ErrInvalid = fmt.Errorf("invalid social")
	// ErrUnauthorized marks anonymous-denied access (→ 401 + Bearer).
	ErrUnauthorized = fmt.Errorf("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = fmt.Errorf("forbidden")
	// ErrConflict marks concurrent-mutation exhaustion (→ 409).
	ErrConflict = fmt.Errorf("conflict")
	// ErrCorrupt marks an unreadable bucket object (→ 500-class).
	ErrCorrupt = fmt.Errorf("corrupt object")
)

// statusFor maps a service error onto its HTTP status.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case isErr(err, ErrNotFound):
		return 404
	case isErr(err, ErrInvalid):
		return 400
	case isErr(err, ErrUnauthorized):
		return 401
	case isErr(err, ErrForbidden):
		return 403
	case isErr(err, ErrConflict):
		return 409
	default:
		return 500
	}
}

func isErr(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

// RoleService is the narrow P6 surface this package consumes (same shape as
// internal/issues, internal/pulls, internal/notify, internal/releases:
// satisfied by *identity.Service; tests substitute a fake).
type RoleService interface {
	// Resolve returns the max repo role for p (P6 verbatim).
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	// CheckRead is the require_read gate: nil allows; anonymous-denied
	// reads are ErrUnauthorized (→ real 401), authenticated-but-insufficient
	// are ErrForbidden (→ 403).
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService, injected by composition). Nil falls back to
// anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// Service is the social store client: star/watch records, the CAS'd
// counters, and starred lists. Construct with New; Roles may be nil in
// tests that exercise pure paths (nil Roles falls back to principal
// flags).
type Service struct {
	Store store.ObjectStore
	Roles RoleService
	Now   func() time.Time
	// starShards serializes Star's check-then-act within this process: the
	// star decision spans two keys (the per-principal record and the
	// shared counter), so the counter CAS alone cannot arbitrate a
	// same-principal race in the post-recreate zero window (issue #69).
	// Sharded by (repo, principal) (issue #97): a slow store call stalls
	// at most same-shard Stars, never the whole instance. A zero Service
	// (nil shards) skips mutual exclusion and relies on CAS alone
	// (degraded but safe — service_test pins the path).
	starShards *[starShardCount]sync.Mutex
}

// starShardCount is the stripe count for the Star gate (issue #97): fixed
// and small (no per-key map to grow or GC), so worst-case unrelated
// sharing is 1/64 of Star traffic — Star-only, human-rate — while the
// correctness set (same repo + same principal, whose record and counter
// keys coincide) always lands on one shard.
const starShardCount = 64

// starShard maps a gate key onto its stripe (FNV-1a, stdlib only).
func starShard(key string) uint {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return uint(h.Sum32() % starShardCount)
}

// starGateKey is the sharding key: the exact (repo, principal) pair whose
// record Create and counter bump must be atomic with each other. Different
// principals on one repo share the counter but not the record, and their
// CAS loops already converge (unconditional bump vs conditional resync,
// arbitrated by the store); different repos share nothing.
func starGateKey(owner, repo, principal string) string {
	return owner + "/" + repo + "\x00" + principal
}

// New builds a Service over st.
func New(st store.ObjectStore, roles RoleService) *Service {
	return &Service{Store: st, Roles: roles, Now: time.Now, starShards: &[starShardCount]sync.Mutex{}}
}

// lockStar acquires the Star serialization shard for key and returns the
// release function for defer. Only Star takes this gate (never nested, so
// no ordering hazard); Unstar keeps its version-conditional Delete token
// and all reads stay lock-free.
//
// Hold bound (issue #97): one record GET + one record PUT + one counter
// CAS loop of at most 8 attempts × (GET + PUT), all small control-plane
// JSON on the control-plane transport — no bulk bytes, no git subprocess,
// no LIST. The gate is Star-only at human rate, so even a wedged store
// call stalls only the ~1/64 shard it hashes to.
func (s *Service) lockStar(key string) func() {
	if s.starShards == nil {
		return func() {}
	}
	shard := &s.starShards[starShard(key)]
	shard.Lock()
	return shard.Unlock
}

// nowUTC is the clock, UTC (RFC 3339 wire timestamps).
func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// --- key helpers (bucket-relative; §§4–6) ----------------------------------

// SocialKey returns repos/<o>/<r>/meta/social.json (same object
// internal/notify writes the watch fields of — field-compatible shape).
func SocialKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/social.json"
}

// StarKey returns users/<principal>/starred/<o>/<r>.json.
func StarKey(principal, owner, repo string) string {
	return "users/" + principal + "/starred/" + owner + "/" + repo + ".json"
}

// StarredPrefix returns users/<principal>/starred/ (LIST root, P5).
func StarredPrefix(principal string) string {
	return "users/" + principal + "/starred/"
}

// WatchingKey returns users/<principal>/watching/<o>/<r>.json (07 §5
// shape; written by internal/notify, read here for viewer flags).
func WatchingKey(principal, owner, repo string) string {
	return "users/" + principal + "/watching/" + owner + "/" + repo + ".json"
}

// repoName renders "owner/repo".
func repoName(owner, repo string) string { return owner + "/" + repo }

// manifestKey returns repos/<o>/<r>/manifest.pb — the repo-existence
// signal (the registry deletes it first on Delete and creates it first
// on Create, so absent = deleted-or-never-existed; the registry's own
// listing refresh probes the same key).
func manifestKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/" + store.Manifest
}

// repoAlive reports whether the repo exists. A probe error fails OPEN
// (returns true): a transient store fault must neither mass-hide star
// lists nor fail closed on mutations — the CAS loops surface real
// errors on their own reads.
func (s *Service) repoAlive(ctx context.Context, owner, repo string) bool {
	ok, err := store.Exists(ctx, s.Store, manifestKey(owner, repo))
	if err != nil {
		return true
	}
	return ok
}

// normPrincipal lowercases and trims a principal name for comparison.
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// --- CAS helper (13 §3 canonical loop; field-scoped mutations only) --------

// casUpdate is the canonical CAS loop: read, apply, Update(version),
// retry-on-412 re-read, bounded attempts, then ErrConflict. f receives the
// current body (nil when absent) and version ("" when absent) and returns
// the replacement body, whether to write, and an error. Absent + write ⇒
// PutCreate (social.json is created lazily on first mutation — a repo with
// no social object reports all counts as 0, §6).
func (s *Service) casUpdate(ctx context.Context, key string, attempts int, f func(cur []byte, ver store.Version) ([]byte, bool, error)) (store.ObjectMeta, error) {
	if attempts <= 0 {
		attempts = 8
	}
	for i := 0; i < attempts; i++ {
		cur, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				cur, meta = nil, store.ObjectMeta{}
			} else {
				return store.ObjectMeta{}, err
			}
		}
		next, write, ferr := f(cur, meta.Version)
		if ferr != nil {
			return store.ObjectMeta{}, ferr
		}
		if !write {
			return meta, nil
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		if meta.Version == "" {
			opts.Mode = store.PutCreate
		}
		m, perr := store.PutBytes(ctx, s.Store, key, next, opts)
		if perr == nil {
			return m, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return store.ObjectMeta{}, perr
		}
	}
	return store.ObjectMeta{}, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

// getJSON reads one JSON object; (nil, "", nil) when absent.
func (s *Service) getJSON(ctx context.Context, key string) ([]byte, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return raw, meta.Version, nil
}

// putCreate writes an immutable record; a 412 means it already exists.
func (s *Service) putCreate(ctx context.Context, key string, body []byte) error {
	_, err := store.PutBytes(ctx, s.Store, key, body, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	return err
}

// --- role helpers (P6; read gate only — stars/watches need no minimum
// role beyond authenticated + repo-visible, so no role ladder lives here)

// requireRead enforces the read gate (identity require_read hook when
// wired; fail-closed for anonymous without roles).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// requireAuthenticated rejects anonymous callers (stars/watches need a
// principal to attribute; never anonymous, §4).
func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}
