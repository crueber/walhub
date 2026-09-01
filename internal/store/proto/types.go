// Package proto: canonical Go message types for the WAL (02_storage_protobuf.md §2.2/§2.2.1,
// from the walgit.v1 protobuf schema). Field order follows field numbers; plain structs, no
// getters. Timestamps are the explicit wire type (never time.Time inside messages).
//
// CONTRACT FILE: frozen types — the wire codec (codec.go, owned by the proto implementer)
// encodes/decodes EXACTLY these. Names and field numbers MUST match walgit.v1 so buckets are
// byte-compatible with the Rust implementation.
package proto

import "time"

// WALFormatVersion is the current Manifest.format_version (readers reject unknown values).
const WALFormatVersion uint32 = 1

// Timestamp is google.protobuf.Timestamp on the wire.
type Timestamp struct {
	Seconds int64 `json:"seconds"`
	Nanos   int32 `json:"nanos"`
}

func (t Timestamp) Go() time.Time {
	return time.Unix(t.Seconds, int64(t.Nanos)).UTC()
}

func TimeFromGo(t time.Time) Timestamp {
	if t.IsZero() {
		return Timestamp{}
	}
	return Timestamp{Seconds: t.Unix(), Nanos: int32(t.Nanosecond())}
}

// PackKind (enum).
type PackKind int32

const (
	PackKindObjects  PackKind = 0
	PackKindHistory  PackKind = 1
)

// EntryKind (enum).
type EntryKind int32

const (
	EntryKindUnspecified EntryKind = 0
	EntryKindPush        EntryKind = 1
	EntryKindCompact     EntryKind = 2
	EntryKindRefUpdate   EntryKind = 3
	EntryKindCheckpoint  EntryKind = 4
	EntryKindSettings    EntryKind = 5
)

// String returns the stable wire name (used in events, CLI output, metrics).
func (k EntryKind) String() string {
	switch k {
	case EntryKindPush:
		return "PUSH"
	case EntryKindCompact:
		return "COMPACT"
	case EntryKindRefUpdate:
		return "REF_UPDATE"
	case EntryKindCheckpoint:
		return "CHECKPOINT"
	case EntryKindSettings:
		return "SETTINGS"
	default:
		return "UNSPECIFIED"
	}
}

// Manifest — repos/<o>/<r>/manifest.pb; the CAS'd linearization point.
type Manifest struct {
	FormatVersion uint32            `json:"format_version"` // 1; readers reject unknown values
	Repo          string            `json:"repo"`           // "<owner>/<repo>"
	ObjectFormat  string            `json:"object_format"`  // "sha1" | "sha256"
	HeadSeq       uint64            `json:"head_seq"`       // last committed entry seq (0 = empty)
	MinSeq        uint64            `json:"min_seq"`        // oldest entry still in log_segments
	Checkpoint    *CheckpointRef    `json:"checkpoint,omitempty"`
	LogSegments   []*LogSegmentRef  `json:"log_segments"` // covers [min_seq, head_seq]; ascending, contiguous
	Packs         []*PackRef        `json:"packs"`        // denormalized live pack set, sorted by seq
	UpdatedAt     *Timestamp        `json:"updated_at,omitempty"`
	Writer        string            `json:"writer"`   // instance id that produced this generation
	Revision      uint64            `json:"revision"` // monotonic counter of successful writes (starts 1)
	Settings      *RepoSettings     `json:"settings,omitempty"` // D24: latest inline
}

// RepoSettings — per-repo TOML overrides published into the WAL (≤ 16 KiB).
type RepoSettings struct {
	Toml      string     `json:"toml"`
	Revision  uint64     `json:"revision"` // per-repo settings revision, 1 = first publish
	Author    string     `json:"author"`
	UpdatedAt *Timestamp `json:"updated_at,omitempty"`
	Message   string     `json:"message"`
}

// LogSegmentRef — manifest pointer to one log segment object.
type LogSegmentRef struct {
	Key      string `json:"key"` // repo-relative, e.g. "log/0000000000000042.pb"
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Size     uint64 `json:"size"`  // bytes at manifest-write time (appendable segments grow)
	Sealed   bool   `json:"sealed"`
}

// LogSegment is the in-memory form / one-shot encoding of a sealed segment.
type LogSegment struct {
	Entries []*LogEntry `json:"entries"`
}

// PackRef — one live pack.
type PackRef struct {
	Checksum       string   `json:"checksum"` // pack trailing SHA, hex; key = wal/<checksum>.pack
	PackSize       uint64   `json:"pack_size"`
	IdxSize        uint64   `json:"idx_size"`
	HasRev         bool     `json:"has_rev"`
	HasBitmap      bool     `json:"has_bitmap"`
	ObjectCount    uint64   `json:"object_count"`
	Seq            uint64   `json:"seq"`  // entry that introduced this pack
	Tier           uint32   `json:"tier"` // 0 fresh push pack, 1 medium, 2 base
	HasCommitGraph bool     `json:"has_commit_graph"`
	Kind           PackKind `json:"kind"`
	DerivedFrom    string   `json:"derived_from"` // base checksum for HISTORY packs
}

// LogEntry — one committed WAL entry.
type LogEntry struct {
	Seq        uint64            `json:"seq"`
	Kind       EntryKind         `json:"kind"`
	Pack       *PackRef          `json:"pack,omitempty"`       // PUSH (objects), COMPACT
	Txn        *RefTransaction   `json:"txn,omitempty"`        // PUSH, REF_UPDATE
	Supersedes []string          `json:"supersedes,omitempty"` // COMPACT: checksums removed
	Checkpoint *CheckpointRef    `json:"checkpoint,omitempty"` // CHECKPOINT
	CreatedAt  *Timestamp        `json:"created_at,omitempty"`
	Writer     string            `json:"writer"`
	Meta       map[string]string `json:"meta,omitempty"` // principal, request_id, agent, imported_from…
	Settings   *RepoSettings     `json:"settings,omitempty"` // SETTINGS
}

// RefUpdate — one ref move inside a transaction.
type RefUpdate struct {
	Name              string `json:"name"`  // "refs/heads/main" or "HEAD" (symbolic)
	OldOid            string `json:"old_oid"` // hex; all-zero = "does not exist"
	NewOid            string `json:"new_oid"` // hex; all-zero = delete
	NewSymbolicTarget string `json:"new_symbolic_target,omitempty"`
	NewPeeled         string `json:"new_peeled,omitempty"` // peeled commit for annotated tags
}

// RefTransaction — a ref move set. WAL application is ALWAYS atomic.
type RefTransaction struct {
	Updates     []*RefUpdate `json:"updates"`
	PushOptions []string     `json:"push_options,omitempty"`
	Atomic      bool         `json:"atomic"` // recorded client intent
}

// Checkpoint — checkpoints/<seq>/checkpoint.pb.
type Checkpoint struct {
	Seq          uint64     `json:"seq"`
	ObjectFormat string     `json:"object_format"`
	Packs        []*PackRef `json:"packs"`
	RefsKey      string     `json:"refs_key"`
	RefCount     uint64     `json:"ref_count"`
	BundleKey    string     `json:"bundle_key,omitempty"`
	CreatedAt    *Timestamp `json:"created_at,omitempty"`
	Writer       string     `json:"writer"`
}

// CheckpointRef — manifest pointer to a checkpoint.
type CheckpointRef struct {
	Seq          uint64     `json:"seq"`
	Key          string     `json:"key"` // "checkpoints/<seq:016x>/checkpoint.pb"
	CreatedAt    *Timestamp `json:"created_at,omitempty"` // drives the time trigger without a fetch
	FirstStateAt *Timestamp `json:"first_state_at,omitempty"` // earliest WAL state ever (carried forward)
	AsOf         *Timestamp `json:"as_of,omitempty"`          // state is "as of" the newest folded entry
}

// RefSnapshot — checkpoints/<seq>/refs.pb; sorted by name, no duplicates.
type RefSnapshot struct {
	Seq          uint64     `json:"seq"`
	ObjectFormat string     `json:"object_format"`
	Refs         []*Ref     `json:"refs"`
	HeadTarget   string     `json:"head_target"` // symbolic target of HEAD
	CreatedAt    *Timestamp `json:"created_at,omitempty"`
}

// Ref — one ref in a snapshot.
type Ref struct {
	Name   string `json:"name"`
	Oid    string `json:"oid"`
	Peeled string `json:"peeled,omitempty"` // peeled tag target if annotated tag
}

// Lease — leases/<name>.pb; the only cross-instance mutex (CAS + TTL).
type Lease struct {
	Holder     string     `json:"holder"`
	Purpose    string     `json:"purpose"`
	AcquiredAt *Timestamp `json:"acquired_at,omitempty"`
	ExpiresAt  *Timestamp `json:"expires_at,omitempty"`
	Epoch      uint64     `json:"epoch"` // incremented on every heartbeat/steal
}

// BundleList — bundles/list.pb (CAS'd, NOT immutable).
type BundleList struct {
	Mode      string          `json:"mode"`      // "all" | "any" (git bundle.mode)
	Heuristic string          `json:"heuristic"` // "creationToken"
	Bundles   []*BundleEntry  `json:"bundles"`
	UpdatedAt *Timestamp      `json:"updated_at,omitempty"`
	Skipped   []*SkippedSlot  `json:"skipped,omitempty"` // closed slots measured and NOT cut
}

// SkippedSlot — a closed slot verdict, final per (strategy, slot, base_id).
type SkippedSlot struct {
	Strategy string     `json:"strategy"`
	Slot     uint64     `json:"slot"`
	BaseID   string     `json:"base_id"` // "" for full / no-state
	AsOfSeq  uint64     `json:"as_of_seq"` // 0 = no state
	Reason   string     `json:"reason"`    // "too-small: N commits (min M)" | "no state as of the slot"
	At       *Timestamp `json:"at,omitempty"`
}

// BundleEntry — one advertised bundle.
type BundleEntry struct {
	ID            string     `json:"id"`     // stable id for bundle.<id>.uri
	Key           string     `json:"key"`    // repo-relative object key
	Strategy      string     `json:"strategy"`
	Kind          string     `json:"kind"`   // "full" | "incremental"
	CreationToken uint64     `json:"creation_token"` // = slot epoch seconds (0 for pre-slot bundles)
	Seq           uint64     `json:"seq"`  // WAL seq the bundle was created from
	Size          uint64     `json:"size"`
	BaseID        string     `json:"base_id,omitempty"`
	CreatedAt     *Timestamp `json:"created_at,omitempty"`
	Version       string     `json:"version,omitempty"` // store version tag → HTTP ETag
	Tips          []*Ref     `json:"tips,omitempty"`
	Slot          uint64     `json:"slot"`
	Filter        string     `json:"filter,omitempty"` // "" | "blob:none"
}

// FsckReport — fsck.pb; overwritten, never replayed.
type FsckReport struct {
	Seq          uint64     `json:"seq"`
	At           *Timestamp `json:"at,omitempty"`
	Host         string     `json:"host"`
	Missing      []string   `json:"missing,omitempty"` // bounded list
	MissingTotal uint64     `json:"missing_total"`
	Problems     uint64     `json:"problems"` // count only
	ElapsedSecs  float64    `json:"elapsed_secs"`
	RepairedSeq  uint64     `json:"repaired_seq"`
}

// RepoCatalog — optional meta/repos.pb (bucket root; not required for correctness).
type RepoCatalog struct {
	Repos     []string   `json:"repos"`
	UpdatedAt *Timestamp `json:"updated_at,omitempty"`
}

// MaintainerHeartbeat — bucket root maintain/<host>.pb.
type MaintainerHeartbeat struct {
	Host        string     `json:"host"`
	Repos       []string   `json:"repos"`
	Exclude     []string   `json:"exclude,omitempty"`
	MaxPackByte uint64     `json:"max_pack_bytes"`
	Disk        string     `json:"disk"` // "tmpfs" | "ssd"
	StartedAt   *Timestamp `json:"started_at,omitempty"`
	LastPassAt  *Timestamp `json:"last_pass_at,omitempty"`
	LastUnit    string     `json:"last_unit"` // "<repo> <kind> <detail>"
	Passes      uint64     `json:"passes"`
}
