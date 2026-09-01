// sync.go — the refs phase of the read path (doc 05 §5.2): checkpoint fold +
// parallel log-tail replay, applied as one offline packed-refs rewrite.
package wal

import (
	"bytes"
	"context"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// applyDelta brings the local refs (and pending-removal bookkeeping) from
// state.AppliedSeq to the manifest's head. Caller holds syncMu (§5.2 step 2).
func (h *RepoHandle) applyDelta(ctx context.Context) error {
	m := h.manifest
	if m == nil {
		return nil
	}

	// Checkpoint fold: write refs.pb straight into packed-refs.
	if m.Checkpoint != nil && m.Checkpoint.Seq > h.state.AppliedSeq {
		if err := h.foldCheckpoint(ctx, m); err != nil {
			return err
		}
	}

	// Replay the tail: all segments overlapping (applied_seq, head_seq].
	var segs []*proto.LogSegmentRef
	for _, s := range m.LogSegments {
		if s.LastSeq > h.state.AppliedSeq && s.FirstSeq <= m.HeadSeq {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		if h.state.AppliedSeq < m.HeadSeq {
			// Nothing to replay but the state lags (the checkpoint fold
			// already covered it): persist the catch-up point.
			return h.updateState(func(st *RepoState) {
				st.AppliedSeq = m.HeadSeq
				st.Revision = m.Revision
			})
		}
		return nil
	}

	entries, err := h.fetchSegments(ctx, segs)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })

	if err := h.replayEntries(ctx, entries); err != nil {
		return err
	}
	return h.updateState(func(st *RepoState) {
		st.AppliedSeq = m.HeadSeq
		st.Revision = m.Revision
	})
}

// foldCheckpoint GETs refs.pb at the checkpoint key and writes it directly
// into packed-refs (head_target applied to HEAD, peeled entries as ^lines).
func (h *RepoHandle) foldCheckpoint(ctx context.Context, m *pbManifest) error {
	// The RefSnapshot lives at checkpoints/<seq>/refs.pb (checkpoint.Key
	// points at checkpoint.pb — the wrong object for a refs fold).
	key := h.repoKey(store.CheckpointRefsKey(m.Checkpoint.Seq))
	body, _, err := store.GetBytes(ctx, h.reg.st, key, store.GetOptions{})
	if err != nil {
		return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
	}
	if body == nil {
		return &WalError{Kind: WalErrCorrupt, Detail: "checkpoint refs absent: " + key}
	}
	snap := &proto.RefSnapshot{}
	if err := snap.Unmarshal(body); err != nil {
		return &WalError{Kind: WalErrCorrupt, Detail: "refs snapshot", Wrapped: err}
	}
	if err != nil {
		return &WalError{Kind: WalErrCorrupt, Detail: key, Wrapped: err}
	}
	refs := make([]git.RefEntry, 0, len(snap.Refs))
	for _, r := range snap.Refs {
		refs = append(refs, git.RefEntry{Name: r.Name, Oid: r.Oid, Peeled: r.Peeled})
	}
	if err := h.Layer().LoadSnapshot(h.repo, refs, snap.HeadTarget, ""); err != nil {
		return &WalError{Kind: WalErrGit, Detail: "checkpoint fold", Wrapped: err}
	}
	if m.Checkpoint.CreatedAt != nil {
		h.noteCheckpoint(m.Checkpoint.Seq, m.Checkpoint.CreatedAt.Go())
	}
	return h.updateState(func(st *RepoState) {
		st.AppliedSeq = m.Checkpoint.Seq
	})
}

// fetchSegments GETs all segment objects in parallel — chunks of 16 goroutines,
// each segment GET a goroutine (§5.2 step 2, 13 §4). Decode is pure: segments
// are immutable objects and no lock is held during decode.
func (h *RepoHandle) fetchSegments(ctx context.Context, segs []*proto.LogSegmentRef) ([]*proto.LogEntry, error) {
	results := make([][]*proto.LogEntry, len(segs))
	errs := make([]error, len(segs))
	g, gctx := store.WithContext(ctx)
	g.SetLimit(16)
	for i, s := range segs {
		i, s := i, s
		g.Go(func() error {
			body, _, err := store.GetBytes(gctx, h.reg.st, h.repoKey(s.Key), store.GetOptions{})
			if err != nil {
				errs[i] = &WalError{Kind: WalErrStore, Detail: s.Key, Wrapped: err}
				return errs[i]
			}
			if body == nil {
				errs[i] = &WalError{Kind: WalErrCorrupt, Detail: "log segment absent: " + s.Key}
				return errs[i]
			}
			// Partial-tail tolerance: stop at the first incomplete trailing
			// frame (a growing segment read mid-append); never an error.
			var entries []*proto.LogEntry
			if _, err := proto.DecodeEntries(bytes.NewReader(body), func(e *proto.LogEntry) error {
				entries = append(entries, e)
				return nil
			}); err != nil {
				errs[i] = &WalError{Kind: WalErrCorrupt, Detail: s.Key, Wrapped: err}
				return errs[i]
			}
			results[i] = entries
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var out []*proto.LogEntry
	for _, es := range results {
		out = append(out, es...)
	}
	return out, nil
}

// replayEntries applies decoded entries in seq order (§5.2 step 2 bullet list).
// Caller holds syncMu.
func (h *RepoHandle) replayEntries(ctx context.Context, entries []*proto.LogEntry) error {
	var txns []*proto.RefTransaction
	for _, e := range entries {
		if e.CreatedAt != nil {
			t := e.CreatedAt.Go()
			if h.firstEntryTime.IsZero() || t.Before(h.firstEntryTime) {
				h.firstEntryTime = t
			}
			if t.After(h.lastEntryTime) {
				h.lastEntryTime = t
			}
		}
		switch e.Kind {
		case proto.EntryKindPush, proto.EntryKindRefUpdate:
			if e.Txn != nil {
				txns = append(txns, e.Txn)
			}
		case proto.EntryKindCompact:
			if len(e.Supersedes) > 0 {
				sup := e.Supersedes
				h.updateState(func(st *RepoState) {
					st.PendingPackRemovals = append(st.PendingPackRemovals, sup...)
				})
			}
		case proto.EntryKindCheckpoint:
			if e.Checkpoint != nil && e.Checkpoint.CreatedAt != nil {
				h.noteCheckpoint(e.Checkpoint.Seq, e.Checkpoint.CreatedAt.Go())
			}
		case proto.EntryKindSettings:
			// No ref effect; settings ride the manifest.
		}
	}

	if len(txns) == 0 {
		return nil
	}
	// One offline ref apply for ALL txns: full packed-refs rewrite (no git
	// process, works before packs exist). Atomicity: build the new refs map
	// in memory from the current packed-refs parse, then tmp+rename.
	snap, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		return &WalError{Kind: WalErrGit, Detail: "snapshot before replay", Wrapped: err}
	}
	refs := make([]git.RefEntry, len(snap.Refs))
	copy(refs, snap.Refs)
	headT := snap.HeadTarget
	for _, txn := range txns {
		refs = git.ApplyRefTxnsOffline(refs, gitUpdates(txn.Updates))
		for _, u := range txn.Updates {
			if u.Name == "HEAD" && u.NewSymbolicTarget != "" {
				headT = u.NewSymbolicTarget
			}
		}
	}
	sortRefs(refs)
	if err := h.Layer().LoadSnapshot(h.repo, refs, headT, ""); err != nil {
		return &WalError{Kind: WalErrGit, Detail: "replay packed-refs rewrite", Wrapped: err}
	}
	return nil
}

func sortRefs(refs []git.RefEntry) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
}

// repoKey resolves a repo-relative key against the repo's store prefix.
func (h *RepoHandle) repoKey(rel string) string {
	return repoPrefix(h.ID) + rel
}

func repoPrefix(id string) string {
	owner, name, _ := cut2(id, "/")
	return "repos/" + owner + "/" + name + "/"
}

func cut2(s, sep string) (string, string, bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}
