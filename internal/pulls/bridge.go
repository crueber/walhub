package pulls

import (
	"context"
)

// This file owns the §7 fork→base object bridge (issue #456): cross-fork PR
// heads contain fork-unique commits whose objects live only under the fork
// prefix (and hence only in the fork serving copy — the child→ancestor
// fallback in internal/wal/forkread.go runs the other direction). Every git
// operation that runs in the base serving copy (open's reachability verdict,
// merge-base, trial merge, diff, commit-tree, replay, pack-tip) needs those
// objects locally first.
//
// The bridge fetches the head sha from the fork serving-copy dir into the
// base serving-copy dir (GitRunner.FetchInto: local disk-to-disk, zero
// bucket round trips) and re-probes reachability. The base manifest is never
// touched — bridged objects are serving-copy warmth only (law 4: "if every
// instance is wiped, what is lost?" still answers "warmth"; the bridge
// re-runs on demand after any eviction).
//
// Publish discipline (load-bearing): `refs/pull/<num>/head` is published in
// the base repo ONLY for heads reachable WITHOUT the bridge (pre == true).
// A bridged-only head's objects are cache-resident, not bucket-backed —
// publishing a base-side ref to them would advertise a ref that dangles
// after the next cache eviction. Cross-fork PRs therefore stay fork-local
// (no base-side pull-head ref) exactly as before; the bridge only makes
// open/mergeability/merge/diff see the objects.
//
// Cost (law 6): the pre-probe is the existing reachability check —
// base-contained heads (the common case after the first bridge, and every
// same-repo head) cost zero extra subprocesses. Only a pre-miss pays one
// local fetch, on the failure path, with zero store round trips.
//
// ### Concurrency
//
// Hazard: concurrent bridges racing on one base serving copy (two PRs from
// one fork, or a merge racing a mergeability recompute), and a bridge racing
// serving-copy eviction/resync. Avoidance: the fetch is idempotent and
// ref-free (FetchInto's contract — concurrent fetches converge on identical
// objects, and no reader observes ref motion); no lock of any kind is held
// (pool-gated subprocess only, 13 §2 rule 4). A bridge evicted before use
// simply makes the follow-up git op fail, whose caller re-bridges or fails
// loud — there is no check-then-act race because reachability is always
// re-probed AFTER the fetch, never assumed from it.

// bridgeForkHead ensures the fork head sha is usable from the base serving
// copy. Same-repo heads (headRepo == baseRepo) need no bridge: both dirs are
// the same object set, so the pre-probe verdict stands as-is.
//
// Returns (pre, post): pre is reachability BEFORE any fetch — the only basis
// for publishing refs/pull/<num>/head (bucket-backed rule above); post is
// reachability after the bridge — the basis for running base-side git ops.
// A fetch failure returns the fetch error (fail closed — the caller maps it:
// open returns 503, diff falls back or 503s, merge paths fail loud).
func (s *Service) bridgeForkHead(ctx context.Context, baseRepo, headRepo, baseDir, headDir, headSHA string) (pre, post bool, err error) {
	ok, perr := s.Git.Reachable(ctx, baseDir, headSHA)
	if perr == nil && ok {
		return true, true, nil
	}
	if headRepo == baseRepo {
		return false, ok, perr
	}
	if ferr := s.Git.FetchInto(ctx, baseDir, headDir, headSHA); ferr != nil {
		return false, false, ferr
	}
	ok, perr = s.Git.Reachable(ctx, baseDir, headSHA)
	if perr != nil {
		return false, false, perr
	}
	return false, ok, nil
}
