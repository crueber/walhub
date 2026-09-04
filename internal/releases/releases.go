// Package releases implements docs/features/07_releases_stars.md: releases,
// release assets, and changelog autodraft over the collaboration object
// family. It registers Seam 1 routes on both lanes (Handler.Handle fronts
// the core mux exactly like internal/identity and internal/notify) plus
// one repo-subpath byte route (HandleRepo, consulted by the server's
// repoDispatch fallback — the static uncompressed group per the 14.3
// routing note).
//
// Bucket layout (all keys bucket-relative):
//
//	repos/<o>/<r>/releases/<tag.json>              CAS'd release header (§1.1;
//	                                               tag percent-encoded, NO lowercasing)
//	repos/<o>/<r>/releases/assets/<tag>/<name>     immutable asset bytes, Create-only (§1.2)
//	repos/<o>/<r>/releases/latest.json             CAS'd latest pointer (§2, monotonic + self-healing)
//
// Asset bytes live in their own subtree — NEVER nested under the header
// file (`releases/<tag.json>/assets/…` would make the header path both a
// file and a directory, which the filesystem backend cannot store and no
// other family does; keys stay prefix-free between files and dirs on every
// backend). The HTTP byte route is unchanged (URL shape ≠ bucket key).
//
// The WAL stays git-only: tags are ordinary WAL ref state resolved read-only
// through the RepoDirs/Git seams (never published here); core
// (internal/store, internal/wal, internal/git) never imports this package.
// Publish fan-out rides the nil-safe Emitter/Streamer seams (P8),
// bound by composition onto internal/notify (EmitRelease/PublishStream).
//
// ### Concurrency
//
// Hazard: two writers CAS the same release header (edit + asset attach) and
// two asset uploads of the same name racing the byte Create. Avoidance
// (13_concurrency.md: CAS loops are the only tool; no locks): every header
// mutation is a read-modify-write PutUpdate(version) loop; asset attach
// re-reads, appends, retries on PreconditionFailed; byte Create arbitrates
// name races at the store (same-sha = idempotent success, clash = 409, and
// the orphan-header case verifies the stored bytes on the failure path).
// Hazard: last-writer-wins parking the latest pointer at an older release.
// Avoidance: the created_at compare inside the CAS loop makes the pointer
// monotonic; a stale/dangling pointer self-heals on the next read via the
// bounded scan. Hazard: autodraft's per-PR merge-base subprocesses.
// Avoidance: bounded (≤ 100 candidates × 2 short argv runs) under the
// package git pool (13 §4 shape); handlers never hold repo locks across
// store calls (13 §2 rule 4); no new locks are introduced.
package releases

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Bounds and budgets (07 §§1-3, 6-7).
const (
	// DefaultMaxAssetBytes caps one asset upload when unconfigured
	// (releases.max_asset_bytes, default 2 GiB → 413 over cap).
	DefaultMaxAssetBytes = int64(2 << 30)
	// ListDefaultPage is the default release/starred list page (n default 50, max 100).
	ListDefaultPage = 50
	ListMaxPage     = 100
	// ScanHeaderCap bounds one latest-repair/list scan (P5: collaboration
	// page scans are permitted but paginated and named).
	ScanHeaderCap = 100
	// ListScanCap bounds the header GETs behind one list page.
	ListScanCap = 1000
	// MaxAutodraftPRs caps the merged-PR candidates examined per autodraft
	// (output capped at the same bound; more=true beyond).
	MaxAutodraftPRs = 100
	// MaxBodyBytes bounds a release body (same bound as issue bodies).
	MaxBodyBytes = 64 << 10
	// MaxNameLen bounds a release display name.
	MaxNameLen = 256
	// MaxAssetNameLen bounds an asset name (1–200 bytes, single segment).
	MaxAssetNameLen = 200
	// MaxContentTypeLen bounds a stored asset content type.
	MaxContentTypeLen = 200
)

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// Sentinel errors (07 §2: mapped to plain-text statuses in http.go).
var (
	// ErrNotFound marks an unknown release/tag/asset (→ 404).
	ErrNotFound = fmt.Errorf("not found")
	// ErrInvalid marks a bad request body/field (→ 400).
	ErrInvalid = fmt.Errorf("invalid release")
	// ErrUnauthorized marks anonymous-denied access (→ 401 + Bearer).
	ErrUnauthorized = fmt.Errorf("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = fmt.Errorf("forbidden")
	// ErrConflict marks state conflicts: asset sha clash (→ 409).
	ErrConflict = fmt.Errorf("conflict")
	// ErrTooLarge marks an over-cap asset upload (→ 413).
	ErrTooLarge = fmt.Errorf("asset too large")
	// ErrUnavailable marks a down dependency (git pool, store) (→ 503).
	ErrUnavailable = fmt.Errorf("temporarily unavailable")
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
	case isErr(err, ErrTooLarge):
		return 413
	case isErr(err, ErrUnavailable):
		return 503
	default:
		return 500
	}
}

func isErr(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

// RoleService is the narrow P6 surface this package consumes (same shape as
// internal/issues and internal/notify: satisfied by *identity.Service;
// tests substitute a fake). Resolution order and require_read semantics
// stay owned by 01.
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

// NotifyEvent is the §3/P8 emission contract with 06 (internal/notify):
// release publications fan out synchronously after the CAS commits.
// Class is always "release_published".
type NotifyEvent struct {
	Repo  string `json:"repo"` // "owner/name"
	Tag   string `json:"tag"`
	Actor string `json:"actor"`
	At    string `json:"at"` // RFC 3339 UTC
}

// Emitter delivers NotifyEvents synchronously in the publishing handler
// (P8). Nil-safe; composition wires the real fan-out.
type Emitter func(ctx context.Context, ev NotifyEvent)

// StreamEvent is the live-update contract for the repo SSE stream (§7):
// event name "release" (data carries the action) on the single
// collaboration stream; no new stream is opened.
type StreamEvent struct {
	Name   string `json:"name"` // always "release"
	Repo   string `json:"repo"`
	Action string `json:"action"` // published|edited|deleted
	Tag    string `json:"tag"`
}

// Streamer appends StreamEvents to the repo's SSE stream. Nil-safe;
// composition wires the real stream.
type Streamer func(ctx context.Context, ev StreamEvent)

// Service is the releases store client: headers, asset bytes, the latest
// pointer, and autodraft. Construct with New; Roles/Git/Dirs may be nil in
// tests that exercise pure paths (nil Git/Dirs makes tag-touching ops
// 503; nil Roles falls back to principal flags).
type Service struct {
	Store store.ObjectStore
	Roles RoleService
	// Git runs tag resolution and merge-base ancestry probes (nil in
	// pure-bucket tests; production wires SubprocessGit).
	Git GitRunner
	// Dirs resolves a repo to its synced local git dir (nil with Git).
	Dirs RepoDirs
	Now  func() time.Time

	// MaxAssetBytes caps one asset upload (0 = DefaultMaxAssetBytes).
	// Composition sets it from releases.max_asset_bytes.
	MaxAssetBytes int64
	// SpoolDir stages verified-then-written asset bytes (LFS §6.2
	// pattern: never buffer the upload in memory). Empty = os.TempDir.
	SpoolDir string

	// Notify receives §3 fan-out synchronously after each publish commit
	// (nil = no-op until internal/notify lands).
	Notify Emitter
	// Stream receives release live updates (nil = no-op).
	Stream Streamer
}

// New builds a Service over st.
func New(st store.ObjectStore, roles RoleService) *Service {
	return &Service{Store: st, Roles: roles, Now: time.Now}
}

// nowUTC is the clock, UTC (RFC 3339 wire timestamps).
func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// maxAssetBytes resolves the upload cap.
func (s *Service) maxAssetBytes() int64 {
	if s.MaxAssetBytes > 0 {
		return s.MaxAssetBytes
	}
	return DefaultMaxAssetBytes
}

// --- key helpers (bucket-relative; §1) --------------------------------------

// ReleaseKey returns repos/<o>/<r>/releases/<tag.json> (tag
// percent-encoded into one key segment, NO lowercasing — the encoding is
// the only transformation).
func ReleaseKey(owner, repo, tag string) string {
	return "repos/" + owner + "/" + repo + "/releases/" + encodeTag(tag) + ".json"
}

// ReleasesPrefix returns repos/<o>/<r>/releases/ (LIST root, P5).
func ReleasesPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/releases/"
}

// AssetKey returns repos/<o>/<r>/releases/assets/<tag>/<name> (the
// assets subtree; disjoint from every header path by construction).
func AssetKey(owner, repo, tag, name string) string {
	return "repos/" + owner + "/" + repo + "/releases/assets/" + encodeTag(tag) + "/" + name
}

// AssetPrefix returns the per-release asset LIST root.
func AssetPrefix(owner, repo, tag string) string {
	return "repos/" + owner + "/" + repo + "/releases/assets/" + encodeTag(tag) + "/"
}

// LatestKey returns repos/<o>/<r>/releases/latest.json.
func LatestKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/releases/latest.json"
}

// repoName renders "owner/repo".
func repoName(owner, repo string) string { return owner + "/" + repo }

// normPrincipal lowercases and trims a principal name for comparison.
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// --- CAS helpers (13 §3 canonical loops) ------------------------------------

// casUpdate is the canonical CAS loop: read, apply, Update(version),
// retry-on-412 re-read, bounded attempts, then ErrConflict. f receives the
// current body (nil when absent) and version ("" when absent) and returns
// the replacement body, whether to write, and an error (validation
// failures abort without writing). Absent + write ⇒ PutCreate (the
// create-against-absent primitive behind release creation).
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

// --- role helpers (P6; same shape as internal/issues) -----------------------

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// roleOf resolves the actor's repo role ("" when none). Host admin/write
// flags short-circuit through identity's own resolution.
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	got := s.roleOf(ctx, owner, repo, p)
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

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

// emit fans out one NotifyEvent synchronously (P8), nil-safe.
func (s *Service) emit(ctx context.Context, ev NotifyEvent) {
	if s.Notify == nil {
		return
	}
	if ev.At == "" {
		ev.At = s.nowUTC().Format(time.RFC3339)
	}
	s.Notify(ctx, ev)
}

// stream appends one live-update event, nil-safe.
func (s *Service) stream(ctx context.Context, ev StreamEvent) {
	if s.Stream == nil {
		return
	}
	s.Stream(ctx, ev)
}
