package api

import (
	"context"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// health.go — the issue-#209 self-heal classifiers shared by the summary and
// overview handlers: the empty/healthy fold, the cached-fsck.pb degraded
// override, and the read-side stall derivation.
//
// ### Concurrency
// Hazard: none of this state is shared — the classifiers are pure functions
// of per-request data (an in-hand snapshot, one conditional store GET), and
// the probe issues no lock, takes no lease, and runs no subprocess. In
// particular no request goroutine ever runs `git fsck`: audits happen only
// in the maintainer loop (existing KindFsck unit) and on-demand via the
// already-addressable POST …/ops/fsck ((repo,kind) single-flight join).
// Avoidance: by construction — there is nothing here to serialize.

// classifyHealth folds the summary repo-state vocabulary from ref data the
// summary path already holds (07_api.md §9.1): "empty" when no head resolves
// and zero branches/tags exist, else "healthy". "degraded" never comes from
// here — only the fsck.pb probe (probeFsck + fsckHasMissing) promotes to it.
func classifyHealth(s SummaryData) string {
	if s.Head == nil && s.Branches == 0 && s.Tags == 0 {
		return RepoHealthEmpty
	}
	return RepoHealthHealthy
}

// probeFsck reads the cached fsck.pb report for one repo (exact-key probe —
// never a LIST, law 4). It reports (nil, false) on absence AND on any error:
// a missing/unreadable audit is not damage, it is "never audited". Callers
// branch on data in hand first (empty repos skip the probe: +0 round trips).
func probeFsck(ctx context.Context, st store.ObjectStore, id git.RepoId) (*proto.FsckReport, bool) {
	if st == nil {
		return nil, false
	}
	body, _, err := store.GetBytes(ctx, st, id.StorePrefix()+store.Fsck, store.GetOptions{})
	if err != nil || body == nil {
		return nil, false // absent (or unreadable) = never audited
	}
	rep := &proto.FsckReport{}
	if err := rep.Unmarshal(body); err != nil {
		return nil, false
	}
	return rep, true
}

// fsckHasMissing reports whether the report records missing objects: either
// the authoritative total or the bounded sample (a report with a total but
// an empty sample still defers repair — maintain/wave4b_test.go pins that).
func fsckHasMissing(rep *proto.FsckReport) bool {
	return rep != nil && (rep.MissingTotal > 0 || len(rep.Missing) > 0 || rep.Problems > 0)
}

// fsckMissingTotal is the authoritative missing count: the stored total,
// falling back to the bounded sample length when only the sample survived.
func fsckMissingTotal(rep *proto.FsckReport) uint64 {
	if rep == nil {
		return 0
	}
	if rep.MissingTotal > 0 {
		return rep.MissingTotal
	}
	return uint64(len(rep.Missing))
}

// effectiveUpstreamGit resolves the repair fetch source for the overview
// projection: the per-repo [upstream] settings (D24) merged over the host
// config, falling back to the host value on any parse/merge failure (an
// unreadable settings doc must never hide the host upstream, and must never
// fail the overview). "" means no repair source is configured.
func effectiveUpstreamGit(host *config.Config, doc SettingsDoc) string {
	hostGit := ""
	if host != nil {
		hostGit = host.Upstream.Git
	}
	if doc.TOML == "" || host == nil {
		return hostGit
	}
	rs, err := config.ParseRepoSettings([]byte(doc.TOML))
	if err != nil {
		return hostGit
	}
	merged, err := rs.Merge(host)
	if err != nil {
		return hostGit
	}
	return merged.Upstream.Git
}
