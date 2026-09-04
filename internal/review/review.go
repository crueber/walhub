// Package review implements docs/features/04_code_review.md: approvals,
// line-anchored threads, review requests, the review_summary rollup, and
// the required-reviews merge gate. It registers Seam 1 routes on both lanes
// (Handler.Handle fronts the core mux exactly like internal/identity,
// internal/issues, and internal/pulls) and the merge-time half of the
// required-reviews policy effect (Seam 3; the push-time half is
// policy.RequiredReviewsEffect, enforced at receive-pack).
//
// Bucket layout (all keys bucket-relative; num is the shared issue/PR
// number, P2; tid is a zero-padded 8-hex thread id; review seqs are
// zero-padded 12-hex, P3):
//
//	repos/<o>/<r>/pulls/<num:06x>/reviews/<seq:012x>.json        immutable review / review_dismissed events (Create)
//	repos/<o>/<r>/pulls/<num:06x>/threads/<tid>/thread.json      CAS'd thread header (anchor, resolution)
//	repos/<o>/<r>/pulls/<num:06x>/threads/<tid>/events/<seq>.json immutable review_thread_comment events (Create)
//	repos/<o>/<r>/pulls/<num:06x>/review-requests.json           CAS'd current requested reviewers
//	repos/<o>/<r>/issues/<num:06x>/thread.json                   PR header (03 owns the shape; 04 adds
//	                                                            next_review_seq, next_thread_num, review_summary)
//
// The `pulls/<num>/reviews/`, `pulls/<num>/threads/`, and
// `pulls/<num>/review-requests.json` families join the frozen
// overwritable-key family (14_extensibility.md §14.11 rule 2) in the same
// revision that adopts this package (see the amendment in
// docs/go/14_extensibility.md Decisions).
//
// The WAL stays git-only: reviews never produce WAL entries and never gate
// a push (except via the push-time half of the policy effect, evaluated in
// receive-pack where review state is NOT observable). Enforcement of review
// state happens inside 03's merge task, which consults this package's gate
// through the ReviewGate seam before publishing the merge ref. Core
// (internal/store, internal/wal, internal/git) never imports this package.
//
// ### Concurrency
//
// Hazard: two writers racing on one CAS'd object (PR header, thread
// header, review-requests) losing an update. Avoidance: every mutation is a
// PutUpdate(version) CAS loop with 412 → re-read → recompute → retry (the
// 13_concurrency.md playbook; CAS IS the lock — no cross-feature locks, no
// sidecar mutexes, no .lock objects). Allocation counters
// (next_review_seq, next_thread_num) live on the PR header so a review
// submit and a thread open arbitrate on ONE CAS. Crash between header CAS
// and event Create skips a seq — gaps are allowed and harmless (P3).
// Hazard: the gate reading review_summary while a review lands (stale
// cache deciding a merge). Avoidance: the gate NEVER trusts the
// denormalized summary — it re-derives the verdict by scanning the review
// events at merge time (authoritative scan; the summary is a render cache).
// The scan runs inside the merge task's context with its own deadline; no
// locks are taken. No lock is held across any store call (13 §2 rule 4).
package review

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

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// RoleService is the narrow P6 surface this package consumes (same shape as
// internal/issues and internal/pulls: satisfied by *identity.Service; tests
// substitute a fake). Resolution order and require_read semantics stay
// owned by 01.
type RoleService interface {
	// Resolve returns the max repo role for p (P6 verbatim).
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	// CheckRead is the require_read gate: nil allows; anonymous-denied
	// reads are ErrUnauthorized (→ real 401), authenticated-but-insufficient
	// are ErrForbidden (→ 403).
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Authenticator resolves the request principal through Seam 2 (the server's
// AuthService, injected by composition). Nil falls back to anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// NotifyEvent is the §7/P8 emission contract with 06 (internal/notify): per
// P8 the mutating handler emits synchronously after the CAS commits.
// Classes: "review_submitted", "review_dismissed", "review_requested",
// "review_request_removed", "thread_commented", "thread_resolved",
// "thread_unresolved", "thread_opened".
type NotifyEvent struct {
	Repo       string   `json:"repo"` // "owner/name"
	Class      string   `json:"class"`
	Actor      string   `json:"actor"`
	PullNum    int      `json:"pull_num"`
	Recipients []string `json:"recipients"`
	At         string   `json:"at"` // RFC 3339 UTC
}

// StreamEvent is the live-update contract for the repo SSE stream (§7):
// event names `review` (posted/dismissed; payload carries the new
// review_summary) and `thread` (comment/resolution; payload carries tid),
// using the 07 §6 envelope, on the single collaboration stream named by
// 06/08.
type StreamEvent struct {
	Name    string         `json:"name"` // "review" | "thread"
	Repo    string         `json:"repo"`
	Action  string         `json:"action"` // submitted|dismissed|opened|commented|resolved|unresolved
	Num     int            `json:"num"`
	Summary *ReviewSummary `json:"summary,omitempty"` // set on review frames
	TID     string         `json:"tid,omitempty"`     // set on thread frames
}

// Emitter delivers NotifyEvents synchronously in the mutating handler (P8).
// Nil until internal/notify lands: a documented no-op; the timeline stays
// the backfill truth.
type Emitter func(ctx context.Context, ev NotifyEvent)

// Streamer appends StreamEvents to the repo's SSE stream. Nil-safe.
type Streamer func(ctx context.Context, ev StreamEvent)

// GroupExpander expands team:/role: spellings to principals for
// review-suggest (satisfied by *identity.Service via ExpandGroups; nil
// means team bindings contribute nothing — user bindings still apply).
type GroupExpander interface {
	ExpandGroups(ctx context.Context, members []string) (expanded []string, warnings []string)
}

// CommitAuthors supplies head-branch commit authors for review-suggest
// (§5 third source). Satisfied in composition by *pulls.Service
// (HeadAuthors); nil means commit authors contribute nothing.
type CommitAuthors interface {
	// HeadAuthors returns up to n author principals of commits in
	// base..head of PR num, newest-first.
	HeadAuthors(ctx context.Context, owner, repo string, num, n int) ([]string, error)
}

// Service is the review store client: immutable review events, CAS'd
// thread headers + comment events, the CAS'd review-requests index, the
// review_summary render cache, review-suggest, and the merge-time gate.
// Construct with New; Roles/Authors/Expander may be nil in tests that
// exercise pure paths (nil Roles falls back to principal flags).
type Service struct {
	Store    store.ObjectStore
	Roles    RoleService
	Authors  CommitAuthors
	Expander GroupExpander
	Now      func() time.Time

	// GateTimeout bounds the merge-time gate's authoritative event scan
	// (§6: the scan runs inside the merge task's context with its own
	// deadline). Zero means the default (15 s).
	GateTimeout time.Duration

	// Notify receives §7/P8 fan-out synchronously after each committed
	// mutation (nil = no-op until internal/notify lands).
	Notify Emitter
	// Stream receives review/thread live updates (nil = no-op).
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

// gateTimeout resolves the gate scan deadline.
func (s *Service) gateTimeout() time.Duration {
	if s.GateTimeout > 0 {
		return s.GateTimeout
	}
	return 15 * time.Second
}

// --- key helpers (bucket-relative) -----------------------------------------

// ThreadKey returns repos/<o>/<r>/issues/<num:06x>/thread.json (the PR
// header shared with 02/03; 04 adds next_review_seq, next_thread_num,
// review_summary — additive optional fields per 14 §14.12).
func ThreadKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/thread.json", owner, repo, num)
}

// PRKey returns repos/<o>/<r>/pulls/<num:06x>/pr.json (the §2.1 sidecar 03
// owns; review reads Head.SHA/Author for the commit_sha pin and the
// self-approve check — never writes).
func PRKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/pr.json", owner, repo, num)
}

// ReviewKey returns repos/<o>/<r>/pulls/<num:06x>/reviews/<seq:012x>.json.
func ReviewKey(owner, repo string, num, seq int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/reviews/%012x.json", owner, repo, num, seq)
}

// ReviewsPrefix returns repos/<o>/<r>/pulls/<num:06x>/reviews/ (scan root
// for the summary recompute and the gate; low-volume collaboration
// subtree, P5-fine).
func ReviewsPrefix(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/reviews/", owner, repo, num)
}

// ReviewThreadKey returns
// repos/<o>/<r>/pulls/<num:06x>/threads/<tid>/thread.json.
func ReviewThreadKey(owner, repo string, num int, tid string) string {
	return "repos/" + owner + "/" + repo + fmt.Sprintf("/pulls/%06x/threads/%s/thread.json", num, tid)
}

// ReviewThreadEventKey returns
// repos/<o>/<r>/pulls/<num:06x>/threads/<tid>/events/<seq:012x>.json.
func ReviewThreadEventKey(owner, repo string, num int, tid string, seq int) string {
	return "repos/" + owner + "/" + repo + fmt.Sprintf("/pulls/%06x/threads/%s/events/%012x.json", num, tid, seq)
}

// ReviewThreadsPrefix returns repos/<o>/<r>/pulls/<num:06x>/threads/ (LIST
// root for thread-header scans; collaboration page, P5-fine).
func ReviewThreadsPrefix(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/threads/", owner, repo, num)
}

// ReviewRequestsKey returns
// repos/<o>/<r>/pulls/<num:06x>/review-requests.json.
func ReviewRequestsKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/review-requests.json", owner, repo, num)
}

// AccessKey returns repos/<o>/<r>/access.json (read for review-suggest's
// first source; 01 owns the shape — review decodes only subject/role).
func AccessKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/access.json"
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
	ev.Recipients = uniqSorted(ev.Recipients)
	s.Notify(ctx, ev)
}

// emitMentioned fans "mentioned" for @-parsed principals and @org/team
// spellings in a review/thread-comment body (06 §3; the consumer
// validates and expands). Bodies without tokens emit nothing.
func (s *Service) emitMentioned(ctx context.Context, owner, repo string, num int, actor, body string) {
	if body == "" {
		return
	}
	users, teams := identity.ParseMentions(body)
	var recips []string
	for _, m := range users {
		if m != actor && identity.ValidPrincipal(m) {
			recips = append(recips, m)
		}
	}
	for _, t := range teams {
		if t != actor {
			recips = append(recips, t)
		}
	}
	if len(recips) == 0 {
		return
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "mentioned", Actor: actor, PullNum: num, Recipients: recips})
}

// stream appends one live-update event, nil-safe.
func (s *Service) stream(ctx context.Context, ev StreamEvent) {
	if s.Stream == nil {
		return
	}
	s.Stream(ctx, ev)
}
