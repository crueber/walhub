// Package checks implements docs/features/05_checks_statuses.md: commit
// statuses (external CI posts per-commit results), the CAS'd checks index,
// CI tokens (the checks:write credential class), the combined worst-of
// view, and the merge-time half of the require_checks policy extension
// (consulted by 03's merge task through pulls' ChecksGate seam — the merge
// logic is NOT forked). v1 is statuses-only; check runs/suites are
// deferred (§1 seam note, no migration needed later).
//
// It registers one Seam 1 surface (Handler.Handle fronts the core mux on
// both lanes, exactly like internal/identity, internal/issues,
// internal/pulls, and internal/review), one Seam 2 credential shape (the
// wct_ prefix, resolved to an unprivileged ci:<id> principal with the
// scoped capability checked handler-side — the frozen Principal is NOT
// extended), zero new policy effects (§6 extends the existing protect
// effect with require_checks), and one task kind (checks-index-compact,
// Seam 5). It depends only on seam interfaces plus frozen types — never on
// another feature package's internals.
//
// Bucket layout (all keys bucket-relative; the store prefix is applied by
// the store layer):
//
//	repos/<o>/<r>/checks/<sha>/<context>.json  status record (Create first, CAS Update after)
//	repos/<o>/<r>/checks/index.json            CAS'd hot-window projection (P4)
//	repos/<o>/<r>/meta/ci_tokens/<id>.json     CI token record (CAS'd; hash only, revoked retained)
//
// The WAL stays git-only: statuses never produce WAL entries and never
// gate a push (the 14.5 honest note); the merge task consults stored
// results only. Core (internal/store, internal/wal, internal/git) never
// imports this package.
//
// ### Concurrency
//
// Hazard: two reports racing for the same (sha, context) CAS the same
// object. Avoidance: by construction this is contention-free — the v1
// contract is one CI system per context (last write wins, no corruption).
// The index CAS is the only multi-writer point: a plain CAS retry loop
// (13_concurrency.md §3 pattern — the CAS *is* the lock; no new repo
// lock, no cross-feature lock). A lost index update (writer died between
// the status write and the index CAS) costs one stale table row until the
// next report — the per-context objects are the backfill truth (P3/P8
// contract). No new locks: this family never touches syncMu/packMu/rw
// (13_concurrency.md §2). Hazard: a report storm stampeding the index
// CAS and blocking reads. Avoidance: two short bucket ops with bounded
// CAS retries (≤ 5, then 503); reads are conditional GETs — no locks, no
// goroutines, no singleflight (a CAS retry loop IS the single-flight
// here). Reports are CI-rate, never git-hot-path; per 13_concurrency.md
// §2 rule 4 nothing here runs under a repo lock.
package checks

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

// TaskKindChecksIndexCompact is the §2 task kind: CAS-compaction of
// checks/index.json past ~256 KiB. (repo, kind) single-flight per
// 13_concurrency.md §3; SSE-attachable. No other task kinds: reports are
// short synchronous writes (P8).
const TaskKindChecksIndexCompact = "checks-index-compact"

// IndexSizeLimit is the ~256 KiB compaction trigger (P4): the report
// handler checks the bytes it just wrote and compacts inline when over.
const IndexSizeLimit = 256 << 10

// IndexHotWindow caps the index projection at the newest 500 shas (P4 hot
// window; older shas are served by LIST over checks/<sha>/ prefixes).
const IndexHotWindow = 500

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// RoleService is the narrow P6 surface this package consumes. It is
// satisfied by *identity.Service; tests substitute a fake. Resolution
// order (access.json → org ownership → principal flags → anonymous) and
// the require_read hook semantics stay owned by 01.
type RoleService interface {
	// Resolve returns the max repo role for p (P6 verbatim).
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	// CheckRead is the require_read gate: nil allows; anonymous-denied
	// reads are ErrUnauthorized (→ real 401), authenticated-but-insufficient
	// are ErrForbidden (→ 403).
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService plus the wct_ shape hook, injected by
// composition). A nil Authenticator falls back to anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// CommitChecker resolves a sha to a commit object (05 §2 step 1: an
// unknown sha is 404, never stored). Satisfied in composition by the
// synced-dir adapter (git rev-parse --verify --quiet <sha>^{commit});
// tests substitute a fake. Nil ⇒ 503 (git backend not wired).
type CommitChecker interface {
	// ResolveCommit resolves sha to its commit sha. Unknown or
	// non-commit ⇒ error.
	ResolveCommit(ctx context.Context, repo, sha string) (string, error)
}

// NotifyEvent is the §8 emission contract with 06 (internal/notify): per
// P8 the report handler enqueues synchronously after the CAS commits, for
// transitions into failure or error ONLY, and only where sha is the head
// of an open PR. Class is "check_reported" (06 §5.3 enum); PR
// participants get reason "subscribed" (fan-out is 06's job — this
// handler only enqueues the event).
type NotifyEvent struct {
	Repo        string `json:"repo"` // "owner/name"
	Class       string `json:"class"`
	Actor       string `json:"actor"`
	SHA         string `json:"sha"`
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
	PR          int    `json:"pr,omitempty"`
	At          string `json:"at"` // RFC 3339 UTC
}

// StreamEvent is the live-update contract for the repo collaboration SSE
// stream (§7, event name "check" — the same stream 03's pull events ride;
// there is NO checks-specific endpoint). The PR page keeps frames whose
// sha equals the PR's current head sha.
type StreamEvent struct {
	Name          string `json:"name"` // always "check"
	Repo          string `json:"repo"`
	SHA           string `json:"sha"`
	Context       string `json:"context"`
	State         string `json:"state"`
	CombinedState string `json:"combined_state"`
	UpdatedAt     string `json:"updated_at"`
}

// Emitter delivers NotifyEvents synchronously in the report handler (P8:
// a crash after CAS but before fan-out loses one notification, never
// data; the per-context objects are the backfill source of truth).
//
// WAVE 05 NOTE: internal/notify does not exist yet, so composition wires
// no durable fan-out; the default (nil) emitter is a documented no-op and
// the emission points are all in one place (Service.emit) for 06 to bind
// to.
type Emitter func(ctx context.Context, ev NotifyEvent)

// Streamer appends StreamEvents to the repo's SSE stream. Nil-safe;
// composition wires the real stream when the per-repo bus accepts
// feature event names.
type Streamer func(ctx context.Context, ev StreamEvent)

// Service is the checks store client: status records, the CAS'd index, CI
// tokens, the combined view, and the require_checks merge-time gate.
// Construct with New; Roles/Commits may be nil in tests that exercise
// pure parsers (nil Commits makes sha-touching ops 503).
type Service struct {
	Store   store.ObjectStore
	Roles   RoleService
	Commits CommitChecker
	Now     func() time.Time

	// Notify receives §8 fan-out synchronously after each committed
	// failure/error report (nil = no-op until internal/notify lands).
	Notify Emitter
	// Stream receives check live updates (nil = no-op).
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

// --- key helpers (bucket-relative; §2) --------------------------------------

// StatusKey returns repos/<o>/<r>/checks/<sha>/<context>.json. Contexts
// containing "/" simply extend the key — LIST by the ChecksPrefix still
// groups them.
func StatusKey(owner, repo, sha, context string) string {
	return "repos/" + owner + "/" + repo + "/checks/" + sha + "/" + context + ".json"
}

// ChecksPrefix returns repos/<o>/<r>/checks/<sha>/ (per-sha LIST root).
func ChecksPrefix(owner, repo, sha string) string {
	return "repos/" + owner + "/" + repo + "/checks/" + sha + "/"
}

// IndexKey returns repos/<o>/<r>/checks/index.json.
func IndexKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/checks/index.json"
}

// TokenKey returns repos/<o>/<r>/meta/ci_tokens/<id>.json.
func TokenKey(owner, repo, id string) string {
	return "repos/" + owner + "/" + repo + "/meta/ci_tokens/" + id + ".json"
}

// TokensPrefix returns repos/<o>/<r>/meta/ci_tokens/ (admin LIST root, P5).
func TokensPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/ci_tokens/"
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
// failures abort without writing).
func (s *Service) casUpdate(ctx context.Context, key string, attempts int, f func(cur []byte, ver store.Version) ([]byte, bool, error)) (store.ObjectMeta, error) {
	if attempts <= 0 {
		attempts = 5
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

// putCreate writes an immutable object; a 412 means it already exists.
func (s *Service) putCreate(ctx context.Context, key string, body []byte) error {
	_, err := store.PutBytes(ctx, s.Store, key, body, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	return err
}

// emit fans out one NotifyEvent synchronously (P8), nil-safe.
func (s *Service) emit(ctx context.Context, ev NotifyEvent) {
	if s.Notify == nil {
		return
	}
	if ev.At == "" {
		ev.At = s.nowUTC().Format(time.RFC3339)
	}
	if ev.Class == "" {
		ev.Class = NotifyClass
	}
	s.Notify(ctx, ev)
}

// stream appends one live-update event, nil-safe.
func (s *Service) stream(ctx context.Context, ev StreamEvent) {
	if s.Stream == nil {
		return
	}
	if ev.Name == "" {
		ev.Name = StreamName
	}
	s.Stream(ctx, ev)
}
