// sync_test.go — sync levels, applyDelta replay (checkpoint fold + tail), the
// freshness conditional GET, and the monotonic revision guard (05 §5.0/§5.2).
package wal

import (
	"context"

	"git.packden.us/crueber/walhub/internal/config"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

type countingStore struct {
	store.ObjectStore
	condGETs int
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if opts.IfNoneMatch != "" {
		c.condGETs++
	}
	return c.ObjectStore.Get(ctx, key, opts)
}

func TestSync_LevelsAndFreshness(t *testing.T) {
	st := &countingStore{ObjectStore: store.NewMemory()}
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("e", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid), Synced: true}); err != nil {
		t.Fatal(err)
	}

	// A refs sync issues exactly one conditional GET (warm) — 13 §9.4 budget.
	before := st.condGETs
	g, err := h.Sync(ctx, LevelRefs)
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	if st.condGETs != before+1 {
		t.Fatalf("conditional GETs = %d, want %d", st.condGETs-before, 1)
	}

	// Freshness TTL suppresses the check entirely.
	cfg.WAL.FreshnessTTL = config.Duration(time.Hour)
	before = st.condGETs
	g, err = h.Sync(ctx, LevelRefs)
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	if st.condGETs != before {
		t.Fatalf("TTL-fresh sync issued %d conditional GETs, want 0", st.condGETs-before)
	}
}

func TestSync_MonotonicRevisionGuard(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, _ := r.Create(ctx, "acme/api", git.Sha1)
	oid := strings.Repeat("f", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid), Synced: true}); err != nil {
		t.Fatal(err)
	}
	held, _ := h.ManifestSnapshot()

	// Plant a STALE manifest (lower revision) in the store — a cached read
	// from after our local publish must be discarded (rule 5.0.4).
	stale := &proto.Manifest{
		FormatVersion: proto.WALFormatVersion,
		Repo:          "acme/api",
		ObjectFormat:  "sha1",
		Revision:      held.Revision - 1,
		HeadSeq:       held.HeadSeq - 1,
		Writer:        "ghost",
	}
	meta, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: stale.Marshal()},
		store.PutOptions{Mode: store.PutOverwrite})
	if err != nil {
		t.Fatal(err)
	}
	_ = meta

	h.freshAt = time.Time{} // force the freshness check
	if err := h.freshenManifest(ctx); err != nil {
		t.Fatalf("freshen: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Revision != held.Revision {
		t.Fatalf("guard failed: revision = %d, want held %d", m.Revision, held.Revision)
	}
}

func TestSync_CheckpointFoldAndColdStart(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := git.Sha1.ZeroHex()
	oidA, oidB := strings.Repeat("1", 40), strings.Repeat("2", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", zero, oidA)}); err != nil {
		t.Fatal(err)
	}
	if err := h.writeCheckpoint(ctx, nil, TriggerManual); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Checkpoint == nil || m.Checkpoint.Seq != 1 {
		t.Fatalf("checkpoint = %+v, want seq 1", m.Checkpoint)
	}
	if m.MinSeq != 2 {
		t.Fatalf("min_seq = %d, want 2", m.MinSeq)
	}
	// Post-checkpoint push lands on a new segment; refs still resolve.
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/dev", zero, oidB)}); err != nil {
		t.Fatal(err)
	}

	// Cold start: a fresh instance folds refs.pb then replays only the tail.
	r2 := NewRegistry(ctx, st, testConfig(t))
	defer r2.Close()
	h2, err := r2.Open(ctx, "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := h2.Layer().Snapshot(h2.Repo())
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := snap.Get("refs/heads/main"); !ok || e.Oid != oidA {
		t.Fatalf("folded main = %+v ok=%v", e, ok)
	}
	if e, ok := snap.Get("refs/heads/dev"); !ok || e.Oid != oidB {
		t.Fatalf("replayed dev = %+v ok=%v", e, ok)
	}
	if st2 := loadState(h2.Dir()); st2.AppliedSeq != 2 {
		t.Fatalf("applied_seq = %d, want 2", st2.AppliedSeq)
	}
}

func TestReadLogAndReplay(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, _ := r.Create(ctx, "acme/api", git.Sha1)
	zero := git.Sha1.ZeroHex()
	for i := 0; i < 4; i++ {
		oid := strings.Repeat(string(rune('a'+i)), 40)
		if _, err := h.Publish(ctx, PublishRequest{
			Txn: refTxn("refs/heads/b"+string(rune('0'+i)), zero, oid), Synced: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := h.ReadLog(ctx, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Seq != 2 || entries[1].Seq != 3 {
		t.Fatalf("readLog [2,3] = %d entries (%d..%d)", len(entries), firstSeqOf(entries), lastSeqOf(entries))
	}

	view, err := h.RefsAtSeq(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Refs) != 2 {
		t.Fatalf("refs at seq 2 = %d, want 2", len(view.Refs))
	}
	if _, ok := refGet(view.Refs, "refs/heads/b1"); !ok {
		t.Fatalf("refs at seq 2 missing b1: %v", view.Refs)
	}
	if _, ok := refGet(view.Refs, "refs/heads/b2"); ok {
		t.Fatalf("refs at seq 2 must not contain b2 (committed at seq 3)")
	}
}

func firstSeqOf(es []*proto.LogEntry) uint64 {
	if len(es) == 0 {
		return 0
	}
	return es[0].Seq
}

func lastSeqOf(es []*proto.LogEntry) uint64 {
	if len(es) == 0 {
		return 0
	}
	return es[len(es)-1].Seq
}

func refGet(refs []git.RefEntry, name string) (git.RefEntry, bool) {
	for _, r := range refs {
		if r.Name == name {
			return r, true
		}
	}
	return git.RefEntry{}, false
}
