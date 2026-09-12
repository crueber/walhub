// Package api implements the JSON API, the SSE envelope, the task/ops surface,
// and the two-tier render cache (07_api.md). Handlers depend on two interfaces
// only — RepoView and Tasks — both defined here and implemented by internal/wal
// (via bind_wal.go). This package owns wire shapes only.
//
// Wire conventions (07_api.md §2, normative): errors are plain text
// (text/plain; charset=utf-8), never a JSON envelope; every array field
// serializes as [] when empty (never null); timestamps are RFC 3339 UTC; SHAs
// are always full 40/64-hex; path segments are decoded one at a time.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// SyncLevel mirrors the engine's materialization level (05_wal_engine.md §2).
// The handler files never import internal/wal: bind_wal.go bridges this enum
// onto wal.SyncLevel.
type SyncLevel int

const (
	SyncRefs  SyncLevel = iota // manifest + refs only (every request)
	SyncServe                  // + the pack set the repo serves from
	SyncFull                   // + the full object set (history pack)
)

// String renders the level for logs and sync bookkeeping.
func (l SyncLevel) String() string {
	switch l {
	case SyncRefs:
		return "refs"
	case SyncServe:
		return "serve"
	default:
		return "full"
	}
}

// TaskRecord is the wire shape of a narrated unit of long work (07_api.md
// §12.3; frozen in doc 05 §6.8). Field-for-field identical to wal.TaskRecord —
// bind_wal.go converts, so no handler file imports internal/wal.
type TaskRecord struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Repo      string            `json:"repo"`
	Hostname  string            `json:"hostname"`
	Started   string            `json:"started"`
	Finished  string            `json:"finished,omitempty"`
	ElapsedMS int64             `json:"elapsed_ms"`
	OK        *bool             `json:"ok,omitempty"`
	Summary   string            `json:"summary"`
	Progress  *Progress         `json:"progress,omitempty"`
	LogTail   []string          `json:"log_tail"`
	Params    map[string]string `json:"params,omitempty"`
}

// Progress is one narration packet (07_api.md §9.3). Kind selects the envelope
// event: "notice" ({"text"}), "progress" ({"label","done","total"?,"unit",
// "percent"?}), "task" (a TaskRecord). Shape mirrors wal.Progress.
type Progress struct {
	Kind    string      `json:"-"`
	Text    string      `json:"text,omitempty"`
	Label   string      `json:"label,omitempty"`
	Done    uint64      `json:"done,omitempty"`
	Total   *uint64     `json:"total,omitempty"`
	Unit    string      `json:"unit,omitempty"`
	Percent *float64    `json:"percent,omitempty"`
	Task    *TaskRecord `json:"task,omitempty"`
}

// Task kinds (frozen list, 07_api.md §12.3).
var TaskKinds = []string{
	"materialize", "remote-index", "history-pack", "compact", "bundle",
	"checkpoint", "fsck", "repair", "follow", "rev-index", "sync",
	"rematerialize", "prewarm",
}

// Lane is which of the two API lanes answered (06_server_http.md §3.3).
type Lane int

const (
	LaneAPI Lane = iota
	LaneAPIBrowser
)

// AuthLevel is the per-route gate (07_api.md §13).
type AuthLevel int

const (
	AuthOpen AuthLevel = iota
	AuthRead
	AuthWrite
	AuthAdmin
)

// OpSpec describes one maintenance op (07_api.md §12.2).
type OpSpec struct {
	Op     string    `json:"op"`
	Params []OpParam `json:"params"`
}

// OpParam is one op parameter: a name plus optional allowed values.
type OpParam struct {
	Name   string   `json:"name"`
	Values []string `json:"values,omitempty"`
}

// RepoRegistry is the owner/repo listing surface, answered from the STORE
// (07_api.md §8 — never from a local disk directory).
type RepoRegistry interface {
	// Owners returns sorted owner names; [] when none.
	Owners(ctx context.Context) ([]string, error)
	// Repos returns sorted short repo names; [] for an unknown owner.
	Repos(ctx context.Context, owner string) ([]string, error)
	// OwnerRepoCounts returns the live-repo count per owner (Forgejo #307:
	// the owners/detailed repo_count rail). Same membership and ghost rule
	// as Owners/Repos (manifest-backed repos only — the liveRepos rule);
	// owners with zero live repos are absent, never zero-valued.
	OwnerRepoCounts(ctx context.Context) (map[string]int, error)
	// Exists reports whether the repo is registered.
	Exists(ctx context.Context, id git.RepoId) (bool, error)
	// Create registers a new repo with the given object format; ErrExists-class
	// failures surface as 409.
	Create(ctx context.Context, id git.RepoId, format git.ObjectFormat) error
	// Delete removes the repo (admin).
	Delete(ctx context.Context, id git.RepoId) error
}

// RepoView is the per-repo data access handlers render from (07_api.md §1).
// It fuses refs sync + object access: implementations materialize what they
// need (sync level control) and run the render recipes behind these methods.
// Errors: wrap ErrNotFound for unknown repo/ref/path/sha (→ 404).
type RepoView interface {
	// Sync materializes the repo to the given level (per-request refs sync).
	Sync(ctx context.Context, id git.RepoId, level SyncLevel) error

	// Resolve implements the §9.3 algorithm over the remaining path segments
	// (already decoded, "/"-joined): 2k exact ref lookups, longest prefix,
	// branch beats tag; rev-parse fallback for the first segment.
	Resolve(ctx context.Context, id git.RepoId, rest string) (Resolution, error)

	// Head is the O(1) default-branch head (§9.2); ok=false → {"head":null}.
	Head(ctx context.Context, id git.RepoId) (Ref, bool, error)

	// RefList is one name-sorted page under "heads" or "tags" (§9.2): the
	// implementation asks for n+1 internally and computes more. Tag shas are
	// the peeled commit.
	RefList(ctx context.Context, id git.RepoId, ns string, q RefQuery) ([]Ref, bool, error)

	Tree(ctx context.Context, id git.RepoId, rev, path string) (TreeResult, error)
	Blob(ctx context.Context, id git.RepoId, rev, path string, raw bool) (BlobResult, error)
	Commits(ctx context.Context, id git.RepoId, ref, path string, skip, n int) (CommitPage, error)
	Commit(ctx context.Context, id git.RepoId, sha string) (CommitDetail, error)
	Summary(ctx context.Context, id git.RepoId) (SummaryData, error)

	// Overview is the WAL health dashboard data (07_api.md §12.1).
	Overview(ctx context.Context, id git.RepoId) (OverviewData, error)

	// Settings read/publish (D24). PublishSettings with an empty body returns
	// to host config at a new revision.
	Settings(ctx context.Context, id git.RepoId) (SettingsDoc, error)
	PublishSettings(ctx context.Context, id git.RepoId, body []byte, message, author string) (uint64, error)
	SettingsHistory(ctx context.Context, id git.RepoId) (SettingsHistory, error)
	// HeadSeq is the repo's current WAL head sequence (describe shape).
	HeadSeq(ctx context.Context, id git.RepoId) (uint64, error)

	// PushHistory returns the last N push entries of the live WAL log
	// (policy dry-run input, §10); force is derived per ref.
	PushHistory(ctx context.Context, id git.RepoId, last int) ([]PushRecord, error)
}

// --- overview wire shapes (07_api.md §3 verbatim; §12.1) -----------------------------

// BundleInfo is one bundle object row.
type BundleInfo struct {
	SHA           string    `json:"sha"`
	Size          int64     `json:"size"`
	AtSeq         uint64    `json:"at_seq"`
	Created       time.Time `json:"created"`
	URI           string    `json:"uri"`
	Strategy      string    `json:"strategy"`
	Kind          string    `json:"kind"`
	BaseID        string    `json:"base_id,omitempty"`
	CreationToken string    `json:"creation_token,omitempty"`
	Filter        string    `json:"filter,omitempty"`
	Tips          []string  `json:"tips"`
}

// Suggestion is one ops suggestion the UI can offer to run.
type Suggestion struct {
	Op     string            `json:"op"`
	Params map[string]string `json:"params,omitempty"`
	Reason string            `json:"reason"`
	Auto   bool              `json:"auto,omitempty"`
}

// Health is the overview health block.
type Health struct {
	Status      string       `json:"status"` // ok | degraded | error
	Issues      []string     `json:"issues"`
	Deep        bool         `json:"deep,omitempty"`
	Suggestions []Suggestion `json:"suggestions"`
}

// ManifestInfo is the manifest-derived overview block.
type ManifestInfo struct {
	Version             uint64     `json:"version"`
	NextSeq             uint64     `json:"next_seq"`
	MinSeq              uint64     `json:"min_seq"`
	Segments            []string   `json:"segments"`
	TailEntries         uint64     `json:"tail_entries"`
	Entries             uint64     `json:"entries"`
	Checkpoint          any        `json:"checkpoint,omitempty"`
	Packset             any        `json:"packset,omitempty"`
	AdvertisedBundleURI string     `json:"advertised_bundle_uri,omitempty"`
	LastPush            *time.Time `json:"last_push,omitempty"`
}

// LocalInfo is the local cache state block.
type LocalInfo struct {
	Version    uint64 `json:"version"`
	NextSeq    uint64 `json:"next_seq"`
	Bootstrap  bool   `json:"bootstrap"`
	Reconciled bool   `json:"reconciled"`
	SizeBytes  int64  `json:"size_bytes"`
}

// PacksInfo is the live-packs block.
type PacksInfo struct {
	Live      int    `json:"live"`
	LiveBytes int64  `json:"live_bytes"`
	Pushes    uint64 `json:"pushes"`
}

// PlanSlot is one bundle_plan slot row.
type PlanSlot struct {
	Strategy string `json:"strategy"`
	Kind     string `json:"kind"`
	Slot     int    `json:"slot"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	BundleID string `json:"bundle_id,omitempty"`
}

// BundlePlanInfo is the planned slot table.
type BundlePlanInfo struct {
	Slots       []PlanSlot `json:"slots"`
	Upcoming    []string   `json:"upcoming"`
	Maintainers []string   `json:"maintainers"`
	Orphaned    []string   `json:"orphaned"`
}

// CompactionInfo is one recent COMPACT entry summary.
type CompactionInfo struct {
	Seq      uint64    `json:"seq"`
	At       time.Time `json:"at"`
	Before   int       `json:"packs_before"`
	After    int       `json:"packs_after"`
	Hostname string    `json:"hostname,omitempty"`
}

// NodeInfo is this instance's counters block.
type NodeInfo struct {
	Counters map[string]uint64 `json:"counters"`
}

// OverviewData backs GET …/overview (§3; no-store).
type OverviewData struct {
	Repo        string           `json:"repo"`
	CloneURL    string           `json:"clone_url"`
	SSHCloneURL string           `json:"ssh_clone_url,omitempty"`
	Hostname    string           `json:"hostname"`
	Health      Health           `json:"health"`
	Fsck        *FsckInfo        `json:"fsck,omitempty"` // nil when never audited (issue #209)
	Manifest    ManifestInfo     `json:"manifest"`
	Local       LocalInfo        `json:"local"`
	Packs       PacksInfo        `json:"packs"`
	Bundles     []BundleInfo     `json:"bundles"`
	BundlePlan  BundlePlanInfo   `json:"bundle_plan"`
	Compactions []CompactionInfo `json:"compactions"`
	Node        NodeInfo         `json:"node"`
}

// ErrExists marks an already-present repo (→ 409 on PUT …/api).
var ErrExists = errors.New("already exists")

// ErrPending marks data the wal engine owns but has not wired yet
// (manifest/WAL-backed endpoints); mapped to 503 plain text.
var ErrPending = errors.New("pending: the wal engine does not expose this surface yet")

// Resolution is the §9.3 resolve answer. Revision is the manifest revision the
// resolution was computed at (the render-cache stamp).
type Resolution struct {
	Ref      string `json:"ref"` // full ref name; "" for a raw commit
	SHA      string `json:"sha"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // "branch"|"tag"|"commit"
	Revision uint64 `json:"-"`
}

// Ref is one ref: full name + full sha (peeled for tags).
type Ref struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// RefQuery shapes a refs page (07_api.md §9.2).
type RefQuery struct {
	Prefix string // under the namespace ("refs/heads/" + prefix)
	Q      string // case-insensitive substring on the short name
	After  string // name cursor, strictly greater in byte order
	N      int    // page size (default 100, max 1000)
}

// TreeEntry is one ls-tree row. CommitSHA/CommitTime carry the newest commit
// touching the entry's path (07_api.md §9.4: one batched log walk per tree
// listing); both are omitted when unknown (submodules, capped walks, empty).
// CommitTime is the verbatim git %cI value (RFC 3339).
type TreeEntry struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // blob|tree|commit
	Mode       string `json:"mode"`
	Size       int64  `json:"size"` // -1 for trees/submodules
	SHA        string `json:"sha"`
	CommitSHA  string `json:"commit_sha,omitempty"`
	CommitTime string `json:"commit_time,omitempty"`
}

// Readme is the repo landing readme (§9.4): emitted only when valid UTF-8.
type Readme struct {
	Name     string `json:"name"`
	Contents string `json:"contents"`
}

// TreeResult backs GET …/tree/{rev}[/{path}].
type TreeResult struct {
	Ref     string      `json:"ref"`
	SHA     string      `json:"sha"`
	Path    string      `json:"path"`
	Entries []TreeEntry `json:"entries"`
	Commit  *Commit     `json:"commit,omitempty"`
	Readme  *Readme     `json:"readme,omitempty"`
}

// BlobResult backs GET …/blob/{rev}/{path}. Exactly one of Contents /
// Binary / TooLarge is meaningful (TooLarge wins: no contents fetched).
type BlobResult struct {
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
	Contents []byte `json:"-"`
}

// CommitPage backs GET …/commits.
type CommitPage struct {
	Ref     string   `json:"ref"`
	SHA     string   `json:"sha"`
	Commits []Commit `json:"commits"`
	More    bool     `json:"more"`
}

// Stat is one --numstat row (binary → -1/-1).
type Stat struct {
	Path      string `json:"path"`
	Additions int64  `json:"additions"`
	Deletions int64  `json:"deletions"`
}

// CommitDetail backs GET …/commit/{sha}.
type CommitDetail struct {
	Commit Commit `json:"commit"`
	Stats  []Stat `json:"stats"`
	Patch  string `json:"patch"`
}

// SummaryData backs GET …/api (§9.1).
type SummaryData struct {
	Head *Ref `json:"head"` // the one sanctioned null
	// Description is the per-repo short display string (issue #235), sourced
	// from the `description` key of the WAL-published settings TOML. Always
	// present on the wire ("" = unset); old clients ignore it (14 §14.12).
	Description string `json:"description"`
	Branches    int    `json:"branches"`
	Tags        int    `json:"tags"`
	// Health is the repo-state vocabulary (07_api.md §9.1; issue #209):
	// "empty" (unborn: no resolvable head, zero branches/tags),
	// "healthy", or "degraded" (refs present, cached fsck.pb lists missing
	// objects). Scoped to repo state — distinct from OverviewData.Health,
	// the WAL-dashboard vocabulary ("ok"|"degraded"|"error").
	Health string `json:"health"`
	// MissingTotal rides the degraded state only (0 otherwise): the
	// authoritative missing-object count from the cached fsck.pb report.
	MissingTotal uint64 `json:"missing_total,omitempty"`
}

// Repo health states: the summary vocabulary (issue #209). Old clients
// ignore the field (14 §14.12 additive rule).
const (
	RepoHealthEmpty    = "empty"
	RepoHealthHealthy  = "healthy"
	RepoHealthDegraded = "degraded"
)

// FsckInfo is the read-only fsck.pb projection on overview (07_api.md §12.1;
// issue #209): what the last audit found, plus the derived stall flag and
// the configured repair source. Nil when no audit report exists. Missing is
// the report's bounded sample ([] never null); MissingTotal is authoritative.
type FsckInfo struct {
	MissingTotal  uint64     `json:"missing_total"`
	Missing       []string   `json:"missing"`
	Problems      uint64     `json:"problems"`
	RepairedSeq   uint64     `json:"repaired_seq"`
	At            *time.Time `json:"at,omitempty"`
	Host          string     `json:"host,omitempty"`
	RepairStalled bool       `json:"repair_stalled"`
	Upstream      string     `json:"upstream,omitempty"` // fetch source; "" = none configured
}

// PushRef is one ref update inside a push record.
type PushRef struct {
	Name  string `json:"name"`
	Old   string `json:"old"`
	New   string `json:"new"`
	Force bool   `json:"force"` // derived: merge-base --is-ancestor semantics
}

// PushRecord is one PUSH entry of the live WAL log (policy dry-run input).
type PushRecord struct {
	Seq       uint64    `json:"seq"`
	At        time.Time `json:"at"`
	Principal string    `json:"principal"`
	Atomic    bool      `json:"atomic"`
	Refs      []PushRef `json:"refs"`
}

// SettingsDoc is GET …/settings (§11). Revision 0 = none ever published.
type SettingsDoc struct {
	Revision  uint64    `json:"revision"`
	Author    string    `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
	Message   string    `json:"message"`
	TOML      string    `json:"toml"`
}

// SettingsEntry is one row of GET …/settings/history.
type SettingsEntry struct {
	Seq      uint64    `json:"seq"`
	Revision uint64    `json:"revision"`
	Author   string    `json:"author"`
	Message  string    `json:"message"`
	At       time.Time `json:"at"`
	TOML     string    `json:"toml"`
}

// SettingsHistory is GET …/settings/history.
type SettingsHistory struct {
	MinSeq  uint64          `json:"min_seq"`
	Entries []SettingsEntry `json:"entries"`
}

// Env is the shared state every handler gets (07_api.md §1; constructed once
// at startup). Caches are created by Ready.
type Env struct {
	Store    store.ObjectStore
	Repos    RepoRegistry
	Repo     RepoView
	Tasks    Tasks
	Cfg      *config.Config
	Version  string
	Hostname string
	Instance string

	// SSHKeys is the user-managed SSH public-key registry (17_ssh.md §3):
	// implemented by internal/server over the object store. Nil → the
	// /api/v1/ssh-keys surface answers 503.
	SSHKeys SSHKeyStore

	// Access is the repo read gate (the identity require_read hook,
	// docs/features/01 §4.1). Nil → legacy flag-only read gating.
	// When set, visibility is the read authority for every repo-scoped
	// AuthRead route (Forgejo #345): CheckRead runs BEFORE the
	// anonymous_read flag gate and a public verdict admits the caller
	// even when the flag is false; the flag keeps its meaning for
	// non-repo surfaces only.
	Access ReadAccess

	// CreateOwnerGate is the creation owner-admission gate (Forgejo #346):
	// the owner must equal the principal's own username or be an org the
	// principal belongs to; host admins bypass. Nil → legacy-open (no
	// gate). Foreign owner → 403 naming the allowed owners; probe
	// errors → 503.
	CreateOwnerGate CreateOwnerGate

	// AccessBoot materializes the eager access.json default at placeholder
	// creation (#210 §3). Nil → no eager write (read-time synthesis only).
	AccessBoot AccessBootstrap

	// PlaceholderHints is the same-process adoption hint set (#210 §4):
	// create paths Add, the server Consumes post-push. Nil → the push
	// path issues no marker ops at all (push-budget default).
	PlaceholderHints *PlaceholderHints

	// GroupExpander resolves team:/role: policy spellings at load time
	// (docs/features/01 §6, Seam 3). Nil → no expansion. Wired by
	// composition when internal/identity is compiled in.
	GroupExpander policy.Expander

	// OwnerEdit grants org-role profile edits (Forgejo #234, docs/features/01
	// P6). Nil → the core default only (host admin or name-matched
	// principal). Wired by composition when internal/identity is compiled
	// in (this package never imports it — law 8).
	OwnerEdit OwnerEditor

	// Orgs marks org-owned namespaces on the owners/detailed listing
	// (Forgejo #348): ListOrgs reports every org name; ownersDetailed
	// renders is_org per row from that set in ONE call regardless of
	// owner count (law 6 — no per-owner probe fan-out). Nil → every row
	// renders is_org=false (instances without the identity surface
	// wired). A list error fails open to all-false (display metadata
	// must never fail the listing — the CollabCounts precedent). Wired
	// by composition (cmd/walhub) so this package never imports the
	// feature (law 8).
	Orgs OrgLister

	// MirrorSummary is the pull-only mirror projection (Forgejo #240):
	// non-nil + true → the summary carries the mirror view (upstream
	// URL, schedule, computed next fire, last outcome). Nil → no
	// mirror field (instances without the mirror surface wired). The
	// next fire is COMPUTED at read by the feature; core never stores
	// it. Wired by composition (cmd/walhub) so this package never
	// imports the feature (law 8).
	MirrorSummary func(ctx context.Context, owner, repo string) (MirrorView, bool)

	// CollabCounts is the repo-level open-count projection (Forgejo #319):
	// open issues + open PRs from the shared issues/index.json, read
	// index-first (one exact-key GET; absent index → ok=false). Nil → the
	// summary carries zero counts with no ETag suffix (instances without
	// the collab surface wired). Wired by composition (cmd/walhub) so
	// this package never imports the feature (law 8).
	CollabCounts func(ctx context.Context, owner, repo string) (CollabCounts, bool)

	// RepoVisibility is the per-repo visibility projection (Forgejo #345):
	// "public"|"private" for the summary badge and listing rows, ok=false
	// when unknown. Nil → the projections omit the field (instances
	// without the identity surface wired). Wired by composition
	// (cmd/walhub) so this package never imports the feature (law 8).
	// Missing access.json resolves public (the §10 legacy default), so
	// pre-existing repos badge public, never private-by-omission.
	RepoVisibility func(ctx context.Context, owner, repo string) (vis string, ok bool)

	// RenderCacheBytes is the rendered-immutable LRU budget
	// (cache.render_cache_bytes; default 256 MiB — see 07_api.md §14).
	RenderCacheBytes int64

	// Now is overridable for tests.
	Now func() time.Time

	cache *renderCache
	refs  *refCache
}

// Ready initializes the derived fields; called by Mount/handlers.
func (e *Env) Ready() {
	if e.RenderCacheBytes <= 0 {
		e.RenderCacheBytes = 256 << 20
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.cache == nil {
		e.cache = newRenderCache(e.RenderCacheBytes)
	}
	if e.refs == nil {
		e.refs = newRefCache(4096)
	}
}

// ReadAccess is the repo read gate consulted after principal resolution
// and before any repo-scoped read handler body (the require_read hook,
// docs/features/01 §4.1). Anonymous-denied reads return ErrUnauthorized
// (→ real 401 + WWW-Authenticate: Bearer); authenticated-but-insufficient
// returns ErrForbidden (→ 403).
type ReadAccess interface {
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// mapAccessErr renders a ReadAccess denial: 401 (+Bearer) for anonymous,
// 403 for authenticated-but-insufficient, 503 when identity state is down.
func mapAccessErr(w http.ResponseWriter, aerr *auth.AuthError) {
	switch aerr.Kind {
	case auth.ErrForbidden:
		writePlain(w, http.StatusForbidden, aerr.Why)
	case auth.ErrUnavailable:
		writePlain(w, http.StatusServiceUnavailable, aerr.Why)
	default:
		writePlain(w, http.StatusUnauthorized, aerr.Why)
	}
}

// MirrorView is the pull-only mirror projection on the repo summary
// (Forgejo #240): the stored sidecar fields plus the feature-computed
// next_sync_at ("" when due now) and the due flag. Absent (nil) on
// non-mirrors — never null-vs-missing ambiguity: the field is
// omitempty.
//
// DegradedReason carries the serve-health verdict (issue #320): when the
// repo's objects are currently unservable, the last serve failure's
// reason, so the projection agrees with the summary health field instead
// of advertising a stale "ok". Empty when servable. Old clients ignore
// it (14 §14.12).
type MirrorView struct {
	UpstreamURL         string `json:"upstream_url"`
	Schedule            string `json:"schedule"`
	NextSyncAt          string `json:"next_sync_at,omitempty"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`
	LastResult          string `json:"last_result,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	Due                 bool   `json:"due"`
	DegradedReason      string `json:"degraded_reason,omitempty"`
}

// CollabCounts is the repo-level open-count projection on the repo summary
// (Forgejo #319): the tab-badge numerators. Version is the shared P4 index
// version (bumped on every card upsert by either collab writer) — the
// summary ETag suffix that busts the SWR cache on a close/reopen with no
// ref move (the #235/#240 suffix precedent). The wire carries the counts
// only; the version rides the ETag.
type CollabCounts struct {
	OpenIssues int `json:"open_issues"`
	OpenPulls  int `json:"open_pulls"`
	Version    int `json:"-"`
}

// --- request context ---------------------------------------------------------

type principalCtxKey struct{}

// WithPrincipal attaches the authenticated identity (injected by the server
// middleware; 06_server_http.md §8).
func WithPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalOf returns the request's identity: the injected Principal, else the
// mode default (mode none → everyone is anon with write+admin).
func (e *Env) PrincipalOf(r *http.Request) auth.Principal {
	if p, ok := r.Context().Value(principalCtxKey{}).(auth.Principal); ok {
		return p
	}
	if e.Cfg != nil && e.Cfg.Server.Auth.Mode == "none" {
		return auth.None()
	}
	return auth.Anonymous()
}

// --- plain-text + JSON writers ------------------------------------------------

const (
	ccImmutable = "private, max-age=31536000, immutable"
	ccSWR       = "private, max-age=0, stale-while-revalidate=60"
	ccNoStore   = "no-store"
	ccNoCache   = "no-cache"
)

func itoa(n int) string { return strconv.Itoa(n) }

// writePlain emits a plain-text error body (§2: errors are text/plain, shown
// verbatim in the UI — never a JSON envelope). 401 carries
// WWW-Authenticate: Bearer realm="walgit"; 503 carries Retry-After: 15.
func writePlain(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Del("ETag")
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		h.Set("Retry-After", "15")
	}
	h.Set("Content-Length", itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

// writeJSON emits a JSON body. Slices must be non-nil (the §2 null-safety
// rule); writeJSON enforces it defensively for common slice shapes via the
// constructors the handlers use.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "api: encode: "+err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeCached writes a JSON body with the §4 cache class headers and the
// If-None-Match → 304 path. class: ccImmutable | ccSWR | ccNoStore | ccNoCache.
func writeCached(w http.ResponseWriter, r *http.Request, class, etag string, status int, v any) {
	h := w.Header()
	h.Set("Cache-Control", class)
	if etag != "" {
		h.Set("ETag", `"`+etag+`"`)
		if etagMatch(r.Header.Get("If-None-Match"), etag) {
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, status, v)
}

// etagMatch compares an If-None-Match header against a bare sha: strip quotes
// and weak prefixes, then any-of match.
func etagMatch(header, sha string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.TrimPrefix(p, "w/")
		p = strings.Trim(p, `"`)
		if p == sha {
			return true
		}
	}
	return false
}

// --- errors -------------------------------------------------------------------

// ErrNotFound marks an unknown owner/repo/ref/path/sha → 404 (07_api.md §2).
var ErrNotFound = errors.New("not found")

// mapViewErr renders a RepoView/registry error per §2: 404 for not-found,
// 409 for exists, 503 otherwise — plain text always.
func mapViewErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writePlain(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrExists):
		writePlain(w, http.StatusConflict, err.Error())
	default:
		writePlain(w, http.StatusServiceUnavailable, err.Error())
	}
}

// writeBody writes pre-rendered JSON bytes with cache-class headers and the
// If-None-Match → 304 path (the sha-addressed handlers' shared tail).
func writeBody(w http.ResponseWriter, r *http.Request, class, etag string, status int, body []byte) {
	h := w.Header()
	h.Set("Cache-Control", class)
	if etag != "" {
		h.Set("ETag", `"`+etag+`"`)
		if etagMatch(r.Header.Get("If-None-Match"), etag) {
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// --- auth gates (07_api.md §13) -----------------------------------------------

// gate enforces the route's auth level. 401 carries WWW-Authenticate: Bearer
// realm="walgit"; 403 for authenticated-but-insufficient.
func (e *Env) gate(w http.ResponseWriter, r *http.Request, level AuthLevel) bool {
	if level == AuthOpen {
		return true
	}
	p := e.PrincipalOf(r)
	cfg := e.Cfg
	anonRead := cfg == nil || cfg.Server.Auth.AnonymousRead
	switch level {
	case AuthRead:
		if p.Anonymous && !anonRead {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return false
		}
	case AuthWrite:
		if p.Anonymous && !anonRead {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return false
		}
		if !p.Write {
			writePlain(w, http.StatusForbidden, "write access required")
			return false
		}
	case AuthAdmin:
		if p.Anonymous && !anonRead {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return false
		}
		if !p.Admin {
			writePlain(w, http.StatusForbidden, "admin access required")
			return false
		}
	}
	return true
}

// --- path/query helpers -------------------------------------------------------

// rawSegments splits the raw (still-encoded) path on "/" and decodes each
// segment separately (07_api.md §2: never unescape a joined multi-segment
// string).
func rawSegments(r *http.Request) []string {
	p := r.URL.EscapedPath()
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		d, err := url.PathUnescape(s)
		if err != nil {
			d = s
		}
		out = append(out, d)
	}
	return out
}

// revIsFullSHA reports whether rev is a full 40/64-hex sha (the §4
// sha-addressed cache class test).
func revIsFullSHA(rev string) bool {
	if len(rev) != 40 && len(rev) != 64 {
		return false
	}
	for i := 0; i < len(rev); i++ {
		c := rev[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// refPartOf recovers the addressed rev from a greedy blob/tree tail: rest
// minus the resolved file path (issue #251 — the rev may itself contain
// slashes, so the split comes from Resolve's longest-prefix match, not from
// the first segment). Only a full-sha rev earns the immutable cache class.
func refPartOf(rest, path string) string {
	if path != "" {
		if s, ok := strings.CutSuffix(rest, "/"+path); ok {
			return s
		}
	}
	return rest
}

// isUTF8 reports whether b is valid UTF-8 (and contains no NUL — the blob
// binary test, §9.5).
func isUTF8(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// nonNil returns s, or an empty non-nil slice (§2: arrays serialize as []
// never null).
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
