# 02 — Storage & protobuf: bucket keys, wire codec, log framing, ObjectStore, CAS
> Source: MASTER_RUST_SPEC.md §5.1, §5.2, §5.3, §5.4, §4.1, §4.10 · Status: normative for the walhub Go implementation.

This doc specifies the bucket data model (key layout, protobuf wire encoding, log framing, seq
semantics) and the Go object-store contract that every backend (GCS, S3, memory) implements behind
one interface. It also specifies the hand-rolled protobuf wire codec in `internal/store/proto` —
**no `google.golang.org/protobuf`** — and the generic CAS helper.

Everything here is byte-compat-critical: a bucket written by the Rust implementation must be
readable by walhub and vice versa. When this doc and MASTER_RUST_SPEC.md §5 disagree on the *format*,
§5 wins and this doc is a spec bug.

Package: `internal/store` (+ `internal/store/proto`). Module: `git.packden.us/crueber/walhub`.
Dependencies used by this package: **stdlib only** (`encoding/binary`, `sort`, `io`, `context`,
`errors`, `fmt`, `time`, `sync`).

---

## 2.1 Bucket key layout (complete, byte-for-byte from §5.1)

Everything repo-scoped lives under `repos/<owner>/<repo>/`. **Everything except `manifest.pb`,
`bundles/list.pb`, `leases/*` is immutable** (content-addressed or seq-addressed; written with
`PutMode` Create + immutable cache headers). All keys below are shown repo-relative; the store
prepends the configured `store.prefix` (normalized to end with `/`).

| Key (repo-relative) | Kind | Written by | Meaning |
|---|---|---|---|
| `manifest.pb` | protobuf `Manifest` | publishers (CAS) | **The linearization point** |
| `log/<first_seq:016x>.pb` | framed `LogEntry` stream | publishers (Create) | Immutable log segment (regional shape); zonal variant is appendable (readers tolerate a partial trailing frame) |
| `wal/<checksum>.pack` | git packfile | publishers | Content-addressed by the pack's trailing SHA (hex) |
| `wal/<checksum>.idx` | pack index | publishers | Pair of the pack |
| `wal/<checksum>.rev` | reverse index | compaction/base rebuild/rev-index unit | Optional; ≥ 250 k objects without one is repaired |
| `wal/<checksum>.bitmap` | pack bitmap | base rebuild | Tier-2 base |
| `wal/<checksum>.commit-graph` | split commit-graph layer | base rebuild / import / commit-graph unit | Chain base for readers |
| `checkpoints/<seq:016x>/checkpoint.pb` | protobuf `Checkpoint` | checkpoint writers | Pack set + pointer |
| `checkpoints/<seq:016x>/refs.pb` | protobuf `RefSnapshot` | checkpoint writers | Full ref state at seq |
| `checkpoints/<seq:016x>/<sha>.bundle` | git bundle | import (optional) | Pre-rendered full bundle |
| `bundles/list.pb` | protobuf `BundleList` | bundle scheduler (CAS) | bundle-uri advertisement state |
| `bundles/<strategy>/<slotUTC>-<sha1-of-content>.bundle` | git bundle | bundle builds | Immutable; ETag = checksum |
| `leases/<name>.pb` | protobuf `Lease` | lease users (CAS+TTL) | `compact`, `bundle-<strategy>` |
| `policy.json` | JSON | admin API/CLI | Push policy (NOT on the WAL) |
| `lfs/objects/<aa>/<bb>/<oid>` | bytes | LFS upload / read-through | sha256-addressed (`aa`=oid[0..2], `bb`=oid[2..4]); oid must be 64 hex chars |
| `events/cursor.json` | JSON `{"published_seq": N, "updated_at": RFC3339}` | events bridge (CAS) | Durable acknowledged WAL seq |
| `fsck.pb` | protobuf `FsckReport` | fsck unit (Overwrite) | Last connectivity audit |
| `cache/api/v1/<sha1-of-cache-key>.json` | JSON | web API (Create) | Shared render cache of immutable API answers |
| bucket root `maintain/<host>.pb` | protobuf `MaintainerHeartbeat` | maintainer (Overwrite) | Who maintains what, capacity, liveness |
| bucket root `meta/repos.pb` | protobuf `RepoCatalog` | optional | Catalog (not required for correctness) |
| `<key>.part/{i:04}`, `<key>.part/mid{g:04}` | bytes | striped upload | Temp parts, deleted after compose |

Normative rules carried over from §5.1:

- `<first_seq:016x>` and `<seq:016x>` are the decimal seq zero-padded to 16 hex-width digits in
  **lowercase hex** (`fmt.Sprintf("%016x", seq)` — e.g. seq 66 → `0000000000000042`). Rust used the
  same `{:016x}` formatting; keys MUST match byte-for-byte.
- Control plane vs bulk (§4.2, relevant for backend transport choice — see 03_store_backends.md):
  every key whose last segment ends `.pb` or `.json` is **control plane**; keys under `wal/`,
  `bundles/`, `lfs/` and every ranged read are **bulk**. The classifier function
  `store.IsBulkKey(key string) bool` MUST exist even for backends that don't need it.
- Writers MUST treat all keys marked immutable above as Create-only; an existing immutable key with
  different bytes is a `Corrupt` condition at the reader, never silently overwritten.

## 2.2 Protobuf schema (canonical)

The canonical wire schema is the Rust spec's `walgit/v1/wal.proto` (`syntax = "proto3"`,
`package walgit.v1`, `WAL_FORMAT_VERSION = 1`). It is reproduced here **verbatim and normative** —
message names, field numbers, field types, enum values, and the append-only rule are the wire
contract shared with the Rust implementation. Go does not use protoc codegen; §2.3 specifies the
hand-rolled codec that implements exactly this schema.

```protobuf
syntax = "proto3";
package walgit.v1;
import "google/protobuf/timestamp.proto";

message Manifest {
  uint32 format_version = 1;        // 1; readers reject unknown values
  string repo = 2;                  // "<owner>/<repo>"
  string object_format = 3;         // "sha1" | "sha256"
  uint64 head_seq = 4;              // last committed entry seq (0 = empty repo)
  uint64 min_seq = 5;               // oldest entry still in log_segments; == checkpoint.seq+1 when checkpointed
  CheckpointRef checkpoint = 6;     // optional
  repeated LogSegmentRef log_segments = 7; // covers [min_seq, head_seq]; ascending, contiguous, non-overlapping
  repeated PackRef packs = 8;       // DENORMALIZED live pack set after all entries <= head_seq; sorted by seq
  google.protobuf.Timestamp updated_at = 9;
  string writer = 10;               // instance id that produced this generation
  uint64 revision = 11;             // monotonic counter of successful manifest writes (starts 1)
  RepoSettings settings = 12;       // D24: latest per-repo settings, INLINE (every refs sync sees it free)
}

message RepoSettings {            // published to the WAL; TOML restricted to allowed sections, <= 16 KiB
  string toml = 1;
  uint64 revision = 2;            // per-repo settings revision, 1 = first publish
  string author = 3;
  google.protobuf.Timestamp updated_at = 4;
  string message = 5;             // free-text reason (history)
}

message LogSegmentRef {
  string key = 1;                 // repo-relative, e.g. "log/0000000000000042.pb"
  uint64 first_seq = 2;
  uint64 last_seq = 3;
  uint64 size = 4;                // bytes at manifest-write time (appendable segments grow)
  bool sealed = 5;                // regional segments always true
}

message LogSegment { repeated LogEntry entries = 1; } // decoded form / whole-object encoding

message PackRef {
  string checksum = 1;            // pack trailing SHA, hex; key = wal/<checksum>.pack
  uint64 pack_size = 2;
  uint64 idx_size = 3;
  bool has_rev = 4;
  bool has_bitmap = 5;
  uint64 object_count = 6;
  uint64 seq = 7;                 // entry that introduced this pack
  uint32 tier = 8;                // 0 fresh push pack, 1 medium (compacted), 2 base
  bool has_commit_graph = 9;      // wal/<checksum>.commit-graph exists
  PackKind kind = 10;             // OBJECTS (default) | HISTORY
  string derived_from = 11;       // base pack checksum for HISTORY packs
}
enum PackKind { PACK_KIND_OBJECTS = 0; PACK_KIND_HISTORY = 1; }

enum EntryKind {
  ENTRY_KIND_UNSPECIFIED = 0;
  ENTRY_KIND_PUSH = 1;            // zero or one pack + a ref transaction
  ENTRY_KIND_COMPACT = 2;         // one new pack superseding `supersedes`; refs unchanged
  ENTRY_KIND_REF_UPDATE = 3;      // ref-only change (deletes, force-updates, admin ops)
  ENTRY_KIND_CHECKPOINT = 4;      // checkpoint written; packs unchanged
  ENTRY_KIND_SETTINGS = 5;        // settings changed (manifest carries latest; this is history)
}

message LogEntry {
  uint64 seq = 1;
  EntryKind kind = 2;
  PackRef pack = 3;               // PUSH (when objects pushed), COMPACT
  RefTransaction txn = 4;         // PUSH, REF_UPDATE
  repeated string supersedes = 5; // COMPACT only: checksums removed from the live set
  CheckpointRef checkpoint = 6;   // CHECKPOINT only
  google.protobuf.Timestamp created_at = 7;
  string writer = 8;
  map<string, string> meta = 9;   // provenance: principal, request_id, agent, push-options, imported_from…
  RepoSettings settings = 10;     // SETTINGS only
}

message RefUpdate {
  string name = 1;                // "refs/heads/main" or "HEAD" (symbolic update)
  string old_oid = 2;             // hex; all-zero = "does not exist"
  string new_oid = 3;             // hex; all-zero = delete
  string new_symbolic_target = 4; // HEAD symbolic target; oids then empty
  string new_peeled = 5;          // peeled commit for annotated-tag updates (replicas advertise ^{} without objects)
}

message RefTransaction {
  repeated RefUpdate updates = 1;
  repeated string push_options = 2;
  bool atomic = 3;                // recorded client intent; WAL application is ALWAYS atomic
}

message Checkpoint {              // checkpoints/<seq>/checkpoint.pb
  uint64 seq = 1;
  string object_format = 2;
  repeated PackRef packs = 3;     // pack set fully representing the repo at seq (typically 1 base)
  string refs_key = 4;            // "checkpoints/<seq>/refs.pb"
  uint64 ref_count = 5;
  string bundle_key = 6;          // optional rendered bundle
  google.protobuf.Timestamp created_at = 7;
  string writer = 8;
}

message CheckpointRef {
  uint64 seq = 1;
  string key = 2;
  google.protobuf.Timestamp created_at = 3;     // drives the time trigger without a fetch
  google.protobuf.Timestamp first_state_at = 4; // earliest WAL state ever (carried forward)
  google.protobuf.Timestamp as_of = 5;          // created_at of newest folded entry
}

message RefSnapshot {
  uint64 seq = 1;
  string object_format = 2;
  repeated Ref refs = 3;          // sorted by name, no duplicates
  string head_target = 4;         // symbolic target of HEAD
  google.protobuf.Timestamp created_at = 5;
}
message Ref { string name = 1; string oid = 2; string peeled = 3; }

message Lease {
  string holder = 1; string purpose = 2;
  google.protobuf.Timestamp acquired_at = 3;
  google.protobuf.Timestamp expires_at = 4;
  uint64 epoch = 5;               // incremented on every heartbeat/steal
}

message BundleList {              // bundles/list.pb (CAS'd, NOT immutable)
  string mode = 1;                // "all" | "any" (git bundle.mode)
  string heuristic = 2;           // "creationToken"
  repeated BundleEntry bundles = 3;
  google.protobuf.Timestamp updated_at = 4;
  repeated SkippedSlot skipped = 5; // closed slots measured and NOT cut
}
message SkippedSlot {
  string strategy = 1; uint64 slot = 2; string base_id = 3;
  uint64 as_of_seq = 4;           // 0 = no state
  string reason = 5;
  google.protobuf.Timestamp at = 6;
}

message BundleEntry {
  string id = 1;                  // stable id used in bundle.<id>.uri
  string key = 2;                 // repo-relative object key
  string strategy = 3;
  string kind = 4;                // "full" | "incremental"
  uint64 creation_token = 5;      // monotonic; = slot epoch seconds (0 for pre-slot bundles)
  uint64 seq = 6;                 // WAL seq the bundle was created from
  uint64 size = 7;
  string base_id = 8;             // bundle this incremental is based on ("" for full)
  google.protobuf.Timestamp created_at = 9;
  string version = 10;            // store version tag at upload → HTTP ETag
  repeated Ref tips = 11;         // ref tips contained
  uint64 slot = 12;               // calendar slot (epoch of the fire time)
  string filter = 13;             // "" = none; "blob:none" = blobless family
}

message FsckReport {              // fsck.pb; overwritten, never replayed
  uint64 seq = 1;
  google.protobuf.Timestamp at = 2;
  string host = 3;
  repeated string missing = 4;    // bounded list
  uint64 missing_total = 5;
  uint64 problems = 6;            // count only
  double elapsed_secs = 7;
  uint64 repaired_seq = 8;
}

message RepoCatalog { repeated string repos = 1; google.protobuf.Timestamp updated_at = 2; }

message MaintainerHeartbeat {     // bucket root maintain/<host>.pb
  string host = 1;
  repeated string repos = 2;
  repeated string exclude = 3;
  uint64 max_pack_bytes = 4;
  string disk = 5;                // "tmpfs" | "ssd"
  google.protobuf.Timestamp started_at = 6;
  google.protobuf.Timestamp last_pass_at = 7;
  string last_unit = 8;           // "<repo> <kind> <detail>"
  uint64 passes = 9;
}
```

Codegen notes carried over: the schema is **append-only** (never remove or renumber a field);
proto is the wire AND the bucket format; `WAL_FORMAT_VERSION = 1` and readers reject unknown
`format_version` values.

### 2.2.1 Go message types

One Go struct per message in `internal/store/proto`, named identically (`Manifest`, `LogEntry`,
`PackRef`, …). Rules:

- Field order in the struct follows field numbers; exported fields; no getters (plain structs).
- `google.protobuf.Timestamp` → struct `Timestamp { Seconds int64; Nanos int32 }` in the same
  package (never `time.Time` inside messages — wire fidelity is explicit; convert to `time.Time` at
  the edges: `func (t Timestamp) Go() time.Time` / `func TimeFromGo(time.Time) Timestamp`).
- `uint64` stays `uint64`, `uint32` stays `uint32`, `double` is `float64`, `bytes` is `[]byte`,
  enums are `int32` named constants (`PackKindObjects PackKind = 0`, `EntryKindPush EntryKind = 1`, …).
- `map<string,string>` (only `LogEntry.meta`) → `Meta map[string]string`.
- Message-typed fields that are semantically optional (`Manifest.checkpoint`, `LogEntry.pack/txn/
  checkpoint/settings`) are pointers (`*CheckpointRef`) and omitted from the wire when nil.
- Repeated scalars/strings are slices; **proto3 empty-vs-absent is not distinguishable on the wire
  and this codec MUST treat them as equal** (both encode as nothing, both decode as zero value).

## 2.3 Hand-rolled wire codec (`internal/store/proto`)

The codec is a fixed, schema-shaped encoder/decoder — not a reflection-based general protobuf
library. It implements the proto3 wire format exactly: same tags, same varint rules, same map
entry encoding as any conformant implementation.

### 2.3.1 Wire rules

| Element | Encoding |
|---|---|
| Tag | `key = (field_number << 3) \| wire_type`, varint-encoded. Wire types: 0 varint, 1 64-bit, 2 length-delimited, 5 32-bit. |
| `uint32`/`uint64` field | wire type 0, value as **unsigned** varint (base-128, little-endian groups, ≤ 10 bytes). |
| `bool` field | wire type 0; `true` = 1, `false` is default ⇒ MUST NOT be written. |
| `enum` field | wire type 0, varint of the int32 value. |
| `string` field | wire type 2; varint byte length then UTF-8 bytes (no NUL terminator). |
| `bytes` field | wire type 2; varint length then raw bytes. |
| `double` field | wire type 1; 8 bytes little-endian IEEE 754 (`math.Float64bits`). |
| submessage field | wire type 2; varint length of the **encoded submessage**, then the submessage bytes. |
| repeated field | one tag+value per element, in slice order (NOT packed for this schema's fields — the Rust codec uses unpacked encoding for `repeated string`/messages; for any `repeated` scalar field the encoder writes unpacked and the decoder accepts both packed and unpacked). |
| `map<string,string>` | one tag (wire type 2) per entry; each entry is a submessage: field 1 = key (string, may be omitted when `""`), field 2 = value (string, may be omitted when `""`). Decoder synthesizes `""` for an omitted side. |
| `Timestamp` field | encoded as a submessage with field 1 = `seconds` (int64 varint, wire type 0) and field 2 = `nanos` (int32 varint, wire type 0); `nanos` ≤ 0 omitted when 0; a nil/zero Timestamp is omitted entirely from the parent. |

Decoder rules:

- Unknown field numbers: skip by wire type (varint, 8 bytes, length-delimited skip, 4 bytes) —
  forward compatibility with future schema versions. Unknown wire types 3/4 (groups): decode error.
- Truncated input (tag cut off, length runs past end, value runs past end) → decode error
  (`ErrTruncated`).
- A varint longer than 10 bytes, or a non-minimal varint over 10 bytes → decode error.
- Duplicate field occurrences: scalars/enums — last wins; strings/bytes — last wins; submessages —
  the codec **merges by last-wins at field granularity** (conformant proto3 behavior for
  non-repeated singular fields); repeated fields concatenate in encounter order.
- Zigzag: **this schema has no `sint32`/`sint64` fields**, so no zigzag is used anywhere; the codec
  must still expose `PutZigZag`/`ZigZag` helpers for completeness but no schema field calls them.
  If a future amendment adds a signed field, it MUST use zigzag and this table gets a row.

### 2.3.2 Encoder/decoder shape

Fixed-codegen style, one encode and one decode function per message, hand-written:

```go
package proto

// Manifest is the doc 02 §2.2 canonical schema, field for field.
type Manifest struct {
    FormatVersion uint32
    Repo          string
    ObjectFormat  string
    HeadSeq       uint64
    MinSeq        uint64
    Checkpoint    *CheckpointRef
    LogSegments   []*LogSegmentRef
    Packs         []*PackRef
    UpdatedAt     *Timestamp
    Writer        string
    Revision      uint64
    Settings      *RepoSettings
}

// Size returns the exact encoded byte length (must equal len(Marshal(m)).
func (m *Manifest) Size() int { /* sum: varint field tags + lengths */ }

func (m *Manifest) Marshal() []byte {
    buf := make([]byte, 0, m.Size())
    return m.AppendTo(buf) // Appends tags+fields; returned slice owns the bytes
}
func (m *Manifest) AppendTo(buf []byte) []byte {
    if m == nil { return buf }
    if m.FormatVersion != 0 {
        buf = appendUvarint(buf, 1<<3|0)
        buf = appendUvarint(buf, uint64(m.FormatVersion))
    }
    // ... fields 2..12 in order; strings/bytes/submessages: appendUvarint(len) then payload
    return buf
}
func (m *Manifest) Unmarshal(b []byte) error {
    // linear scan; unknown fields skipped per §2.3.1; returns ErrTruncated / ErrInvalid
}
```

- `Marshal`/`AppendTo` MUST produce the same bytes as the Rust implementation for the same message
  value — field order ascending by number, default-valued singular fields omitted, no packed
  encoding where the Rust codec doesn't pack, map entries in **sorted key order** (this is the one
  place Rust maps are unordered; sorted order is deterministic and byte-compatible in content,
  though map entry ordering is not semantically significant to any conformant reader).
- Decoders MUST NOT require sorted map order or any canonical ordering — accept whatever a
  conformant encoder wrote.

### 2.3.3 Golden fixtures (wire compatibility proof)

`internal/store/proto/testdata/golden/` holds checked-in byte fixtures that cross-validate this
codec against a reference protobuf implementation **once, offline** (generate them with a one-off
local run of a real protobuf toolchain — e.g. `protoc --encode` piped against the canonical
`.proto`; the tool itself is a dev-time-only dependency, never imported by the build):

- `manifest_full.bin`, `manifest_min.bin` — one manifest with every field set, one with only
  required-by-semantics fields.
- `log_entry_all_kinds/*.bin` — one LogEntry per `EntryKind` value.
- `log_segment_frames.bin` — a framed segment (§2.4) with 3 entries.
- `maps.bin`, `timestamps.bin`, `large_varints.bin` — map entries, Timestamp with nanos, and
  64-bit varint boundary values (0, 1, 127, 128, 2^32-1, 2^32, 2^63, 2^64-1).
- A `.golden.json` manifest beside each binary file documenting the decoded expectation so failures
  are diffable.

Normative test (`TestGoldenWireCompat`, see 15_testing.md): decode each `.bin` fixture with this
codec → assert decoded values match the `.golden.json` → re-encode → assert **byte-identical** to
the fixture. These fixtures are the proof that walhub objects round-trip byte-identical with the
Rust implementation. A fixture that fails is a codec bug, never a fixture bug, unless the
`.golden.json` prose disagrees with the binary — then regenerate the pair offline and re-check in.

### 2.3.4 Concurrency

- Hazard: shared mutable scratch buffers across goroutines (pool reuse racing).
- Avoidance: `Marshal`/`Unmarshal` are pure functions over their arguments — no package-level
  mutable state, no shared buffers, no sync needed. Decoding a segment while an appender goroutine
  extends the byte slice is prevented by ownership: the frame decoder (§2.4) reads from an
  `io.Reader` it exclusively owns; appenders write to the store, never to a reader's buffer.
- Hazards and idioms beyond this package: see 13_concurrency.md.

## 2.4 Log framing (§5.3)

A log segment object body is a sequence of frames:

```
frame := uvarint(len(entry_bytes)) || entry_bytes
```

- Encoding: appends only. For a `[]LogEntry`, encode each entry and prefix its byte length as an
  unsigned varint. There is no frame header, no checksum, no magic — the manifest's `size` field and
  the seq range are the integrity bounds.
- Decoding: `DecodeFrames(r io.Reader, cb func(*LogEntry) error) error`:
  1. Read a varint. If `io.EOF` at position 0 of a frame boundary → clean end.
  2. If the varint or the following `len` bytes are incomplete (EOF mid-frame) → **stop, return
     nil**: a partial trailing frame is tolerated, left unconsumed, NOT an error — appendable
     (zonal) segments may be read while growing.
  3. Unmarshal the frame body. A corrupt *complete* frame (body length complete but unmarshal
     fails, or a truncated entry whose length was complete but bytes short) → decode error
     (`ErrCorruptFrame`), surfaced to the caller — never silently skipped.
  4. Frame length cap: 32 MiB. A varint larger than that → `ErrCorruptFrame` (bounds a corrupt
     stream's allocation).
- `LogSegment` (repeated entries) is the in-memory form and the one-shot encoding of a sealed
  segment: `EncodeSegment(entries)` = all frames concatenated; `DecodeSegment(b)` = decode all
  frames, error on any incomplete trailing data (a sealed segment has no partial tail — a partial
  tail there is `ErrCorruptFrame`).

```go
// AppendFrames appends the uvarint-prefixed frames for entries to buf.
func AppendFrames(buf []byte, entries []*LogEntry) []byte

// DecodeFrames streams frames from r, invoking cb per complete entry.
// Returns (n, err): n = complete frames consumed. Partial trailing frame is not an error.
func DecodeFrames(r io.Reader, cb func(*LogEntry) error) (int, error)
```

### Concurrency

- Hazard: a reader decoding a segment object concurrently with a writer appending to the same
  store object (zonal segments). The reader may legitimately see a partial tail.
- Avoidance: the reader owns its byte stream exclusively (single `Get` range or full body → one
  `io.Reader` fed to `DecodeFrames`); the append path writes to the store, never into the reader's
  buffer. No shared state, no locks needed between the two. On stores without append semantics,
  re-Get the object to observe growth; do not hold a decoded snapshot and expect it to update.
- The seq-burn grace/probe loop (§5.4) polls the store; it must not spin a goroutine per probe —
  one goroutine, `time.Sleep` backoff, bounded by the burn cap (8); see 05_wal_engine.md for the
  ladder and 13_concurrency.md for the bounded-polling rule.

## 2.5 Sequence number semantics (§5.4, summary for the store layer)

- `head_seq` = last committed entry; entries strictly increasing, NOT dense. Gaps: a writer crashed
  between log PUT and manifest CAS; after a 100 ms × 3-probe grace the seq is burned (claimed by a
  new writer with a fresh segment; cap 8 consecutive burns → `Corrupt`). Burned segments are
  CAS-deleted only after a later commit by the same writer ("sweep"), or left in place when the CAS
  outcome was ambiguous.
- `min_seq` = oldest replayable entry (below is folded into the checkpoint); `min_seq =
  checkpoint.seq + 1` when a checkpoint exists.
- Log segments in the manifest cover `[min_seq, head_seq]`, ascending, contiguous, non-overlapping.
  An import publishes with `min_seq = seq + 1` (no history before the import; the checkpoint IS the
  origin).

The store layer's responsibility is only: `Version` tokens for CAS, and the key layout. Burn/probe
logic belongs to `internal/wal` (05_wal_engine.md).

## 2.6 ObjectStore interface (Go)

Direct translation of §4.1. Backends (GCS, S3, memory) implement it exactly once each; callers
never see backend types.

```go
package store

import (
    "context"
    "io"
    "time"
)

// Version is an opaque CAS token. GCS = object generation (decimal string);
// S3 = ETag with quotes stripped; memory = global counter.
// Callers compare for equality only; NEVER parse it.
type Version string

// AccelTarget is what a trusted edge uses to fetch one object (§8.6).
type AccelTarget struct {
    URL           string
    Authorization string // optional bearer; empty = not set
}

// ObjectMeta describes one stored object.
type ObjectMeta struct {
    Key     string
    Size    int64   // whole object size, even for range reads
    Version Version // "" for backends with no versioning
}

// GetOptions shapes a Get.
type GetOptions struct {
    IfNoneMatch Version    // equal → NotModified (304-style)
    IfMatch     Version    // different → ErrPreconditionFailed
    Range       *[2]int64  // half-open [start, end); nil = whole object
}

// GetResult is the discriminated union returned by Get.
type GetResult interface{ isGetResult() }

type NotModified struct{ Version Version }
type Object struct {
    Meta ObjectMeta
    Body io.ReadCloser // caller closes
}

func (NotModified) isGetResult() {}
func (Object) isGetResult()     {}

// PutMode is the write discipline.
type PutMode int

const (
    PutOverwrite PutMode = iota // default
    PutCreate                   // only if absent
    PutUpdate                   // CAS: only if current version equals PutOptions.IfVersion
)

// PutOptions carries the write discipline and caching hints.
type PutOptions struct {
    Mode      PutMode
    IfVersion Version      // required for PutUpdate, ignored otherwise
    ContentType string
    Immutable bool         // ⇒ long cache headers
}

// PutBody is the payload. Exactly one variant is set.
type PutBody struct {
    Bytes  []byte
    Stream io.Reader // KNOWN length required (StreamLen > 0)
    StreamLen int64
    File   string      // local path, streamed by the backend
}

// PutResult is what a successful Put returns.
type PutResult struct{ Meta ObjectMeta }

type ObjectStore interface {
    Backend() string // "gcs" | "s3" | "memory"

    Get(ctx context.Context, key string, opts GetOptions) (GetResult, error)
    Head(ctx context.Context, key string) (*ObjectMeta, error) // nil = absent
    Put(ctx context.Context, key string, body PutBody, opts PutOptions) (ObjectMeta, error)
    Delete(ctx context.Context, key string, ifVersion Version) error
    // Delete is CAS: with ifVersion != "" it deletes only if the version matches.
    // Unconditional delete of an absent key is Ok (no error).

    // List returns a lazy stream of ObjectMeta, lexicographic, strictly after startAfter.
    List(ctx context.Context, prefix, startAfter string, fn func(ObjectMeta) error) error
    // ListPrefixes returns distinct "<prefix><segment>/" (delimiter listing), sorted.
    ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error

    // SignedGetURL is a presigned direct-download URL; nil if unsupported/signing fails.
    SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error)
    // AccelTarget is the edge-offload target (§8.6); nil if unsupported.
    AccelTarget(ctx context.Context, key string) (*AccelTarget, error)

    SupportsCompose() bool
    ComposeIsNative() bool
    // Compose: server-side concat of 1..=32 sources, in order, into dst.
    Compose(ctx context.Context, dst string, sources []string, opts PutOptions) (ObjectMeta, error)
}
```

Error taxonomy — sentinel errors + a wrapped type (Go idiom; callers use `errors.Is`/`errors.As`):

```go
type StoreErrorKind int

const (
    ErrKindNotFound StoreErrorKind = iota
    ErrKindPreconditionFailed
    ErrKindRetryable
    ErrKindInvalidArgument
    ErrKindOther
    ErrKindCorrupt
)

type StoreError struct {
    Kind    StoreErrorKind
    Key     string
    Current Version // PreconditionFailed: the version that beat the CAS, if known
    Err     error   // underlying cause, if any
}

func (e *StoreError) Error() string
func (e *StoreError) Unwrap() error { return e.Err }

func IsNotFound(err error) bool
func IsPreconditionFailed(err error) bool
func IsRetryable(err error) bool
```

Normative behaviors carried from §4.1:

- `PreconditionFailed` is protocol-normal (CAS contention). It MUST NOT be counted as an error
  outcome in telemetry — it is the success path of a lost race.
- All keys are wrapped by the store prefix (`Prefixed` wrapper; prefix normalized with trailing `/`)
  before hitting a backend.
- Extensions implemented as package-level helpers over the interface:
  `GetBytes(ctx, s, key, opts) ([]byte, ObjectMeta, error)`,
  `GetIfChanged(ctx, s, key, known Version) (GetResult, error)`,
  `PutBytes(ctx, s, key, b []byte, opts PutOptions) (ObjectMeta, error)`,
  `Exists(ctx, s, key) (bool, error)`.
- `Compose` takes 1..=32 sources; sources left in place; honors Create/Update/Overwrite on dst.
  Backends without native compose implement it as multipart-from-source-ranges
  (03_store_backends.md); callers wanting parallel single-file upload use striped upload instead
  when `ComposeIsNative()` is false (03_store_backends.md §striped).

### Memory store (tests, §4.5)

`internal/store/memstore.go` — the test backend, also used by the FaultStore simulation wrapper
(15_testing.md):

- `BTreeMap`-ordered storage → Go: a `map[string][]byte` plus a sorted key index kept by a simple
  `sort.SearchStrings` on a maintained `[]string` slice, guarded by one `sync.Mutex`
  (the store is the lock; see Concurrency below).
- Versions: a global monotonic counter (`atomic.Uint64`), unique across keys (mimics generations);
  formatted as decimal string.
- Implements the full interface incl. `Compose` (concat under the lock, honors CAS via `Put`)
  and range clamping (clamp start/end to size; start ≥ size → empty body).
- Test knobs: `Latency time.Duration` (artificial per-op sleep), `FakeObjectURLs bool`
  (`AccelTarget` returns a GCS-like URL + bearer for edge tests), `SigningFails bool`
  (`SignedGetURL` errors like VPC-SC).

```go
func NewMemory() *Memory  // implements ObjectStore; *Memory adds test knobs
```

### Concurrency

- Hazard: goroutines sharing one memory-store instance while tests exercise CAS races.
- Avoidance: **the store is the lock** — every mutation takes the single mutex; the CAS loop (§2.7)
  holds no lock across store calls, it re-reads on 412, so the mutex is never held by a CAS
  attempt. No lock ordering issues exist because there is exactly one lock per store instance and
  nothing acquires two stores' locks in sequence while holding the first (the FaultStore wrapper
  delegates, never nests).
- Hazard: unbounded parallel List callbacks racing shared test state.
- Avoidance: List/ListPrefixes invoke the caller callback **synchronously in the caller's
  goroutine** — the caller decides parallelism (typically none, or its own bounded pool, see
  13_concurrency.md bounded-parallelism rule).

## 2.7 `casUpdate` — the generic CAS helper (§4.10)

```go
// CasUpdate reads the object at key, passes the decoded value (nil if absent) to f, and
// writes f's result with the correct PutMode (Create if absent, Update(current) otherwise).
// On PreconditionFailed (412): re-read and retry — NO SLEEP, the retry itself is counted.
// On Retryable errors: backoff 5 ms → 100 ms (doubling with jitter).
// Exceeds maxRetries CAS conflicts or retryable failures → ErrRetriesExhausted.
// f returning (nil, nil) aborts the update (nothing written, no error).
func CasUpdate[T any](
    ctx context.Context,
    s ObjectStore,
    key string,
    maxRetries int,
    decode func([]byte) (*T, error),
    encode func(*T) ([]byte, error),
    f func(cur *T) (*T, error),
) (*T, error)
```

Semantics (normative, mirrors §4.10 exactly):

1. Read (absent → `f(nil)`).
2. `f` returns the new value, or aborts (nil, nil → no write, return nil).
3. Put with `PutCreate` when the read was absent, `PutUpdate(IfVersion=current)` otherwise.
4. On 412 (`PreconditionFailed`): **re-read and retry immediately — no sleep**. Contention means
   someone else just advanced the value; sleeping only widens the race window and slows the
   retry. The retry is counted against `maxRetries`.
5. On `Retryable`: sleep with backoff starting 5 ms, doubling, capped 100 ms, +jitter, then retry
   (does NOT consume a CAS-conflict retry).
6. `maxRetries` exhausted → `ErrRetriesExhausted` (wraps the last error).
7. Context cancellation checked at the top of every attempt; cancelled → ctx error immediately.

Used by bundle list, policy, catalog (08_bundles.md, 10_maintenance.md). The manifest publishers
implement their own inline ladder (05_wal_engine.md §6.4) and do NOT use this helper.

Example call site (shape an implementer can copy):

```go
newList, err := store.CasUpdate(ctx, st, key, 8,
    proto.UnmarshalBundleList,            // func([]byte) (*BundleList, error)
    func(v *proto.BundleList) ([]byte, error) { return v.Marshal(), nil },
    func(cur *proto.BundleList) (*proto.BundleList, error) {
        if cur == nil { cur = &proto.BundleList{} }
        cur.Bundles = append(cur.Bundles, entry)
        cur.UpdatedAt = proto.TimeFromGo(time.Now())
        return cur, nil
    })
if errors.Is(err, store.ErrRetriesExhausted) { /* treat as contention/failure */ }
```

### Concurrency

- Hazard: N instances (or N goroutines in one instance) doing read-modify-write on one CAS'd key
  (bundle list, policy, cursor) — a naive mutex-protected in-process loop does nothing for
  cross-instance contention and a blocking lock serializes unrelated work.
- Avoidance: **the store is the lock — no mutex at all.** The loop's correctness comes from the
  conditional PUT: only one writer's version matches. 412 → immediate re-read (no sleep), counted
  retries, `ErrRetriesExhausted` as the backstop. This is lock-free in the Go sense: no lock is
  ever held; a goroutine waiting on another's write is just doing another read. Note the
  read-modify-write window is intentionally re-opened on every retry — callers' `f` MUST be a pure
  function of the current value (re-derive, don't accumulate outside state).
- Hazard: retryable-error backoff sleeping long enough to starve a request goroutine.
- Avoidance: the backoff is capped at 100 ms and context-cancelled; the loop runs on the caller's
  goroutine and callers on hot paths SHOULD wrap it in their own bounded worker (13_concurrency.md).
- Never hold a store lease (§4.9, 03_store_backends.md) while running `CasUpdate` on a key guarded
  by that lease unless the lease's heartbeat keeps it alive — otherwise a slow CAS ladder can
  outlive the lease and two writers interleave "legitimately". See 05_wal_engine.md for which keys
  are lease-guarded vs CAS-only.

## 2.8 Compatibility callout

**Wire encoding MUST round-trip byte-identical with the Rust implementation.** Concretely:

- Same key layout (§2.1) — byte-for-byte, including the `%016x` hex padding.
- Same protobuf wire bytes (§2.3) for the same message values — proven by the checked-in golden
  fixtures (§2.3.3), generated once offline against a reference protobuf implementation.
- Same framing (§2.4): uvarint-length-prefixed frames, partial-tail tolerance for appendable
  segments.
- Same Version semantics per backend (generation string / ETag-without-quotes / counter).
- A walhub-written bucket and a walgit-written bucket are interchangeable: walhub can adopt a
  bucket after a walgit crash mid-publish (partial tail) and vice versa.

---

## Decisions & deviations from the Rust design

- **No `google.golang.org/protobuf`** — hand-rolled fixed-schema codec in `internal/store/proto`; the
  canonical `.proto` text stays the wire contract, and golden fixtures generated once against a real
  protobuf toolchain prove byte equality. Rationale: dependency minimization is the rewrite's first
  law; the schema is small, closed, and version-frozen, so a reflection library buys nothing.
- **No protoc codegen at build time** — message structs and `Marshal`/`Unmarshal` are written by hand
  (or generated once by a dev-only script whose output is checked in). Rationale: keeps the build
  hermetic (stdlib-only), and the schema is append-only/frozen so hand maintenance is low-risk.
- **`Timestamp` as an explicit `{Seconds, Nanos}` struct** rather than `time.Time` inside messages.
  Rationale: makes the wire format visible and testable at the codec boundary; `time.Time`'s
  monotonic clock and location are meaningless on the wire and could silently round-trip wrong.
- **Errors as a typed `StoreError` with `errors.Is/As` sentinels** instead of a Rust-style enum.
  Rationale: Go idiom; `PreconditionFailed` stays protocol-normal (never telemetry-error) exactly as
  the Rust spec requires.
- **`GetResult` as a Go interface union** (`NotModified` / `Object`) instead of a Rust enum —
  same discriminated semantics, implemented with two small types. Rationale: idiomatic Go sum type;
  exhaustive handling is enforced by the two-variant `isGetResult()` marker.
- **List callbacks instead of lazy iterators** — the Rust `list()` returns a lazy stream; in Go the
  interface takes a `fn func(ObjectMeta) error` callback so each backend owns its paging and the
  caller's goroutine does no extra allocation. Rationale: avoids inventing an iterator abstraction
  per backend; callback-with-error is the stdlib pattern (`filepath.WalkDir`).
- **Frame length cap of 32 MiB** added to the frame decoder (not in the Rust spec). Rationale:
  bounds the allocation a corrupt varint can trigger; the largest legitimate `LogEntry` (settings
  ≤ 16 KiB + meta + txn) is orders of magnitude below it.
- **Map entries encoded in sorted key order** (Rust maps are unordered). Rationale: deterministic
  output makes fixtures stable and log-diffable; map entry order is semantically irrelevant to any
  conformant decoder, so this is compatibility-safe, not a deviation.
- **`ErrKindCorrupt` added to the error taxonomy** (Rust maps corruption elsewhere). Rationale: the
  frame decoder and `wal/<checksum>.*` readers need one explicit kind for bucket-corruption cases
  (bad frame, unknown format_version) so callers can distinguish "bucket is wrong" from
  "key is absent".
- **Memory store keeps one mutex instead of a lock-free design.** Rationale: it is the test double
  the FaultStore wraps; a single lock is trivially correct, and CAS contention tests exercise the
  store's *version* behavior (which is what the Rust spec's global-counter mimics), not lock
  contention. Real backends (doc 03) do their own concurrency.
- **`casUpdate` split into `decode`/`encode`/`f` functions rather than one generic trait.** Rationale:
  Go generics can't express Rust's trait bounds cleanly; explicit function args keep the helper
  usable with hand-rolled codec types and make the pure-function requirement of `f` visible at the
  call site.
