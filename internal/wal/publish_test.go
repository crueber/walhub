// publish_test.go — the publish path (05 §5.3): CAS-ladder races with one
// winner, the orphan burn protocol (§5.4), monotonic created_at, per-ref
// conflicts, and refs-before-advertise.
package wal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/fault"
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

func TestPublishCompact_AdvancesSeqUploadsPackUpdatesManifest(t *testing.T) {
	// Regression: COMPACT carries a nil Txn — verifyTxn's nil rejection used
	// to silently no-op the whole publish (err nil, seq 0, no manifest change).
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Publish(ctx, PublishRequest{
		Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("a", 40)),
	}); err != nil {
		t.Fatal(err)
	}
	m0, _ := h.ManifestSnapshot()

	// A "repack" result: fake pack + idx files, superseding nothing here.
	packData := []byte("PACK-fake-pack-bytes")
	idxData := []byte("fake-idx-bytes")
	dir := t.TempDir()
	packPath := filepath.Join(dir, "pack-cc.pack")
	indexPath := filepath.Join(dir, "pack-cc.idx")
	if err := os.WriteFile(packPath, packData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, idxData, 0o644); err != nil {
		t.Fatal(err)
	}
	pack := &PreparedPack{Checksum: "cc", PackPath: packPath, IdxPath: indexPath,
		PackSize: uint64(len(packData)), IdxSize: uint64(len(idxData)), Tier: 2}

	res, err := h.PublishCompact(ctx, pack, nil, map[string]string{"agent": "repack"})
	if err != nil {
		t.Fatalf("publishCompact: %v", err)
	}
	if res.Seq != m0.HeadSeq+1 {
		t.Fatalf("seq = %d, want %d (COMPACT must advance the WAL)", res.Seq, m0.HeadSeq+1)
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != m0.HeadSeq+1 {
		t.Fatalf("head = %d, want %d", m.HeadSeq, m0.HeadSeq+1)
	}
	found := false
	for _, p := range m.Packs {
		if p.Checksum == "cc" {
			found = true
			if p.Tier != 2 {
				t.Fatalf("tier = %d, want 2", p.Tier)
			}
		}
	}
	if !found {
		t.Fatalf("manifest packs missing cc: %+v", m.Packs)
	}
	// The pack + idx were uploaded create-if-absent into the bucket.
	for _, key := range []string{"repos/acme/api/" + store.PackKey("cc"), "repos/acme/api/" + store.IdxKey("cc")} {
		if ok, _ := store.Exists(ctx, st, key); !ok {
			t.Fatalf("bucket object missing: %s", key)
		}
	}

	// publishCompact with supersedes: the superseded checksum lands in
	// pending_pack_removals (its removal goes through the try-write path).
	// The dd job must upload its idx too — the manifest's IdxSize claim is
	// only satisfied when IdxPath points at the local idx file.
	res2, err := h.PublishCompact(ctx, &PreparedPack{Checksum: "dd", PackPath: packPath, IdxPath: indexPath,
		PackSize: uint64(len(packData)), IdxSize: uint64(len(idxData)), Tier: 1}, []string{"cc"}, nil)
	if err != nil || res2.Seq != m.HeadSeq+1 {
		t.Fatalf("publishCompact 2: res=%+v err=%v", res2, err)
	}
	m2, _ := h.ManifestSnapshot()
	for _, p := range m2.Packs {
		if p.Checksum == "cc" {
			t.Fatalf("superseded pack still live: %+v", m2.Packs)
		}
	}
	if stt := loadState(h.Dir()); len(stt.PendingPackRemovals) == 0 || stt.PendingPackRemovals[0] != "cc" {
		t.Fatalf("pending removals = %v, want [cc]", stt.PendingPackRemovals)
	}

	// PublishSettings also publishes with a nil Txn.
	if err := h.PublishSettings(ctx, "[git]\ndefault_branch = \"main\"\n", "op", "set defaults", nil); err != nil {
		t.Fatalf("publishSettings: %v", err)
	}
	m3, _ := h.ManifestSnapshot()
	if m3.Settings == nil || m3.Settings.Toml == "" {
		t.Fatalf("settings = %+v, want published", m3.Settings)
	}
	if m3.HeadSeq != m2.HeadSeq+1 {
		t.Fatalf("SETTINGS did not advance head: %d vs %d", m3.HeadSeq, m2.HeadSeq+1)
	}
}

func TestSweepBurned_KeepsConcurrentlyCommittedSegments(t *testing.T) {
	// The sim race (Forgejo #338): a burner records slot S (Create-412 +
	// head-behind + HEAD-present against an in-flight claim), the slot owner
	// commits S, the burner commits on top and sweeps. The sweep must keep
	// the now-listed S and delete only genuine (unlisted) orphans.
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid0 := strings.Repeat("a", 40)
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid0)})
	if err != nil || res.Seq != 1 {
		t.Fatalf("setup push: %+v %v", res, err)
	}
	m, _ := h.ManifestSnapshot()
	if len(m.LogSegments) != 1 {
		t.Fatalf("segments = %+v, want one", m.LogSegments)
	}
	listedKey := m.LogSegments[0].Key // "log/0000000000000001.pb"

	// A genuine orphan: body present, never listed.
	orphanKey := store.LogSegmentKey(9)
	if _, err := store.PutBytes(ctx, st, h.repoKey(orphanKey), []byte("orphan"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}

	h.ensurePublisher()
	h.pub.sweepBurned(map[uint64]string{1: listedKey, 9: orphanKey})

	if ok, err := store.Exists(ctx, st, h.repoKey(listedKey)); err != nil || !ok {
		t.Fatalf("sweep deleted the listed segment %s (exists=%v err=%v)", listedKey, ok, err)
	}
	if ok, err := store.Exists(ctx, st, h.repoKey(orphanKey)); err != nil || ok {
		t.Fatalf("sweep kept the unlisted orphan %s (exists=%v err=%v)", orphanKey, ok, err)
	}
}

func TestPublish_FailedBatchSweepsItsBurns(t *testing.T) {
	// Failure-path GC (Forgejo #338): a batch that burns orphan slots and
	// then fails must sweep what IT burned. Otherwise orphan backlogs grow
	// without bound — every later attempt re-burns them all (300 ms + fault
	// exposure per slot), deaths add frontier orphans, and at 9 consecutive
	// the ErrCorrupt cap locks every writer out permanently (liveness death
	// spiral, found by TestSim_SafetyThenLiveness).
	truth := store.NewMemory()
	link := fault.New(truth, "t", 1)
	cfg := testConfig(t)
	cfg.WAL.CASMaxRetries = 1
	r := NewRegistry(context.Background(), link, cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Plant consecutive orphans above head 0 (bypassing the link: no faults).
	for _, seq := range []uint64{1, 2, 3} {
		if _, err := store.PutBytes(ctx, truth, "repos/acme/api/"+store.LogSegmentKey(seq),
			[]byte("orphan"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	// From here the manifest CAS always loses (fake 412, manifest keys
	// only — log slot ops behave normally).
	link.Set(fault.Plan{PCASFail: 1.0}.WithOnly("manifest.pb"))

	// The push burns 1,2,3, claims 4, loses the CAS, exhausts the 1-attempt
	// ladder and fails — then the failure defer must sweep the burns.
	res, err := h.Publish(ctx, PublishRequest{
		Txn:    refTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("a", 40)),
		Synced: true,
	})
	if err == nil || res.Seq != 0 {
		t.Fatalf("doomed push: res=%+v err=%v (want batch failure, no seq)", res, err)
	}
	// The failure-path sweep runs in runBatch's return defer — after the
	// reply — so poll (it always runs; the test must not race it).
	deadline := time.Now().Add(10 * time.Second)
	for _, seq := range []uint64{1, 2, 3, 4} {
		key := "repos/acme/api/" + store.LogSegmentKey(seq)
		for {
			ok, herr := store.Exists(ctx, truth, key)
			if herr != nil {
				t.Fatalf("slot %d HEAD: %v", seq, herr)
			}
			if !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("slot %d not swept after failed batch", seq)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 0 {
		t.Fatalf("truth moved under a failed batch: head=%d", m.HeadSeq)
	}
}

func TestPublish_WipedVersionTokenHealsOnFreshen(t *testing.T) {
	// Forgejo #338: a wiped version token ("" — e.g. after a casLanded
	// version-recovery HEAD failure) must heal on the next freshen. The
	// revision guard rejects the same-rev manifest but must still adopt its
	// token (same rev = same commit = same bytes). Without the adopt, every
	// later CAS degrades to PutCreate-412 and the ladder spins to exhaustion
	// deterministically — no faults needed, no Sync ever re-runs on the
	// CAS-412 path to save it.
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid0 := strings.Repeat("a", 40)
	if _, err := h.Publish(ctx, PublishRequest{
		Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid0),
	}); err != nil {
		t.Fatalf("setup push: %v", err)
	}
	h.version = "" // simulate the wipe (white-box: casLanded HEAD failure)
	res, err := h.Publish(ctx, PublishRequest{
		Txn: refTxn("refs/heads/main", oid0, strings.Repeat("b", 40)),
	})
	if err != nil || res.Seq != 2 {
		t.Fatalf("post-wipe push: res=%+v err=%v (want commit at seq 2)", res, err)
	}
	if h.version == "" {
		t.Fatal("version token still wiped after a successful freshen")
	}
}
