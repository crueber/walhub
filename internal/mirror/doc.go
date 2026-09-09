// Package mirror implements Forgejo #240 (docs/features/11_mirror.md):
// pull-only mirror repos with scheduled upstream syncs.
//
// A repo becomes a mirror when repos/<o>/<r>/meta/mirror.json exists
// (Create-once-then-CAS'd, the frozen overwritable family —
// 14_extensibility.md §14.11 rule 2). The sidecar carries the upstream
// URL, the schedule preset, and the last sync outcome; next_sync_at is
// DERIVED at read time via the preset→cron map, never stored.
//
// Design rules (plan revision R1, normative on conflict):
//   - Public-upstreams-only v1: no stored secrets. A token is accepted
//     ONLY for the creation-time first sync and the manual "Sync now"
//     POST body (memory-only, never persisted, never logged); scheduled
//     fires fetch anonymously.
//   - Push refusal lives at the server's push funnel (pushPipeline top
//     covers HTTP+SSH pack flow; SSH advertisement refuses before the
//     client read; discovery answers 403). The sync engine publishes via
//     Publish/PublishRefs directly and never enters that pipeline.
//   - Sync converge is followOnce-shaped (compare + ff-only +
//     PublishRefs), reusing repoimport's NormalizeSource/CheckSSRF/
//     FilterRefs/scrub layers — never completeBody (create-only, 409s
//     on any move).
//   - Cross-instance exclusion is a bucket lease
//     (leases/mirror-<owner>-<name>.pb, CAS+TTL) taken before cloning.
//   - Enumeration is the in-memory repo registry + mirror.json probe
//     (no LIST); overdue-after-restart fires ONCE.
//   - Import-vs-sync exclusion: Begin on a mirrored target → 409;
//     a sync fire during a live repo-import claim → skip + narrate.
//   - Failures record consecutive_failures + capped backoff; a failed
//     sync never moves the computed next fire. Upstream rewind is
//     refused + narrated (ff-only default); force-resync is the escape.
//
// Seams (14_extensibility.md): Seam 1 via Handler (server.ExtraRoutes,
// both lanes, repo lanes + the create-from-URL top-level twin); Seam 5
// via KindMirrorSync run on the core wal.TaskTable; Seam 7 via the
// `walhub mirror` CLI. Core (internal/store, internal/wal,
// internal/git) is never imported upward: this package depends only on
// seam interfaces + frozen types. Git is always the subprocess binary
// with pinned argv (docs/go/04_git.md); scheduled fetches carry no
// credentials at all.
package mirror
