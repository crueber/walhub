# 04 — Git layer (`internal/git`)
> Source: MASTER_RUST_SPEC.md §7 (7.1–7.9), with §8.4 gates and §15 config keys · Status: normative for the walhub Go implementation.


`internal/git` owns every interaction with the `git` binary: pack ingest, ref storage, pkt-line
advertisements, receive-pack and upload-pack flows, repack/commit-graph maintenance plumbing, bundle
primitives, and upstream-repair/follow helpers. Git is ALWAYS the subprocess `git` binary with exact
argv — never go-git or any VCS library (hard dependency rule). Zero third-party imports in this
package; the concurrency playbook is `13_concurrency.md`.

Interfaces this package exposes (all cross-doc references by file name only):

```go
// internal/git
type Layer struct { /* binary path, scratch root, config, blocking pool */ }
func (l *Layer) Ingest(ctx, repo *Repo, pack io.Reader, maxBytes int64, thin, fsck bool) (*IngestResult, error)
func (l *Layer) ApplyRefTxn(ctx, repo *Repo, txn []RefUpdate, checkOld bool) error
func (l *Layer) Snapshot(repo *Repo) (*RefSnapshot, error)
func (l *Layer) Advertisement(repo *Repo, svc Service, v2 bool, version string) ([]byte, error)
func (l *Layer) LsRefs(repo *Repo, args LsRefsArgs) ([]byte, error)
func (l *Layer) UploadPack(ctx, repo *Repo, body io.Reader, out io.Writer, protocol string) error   // --stateless-rpc (HTTP framing)
func (l *Layer) UploadPackSSH(ctx, repo *Repo, body io.Reader, out io.Writer, protocol string) error  // no --stateless-rpc (17_ssh.md §2)
func (l *Layer) ParsePushRequestStream(repo *Repo, r io.Reader) (*PushRequest, io.Reader, error)      // commands only; pack stays a stream (17_ssh.md §5)
func (l *Layer) IngestStream(ctx, repo *Repo, pack io.Reader, maxBytes int64, thin, fsck bool) (*IngestResult, error) // pack piped into index-pack (17_ssh.md §5)
```

Errors are sentinel-wrapped values the callers map: `ErrMaxBytes`, `ErrPackRejected`, `ErrMissingObject`,
`ErrRefConflict`, `ErrTooManyWants`. The WAL publish path (`05_wal_engine.md`) and HTTP layer
(`06_server_http.md`) consume these; policy evaluation (`policy.json`, §14) happens in `internal/policy`,
not here.

## 1. Repo identity and local layout (§7.1)

### 1.1 RepoId validation

`RepoId{Owner, Name string}`; parse from `owner/name` or `owner/name.git` (strip one trailing `.git`).
Per part, ALL of:

| Rule | Value |
|---|---|
| Charset | ASCII `[A-Za-z0-9._-]` only (reject non-ASCII, whitespace, `/`, `+`, `%`, `:`) |
| Length | 1..=100 chars |
| No leading `.` | reject `.foo` (also blocks hidden dirs) |
| Not `..` | exact match rejected |

`ValidateRepoPath(s string) (RepoId, error)` is the single entry point; every route handler MUST call it
before touching the filesystem or the store. The store prefix is `repos/<owner>/<repo>/` (doc 02) and the
local dir is `<cache.dir>/<owner>/<name>.git`.

### 1.2 Local bare layout

The local directory is a **standard bare repo** readable by stock git (and by any future in-process
engine): `objects/pack/*`, loose refs + `packed-refs`, `HEAD`, `config`.

Init sequence for a new repo:

```
git init --bare --object-format=sha256    # ONLY when the repo's format is sha256
git init --bare                           # for sha1 repos (no flag)
```

Then write repo-local config (append to `config`; idempotent re-init rewrites the same keys):

```ini
[uploadpack]
	allowFilter = true
	allowAnySHA1InWant = true
	allowSidebandAll = true
[pack]
	writeReverseIndex = true
```

Then write `HEAD` directly: `ref: refs/heads/main\n`.

Object formats are sha1/sha256 everywhere: 40/64-hex oids, all-zero zero-oids (`strings.Repeat("0", n)`),
the `object-format=<fmt>` capability, and the per-repo format persisted in the manifest/checkpoint/snapshot
(doc 02/05). Every oid type in this package is `type Oid string` validated by format.

#### Subprocess
`git init` is synchronous and fast: run via the shared exec helper (§2) with a 30 s
`context.WithTimeout`. No blocking-pool needed, but count it against the pool anyway for uniform
accounting.

## 2. Subprocess discipline and the blocking pool (normative for EVERY git call)

Every git spawn in this package follows ONE shape:

```go
cmd := exec.CommandContext(ctx, l.Binary, args...)   // ctx always has a timeout
cmd.Env = []string{"GIT_DIR=" + gitDir, /* extra env below */}
cmd.Dir = workDir
// stdin/stdout discipline:
//   - feed stdin from a helper goroutine, then cmd.StdinPipe().Close()
//   - drain stdout with io.Copy (to the consumer or io.Discard) on the calling goroutine
//   - drain stderr into a bounded ring buffer (capped 8 KiB, kept for error text)
//   - ONLY THEN cmd.Wait()
```

**The deadlock rule**: never `Wait()` while a stdin writer and a stdout reader both live on the same
goroutine and neither is closed; git blocks writing to a full stdout pipe while we block writing stdin.
Concretely: large-stdin commands (`index-pack`, `upload-pack`, `update-ref --stdin`, `bundle create`,
`pack-objects`) get a feeder goroutine that `io.Copy`s from the source and closes the pipe (close errors
are swallowed — the child may have exited); the parent reads stdout to EOF, then `Wait`s.

Environment variables used across this package (set them all explicitly; do not inherit the ambient env
except `PATH`): `GIT_DIR`, `GIT_TERMINAL_PROMPT=0` (every call — git must never hang on a credential
prompt), `GIT_TRACE2_EVENT` (ingest only), `GIT_PROTOCOL` (upload-pack only), credential-helper config
passed as `-c` argv (upstream helpers only). `git.binary` (default `"git"`) supplies the executable path.

**Blocking pool.** Rust used `spawn_blocking`; Go's equivalent is a bounded semaphore of goroutines:

```go
type Pool struct{ sem chan struct{} }        // capacity = git.max_git_procs (default: 4 * GOMAXPROCS)
func (p *Pool) Run(ctx context.Context, fn func() error) error {
    select {
    case p.sem <- struct{}{}:
        defer func() { <-p.sem }()
        return fn()
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

### Concurrency
- **Hazard:** unbounded concurrent git processes exhaust CPU/memory (each `upload-pack` can be ~RSS of the
  pack); a push handler holding a pool slot forever starves the server.
- **Avoidance:** every heavyweight git exec (`index-pack`, `upload-pack`, `repack`, `commit-graph`,
  `pack-objects`, connectivity pipeline, `bundle create`) runs inside `Pool.Run`. Per-repo request
  parallelism is additionally capped by the `server.max_concurrent_per_repo` semaphore taken by the HTTP
  layer BEFORE the handler (doc 06). Timeouts: ingest 600 s (config `git.ingest_timeout`), upload-pack and
  connectivity bound by the request context, maintenance commands 1800 s. Context cancellation kills the
  process (`exec.CommandContext` sends SIGKILL); scratch cleanup still runs (§3). Channel/pipe ownership:
  the feeder goroutine owns and closes the stdin pipe; the caller owns and drains stdout. Canonical
  patterns: `13_concurrency.md` §"bounded parallelism", §"drain".

## 3. Pack ingest (receive path) (§7.2)

`Ingest` runs `git index-pack` in a **scratch git-dir per ingest — never the serving copy**.

### 3.1 Scratch dir

Path: `<repo>/walgit-ingest-<pid>-<nanos>/` under the repo's local dir. Contents created by hand (no
subprocess): `objects/pack/` (empty), `objects/info/alternates` containing one line: the repo's
`objects/` dir absolute path (so `--fix-thin`/`--fsck-objects` resolve bases), empty `refs/`, a `HEAD`
copy (`ref: refs/heads/main\n` — format-irrelevant here), and a byte copy of the repo's `config`.

Removal on EVERY exit path: `defer os.RemoveAll(scratch)` after the defer list also sweeps git's leaked
tmp files (`scratch/tmp_*`, `scratch/objects/pack/tmp_*`). A rejected push leaves nothing behind.

### 3.2 Spawn

```
git index-pack --stdin --keep --rev-index --threads=0 [--fix-thin] [--fsck-objects]
```

- `--fsck-objects` present when `wal.fsck_objects` (default true); `--fix-thin` always for receive-pack
  (`thin: true`); neither for bundle backfill ingest paths that pass complete packs.
- Env: `GIT_DIR=<scratch>`, `GIT_TRACE2_EVENT=<tmp>/walgit-index-pack-<suffix>.jsonl` (suffix = the
  nanos already in the scratch name).
- stdin = the streamed pack, piped. `max_bytes` = `server.max_push_bytes` is enforced WHILE streaming:
  the feeder goroutine copies 64 KiB chunks into a temp file, aborting with `ErrMaxBytes` ("pack exceeds
  max_bytes") the moment the cap is crossed; the temp file's fd is then piped into git's stdin.
- Runs via `Pool.Run` (§2), timeout `git.ingest_timeout` (default 600 s), under the per-repo ingest lock
  (see Concurrency).

### 3.3 After success

1. Parse the trailing checksum line from index-pack's stdout (`pack\t<sha>\t<n>`? — git prints
   `pack\t<pack-checksum>` plus keep/idx notices; take the LAST 40/64-hex token as the pack checksum).
2. **Move order idx → rev → pack**, atomic renames into the repo's `objects/pack/pack-<hex>.*`
   (`<hex>` = checksum, lowercase). Pack LAST so an interrupt never leaves a pack without an idx.
   The `.keep` file is discarded — publish (the WAL manifest CAS, doc 05) is the commit point.
3. `object_count` is read from the **idx fanout**: after the 4-byte magic `\xfftOc`, 4-byte version, the
   next 1024 bytes are 256 big-endian fanout counts; `count = fanout[255]`. No subprocess.
4. Zero-object pack (ref-only push) → install nothing, publish no pack, return `object_count == 0`.
5. Parse GIT_TRACE2_EVENT JSONL for metrics: git duration = the `t_abs` (float seconds) of the last
   `region_leave` event (fallback: Go-measured wall time around the exec); region phases from
   `region_enter`/`region_leave` pairs — feed time measured by the Go feeder goroutine (`phase="feed"`),
   git's own regions reported by `region` label (`phase=<label>`). Metrics are advisory
   (`git_index_pack_duration_seconds`, `git_index_pack_phase_seconds{phase}`); a malformed trace file is
   ignored, never an error.

`IngestResult{Checksum Oid, ObjectCount uint64, KeepPath discarded, Trace *IngestMetrics}`.

### Concurrency
- **Hazard:** two concurrent ingests into one repo interleave pack renames and corrupt the local odb;
  crash mid-move leaves a torn pack set.
- **Avoidance:** ingest is serialized per repo by the ingest lock — one `sync.Mutex` per repo handle,
  owned by `internal/wal`'s registry (doc 05) and acquired by the receive-pack flow BEFORE streaming the
  body (never while holding the WAL sync lock; lock order: ingest lock → wal sync, never reversed).
  Scratch dirs are unique per call (pid+nanos) so crash debris never collides. The move order idx→rev→pack
  is itself the crash-safety mechanism: a pack without idx is unusable, so partial state is inert and
  swept by the maintainer (doc 10). After a successful ingest the caller refreshes the repo handle (odb
  re-open). Reference: `13_concurrency.md` §"lock ordering".

## 4. Refs (§7.3)

### 4.1 Reading

- `HEAD`: read the file; `ref: <target>` prefix → symbolic (`head_target`), else detached oid.
- `packed-refs`: line format `<oid> <name>`; a following line `^<peeled>` attaches `Peeled` to the
  previous ref. Header line `# pack-refs with: peeled fully-peeled sorted` skipped.
- Loose refs: walk `refs/` recursively (`filepath.WalkDir`), skipping `*.lock`; a loose file starting
  `ref: ` is a symref resolved ONLY when its target is already known in the snapshot (packed or
  previously read); otherwise record unresolved and skip. Loose overrides packed.
- Tag peeling: only `refs/tags/*`; per-oid cache (`map[Oid]Oid` + mutex, per repo, LRU-capped at
  `cache.ref_advert_entries`); max 16 tag hops, deeper chains treated as unpeelable. Peeling uses one
  persistent `git cat-file --batch` process per repo (send `<oid>\n`, read `<oid> <type> <size>\n` +
  payload; for `tag` parse the `object <oid>` header line and repeat).
- Result: `RefSnapshot` — name-sorted, each entry `{Name, Oid, Peeled, HeadTarget}`; `head_target` on the
  snapshot itself.

### 4.2 Snapshot and cache types

```go
type RefEntry struct {
    Name   string // "refs/heads/main"
    Oid    Oid
    Peeled Oid // "" unless an annotated tag was peeled
}
type RefSnapshot struct {
    Gen        uint64      // writer generation at load
    Refs       []RefEntry  // sorted by Name
    HeadTarget string      // "refs/heads/main" or ""
    HeadOid    Oid         // resolved HEAD, "" if unborn
    Fingerprint            // {Gen, PackedLen, PackedMtime, HeadMtime}
}
type RefCache struct {
    mu      sync.RWMutex
    base    *RefSnapshot            // last full parse
    pending map[string]Oid          // name→oid overlay from in-flight txns (nil = deleted)
    peel    map[Oid]Oid             // tag peel cache
}
```

Cache validity is keyed by `(Gen, packed-refs len, packed-refs mtime, HEAD mtime)` — `os.Stat` the two
files, compare, re-parse only on mismatch. Pending txns are folded in memory (O(n) patch over the sorted
slice, no re-parse) when the WAL publish confirms a txn (doc 05 calls `Patch(txn)` after its CAS
succeeds). `RefView()` is O(k): binary-search the base snapshot, merge the pending map — it NEVER
materializes a full copy for a 500 k-ref repo. The `refs_parses` counter (Prometheus, doc 06 metrics) is
the O(refs) gauge; it increments only on real parses.

### Concurrency
- **Hazard:** a writer patching `Refs` while a reader binary-searches it; a pending overlay updated while
  `RefView` iterates it.
- **Avoidance:** `RWMutex`: readers take RLock for the whole view call; writers take Lock for patch or
  re-parse. `RefView` copies nothing — the returned slice aliases the immutable base (base snapshots are
  never mutated in place; patches produce a NEW slice under the write lock, then swap the pointer). Peel
  cache has its own mutex (it is written on read paths). Lock order: ref cache RWMutex → peel mutex;
  never the reverse; never acquire ref cache locks while holding the ingest lock and vice versa.
  `13_concurrency.md` §"copy-on-write snapshots".

### 4.3 Writing: `apply_ref_txn` (authoritative path)

Validation first (reject before any subprocess):

- **Ref names:** `HEAD` is always OK. Everything else MUST start with `refs/`; forbidden bytes
  ``space \n \r ~ ^ : ? * [ \``; no `..`; no `@{`; no `//`; no leading or trailing `/`; no trailing `.`;
  no trailing `.lock`.
- **Oids:** empty or all-zero = absent marker (allowed); otherwise exactly 40 (sha1) or 64 (sha256)
  lowercase hex.
- If `check_old`, verify each old value against the current snapshot view: old = zero → ref MUST NOT
  exist; old set → MUST match. Any mismatch → `ErrRefConflict{Ref, Expected, Actual}`.

Then ONE transaction against git:

```
git update-ref --stdin
```

with stdin (newline-terminated, non-`-z` grammar):

```
start
create <ref> <new> [<old>]      # or: update <ref> <new> [<old>] / delete <ref> [<old>]
...
prepare
commit
```

`<old>` is emitted only when checked; git interprets a zero old oid as "must not exist". Symbolic updates
(`HEAD`) are NOT passed to update-ref: they are applied afterwards by direct file write
(`ref: <target>\n`, target validated as a ref name). On failure (non-zero exit, or `fatal:` on stderr
before `commit`): parse the refname from stderr — the first single-quoted token (`'refs/heads/x'` or
`'HEAD'`) in the offending line — into the `ErrRefConflict`; re-verify against the snapshot so the caller
gets expected/actual.

After success: bump the writer generation and patch the cached snapshot with the txn instead of re-parsing
(§4.2).

### 4.4 Snapshot load (replicas) and offline txns

`LoadSnapshot(refs map[string]RefEntry, head ...)` writes `packed-refs` **atomically** (temp file +
`rename`): header `# pack-refs with: peeled fully-peeled sorted`, entries sorted by name, `^<peeled>`
continuation lines after annotated tags; then remove the loose `refs/` tree and recreate `refs/heads/` +
`refs/tags/` skeletons (empty dirs); then rewrite `HEAD`; then refresh the cache. `ApplyRefTxnsOffline`
is the same merge performed purely in memory (works when objects are absent) and is what snapshot/replay
apply (doc 05). `pack_refs` is `git pack-refs --all --prune` (run in the repo git-dir, 60 s timeout).

## 5. Pkt-line codec

Hand-rolled in this package (`pktline.go`), used by advertisements, receive-pack parsing, and
report-status:

- One line = 4 hex length bytes (total length incl. the 4) + payload. Max total 65520 → payload ≤ 65516.
- Special lengths: `0000` flush-pkt, `0001` delim-pkt, `0002` response-end (v2).
- Encoder: `Pkt(s string)` (payload + `\n` if absent — callers pass exact payloads), `Flush()`,
  `Delim()`. Decoder: `Next() (payload []byte, kind)`; a decoded payload longer than 65516 or non-hex
  length is a protocol error → the request is rejected with pkt `ERR`.
- The v0 first line embeds capabilities after a single NUL byte (`...\0cap1 cap2\n`); parsing splits on
  the first `\x00` only.

## 6. Advertisements (§7.4)

Selection: the `Git-Protocol: version=2` header (case-insensitive token match) selects v2; otherwise v0.
The HTTP layer (doc 06) prepends `# service=<svc>\n` + flush for `info/refs`; `Advertisement` returns only
the body.

### 6.1 v0 (`info/refs`)

Body = refs in name-sorted order, one `<oid> <name>\n` pkt per ref, with the capability line appended to
the FIRST ref line: `<oid> <name>\0<caps>\n`. Peeled lines follow each annotated tag:
`<peeled> <name>{}\n`. For upload-pack, a final `<oid> HEAD\n` pkt is appended if HEAD resolves. Empty
repo → single line `<zero-oid> capabilities^{}\0<caps>\n`. Ends with flush-pkt.

Capability lines, **copied verbatim** (single spaces, this exact order):

| Service | Capabilities |
|---|---|
| receive-pack | `report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta push-options object-format=<fmt> agent=walgit/<version>` |
| upload-pack | `multi_ack_detailed side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want filter object-format=<fmt> agent=walgit/<version>` |

`<fmt>` = `sha1` or `sha256` from the repo format. (Note: repo init sets `uploadpack.allowAnySHA1InWant=true`
unconditionally — that is what makes the repair path's wants-by-SHA work; the separate config key
`git.allow_any_sha1_in_want` may additionally gate advertisement, default false — the known §20
discrepancy, resolved in favor of init-always-true.)

### 6.2 v2 capability advertisement

Rendered by hand (we own ls-refs; `fetch` is delegated to stock git, §8):

```
version 2
agent=walgit/<version>
ls-refs=unborn
fetch=thin-pack ofs-delta sideband-all wait-for-done shallow deepen-since deepen-not deepen-relative filter include-tag
object-format=<fmt>
<flush>
```

Each as its own pkt-line. Advertised `fetch` features MUST be a subset of what stock git accepts in the
delegated request; `packfile-uris` is deliberately absent (no bundle-uri-in-fetch in v1).

### 6.3 v2 `ls-refs`

Args parsed from the client request: `symrefs`, `peel`, `unborn` (flags), `ref-prefix <p>` — also
tolerate `ref-prefix=<p>` — terminated by flush.

**Prefix filtering, O(log n + k)** over the name-sorted snapshot: sort + dedupe the prefixes; for each
prefix `p`, binary-search the lower bound in `Refs`, then walk forward emitting refs whose name starts
with `p` and stops when `name > p` (i.e. when the next ref's name does not have `p` as a prefix — compare
`strings.HasPrefix` and break when `!HasPrefix && name > p`). Merge overlapping prefix ranges (dedupe by
skipping refs already emitted — track the previous emitted index; ranges arrive in ascending prefix
order, so a single lastEmittedIdx suffices). Total cost: k prefixes × O(log n) search + O(matched refs).

HEAD is resolved from `head_target` BEFORE prefix filtering — a prefix that excludes the target must not
hide HEAD; HEAD is advertised when prefixes are empty or one of them is exactly `HEAD`. Unborn HEAD → the
`unborn` pseudo-oid plus ` symref-target:<t>` when `unborn` requested.

Line rendering per ref: `<oid> <name>` + ` symref-target:<t>` (when `symrefs` requested and the ref is a
symref, or for unborn HEAD) + ` peeled:<oid>` (when `peel` requested and `Peeled != ""`). Flush-pkt ends
the response.

## 7. Receive-pack server flow (§7.5), in order

The HTTP handler (doc 06) does placement/drain gates and the `.git` URL suffix check FIRST (non-`.git`
URL → pkt-line refusal before any work), takes the per-repo `server.max_concurrent_per_repo` semaphore,
then calls this flow. Request/response may be gzip `Content-Encoding` (handled in doc 06); git responses
carry the no-cache header triple (doc 06).

1. **Parse the request** from the full body: first pkt-line `<old> <new> <ref>\0<caps>`, then further
   `<old> <new> <ref>` lines until flush; `shallow <oid>` lines collected (NOT enforced); caps parsed:
   `report-status`, `report-status-v2` (parsed but option lines deliberately never emitted, §7.8 step 8),
   `side-band-64k`, `atomic`, `quiet`, `push-options`, `ofs-delta`, `agent=`, `object-format=` (validate
   against repo format; mismatch → refuse). If the client sent `push-options` (and we advertise it):
   the next section is push-option lines until flush. The REMAINDER of the body is the raw pack bytes
   (may be absent for a pure delete push).
2. **Ingest** the pack (§3): `thin=true`, `fsck=wal.fsck_objects`, `max_bytes=server.max_push_bytes`.
   Failure → `unpack ng <msg>` and skip to step 8 emitting per-ref `ng` refusals (with sideband, the
   message goes on band 2 FIRST, then the report).
3. **Connectivity** (when `wal.check_connectivity`): see below.
4. **Policy** (§14, `internal/policy`): classify each update (create/update/delete; force = non-FF
   determined via `git merge-base --is-ancestor <old> <new>` after ingest — exit 0 = fast-forward; exit 1
   = force; other = treat as force-with-verification-needed → force). Disallowed refs →
   `ng "rejected by rule '<name>'"`.
5. **Publish only the allowed subset** via the WAL (doc 05, group-committed, `new_peeled` filled per
   §6.3 of the spec); per-ref results from policy + verify overwrite by name. If none allowed → report
   and done.
6. **Report status** (step 8 below).

### 7.1 Connectivity — DECISION: subprocess pipeline, not an in-process walker

The Rust implementation walks with gix. **Go uses stock git semantics:**

```
git rev-list --objects --stdin --not --all        # in the repo git-dir
  stdout | git cat-file --batch-check             # existence check for every listed oid
```

- Tips (non-zero new oids) are written one per line to rev-list's stdin (`--stdin` avoids ARG_MAX);
  `--not --all` hides history reachable from existing refs — exactly the spec's
  `stop_at_existing_refs` (`hidden = existing ref tips`).
- `rev-list --objects` must LOAD every tree/commit it traverses, so a missing tree/commit makes rev-list
  itself fail (non-zero exit, `unable to read`/`missing` on stderr) → `ErrMissingObject`.
- `cat-file --batch-check` is fed rev-list's stdout through an `io.Pipe` and reports `<oid> missing` for
  any missing tree/blob; missing oids (retained up to 16) → `ErrMissingObject{Oids}`. `gitlink` (submodule)
  entries are skipped by matching the mode in rev-list's `--objects` output line format
  (`<oid> <path>` — filter: lines whose path component has no type check possible; simplest: treat
  `missing` submodules by ignoring oids whose path is a recorded gitlink — rev-list marks them; accept
  rev-list's failure as the only commit/tree error source). Peel tips first: each tip oid is verified to
  exist via the same batch-check stream before the walk (`peel tips, verify existence`).
- Per-ref attribution: on joint failure, re-run the pipeline once per tip (rare path, bounded by ref
  count) to name the offending ref → per-ref `ng "connectivity: missing <oid>"`.

### Subprocess
Two processes, one pipeline, via `Pool.Run`. Discipline: rev-list's stdin written by a feeder goroutine
(closes on tips-EOF); `io.Pipe` connects rev-list stdout → cat-file stdin (the copier goroutine closes
the write end); the caller drains cat-file stdout, then `Wait`s rev-list first (its exit status carries
the walk verdict), then cat-file. Timeout: the request context plus a hard cap of `git.connectivity_timeout`
(default 300 s). stderr of both → bounded buffers for error text.

### 7.2 Report status emission (normative)

Body: `unpack ok` or `unpack ng <msg>`, then per-ref `ok <ref>` / `ng <ref> <reason>`. **Plain
report-status lines are emitted EVEN WHEN report-status-v2 was negotiated** — an `option atomic` line
after an `ng` confuses clients, and a rejected atomic txn must be plain `ng` per command (§20 item 12).
Encoding: pkt-lines; then, when the client requested `side-band-64k`, the ENTIRE report is wrapped in
band-1 frames (≤ 65516-byte payload chunks), an empty band-1 frame is NOT sent, and the message text of
failures was already emitted on band 2 before the report. Ends with flush-pkt. `quiet` suppresses only
band-2 progress chatter, never the report.

### Concurrency
- **Hazard:** the whole flow holds the per-repo ingest lock while streaming a 64 GiB push; ref cache
  patched concurrently with a snapshot load from a Serve sync.
- **Avoidance:** lock order is ingest lock → (policy, publish) → WAL sync → ref cache write. The body is
  streamed to a temp file BEFORE the ingest lock is taken when possible (buffering rule from doc 06), so
  a slow client does not hold the lock. Ref cache patches are sub-millisecond under the write lock.
  Everything blocking (ingest, connectivity, `merge-base`) runs in `Pool.Run` with contexts derived from
  the request — a client disconnect cancels all of it. `13_concurrency.md` §"lock ordering", §"drain".

## 8. Upload-pack (§7.6)

### 8.1 Engine selection — DECISION (Go v1)

| `git.upload_pack_engine` | Rust behavior | walhub Go v1 |
|---|---|---|
| repo base is remote-served (no store mount) | gix engine + remote-reader faulter | **refuse** (see below) |
| `auto` (default) | stock git | stock git |
| `git` | stock git | stock git |
| `gix` | gix | stock git + WARN log (config accepted for bucket compatibility; in-process engine deferred) |

**Refusal (remote-served base):** pkt-line
`ERR walgit: <owner>/<repo> is served through a remote base that is not mounted on this host; fetch from the serving host, or set cache.store_mount so the base is local (this walhub build has no remote-reader fetch engine)`
followed by flush, HTTP 503 + `Retry-After: 15`. The engine field is kept in config and surfaced in
`/services/api/instance` so a future in-process engine slots in without config churn (doc 14).

### 8.2 Stock git spawn

```
git -c uploadpack.allowSidebandAll=true upload-pack --stateless-rpc .
```

with `GIT_DIR=<repo>`, `GIT_PROTOCOL=version=2` or `version=0` (matching the negotiated version), env
`GIT_TERMINAL_PROMPT=0`, on `Pool.Run`. stdin = request body (streamed), stdout = response
(`Content-Type: application/x-git-upload-pack-result`, no-cache triple — doc 06). Discipline: close stdin
after EOF, drain stdout to the client writer concurrently, drain stderr to a bounded buffer, then Wait;
stderr content on non-zero exit → `Subprocess error` (500-shaped; doc 06).

For v2 the request body is parsed (pkt-lines) BEFORE spawning for the guards below, but is passed
through **byte-for-byte** — walhub never re-encodes client pkt-lines (only v0 advertisement and ls-refs
are hand-rendered). If a future need requires building a v2 request, the arg grammar is: `command=fetch`,
`thin-pack`, `ofs-delta`, `no-progress`, `include-tag`, `sideband-all`, `wait-for-done`, `filter <f>`,
`want <oid>`, `have <oid>`, `shallow`, `deepen`, `deepen-since`, `deepen-not`, `want-ref <ref>`, `done`.

**max_wants guard:** when `git.max_wants > 0`, count `want <oid>` lines (v0 and v2 alike) while parsing;
exceeding the cap → pkt-line `ERR walgit: fetch wants more than git.max_wants=<n> objects; use --sparse or --no-checkout for blobless/partial workflows`
+ flush, no git spawn.

**D17 forcing (`bundles.require`, fetch path):** an **unbounded zero-have** fetch (no `deepen*`, no
`filter`, no `have`) of a repo listed in `bundles.require` is refused with the exact fix in the error
text: `ERR walgit: <owner>/<repo> requires bundle-uri clones; use bundle-uri (pass -c transfer.bundleURI=false for shallow/CI fetches)`.
Bounded zero-have fetches (CI `--depth`/`--filter`) and all fetches with haves proceed. **One-shot
fallback:** a principal that fetched this repo's `bundles/list` within the last hour demonstrably tried
bundle-uri → ONE upload-pack full clone per 6 h with a loud band-2 `WARNING ... bundle-uri fallback clone`
frame; the next one and everyone else is refused. State: per-principal LRU (principal, repo, last-grant)
in `internal/bundle` (doc 08 owns the ledger; this package queries it).

### Subprocess
`upload-pack` lifetime = request lifetime: the request context cancels it (client disconnect → SIGKILL →
scratch/pipes cleaned by defer). No internal timeout beyond `server.request_timeout` (doc 06 middleware).
Feeder goroutine streams the body into stdin and closes; the handler copies stdout to the response
writer as it arrives (git's own band framing provides the flush boundaries; no re-buffering). stderr
drained to a bounded buffer on a goroutine that exits at Wait.

## 9. Repack, commit-graph, history pack (§7.7)

All maintenance commands run in the repo git-dir (or scratch as noted), `Pool.Run`, 1800 s timeout,
under the per-repo pack_mutex (doc 05/10). Exact argv:

- **Geometric compaction (maintainer):**
  `git repack -d --geometric=<factor> --write-midx [--write-bitmap-index] [--keep-pack <name>]…`
  (keep-pack for the base and every history pack that must survive). Diff the `objects/pack/*.idx` set
  before/after → new packs + removed packs (returned to the caller for store reconciliation).
- **Full repack (base rebuild):** `git repack -a -d --threads=0 --write-bitmap-index --write-midx
  [--keep-pack …]`; delete stray `*.keep` markers first.
- **Split commit-graph:** `git commit-graph write --reachable --split=replace [--changed-paths]`
  (`git.commit_graph` / `git.commit_graph_changed_paths`). Identify the last chain layer by its trailing
  checksum (the file name under `objects/info/commit-graphs/`); copy it out as the `wal/<checksum>.commit-graph`
  side-file (store key, doc 02). On a reader, install the side-file as the chain BASE:
  `objects/info/commit-graphs/graph-<hash>.graph` + `commit-graph-chain` naming only it.
- **Incremental fold (downloaded packs):** `git commit-graph write --split --stdin-packs` fed the pack
  idx names on stdin (one per line; close stdin, drain, Wait).
- **History pack (D18, when `git.history_pack`):** pipeline
  `git pack-objects --filter=blob:none --revs --delta-base-offset --stdout -q` (all ref oids on stdin)
  piped into `git index-pack --stdin` in a scratch with alternates → rename to
  `pack-<sha>.pack/.idx/.rev` + a `.history` marker file naming the base. Store kind = HISTORY,
  `derived_from=<base>`. A **history midx**: `git multi-pack-index write --stdin-packs
  --preferred-pack=<history idx>` covering history packs + installed bases; removed when no history packs
  exist. History packs never supersede anything; they are superseded with their base (doc 10).

## 10. Bundle creation primitives (§7.8)

- **Stock create:** `git bundle create <out> --stdin` fed `<ref>` lines and `^<oid>` excludes
  (one per line, stdin closed after). Blocking, `Pool.Run`. Returns size + the byte offset of the
  `PACK` magic — scanned with a 3-byte overlap between chunks (magic may straddle a boundary); offset
  = header/pack split used by composition (doc 08).
- **gix variant:** N/A in Go v1 (no in-process engine) — composed bundles are always header ∘ existing
  pack bytes (below).
- **Header rendering (no git):** `# v2 git bundle\n` + `-<oid> \n` per prerequisite + `<oid> <name>\n`
  per ref + `\n`. (Exact bytes: `-` + oid + one space + `\n`.) A **full bundle** = header ∘ an existing
  pack's bytes — this is how composed weeklies and import bundles are built with zero pack bytes through
  the host. `BundleHeader(refs, prereqs)` is pure and unit-tested byte-for-byte (doc 15 fixtures).

## 11. Upstream-git helpers (repair and follow) (§7.9)

Config: `upstream.git` (source URL), `upstream.lfs`, `upstream.token_env` (env var NAME holding the
token — never the token), `upstream.follow`. Credentials are passed as an **inline config-pair credential
helper**, never on argv or the environment of git itself beyond that env:

```
-c credential.helper= -c credential.helper=!f(){ echo username=x-access-token; echo password=$WALGIT_UPSTREAM_TOKEN; };f
```

(the empty helper first clears inherited helpers). `WALGIT_UPSTREAM_TOKEN` is set in the child env from
the named env var; `GIT_TERMINAL_PROMPT=0` always. For LFS Basic auth: `x-access-token:<token>`.

### 11.1 repair (`fetch_objects_as_pack`)

Scratch bare repo; fetch missing oids in **500-oid batches**:

```
git -c fetch.negotiationAlgorithm=noop -c protocol.version=2 fetch --no-tags --no-write-fetch-head --quiet --depth=1 <upstream> <oid>…
```

then `git pack-objects --no-reuse-delta --compression=6` over exactly the requested oids (no closure);
verify EVERY requested oid is present in the resulting idx (`git verify-pack -v` or idx lookup via the
fanout + binary search — idx lookup preferred, no subprocess); a refused want is an error, never a silent
hole. Publish via add-pack (tier 0, doc 05).

### 11.2 follow (`fetch_refs`)

Persistent scratch `<cache.dir>/follow/<owner>/<name>.git` with `objects/info/alternates` → the serving
objects dir. Set `refs/follow/<ref>` to the WAL values via `git update-ref --stdin` (§4.3 grammar); then:

```
git -c fetch.unpackLimit=1 -c transfer.unpackLimit=1 -c fetch.writeCommitGraph=false -c gc.auto=0 -c protocol.version=2 fetch <upstream> +<ref>:refs/follow/<ref>…
```

Read tips back via `git for-each-ref refs/follow/` (`--format=%(objectname) %(refname)`); the fetched
pack is discarded after ingest (the scratch's packs are trash — alternates make objects resolve anyway).
Scheduling cadence `maintenance.follow_interval` and the D33 loop live in doc 10.

### Subprocess
Both helpers run under `Pool.Run` with a 900 s timeout per fetch batch; stdin/stdout discipline as §2;
`credential.helper` argv order is significant (clear-then-set). The follow scratch is shared per repo —
serialized by the same repo mutex as maintenance (doc 10 owns it).

## 12. Config keys consumed by this package

| Key | Default | Used in |
|---|---|---|
| `git.binary` | `"git"` | every exec (§2) |
| `git.upload_pack_engine` | `"auto"` | §8.1 (`gix` → stock + WARN) |
| `git.max_wants` | `0` | §8.2 guard |
| `git.allow_filter` / `git.allow_any_sha1_in_want` | `true` / `false` | advertisement path (init always sets allowAnySHA1InWant=true — §6.1 note) |
| `git.object_format` | `"sha1"` | §1.2 init |
| `git.commit_graph` / `git.commit_graph_changed_paths` | `true` / `false` | §9 |
| `git.history_pack` | `true` | §9 |
| `git.ingest_timeout` / `git.connectivity_timeout` *(new)* | `600s` / `300s` | §2, §3, §7.1 |
| `git.max_git_procs` *(new)* | `4 × GOMAXPROCS` | §2 pool |
| `server.max_push_bytes` | `64GiB` | §3.2 |
| `wal.fsck_objects` / `wal.check_connectivity` | `true` / `true` | §3.2, §7.1 |
| `server.max_concurrent_per_repo` | `64` | taken by HTTP layer before the handler |
| `cache.dir`, `cache.ref_advert_entries` | `/tmp/walgit`, `256` | §1.2, §4.1 peel cache |
| `bundles.require` | `[]` | §8.2 D17 |
| `upstream.*` | — | §11 |

New keys (`git.ingest_timeout`, `git.connectivity_timeout`, `git.max_git_procs`) are Go-only additions —
listed for doc 11; Rust-compat keys keep their names verbatim.

## Decisions & deviations from the Rust design

- **Connectivity via `git rev-list --objects --stdin --not --all | git cat-file --batch-check`** instead of
  the gix rev-walk: stock git gives exact `stop_at_existing_refs`/missing-object semantics with zero
  in-process walker code (dependency policy); per-ref attribution by bounded per-tip re-runs on failure.
- **No gix/upload engine at all in Go v1:** stock git for every fetch; a remote-served base (which Rust
  serves via the gix engine + remote-reader faulter) is refused with an explicit pkt ERR + 503/Retry-After
  naming the fix (`cache.store_mount` or fetch from the serving host); `gix` config value accepted and
  downgraded with a WARN. The Rust gix engine carried the 178 GB OOM and wrong-id thin-pack bug; deferring
  keeps v1 boring while the config seam (doc 14) preserves the slot.
- **v2 fetch requests passed through byte-for-byte** after parsing for guards (Rust sometimes rebuilds the
  request): no re-encoding means no encode bugs; guards only read.
- **v2 capability advertisement rendered by hand** (Rust spec implies engine-rendered): needed since
  ls-refs is ours while fetch is stock git's; advertised fetch features are the subset stock git accepts.
- **v2 report-status-v2 negotiated but never used for output; plain `ng` lines always** — carried over
  from §20 item 12 (behavioral truth), not a new deviation.
- **Ref cache pending-overlay uses copy-on-write slice swap under a RWMutex** instead of Rust's
  generation-keyed moka cache: stdlib-only, same O(k) view and O(n) patch, and the base snapshot's
  immutability is enforced by construction.
- **Peel cache via one persistent `git cat-file --batch` per repo** instead of gix object reads: avoids a
  subprocess per tag while staying stdlib; bounded by `cache.ref_advert_entries`.
- **Blocking pool is a semaphore-bounded goroutine pool (`git.max_git_procs`)** replacing tokio's 4-worker
  bulk runtime + spawn_blocking: one pool for all git execs; per-repo caps live in the HTTP layer.
- **New Go-only config keys** `git.ingest_timeout`, `git.connectivity_timeout`, `git.max_git_procs`:
  Rust hard-codes tokio timeouts/worker counts; Go makes them explicit for operators (documented in doc 11).
- **`git.binary` is plumbed everywhere** (unlike Rust, which hardcodes `"git"` — §20 item 5): one field on
  `Layer`, zero cost, closes the known discrepancy.
