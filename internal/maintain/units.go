// Package maintain: the maintainer loop (docs/go/10_maintenance.md) — a
// self-healing background engine that performs exactly one bounded unit of
// work per assigned repo per pass (or none), narrated as a task,
// lease-guarded against other instances, and observed via heartbeats and
// Prometheus metrics.
//
// The package codes against the narrow interfaces defined here (Engine, Repo,
// TaskRunner, BundlePlanner, Leaser, FsckRunner, FollowFetcher);
// the real binding onto internal/wal + internal/git lives in bind_wal.go, with
// TODO-INTEGRATION markers where the concrete engine surface drifts from
// docs/go/05_wal_engine.md's exported contract.
package maintain

import (
	"context"
	"errors"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Task kinds (§3.2 step 3; §6.8 of doc 05).
const (
	KindCheckpoint = "checkpoint"
	KindRepair     = "repair"
	KindBundle     = "bundle"
	KindCompact    = "compact"
	KindRevIndex   = "rev-index"
	KindFsck       = "fsck"
	KindFollow     = "follow" // the follow loop's kind; NEVER a unit (§8)
)

// Outcome is a unit result (§3.2 step 6).
type Outcome string

const (
	OutcomeOK        Outcome = "ok"
	OutcomeError     Outcome = "error"
	OutcomeTimeout   Outcome = "timeout"
	OutcomeWrongHost Outcome = "wrong-host"
	OutcomeHeld      Outcome = "held"
	OutcomeIdle      Outcome = "idle"
)

const (
	// unitCap is the §3.2 step 4 wait cap: the pass moves on after one hour
	// while the unit's own goroutine keeps running, discoverable in the task
	// table.
	unitCap = time.Hour

	// leaseSkew is the §4.9 steal tolerance (now < expires_at + skew).
	leaseSkew = 2 * time.Second

	// revIndexThreshold is the §10 object-count gate below which packs
	// intentionally stay rev-less.
	revIndexThreshold uint64 = 250_000

	// repairBatch is the §9.2 oid batch size.
	repairBatch = 500

	// fsckMissingBound is the FsckReport.missing list cap (§9.1).
	fsckMissingBound = 100_000
)

// ErrLeaseHeld reports a live lease the unit must not wait on (§3.3: never
// waits, never retries within the pass).
var ErrLeaseHeld = errors.New("lease held by another instance")

// ErrNotWired marks a binding that awaits integration (TODO-INTEGRATION in
// bind_wal.go); the unit reports outcome error until the engine surface lands.
var ErrNotWired = errors.New("TODO-INTEGRATION: engine surface not wired")

// TaskLogger is the narration surface handed to unit bodies.
type TaskLogger interface {
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

// TaskRunner is the (repo,kind) single-flight task registry (§7): a second
// start JOINS the running task. Cross-instance exclusivity is the lease, not
// the registry.
type TaskRunner interface {
	Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, t TaskLogger) error) error
}

// PreparedPack is a pack ready to publish (bound onto wal.PreparedPack).
type PreparedPack struct {
	Checksum    string // pack trailing SHA, hex
	PackPath    string // local file path ("" for pack-less publishes)
	IdxPath     string
	PackSize    uint64
	IdxSize     uint64
	ObjectCount uint64
	Tier        uint32
}

// RefsView is a point-in-time refs fold (bound onto wal.RefsView).
type RefsView struct {
	Seq        uint64
	Refs       []git.RefEntry // name-sorted
	HeadTarget string
}

// Repo is the per-repo handle surface the units exercise (bound onto
// wal.RepoHandle + *git.LocalRepo).
type Repo interface {
	ID() string
	Dir() string // serving copy path
	// Local is the on-disk bare repository handle for git machinery.
	Local() *git.LocalRepo
	// Prefix is the repo-relative store key prefix ("repos/<o>/<r>/").
	Prefix() string
	// Manifest returns a stable manifest copy + its CAS version.
	Manifest() (*proto.Manifest, string)
	// SyncRefs refreshes the refs-level view (cheap; no pack materialization).
	SyncRefs(ctx context.Context) error
	// RefValues returns the current local ref tips (name → oid).
	RefValues(ctx context.Context) (map[string]string, error)
	ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error)
	RefsAtSeq(ctx context.Context, seq uint64) (*RefsView, error)
	RefsAsOf(ctx context.Context, at time.Time) (*RefsView, error)
	// WriteCheckpoint is the wal checkpoint writer (§11; refs-level, idempotent).
	WriteCheckpoint(ctx context.Context, trigger string) error
	// PublishCompact publishes a COMPACT entry (fold / base / add-pack result).
	PublishCompact(ctx context.Context, pack *PreparedPack, supersedes []string, meta map[string]string) (uint64, error)
	// AddPack publishes a pack as a COMPACT entry superseding nothing.
	AddPack(ctx context.Context, path, checksum string, tier uint32, meta map[string]string) error
	// AnnotatePack retrofits .rev/.bitmap/.commit-graph flags (manifest-only
	// CAS, no log entry, head_seq unchanged).
	AnnotatePack(ctx context.Context, checksum string, hasRev, hasBitmap, hasCommitGraph bool) error
	// PublishRefs publishes ref moves as an ordinary PUSH entry.
	PublishRefs(ctx context.Context, txn *proto.RefTransaction, meta map[string]string) (uint64, error)
	// TryLockPacks takes the repo handle's pack_mutex with try-lock semantics
	// (§6.1 b): readers must never queue behind maintenance.
	TryLockPacks() (unlock func(), ok bool)
	// GitOps is the git machinery layer (bound onto *git.Layer).
	GitOps() GitOps
}

// GitOps is the git machinery the units exercise (bound onto *git.Layer; all
// subprocesses honor ctx so drain kills them).
type GitOps interface {
	// GeometricRepack: git repack -d --geometric=<factor> --write-midx
	// [--write-bitmap-index] [--keep-pack …] (§6.1 exact argv).
	GeometricRepack(ctx context.Context, repo *git.LocalRepo, factor int, bitmap bool, keepPacks []string) (*git.PackDiff, error)
	// FullRepack: git repack -a -d --threads=0 --write-bitmap-index
	// --write-midx [--keep-pack …] after deleting stray *.keep (§6.2 step 3).
	FullRepack(ctx context.Context, repo *git.LocalRepo, keepPacks []string) (*git.PackDiff, error)
	// WriteCommitGraph: git commit-graph write --reachable --split=replace
	// [--changed-paths]; returns the trailing chain checksum copied out as
	// wal/<checksum>.commit-graph (§6.2 step 3).
	WriteCommitGraph(ctx context.Context, repo *git.LocalRepo, changedPaths bool, sideDir string) (string, error)
	// HistoryPack builds the blobless history pack (§6.2 step 3); "" when the
	// repo has no refs.
	HistoryPack(ctx context.Context, repo *git.LocalRepo, base string) (string, error)
	// FetchObjectsAsPack is the §7.9 repair helper: 500-oid batches from
	// upstream, pack exactly the requested oids, verify EVERY requested oid
	// is in the resulting idx (a refused want is an error, never a hole).
	FetchObjectsAsPack(ctx context.Context, repo *git.LocalRepo, u git.UpstreamSpec, oids []string) (string, error)
	// Snapshot parses the repo's ref state (name-sorted).
	Snapshot(repo *git.LocalRepo) (*git.RefSnapshot, error)
}

// Engine is the WAL surface the maintainer drives (bound onto wal.Registry).
type Engine interface {
	// Repos lists the repo ids in registration order (§3.2 step 2).
	Repos() []string
	Open(ctx context.Context, id string) (Repo, error)
	// Store is the bucket (leases, heartbeats, fsck.pb, GC sweeps).
	Store() store.ObjectStore
	Tasks() TaskRunner
	HostConfig() *config.Config
	InstanceID() string
}

// Slot is one planned bundle slot (§4 unit 3 / §5; internal/bundle owns the
// planner and the builders).
type Slot struct {
	Strategy string
	Kind     string // "full" | "incremental"
	Slot     uint64
	State    string // built|missing|pending|blocked|too-small|skipped|unavailable|wrong-host
	BundleID string
	Detail   string
}

// BundlePlanner is the internal/bundle seam (§5; the same planner the
// `bundle plan` CLI uses).
type BundlePlanner interface {
	// Plan runs the retention pass, settles closed slots, and returns the
	// slot table (strategies in config order; oldest missing slot first).
	Plan(ctx context.Context, repo string, eff *config.Config, m *proto.Manifest, now time.Time) ([]Slot, error)
	// Build runs bundle strategy=<s> slot=<n>; false = built nothing.
	Build(ctx context.Context, repo string, s Slot) (bool, error)
	// PreviousFire returns the strategy schedule's latest fire time ≤ now
	// (the base-rebuild window start, §6.2 trigger 4).
	PreviousFire(s config.BundleStrategy, now time.Time) time.Time
}

// Leaser takes leases/<name>.pb via the CAS ladder (§3.3/§4.9): the only
// cross-instance mutex. A unit finding its lease held reports OutcomeHeld and
// moves on — never waits, never retries within the pass.
type Leaser interface {
	// Acquire returns the release func. skew is the steal tolerance
	// (leases/compact.pb uses leaseSkew; bundle leases keep the historical
	// absence of the skew tolerance, 0).
	Acquire(ctx context.Context, name, holder, purpose string, ttl, skew time.Duration) (release func(), err error)
}

// FsckRunner is the connectivity audit seam (§9.1: git fsck
// --connectivity-only --no-dangling over the serving copy).
type FsckRunner interface {
	Fsck(ctx context.Context, binary, dir string) (missing []string, problems int, err error)
}

// FollowFetcher is the upstream-follow scratch seam (§8.2/§8.3). The real
// implementation owns the persistent scratch <cache.dir>/follow/<o>/<n>.git
// with alternates into the serving objects dir.
type FollowFetcher interface {
	// Fetch stages ours into refs/follow (the delta-request base), fetches
	// the upstream refs from url, and returns the upstream tips per followed
	// ref. tokenEnv names the env var holding the upstream token.
	Fetch(ctx context.Context, repo, url, tokenEnv string, ours map[string]string, refs []string) (upstream map[string]string, err error)
	// AncestorOf reports whether old is an ancestor of (or equal to) new —
	// the §8.3 fast-forward rule.
	AncestorOf(ctx context.Context, repo, old, new string) (bool, error)
}
