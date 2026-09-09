// sizecatalog.go — the size-catalog fold wiring (Forgejo #248 §4).
//
// The sweep lives in internal/sizecatalog (pure derivation + store fold);
// this file is the maintainer-loop seam: one bounded fold per pass,
// best-effort, cursor-resumed across passes.
//
// ### Concurrency
// Hazard: the pass goroutine folding while a previous fold still runs
// (overlapping passes) or concurrent cursor updates racing.
// Avoidance: RunPass is single-threaded per Maintainer (one pass at a
// time); the cursor mutex guards only the in-memory resume token, never
// held across store I/O (copied out before Sweep, stored after).
package maintain

import (
	"context"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
)

// sizeSweepMaxRepos bounds one pass's fold (resumable via sizeCursor).
const sizeSweepMaxRepos = 256

// foldSizeCatalog runs one bounded size-catalog fold. Best-effort: errors
// are logged, never fail the pass (the catalog is optional/rebuildable).
func (m *Maintainer) foldSizeCatalog(ctx context.Context) {
	st := m.store()
	if st == nil {
		return
	}
	m.sizeMu.Lock()
	cursor := m.sizeCursor
	m.sizeMu.Unlock()
	// ListRepos comes from the engine's registration order (already
	// manifest-gated in production bind_wal wiring); foldOne re-probes the
	// manifest per repo and isolates failures, so stale names are skipped,
	// never fatal. This also keeps the fold independent of store LIST
	// support (law 4: probe, don't list — even the sweep probes per repo).
	repos := m.eng.Repos()
	res, err := sizecatalog.Sweep(ctx, st, sizecatalog.SweepOptions{
		MaxRepos: sizeSweepMaxRepos,
		Cursor:   cursor,
		ListRepos: func(context.Context) ([]string, error) {
			return append([]string{}, repos...), nil
		},
	})
	if err != nil {
		m.logf("size catalog sweep failed: %v", err)
		return
	}
	m.sizeMu.Lock()
	m.sizeCursor = res.NextCursor
	m.sizeMu.Unlock()
	if res.Folded > 0 || res.Failed > 0 {
		m.logf("size catalog sweep folded=%d unchanged=%d failed=%d", res.Folded, res.Unchanged, res.Failed)
	}
}
