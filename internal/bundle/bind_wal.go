package bundle

// bind_wal.go — the real binding of WalView to internal/wal (§8.2 seam).
// The core package only knows the narrow WalView interface (resolve.go);
// everything wal-shaped lives here.

import (
	"context"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/wal"
)

// WalAdapter adapts a wal.Registry to WalView by opening repo handles.
type WalAdapter struct {
	R *wal.Registry
}

var _ WalView = (*WalAdapter)(nil)

// RefsAsOf folds the WAL to the highest seq whose created_at ≤ at (§8.4).
func (a *WalAdapter) RefsAsOf(ctx context.Context, repo string, at time.Time) (Refs, uint64, error) {
	h, err := a.R.Open(ctx, repo)
	if err != nil {
		return nil, 0, err
	}
	v, err := h.RefsAsOf(ctx, at)
	if err != nil {
		return nil, 0, err
	}
	return refsFromView(v), v.Seq, nil
}

// RefsAtSeq folds to exactly a seq (§8.4; the compose path of §8.9.4).
func (a *WalAdapter) RefsAtSeq(ctx context.Context, repo string, seq uint64) (Refs, error) {
	h, err := a.R.Open(ctx, repo)
	if err != nil {
		return nil, err
	}
	v, err := h.RefsAtSeq(ctx, seq)
	if err != nil {
		return nil, err
	}
	return refsFromView(v), nil
}

// FirstStateAt reports when repo state begins; earlier slots are unavailable
// (§8.4: never built, never recorded, never backfilled).
func (a *WalAdapter) FirstStateAt(repo string) (time.Time, bool) {
	h := a.R.Get(repo)
	if h == nil {
		return time.Time{}, false
	}
	m, _ := h.ManifestSnapshot()
	if m == nil {
		return time.Time{}, false
	}
	if m.Checkpoint != nil && m.Checkpoint.FirstStateAt != nil {
		return m.Checkpoint.FirstStateAt.Go(), true
	}
	// TODO-INTEGRATION: with no checkpoint yet the first state is the
	// earliest log entry's created_at; wal does not expose it. Until it
	// does, no floor is claimed (every slot is considered available).
	return time.Time{}, false
}

// refsFromView converts a wal fold view to the Refs seam type (name-sorted).
func refsFromView(v *wal.RefsView) Refs {
	refs := make(Refs, 0, len(v.Refs))
	for _, r := range v.Refs {
		refs = append(refs, protoRefOf(r))
	}
	SortRefs(refs)
	return refs
}

func protoRefOf(r git.RefEntry) protoRef {
	return protoRef{Name: r.Name, Oid: string(r.Oid), Peeled: string(r.Peeled)}
}
