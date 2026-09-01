# 03 — Store backends (S3, GCS, memory) and the lease layer

> Source: MASTER_RUST_SPEC.md §4.1–§4.10 · Status: normative for the walhub Go implementation.

All bucket access in walhub goes through one Go interface in `internal/store`, with three backends behind
it: `s3` (default, covers rustfs/MinIO), `gcs`, and `memory` (tests). The interface contract (types,
semantics, `Prefixed` wrapper) is fixed by the Rust spec §4.1 and is not restated here; this doc specifies
the Go mechanics of each backend, the transport discipline, striped I/O, the round-trip budgets, and the
lease layer. Wire/bucket formats stay byte-compatible with the Rust implementation.

**Zero third-party deps.** This layer uses only the Go standard library: `net/http`, `crypto/sha256`,
`crypto/hmac`, `crypto/rsa`, `encoding/json`, `encoding/base64`, `time`, `context`, `sync`. No AWS SDK,
no Google client libraries, no gRPC.

```go
package store // git.packden.us/crueber/walhub/internal/store

type Version string // opaque CAS token; compare for equality only, never parse

type GetOptions struct {
    IfNoneMatch Version // equal → NotModified{Version}
    IfMatch     Version // different → PreconditionFailed
    Range       *[2]int64 // half-open [start,end)
}
type PutMode int // PutOverwrite | PutCreate (only if absent) | PutUpdate (CAS on version)
type PutOptions struct {
    Mode      PutMode
    Version   Version // required for PutUpdate
    ContentType string
    Immutable bool    // ⇒ long Cache-Control headers
}
type PutBody struct {
    Bytes  []byte
    Stream io.Reader // Len MUST be known (PutLen); File delegates to Stream
    Len    int64
}
type StoreError struct {
    Kind    StoreErrKind // NotFound | PreconditionFailed | Retryable | InvalidArgument | Other
    Key     string
    Current Version      // filled on PreconditionFailed when cheaply known
    Err     error
}
```

Every backend implements: `Backend() string`, `Get(ctx, key, GetOptions)`, `Head`, `Put`, `Delete(key,
ifVersion)`, `List(prefix, startAfter)` (lazy, lexicographic, strictly after), `ListPrefixes` (delimiter
listing, sorted), `SignedGetURL(key, ttl)`, `AccelTarget(key)`, `SupportsCompose()`,
`ComposeIsNative()`, `Compose(dest, sources, opts)` (1..=32 sources, in order), plus convenience
`GetBytes`/`GetIfChanged`/`PutBytes`/`Exists`. Delete with `ifVersion == nil` on an absent key returns Ok.

## 1. Key classification: control plane vs bulk

Per §4.2, exactly this function decides which transport and permit pool a request uses:

- **Control plane**: every key whose last path segment ends `.pb` or `.json` (manifests, log segments,
  checkpoints, leases, bundle lists, policy, cursor, fsck reports, render cache, refs blobs).
- **Bulk**: keys under `wal/`, `bundles/`, `lfs/`, plus **every ranged read** (`GetOptions.Range != nil`)
  and every striped download/upload part regardless of key.
- Everything else (e.g. object bytes under other prefixes) classifies as bulk; the classifier MUST exist
  even on backends with a single client (S3), because tests and metrics key off it.

Control-plane traffic MUST NEVER queue behind bulk bytes. This is the incident-proven rule (§4.6): a
`bundles/list.pb` GET once sat 455–472 s behind 32 range stripes on a shared transport.

## 2. S3 backend

Credentials come from env-var *names* in config (`store.s3.access_key_env` / `store.s3.secret_key_env`,
defaults `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`; `AWS_SESSION_TOKEN` honored if set), plus region,
endpoint, and `force_path_style` (true for rustfs/MinIO). Reads are presigned GETs; writes are signed PUTs.

### 2.1 Hand-rolled SigV4

Implement AWS Signature Version 4 with `crypto/hmac` + `crypto/sha256`. For each request:

1. Canonical request (newline-separated):
   ```
   <HTTP method>\n
   <canonical URI — absolute path, each segment URI-encoded, '/' NOT encoded>\n
   <canonical query — keys sorted byte-wise, k=v both URI-encoded, joined by &>\n
   <canonical headers — lowercase names, sorted, trim inner/outer spaces, values joined, each "name:value\n">\n
   <signed headers — semicolon-joined lowercase header names>\n
   <hashed payload = hex(sha256(body)) or "UNSIGNED-PAYLOAD">
   ```
2. String to sign: `AWS4-HMAC-SHA256\n<amz-date ISO8601 basic>\n<date>/<region>/s3/aws4_request\n<hex(sha256(canonical request))>`.
3. Signing key: `k = HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), "s3"), "aws4_request")`;
   `Authorization: AWS4-HMAC-SHA256 Credential=<ak>/<date>/<region>/s3/aws4_request, SignedHeaders=<sh>, Signature=<hex>`.

Mandatory headers on every request: `x-amz-date` (and `x-amz-content-sha256`). Sign
`host`, `x-amz-date`, `x-amz-content-sha256`, and `range` when present. **Do NOT sign** `if-none-match`,
`if-match`, or any conditional header: presigning covers the URL only, and the conditional headers are
attached to the *outgoing* HTTP request by the caller (this is the whole reason reads go through an HTTP
client instead of a bare redirect). For presigned GETs the query-string variant of SigV4 is used
(`X-Amz-Algorithm`, `X-Amz-Credential`, `X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders=host`,
`X-Amz-Signature`; payload hash `UNSIGNED-PAYLOAD`).

Path-style addressing when `force_path_style`: URL is `https://<endpoint>/<bucket>/<encoded-key>`;
virtual-host style otherwise. Validation is **normative**: the implementation MUST pass the AWS official
SigV4 test vectors (the publicly documented `get-vanilla`, `get-header-value-trim`, `get-space`,
`get-utf8`, `post-vanilla-empty-query-value` suite from the aws-sig-v4-test-suite repo, adapted for S3
`s3/aws4_request` terminators) — port them as table-driven Go tests with canned credentials.

### 2.2 Reads: presigned GET + conditional headers

Presign a GET with 60 s expiry, then execute it with a plain HTTP request carrying:

| Intent | Attached header | Wire result → mapping |
|---|---|---|
| plain | — | 200 → Object (Version = ETag, **quotes stripped**) |
| IfNoneMatch | `If-None-Match: "<version>"` | 304 → NotModified |
| IfMatch | `If-Match: "<version>"` | 412 → PreconditionFailed |
| Range | `Range: bytes=<start>-<end-1>` (**inclusive on the wire**) | 206 → Object (Meta.Size = whole object from `Content-Range: bytes s/total`) |
| — | — | 404 → NotFound; 5xx/429 → Retryable; 416 → PreconditionFailed |

ETag handling: strip surrounding `"` from `ETag` before storing as `Version`; re-add quotes on the wire
for conditional headers. `SignedGetURL` = the presigned URL itself (TTL as configured). `AccelTarget` =
presigned GET, TTL 1 h, **no** authorization header — `Range` is not a signed header, so an edge may slice
the object freely.

### 2.3 Writes: single PUT with conditionals, multipart only for Overwrite

- **Single-shot PUT** (default): `PUT /<bucket>/<key>` with body, `x-amz-content-sha256` = body hash, and:
  - PutCreate → `If-None-Match: *`; PutUpdate → `If-Match: "<version>"`; PutOverwrite → none.
  - 200 → Object (Version = returned ETag); 412 → PreconditionFailed (fill `Current` with a follow-up HEAD
    when the caller needs it — see below); 404/5xx/429 as in the table.
- **Multipart ONLY when `body.Len > store.multipart_threshold` (default 64 MiB) AND mode == Overwrite.**
  S3 multipart has no conditional headers, so it can never implement Create/Update. Flow:
  `CreateMultipartUpload` → parts of `store.multipart_part_size` (default 32 MiB) uploaded concurrently →
  `CompleteMultipartUpload` (ordered part list); abort (AbortMultipartUpload) on any failure.
- On PreconditionFailed from a conditional PUT, fill `StoreError.Current` via a follow-up HEAD (the
  "verification goes on the failure path" rule — never pay the HEAD before the write fails).
- **Quirk to preserve:** a PutUpdate with an unparseable `Version` value silently skips the precondition
  (matches the write path of the Rust backend).

### 2.4 compose via UploadPartCopy (compose_is_native = false)

`SupportsCompose() = true`, `ComposeIsNative() = false`. `Compose(dest, sources)` is emulated:

1. PutCreate → HEAD dest first; present → PreconditionFailed (non-atomic pre-check, accepted).
2. `CreateMultipartUpload` on dest.
3. For each source in order, copy its bytes as parts: part i = `UploadPartCopy` with
   `x-amz-copy-source: <bucket>/<encoded-src-key>` and `x-amz-copy-source-range: bytes=<start>-<end>`
   (inclusive), **copy chunk = 1 GiB** (the S3 limit for one copy operation). A part must be ≥ **5 MiB**
   except the last of a source; so: sources ≥ 5 MiB are sliced into 1 GiB copy chunks; sources (or final
   tails) < 5 MiB are read via ranged GET and re-uploaded as ordinary PUT parts. Part numbers are
   assigned sequentially across all sources.
4. `CompleteMultipartUpload`. Sources are left in place. On any failure: abort the upload, delete any
   already-uploaded plain parts, return the error.
5. PutUpdate/PutOverwrite on the destination cannot use conditionals here — apply the CAS by pre-checking
   (step 1) and accept the documented race; all mutation of the same key is lease-guarded by protocol.

### 2.5 Delete: HEAD-then-delete emulation

S3 has no conditional delete. When `ifVersion` is given: HEAD first (absent → NotFound; ETag ≠ version →
PreconditionFailed), then unconditional `DELETE`. The check-then-act race is **accepted** — all mutation
of the same key is lease-guarded by protocol. Unconditional delete of an absent key → Ok.

### 2.6 S3 error mapping

| Wire | StoreError.Kind |
|---|---|
| 404 (GET/HEAD) | NotFound |
| 304 | NotModified (GetResult variant, not an error) |
| 412 | PreconditionFailed |
| 416 | PreconditionFailed (range past EOF) |
| 405 | InvalidArgument (e.g. HEAD on presigned upload) |
| 5xx, 429 | Retryable |
| network/timeout (req.Errors, `net.Error`) | Retryable |
| malformed signed request (400 w/ signature error codes) | Other |

Retry policy: `store.max_retries` (default 8), jittered exponential backoff; retry only `Retryable`, and
never retry a non-idempotent write unless the body is replayable (Bytes or File; a Stream PUT that fails
mid-body surfaces the error rather than re-reading).

#### Concurrency

- Hazard: multipart part uploads and UploadPartCopy loops can explode into unbounded goroutines.
  Avoidance: bounded `errgroup` (see §4) with `SetLimit`-style counting (hand-rolled weighted semaphore,
  §5.2); the owning goroutine owns cancellation context; every part upload takes `ctx` and aborts via
  `ctx.Err()`. One writer owns the multipart state machine; only it may Complete/Abort (no double-abort).
- The presigned GET client is one `http.Client` per backend instance; never construct a client per call
  (each drains the connection pool).

## 3. GCS backend

**Decision: JSON API over plain HTTPS only. No gRPC client.** (Dependency policy; the Rust spec's gRPC
topology is translated — see Decisions.) Version = decimal generation string. Base URL
`https://storage.googleapis.com/storage/v1/b/<bucket>/o/<url-encoded-key>` (or `store.gcs.endpoint`
override for emulators).

### 3.1 Conditionals

| Op | Mechanism |
|---|---|
| GET with IfNoneMatch | `?ifGenerationNotMatch=<gen>` → HTTP 304 → NotModified |
| GET with IfMatch | `?ifGenerationMatch=<gen>` → 412 / `FailedPrecondition` → PreconditionFailed |
| PUT Create | `?ifGenerationMatch=0` |
| PUT Update (CAS) | `?ifGenerationMatch=<gen>` |
| DELETE CAS | `?ifGenerationMatch=<gen>` |

304 and `FailedPrecondition` both mean "not modified" on reads; 412/`FailedPrecondition` mean precondition
failure on writes. Preserve the Rust quirk: CAS PUT (Update) with an unparseable version silently skips
the precondition, while read/delete paths return PreconditionFailed.

### 3.2 Auth: hand-rolled OAuth2 service-account flow (or metadata server)

1. Service account: build a JWT `{alg:RS256, typ:JWT, iss:<sa-email>, scope:"https://www.googleapis.com/auth/devstorage.read_write", aud:"https://oauth2.googleapis.com/token", iat, exp:iat+3600}`
   signed with `crypto/rsa` + `crypto/sha256` (PKCS1v15) over the base64url header/payload; `POST` it as
   `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer` to the token endpoint; cache the returned
   token, refresh at 80% of `expires_in` under single-flight.
2. Metadata server (GCE): `GET http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token`
   with `Metadata-Flavor: Google`. Pick service-account flow when credentials are configured, metadata
   otherwise. Bearer token on every storage request. Token fetch failure → `Retryable`.
3. `SignedGetURL`: V4 signing via IAM `signBlob` (signed JWT flow, plain HTTPS POST to
   `https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/<sa>:signBlob`) when
   `store.gcs.signing_service_account` (or ADC with signing rights) is set; failure → return None so
   callers fall back to proxying.
4. `AccelTarget`: path-style URL `https://storage.googleapis.com/<bucket>/<encoded-key>` plus the
   process's **own bearer token** as `authorization` (the edge cannot refresh tokens).

### 3.3 Reads: alt=media and range reads

- Full read: `GET <base>?alt=media` (plus conditionals from §3.1). Response is the raw object bytes.
- Range read: same request with `Range: bytes=<start>-<end-1>` → 206, `Content-Range: bytes s/e/total`;
  Meta.Size = total.
- **Bulk read resume:** a mid-stream failure re-issues the request for the undelivered suffix pinned to
  the same generation (`ifGenerationMatch`), up to 5 attempts with exponential backoff — a rewritten
  object can never be spliced into a partial read.

### 3.4 Writes

- ≤ 8 MiB: single-shot multipart/related PUT (metadata part + media part, or `?uploadType=media` when no
  custom metadata), with the generation conditionals as query params.
- \> 8 MiB (Stream/File bodies): **resumable upload** — `POST ?uploadType=resumable` (with conditionals)
  → session URI from `Location`; upload in **256 KiB** chunks with `Content-Range: bytes <start>-<end>/<total>`;
  on interruption, `PUT` the session URI with `Content-Range: bytes */<total>` to get the resume offset;
  finalize with the last chunk. Deadline per chunk per the table below.
- Compose: `POST <base>/compose` with `{sourceObjects: [{name, generation?}…], destination}` — ≤ 32 sources,
  honors Create (`ifGenerationMatch=0`) / Update / Overwrite on the destination, sources left in place,
  ≤ 30 s deadline. `ComposeIsNative() = true`.

### 3.5 GCS deadlines

| Operation | Deadline |
|---|---|
| metadata op (get meta, list page, delete, compose) | 10 s, retry once with 100–500 ms jitter |
| read open (TTFB of alt=media/Range) | 60 s |
| read chunk (per subsequent stream chunk) | 60 s |
| PUT single-shot | 30 s + bytes/1 MiB seconds |
| resumable chunk | 60 s per chunk |
| list page size | 1000 |

### 3.6 GCS error mapping

| Wire | StoreError.Kind |
|---|---|
| 404 | NotFound |
| 304 / `FailedPrecondition` (not-modified) | NotModified |
| 412 / `FailedPrecondition` (write), 416 | PreconditionFailed |
| 5xx, 429, `Unavailable`, `DeadlineExceeded`, `ResourceExhausted`, `Internal`, `Aborted` | Retryable |
| 400 | InvalidArgument |
| token fetch failure, network | Retryable |

#### Concurrency

- Hazard: one shared HTTP transport lets 32 concurrent 32 MiB range stripes starve a 200-byte manifest
  GET (recorded incidents: 3–11 s queues behind a 7.5 GB download; 455 s behind stripe fan-out).
  Avoidance: two separate `http.Client`s per GCS backend instance — one control-plane client (small
  connection pool, `MaxIdleConnsPerHost` ≥ 8) and one bulk client — selected by the §1 classifier. Never
  share a transport between them.
- Hazard: unbounded bulk concurrency saturates the NIC and inflates queue times invisibly. Avoidance: the
  weighted semaphore of §5.2 gates **every** bulk request (reads and writes) with
  `store.gcs.bulk_concurrency` (default 32) permits; queue time recorded as
  `walhub_store_bulk_queue_seconds` (histogram) plus an in-flight gauge; WARN past
  `telemetry.lock_wait_warn`. Metric names are kept Rust-compatible (§8.10 uses `walgit_*`; walhub renames
  the prefix — see Decisions).

## 4. Striped upload and striped download

### 4.1 Striped upload (`PutFileParallel`)

Used for packs, bundles, LFS, and composed artifacts. Package: `internal/store` (algorithm) with the
concurrency primitive from §5.2.

- If the backend cannot compose natively (`ComposeIsNative() == false`, i.e. S3) **or** `size ≤ 128 MiB`
  → single `Put` with the caller's PutMode (Create applies to the final object only).
- Else: part size = clamp(ceil(size/1024), 64 MiB, 1 GiB); upload parts concurrently as
  `<key>.part/<i:042d>` with PutOverwrite.
- Compose: ≤ 32 parts in one `Compose` call → done. More: per-32 groups composed into
  `<key>.part/mid<g:042d>`, then one final compose of the mids (two levels; max 1024 parts ⇒ two levels
  suffice: 1024 = 32×32). Part and mid keys are best-effort deleted after success (Delete errors logged,
  never propagated).
- PutMode (Create/Update) applies to the **final object only**; parts/mids are always Overwrite.

```go
// Shape (internal/store/striped.go)
func PutFileParallel(ctx context.Context, s ObjectStore, key string, f *os.File, size int64, opts PutOptions) (ObjectMeta, error) {
    if !s.ComposeIsNative() || size <= 128<<20 { return s.Put(ctx, key, putBodyFromFile(f, size), opts) }
    n := (size + partSize(size) - 1) / partSize(size) // ≤ 1024 by construction
    g, ctx := errgroup.WithContext(ctx)               // hand-rolled; §5.3
    g.SetLimit(8)                                     // bounded stripes; not 1024 goroutines
    for i := 0; i < int(n); i++ { i := i
        g.Go(func() error { return uploadPart(ctx, s, key, i, f, size, partSize(size)) })
    }
    if err := g.Wait(); err != nil { cleanupParts(key, n); return ObjectMeta{}, err }
    return composeTwoLevels(ctx, s, key, int(n), opts) // deletes parts/mids best-effort
}
```

### 4.2 Striped download (large pack materialization)

- Chunk = **32 MiB**, **16 concurrent stripes** per object. Requires a known size: take it from
  `PackRef` when available (skips the HEAD); otherwise one HEAD first.
- Preallocate the destination file (`f.Truncate(size)`), then write each stripe at its offset via
  `WriteAt` — no seeks, no ordering constraint between stripes.
- A **short read is a Corrupt error** (`StoreError{Kind: Other}` wrapped as corruption; do not
  silently pad). Stripes retry per §3.3 resume rules on GCS / plain retry on S3.

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(16)
for off := int64(0); off < size; off += 32 << 20 {
    end := min(off+32<<20, size) // half-open
    g.Go(func() error { return readRangeInto(ctx, s, key, off, end, f) })
}
err := g.Wait()
```

#### Concurrency

- Hazard: 1024 part uploads at once exhaust file descriptors and hide head-of-line latency. Avoidance:
  bounded errgroup (`SetLimit(8)` upload / `SetLimit(16)` download stripes); see `13_concurrency.md` for
  the canonical bounded-errgroup pattern. **Context cancellation on first error**: use the errgroup's
  derived context so one failed stripe cancels all in-flight siblings immediately; cleanup (abort
  multipart, delete parts) runs after `Wait()` returns, on the parent context, with its own short timeout.
- Hazard: a stripe retrying forever pins a semaphore permit. Avoidance: retries live *inside* the stripe
  goroutine with per-attempt deadlines (§3.5 table) and a total attempt cap; the permit is released only
  when the goroutine returns.
- Who owns/closes channels: no channels are needed — results go straight to the file via `WriteAt`; the
  only synchronization is `g.Wait()`. Do not introduce a results channel "for symmetry".

## 5. Bulk vs control-plane transport discipline in Go

### 5.1 Separate http.Clients

Per backend instance:

| Client | Pool | Used for |
|---|---|---|
| control | `MaxIdleConnsPerHost: 8`, idle timeout 90 s | `.pb`/`.json` keys, non-ranged gets/puts, lists, deletes, leases |
| bulk | `MaxIdleConnsPerHost: 64`, `MaxConnsPerHost` ≈ bulk_concurrency | `wal/`, `bundles/`, `lfs/`, every Range, every stripe |

On GCS the split is mandatory (§3.6). On S3 one client is *acceptable*, but the classifier MUST still
exist and both clients route through the same signing code. Rust's "4 dedicated bulk workers" maps to the
semaphore below plus the materialization worker pool (see `13_concurrency.md`); bulk work never runs on
request goroutines.

### 5.2 Weighted semaphore with permit-wait metrics

Hand-roll (no `golang.org/x/sync`):

```go
type Weighted struct { mu sync.Mutex; cond *sync.Cond; cur, cap int64 }
// Acquire(ctx, n) blocks on cond; on ctx.Done returns ctx.Err() AND releases nothing (nothing taken).
// TryAcquire(n) bool for the try-lock rule. Release(n) wakes waiters.
```

Wrap every bulk call site:

```go
if err := sem.Acquire(ctx, weight); err != nil { return err }        // ctx-aware, no leaks
defer sem.Release(weight)
start := time.Now()
// … actual request …
bulkQueueSeconds.Observe(time.Since(start).Seconds())                 // name: walhub_store_bulk_queue_seconds
if wait := time.Since(start); wait > lockWaitWarn { slog.Warn("bulk permit wait", "wait", wait, "key", key) }
```

Wait-time is measured from *before* Acquire to *after* acquisition (queue time), separately from request
time. Capacity = `store.gcs.bulk_concurrency` (32) on GCS; on S3 use a generous default (e.g. 64) or share
the GCS-style config knob — one semaphore per backend instance.

### 5.3 errgroup equivalent

Hand-rolled in `internal/store/errgroup.go` (~30 lines): `group{ctx, cancel, wg, sem}`; `Go(f)` runs `f`
in a goroutine, stores the first non-nil error, and calls `cancel()`; `Wait` = `wg.Wait()` + cancel +
return first error; `SetLimit(n)` gates `Go` on an internal counting semaphore. This is the only
concurrency helper the store layer needs; the WAL/git layers have their own (see `13_concurrency.md`).

#### Concurrency

- Hazard: goroutine leak when a caller abandons a bulk read. Avoidance: every request carries the caller's
  `ctx`; the semaphore Acquire and the HTTP request both honor it; response bodies are always closed via
  `defer resp.Body.Close()` — an unclosed body pins the connection and, with `MaxConnsPerHost` set,
  deadlocks the pool.
- Hazard: one shared transport starves control-plane traffic. Avoidance: the two-client split (§5.1) plus
  classifier routing; the control client never takes a bulk permit, so a 7.5 GB download can add latency
  to *bulk* work only.
- Never block a request goroutine on bulk work: materialization and striped downloads run on the bounded
  worker pool (see `13_concurrency.md`), not on the HTTP handler goroutine.

## 6. Round-trip budget model (normative)

Every protocol touching the bucket is judged on sequential round trips (depth) and total requests. Happy
path budgets to defend (copy of §4.8 — normative numbers):

| Operation | Depth | Requests |
|---|---|---|
| Any read (info/refs, ls-refs, web refs/resolve) | 1 conditional GET (0 within freshness_ttl) | 1 |
| Cold refs sync | manifest GET → (checkpoint refs ∥ tail segments) | 2 + tail (no checkpoint: 1 + tail) |
| Push / publish (per batch) | freshness GET → (pack PUTs ∥ log PUT) → manifest CAS | 5 (4 if already synced) |
| Checkpoint | freshness GET → (refs PUT ∥ checkpoint PUT) → manifest CAS | 4 |
| Settings publish | refs sync → log PUT → manifest CAS | 3 (readers pay 0) |
| Lease acquire | GET → CAS put | 2 |
| Repo listing | 0 within 30 s cache; else prefixes → (HEAD manifests ∥ owners) | 1 + owners + repos |
| Maintainer bundle pass | list GET → ≤ 1 retention CAS → ≤ 1 verdict-batch CAS | ≤ 3 |

Rules of thumb (copy): depth before count; **let the conditional write be the read** (a 412 tells you what
a GET would have); verification on the failure path (HEAD only after 412); carry state in the manifest;
re-use held version tokens; batch at the CAS; never pay per ref or per pack on a hot path; immutable
answers cached forever (process LRU → bucket `cache/api/v1` → HTTP `immutable`); jitter every retry on a
CAS'd object.

**Asserting budgets in Go tests:** the memory backend gains an op counter; a counting
`RoundTripper` wraps the test clients:

```go
type CountingTransport struct{ Inner http.RoundTripper; N atomic.Int64 }
func (t *CountingTransport) RoundTrip(r *http.Request) (*http.Response, error) { t.N.Add(1); return t.Inner.RoundTrip(r) }
```

The contract suite (memory + FaultStore wrapper, §17.3 of the Rust spec) asserts exact numbers —
push ≤ 5, warm refs = 1, cold refs with one tail = 2, checkpoint = 4 — via `t.Cleanup`-scoped counters and
`assert.LessOrEqual`-style checks. Depth (parallelism structure) is asserted by issuing the second-phase
requests concurrently and requiring their start timestamps to precede the first phase's completion (a
simple instrumented transport recording per-request start/end suffices).

## 7. Leases

Object: protobuf `Lease{holder=1, purpose=2, acquired_at=3, expires_at=4, epoch=5}` (wire encoding per
doc 02; field numbers frozen) stored at `leases/<name>.pb` (repo-scoped — the store prefix already scopes
it). Rust guards map to Go: LeaseGuard is a struct with `Release()`; use `defer lease.Release()`.

- **Acquire:** GET lease (absent) → `Put` Create (epoch 0). Present: stealable only when
  `now ≥ expires_at + 2 s` (clock-skew tolerance) → CAS `PutUpdate` with `epoch+1`; 412 → lost race,
  return None. Not expired → None.
- **Heartbeat:** CAS-rewrite with `epoch+1`, `expires_at = now+ttl`; loss (412) = `LeaseLost` — stop work
  immediately. Long holders run a background heartbeat; transient store errors are retried, loss releases.
- **Release:** CAS delete (`ifGenerationMatch` / HEAD-then-delete on S3); deleting an already-stolen or
  absent lease is Ok. `Release` is best-effort (log on failure, never panic).
- **Names in use:** `leases/compact.pb` (compaction + base rebuild; TTL `compaction.lease_ttl`, default
  10 m; no heartbeat — TTL + release suffice); `leases/bundle-<strategy>.pb` (per-strategy; TTL per
  strategy; NOTE: the bundle variant historically lacks the 2 s skew tolerance — preserve the behavior).
- **Polling acquire:** jittered backoff 10–200 ms until `wait_up_to` elapses, then None.

### 7.1 Go API

```go
type Lease struct {
    Holder     string    // instance id
    Purpose    string
    AcquiredAt time.Time
    ExpiresAt  time.Time
    Epoch      uint64
}

type LeaseGuard struct {
    store ObjectStore; key string; ttl time.Duration
    cancel context.CancelFunc; done chan struct{} // closed when heartbeat stopped
    lost atomic.Bool
}
// AcquireOrSteal(ctx, name, ttl) (*LeaseGuard, error) — the 2-RTT acquire; CAS loop §7.
// g.Lost() bool — set when a heartbeat observed 412; workers check between units of work.
// g.Release() — stops heartbeat (once), CAS-deletes, idempotent.
```

Heartbeat loop: one goroutine per held lease, tick = ttl/3, single-flight with the release path (a mutex
ensures a concurrent Release stops the ticker before the next fire; after `Release` returns, no heartbeat
write is in flight). On heartbeat 412 → set `lost`, stop, do NOT release (the thief owns it now); on
Retryable → retry next tick; on context cancel → exit silently.

#### Concurrency

- Hazard: heartbeat goroutine outliving the work or racing Release (double epoch bump → self-steal).
  Avoidance: the guard owns exactly one goroutine; lifecycle = context cancel + `done` channel handshake
  (close-then-join); `Release` is idempotent and single-flight with the heartbeat via one mutex. See
  `13_concurrency.md` (goroutine lifecycle: owner, close, join).
- Hazard: a process crash leaves a stale lease blocking everyone. Avoidance: that is the *design* — expiry
  (`expires_at + 2 s`) is the recovery path, so heartbeats must be reliable and TTLs conservative; never
  add a "force delete" escape hatch.
- Hazard: lost-lease work continuing to mutate shared keys. Avoidance: workers consult `g.Lost()` between
  work units; all shared-key mutations are lease-guarded by protocol, so a lost lease means stop, not
  retry-through.
- S3 lease delete is HEAD-then-act (§2.5): the steal race on delete is accepted — the 2 s skew plus epoch
  CAS on acquire/heartbeat covers it.

## 8. Memory backend (tests)

`BTreeMap` keyed (stdlib, no external LRU/map), global monotonic version counter unique across keys
(mimics GCS generations). Implements the full interface including compose (concat under lock, CAS via
Put) and range clamping. Test knobs: artificial per-op latency, `fake_object_urls` (accel returns a
GCS-like URL + bearer for edge tests), `signing_fails` (SignedGetURL errors like VPC-SC). The simulation
suite wraps it per-instance in a FaultStore (see `15_testing.md`); the op counter feeding the §6 budget
assertions lives here.

## 9. Contract suite (what an implementer must pass)

Shared tests run against all three backends (table-driven, `testing/T`, no third-party assert libs):

1. Put/Create/Update/Overwrite × Get/Head/Delete round-trips; version equality only.
2. Conditional GET: NotModified on equal IfNoneMatch; PreconditionFailed on wrong IfMatch.
3. Range: half-open [start,end) clamped; inclusive wire form verified per backend; short read = error.
4. Delete: CAS mismatch → PreconditionFailed; absent + unconditional → Ok.
5. List/ListPrefixes ordering, startAfter, delimiter behavior.
6. compose: 1..=32 sources, order preserved, destination CAS honored; S3 path exercises the UploadPartCopy
   path including a < 5 MiB source (ranged-read fallback).
7. Striped upload/download: 200 MiB synthetic file through both paths, byte-identical round-trip.
8. Budget assertions (§6) on the memory/FaultStore stack.
9. SigV4 test vectors; GCS token-source unit tests against an httptest OAuth server.

## Decisions & deviations from the Rust design

- **GCS uses the JSON API over plain HTTPS; no gRPC clients** — hard dependency rule (stdlib + two allowed
  modules only); the Rust gRPC topology (Storage + StorageControl + N bulk gRPC channels) is collapsed
  into control/bulk HTTP client pools with identical isolation semantics.
- **S3 implemented with presigned/signed plain HTTP instead of an SDK** — the AWS SDK Go hides conditional
  GET paths and adds a dependency; hand-rolled SigV4 (~150 lines) matches the Rust behavior exactly and is
  validated against the official AWS test vectors.
- **Weighted semaphore + errgroup are hand-rolled** (`internal/store`), not `golang.org/x/sync` — dependency
  rule; ~40 lines total, and the ctx-aware Acquire is the piece that matters.
- **Metric prefix renamed `walgit_*` → `walhub_*`** — product rename; histogram/gauge names and semantics
  (bulk queue seconds, in-flight, lock-wait WARN) preserved so dashboards port trivially.
- **Config keys unchanged** (`store.s3.*`, `store.gcs.bulk_clients`, `bulk_concurrency`, `multipart_*`,
  `telemetry.lock_wait_warn`) — compatibility requirement; `bulk_clients` counts HTTP client pools on GCS
  instead of gRPC channels (same default 4, same effect).
- **Two-level compose for striped upload retained exactly** (≤ 32 sources per Compose, mid objects,
  1024-part cap) — bucket-format compatibility with Rust-produced bundles.
- **S3 HEAD-then-delete and non-atomic compose pre-check races are accepted verbatim** — protocol-level
  lease guarding makes them safe; changing them would diverge the backends' observable behavior.
- **`SignedGetURL` on GCS via IAM signBlob over HTTPS** — the Rust path is the same call shape; no extra
  client library needed.
