# Performance evidence

Measured claims about walhub's behavior under load — not estimates. Every entry
in this document was produced by a harness committed to the repo (reproduction
command included), on the real code path, and states the backend it ran on.
Entries are added when a feature's scale behavior needs pinning (explicitly
requested or when a review questions a hot path).

## Method standards (what qualifies as an entry)

- **Measured, not modeled.** The harness runs the real code path — no mocks for
  the layer under test. Fixtures may be synthetic; the measurement is not.
- **Named backend and hardware.** Absolute numbers are environment-bound
  (this workstation is a QEMU VM); the *shape* claims — flat vs linear vs
  quadratic, per-op vs per-request — are the durable part of every entry.
- **Reproducible.** Each entry names its harness (package + env var + command)
  and the harness lives in `internal/devtools/` or the package's own tests.
- **Both sizes.** Entries measure at a "normal" population and an order(s) of
  magnitude beyond, so flat-vs-growing is visible in the table itself.
- **Hot paths vs convenience paths are called out.** A UI page taking 100×
  longer than an auth lookup is a different kind of fact than an auth lookup
  degrading.

## Index

| # | Date | Area | Question | Verdict |
|---|---|---|---|---|
| E1 | 2026-09-03 | SSH key registry (`internal/server/sshkeys.go`) | 10k vs 1M registered keys: what happens to auth, the keys page, and writes? | Auth flat (O(1) per handshake); keys page linear in per-user keys; writes constant. Scale-safe to 1M+. |

---

## E1 — SSH key registry at 10k and 1M keys (2026-09-03)

**Area:** SSH git transport, key auth (`internal/server/sshkeys.go`,
`internal/sshd`). Spec: `17_ssh.md` §3/§5.

**Question:** the registry stores one object per key in two families
(`ssh-keys/k/<fp>` for auth lookup, `ssh-keys/u/<principal>/<fp>` for the UI
list). What happens to (a) SSH auth, (b) the keys page, and (c) writes when the
host accumulates 10,000 keys — and 1,000,000?

**Method.** Harness: `internal/devtools/keybench` (`go test -tags evidence
./internal/devtools/keybench/ -run TestSSHKeyRegistryScale -v -timeout 30m`;
the `evidence` build tag is what keeps the harness out of CI and `go vet`).
Real ed25519 keypairs (1 per key — distinct fingerprints), registered through
the real `Add` path over the **memory store**, keys spread over 100 principals
(the population shape: many users, a handful of keys each — the benchmark's
`user0` deliberately holds n/100 keys to also probe the pathological end).
Measured: 1000 auth lookups (mean/max), one keys-page list, one add. Memory
store isolates the registry's algorithmic shape from network RTT; see
"over the network" below for the S3/GCS constant.

**Results.**

| path | 10,000 keys | 1,000,000 keys | shape |
|---|---|---|---|
| setup (n adds, 2 PUTs each) | 0.27s (~27µs/add) | 29.9s (~30µs/add) | constant per add |
| **SSH auth lookup** (1 GET by fingerprint) | mean **5.9µs**, max 221µs | mean **5.6µs**, max 29µs | **flat** — O(1), no LIST |
| keys-page list (one principal) | 5.1ms for 100 keys | 1.51s for 10,000 keys | linear in **per-principal** key count |
| store footprint | ~20k objects | ~2M objects | grows with total keys |

**Analysis.**

- **Auth is flat because the fingerprint IS the storage key.** `LookupBy-
  Fingerprint` is one exact-key GET + one in-memory rights resolution
  (`PrincipalForName`). No LIST, no scan, no cache to warm: 5.9µs at 10k keys,
  5.6µs at 1M. An SSH handshake runs one lookup, and handshakes are rare
  relative to HTTP git traffic — auth cost per handshake is invariant at any
  population size. This is the property that matters most, and it holds.
- **The keys page is linear in per-user keys, not total keys.** `List` walks one
  principal's prefix and decodes each entry (the listing entry carries the full
  record; LIST returns only metadata, so one GET per entry). The bound is
  keys-per-principal — 5.1ms for a 100-key principal, 1.51s for a 10,000-key
  principal, at every total population. Real distributions (1–20 keys/user)
  sit at the low end. If a deployment ever needed more, the fix is mechanical:
  make the listing entry self-sufficient so LIST alone answers (index format
  change, not a redesign) — noted as the scale-out lever, not built.
- **Writes are constant** — 2 PUTs per add (record PutCreate + index
  overwrite), fingerprint uniqueness enforced by the store's PutCreate
  (duplicate → 409 at the API). ~30µs/add on the memory store across the whole
  1M-key run.

**Over the network (S3/GCS).** The memory-store numbers isolate the shape;
every auth lookup then costs one GET (~60–80ms typical per-object latency) and
the keys page costs (1 + K) GETs. Flat in total keys, per the table — at 1M
keys an SSH handshake is still exactly one GET.

**Operational notes at 1M keys** (what grows — storage and housekeeping, not
latency):

- ~2M store objects, ~1 GB of small objects. Trivial for S3/GCS. On the
  **filesystem backend** that is 2M inodes across `ssh-keys/k/` and the
  per-principal `u/` dirs — ext4 lookups stay O(log n), but inode budget and
  backup size feel it.
- No key expiry: keys are identity and accumulate (same as every git host).
- One rollback window: if the listing-entry PUT fails after the record PUT
  commits, an orphan k-doc survives — unlistable, and it blocks that
  fingerprint's re-registration until removed. One store call wide; at any
  scale this is a rounding-error event, but it is the one thing a cleanup tool
  would have to do.
- The store-backed registry is only half the key story; the server's SSH host
  key is machine-local (data dir, not the bucket) — see `17_ssh.md` §3.

**Verdict.** Scale-safe where it matters: auth is O(1) per handshake at every
population size; the UI path is bounded by per-user key counts; writes are
constant. The 10k → 1M jump costs storage footprint and housekeeping, not
user-visible latency. No redesign required; the UI-list self-sufficiency
change is the documented lever if keys-per-principal ever grows large.
