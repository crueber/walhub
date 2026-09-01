// logreader.go — point-in-time replay (doc 05 §5.6): readLog (sequential,
// read-only) and refsAtSeq/refsAsOf (pure in-memory fold from the newest
// usable checkpoint).
package wal

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// RefsView is the result of a point-in-time refs fold.
type RefsView struct {
	Seq        uint64
	Refs       []git.RefEntry // name-sorted
	HeadTarget string
}

// readLog returns the committed entries in [from, to] (to == 0 → head),
// sorted by seq. Never mutates anything; segments are fetched SEQUENTIALLY
// (read-only path, not latency-critical, §5.6).
func (h *RepoHandle) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		return nil, err
	}
	if err := h.freshenManifest(ctx); err != nil {
		h.syncMu.Unlock()
		return nil, err
	}
	m := h.manifest
	h.syncMu.Unlock()

	if to == 0 {
		to = m.HeadSeq
	}
	var segs []*proto.LogSegmentRef
	for _, s := range m.LogSegments {
		if s.LastSeq >= from && s.FirstSeq <= to {
			segs = append(segs, s)
		}
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].FirstSeq < segs[j].FirstSeq })

	var out []*proto.LogEntry
	for _, s := range segs {
		body, _, err := store.GetBytes(ctx, h.reg.st, h.repoKey(s.Key), store.GetOptions{})
		if err != nil {
			return nil, &WalError{Kind: WalErrStore, Detail: s.Key, Wrapped: err}
		}
		if body == nil {
			return nil, &WalError{Kind: WalErrCorrupt, Detail: "log segment absent: " + s.Key}
		}
		_, _ = proto.DecodeEntries(bytes.NewReader(body), func(e *proto.LogEntry) error {
			if e.Seq >= from && e.Seq <= to {
				out = append(out, e)
			}
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// RefsAtSeq folds the WAL up to seq (§5.6): newest checkpoint with
// cp.seq ≤ seq, then entries in order until the cut.
func (h *RepoHandle) RefsAtSeq(ctx context.Context, seq uint64) (*RefsView, error) {
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		return nil, err
	}
	if err := h.freshenManifest(ctx); err != nil {
		h.syncMu.Unlock()
		return nil, err
	}
	m := h.manifest
	h.syncMu.Unlock()

	if seq < m.MinSeq {
		var cpSeq uint64
		if m.Checkpoint != nil {
			cpSeq = m.Checkpoint.Seq
		}
		if seq < cpSeq {
			return nil, &WalError{Kind: WalErrInvalid,
				Detail: fmt.Sprintf("refs at seq %d are not replayable (min_seq %d, checkpoint seq %d)", seq, m.MinSeq, cpSeq)}
		}
	}
	if seq > m.HeadSeq {
		return nil, &WalError{Kind: WalErrInvalid, Detail: fmt.Sprintf("seq %d is beyond head %d", seq, m.HeadSeq)}
	}

	var view *RefsView
	var haveSnapshot bool
	if m.Checkpoint != nil && m.Checkpoint.Seq <= seq {
		snap, err := h.checkpointRefs(ctx, m.Checkpoint)
		if err != nil {
			return nil, err
		}
		view = snapshotRefsView(snap)
		haveSnapshot = true
	} else if seq >= m.MinSeq {
		view = &RefsView{}
		haveSnapshot = true
	}
	if !haveSnapshot {
		return nil, &WalError{Kind: WalErrInvalid,
			Detail: fmt.Sprintf("refs at seq %d are not replayable", seq)}
	}
	return h.foldTo(ctx, m, view, seq, time.Time{})
}

// RefsAsOf folds the WAL to the newest state whose entries all carry
// created_at ≤ t (§5.6: reads to head, breaks by created_at; a missing
// created_at never breaks the walk).
func (h *RepoHandle) RefsAsOf(ctx context.Context, t time.Time) (*RefsView, error) {
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		return nil, err
	}
	if err := h.freshenManifest(ctx); err != nil {
		h.syncMu.Unlock()
		return nil, err
	}
	m := h.manifest
	h.syncMu.Unlock()

	var view *RefsView
	if m.Checkpoint != nil && m.Checkpoint.AsOf != nil && !m.Checkpoint.AsOf.Go().After(t) {
		snap, err := h.checkpointRefs(ctx, m.Checkpoint)
		if err != nil {
			return nil, err
		}
		view = snapshotRefsView(snap)
	} else {
		view = &RefsView{}
	}
	return h.foldTo(ctx, m, view, m.HeadSeq, t)
}

// foldTo applies entries in order until the cut. seqCut > 0: seq-bounded;
// timeCut non-zero: created_at-bounded.
func (h *RepoHandle) foldTo(ctx context.Context, m *pbManifest, view *RefsView, seqCut uint64, timeCut time.Time) (*RefsView, error) {
	from := view.Seq + 1
	entries, err := h.ReadLog(ctx, from, seqCut)
	if err != nil {
		return nil, err
	}
	refs := make([]git.RefEntry, len(view.Refs))
	copy(refs, view.Refs)
	for _, e := range entries {
		if !timeCut.IsZero() {
			if e.CreatedAt == nil {
				continue // missing created_at never breaks the walk
			}
			if e.CreatedAt.Go().After(timeCut) {
				break
			}
		}
		if e.Txn == nil {
			continue
		}
		refs = git.ApplyRefTxnsOffline(refs, gitUpdates(e.Txn.Updates))
		for _, u := range e.Txn.Updates {
			if u.Name == "HEAD" && u.NewSymbolicTarget != "" {
				view.HeadTarget = u.NewSymbolicTarget
			}
		}
		view.Seq = e.Seq
	}
	sortRefs(refs)
	view.Refs = refs
	return view, nil
}

// checkpointRefs GETs and converts a checkpoint's refs.pb snapshot.
func (h *RepoHandle) checkpointRefs(ctx context.Context, cp *proto.CheckpointRef) (*proto.RefSnapshot, error) {
	key := cp.Key
	if key == "" {
		key = store.CheckpointRefsKey(cp.Seq)
	}
	body, _, err := store.GetBytes(ctx, h.reg.st, h.repoKey(key), store.GetOptions{})
	if err != nil {
		return nil, &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
	}
	if body == nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: "checkpoint refs absent: " + key}
	}
	snap := &proto.RefSnapshot{}
	if err := snap.Unmarshal(body); err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: "refs snapshot", Wrapped: err}
	}
	if err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: key, Wrapped: err}
	}
	return snap, nil
}

// snapshotRefsView converts a proto snapshot to a RefsView.
func snapshotRefsView(s *proto.RefSnapshot) *RefsView {
	v := &RefsView{Seq: s.Seq, HeadTarget: s.HeadTarget}
	for _, r := range s.Refs {
		v.Refs = append(v.Refs, git.RefEntry{Name: r.Name, Oid: r.Oid, Peeled: r.Peeled})
	}
	return v
}
