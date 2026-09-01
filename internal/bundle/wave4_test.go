package bundle

// wave4_test.go — third-pass coverage: updateList CAS ladder, backoff, lease
// acquire/keeper release, cron parsing arms, plan windows/states/backfill
// arms, strategy validation arms, resolve verdicts, serve rendering branches,
// v2 advertisement, D17 forcing eviction, and the scan-boundary header path.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- fault-injecting store wrapper ---------------------------------------------

type wave4Store struct {
	store.ObjectStore
	casFail     atomic.Int64 // next N conditional puts/composes → PreconditionFailed
	retryGet    atomic.Int64 // next N gets → Retryable
	composeErr  atomic.Int64 // next N composes → generic error
	retryPut    atomic.Int64 // next N puts → Retryable
	listPutErr  atomic.Int64 // next N puts of bundles/list.pb → generic error
	composeFail atomic.Int64 // next N composes → PreconditionFailed
	headErr     atomic.Int64 // next N heads → generic error
	putErr      atomic.Int64 // next N puts (before casFail) → generic error
}

func (w *wave4Store) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == BundleListKey && w.listPutErr.Load() > 0 {
		w.listPutErr.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindOther, Key: key, Err: errors.New("boom")}
	}
	if w.putErr.Load() > 0 {
		w.putErr.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindOther, Key: key, Err: errors.New("bucket down")}
	}
	if w.retryPut.Load() > 0 {
		w.retryPut.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindRetryable, Key: key, Err: errors.New("flaky")}
	}
	if w.casFail.Load() > 0 {
		w.casFail.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: key}
	}
	return w.ObjectStore.Put(ctx, key, body, opts)
}

func (w *wave4Store) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	if w.composeFail.Load() > 0 {
		w.composeFail.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: dst}
	}
	if w.casFail.Load() > 0 {
		w.casFail.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: dst}
	}
	if w.composeErr.Load() > 0 {
		w.composeErr.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindOther, Key: dst, Err: errors.New("compose down")}
	}
	return w.ObjectStore.Compose(ctx, dst, sources, opts)
}

func (w *wave4Store) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if w.retryGet.Load() > 0 {
		w.retryGet.Add(-1)
		return nil, &store.StoreError{Kind: store.ErrKindRetryable, Key: key, Err: errors.New("flaky read")}
	}
	return w.ObjectStore.Get(ctx, key, opts)
}

func (w *wave4Store) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if w.headErr.Load() > 0 {
		w.headErr.Add(-1)
		return nil, &store.StoreError{Kind: store.ErrKindOther, Key: key, Err: errors.New("head down")}
	}
	return w.ObjectStore.Head(ctx, key)
}

func noCtx() context.Context { return context.Background() }

// --- updateList CAS ladder -------------------------------------------------------

func TestWave4UpdateListConflictLadder(t *testing.T) {
	st := &wave4Store{ObjectStore: store.NewMemory()}
	st.casFail.Store(2) // two 412s, third attempt wins
	n := 0
	err := updateList(noCtx(), st, 3, func(cur *proto.BundleList) (*proto.BundleList, error) {
		n++
		return &proto.BundleList{Mode: "all"}, nil
	})
	if err != nil || n != 3 {
		t.Fatalf("err=%v attempts=%d, want nil/3", err, n)
	}
	if l, _, _ := storeGetList(t, st); l.Mode != "all" || l.UpdatedAt == nil {
		t.Fatalf("committed list = %+v", l)
	}
}

func TestWave4UpdateListRetriesExhausted(t *testing.T) {
	st := &wave4Store{ObjectStore: store.NewMemory()}
	st.casFail.Store(1)
	err := updateList(noCtx(), st, 0, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return &proto.BundleList{}, nil
	})
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("err = %v, want ErrRetriesExhausted", err)
	}
}

func TestWave4UpdateListRetryablePutAndRead(t *testing.T) {
	// Retryable PUT: backoff, re-attempt, not counted as a CAS conflict.
	st := &wave4Store{ObjectStore: store.NewMemory()}
	st.retryPut.Store(1)
	err := updateList(noCtx(), st, 2, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return &proto.BundleList{Mode: "any"}, nil
	})
	if err != nil {
		t.Fatalf("retryable put: %v", err)
	}
	// Retryable GET: same treatment on the read path.
	st2 := &wave4Store{ObjectStore: store.NewMemory()}
	st2.retryGet.Store(1)
	err = updateList(noCtx(), st2, 2, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return &proto.BundleList{Mode: "all"}, nil
	})
	if err != nil {
		t.Fatalf("retryable get: %v", err)
	}
}

func TestWave4UpdateListAbortCorruptCancel(t *testing.T) {
	// f returns (nil, nil) → abort without writing.
	st := store.NewMemory()
	err := updateList(noCtx(), st, 2, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, _, err := store.GetBytes(noCtx(), st, BundleListKey, store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("abort must not write; get err = %v", err)
	}

	// Corrupt list body → store.NewCorrupt.
	bad := store.NewMemory()
	if _, err := bad.Put(noCtx(), BundleListKey, store.PutBody{Bytes: []byte("junk")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	err = updateList(noCtx(), bad, 2, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return &proto.BundleList{}, nil
	})
	if err == nil || !store.IsCorrupt(err) {
		t.Fatalf("corrupt err = %v", err)
	}

	// Mutator error propagates.
	err = updateList(noCtx(), st, 2, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return nil, errors.New("mutator failed")
	})
	if err == nil || err.Error() != "mutator failed" {
		t.Fatalf("mutator err = %v", err)
	}

	// Cancelled context short-circuits.
	ctx, cancel := context.WithCancel(noCtx())
	cancel()
	if err := updateList(ctx, st, 2, func(cur *proto.BundleList) (*proto.BundleList, error) {
		return &proto.BundleList{}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx err = %v", err)
	}
}

func TestWave4Backoff(t *testing.T) {
	if err := backoff(noCtx(), 0); err != nil { // 5 ms base
		t.Fatalf("backoff(0): %v", err)
	}
	if err := backoff(noCtx(), 9); err != nil { // capped at 100 ms
		t.Fatalf("backoff(9): %v", err)
	}
	ctx, cancel := context.WithCancel(noCtx())
	cancel()
	if err := backoff(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backoff = %v", err)
	}
}

// --- leases -----------------------------------------------------------------------

func wave4LeaseBytes(t *testing.T, holder string, epoch uint64, expires time.Time) []byte {
	t.Helper()
	exp := proto.TimeFromGo(expires)
	acq := proto.TimeFromGo(expires.Add(-leaseTTL))
	return (&proto.Lease{Holder: holder, Purpose: "bundle build", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: epoch}).Marshal()
}

func TestWave4AcquireLeaseCreateAndRelease(t *testing.T) {
	st := store.NewMemory()
	release, err := acquireLease(noCtx(), st, "s1", "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Head(noCtx(), store.LeaseKey("s1")); err != nil {
		t.Fatalf("lease object missing: %v", err)
	}
	release()
	if meta, _ := st.Head(noCtx(), store.LeaseKey("s1")); meta != nil {
		t.Fatal("release must delete the lease")
	}
}

func TestWave4AcquireLeaseHeldAndSteal(t *testing.T) {
	st := store.NewMemory()
	// Live lease by another host → held.
	if _, err := st.Put(noCtx(), store.LeaseKey("s2"), store.PutBody{Bytes: wave4LeaseBytes(t, "other", 1, time.Now().Add(time.Minute))}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLease(noCtx(), st, "s2", "me"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld", err)
	}
	// Expired lease by another host → stolen, epoch incremented.
	if _, err := st.Put(noCtx(), store.LeaseKey("s3"), store.PutBody{Bytes: wave4LeaseBytes(t, "other", 3, time.Now().Add(-time.Minute))}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	release, err := acquireLease(noCtx(), st, "s3", "me")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := store.GetBytes(noCtx(), st, store.LeaseKey("s3"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var cur proto.Lease
	if err := cur.Unmarshal(body); err != nil {
		t.Fatal(err)
	}
	if cur.Holder != "me" || cur.Epoch != 4 {
		t.Fatalf("stolen lease = %+v, want holder me epoch 4", cur)
	}
	release()

	// Corrupt lease body → corrupt error.
	if _, err := st.Put(noCtx(), store.LeaseKey("s4"), store.PutBody{Bytes: []byte("junk")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLease(noCtx(), st, "s4", "me"); err == nil || !store.IsCorrupt(err) {
		t.Fatalf("corrupt err = %v", err)
	}
}

func TestWave4AcquireLeaseOwnReacquire(t *testing.T) {
	st := store.NewMemory()
	r1, err := acquireLease(noCtx(), st, "s5", "me")
	if err != nil {
		t.Fatal(err)
	}
	// Same holder, live lease → re-acquired with epoch+1 (idempotent reclaim).
	r2, err := acquireLease(noCtx(), st, "s5", "me")
	if err != nil {
		t.Fatalf("same-holder reacquire: %v", err)
	}
	r2()
	r1()
}

func TestWave4AcquireLeaseContentionExhaustsAttempts(t *testing.T) {
	st := &wave4Store{ObjectStore: store.NewMemory()}
	st.casFail.Store(99) // every claim loses the race
	if _, err := acquireLease(noCtx(), st, "s6", "me"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld after 4 attempts", err)
	}
}

func TestWave4LeaseReleaseOnVanishedObject(t *testing.T) {
	st := store.NewMemory()
	release, err := acquireLease(noCtx(), st, "s7", "me")
	if err != nil {
		t.Fatal(err)
	}
	// Lease object disappears before release: release must not panic or write.
	_ = st.Delete(noCtx(), store.LeaseKey("s7"), "")
	release()
}

// --- cron parsing arms -------------------------------------------------------------

func TestWave4CronHasAndFirst(t *testing.T) {
	c, err := ParseSchedule("0 30 4 1 6 *")
	if err != nil {
		t.Fatal(err)
	}
	if !c.has(1, 30) || c.has(1, 31) {
		t.Fatal("has(min)")
	}
	if !c.has(2, 4) || !c.has(3, 1) || !c.has(4, 6) || !c.has(0, 0) {
		t.Fatal("has arms")
	}
	if !c.has(5, 0) || !c.has(5, 1) || !c.has(5, 6) {
		t.Fatal("has(dow)")
	}
	var empty Cron
	if empty.first(1) != -1 {
		t.Fatalf("first on empty mask = %d, want -1", empty.first(1))
	}
	if c.first(1) != 30 || c.first(4) != 6 {
		t.Fatal("first arms")
	}
}

func TestWave4NextSetArms(t *testing.T) {
	m := maskOf(3) | maskOf(9)
	if got := nextSet(m, 1); got != 3 {
		t.Fatalf("nextSet = %d, want 3", got)
	}
	if got := nextSet(m, 3); got != 9 {
		t.Fatalf("nextSet = %d, want 9", got)
	}
	if got := nextSet(0, 0); got != -1 {
		t.Fatalf("nextSet(empty) = %d, want -1", got)
	}
	if got := nextSet(m, 9); got != -1 {
		t.Fatalf("nextSet(past last) = %d, want -1", got)
	}
	if got := nextSet(maskOf(63), 62); got != 63 {
		t.Fatalf("nextSet(62) = %d, want 63", got)
	}
	if got := nextSet(m, 63); got != -1 {
		t.Fatalf("nextSet(after>=63) = %d, want -1", got)
	}
}

func TestWave4ParseScheduleErrorArms(t *testing.T) {
	cases := []string{
		"0 0 0 1,,1 * *",                 // empty term
		"0 0 */0 * * *",                  // step 0
		"0 0 */x * * *",                  // step non-numeric
		"0 0 0 10-5 * *",                 // range lo > hi
		"0 99999999999999999999 * * * *", // value overflow → bad value
		"0 0 0 * * fri",                  // weekday name
	}
	for _, in := range cases {
		if _, err := ParseSchedule(in); err == nil {
			t.Fatalf("ParseSchedule(%q) must fail", in)
		}
	}
	// Vixie `a/s` step from an anchored value; dow 7 folds onto Sunday.
	c, err := ParseSchedule("0 0 0 5/10 * 7")
	if err != nil {
		t.Fatal(err)
	}
	if !c.has(5, 0) || !c.has(3, 5) || !c.has(3, 15) || !c.has(3, 25) {
		t.Fatalf("a/s + dow-7 masks wrong: dom=%b dow=%b", c.dom, c.dow)
	}
	if c.has(3, 10) {
		t.Fatal("5/10 must not include 10")
	}
}

func TestWave4BetweenArms(t *testing.T) {
	c := cronOf(t, "* * * * * *") // every second → cap trips
	start := at("2026-03-10T00:00:00Z")
	if _, err := c.Between(start, start.Add(3*time.Hour)); !errors.Is(err, ErrBetweenCap) {
		t.Fatalf("err = %v, want ErrBetweenCap", err)
	}
	// end before start → empty, no error.
	if got, err := c.Between(start.Add(time.Hour), start); err != nil || got != nil {
		t.Fatalf("reversed window = %v, %v", got, err)
	}
	// Window with no fires (Feb 31) → empty without error.
	nf := cronOf(t, "0 0 0 31 2 *")
	got, err := nf.Between(start, start.AddDate(0, 3, 0))
	if err != nil || len(got) != 0 {
		t.Fatalf("no-fire window = %v, %v", got, err)
	}
}

// --- plan windows / states / backfill ----------------------------------------------

func TestWave4NewestSlotsArms(t *testing.T) {
	// n <= 0 → nil.
	if got := newestSlots(&Strategy{Schedule: cronOf(t, "@daily")}, at("2026-03-10T12:00:00Z"), 0); got != nil {
		t.Fatalf("n=0 → %v", got)
	}
	// Schedule with >10k fires in the lookback window → Between cap → nil.
	if got := newestSlots(&Strategy{Schedule: cronOf(t, "* * * * * *")}, at("2026-03-10T12:00:00Z"), 2); got != nil {
		t.Fatalf("capped schedule → %v", got)
	}
	// Schedule that never fires: lookback expands past the cap → empty.
	got := newestSlots(&Strategy{Schedule: cronOf(t, "0 0 0 31 2 *")}, at("2026-03-10T12:00:00Z"), 2)
	if len(got) != 0 {
		t.Fatalf("never-fires → %v", got)
	}
	// Weekly keep=3 forces multi-step lookback expansion.
	wk := &Strategy{Schedule: cronOf(t, "@weekly")}
	got = newestSlots(wk, at("2026-03-10T12:00:00Z"), 3)
	if len(got) != 3 || got[0] >= got[1] {
		t.Fatalf("weekly newest 3 = %v", got)
	}
}

func TestWave4PlanStatesArms(t *testing.T) {
	strats := []Strategy{
		{Name: "blk", Kind: KindIncremental, Base: "hr2", Schedule: cronOf(t, "@hourly")},
		{Name: "dl", Kind: KindFull, Schedule: cronOf(t, "@daily"), Keep: 2},
		{Name: "hr", Kind: KindFull, Schedule: cronOf(t, "@hourly"), Keep: 2},
		{Name: "hr2", Kind: KindFull, Schedule: cronOf(t, "@hourly"), Keep: 2},
		{Name: "nc", Kind: KindIncremental, Base: "wk", Schedule: cronOf(t, "@daily")},
		{Name: "wk", Kind: KindFull, Schedule: cronOf(t, "@weekly"), Keep: 3},
	}
	first := at("2026-03-05T00:00:00Z")
	now := at("2026-03-10T12:00:00Z")
	wkEntry := entry("wk", "wk/2026-02-15T00:00:00Z", KindFull, epoch(t, "2026-02-15T00:00:00Z"), epoch(t, "2026-02-15T00:00:00Z"), 1)
	dlEntry := entry("dl", "dl/2026-03-10T00:00:00Z", KindFull, epoch(t, "2026-03-10T00:00:00Z"), epoch(t, "2026-03-10T00:00:00Z"), 2)
	skipped := &proto.SkippedSlot{Strategy: "dl", Slot: epoch(t, "2026-03-09T00:00:00Z"),
		Reason: "too-small: 1 commits (min 5)"}
	list := protoBundleList(wkEntry, dlEntry)
	list.Skipped = []*proto.SkippedSlot{skipped}

	states := PlanStates("o/r", strats, list, now,
		func(string) (time.Time, bool) { return first, true },
		func(s *Strategy) bool { return s.Name != "hr" })

	got := map[string]string{}
	for _, ps := range states {
		got[ps.Strategy+"/"+FormatSlot(ps.When)] = ps.State
	}
	want := map[string]string{
		"blk/20260310T110000Z": "blocked", // base hr2 has no entries at all
		"blk/20260310T120000Z": "pending", // pending precedes the blocked check
		"dl/20260309T000000Z":  "too-small",
		"dl/20260310T000000Z":  "built",
		"hr/20260310T110000Z":  "wrong-host",
		"hr/20260310T120000Z":  "wrong-host", // hostFits precedes the pending check
		"hr2/20260310T110000Z": "missing",
		"hr2/20260310T120000Z": "pending",
		"nc/20260309T000000Z":  "missing", // base wk entry at Feb 15 resolves
		"nc/20260310T000000Z":  "missing",
		"wk/20260222T000000Z":  "unavailable",
		"wk/20260301T000000Z":  "unavailable",
		"wk/20260308T000000Z":  "missing",
	}
	for k, w := range want {
		if g := got[k]; g != w {
			t.Errorf("state[%s] = %q, want %q", k, g, w)
		}
	}
	if len(got) != len(want) {
		for k, v := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("unexpected state %s=%s", k, v)
			}
		}
	}
	// No hostFits/firstStateAt callbacks → those arms are skipped.
	plain := PlanStates("o/r", strats, protoBundleList(), now, nil, nil)
	if len(plain) == 0 {
		t.Fatal("plain plan states empty")
	}
}

func TestWave4BackfillPlanArms(t *testing.T) {
	strats := []Strategy{
		{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily"), Keep: 3, BackfillMax: 1},
		{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily")},
	}
	now := at("2026-03-10T12:00:00Z")
	// Newest full slot built; two older full slots missing → the full's plan is
	// capped at backfill_max=1, oldest-first. inc's Mar 9 slot has no resolvable
	// base (base entry is at Mar 10) → blocked, excluded; Mar 10 resolves → plan.
	built := entry("full", "full/2026-03-10T00:00:00Z", KindFull, epoch(t, "2026-03-10T00:00:00Z"), epoch(t, "2026-03-10T00:00:00Z"), 1)
	plan := BackfillPlan(strats, protoBundleList(built), now)
	if len(plan) != 2 {
		t.Fatalf("plan = %+v, want [full capped oldest, inc tie slot]", plan)
	}
	if plan[0].Strategy != "full" || plan[0].State != "missing" ||
		plan[0].Slot != epoch(t, "2026-03-08T00:00:00Z") {
		t.Fatalf("plan[0] = %+v", plan[0])
	}
	if plan[1].Strategy != "inc" || plan[1].Slot != epoch(t, "2026-03-10T00:00:00Z") {
		t.Fatalf("plan[1] = %+v", plan[1])
	}
}

func TestWave4ListHelpers(t *testing.T) {
	if entryAt(nil, "a", 1) != nil {
		t.Fatal("entryAt(nil)")
	}
	if inWindow([]uint64{1, 2}, 3) || !inWindow([]uint64{1, 2}, 2) {
		t.Fatal("inWindow")
	}
	if s := skippedAt(nil, "a", 1); s != nil {
		t.Fatal("skippedAt(nil)")
	}
	// settleSkipped: strategy gone from config, slot with entry, slot outside window.
	vGone := &proto.SkippedSlot{Strategy: "ghost", Slot: 1}
	vSettled := &proto.SkippedSlot{Strategy: "s", Slot: 2, BaseID: ""}
	vStale := &proto.SkippedSlot{Strategy: "s", Slot: 3, BaseID: ""}
	vKeep := &proto.SkippedSlot{Strategy: "s", Slot: 4, BaseID: ""}
	e := entry("s", "s/2", KindFull, 2, 2, 0)
	list := protoBundleList(e)
	list.Skipped = []*proto.SkippedSlot{vGone, vSettled, vStale, vKeep}
	out := settleSkipped(list, []Strategy{{Name: "s", Kind: KindFull}},
		ByName([]Strategy{{Name: "s", Kind: KindFull}}), map[string][]uint64{"s": {4}})
	if len(out.Skipped) != 1 || out.Skipped[0] != vKeep {
		t.Fatalf("settleSkipped kept = %+v", out.Skipped)
	}
	// nil list / empty skipped → unchanged.
	if settleSkipped(nil, nil, nil, nil) != nil {
		t.Fatal("settleSkipped(nil)")
	}
	// SettleAndPrune over an absent list aborts without writing.
	keys, err := SettleAndPrune(noCtx(), store.NewMemory(), []Strategy{{Name: "s", Kind: KindFull}}, time.Now())
	if err != nil || keys != nil {
		t.Fatalf("SettleAndPrune empty = %v, %v", keys, err)
	}
}

// --- strategy arms -------------------------------------------------------------------

func TestWave4EffectiveDefaults(t *testing.T) {
	s := &Strategy{Name: "x", Kind: KindFull}
	if s.EffectiveKeep() != DefaultKeep {
		t.Fatal("EffectiveKeep default")
	}
	if (&Strategy{Name: "y", MinCommits: 3}).EffectiveMinCommits(0) != 3 {
		t.Fatal("EffectiveMinCommits per-strategy")
	}
	if (&Strategy{Name: "z"}).EffectiveMinCommits(0) != DefaultMinCommits {
		t.Fatal("EffectiveMinCommits host-default fallback")
	}
}

func TestWave4ValidateStrategiesMoreArms(t *testing.T) {
	daily := cronOf(t, "@daily")
	cases := []struct {
		name string
		s    []Strategy
	}{
		{"bad kind", []Strategy{{Name: "x", Kind: "weird", Schedule: daily}}},
		{"negative backfill", []Strategy{{Name: "x", Kind: KindFull, Schedule: daily, BackfillMax: -1}}},
		{"negative min_commits", []Strategy{{Name: "x", Kind: KindFull, Schedule: daily, MinCommits: -1}}},
		{"bad filter", []Strategy{{Name: "x", Kind: KindFull, Schedule: daily, Filter: "tree:0"}}},
		{"empty ref glob", []Strategy{{Name: "x", Kind: KindFull, Schedule: daily, Refs: []string{""}}}},
		{"cycle", []Strategy{
			{Name: "a", Kind: KindIncremental, Base: "b", Schedule: daily},
			{Name: "b", Kind: KindIncremental, Base: "a", Schedule: daily},
		}},
		{"filter mix", []Strategy{
			{Name: "a", Kind: KindIncremental, Base: "b", Schedule: daily},
			{Name: "b", Kind: KindFull, Schedule: daily, Filter: FilterBlobNone},
		}},
	}
	for _, tc := range cases {
		if err := ValidateStrategies(tc.s); err == nil {
			t.Errorf("ValidateStrategies(%s) must fail", tc.name)
		}
	}
}

func TestWave4SelectRefsArms(t *testing.T) {
	refs := []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1", "HEAD", "refs/notes/x"}
	// Default globs: heads, tags, HEAD; extra_refs adds notes.
	got := SelectRefs(nil, false, []string{"refs/notes/*"}, refs)
	want := []string{"HEAD", "refs/heads/dev", "refs/heads/main", "refs/notes/x", "refs/tags/v1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default = %v, want %v", got, want)
	}
	// main_only: HEAD + main only.
	got = SelectRefs(nil, true, nil, refs)
	if strings.Join(got, ",") != "HEAD,refs/heads/main" {
		t.Fatalf("mainOnly = %v", got)
	}
	// Strategy refs override everything.
	s := &Strategy{Refs: []string{"refs/tags/*"}}
	got = SelectRefs(s, true, nil, refs)
	if strings.Join(got, ",") != "refs/tags/v1" {
		t.Fatalf("own refs = %v", got)
	}
	// HEAD reorder: HEAD arrives mid-list and must land first.
	got = SelectRefs(nil, false, nil, []string{"refs/heads/a", "HEAD", "refs/heads/b"})
	if got[0] != "HEAD" || len(got) != 3 {
		t.Fatalf("HEAD-first = %v", got)
	}
	// Unmatched globs are fine.
	if got := SelectRefs(nil, true, nil, []string{"refs/heads/other"}); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}
}

// --- resolve arms ---------------------------------------------------------------------

type errWal struct{ fakeWal }

func (e *errWal) RefsAsOf(ctx context.Context, repo string, at time.Time) (Refs, uint64, error) {
	return nil, 0, errors.New("fold failed")
}

func TestWave4ContentAtArms(t *testing.T) {
	w := &errWal{}
	c, err := ContentAt(noCtx(), w, "o/r", at("2026-03-10T00:00:00Z"))
	if err != nil || c.Verdict != VerdictNoState || c.NoStateErr == nil || c.AsOfSeq != 0 {
		t.Fatalf("no-state content = %+v, %v", c, err)
	}
	fw := &fakeWal{first: map[string]time.Time{"o/r": at("2026-03-09T00:00:00Z")}}
	c, err = ContentAt(noCtx(), fw, "o/r", at("2026-03-08T00:00:00Z"))
	if err != nil || c.Verdict != VerdictUnavail {
		t.Fatalf("unavail = %+v, %v", c, err)
	}
}

func TestWave4BaseIDForAndTips(t *testing.T) {
	byName := ByName([]Strategy{{Name: "full", Kind: KindFull}, {Name: "inc", Kind: KindIncremental, Base: "full"}})
	id, err := BaseIDFor(byName["full"], 1, protoBundleList(), byName)
	if err != nil || id != "" {
		t.Fatalf("full base id = %q, %v", id, err)
	}
	if _, err := BaseIDFor(byName["inc"], 1, protoBundleList(), byName); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked err = %v", err)
	}
	// NewestEntry nil list + tie-break on CreationToken.
	if NewestEntry(nil, "s", 1, false) != nil {
		t.Fatal("NewestEntry(nil)")
	}
	e1 := entry("s", "s/1", KindFull, 1, 1, 0)
	e1b := entry("s", "s/1b", KindFull, 1, 2, 0)
	if got := NewestEntry(protoBundleList(e1, e1b), "s", 1, true); got != e1b {
		t.Fatalf("tie-break = %+v", got)
	}
	// SameTips order-insensitive, len-mismatch, and derefRefs nil filtering.
	a := Refs{{Name: "HEAD", Oid: "1"}, {Name: "refs/heads/main", Oid: "2"}}
	b := Refs{{Name: "refs/heads/main", Oid: "2"}, {Name: "HEAD", Oid: "1"}}
	if !SameTips(a, b) || SameTips(a, Refs{{Name: "HEAD", Oid: "1"}}) {
		t.Fatal("SameTips")
	}
	d := derefRefs([]*proto.Ref{nil, {Name: "HEAD", Oid: "1"}})
	if len(d) != 1 || d[0].Oid != "1" {
		t.Fatalf("derefRefs = %v", d)
	}
}

// --- serve rendering branches ----------------------------------------------------------

func TestWave4ServeURIAndRender(t *testing.T) {
	st := store.NewMemory()
	srv := &Server{PublicBase: "https://b.example", St: st, ServeVia: ServeSignedURL,
		SignedURLFor: map[string]bool{"o/r": true}, WarnOnce: func(string, string) {}}
	e := entry("full", "full/1", KindFull, 1, 1, 0)
	if u := srv.URI(noCtx(), "o/r", e); u == "" || u == srv.ProxyURI("o/r", e) {
		t.Fatalf("signed uri = %q", u)
	}
	// Unlisted repo → proxy even under signed mode.
	if u := srv.URI(noCtx(), "o/other", e); u != srv.ProxyURI("o/other", e) {
		t.Fatalf("proxy uri = %q", u)
	}
	// warnOnce: nil callback is a no-op; second report suppressed.
	(&Server{}).warnOnce("o/r", "m")
	n := 0
	w := &Server{WarnOnce: func(string, string) { n++ }}
	w.warnOnce("o/r", "m")
	w.warnOnce("o/r", "m")
	if n != 1 {
		t.Fatalf("warnOnce count = %d", n)
	}
	// mode/heuristic fallbacks and catchup rendering.
	list := &proto.BundleList{Bundles: []*proto.BundleEntry{e}}
	if modeOf(nil) != "all" || heuristicOf(nil) != "creationToken" {
		t.Fatal("mode/heuristic fallbacks")
	}
	out, err := srv.Render(noCtx(), "o/r", list, false, "")
	if err != nil || !strings.Contains(out, "[bundle]") || strings.Contains(out, e.ID) {
		t.Fatalf("catchup render = %q, %v (fulls excluded)", out, err)
	}
	// uriLine quoting arms.
	if uriLine("") != `""` || !strings.HasPrefix(uriLine("a b"), `"`) {
		t.Fatal("uriLine arms")
	}
	// intersectIDs with an empty right side.
	if got := intersectIDs([]*proto.BundleEntry{e}, nil); got != nil {
		t.Fatal("intersectIDs empty")
	}
	// FamilyFilter rejects unknown filters (error path through Render).
	if _, err := srv.Render(noCtx(), "o/r", protoBundleList(e), true, "tree:0"); err == nil {
		t.Fatal("bad filter must fail")
	}
}

// --- advertisement + D17 ----------------------------------------------------------------

func TestWave4AdvertiseLines(t *testing.T) {
	st := store.NewMemory()
	srv := &Server{PublicBase: "https://b.example", St: st}
	full := entry("full", "full/1", KindFull, 1, 1, 0)
	inc := entry("inc", "inc/1", KindIncremental, 1, 2, 0)
	inc.BaseID = full.ID
	filt := entry("full", "full/f1", KindFull, 1, 3, 0)
	filt.Filter = FilterBlobNone
	filtInc := entry("inc", "inc/f1", KindIncremental, 1, 4, 0)
	filtInc.Filter = FilterBlobNone
	filtInc.BaseID = filt.ID
	list := protoBundleList(full, inc, filt, filtInc)

	lines, err := AdvertiseLines(noCtx(), srv, "o/r", list, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"bundle.version=1", "bundle.mode=all", "bundle.heuristic=creationToken",
		"bundle." + full.ID + ".uri=https://b.example/o/r.git/bundles/full/",
		"bundle." + inc.ID + ".creationToken=2",
		"bundle." + filt.ID + ".filter=blob:none",
		"bundle." + filtInc.ID + ".uri=",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lines missing %q:\n%s", want, joined)
		}
	}
	// advertise_filtered=false keeps the filtered family out.
	lines, err = AdvertiseLines(noCtx(), srv, "o/r", list, false)
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(lines, "\n"); strings.Contains(j, ".filter=") {
		t.Fatalf("plain advertisement must omit filter lines:\n%s", j)
	}
}

func TestWave4D17EvictionAndArms(t *testing.T) {
	g := NewD17Tracker()
	now := at("2026-03-10T12:00:00Z")
	// Unknown principal → refuse.
	if ok, warn := g.Decide("o/r", "ghost", now); ok || warn != "" {
		t.Fatal("unknown principal must refuse")
	}
	// Stale list fetch → refuse.
	g.RecordListFetch("o/r", "stale", now.Add(-2*time.Hour))
	if ok, _ := g.Decide("o/r", "stale", now); ok {
		t.Fatal("stale list fetch must refuse")
	}
	// Fresh fetch, no fallback → allow once, warn; second within the gap → refuse.
	g.RecordListFetch("o/r", "fresh", now)
	if ok, warn := g.Decide("o/r", "fresh", now); !ok || warn != D17Warning {
		t.Fatalf("allow = %v, warn = %q", ok, warn)
	}
	if ok, _ := g.Decide("o/r", "fresh", now.Add(time.Minute)); ok {
		t.Fatal("second fallback within the 6 h gap must refuse")
	}
	// Sweep keeps fresh entries (fallback newer than listFetch) and drops old.
	g.RecordListFetch("o/r", "old", now.Add(-7*time.Hour))
	g.Sweep(now)
	g.mu.Lock()
	_, fresh := g.entries[d17Key{"o/r", "fresh"}]
	_, stale := g.entries[d17Key{"o/r", "stale"}] // 2 h old: inside the 6 h TTL
	_, old := g.entries[d17Key{"o/r", "old"}]
	order := len(g.order)
	g.mu.Unlock()
	if !fresh || !stale || old || order != 2 {
		t.Fatalf("sweep: fresh=%v stale=%v old=%v order=%d", fresh, stale, old, order)
	}
}

func TestWave4D17EvictOldest(t *testing.T) {
	g := NewD17Tracker()
	now := at("2026-03-10T12:00:00Z")
	first := d17Key{"o/r", "p0"}
	for i := range d17Cap {
		g.RecordListFetch("o/r", fmt.Sprintf("p%d", i), now)
	}
	g.RecordListFetch("o/r", "overflow", now) // pushes past the cap
	g.mu.Lock()
	_, gone := g.entries[first]
	_, kept := g.entries[d17Key{"o/r", "overflow"}]
	g.mu.Unlock()
	if gone || !kept {
		t.Fatalf("evict-oldest: first present=%v overflow present=%v", gone, kept)
	}
}

// --- header scan boundary ------------------------------------------------------------------

func TestWave4ScanPackOffsetStraddle(t *testing.T) {
	dir := t.TempDir()
	// PACK magic straddling the 64 KiB chunk boundary (offset 65533).
	filler := make([]byte, 65_533)
	f := filepath.Join(dir, "b.bundle")
	if err := os.WriteFile(f, append(filler, []byte("PACK\x00\x00\x00\x02")...), 0o644); err != nil {
		t.Fatal(err)
	}
	off, err := ScanPackOffset(f)
	if err != nil || off != 65_533 {
		t.Fatalf("off = %d, %v; want 65533", off, err)
	}
	// No magic anywhere → ErrPackMagic.
	nf := filepath.Join(dir, "none.bundle")
	if err := os.WriteFile(nf, make([]byte, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanPackOffset(nf); !errors.Is(err, ErrPackMagic) {
		t.Fatalf("err = %v, want ErrPackMagic", err)
	}
	// Missing file → open error.
	if _, err := ScanPackOffset(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("missing file must fail")
	}
}

// --- bind_wal conversion + error plumbing ---------------------------------------------------

func TestWave4RefsFromView(t *testing.T) {
	v := &wal.RefsView{Seq: 7, Refs: []git.RefEntry{
		{Name: "refs/heads/b", Oid: "22", Peeled: ""},
		{Name: "HEAD", Oid: "11"},
		{Name: "refs/tags/v1", Oid: "33", Peeled: "44"},
	}}
	refs := refsFromView(v)
	if len(refs) != 3 || refs[0].Name != "HEAD" || refs[1].Name != "refs/heads/b" || refs[2].Name != "refs/tags/v1" {
		t.Fatalf("refsFromView = %+v", refs)
	}
	if refs[2].Oid != "33" || refs[2].Peeled != "44" {
		t.Fatalf("protoRefOf mapping = %+v", refs[2])
	}
}
func configDefaultsForWave4(t *testing.T) *config.Config {
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	return cfg
}

func TestWave4WalAdapterErrorPlumbing(t *testing.T) {
	cfg := configDefaultsForWave4(t)
	reg := wal.NewRegistry(noCtx(), store.NewMemory(), cfg)
	defer reg.Close()
	a := &WalAdapter{R: reg}
	// Unopenable repo → error from Open (manifest absent).
	if _, _, err := a.RefsAsOf(noCtx(), "ghost/none", time.Now()); err == nil {
		t.Fatal("RefsAsOf on unknown repo must fail")
	}
	if _, err := a.RefsAtSeq(noCtx(), "ghost/none", 1); err == nil {
		t.Fatal("RefsAtSeq on unknown repo must fail")
	}
	// Unopenable repo → FirstStateAt reports absent.
	if _, ok := a.FirstStateAt("ghost/none"); ok {
		t.Fatal("FirstStateAt on unknown repo must be absent")
	}
}

// dead-code seam kept for completeness: strategyFilterOf reads the base's filter.
func TestWave4StrategyFilterOf(t *testing.T) {
	base := entry("full", "full/1", KindFull, 1, 1, 0)
	base.Filter = FilterBlobNone
	if got := strategyFilterOf(&Deps{}, base); got != FilterBlobNone {
		t.Fatalf("strategyFilterOf = %q", got)
	}
}

// --- second pass: error arms + bind_wal happy path -----------------------------------

func TestWave4LeaseErrorArms(t *testing.T) {
	// BuildSlot surfaces the lease failure (§8.9.1 lease before any work).
	st := &wave4Store{ObjectStore: store.NewMemory()}
	st.casFail.Store(99)
	d := &Deps{Wal: &fakeWal{}, Prim: &fakePrim{}, St: st, HostID: "h", List: protoBundleList()}
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}
	if err := BuildSlot(noCtx(), d, "o/r", []Strategy{*s}, s, at("2026-03-10T00:00:00Z")); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("err = %v", err)
	}

	// Head failure.
	st = &wave4Store{ObjectStore: store.NewMemory()}
	st.headErr.Store(1)
	if _, err := acquireLease(noCtx(), st, "e1", "me"); err == nil {
		t.Fatal("head failure must propagate")
	}

	// Generic put failure on create.
	st = &wave4Store{ObjectStore: store.NewMemory()}
	st.putErr.Store(1)
	if _, err := acquireLease(noCtx(), st, "e2", "me"); err == nil {
		t.Fatal("create put failure must propagate")
	}

	// 412 during the steal path → retry, then win.
	base := store.NewMemory()
	if _, err := base.Put(noCtx(), store.LeaseKey("e3"), store.PutBody{Bytes: wave4LeaseBytes(t, "other", 3, time.Now().Add(-time.Minute))}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	st = &wave4Store{ObjectStore: base}
	st.casFail.Store(1)
	release, err := acquireLease(noCtx(), st, "e3", "me")
	if err != nil {
		t.Fatalf("steal after one 412: %v", err)
	}
	release()

	// Generic put failure on the steal path.
	st = &wave4Store{ObjectStore: base}
	st.putErr.Store(1)
	if _, err := acquireLease(noCtx(), st, "e3", "me"); err == nil {
		t.Fatal("steal put failure must propagate")
	}

	// Release with a failing read → best-effort no-op.
	lst := store.NewMemory()
	rel, err := acquireLease(noCtx(), lst, "e4", "me")
	if err != nil {
		t.Fatal(err)
	}
	wl := &wave4Store{ObjectStore: lst}
	_ = wl
	rel()

	// Release when another host has since taken the lease: no delete.
	lst = store.NewMemory()
	rel, err = acquireLease(noCtx(), lst, "e5", "me")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := lst.Head(noCtx(), store.LeaseKey("e5"))
	if err != nil || meta == nil {
		t.Fatal(err)
	}
	if _, err := lst.Put(noCtx(), store.LeaseKey("e5"), store.PutBody{Bytes: wave4LeaseBytes(t, "thief", 9, time.Now().Add(time.Minute))}, store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}); err != nil {
		t.Fatal(err)
	}
	rel()
	if meta, _ := lst.Head(noCtx(), store.LeaseKey("e5")); meta == nil {
		t.Fatal("release must not delete another host's lease")
	}
}

func TestWave4WalAdapterHappyPath(t *testing.T) {
	st := store.NewMemory()
	cfg := configDefaultsForWave4(t)
	reg := wal.NewRegistry(noCtx(), st, cfg)
	defer reg.Close()

	// One log segment at seq 1 creating refs/heads/main; no checkpoint.
	const oid = "1111111111111111111111111111111111111111"
	created := proto.TimeFromGo(at("2026-03-01T00:00:00Z"))
	createdPtr := &created
	seg := proto.EncodeSegment([]*proto.LogEntry{{
		Seq:       1,
		Txn:       &proto.RefTransaction{Updates: []*proto.RefUpdate{{Name: "refs/heads/main", OldOid: strings.Repeat("0", 40), NewOid: oid}}},
		CreatedAt: createdPtr,
		Writer:    "wave4",
	}})
	id := git.RepoId{Owner: "demo", Name: "repo"}
	segKey := id.StorePrefix() + store.LogSegmentKey(1)
	if _, err := st.Put(noCtx(), segKey, store.PutBody{Bytes: seg}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	man := &proto.Manifest{
		FormatVersion: 1, Repo: "demo/repo", ObjectFormat: "sha1",
		HeadSeq: 1, MinSeq: 1, Revision: 1, Writer: "wave4",
		LogSegments: []*proto.LogSegmentRef{{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 1, Size: uint64(len(seg)), Sealed: true}},
	}
	if _, err := st.Put(noCtx(), id.StorePrefix()+store.Manifest, store.PutBody{Bytes: man.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}

	a := &WalAdapter{R: reg}
	refs, seq, err := a.RefsAsOf(noCtx(), "demo/repo", at("2026-03-02T00:00:00Z"))
	if err != nil || seq != 1 {
		t.Fatalf("RefsAsOf = %v, %d, %v", refs, seq, err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" || refs[0].Oid != oid {
		t.Fatalf("refs = %+v", refs)
	}
	refs, err = a.RefsAtSeq(noCtx(), "demo/repo", 1)
	if err != nil || len(refs) != 1 || refs[0].Oid != oid {
		t.Fatalf("RefsAtSeq = %v, %v", refs, err)
	}
	first, ok := a.FirstStateAt("demo/repo")
	if !ok || !first.Equal(at("2026-03-01T00:00:00Z")) {
		t.Fatalf("FirstStateAt = %v, %v", first, ok)
	}
}

func TestWave4MicroArms(t *testing.T) {
	// EffectiveMinCommits host default > 0.
	if got := (&Strategy{Name: "x"}).EffectiveMinCommits(7); got != 7 {
		t.Fatalf("EffectiveMinCommits = %d", got)
	}
	// FromConfig: invalid table fails closed.
	if _, err := FromConfig([]config.BundleStrategy{
		{Name: "x", Kind: "full", Schedule: "@daily"},
		{Name: "x", Kind: "full", Schedule: "@daily"},
	}, 0); err == nil {
		t.Fatal("duplicate names must fail")
	}
	// EntryByID arms.
	if EntryByID(nil, "a") != nil || EntryByID(protoBundleList(), "a") != nil {
		t.Fatal("EntryByID arms")
	}
	// newestAncestorEntry with an unknown base → nil (BaseFor → blocked).
	byName := ByName([]Strategy{{Name: "inc", Kind: KindIncremental, Base: "ghost"}})
	if BaseFor(byName["inc"], 1, protoBundleList(), byName) != nil {
		t.Fatal("unknown base must resolve to no base")
	}
	// slotsFrom capped window → nil.
	if got := slotsFrom(&Strategy{Schedule: cronOf(t, "* * * * * *")}, 1, at("2026-03-10T12:00:00Z")); got != nil {
		t.Fatalf("slotsFrom cap = %v", got)
	}
	// newestBaseBundle with a missing base strategy → nil.
	if got := newestBaseBundle(byName["inc"], byName, protoBundleList()); got != nil {
		t.Fatal("newestBaseBundle ghost")
	}
	// nonOrphaned/CloneEntries on a nil list.
	if got := CloneEntries(nil); got != nil {
		t.Fatal("CloneEntries(nil)")
	}
	// pruneRetention on a nil list.
	if got := pruneRetention(nil, nil, nil); got != nil {
		t.Fatal("pruneRetention(nil)")
	}
	// PlanStates "skipped" arm: a verdict that is not too-small.
	strats := []Strategy{{Name: "s", Kind: KindFull, Schedule: cronOf(t, "@daily"), Keep: 2}}
	list := protoBundleList()
	list.Skipped = []*proto.SkippedSlot{{Strategy: "s", Slot: epoch(t, "2026-03-09T00:00:00Z"), Reason: "unchanged since x"}}
	states := PlanStates("o/r", strats, list, at("2026-03-10T12:00:00Z"), nil, nil)
	found := false
	for _, ps := range states {
		if ps.Strategy == "s" && ps.State == "skipped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("states = %+v", states)
	}
}

func TestWave4CronMicroArms(t *testing.T) {
	c := cronOf(t, "0 30 4 1 6 *")
	for f, want := range map[int]int{0: 0, 2: 4, 3: 1} {
		if got := c.first(f); got != want {
			t.Fatalf("first(%d) = %d, want %d", f, got, want)
		}
	}
	// Range with an out-of-bounds hi value.
	if _, err := ParseSchedule("0 0 0 5-99 * *"); err == nil {
		t.Fatal("range hi out of bounds must fail")
	}
	// Range with an empty hi value.
	if _, err := ParseSchedule("0 0 0 5- * *"); err == nil {
		t.Fatal("empty range hi must fail")
	}
	// Step crossing the field bound breaks the loop.
	if c, err := ParseSchedule("0 0 0 * * 5/3"); err != nil || !c.has(5, 5) || c.has(5, 1) {
		t.Fatalf("dow step = %v, %v", c, err)
	}
	// Minute field exhausted → Next jumps a full hour.
	c, err := ParseSchedule("0 59 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	next, err := c.Next(at("2026-03-10T01:59:01Z"))
	if err != nil || next.Equal(at("2026-03-10T01:59:01Z")) {
		t.Fatalf("Next = %v, %v", next, err)
	}
	if next.Hour() != 2 || next.Minute() != 59 || next.Day() != 10 {
		t.Fatalf("Next = %v, want 02:59 same day", next)
	}
}

func TestWave4ScanPackSmallFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tiny")
	if err := os.WriteFile(f, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanPackOffset(f); !errors.Is(err, ErrPackMagic) {
		t.Fatalf("err = %v", err)
	}
}

func TestWave4D17SweepFallbackNewer(t *testing.T) {
	g := NewD17Tracker()
	t1 := at("2026-03-10T10:00:00Z")
	g.RecordListFetch("o/r", "p", t1)
	if ok, _ := g.Decide("o/r", "p", t1.Add(time.Hour)); !ok {
		t.Fatal("fallback must be allowed")
	}
	g.Sweep(t1.Add(time.Hour)) // fallback (11:00) is newer than listFetch (10:00)
	g.mu.Lock()
	_, kept := g.entries[d17Key{"o/r", "p"}]
	g.mu.Unlock()
	if !kept {
		t.Fatal("entry with newer fallback must survive the sweep")
	}
}

func TestWave4BuildAndComposeErrorMicro(t *testing.T) {
	// composeFull: scratch put fails.
	d, fw, base, packPath := wave4ComposeFixture(t)
	wst := &wave4Store{ObjectStore: d.St}
	wst.putErr.Store(1) // fails the scratch header put
	d.St = wst
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@weekly")}
	slot := at("2026-03-10T00:00:00Z")
	if _, err := d.composeFull(noCtx(), "o/r", s, slot, base, packPath); err == nil || !strings.Contains(err.Error(), "scratch put") {
		t.Fatalf("err = %v", err)
	}
	// composeFull: list CAS fails after a successful compose.
	failList := &wave4Store{ObjectStore: store.NewMemory()}
	if _, err := failList.Put(noCtx(), base.Key, store.PutBody{Bytes: []byte("FAKEPACKBYTES")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	failList.listPutErr.Store(1)
	d2 := &Deps{Wal: fw, St: failList, Now: func() time.Time { return slot }}
	if _, err := d2.composeFull(noCtx(), "o/r", s, slot, base, packPath); err == nil || !strings.Contains(err.Error(), "list cas") {
		t.Fatalf("err = %v", err)
	}

	// buildAndPublish: CreateTemp under an unwritable cache dir.
	_, oids := wave4Repo(t)
	fw2 := &fakeWal{asOf: map[time.Time]fold{slot: {refs: tipRefs(oids[2]), seq: 1}}}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o700)
	d3 := &Deps{Wal: fw2, Prim: &fakePrim{}, St: store.NewMemory(), CacheDir: ro,
		HostID: "h", List: protoBundleList(), Now: func() time.Time { return slot }}
	fs := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}
	if err := BuildSlot(noCtx(), d3, "o/r", []Strategy{*fs}, fs, slot); err == nil {
		t.Fatal("unwritable cache dir must fail")
	}
}

func TestWave4ListCASMicro(t *testing.T) {
	// UpsertEntry fills in missing mode/heuristic on an existing bare list.
	st := store.NewMemory()
	if _, err := st.Put(noCtx(), BundleListKey, store.PutBody{Bytes: (&proto.BundleList{}).Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	e := entry("s", "s/1", KindFull, 1, 1, 0)
	if err := UpsertEntry(noCtx(), st, e); err != nil {
		t.Fatal(err)
	}
	l, _, err := storeGetList(t, st)
	if err != nil || l.Mode != "all" || l.Heuristic != "creationToken" {
		t.Fatalf("list = %+v, %v", l, err)
	}
	// RemoveEntries: nothing to remove on an empty store; no-change abort.
	if keys, err := RemoveEntries(noCtx(), store.NewMemory(), []string{"a"}); err != nil || keys != nil {
		t.Fatalf("empty remove = %v, %v", keys, err)
	}
	if err := UpsertEntry(noCtx(), st, e); err != nil {
		t.Fatal(err)
	}
	if keys, err := RemoveEntries(noCtx(), st, []string{"ghost"}); err != nil || keys != nil {
		t.Fatalf("no-change remove = %v, %v", keys, err)
	}
	// RecordVerdicts with nothing to record.
	if err := RecordVerdicts(noCtx(), st, nil); err != nil {
		t.Fatal(err)
	}
	// SettleAndPrune fills mode/heuristic and keeps verdicts whose base
	// resolution errors (blocked incrementals stay recorded).
	strats := []Strategy{
		{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily")},
		{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily"), Keep: 2},
	}
	st2 := store.NewMemory()
	v := &proto.SkippedSlot{Strategy: "inc", Slot: epoch(t, "2026-03-09T00:00:00Z"), BaseID: ""}
	bare := &proto.BundleList{Skipped: []*proto.SkippedSlot{v}} // no mode/heuristic
	if _, err := st2.Put(noCtx(), BundleListKey, store.PutBody{Bytes: bare.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := SettleAndPrune(noCtx(), st2, strats, at("2026-03-10T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	l2, _, err := storeGetList(t, st2)
	if err != nil || l2.Mode != "all" || l2.Heuristic != "creationToken" || len(l2.Skipped) != 1 {
		t.Fatalf("settle list = %+v, %v", l2, err)
	}
}

// binary() with an unbound layer falls back to the environment, and a git
// binary that prints garbage fails the rev-list count parse.
func TestWave4GitBinaryFallbackAndGarbage(t *testing.T) {
	if got := (&GitPrimitives{}).binary(); got == "" {
		t.Fatal("binary() must resolve through the environment")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fakegit")
	body := "#!/bin/sh\ncase \"$1\" in\n  rev-list) echo not-a-number ;;\n  *) echo boom >&2; exit 3 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WALGIT_GIT_BINARY", script)
	p := &GitPrimitives{} // no layer bound → env binary
	if _, err := p.CountCommits(noCtx(), dir, []string{"oid"}, nil); err == nil {
		t.Fatal("garbage rev-list output must fail")
	}
	if err := p.PackDelta(noCtx(), dir, []string{"oid"}, nil, "", &bytes.Buffer{}); err == nil {
		t.Fatal("garbage binary pack-objects must fail")
	}
}
