package main

import (
	"context"
	"encoding/json"
	"fmt"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// This file owns the issue-#424 fork composition: the manifest-sharing
// executor's refs seam over the WAL registry, and the summary fork
// projection over the collaboration objects. No new product surface:
// pure wiring behind the pulls/api seams (law 8).

// forkRefsReader supplies the fork executor's live parent refs: open the
// WAL handle and sync to refs level (the same path any reader takes),
// snapshot the ref set, release the guard before returning (13 §2 rule 4
// — objects stay materialized on disk, no lock crosses the snapshot).
type forkRefsReader struct {
	reg *wal.Registry
}

func (f *forkRefsReader) ParentRefs(ctx context.Context, parent string) ([]pulls.ForkRef, string, error) {
	h, err := f.reg.Open(ctx, parent)
	if err != nil {
		return nil, "", fmt.Errorf("fork parent %s: %w", parent, err)
	}
	g, serr := h.Sync(ctx, wal.LevelRefs)
	if serr != nil {
		return nil, "", fmt.Errorf("fork parent %s: %w", parent, serr)
	}
	g.Release()
	snap, serr := h.Layer().Snapshot(h.Repo())
	if serr != nil {
		return nil, "", fmt.Errorf("fork parent %s: %w", parent, serr)
	}
	out := make([]pulls.ForkRef, 0, len(snap.Refs))
	for _, r := range snap.Refs {
		out = append(out, pulls.ForkRef{Name: r.Name, Oid: string(r.Oid), Peeled: string(r.Peeled)})
	}
	return out, snap.HeadTarget, nil
}

// forkSummaryOf reads the summary fork projection index-first (issue
// #424): the fork.json parent pointer plus the meta/forks.json
// count/version. Both absent → ok=false (fields omitted, ETag
// byte-identical). Any store error fails open to absent — display
// metadata must never fail the summary.
func forkSummaryOf(ctx context.Context, st store.ObjectStore, owner, repo string) (api.ForkSummary, bool) {
	if st == nil {
		return api.ForkSummary{}, false
	}
	sum := api.ForkSummary{}
	seen := false
	if raw, _, err := store.GetBytes(ctx, st, pulls.ForkKey(owner, repo), store.GetOptions{}); err == nil && raw != nil {
		var doc struct {
			Parent string `json:"parent"`
		}
		if jerr := json.Unmarshal(raw, &doc); jerr == nil && doc.Parent != "" {
			sum.Parent = doc.Parent
			seen = true
		}
	}
	if raw, _, err := store.GetBytes(ctx, st, pulls.ForksKey(owner, repo), store.GetOptions{}); err == nil && raw != nil {
		var fx struct {
			Version int `json:"version"`
			Forks   []struct {
				Repo string `json:"repo"`
			} `json:"forks"`
		}
		if jerr := json.Unmarshal(raw, &fx); jerr == nil {
			sum.Count = len(fx.Forks)
			sum.Version = fx.Version
			seen = true
		}
	}
	if !seen {
		return api.ForkSummary{}, false
	}
	return sum, true
}
