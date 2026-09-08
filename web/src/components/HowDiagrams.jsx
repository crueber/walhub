// web/src/components/HowDiagrams.jsx — static inline diagrams for /how-it-works
// (issue #191). Zero JS behavior, zero deps: pure SVG plus a <details> text
// fallback each, so nothing is GIF-only. Theme-safe: currentColor strokes and
// muted labels read in dark (default) and light; no animation, no new assets.

export function CasDiagram() {
  // Trace: docs/go/05_wal_engine.md §5.3.2 (publish ladder) + §5.4 (burn
  // protocol); docs/go/02_storage_protobuf.md §2.6 (PutCreate/PutUpdate CAS).
  return (
    <figure class="card my-4 p-4">
      <svg
        viewBox="0 0 640 236"
        width="100%"
        role="img"
        aria-label="Commit-point sequence: log segment create, then manifest conditional update, then local refs apply, then answer ok"
      >
        <g font-family="inherit" font-size="12">
          <rect x="8" y="24" width="146" height="52" rx="8" fill="none" stroke="currentColor" stroke-opacity="0.5" />
          <text x="20" y="46" fill="currentColor">pack+idx PUT</text>
          <text x="20" y="62" fill="currentColor" opacity="0.65">create-if-absent</text>
          <rect x="8" y="88" width="146" height="52" rx="8" fill="none" stroke="currentColor" stroke-opacity="0.5" />
          <text x="20" y="110" fill="currentColor">log slot PUT</text>
          <text x="20" y="126" fill="currentColor" opacity="0.65">PutCreate (412 → retry)</text>
          <text x="158" y="88" fill="currentColor" opacity="0.65">∥ in parallel</text>
          <text x="168" y="56" fill="currentColor">→</text>
          <rect x="252" y="48" width="146" height="52" rx="8" fill="none" stroke="currentColor" />
          <text x="264" y="70" fill="currentColor">manifest CAS</text>
          <text x="264" y="86" fill="currentColor" opacity="0.65">PutUpdate(version)</text>
          <text x="402" y="80" fill="currentColor">→</text>
          <rect x="426" y="48" width="206" height="52" rx="8" fill="none" stroke="currentColor" stroke-opacity="0.5" />
          <text x="438" y="70" fill="currentColor">local commit, refs first</text>
          <text x="438" y="86" fill="currentColor" opacity="0.65">then advertise → answer ok</text>
          <text x="8" y="168" fill="currentColor" opacity="0.65">412 contention → delete own segment, re-sync, retry</text>
          <text x="8" y="186" fill="currentColor" opacity="0.65">ambiguous error → re-read casLanded, delete nothing</text>
          <text x="8" y="204" fill="currentColor" opacity="0.65">never ACK before the bucket ACKs (law 4)</text>
          <text x="8" y="226" fill="currentColor" opacity="0.65">burned-seq gaps are normal, not damage</text>
        </g>
      </svg>
      <details class="muted mt-2 text-sm">
        <summary class="cursor-pointer hover:underline">Text version of this diagram</summary>
        <p class="mt-2 leading-relaxed">
          The writer uploads pack+idx (create-if-absent) in parallel with claiming its log slot
          (PutCreate). It then commits with one conditional manifest write (PutUpdate on the version
          it synced). Only after the bucket acknowledges the commit does the server apply refs
          locally (refs first, then advertise) and answer the push ok. A 412 means contention: the
          loser deletes only its own segment, re-syncs, and retries. On an ambiguous error the writer
          re-reads whether its CAS landed and deletes nothing.
        </p>
      </details>
    </figure>
  );
}

export function CheckpointDiagram() {
  // Trace: docs/go/05_wal_engine.md §5.5 (checkpoints, cold-start fold);
  // docs/go/02_storage_protobuf.md §2.5 (seq semantics). Pins:
  // wal.snapshot_every_entries, wal.checkpoint_tail_bytes, wal.checkpoint_interval.
  return (
    <figure class="card my-4 p-4">
      <svg
        viewBox="0 0 640 150"
        width="100%"
        role="img"
        aria-label="Checkpoint timeline: folded prefix before min_seq, checkpoint snapshot at seq N, live tail up to head_seq"
      >
        <g font-family="inherit" font-size="12">
          <rect x="8" y="48" width="180" height="36" fill="currentColor" opacity="0.12" />
          <text x="16" y="63" fill="currentColor" opacity="0.65">folded away</text>
          <text x="16" y="78" fill="currentColor" opacity="0.65">&lt; min_seq</text>
          <line x1="188" y1="36" x2="188" y2="96" stroke="currentColor" stroke-width="2" />
          <text x="196" y="50" fill="currentColor">checkpoint @N</text>
          <text x="196" y="65" fill="currentColor" opacity="0.65">refs.pb snapshot</text>
          <rect x="196" y="48" width="300" height="36" fill="none" stroke="currentColor" stroke-opacity="0.5" />
          <text x="204" y="63" fill="currentColor" opacity="0.65">live tail: log/N+1 …</text>
          <text x="204" y="78" fill="currentColor" opacity="0.65">min_seq = N+1</text>
          <line x1="620" y1="36" x2="620" y2="96" stroke="currentColor" stroke-width="2" />
          <text x="560" y="112" fill="currentColor">head_seq</text>
          <text x="8" y="128" fill="currentColor" opacity="0.65">a fresh instance loads refs.pb, replays only the tail</text>
        </g>
      </svg>
      <details class="muted mt-2 text-sm">
        <summary class="cursor-pointer hover:underline">Text version of this diagram</summary>
        <p class="mt-2 leading-relaxed">
          A checkpoint writes a refs snapshot at seq N and advances min_seq to N+1; everything below
          is folded away. A fresh instance loads the snapshot and replays only the live tail — it
          never replays the whole log.
        </p>
      </details>
    </figure>
  );
}
