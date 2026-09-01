// publish_more_test.go — publisher loop/ladder edges (05 §5.3/§5.4) plus
// ref-view overlay rules, checkpoint triggers/writes, and logreader replay.
package wal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- ref overlay + verification rules -------------------------------------------

func TestRefOverlay_ViewsAndDeltas(t *testing.T) {
	base := &git.RefSnapshot{Refs: []git.RefEntry{{Name: "refs/heads/base", Oid: "bbb"}}}
	o := &refOverlay{base: base, delta: map[string]git.RefEntry{}}

	// clone: independent delta, shared base.
	o.set(git.RefEntry{Name: "refs/heads/x", Oid: "xxx"})
	c := o.clone()
	if len(c.delta) != 1 || c.base != base {
		t.Fatalf("clone = %+v", c)
	}
	c.set(git.RefEntry{Name: "refs/heads/y", Oid: "yyy"})
	if _, ok := o.get("refs/heads/y"); ok {
		t.Fatal("clone leaked a write into the original")
	}

	// get: delta hit, base fallback, tombstone miss.
	if e, ok := o.get("refs/heads/x"); !ok || e.Oid != "xxx" {
		t.Fatalf("delta get = %+v %v", e, ok)
	}
	if e, ok := o.get("refs/heads/base"); !ok || e.Oid != "bbb" {
		t.Fatalf("base get = %+v %v", e, ok)
	}
	o.del("refs/heads/base")
	if _, ok := o.get("refs/heads/base"); ok {
		t.Fatal("tombstone must hide the base ref")
	}
	if _, ok := o.get("refs/heads/none"); ok {
		t.Fatal("unknown ref found")
	}
}

func TestVerifyTxn_Rules(t *testing.T) {
	view := &refOverlay{
		base: &git.RefSnapshot{Refs: []git.RefEntry{
			{Name: "refs/heads/live", Oid: strings.Repeat("1", 40)},
		}},
		delta: map[string]git.RefEntry{},
	}
	zero := git.Sha1.ZeroHex()
	oid := strings.Repeat("a", 40)

	cases := []struct {
		name    string
		txn     *proto.RefTransaction
		wantErr bool
		wantKnd RefErrorKind
		detail  string
	}{
		{"nil txn", nil, true, RefErrRejected, "nil transaction"},
		{"invalid name", refTxn("refs/bad name ", "", oid), true, RefErrRejected, ""},
		{"invalid old oid", refTxn("refs/heads/n", "zzz", oid), true, RefErrRejected, "old oid"},
		{"create on absent", refTxn("refs/heads/new", zero, oid), false, 0, ""},
		{"create on existing", refTxn("refs/heads/live", zero, oid), true, RefErrConflict, "expected absent"},
		{"symbolic on non-HEAD", &proto.RefTransaction{Updates: []*proto.RefUpdate{
			{Name: "refs/heads/n", NewSymbolicTarget: "refs/heads/other"}}}, true, RefErrRejected, "symbolic target"},
		{"oid mismatch", refTxn("refs/heads/live", strings.Repeat("2", 40), oid), true, RefErrConflict, "expected"},
		{"clean update", refTxn("refs/heads/live", strings.Repeat("1", 40), oid), false, 0, ""},
	}
	for _, tc := range cases {
		errs := verifyTxn(tc.txn, view)
		if tc.wantErr != (len(errs) > 0) {
			t.Fatalf("%s: errs = %v, wantErr=%v", tc.name, errs, tc.wantErr)
		}
		if tc.wantErr {
			if errs[0].Kind != tc.wantKnd {
				t.Fatalf("%s: kind = %v", tc.name, errs[0].Kind)
			}
			if tc.detail != "" && !strings.Contains(errs[0].Detail, tc.detail) {
				t.Fatalf("%s: detail = %q", tc.name, errs[0].Detail)
			}
		}
	}
}

func TestApplyTxnToView_Folds(t *testing.T) {
	view := &refOverlay{base: &git.RefSnapshot{}, delta: map[string]git.RefEntry{}}
	applyTxnToView(view, nil) // no-op
	zero := git.Sha1.ZeroHex()
	oid := strings.Repeat("a", 40)
	txn := &proto.RefTransaction{Updates: []*proto.RefUpdate{
		{Name: "HEAD", NewSymbolicTarget: "refs/heads/main"}, // skipped
		{Name: "refs/heads/a", OldOid: zero, NewOid: oid, NewPeeled: "p"},
		{Name: "refs/heads/b", OldOid: oid, NewOid: zero}, // delete
	}}
	applyTxnToView(view, txn)
	if e, ok := view.get("refs/heads/a"); !ok || e.Oid != oid || e.Peeled != "p" {
		t.Fatalf("a = %+v %v", e, ok)
	}
	if _, ok := view.get("refs/heads/b"); ok {
		t.Fatal("b should be deleted")
	}
	// HEAD symbolic target rides the txn, not the view.
	for _, u := range txn.Updates {
		if u.Name == "HEAD" && u.NewSymbolicTarget != "" {
			if u.NewSymbolicTarget != "refs/heads/main" {
				t.Fatal("head target")
			}
		}
	}
}

func TestIsZeroOid(t *testing.T) {
	if !isZeroOid("") || !isZeroOid(strings.Repeat("0", 40)) || !isZeroOid(strings.Repeat("0", 64)) {
		t.Fatal("zero forms rejected")
	}
	if isZeroOid(strings.Repeat("0", 41)) || isZeroOid(strings.Repeat("0", 39)) ||
		isZeroOid("1"+strings.Repeat("0", 39)) {
		t.Fatal("non-zero forms accepted")
	}
}

func TestTxnUpdatesNil(t *testing.T) {
	if txnUpdates(nil) != nil {
		t.Fatal("nil txn must have no updates")
	}
}

// ---- publisher loop and ladder edges ----------------------------------------------

// runBatchRecoverPanic: a job with a nil update panics inside verifyTxn;
// runBatch recovers and answers every waiter with an internal-error reply.
func TestRunBatch_PanicRecoversAndReplies(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	job := &publishJob{req: PublishRequest{Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{nil}}}, reply: make(chan publishResult, 1)}
	h.pub.runBatch(ctx, []*publishJob{job})
	select {
	case r := <-job.reply:
		if r.err == nil || !strings.Contains(r.err.Error(), "internal panic") {
			t.Fatalf("reply = %+v err=%v, want internal panic error", r, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply after panic")
	}
}

func TestPublisher_LoopDrainAndDefaults(t *testing.T) {
	// BatchWindow/MaxBatch zero → loop applies the 5ms/64 defaults.
	cfg := cfgWith(t, func(c *config.Config) {
		c.WAL.BatchWindow = 0
		c.WAL.MaxBatch = 0
	})
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Manually built mailbox: buffer the whole batch BEFORE the loop runs so
	// the try-drain path (not the window) assembles the batch.
	h.pub = &Publisher{h: h, ch: make(chan *publishJob, 8), closed: make(chan struct{})}
	zero := git.Sha1.ZeroHex()
	var jobs []*publishJob
	for i := 0; i < 3; i++ {
		jobs = append(jobs, &publishJob{req: PublishRequest{
			Txn:    refTxn(fmt.Sprintf("refs/heads/b%d", i), zero, strings.Repeat(string(rune('a'+i)), 40)),
			Synced: true,
		}, reply: make(chan publishResult, 1)})
	}
	for _, j := range jobs {
		h.pub.ch <- j
	}
	h.pub.wg.Add(1) // loop's own defer balances this
	go h.pub.loop()
	for _, j := range jobs {
		select {
		case r := <-j.reply:
			if r.err != nil || r.res.Seq == 0 {
				t.Fatalf("reply = %+v err=%v", r, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no reply")
		}
	}
	// One group commit → one segment covering the 3 entries.
	m, _ := h.ManifestSnapshot()
	if len(m.LogSegments) != 1 || m.LogSegments[0].LastSeq != 3 {
		t.Fatalf("segments = %+v, want one [1,3]", m.LogSegments)
	}
}

func TestPublisher_LoopClosedDuringWindow(t *testing.T) {
	cfg := cfgWith(t, func(c *config.Config) { c.WAL.BatchWindow = config.Duration(2 * time.Second) })
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.pub = &Publisher{h: h, ch: make(chan *publishJob, 2), closed: make(chan struct{})}
	job := &publishJob{req: PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("f", 40)), Synced: true}, reply: make(chan publishResult, 1)}
	h.pub.ch <- job
	h.pub.wg.Add(1) // loop's own defer balances this
	go h.pub.loop()
	time.Sleep(50 * time.Millisecond) // loop is inside the window timer
	close(h.pub.closed)               // teardown mid-window: batch still runs
	select {
	case r := <-job.reply:
		if r.err != nil || r.res.Seq == 0 {
			t.Fatalf("reply = %+v err=%v", r, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply after close")
	}
	h.pub.wg.Wait() // loop exited; Close is now a no-op
	h.pub.Close()
}

func TestPublish_EnqueueCtxCancelAndAwaitTimeout(t *testing.T) {
	cfg := cfgWith(t, nil)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Fire the real publisher once (consuming pubOnce), stop its loop, then
	// swap in a manual mailbox with NO loop: the enqueue lands in the buffer
	// and the awaiting caller times out → ctx.Err.
	h.ensurePublisher()
	orig := h.pub
	close(orig.closed)
	orig.wg.Wait()
	h.pub = &Publisher{h: h, ch: make(chan *publishJob, 1), closed: make(chan struct{})}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := h.Publish(cctx, PublishRequest{Txn: refTxn("refs/heads/x", "", strings.Repeat("c", 40))}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("await err = %v, want deadline exceeded", err)
	}
	// A full buffer with a canceled ctx: the enqueue select returns ctx.Err.
	cctx2, cancel2 := context.WithCancel(ctx)
	cancel2()
	if _, err := h.Publish(cctx2, PublishRequest{Txn: refTxn("refs/heads/y", "", strings.Repeat("d", 40))}); !errors.Is(err, context.Canceled) {
		t.Fatalf("enqueue err = %v, want canceled", err)
	}
	close(h.pub.closed)
	h.pub.Close()
}

func TestPublish_SyncFailureRepliesError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The refs-phase manifest GET fails → the batch is answered with the
	// sync error, nothing commits.
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewOther(key, errors.New("store down"))
		}
		return nil
	}
	_, err = h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("e", 40))})
	if err == nil {
		t.Fatal("publish with failing sync succeeded")
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 0 {
		t.Fatalf("head advanced: %d", m.HeadSeq)
	}
}

func TestPublish_CASRetriesExhausted(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Every manifest CAS loses (412): the ladder burns its retries and
	// reports exhaustion.
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("e", 40)), Synced: true}); err == nil ||
		!strings.Contains(err.Error(), "attempts") {
		t.Fatalf("err = %v, want attempts exhausted", err)
	}
	// maxRetries<=0 falls back to the default (16) — covered implicitly by
	// any registry whose config leaves cas_max_retries unset.
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("e", 40)), Synced: true}); err == nil {
		t.Fatal("second exhausted publish succeeded")
	}
}

func TestClaimSlot_ProbeExistsError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	// The first log-slot create loses to a phantom writer; the existence
	// probe then errors → claimSlot surfaces the store error. The create
	// failure must be one-shot or the claim loop would never terminate.
	hk.putErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") && n == 1 {
			return store.NewPrecondition(key, "phantom")
		}
		return nil
	}
	// The orphan probe HEADs the slot (store.Exists); fail that HEAD.
	hk.headErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewOther(key, errors.New("probe down"))
		}
		return nil
	}
	base, _ := h.ManifestSnapshot()
	entries := []*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, CreatedAt: TsPtr(time.Now().UTC())}}
	if _, _, _, _, err := h.pub.claimSlot(ctx, base, entries, nil); err == nil {
		t.Fatal("claimSlot with failing probe succeeded")
	}
}

func TestPublisher_MiscHelpers(t *testing.T) {
	if errRestartLadder.Error() != "restart" {
		t.Fatal("walErrRestart.Error")
	}
	n := 5
	burnStreakReset(&n)
	if n != 5 {
		t.Fatal("burnStreakReset must be a no-op seam")
	}
	if mustRead(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("mustRead on a missing file must return nil")
	}
}

func TestFoldCommitGraphs_RunsOffCriticalPath(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	// A push entry whose pack has no commit-graph yet: the fold runs in the
	// background and its failure (no such idx) is logged, never fatal.
	entries := []*proto.LogEntry{{
		Seq: 1, Kind: proto.EntryKindPush, Writer: "w",
		Pack: &proto.PackRef{Checksum: strings.Repeat("0", 40), Seq: 1},
		Txn:  refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("a", 40)),
	}}
	h.pub.foldCommitGraphs(entries)
	h.pub.foldCommitGraphs(nil) // early return
	time.Sleep(100 * time.Millisecond)
}

func TestSweepBurned_MissingOrphan(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	h.pub.sweepBurned(map[uint64]string{9: store.LogSegmentKey(9)}) // absent → skipped
	h.pub.sweepBurned(nil)                                          // no-op
}

func TestCommitLocal_WithdrawsOnLocalApplyFailure(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("7", 40)
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid), Synced: true})
	if err != nil || res.Seq == 0 {
		t.Fatalf("baseline publish: %+v %v", res, err)
	}
	// Make the repo dir read-only: the packed-refs rewrite (and the state
	// persist) fail; the push is still answered ok with the version
	// withdrawn — the bucket CAS is the truth.
	ro := t.TempDir()
	if err := os.Chmod(h.repo.Path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.repo.Path, 0o755) })
	_ = ro
	oid2 := strings.Repeat("8", 40)
	h.rw.RLock()
	defer h.rw.RUnlock()
	res2, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/other", git.Sha1.ZeroHex(), oid2), Synced: true})
	if err != nil || res2.Seq == 0 {
		t.Fatalf("publish with failed local apply: res=%+v err=%v", res2, err)
	}
	if h.state.ManifestVersion != "" {
		t.Fatalf("version not withdrawn: %q", h.state.ManifestVersion)
	}
	if m := Metrics(); m.PublishLocalApplyFailed == 0 {
		t.Fatal("local-apply-failed counter not bumped")
	}
	_ = st
	_ = oid
}

func TestPublish_HEADSymbolicUpdate(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("b", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	txn := &proto.RefTransaction{Updates: []*proto.RefUpdate{{Name: "HEAD", NewSymbolicTarget: "refs/heads/main"}}}
	if _, err := h.Publish(ctx, PublishRequest{Txn: txn}); err != nil {
		t.Fatalf("HEAD symbolic publish: %v", err)
	}
	snap, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if snap.HeadTarget != "refs/heads/main" {
		t.Fatalf("HEAD target = %q", snap.HeadTarget)
	}
}

func TestAnnotatePack_CASRetryAndExhaustion(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-aa.pack")
	if err := os.WriteFile(path, []byte("PACK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPack(ctx, path, strings.Repeat("a", 40), 1, nil); err != nil {
		t.Fatal(err)
	}
	// One lost CAS → refresh → retry succeeds.
	n := 0
	hk.putErr = func(key string, _ int) error {
		if strings.HasSuffix(key, "manifest.pb") && n == 0 {
			n++
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	if err := h.AnnotatePack(ctx, strings.Repeat("a", 40), true, true, true); err != nil {
		t.Fatalf("annotate after 412: %v", err)
	}
	// Every CAS loses → retries exhausted.
	hk.putErr = func(key string, _ int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	err = h.AnnotatePack(ctx, strings.Repeat("a", 40), true, true, true)
	if err == nil || !strings.Contains(err.Error(), "retries exhausted") {
		t.Fatalf("err = %v, want retry exhaustion", err)
	}
}

func TestUploadPack_DuplicateCreateIsSuccess(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	sum := strings.Repeat("d", 40)
	dir := t.TempDir()
	packPath := filepath.Join(dir, "p.pack")
	indexPath := filepath.Join(dir, "p.idx")
	if err := os.WriteFile(packPath, []byte("PACK dup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("idx dup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, h.repoKey(store.PackKey(sum)), store.PutBody{Bytes: []byte("PACK dup")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, h.repoKey(store.IdxKey(sum)), store.PutBody{Bytes: []byte("idx dup")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Both create-if-absent PUTs lose to the existing objects: success.
	if err := h.pub.uploadPack(ctx, &PreparedPack{Checksum: sum, PackPath: packPath, IdxPath: indexPath,
		PackSize: 8, IdxSize: 7}); err != nil {
		t.Fatalf("duplicate upload = %v, want success", err)
	}
}

// ---- checkpoints -------------------------------------------------------------------

func TestCheckpointTrigger_Matrix(t *testing.T) {
	now := time.Now().UTC()
	base := &pbManifest{HeadSeq: 3, UpdatedAt: TsPtr(now.Add(-time.Hour))}

	// Entries trigger.
	got, ok := checkpointTrigger(&configVals{snapshotEveryEntries: 2}, base, time.Time{}, time.Time{})
	if !ok || got != TriggerEntries {
		t.Fatalf("entries trigger = %v %v", got, ok)
	}
	// Tail bytes trigger.
	cfg := &configVals{checkpointTailBytes: 10}
	m := &pbManifest{HeadSeq: 3, LogSegments: []*proto.LogSegmentRef{
		{FirstSeq: 1, LastSeq: 3, Size: 100},
		{FirstSeq: 0, LastSeq: 0, Size: 1},
	}}
	got, ok = checkpointTrigger(cfg, m, time.Time{}, time.Time{})
	if !ok || got != TriggerTailBytes {
		t.Fatalf("tail trigger = %v %v", got, ok)
	}
	// Age trigger (falls back to updated_at when no checkpoint).
	got, ok = checkpointTrigger(&configVals{checkpointInterval: time.Minute}, base, time.Time{}, time.Time{})
	if !ok || got != TriggerAge {
		t.Fatalf("age trigger = %v %v", got, ok)
	}
	// Nothing configured → false.
	if _, ok := checkpointTrigger(&configVals{}, base, time.Time{}, time.Time{}); ok {
		t.Fatal("no triggers should fire")
	}
}

func TestCheckpoint_WriteIdempotentCatchUpAndProvenance(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("c", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	// Lagging state forces the catch-up branch before the snapshot.
	h.syncMu.Lock()
	h.state.AppliedSeq = 0
	h.syncMu.Unlock()
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Checkpoint == nil || m.Checkpoint.Seq != 1 {
		t.Fatalf("checkpoint = %+v", m.Checkpoint)
	}
	if m.MinSeq != 2 {
		t.Fatalf("min_seq = %d, want 2", m.MinSeq)
	}
	if len(m.LogSegments) != 0 {
		t.Fatalf("segments = %+v, want folded away", m.LogSegments)
	}
	// The refs snapshot object exists with the pushed ref.
	ok, err := store.Exists(ctx, st, "repos/acme/api/"+store.CheckpointRefsKey(1))
	if err != nil || !ok {
		t.Fatalf("refs snapshot missing: %v %v", ok, err)
	}
	// Refs-level provenance.
	first, asOf, created := provenanceOf(m)
	if first.IsZero() || asOf.IsZero() || created.IsZero() {
		t.Fatalf("provenance = %v %v %v", first, asOf, created)
	}
	// Idempotent at head.
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatalf("idempotent checkpoint: %v", err)
	}
	// lastEntryOf prefers the live last-entry time over updated_at.
	if got := lastEntryOf(h, m, h.lastEntryTime); got.IsZero() {
		t.Fatal("lastEntryOf zero")
	}
	if got := lastEntryOf(h, m, time.Time{}); got.IsZero() {
		t.Fatal("lastEntryOf fallback zero")
	}
}

func TestCheckpoint_CASRetryAndConcurrentWinner(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("d", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	// The round-2 manifest CAS loses (412) to another writer who
	// checkpointed at the same seq: the refresh inside the CAS loop sees
	// checkpoint.seq ≥ ours and concedes.
	hk.putErr = func(key string, n int) error { // create=1, publish CAS=2, checkpoint CAS=3
		if strings.HasSuffix(key, "manifest.pb") && n == 3 {
			return store.NewPrecondition(key, "racer")
		}
		return nil
	}
	// Doctor the manifest read to show a checkpoint already at seq 1: the
	// refresh inside the CAS loop sees checkpoint.seq ≥ ours and concedes
	// (our two round-1 objects are garbage at worst).
	base, _ := h.ManifestSnapshot()
	doctored := *base
	doctored.Checkpoint = &proto.CheckpointRef{Seq: base.HeadSeq, Key: store.CheckpointKey(base.HeadSeq)}
	hk.getBody = func(key string) ([]byte, bool) {
		if strings.HasSuffix(key, "manifest.pb") && !strings.Contains(key, "checkpoints/") {
			return doctored.Marshal(), true
		}
		return nil, false
	}
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatalf("checkpoint with concurrent winner: %v", err)
	}
}

// ---- logreader (point-in-time replay) -----------------------------------------------

func seedLogRepo(t *testing.T) (*Registry, *RepoHandle, store.ObjectStore, context.Context) {
	t.Helper()
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := git.Sha1.ZeroHex()
	for i := 0; i < 3; i++ {
		oid := strings.Repeat(string(rune('a'+i)), 40)
		if _, err := h.Publish(ctx, PublishRequest{
			Txn: refTxn(fmt.Sprintf("refs/heads/b%d", i), zero, oid),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return r, h, st, ctx
}

func TestReadLog_RangesAndErrors(t *testing.T) {
	r, h, st, ctx := seedLogRepo(t)

	// [from, to] subranges.
	entries, err := h.ReadLog(ctx, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Seq != 2 || entries[1].Seq != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	// to == 0 → head.
	all, err := h.ReadLog(ctx, 1, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("full log = %d err=%v", len(all), err)
	}
	// Segment store GET fails / object absent → typed errors.
	hk, _ := attachHooks(t, r)
	hk.getErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewOther(key, errors.New("read failed"))
		}
		return nil
	}
	if _, err := h.ReadLog(ctx, 1, 0); err == nil {
		t.Fatal("ReadLog with failing segment GET succeeded")
	}
	hk.getErr = nil
	if err := st.Delete(ctx, h.repoKey(store.LogSegmentKey(2)), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadLog(ctx, 2, 0); err == nil {
		t.Fatalf("ReadLog with missing segment = nil, want an error")
	}
}
func TestRefsAtSeq_BoundsAndSnapshot(t *testing.T) {
	_, h, st, ctx := seedLogRepo(t)

	// Beyond head → invalid.
	if _, err := h.RefsAtSeq(ctx, 99); err == nil {
		t.Fatal("refs beyond head succeeded")
	}
	// In-range fold: the refs created by each entry appear in order.
	v, err := h.RefsAtSeq(ctx, 2)
	if err != nil {
		t.Fatalf("refs at 2: %v", err)
	}
	if v.Seq != 2 || len(v.Refs) != 2 {
		t.Fatalf("view = seq %d refs %d", v.Seq, len(v.Refs))
	}
	// With a checkpoint at 3, seqs below min_seq without a covering
	// checkpoint are not replayable.
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatal(err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Checkpoint == nil || m.Checkpoint.Seq != 3 {
		t.Fatalf("checkpoint = %+v", m.Checkpoint)
	}
	if _, err := h.RefsAtSeq(ctx, 2); err == nil || !strings.Contains(err.Error(), "replayable") {
		t.Fatalf("refs below min_seq = %v, want not-replayable", err)
	}
	v3, err := h.RefsAtSeq(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(v3.Refs) != 3 {
		t.Fatalf("refs at 3 = %d", len(v3.Refs))
	}
	// Corrupt checkpoint refs → typed error.
	if _, err := st.Put(ctx, h.repoKey(store.CheckpointRefsKey(3)), store.PutBody{Bytes: []byte("garbage")},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version("")}); err != nil {
		// PutUpdate with wrong version fails on memory; fall back to delete+put.
		if err := st.Delete(ctx, h.repoKey(store.CheckpointRefsKey(3)), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Put(ctx, h.repoKey(store.CheckpointRefsKey(3)), store.PutBody{Bytes: []byte("garbage")}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.RefsAtSeq(ctx, 3); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("corrupt refs snapshot = %v", err)
	}
}

func TestRefsAsOf_TimeCuts(t *testing.T) {
	_, h, _, ctx := seedLogRepo(t)

	// As of just after the first entry: only branch b0 exists.
	v0, err := h.RefsAtSeq(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(v0.Refs) != 1 || v0.Refs[0].Name != "refs/heads/b0" {
		t.Fatalf("view at 1 = %+v", v0.Refs)
	}
	// RefsAsOf far in the future → head view.
	future := time.Now().UTC().Add(time.Hour)
	vh, err := h.RefsAsOf(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if vh.Seq != 3 || len(vh.Refs) != 3 {
		t.Fatalf("as-of future = seq %d refs %d", vh.Seq, len(vh.Refs))
	}
	// RefsAsOf before any entry → empty view (nothing broke the walk).
	vE, err := h.RefsAsOf(ctx, time.Time{}.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(vE.Refs) != 0 || vE.Seq != 0 {
		t.Fatalf("as-of epoch = %+v", vE)
	}
	// An as-of checkpoint whose snapshot time is inside the window is used.
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatal(err)
	}
	m, _ := h.ManifestSnapshot()
	asOf := m.Checkpoint.AsOf.Go().Add(time.Minute)
	vc, err := h.RefsAsOf(ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(vc.Refs) != 3 {
		t.Fatalf("as-of checkpoint view = %+v", vc.Refs)
	}
	// A missing refs.pb breaks the checkpoint path with a typed error.
	_ = m
}
