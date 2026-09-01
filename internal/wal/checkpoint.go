// checkpoint.go — checkpoints (doc 05 §5.5): trigger math, the two-object
// create-only round, the CAS manifest trim with provenance (first_state_at/as_of).
package wal

import (
	"context"
	"fmt"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// checkpointTrigger evaluates the §5.5 triggers (any; 0 disables):
// entries ≥ snapshot_every_entries; tail bytes > checkpoint_tail_bytes;
// age since checkpoint.created_at (or updated_at) ≥ checkpoint_interval.
func checkpointTrigger(cfg *configVals, m *pbManifest, firstEntry, lastEntry time.Time) (CheckpointTrigger, bool) {
	cpSeq := uint64(0)
	var cpAt time.Time
	if m.Checkpoint != nil {
		cpSeq = m.Checkpoint.Seq
		if m.Checkpoint.CreatedAt != nil {
			cpAt = m.Checkpoint.CreatedAt.Go()
		}
	}
	ageBase := cpAt
	if ageBase.IsZero() && m.UpdatedAt != nil {
		ageBase = m.UpdatedAt.Go()
	}

	if cfg.snapshotEveryEntries > 0 && m.HeadSeq-cpSeq >= cfg.snapshotEveryEntries {
		return TriggerEntries, true
	}
	if cfg.checkpointTailBytes > 0 {
		var tail uint64
		for _, s := range m.LogSegments {
			if s.LastSeq > cpSeq {
				tail += s.Size
			}
		}
		if tail > cfg.checkpointTailBytes {
			return TriggerTailBytes, true
		}
	}
	if cfg.checkpointInterval > 0 && !ageBase.IsZero() && time.Since(ageBase) >= cfg.checkpointInterval {
		return TriggerAge, true
	}
	return "", false
}

// writeCheckpoint performs the two-object checkpoint write (§5.5).
// Refs-level only: works on an instance that could never hold the packs.
// Idempotent: cp.seq == head_seq → the existing checkpoint is returned.
func (h *RepoHandle) writeCheckpoint(ctx context.Context, t *Task, trig CheckpointTrigger) error {
	// Freshness + snapshot of the manifest (refs-level sync semantics).
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		return err
	}
	if err := h.freshenManifest(ctx); err != nil {
		h.syncMu.Unlock()
		return err
	}
	m := h.manifest
	h.syncMu.Unlock()

	if m.Checkpoint != nil && m.Checkpoint.Seq == m.HeadSeq {
		return nil // already checkpointed at head
	}
	seq := m.HeadSeq
	now := time.Now().UTC()

	// Snapshot = local refs (sorted, peeled). Requires a sync first so the
	// local view is at head; the caller's task ctx bounds the work.
	snap, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		return &WalError{Kind: WalErrGit, Detail: "checkpoint snapshot", Wrapped: err}
	}
	if h.state.AppliedSeq < m.HeadSeq {
		// Local view lags the manifest: bring refs up before snapshotting.
		if err := h.catchUp(ctx); err != nil {
			return err
		}
		snap, err = h.Layer().Snapshot(h.repo)
		if err != nil {
			return &WalError{Kind: WalErrGit, Detail: "checkpoint snapshot", Wrapped: err}
		}
	}

	pbSnap := &proto.RefSnapshot{
		Seq:          seq,
		ObjectFormat: m.ObjectFormat,
		HeadTarget:   snap.HeadTarget,
		CreatedAt:    TsPtr(now),
	}
	for _, r := range snap.Refs {
		pbSnap.Refs = append(pbSnap.Refs, &proto.Ref{Name: r.Name, Oid: r.Oid, Peeled: r.Peeled})
	}

	// Provenance (§5.5): first_state_at = previous.first_state_at →
	// first_entry_time → first_seq_published_at → previous.created_at;
	// as_of = last_entry_time → previous.as_of → created_at.
	prevFirst, prevAsOf, prevCreated := provenanceOf(m)
	firstStateAt := prevFirst
	if firstStateAt.IsZero() {
		firstStateAt = h.firstEntryTime
	}
	if firstStateAt.IsZero() {
		firstStateAt = prevCreated
	}
	if firstStateAt.IsZero() {
		firstStateAt = now
	}
	asOf := lastEntryOf(h, m, h.lastEntryTime)
	if asOf.IsZero() {
		asOf = prevAsOf
	}
	if asOf.IsZero() {
		asOf = now
	}
	cp := &proto.Checkpoint{
		Seq:          seq,
		ObjectFormat: m.ObjectFormat,
		Packs:        m.Packs, // full live pack set with side-file flags
		RefsKey:      store.CheckpointRefsKey(seq),
		RefCount:     uint64(len(pbSnap.Refs)),
		CreatedAt:    TsPtr(now),
		Writer:       h.reg.instance,
	}

	if t != nil {
		t.Notice(fmt.Sprintf("checkpoint at seq %d (%s): %d refs, %d packs", seq, trig, cp.RefCount, len(m.Packs)))
	}

	// Round 1: two create-only PUTs in parallel — same content, keyed by seq,
	// so racing instances are benign (a crash leaves garbage, never a hazard).
	ckKey := h.repoKey(store.CheckpointKey(seq))
	rfKey := h.repoKey(store.CheckpointRefsKey(seq))
	g, gctx := store.WithContext(ctx)
	g.Go(func() error {
		_, err := h.reg.st.Put(gctx, ckKey, store.PutBody{Bytes: cp.Marshal()},
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf", Immutable: true})
		return err
	})
	g.Go(func() error {
		_, err := h.reg.st.Put(gctx, rfKey, store.PutBody{Bytes: pbSnap.Marshal()},
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf", Immutable: true})
		return err
	})
	if err := g.Wait(); err != nil {
		return &WalError{Kind: WalErrStore, Detail: "checkpoint round 1", Wrapped: err}
	}

	// Round 2: CAS manifest (checkpoint, min_seq, trim folded segments).
	ref := &proto.CheckpointRef{
		Seq:          seq,
		Key:          store.CheckpointKey(seq),
		CreatedAt:    TsPtr(now),
		FirstStateAt: TsPtr(firstStateAt),
		AsOf:         TsPtr(asOf),
	}
	for attempt := 0; attempt < 16; attempt++ {
		if attempt > 0 {
			publishCASRetries.Add(1)
		}
		h.syncMu.Lock()
		base, version := h.manifest, h.version
		h.syncMu.Unlock()
		if base.Checkpoint != nil && base.Checkpoint.Seq >= seq {
			return nil // a racing checkpoint won; ours is garbage at worst
		}
		next := trimManifest(base, ref, seq, h.reg.instance)
		key := manifestKey(h.ID)
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version), ContentType: "application/x-protobuf"}
		if version == "" {
			opts.Mode = store.PutCreate
		}
		meta, err := h.reg.st.Put(ctx, key, store.PutBody{Bytes: next.Marshal()}, opts)
		if err == nil {
			h.syncMu.Lock()
			h.manifest = next
			h.version = string(meta.Version)
			h.heldRev = next.Revision
			h.freshAt = time.Now()
			h.noteCheckpoint(seq, now)
			h.syncMu.Unlock()
			if t != nil {
				t.Notice("checkpoint committed at revision " + itoa(next.Revision))
			}
			return nil
		}
		if !store.IsPreconditionFailed(err) {
			return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
		}
		// 412: another writer checkpointed/committed — refresh and re-decide.
		if err := h.freshenManifest(ctx); err != nil {
			return err
		}
	}
	return &WalError{Kind: WalErrRetry, Detail: "checkpoint CAS retries exhausted"}
}

func provenanceOf(m *pbManifest) (first, asOf, created time.Time) {
	if m.Checkpoint != nil {
		if m.Checkpoint.FirstStateAt != nil {
			first = m.Checkpoint.FirstStateAt.Go()
		}
		if m.Checkpoint.AsOf != nil {
			asOf = m.Checkpoint.AsOf.Go()
		}
		if m.Checkpoint.CreatedAt != nil {
			created = m.Checkpoint.CreatedAt.Go()
		}
	}
	return
}

func lastEntryOf(h *RepoHandle, m *pbManifest, lastEntry time.Time) time.Time {
	if !lastEntry.IsZero() {
		return lastEntry
	}
	if m.UpdatedAt != nil {
		return m.UpdatedAt.Go()
	}
	return time.Time{}
}

// trimManifest folds the checkpoint into a copy: min_seq = seq+1, segments
// entirely below min_seq trimmed, revision+1 (§5.5 round 2).
func trimManifest(base *pbManifest, cp *proto.CheckpointRef, seq uint64, writer string) *pbManifest {
	next := *base
	next.Checkpoint = cp
	next.MinSeq = seq + 1
	segs := make([]*proto.LogSegmentRef, 0, len(base.LogSegments))
	for _, s := range base.LogSegments {
		if s.LastSeq >= next.MinSeq { // entirely below min_seq → folded away
			segs = append(segs, s)
		}
	}
	next.LogSegments = segs
	next.UpdatedAt = TsPtr(time.Now().UTC())
	next.Writer = writer
	next.Revision = base.Revision + 1
	return &next
}

func itoa(v any) string { return fmt.Sprintf("%d", v) }
