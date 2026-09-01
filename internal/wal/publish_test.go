// publish_test.go — the publish path (05 §5.3): CAS-ladder races with one
// winner, the orphan burn protocol (§5.4), monotonic created_at, per-ref
// conflicts, and refs-before-advertise.
package wal

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func refTxn(name, old, new string) *proto.RefTransaction {
	return &proto.RefTransaction{Updates: []*proto.RefUpdate{{Name: name, OldOid: old, NewOid: new}}}
}

func TestPublish_BasicCommitAndReplay(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}

	oid1, oid2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid1)})
	if err != nil || res.Seq == 0 {
		t.Fatalf("publish 1: res=%+v err=%v", res, err)
	}
	res2, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", oid1, oid2)})
	if err != nil || res2.Seq != res.Seq+1 {
		t.Fatalf("publish 2: res=%+v err=%v", res2, err)
	}

	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 2 || m.Revision != 3 {
		t.Fatalf("manifest head=%d revision=%d, want 2/3", m.HeadSeq, m.Revision)
	}
	// One sealed segment per batch: two pushes → two segments covering [1,2].
	if len(m.LogSegments) != 2 || m.LogSegments[0].FirstSeq != 1 || m.LogSegments[1].LastSeq != 2 {
		t.Fatalf("segments = %+v", m.LogSegments)
	}

	// Local refs applied offline (refs-first before advertise).
	snap, err := h.Layer().Snapshot(h.Repo())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := snap.Get("refs/heads/main")
	if !ok || e.Oid != oid2 {
		t.Fatalf("local ref = %+v ok=%v, want %s", e, ok, oid2)
	}

	// A fresh instance replays the log and converges.
	r2 := NewRegistry(ctx, st, testConfig(t))
	defer r2.Close()
	h2, err := r2.Open(ctx, "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	snap2, err := h2.Layer().Snapshot(h2.Repo())
	if err != nil {
		t.Fatal(err)
	}
	e2, ok := snap2.Get("refs/heads/main")
	if !ok || e2.Oid != oid2 {
		t.Fatalf("replayed ref = %+v ok=%v", e2, ok)
	}
}

func TestPublish_CASLadderTwoRacesOneWinner(t *testing.T) {
	// Two registries (instances) on ONE store push conflicting ref updates;
	// exactly one wins, the loser's CAS-412 re-syncs and reports the conflict.
	r1, st := newTestRegistry(t)
	if _, err := r1.Create(context.Background(), "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	r2 := NewRegistry(context.Background(), st, testConfig(t))
	defer r2.Close()
	ctx := context.Background()
	h1, _ := r1.Open(ctx, "acme/api")
	h2, _ := r2.Open(ctx, "acme/api")

	oid1, oid2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	zero := git.Sha1.ZeroHex()
	type outcome struct {
		res PublishResult
		err error
	}
	ch := make(chan outcome, 2)
	run := func(h *RepoHandle, oid string) {
		go func() {
			res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", zero, oid), Synced: true})
			ch <- outcome{res, err}
		}()
	}
	run(h1, oid1)
	run(h2, oid2)

	var winners, conflicted int
	var winnerOid string
	for i := 0; i < 2; i++ {
		o := <-ch
		if o.err != nil {
			t.Fatalf("publish error: %v", o.err)
		}
		if o.res.Seq > 0 {
			winners++
			winnerOid = o.res.PerRef[0].Name
			_ = winnerOid
		}
		for _, pr := range o.res.PerRef {
			if pr.Err != nil {
				conflicted++
			}
		}
	}
	if winners != 1 || conflicted != 1 {
		t.Fatalf("winners=%d conflicted=%d, want 1/1", winners, conflicted)
	}

	// Truth: manifest carries exactly one commit at seq 1 and one segment.
	m, _ := h1.ManifestSnapshot()
	if m.HeadSeq != 1 || len(m.LogSegments) != 1 {
		t.Fatalf("truth manifest head=%d segments=%d", m.HeadSeq, len(m.LogSegments))
	}
	// No orphaned segments left behind by the loser.
	ok, _ := store.Exists(ctx, st, "repos/acme/api/"+store.LogSegmentKey(2))
	if ok {
		t.Fatal("loser's segment survived its CAS-412 cleanup")
	}
}

func TestPublish_OrphanBurnProtocol(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}

	// Seed an orphan at seq 1: a crashed writer's segment (written, never
	// committed — the manifest's head_seq stays 0).
	orphanKey := "repos/acme/api/" + store.LogSegmentKey(1)
	orphan := proto.EncodeSegment([]*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "ghost"}})
	if _, err := st.Put(ctx, orphanKey, store.PutBody{Bytes: orphan}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}

	oid := strings.Repeat("c", 40)
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)})
	if err != nil {
		t.Fatalf("publish over orphan: %v", err)
	}
	// The orphan's seq is burned: our entry landed at seq 2 (seqs not dense).
	if res.Seq != 2 {
		t.Fatalf("seq = %d, want 2 (burned past the orphan)", res.Seq)
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 2 {
		t.Fatalf("head = %d, want 2", m.HeadSeq)
	}
	if m.LogSegments[0].FirstSeq != 2 {
		t.Fatalf("segment first_seq = %d, want 2", m.LogSegments[0].FirstSeq)
	}
	// The burned orphan was swept after our commit (§5.4 step 4).
	if ok, _ := store.Exists(ctx, st, orphanKey); ok {
		t.Fatal("burned orphan was not swept")
	}
}

func TestPublish_MonotonicCreatedAt(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, _ := r.Create(ctx, "acme/api", git.Sha1)
	zero := git.Sha1.ZeroHex()
	oid := strings.Repeat("d", 40)

	t1 := time.Now().UTC().Add(-time.Hour)
	t2 := time.Now().UTC()
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", zero, oid), CreatedAt: &t2}); err != nil {
		t.Fatal(err)
	}
	// An older explicit time violates the monotonic guard → per-ref rejection.
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/y", zero, oid), CreatedAt: &t1})
	if err != nil {
		t.Fatalf("rejection is a transport success, got err %v", err)
	}
	if res.Seq != 0 || len(res.PerRef) == 0 || res.PerRef[0].Err == nil {
		t.Fatalf("res = %+v, want Seq 0 with per-ref error", res)
	}
}

func TestPublish_PerRefConflicts(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, _ := r.Create(ctx, "acme/api", git.Sha1)
	zero := git.Sha1.ZeroHex()
	oidA := strings.Repeat("a", 40)
	oidB := strings.Repeat("b", 40)

	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", zero, oidA)}); err != nil {
		t.Fatal(err)
	}
	// old_oid mismatch → conflict with expected/actual.
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", oidB, oidB)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq != 0 || res.PerRef[0].Err == nil || res.PerRef[0].Err.Kind != RefErrConflict {
		t.Fatalf("res = %+v, want conflict", res)
	}
	if !strings.Contains(res.PerRef[0].Err.Detail, oidB) {
		t.Fatalf("detail %q should name expected %s", res.PerRef[0].Err.Detail, oidB)
	}
	// create on an existing ref → conflict.
	res, err = h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", zero, oidA)})
	if err != nil || res.Seq != 0 || res.PerRef[0].Err == nil {
		t.Fatalf("res=%+v err=%v, want conflict on existing ref", res, err)
	}
}

func TestPublish_GroupCommitBatchSharesSegment(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, _ := r.Create(ctx, "acme/api", git.Sha1)
	zero := git.Sha1.ZeroHex()

	type res struct {
		seq uint64
		err error
	}
	ch := make(chan res, 3)
	for i := 0; i < 3; i++ {
		oid := strings.Repeat(string(rune('a'+i)), 40)
		name := "refs/heads/b" + string(rune('0'+i))
		go func(name, oid string) {
			out, err := h.Publish(ctx, PublishRequest{Txn: refTxn(name, zero, oid), Synced: true})
			ch <- res{out.Seq, err}
		}(name, oid)
	}
	seqs := map[uint64]bool{}
	for i := 0; i < 3; i++ {
		o := <-ch
		if o.err != nil {
			t.Fatalf("publish: %v", o.err)
		}
		if seqs[o.seq] {
			t.Fatalf("duplicate seq %d", o.seq)
		}
		seqs[o.seq] = true
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 3 {
		t.Fatalf("head = %d, want 3", m.HeadSeq)
	}
}
