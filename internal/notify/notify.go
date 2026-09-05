// Package notify implements docs/features/06_notifications.md: the
// fan-out layer of the collaboration family — notifications,
// subscriptions, mentions, per-user SSE, repo webhooks, and retention.
//
// 09 §2 dispatch name is `internal/notify` (the 06 text says
// `internal/notifications`; the dispatch name wins — see the 06
// Decisions note). One api seam surface (Handler.Handle fronts the core
// mux exactly like internal/identity: top-level /api/v1/notifications*
// twins plus repo-scoped watch/webhook routes on both lanes), task kinds
// `webhooks` / `notify-fanout` / `notify-retention` (Seam 5, in-process
// single-flight + background sweeps started by composition). No WAL
// entries, no manifest touches (P1 law).
//
// Bucket layout (all keys bucket-relative):
//
//	users/<principal>/notifications/<id>.json        Create-only notification (§1.1)
//	users/<principal>/notifications/index.json       CAS'd unread hot window (§1.2, P4)
//	users/<principal>/watching/<o>/<r>.json          watch record (07-owned shape; 06 writes until 07 lands)
//	repos/<o>/<r>/meta/social.json                   CAS'd counters + watcher_list (07-owned shape; 06 writes until 07 lands)
//	repos/<o>/<r>/meta/collab_state.json             CAS'd activity seq allocator {"next_seq": N}
//	repos/<o>/<r>/collab-events/<seq:012x>.json      immutable activity events, Create-only (§5.3)
//	repos/<o>/<r>/collab-fanout/<seq:012x>.json      per-seq fan-out completion records (§8 redrain)
//	repos/<o>/<r>/webhooks/<id>.json                 CAS'd hook config (§1.4)
//	repos/<o>/<r>/webhooks/cursors/<id>.json         CAS'd per-hook cursor (§5.3)
//	repos/<o>/<r>/webhooks/<id>/deliveries/recent.json  CAS'd last-25 ring (§1.4)
//
// ### Concurrency
//
// Hazard: a fan-out touching N users races concurrent emissions, sweeps,
// and deliveries on the same objects. Avoidance: CAS loops are the ONLY
// coordination tool (13_concurrency.md §2; the three repo locks are
// closed, no cross-feature locks, no new repo locks). Each notification
// Create is independent and idempotent (deterministic id, §1.1); the
// unread index is a single CAS retry loop; a lost index race is retried,
// a lost Create is a no-op (412). The notify-fanout task end is the one
// mutex coordination: drain-then-end is atomic under taskTable.mu
// nesting taskEntry.mu (endIfQuiescent refuses while seqs are pending),
// and a joiner re-enqueues when its entry detached mid-attach (tasks.go;
// issue #72). The webhook delivery loop advances a
// per-hook cursor by CAS only after successful POSTs (at-least-once; a
// lost cursor CAS redelivers, never skips). Fan-out parallelism is bounded
// (semaphore cap 8, 13_concurrency.md §4, no raw `go func()` over store
// I/O) under a 5 s budget; overflow (> MaxSyncRecipients) falls back to
// the notify-fanout task. SSE publishes never block (drop-oldest
// broadcast). No lock is held across any store or network call.
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Task kinds (Seam 5; 06 §8). (repo, kind) single-flight per
// 13_concurrency.md §3; the overflow task carries attached activity seqs.
const (
	// TaskKindWebhooks runs the per-hook delivery loop for one repo
	// (§5.3), started by sweep + wake-up after each fanned-out event.
	TaskKindWebhooks = "webhooks"
	// TaskKindFanout drains a bulk notification Create burst for
	// overflow emissions (> MaxSyncRecipients) and sync-path shortfalls.
	TaskKindFanout = "notify-fanout"
	// TaskKindRetention runs the §9 compaction pass (global, Ops nil).
	TaskKindRetention = "notify-retention"
)

// Bounds and budgets (06 §4/§5.3/§9).
const (
	// MaxSyncRecipients caps synchronous fan-out per emission; beyond it
	// the request writes the activity event and defers to notify-fanout.
	MaxSyncRecipients = 100
	// FanoutBudget bounds the synchronous fan-out of one emission.
	FanoutBudget = 5 * time.Second
	// FanoutParallel caps concurrent store writes inside one fan-out.
	// Slots are acquired BEFORE spawning (issue #153), so in-flight
	// fan-out goroutines are bounded by construction on every path.
	FanoutParallel = 8
	// WebhookTimeout bounds one webhook POST (§5.3).
	WebhookTimeout = 10 * time.Second
	// MaxWatchers caps social.json watcher_list (§2: 1 000, truncated).
	MaxWatchers = 1000
	// MaxTeamFanout caps one @org/team expansion (§3: 100, sorted).
	MaxTeamFanout = 100
	// MaxDeliveries bounds the per-hook deliveries ring (§1.4: 25).
	MaxDeliveries = 25
	// DefaultMaxHooksPerRepo caps hook configs per repo (§1.4). The sweep
	// and delivery cost per repo is O(hooks), so the cap is also the
	// per-repo sweep bound (issue #156). Overridable per Service via
	// MaxHooks (composition leaves zero for the default — no config key
	// in v1, same convention as RetentionDays).
	DefaultMaxHooksPerRepo = 20
	// DefaultRetentionDays drops read notifications past this age (§9).
	DefaultRetentionDays = 30
	// CollabEventsFloorDays deletes activity events below the minimum
	// webhook cursor only past this age (§9).
	CollabEventsFloorDays = 7
	// TrayPageSize is the LIST-overflow page (§1.2/§6: 50, max 200).
	TrayPageSize = 50
	TrayMaxPage  = 200
)

// Notification reasons (§1.1 enum).
const (
	ReasonMentioned       = "mentioned"
	ReasonAssigned        = "assigned"
	ReasonReviewRequested = "review_requested"
	ReasonSubscribed      = "subscribed"
	ReasonAuthor          = "author"
	ReasonTeamMention     = "team_mention"
)

// Notification states (§1.1: the ONLY mutable field is state).
const (
	StateUnread = "unread"
	StateRead   = "read"
)

// Activity actions (§5.3 enum + wildcard).
const (
	ActionCommented        = "commented"
	ActionOpened           = "opened"
	ActionClosed           = "closed"
	ActionReopened         = "reopened"
	ActionAssigned         = "assigned"
	ActionReviewRequested  = "review_requested"
	ActionReviewPosted     = "review_posted"
	ActionCheckReported    = "check_reported"
	ActionReleasePublished = "release_published"
	ActionMentioned        = "mentioned"
	ActionPing             = "ping"
)

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// --- errors (wire: plain text, foreign ids → 404) ---------------------------

// Sentinel errors; handlers map them to status codes via statusFor.
var (
	ErrNotFound     = fmt.Errorf("not found")
	ErrUnauthorized = fmt.Errorf("unauthorized")
	ErrForbidden    = fmt.Errorf("forbidden")
	ErrInvalid      = fmt.Errorf("invalid")
	ErrConflict     = fmt.Errorf("conflict")
)

// statusFor maps sentinels to HTTP status codes.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case isErr(err, ErrNotFound):
		return 404
	case isErr(err, ErrUnauthorized):
		return 401
	case isErr(err, ErrForbidden):
		return 403
	case isErr(err, ErrInvalid):
		return 400
	case isErr(err, ErrConflict):
		return 409
	default:
		return 500
	}
}

func isErr(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

// --- object shapes -----------------------------------------------------------

// Notification is one users/<principal>/notifications/<id>.json object
// (§1.1). Immutable except State (CAS flip read/unread); deleted on
// retention (§9).
type Notification struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Num       int    `json:"num"`
	Kind      string `json:"kind"` // "issue"|"pull"|"release"|"repo"
	Reason    string `json:"reason"`
	Title     string `json:"title"`
	Actor     string `json:"actor"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// IndexEntry is one hot-window row of the per-user unread index (§1.2).
type IndexEntry struct {
	ID     string `json:"id"`
	Repo   string `json:"repo"`
	Num    int    `json:"num"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Title  string `json:"title"`
	State  string `json:"state"`
	At     string `json:"at"`
}

// IndexDoc is users/<principal>/notifications/index.json (§1.2, P4).
type IndexDoc struct {
	Version          int          `json:"version"`
	UnreadCount      int          `json:"unread_count"`
	Entries          []IndexEntry `json:"entries"`
	CompactedThrough string       `json:"compacted_through,omitempty"`
	SweptAt          string       `json:"swept_at,omitempty"`
}

// Hook is one repos/<o>/<r>/webhooks/<id>.json config (§1.4). Secret is
// write-only: stored on the object, NEVER serialized on reads — the wire
// shape carries SecretSet instead (hookWire in http.go).
type Hook struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Secret      string   `json:"secret,omitempty"`
	Events      []string `json:"events"`
	Active      bool     `json:"active"`
	InsecureTLS bool     `json:"insecure_tls,omitempty"`
	CreatedBy   string   `json:"created_by"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Version     int      `json:"version"`
}

// CursorDoc is one repos/<o>/<r>/webhooks/cursors/<id>.json (§5.3).
type CursorDoc struct {
	PublishedSeq int    `json:"published_seq"`
	UpdatedAt    string `json:"updated_at"`
}

// DeliveryEntry is one row of the per-hook deliveries ring (§1.4).
type DeliveryEntry struct {
	Seq        int    `json:"seq"`
	Event      string `json:"event"`
	Status     int    `json:"status"`
	At         string `json:"at"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// DeliveriesDoc is repos/<o>/<r>/webhooks/<id>/deliveries/recent.json.
type DeliveriesDoc struct {
	UpdatedAt string          `json:"updated_at"`
	Entries   []DeliveryEntry `json:"entries"`
}

// ActivityEvent is one repos/<o>/<r>/collab-events/<seq:012x>.json (§5.3):
// the webhook delivery unit and the notification backfill source (§4).
type ActivityEvent struct {
	Seq     int             `json:"seq"`
	Repo    string          `json:"repo"`
	Action  string          `json:"action"`
	Num     int             `json:"num,omitempty"`
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor"`
	Title   string          `json:"title,omitempty"`
	At      string          `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// CollabState is repos/<o>/<r>/meta/collab_state.json: the activity seq
// allocator (P3 two-step: CAS to reserve, Create the event).
type CollabState struct {
	NextSeq int `json:"next_seq"`
}

// SocialDoc is the 07 §4 shape as written until 07 lands: counters stay
// 07-compatible (`watchers` is the COUNT); the 06 §2 watcher array lives
// in `watcher_list` (capped, truncated-flagged) — see Decisions.
type SocialDoc struct {
	Stars             int      `json:"stars"`
	Watchers          int      `json:"watchers"`
	Forks             int      `json:"forks"`
	WatcherList       []string `json:"watcher_list,omitempty"`
	WatchersTruncated bool     `json:"watchers_truncated,omitempty"`
	UpdatedAt         string   `json:"updated_at"`
}

// WatchRecord is users/<principal>/watching/<o>/<r>.json (07 §5 shape).
type WatchRecord struct {
	Repo      string `json:"repo"`
	WatchedAt string `json:"watched_at"`
}

// --- key helpers (bucket-relative) -------------------------------------------

// NotifKey returns users/<principal>/notifications/<id>.json.
func NotifKey(principal, id string) string {
	return "users/" + principal + "/notifications/" + id + ".json"
}

// NotifPrefix returns users/<principal>/notifications/ (LIST root, P5).
func NotifPrefix(principal string) string {
	return "users/" + principal + "/notifications/"
}

// NotifIndexKey returns users/<principal>/notifications/index.json.
func NotifIndexKey(principal string) string { return NotifPrefix(principal) + "index.json" }

// WatchingKey returns users/<principal>/watching/<o>/<r>.json.
func WatchingKey(principal, owner, repo string) string {
	return "users/" + principal + "/watching/" + owner + "/" + repo + ".json"
}

// SocialKey returns repos/<o>/<r>/meta/social.json.
func SocialKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/social.json"
}

// CollabStateKey returns repos/<o>/<r>/meta/collab_state.json.
func CollabStateKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/collab_state.json"
}

// ActivityKey returns repos/<o>/<r>/collab-events/<seq:012x>.json.
func ActivityKey(owner, repo string, seq int) string {
	return fmt.Sprintf("repos/%s/%s/collab-events/%012x.json", owner, repo, seq)
}

// ActivityPrefix returns repos/<o>/<r>/collab-events/.
func ActivityPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/collab-events/"
}

// HookKey returns repos/<o>/<r>/webhooks/<id>.json.
func HookKey(owner, repo, id string) string {
	return "repos/" + owner + "/" + repo + "/webhooks/" + id + ".json"
}

// WebhooksPrefix returns repos/<o>/<r>/webhooks/ (LIST root, P5).
func WebhooksPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/webhooks/"
}

// CursorKey returns repos/<o>/<r>/webhooks/cursors/<id>.json.
func CursorKey(owner, repo, id string) string {
	return "repos/" + owner + "/" + repo + "/webhooks/cursors/" + id + ".json"
}

// CursorsPrefix returns repos/<o>/<r>/webhooks/cursors/.
func CursorsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/webhooks/cursors/"
}

// DeliveriesKey returns the per-hook deliveries ring key.
func DeliveriesKey(owner, repo, id string) string {
	return "repos/" + owner + "/" + repo + "/webhooks/" + id + "/deliveries/recent.json"
}

// NotificationID derives the deterministic §1.1 id: the sync fan-out and
// any replayed backfill re-derive the same key, so a retried Create 412s
// into success. eventSeq is the reserved activity seq (decimal).
func NotificationID(principal, repo string, num int, reason string, eventSeq int) string {
	h := sha256.New()
	h.Write([]byte("notification\x00" + principal + "\x00" + repo + "\x00"))
	h.Write([]byte(fmt.Sprintf("%d\x00%s\x00%d", num, reason, eventSeq)))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// --- service -----------------------------------------------------------------

// RoleService is the narrow P6 surface this package consumes (same shape
// as internal/issues: satisfied by *identity.Service; tests substitute a
// fake). Resolution order and require_read semantics stay owned by 01.
type RoleService interface {
	// Resolve returns the max repo role for p (P6 verbatim).
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	// CheckRead is the require_read gate: nil allows; anonymous-denied
	// reads are ErrUnauthorized (→ real 401), authenticated-but-insufficient
	// are ErrForbidden (→ 403).
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// ProfileProber validates @-mentioned principals (06 §3: a plain GET of
// users/<principal>/profile.json; 404 = no such principal). Satisfied by
// *identity.Service via GetProfile; tests substitute a fake.
type ProfileProber interface {
	GetProfile(ctx context.Context, principal string) (*identity.Profile, error)
}

// TeamReader reads team membership for @org/team expansion (06 §3:
// orgs/<org>/teams/<slug>.json members[] directly, no API hop).
// Satisfied by *identity.Service via GetTeam; tests substitute a fake.
type TeamReader interface {
	GetTeam(ctx context.Context, org, slug string) (*identity.Team, store.Version, error)
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService, injected by composition). Nil falls back to
// anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// Service is the notify store client: fan-out, indexes, activity log,
// webhooks, SSE buses, tasks, retention. Construct with New; Roles,
// Profiles, Teams may be nil in tests that exercise pure paths (nil
// Roles falls back to principal flags; nil Profiles/Teams drops
// mention/team recipients).
type Service struct {
	Store    store.ObjectStore
	Roles    RoleService
	Profiles ProfileProber
	Teams    TeamReader
	Now      func() time.Time

	// RetentionDays overrides DefaultRetentionDays (tests; composition
	// leaves zero for the default — no config key in v1, see Decisions).
	RetentionDays int

	// MaxHooks caps hook configs per repo (CreateHook refuses beyond it
	// with ErrConflict). Zero or negative selects
	// DefaultMaxHooksPerRepo; tests set a small positive cap. No config
	// key in v1 (same convention as RetentionDays).
	MaxHooks int

	// Logger receives emission drop/failure records (06 §4: no silent
	// drops — every reserve/append failure path logs). Nil → discard
	// (same convention as the events bridge Logger).
	Logger *slog.Logger

	ubus  *userBus
	rbus  *repoBus
	tasks *taskTable
	wake  chan string

	// Phase-1 drain state (13 §8, same shape as the #74 import fix):
	// drainCtx is cancelled by Drain; task leaders (webhooks, fanout)
	// derive their store/network work from it, so a wedged store call
	// that honors ctx terminates on drain instead of hanging forever
	// (issue #154). It is independent of any request ctx — delivery
	// outlives a client disconnect exactly as the old WithoutCancel
	// did, but no longer outlives the process drain. draining flips at
	// the same point so new tasks refuse fast (their seqs stay durable
	// for the redrain sweep). wg tracks the leader goroutines; Drain
	// itself is non-blocking (cancel only), mirroring importSvc.Drain.
	// drainCtx/drainCancel are immutable after New (no lock to read);
	// draining is drainMu-guarded; drainCancel is never called under mu.
	drainCtx    context.Context
	drainCancel context.CancelFunc
	drainMu     sync.Mutex
	draining    bool
	wg          sync.WaitGroup

	// fanoutSeen is the redrain high-water (issue #77): repo →
	// highest activity seq the fan-out sweep has probed. In-memory
	// only (rebuilt every process start — the restart sweep re-probes
	// the recent window); guarded by fanoutMu, never held across a
	// store or network call.
	fanoutMu   sync.Mutex
	fanoutSeen map[string]int

	// hookSeen is the webhooks-sweep high-water (issue #156): repo →
	// highest collab_state NextSeq the sweep has scheduled a pass for.
	// hookPending marks repos whose last pass left cursors behind the
	// head (POST failure or a 256-event backlog) plus repos with a new
	// or newly-activated hook not yet delivered. Both are in-memory
	// only (a restart re-passes every repo with activity once, then the
	// watermarks rebuild); guarded by hookMu, never held across a
	// store or network call.
	hookMu      sync.Mutex
	hookSeen    map[string]int
	hookPending map[string]bool
}

// New builds a Service over st.
func New(st store.ObjectStore, roles RoleService) *Service {
	drainCtx, drainCancel := context.WithCancel(context.Background())
	return &Service{
		Store: st, Roles: roles, Now: time.Now,
		ubus: newUserBus(), rbus: newRepoBus(),
		tasks: newTaskTable(), wake: make(chan string, 64),
		fanoutSeen: map[string]int{},
		hookSeen:   map[string]int{}, hookPending: map[string]bool{},
		drainCtx: drainCtx, drainCancel: drainCancel,
	}
}

// Drain enters phase-1 drain for the notify surface (13 §8, law 7,
// same shape as the #74 import fix): in-flight task leaders see
// cancellation promptly (their store/network work derives from
// drainCtx), new tasks refuse fast, and dropped fan-out seqs stay
// durable for the redrain sweep (fanout_pending + no completion
// record). Idempotent; safe to call with no tasks running.
func (s *Service) Drain() {
	s.drainMu.Lock()
	s.draining = true
	s.drainMu.Unlock()
	s.drainCancel() // non-blocking; leaders observe it via drainCtx
}

// Draining reports whether Drain has begun (phase ≥ 1).
func (s *Service) Draining() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.draining
}

// nowUTC is the clock, UTC (RFC 3339 wire timestamps).
func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// log returns the emission logger, discarding when unwired (the events
// bridge Logger convention: nil → discard).
func (s *Service) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// retentionDays resolves the retention window.
func (s *Service) retentionDays() int {
	if s.RetentionDays > 0 {
		return s.RetentionDays
	}
	return DefaultRetentionDays
}

// maxHooks resolves the per-repo hook cap: a positive MaxHooks wins,
// anything else selects the documented default (a misconfigured
// non-positive value fails open to the default, never to uncapped).
func (s *Service) maxHooks() int {
	if s.MaxHooks > 0 {
		return s.MaxHooks
	}
	return DefaultMaxHooksPerRepo
}

// --- role helpers (same shape as internal/issues) ----------------------------

func roleRank(r string) int {
	switch identity.Role(r) {
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
	default:
		return 0
	}
}

func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		return ""
	}
	r, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(r)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	if got := s.roleOf(ctx, owner, repo, p); roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// requireAuth rejects anonymous callers (self-only user routes).
func requireAuth(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}

// requireRead enforces the read gate (identity require_read hook when
// wired; principal flags otherwise).
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
			return fmt.Errorf("unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// --- store helpers ------------------------------------------------------------

// casUpdate is the canonical CAS loop (13 §3, same shape as identity's
// and issues'): read, apply, Update(version), retry-on-412 re-read,
// bounded attempts, then ErrConflict.
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

// putCreate writes an immutable object; a 412 means it already exists
// (idempotent success on the fan-out path).
func (s *Service) putCreate(ctx context.Context, key string, body []byte) error {
	_, err := store.PutBytes(ctx, s.Store, key, body, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	return err
}

// normPrincipal lowercases and trims a principal name for comparison.
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// manifestKey returns repos/<o>/<r>/manifest.pb — the repo-existence
// signal (deleted first on Delete, created first on Create; absent =
// deleted-or-never-existed).
func manifestKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/manifest.pb"
}

// repoAlive reports whether the repo exists. A probe error fails OPEN
// (returns true): a transient store fault must neither mass-hide trays
// nor fail closed on mutations — the CAS loops surface real errors on
// their own reads.
func (s *Service) repoAlive(ctx context.Context, owner, repo string) bool {
	ok, err := store.Exists(ctx, s.Store, manifestKey(owner, repo))
	if err != nil {
		return true
	}
	return ok
}

// repoOf splits an "owner/repo" reference; false when malformed (the
// caller keeps the entry — fail open toward serving).
func repoOf(repo string) (string, string, bool) {
	o, r, ok := strings.Cut(repo, "/")
	if !ok || o == "" || r == "" || strings.Contains(r, "/") {
		return "", "", false
	}
	return o, r, true
}

// sortStrings is insertion sort (small slices only: recipients, teams).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// encode marshals v (wire/persisted JSON). Most callers pass fixed
// shapes, which marshal infallibly in practice — but Emission.Detail is
// composition-supplied (map[string]any, issue #98), so marshal CAN fail.
// Callers propagate the error (CAS closures return it; emission paths
// log-drop it); nothing on the request path panics. The recover closes
// the last panic path absolutely: a value inside the untyped map may
// carry a panicking MarshalJSON, which encoding/json would propagate —
// here it becomes an error instead.
func encode(v any) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			raw, err = nil, fmt.Errorf("notify: encode: %v", r)
		}
	}()
	return json.Marshal(v)
}

// marshalable reports whether v survives json.Marshal (issue #98: the
// cheap screen at the emit entry — Detail is composition-supplied and
// may hold chan/func values). encoding/json itself only errors on such
// values; the recover guards pathological MarshalJSON implementations
// supplied inside the untyped map.
func marshalable(v any) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_, err := json.Marshal(v)
	return err == nil
}
