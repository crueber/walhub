// Package pulls implements docs/features/03_pull_requests.md: pull-request
// threads (kind:"pr" over the shared issues numbering/thread/index family),
// the pr.json sidecar, the stamped mergeable.json cache, the `pulls` event
// sink, and the `pull-merge` / `pull-mergeable` / `pull-fork` /
// `pull-update-branch` task kinds. It registers Seam 1 routes on both lanes
// (Handler.Handle fronts the core mux exactly like internal/identity and
// internal/issues) and consumes P6 role resolution owned by
// internal/identity through the narrow RoleService interface below.
//
// Bucket layout (all keys bucket-relative):
//
//	repos/<o>/<r>/issues/<num:06x>/thread.json      shared P3 header, kind:"pr" (02 owns the shape;
//	                                                 PR fields MUST NOT leak into it)
//	repos/<o>/<r>/issues/<num:06x>/events/<seq>.json shared P3 immutable events, Create-only
//	repos/<o>/<r>/issues/index.json                  shared P4 list index (cards carry kind)
//	repos/<o>/<r>/meta/next_num                      shared P2 counter {"next": N}
//	repos/<o>/<r>/pulls/<num:06x>/pr.json            PR sidecar (CAS'd)
//	repos/<o>/<r>/pulls/<num:06x>/mergeable.json     mergeability cache (Create-then-CAS overwrite)
//	repos/<o>/<r>/meta/forks.json                    parent-side fork index (CAS'd)
//	repos/<o2>/<r2>/fork.json                        fork-side provenance (Create once, then CAS'd)
//
// pr.json, mergeable.json, meta/forks.json join the frozen overwritable-key
// family (D-EXT-2) in the same revision that adopts this package (see the
// amendment in docs/go/14_extensibility.md Decisions).
//
// The WAL stays git-only: `refs/pull/<num>/head` is ordinary WAL ref state
// published through the RefPublisher seam (the WAL publish funnel), never a
// new WAL kind. Core (internal/store, internal/wal, internal/git) never
// imports this package; the `refs/pull/**` client-push refusal lives in the
// core push pipeline (internal/git.IsManagedRef + server pushPipeline) so it
// covers every transport without an upward import.
//
// ### Concurrency
//
// Hazard: two writers mutating one CAS'd object (a pr.json sidecar, a
// mergeable.json cache, forks.json, the shared thread header/index) losing
// an update on blind PUT. Avoidance: numbering via the shared P2 CAS loop,
// thread events via the shared P3 two-step, sidecars via read-modify-write
// CAS loops (bounded attempts, then 409), the mergeable cache via CAS with
// loser tantamount to a recompute (derived state — a 412 loser re-runs after
// the winner and converges). Merge arbitration is exactly two mechanisms, no
// cross-feature locks: (a) the in-process (repo,"pull-merge") task
// single-flight serializes merges per repo; (b) the WAL publish CAS
// arbitrates the final base-ref update (a moved base loses and the task
// re-plans or fails loudly — the merge task NEVER force-publishes).
// Hazard: N readers of a just-pushed PR head triggering N concurrent
// merge-tree runs (git-pool exhaustion, 13 §4). Avoidance: recompute goes
// through the in-process single-flight keyed "mergeable:"+repo+"/"+num (13
// §3 — joiners share the result with bounded wait); git runs go through the
// bounded per-repo pool, never bare on request goroutines. No lock is held
// across any store call or git subprocess (13 §2 rule 4).
package pulls

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Task kinds (Seam 5; §5/§4/§7). (repo, kind) single-flight per
// 13_concurrency.md §3; SSE-attachable narrated tasks.
const (
	// TaskKindMerge recomputes nothing: it lands one PR (strategy argv,
	// protected-ref gate, WAL publish, P3/P4/P8 commit).
	TaskKindMerge = "pull-merge"
	// TaskKindMergeable recomputes stamped mergeable.json caches, batching
	// all dirty PRs of one repo into one pass.
	TaskKindMergeable = "pull-mergeable"
	// TaskKindFork creates a fork prefix sharing the parent's pack set.
	TaskKindFork = "pull-fork"
	// TaskKindUpdateBranch merges base into head (write, 409 if dirty).
	TaskKindUpdateBranch = "pull-update-branch"
)

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// RoleService is the narrow P6 surface this package consumes (same shape as
// internal/issues: satisfied by *identity.Service; tests substitute a fake).
// Resolution order and require_read semantics stay owned by 01.
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

// NotifyEvent is the §8/P8 emission contract with 06 (internal/notify): per
// P8 the mutating handler emits synchronously after the CAS commits.
// Classes: "opened" (pull_opened), "closed", "reopened", "merged",
// "head_force_pushed", "assigned", "mentioned", "subscribed".
type NotifyEvent struct {
	Repo       string   `json:"repo"` // "owner/name"
	Class      string   `json:"class"`
	Actor      string   `json:"actor"`
	PullNum    int      `json:"pull_num"`
	Recipients []string `json:"recipients"`
	At         string   `json:"at"` // RFC 3339 UTC
}

// StreamEvent is the live-update contract for the repo SSE stream (§8):
// event name "pull" (data carries the action) on the single collaboration
// stream named by 06/08; comment events arrive as 02's "comment" event.
type StreamEvent struct {
	Name    string `json:"name"` // always "pull"
	Repo    string `json:"repo"`
	Action  string `json:"action"` // opened|closed|reopened|merged|head_force_pushed
	Num     int    `json:"num"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Author  string `json:"author"`
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
}

// Emitter delivers NotifyEvents synchronously in the mutating handler (P8).
// Nil until internal/notify lands: a documented no-op; the timeline stays
// the backfill truth.
type Emitter func(ctx context.Context, ev NotifyEvent)

// Streamer appends StreamEvents to the repo's SSE stream. Nil-safe.
type Streamer func(ctx context.Context, ev StreamEvent)

// IssueCloser is the 02-provided seam the merge task calls at PR MERGE time
// (never at push): ApplyClosingReferences parses closing keywords and closes
// referenced issues. Satisfied by *issues.Service; tests substitute a fake.
type IssueCloser interface {
	// ApplyClosingReferences closes issues matched by the §5 grammar in
	// texts (PR body + merged commit messages). Returns closed nums, sorted.
	ApplyClosingReferences(ctx context.Context, owner, repo string, prNum int, mergedSHA, actor string, texts []string) ([]int, error)
}

// Service is the pulls store client: numbering (shared P2), threads (shared
// P3), the shared index (P4), pr.json sidecars, mergeable.json caches,
// forks, git mergeability/merge execution, and the WAL ref publishes.
// Construct with New; Roles/Git/Dirs/Refs/Closer/Reviews may be nil in tests that
// exercise pure paths (nil Git/Dirs/Refs makes git-touching ops 503;
// nil Reviews skips the 04 required-reviews gate).
type Service struct {
	Store  store.ObjectStore
	Roles  RoleService
	Git    GitRunner
	Dirs   RepoDirs
	Refs   RefPublisher
	Closer IssueCloser
	// Reviews is the 04-provided merge-time gate (docs/features/04 §6;
	// see review.go). Wired in composition (cmd/walhub); nil skips it.
	Reviews ReviewGate
	Now     func() time.Time

	// ServerID is the committer identity for merge commits
	// ("walhub <server.identity_email>", default "walhub@localhost").
	ServerID string

	// Notify receives §8/P8 fan-out synchronously after each committed
	// mutation (nil = no-op until internal/notify lands).
	Notify Emitter
	// Stream receives pull live updates (nil = no-op).
	Stream Streamer

	flight *flightGroup
	tasks  *taskTable
}

// New builds a Service over st.
func New(st store.ObjectStore, roles RoleService) *Service {
	return &Service{Store: st, Roles: roles, Now: time.Now, ServerID: "walhub@localhost", flight: newFlightGroup(), tasks: newTaskTable()}
}

// nowUTC is the clock, UTC (RFC 3339 wire timestamps).
func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// committer splits ServerID ("name <email>") for commit-tree env.
func (s *Service) committer() (name, email string) {
	name, email = "walhub", s.ServerID
	if i := strings.Index(s.ServerID, "<"); i >= 0 {
		name = strings.TrimSpace(s.ServerID[:i])
		email = strings.TrimSuffix(strings.TrimSpace(s.ServerID[i+1:]), ">")
		if name == "" {
			name = "walhub"
		}
		if email == "" {
			email = "walhub@localhost"
		}
		return name, email
	}
	if strings.Contains(s.ServerID, "@") {
		return "walhub", s.ServerID
	}
	if s.ServerID != "" {
		return s.ServerID, "walhub@localhost"
	}
	return "walhub", "walhub@localhost"
}

// --- key helpers (bucket-relative) -----------------------------------------

// CounterKey returns repos/<o>/<r>/meta/next_num (shared P2 counter with issues).
func CounterKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/next_num"
}

// ThreadKey returns repos/<o>/<r>/issues/<num:06x>/thread.json (shared P3 header).
func ThreadKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/thread.json", owner, repo, num)
}

// EventKey returns repos/<o>/<r>/issues/<num:06x>/events/<seq:012x>.json (shared P3).
func EventKey(owner, repo string, num, seq int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/events/%012x.json", owner, repo, num, seq)
}

// EventsPrefix returns repos/<o>/<r>/issues/<num:06x>/events/.
func EventsPrefix(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/events/", owner, repo, num)
}

// IssuesPrefix returns repos/<o>/<r>/issues/ (shared LIST root, P5).
func IssuesPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/issues/"
}

// IndexKey returns repos/<o>/<r>/issues/index.json (shared P4 index).
func IndexKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/issues/index.json"
}

// PRKey returns repos/<o>/<r>/pulls/<num:06x>/pr.json (the §2.1 sidecar).
func PRKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/pr.json", owner, repo, num)
}

// MergeableKey returns repos/<o>/<r>/pulls/<num:06x>/mergeable.json (the §2.2 cache).
func MergeableKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/mergeable.json", owner, repo, num)
}

// PullsPrefix returns repos/<o>/<r>/pulls/ (LIST root for sidecar scans, P5).
func PullsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/pulls/"
}

// ForksKey returns repos/<o>/<r>/meta/forks.json (parent-side fork index).
func ForksKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/forks.json"
}

// ForkKey returns repos/<o>/<r>/fork.json (fork-side provenance).
func ForkKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/fork.json"
}

// PolicyKey returns repos/<o>/<r>/policy.json (the push rule language the
// merge task explicitly evaluates for the base-ref publish, §5 step 4).
func PolicyKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/policy.json"
}

// PullHeadRef renders refs/pull/<num>/head (ordinary WAL ref state, §2).
func PullHeadRef(num int) string { return fmt.Sprintf("refs/pull/%d/head", num) }

// repoName renders "owner/repo".
func repoName(owner, repo string) string { return owner + "/" + repo }

// normPrincipal lowercases and trims a principal name for comparison.
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// --- CAS helpers (13 §3 canonical loops) ------------------------------------

// casUpdate is the canonical CAS loop: read, apply, Update(version),
// retry-on-412 re-read, bounded attempts, then ErrConflict. f receives the
// current body (nil when absent) and version ("" when absent) and returns
// the replacement body, whether to write, and an error (validation failures
// abort without writing).
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

// stream appends one live-update event, nil-safe.
func (s *Service) stream(ctx context.Context, ev StreamEvent) {
	if s.Stream == nil {
		return
	}
	s.Stream(ctx, ev)
}

// --- single-flight (13 §3) ---------------------------------------------------

// flightGroup is the hand-rolled single-flight (one Group per concern):
// joiners share the leader's result with bounded wait (ctx-select), so a
// stuck leader cannot pin request goroutines.
type flightGroup struct {
	mu sync.Mutex
	fl map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

func newFlightGroup() *flightGroup { return &flightGroup{fl: map[string]*flightCall{}} }

// Do runs fn under key; concurrent joiners await the leader (bounded by ctx).
func (g *flightGroup) Do(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if c, ok := g.fl[key]; ok {
		g.mu.Unlock()
		done := make(chan struct{})
		go func() { c.wg.Wait(); close(done) }()
		select {
		case <-done:
			return c.val, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.fl[key] = c
	g.mu.Unlock()
	c.val, c.err = fn()
	c.wg.Done()
	g.mu.Lock()
	delete(g.fl, key)
	g.mu.Unlock()
	return c.val, c.err
}
