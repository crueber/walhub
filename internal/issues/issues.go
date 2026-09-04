// Package issues implements docs/features/02_issues.md: issue threads,
// comments, labels, milestones, assignees, close/reopen, cross-references
// (#N at write time), reactions, the CAS'd list index, and the
// issue-index-compact task kind. It registers Seam 1 routes on both lanes
// (Handler.Handle fronts the core mux exactly like internal/identity) and
// consumes the P6 role resolution owned by internal/identity through the
// narrow RoleService interface below.
//
// Bucket layout (all keys bucket-relative; the store prefix is applied by
// the store layer):
//
//	repos/<o>/<r>/meta/next_num                  CAS'd shared counter {"next": N} (P2)
//	repos/<o>/<r>/issues/<num:06x>/thread.json    CAS'd thread header (P3)
//	repos/<o>/<r>/issues/<num:06x>/events/<seq:012x>.json  immutable events, Create-only (P3)
//	repos/<o>/<r>/issues/index.json              CAS'd list index (P4, frozen overwritable family)
//	repos/<o>/<r>/meta/labels.json               CAS'd repo label set
//	repos/<o>/<r>/meta/milestones/<id:06x>.json   milestone (Create + CAS'd update)
//	repos/<o>/<r>/meta/milestones/index.json     CAS'd milestone id allocator {"next": N}
//
// PR threads (docs/features/03) share the numbering space and the
// thread.json/event shapes here; 03 owns a pr.json sidecar and never writes
// issue-owned fields.
//
// ### Concurrency
//
// Hazard: two writers mutating one CAS'd object (a thread header, the
// index, labels.json, a milestone) losing an update on blind PUT.
// Avoidance: every mutation is the P3 two-step — CAS the header
// (Update(version), reserving next_event_seq), then Create the event — plus
// a second CAS loop for the index. CAS loops are the ONLY coordination
// tool (13_concurrency.md §3/§5): no cross-feature or cross-thread locks,
// no lock object, no .lock sidecar, no new in-process mutex. A reference
// write into another thread's log runs the same two-step on that thread;
// no ordering between two threads' CAS targets is promised anywhere.
// Hazard: a blocked index writer starving readers. Avoidance: the index
// CAS loop is bounded (10 attempts) and then DROPPED — the repair path
// (next mutation re-reads, diffs, repairs; LIST fallback covers reads), so
// the index is performance scaffolding, never authoritative.
package issues

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

// TaskKindIssueIndexCompact is the §9 task kind: CAS-compaction of
// issues/index.json past ~256 KiB. (repo, kind) single-flight per
// 13_concurrency.md §3; SSE-attachable. No other task kinds: issue
// mutations are synchronous (P8).
const TaskKindIssueIndexCompact = "issue-index-compact"

// IndexSizeLimit is the ~256 KiB compaction trigger (§2): handlers check
// the bytes they just wrote and compact inline when over.
const IndexSizeLimit = 256 << 10

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
// server's AuthService, injected by composition). A nil Authenticator falls
// back to the mode default, mirroring internal/identity.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// NotifyEvent is the §10 emission contract with 06 (internal/notify):
// per P8 the mutating handler emits synchronously after the CAS commits,
// resolving targets from participants[] + parsed mentions — 06 MUST NOT
// scan the event log. Classes: "assigned" (assignees_changed adds),
// "mentioned" (@name in opened/commented), "subscribed" (participation:
// opened/commented/state/label changes by a non-participant, fanned to
// participants).
type NotifyEvent struct {
	Repo       string   `json:"repo"` // "owner/name"
	Class      string   `json:"class"`
	Actor      string   `json:"actor"`
	IssueNum   int      `json:"issue_num"`
	Recipients []string `json:"recipients"`
	At         string   `json:"at"` // RFC 3339 UTC
	// Action disambiguates the coarse "subscribed" class for 06's
	// activity log (opened|commented|closed|reopened|…; "" = commented).
	// Additive (06 wave); readers treat "" as "commented".
	Action string `json:"action,omitempty"`
}

// StreamEvent is the live-update contract for the repo SSE stream (§11):
// event name "issue" (header upsert) or "issue_event" (timeline frame).
// Seq carries the appended event's seq on issue_event frames (08 §4: the
// timeline appends without refetch); header frames leave it zero.
type StreamEvent struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	IssueNum int    `json:"issue_num"`
	Seq      int    `json:"seq,omitempty"`
}

// Emitter delivers NotifyEvents synchronously in the mutating handler
// (P8: a crash after CAS but before fan-out loses one notification, never
// data; the timeline is the backfill source of truth).
//
// WAVE B NOTE: internal/notify does not exist yet, so composition wires
// no durable fan-out; the default (nil) emitter is a documented no-op and
// the emission points are all in one place (Service.emit) for 06 to bind
// to. The data needed for backfill (participants[], mentions in bodies)
// is durable in the bucket regardless.
type Emitter func(ctx context.Context, ev NotifyEvent)

// Streamer appends StreamEvents to the repo's SSE stream. Nil-safe;
// composition wires the real stream when the per-repo bus accepts
// feature event names.
type Streamer func(ctx context.Context, ev StreamEvent)

// Service is the issues store client: all bucket I/O for numbering,
// threads, events, the index, labels, and milestones. Construct with New;
// Roles/Auth may be nil in tests that exercise pure parsers.
type Service struct {
	Store store.ObjectStore
	Roles RoleService
	Now   func() time.Time

	// Notify receives §10 fan-out synchronously after each committed
	// mutation (nil = no-op until internal/notify lands).
	Notify Emitter
	// Stream receives issue/issue_event live updates (nil = no-op).
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

// --- key helpers (bucket-relative; §1) --------------------------------------

// CounterKey returns repos/<o>/<r>/meta/next_num.
func CounterKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/next_num"
}

// ThreadKey returns repos/<o>/<r>/issues/<num:06x>/thread.json.
func ThreadKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/thread.json", owner, repo, num)
}

// ThreadPrefix returns repos/<o>/<r>/issues/<num:06x>/.
func ThreadPrefix(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/", owner, repo, num)
}

// EventKey returns repos/<o>/<r>/issues/<num:06x>/events/<seq:012x>.json.
func EventKey(owner, repo string, num, seq int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/events/%012x.json", owner, repo, num, seq)
}

// EventsPrefix returns repos/<o>/<r>/issues/<num:06x>/events/.
func EventsPrefix(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/events/", owner, repo, num)
}

// IssuesPrefix returns repos/<o>/<r>/issues/ (LIST fallback root, P5).
func IssuesPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/issues/"
}

// IndexKey returns repos/<o>/<r>/issues/index.json.
func IndexKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/issues/index.json"
}

// LabelsKey returns repos/<o>/<r>/meta/labels.json.
func LabelsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/labels.json"
}

// MilestoneKey returns repos/<o>/<r>/meta/milestones/<id:06x>.json.
func MilestoneKey(owner, repo, id string) string {
	return "repos/" + owner + "/" + repo + "/meta/milestones/" + id + ".json"
}

// MilestonePrefix returns repos/<o>/<r>/meta/milestones/.
func MilestonePrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/milestones/"
}

// MilestoneCounterKey returns repos/<o>/<r>/meta/milestones/index.json.
func MilestoneCounterKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/milestones/index.json"
}

// repoName renders "owner/repo".
func repoName(owner, repo string) string { return owner + "/" + repo }

// normPrincipal lowercases and trims a principal name for comparison.
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// --- CAS helper ------------------------------------------------------------

// casUpdate is the canonical CAS loop (13 §3, same shape as identity's):
// read, apply, Update(version), retry-on-412 re-read, bounded attempts,
// then ErrConflict. f receives the current body (nil when absent) and
// version ("" when absent) and returns the replacement body, whether to
// write, and an error (validation failures abort without writing).
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

// emit fans out one NotifyEvent synchronously (P8), nil-safe, with the
// recipients normalized (sorted, unique, non-empty, actor excluded for
// the subscribed class by the caller).
func (s *Service) emit(ctx context.Context, ev NotifyEvent) {
	if s.Notify == nil {
		return
	}
	if ev.At == "" {
		ev.At = s.nowUTC().Format(time.RFC3339)
	}
	ev.Recipients = uniqSorted(ev.Recipients)
	s.Notify(ctx, ev)
}

// stream appends one live-update event, nil-safe.
func (s *Service) stream(ctx context.Context, ev StreamEvent) {
	if s.Stream == nil {
		return
	}
	s.Stream(ctx, ev)
}
