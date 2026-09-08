// web/src/pages/HowItWorks.jsx — route "/how-it-works": the developer
// deep-dive page (issue #191). Static marketing, ZERO API calls (no useData,
// no SDK import, no fetch): like Landing.jsx, it spends no store round trips.
// Concept GIFs are reused verbatim via CONCEPT_ALTS imported from Landing.jsx
// (one source of truth for alt text); the two commit/checkpoint figures are
// static inline SVGs (HowDiagrams.jsx — no animation, no new deps).
//
// Every non-obvious technical claim below carries a trace comment (doc section
// or code path + config key names, which move less than section numbers).
// Numbers are sim-asserted transport budgets (docs/go/15_testing.md §4.8),
// never speed promises; no ms figures anywhere on the page.

import { A } from "@solidjs/router";
import ConceptGif from "../components/ConceptGif.jsx";
import { CONCEPT_ALTS } from "./Landing.jsx";
import { CasDiagram, CheckpointDiagram } from "../components/HowDiagrams.jsx";

const CAPTIONS = {
  push: "Push over smart HTTP; objects land in the bucket.",
  bucket: "No SQL, no Redis. Kill the instance; the bucket doesn't notice.",
  fetch: "Clone reads objects back.",
  collab: "Issues, PRs, reviews, and checks live next to git data.",
};

function Section(props) {
  return (
    <section id={props.id} class="scroll-mt-16 py-8">
      <h2 class="text-xl font-semibold tracking-tight">{props.title}</h2>
      <div class="mt-3 space-y-3 text-sm leading-relaxed">{props.children}</div>
    </section>
  );
}

export default function HowItWorks() {
  return (
    <div class="how-page mx-auto max-w-4xl">
      <section class="py-10 text-center">
        <h1 class="text-3xl font-bold tracking-tight">How walhub works</h1>
        {/* Trace: README.MD L3–5; AGENTS.md law 4 ("The bucket is the repository"). */}
        <p class="muted mx-auto mt-4 max-w-2xl text-sm leading-relaxed">
          walhub is a git host whose only database is an object store. Every repository's state —
          refs, packs, config, policy — lives as objects in a bucket (filesystem, S3, or GCS).
          Instances are disposable: wipe one and you lose nothing but warmth.
        </p>
        {/* Wire-compat wording copies Landing/README verbatim — never paraphrased stronger. */}
        <p class="muted mt-4 text-xs">
          Object-protocol compliant with walgit — bucket layout, protobuf wire encoding, and git wire
          behavior follow walgit's formats.
        </p>
        <p class="muted mt-3 text-sm">
          <A class="hover:underline" href="/">
            ← walhub in 30 seconds
          </A>
        </p>
        <nav class="muted mt-6 flex flex-wrap items-center justify-center gap-x-4 gap-y-2 text-sm" aria-label="On this page">
          <a class="hover:underline" href="#idea">the idea</a>
          <a class="hover:underline" href="#wal">the WAL</a>
          <a class="hover:underline" href="#push">push</a>
          <a class="hover:underline" href="#read">fetch</a>
          <a class="hover:underline" href="#checkpoints">checkpoints</a>
          <a class="hover:underline" href="#boundaries">boundaries</a>
        </nav>
      </section>

      <div class="divide-y divide-zinc-200 dark:divide-zinc-800">
        <Section id="idea" title="The original idea: git objects love dumb object stores">
          {/* 1. Trace: docs/go/02_storage_protobuf.md §2.1 (immutable pack/sidecar keys
              wal/<checksum>.pack|.idx|.rev|.bitmap|.commit-graph) + store.go PutCreate. */}
          <p>
            Git objects are content-addressed, so packs and their sidecars drop unchanged into a
            key-value bucket. The store needs no understanding of git — it just keeps bytes under
            keys.
          </p>
          {/* 2. Trace: 02 §2.1 key table (repos/<owner>/<repo>/…) + §2.6 ObjectStore; law 4. */}
          <p>
            There is no database to run, back up, or scale: anything that must survive a restart is
            an object under <code>repos/&lt;owner&gt;/&lt;repo&gt;/</code>; disk and memory are
            caches. Any S3, GCS, or filesystem works behind one <code>ObjectStore</code> contract
            (CAS, conditional GET, compose, leases).
          </p>
          <ConceptGif name="bucket" alt={CONCEPT_ALTS.bucket} caption={CAPTIONS.bucket} />
          {/* 3. Trace: 02 §2.6–2.7 (PutCreate/PutUpdate, casUpdate; "the store is the lock"). */}
          <p>
            The store is dumb on purpose: conditional writes (CAS) and conditional reads are the only
            coordination primitives — there are no locks on the store.
          </p>
          {/* 4. Trace: AGENTS.md law 6; docs/go/15_testing.md §4.8 budget table. */}
          <p>
            Round trips are the cost model: every protocol change is judged on counted sequential
            bucket round trips (push ≤ 5 — 4 if already synced; warm refs depth 1, cold refs depth
            2; checkpoint 4). Budgets, not benchmarks.
          </p>
        </Section>

        <Section id="wal" title="What the WAL is: the log of manifest mutations">
          {/* 1. Trace: 02 §2.2 Manifest schema; docs/go/05_wal_engine.md §5.0 rule 1. */}
          <p>
            The <strong>manifest</strong> (<code>repos/&lt;o&gt;/&lt;r&gt;/manifest.pb</code>) is the
            linearization point: <code>head_seq</code>, <code>min_seq</code>, the checkpoint pointer,
            the <code>log_segments</code> covering [<code>min_seq</code>, <code>head_seq</code>], the
            denormalized live pack set, settings, and a monotonic revision.
          </p>
          {/* 2. Trace: 02 §2.2 EntryKind/LogEntry + §2.4 framing; 05 §5.3.3. */}
          <p>
            The <strong>WAL</strong> is the append-only stream of <code>LogEntry</code> frames in{" "}
            <code>log/&lt;seq:016x&gt;.pb</code> segments (uvarint-length-prefixed; a partial
            trailing frame is tolerated on appendable segments). Five entry kinds:{" "}
            <code>PUSH</code> (pack + ref txn), <code>COMPACT</code> (new pack superseding old),{" "}
            <code>REF_UPDATE</code> (ref-only), <code>CHECKPOINT</code> (marker),{" "}
            <code>SETTINGS</code> (history — the latest rides the manifest inline).
          </p>
          {/* 3. Trace: 05 §5.0 rules 1–2; 02 §2.6 error taxonomy (PreconditionFailed is protocol-normal). */}
          <p>
            <strong>The manifest CAS is the single commit point.</strong> A manifest write is always
            conditional (<code>PutUpdate(version)</code> / <code>PutCreate</code>); a 412 is the
            normal contention signal, never an error.
          </p>
          <CasDiagram />
          {/* 4. Trace: 02 §2.5 seq semantics; 05 §5.4 burn ladder (3 probes × 100ms, cap 8 → Corrupt). */}
          <p>
            Seq numbers are strictly increasing but <strong>not dense</strong>: a writer that crashes
            between its log PUT and its manifest CAS leaves an orphan; later writers burn past it
            (3 probes × 100 ms, cap 8 consecutive burns → Corrupt). Orphans are harmless and swept
            after a later commit.
          </p>
          {/* 5. Trace: 05 §5.3.1–5.3.2 + Concurrency notes; internal/wal/publish.go.
              Pins: wal.batch_window (5ms), wal.max_batch (64), wal.cas_max_retries (16). */}
          <p>
            Concurrent writers stay safe without store locks: one single-flight publisher goroutine
            per repo serializes all publishes in-process; across instances the CAS serializes
            commits, and losers re-sync, re-verify, and retry on a bounded ladder (16 attempts).
            Group commit batches arrivals (5 ms window, up to 64) so concurrent pushes share one
            commit.
          </p>
          {/* 6. Trace: 05 §5.3.2 step 3 (old-value-checked ref txns; Seq: 0 rejections). */}
          <p>
            Ref updates are old-value-checked transactions (<code>old_oid</code> must equal current
            unless all-zero creation; symbolic HEAD updates always ok). Rejected pushes are
            transport successes with per-ref errors.
          </p>
        </Section>

        <Section id="push" title="How a push flows, end to end">
          {/* Ladder trace: 05 §5.3.2 steps 1–8; law 4 (never ACK before the bucket ACKs). */}
          <ol class="list-decimal space-y-2 pl-5">
            <li>Sync (refs+serve) unless already fresh — snapshot the manifest + version.</li>
            <li>Verify each ref txn against current refs (old-value check).</li>
            <li>
              Assign seqs (<code>head_seq+1</code> …), build <code>PUSH</code> entries with pack refs
              plus caller meta (principal, request id, push options).
            </li>
            <li>
              In parallel: upload pack+idx <strong>create-if-absent</strong> (duplicates are success)
              while claiming the log slot (PutCreate; 412 → re-read, burn-or-retry per §5.4).
            </li>
            <li>Build the manifest update (head_seq, extended pack set, new segment ref, revision+1).</li>
            <li>
              CAS the manifest. <strong>The server never ACKs the push before the bucket ACKs</strong> —
              the push reply is answered only after commit.
            </li>
            <li>
              Commit locally under syncMu, <strong>refs first, then advertise</strong>;
              withdraw-on-failure (reset the advertised version so the next sync replays — but still
              answer ok, because the bucket is the truth).
            </li>
            <li>Sweep burned orphans; fold commit-graph work off the critical path; opportunistic checkpoint check.</li>
          </ol>
          <ConceptGif name="push" alt={CONCEPT_ALTS.push} caption={CAPTIONS.push} />
        </Section>

        <Section id="read" title="How a fetch/clone reads it back">
          {/* 1. Trace: 05 §5.2 sync-level table; internal/wal/sync.go. */}
          <p>
            Every request syncs first, at one of four levels: <strong>Refs</strong> (checkpoint
            snapshot + log-tail ref txns → local packed-refs, no packs) serves info/refs, ls-refs,
            bundle lists, and web refs; <strong>Serve</strong> adds the pack set this instance can
            hold; <strong>Full</strong> materializes everything (refused with ErrTooLarge over
            budget); <strong>Objects</strong> serves-or-remote-reads for the web API.
          </p>
          {/* 2. Trace: 05 §5.2 step 2 (offline packed-refs rewrite) + §5.3.3 (new_peeled, max 16 hops). */}
          <p>
            Refs apply is an offline full packed-refs rewrite (parse → map → tmp+rename), atomic
            across many refs, and it works before packs exist. Annotated-tag{" "}
            <code>^{"}"}</code> comes from writer-recorded peeled values (max 16 hops), so replicas
            advertise without objects.
          </p>
          {/* 3. Trace: 15_testing.md §4.8 budget table. R1 S2: depth, not requests
              (cold = 2+tail with checkpoint, 1+tail without); N2: push 4 if synced. */}
          <p>
            Budgets: warm refs resolve at depth 1 (conditional GET; 0 within the freshness TTL);
            cold refs at depth 2 (manifest GET → checkpoint refs in parallel with the tail; depth 1
            without a checkpoint); push ≤ 5 (4 if already synced); checkpoint 4 (never a log GET —
            provenance rides the applied state).
          </p>
          {/* 4. Trace: docs/go/08_bundles.md §§8.1/8.4 (creationToken = slot epoch seconds). */}
          <p>
            At scale, clones ride <strong>bundle-uri</strong>: the CAS'd{" "}
            <code>bundles/list.pb</code> advertises full + incremental bundles with creationToken =
            slot epoch seconds; git downloads bundles, then fetches the remainder.
          </p>
          <ConceptGif name="fetch" alt={CONCEPT_ALTS.fetch} caption={CAPTIONS.fetch} />
          {/* 5. Trace: 05 §5.7 (v1 decision: stock git + materialized packs + bundle-uri;
              no native serving engine in v1) + remote reader/faulter. */}
          <p>
            Too-large repos don't break stock git: serve-tier packs can be remote-served (side-files
            + store mount, or no local copy) with the block-cache remote reader and a fetch-path
            faulter filling gaps for the web API and fetches. Stated plainly: v1 serves through the
            stock git subprocess plus materialized packs and bundle-uri — there is no native serving
            engine in v1.
          </p>
        </Section>

        <Section id="checkpoints" title="Checkpoints, ref snapshots, and cold starts">
          {/* 1. Trace: 05 §5.5 para 1. Pins: wal.snapshot_every_entries (256),
              wal.checkpoint_tail_bytes (8 MiB), wal.checkpoint_interval (1h). */}
          <p>
            Checkpoint triggers (any; 0 disables): ≥ 256 entries since the last checkpoint, tail
            bytes over 8 MiB, or age ≥ 1 h. Evaluated after publishes (background, off the reply
            path) and by the maintainer loop.
          </p>
          {/* 2. Trace: 05 §5.5 (2-round refs-level write, Create-idempotence + CAS races). */}
          <p>
            The write is refs-level (it works on an instance that could never hold the packs), in 2
            rounds: <code>checkpoint.pb</code> alongside <code>refs.pb</code> (PutCreate,
            deterministic seq-keyed), then the manifest CAS (checkpoint,{" "}
            <code>min_seq = seq+1</code>, trim folded segments, revision+1). Idempotent at equal seq;
            racing checkpointers resolve benignly via Create-idempotence + CAS.
          </p>
          <CheckpointDiagram />
          {/* 3. Trace: 05 §5.5 "Cold start fold"; 02 §2.5. */}
          <p>
            <strong>Cold-start fold:</strong> a fresh instance loads <code>refs.pb</code> at the
            checkpoint and replays only the tail — it never replays the whole log.{" "}
            <code>min_seq = checkpoint.seq + 1</code>; below is folded away.
          </p>
          {/* 4. Trace: 05 §5.6 (refsAtSeq/refsAsOf; unreplayable cuts). */}
          <p>
            Point-in-time reads fold from the newest usable checkpoint plus ordered entries; cuts
            older than <code>min_seq</code> with no usable checkpoint are unreplayable (which is why
            provenance timestamps exist).
          </p>
          {/* 5. Trace: 02 §2.1 key table; 05 §5.3.3 publish_settings (≤ 16 KiB validated TOML). */}
          <p>
            <code>policy.json</code> is NOT on the WAL (admin API/CLI object); per-repo settings ARE
            published (manifest-inline + <code>SETTINGS</code> history, ≤ 16 KiB TOML, validated
            before publish).
          </p>
        </Section>

        <Section id="boundaries" title="Honest boundaries: what the WAL deliberately does NOT do">
          {/* 1. Trace: docs/features/README.md L11–15 (the one architectural law), P1–P4. */}
          <p>
            The WAL stays <strong>git-only</strong>. Issues, PRs, reviews, checks, and notifications
            are a <strong>parallel object family</strong> (
            <code>orgs/…</code>, <code>users/…</code>,{" "}
            <code>repos/&lt;o&gt;/&lt;r&gt;/{"{meta,issues,checks,releases,access.json}"}</code>) with
            its own CAS discipline — never WAL entries, never the manifest, never gating a push
            (except policy effects that explicitly consult them, e.g. required checks).
          </p>
          <ConceptGif name="collab" alt={CONCEPT_ALTS.collab} caption={CAPTIONS.collab} />
          <p>
            What that buys: collaboration writes never contend with the push CAS ladder; git stays
            readable without the collaboration layer and vice versa.
          </p>
          {/* 3. Trace: docs/features/README.md P9. */}
          <p>
            Explicitly out of this layer's scope (so readers don't project): code search, CI runners
            (walhub stores check results, doesn't run CI), Discussions/Packages/Projects, SAML/SCIM.
          </p>
          {/* 4. Trace: 01_overview.md "No LIST on a hot path"; 05 §5.7; 02 §2.5. */}
          <p>
            And what the WAL itself doesn't do, stated plainly: no query engine (reads are key probes
            + replay, no LIST on hot paths); no cross-repo transactions (one publisher per repo; the
            CAS is per-manifest); no native git serving engine in v1; burned-seq gaps are normal, not
            damage.
          </p>
        </Section>
      </div>

      <section class="card my-8 p-4">
        <h2 class="text-lg font-semibold">Where to go next</h2>
        <p class="muted mt-2 text-sm leading-relaxed">
          Push something (the 30-second quickstart lives on <A class="hover:underline" href="/">/</A>
          ), browse <A class="hover:underline" href="/explore">/explore</A>, configure{" "}
          <A class="hover:underline" href="/setup">/setup</A>, watch a live manifest on a repo's WAL
          tab, or read the <A class="hover:underline" href="/api">/api docs</A>.
        </p>
      </section>
    </div>
  );
}
