// activity.go — Forgejo #247 cold-derivation resolver (R1 B5).
//
// The sweep folds sizes from the manifest alone, but activity (HEAD-tip sha
// + commit date) needs git: the tip from the refs view, the date from the
// commit object. This file resolves one repo's activity for the sweep's
// Activity hook: refs view (store reads, no packs) → tip → git read (local
// objects) → serve-sync retry (materialize, then re-read) → give up.
//
// Budget per repo needing derivation (documented in 10_maintenance.md §4):
// refs-view reads (log segments/checkpoint, no packs) + at most one
// serve-sync (one-time per host — Sync is incremental) + one git subprocess.
// Repos whose sidecar is already fresh cost zero git (the sweep skips the
// hook — see sizecatalog.foldOne). Every failure isolates to the repo
// (found=false): the sweep preserves existing activity and continues.
//
// Enumeration source: the maintain engine's local repo list (m.eng.Repos,
// the same source as the size fold — zero idle cost, no bucket LIST).
// Single-host zero-config converges fully (serve opens repos on demand;
// pushes write activity directly); multi-role fleets converge per host as
// repos are served. A fleet-wide bucket-LIST enumeration is an explicit
// non-goal for V1 (off-hot-path-legal but recurring LIST cost per pass).
//
// ### Concurrency
// Hazard: the resolver running inside the sweep's parallel fold while a
// serve-sync mutates the shared local cache.
// Avoidance: Sync levels are per-repo serialized by the WAL handle (the
// handle's own syncMu ladder); the resolver holds no maintain lock across
// any of it (it takes none at all — eng.Open hands out the handle).
package maintain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
)

// resolveActivity cold-derives one repo's HEAD-tip activity for the sweep.
// found=false on ANY failure (unknown repo, unborn HEAD, missing objects,
// git failure): the caller preserves and continues, never failing the pass.
func resolveActivity(ctx context.Context, eng Engine, id string) (tipSHA string, commitTime time.Time, found bool) {
	repo, err := eng.Open(ctx, id)
	if err != nil {
		return "", time.Time{}, false
	}
	var head uint64
	if m, _ := repo.Manifest(); m != nil {
		head = m.HeadSeq
	}
	rv, err := repo.RefsAtSeq(ctx, head)
	if err != nil || rv == nil {
		return "", time.Time{}, false
	}
	// The head target rides the WAL view only after a checkpoint or a HEAD
	// move (logreader tracks HEAD symbolic updates; checkpoints snapshot the
	// local HEAD file). Before either, fall back to the serving copy's HEAD
	// symref (seeded refs/heads/main at init — the same file read the
	// mirror/import packages use). The TIP always comes from the WAL view
	// (authoritative; the local packed-refs may lag on this host).
	target := rv.HeadTarget
	if target == "" {
		target = readHeadSymref(repo.Dir())
	}
	if target == "" {
		return "", time.Time{}, false
	}
	var tip string
	for _, e := range rv.Refs {
		if e.Name == target {
			tip = e.Oid
			break
		}
	}
	if tip == "" || isZeroOid(tip) || !git.ValidOid(tip) {
		return "", time.Time{}, false
	}
	local := repo.Local()
	if local == nil {
		return "", time.Time{}, false
	}
	ops := repo.GitOps()
	if ops == nil {
		return "", time.Time{}, false
	}
	committer, author, err := ops.CommitDates(ctx, local, tip)
	if err != nil {
		// Objects may simply not be local yet: materialize once, re-read
		// once. Still failing → preserve (a corrupt/unservable repo is
		// fsck territory, not sweep territory).
		if serr := repo.SyncServe(ctx); serr != nil {
			return "", time.Time{}, false
		}
		if committer, author, err = ops.CommitDates(ctx, local, tip); err != nil {
			return "", time.Time{}, false
		}
	}
	ct, ok := git.PickCommitTime(committer, author)
	if !ok {
		return "", time.Time{}, false
	}
	return tip, ct, true
}

// isZeroOid reports an all-zero oid of either object format (a delete).
func isZeroOid(s string) bool {
	if s == "" {
		return true
	}
	return strings.Trim(s, "0") == ""
}

// readHeadSymref reads a bare repo dir's HEAD symref ("ref: <target>"); ""
// when detached, missing, or unreadable (the mirror.HeadTarget /
// repoimport.HeadTarget shape — a file read, never a subprocess).
func readHeadSymref(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	t, ok := strings.CutPrefix(s, "ref: ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(t)
}
