// harness_test.go — short-mode unit tests for the sim harness itself
// (pure helpers, oracle edge cases, lease ladder, crash recovery). These run
// in `make test`; the TestSim_ scenarios above run only in `make sim`.
package sim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/fault"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newFaultLink builds a throwaway link over a private memory store (no
// cluster needed) for link-level unit tests.
func newFaultLink(name string) *fault.FaultStore {
	return fault.New(store.NewMemory(), name, 1)
}

func denyPlan(keys ...string) fault.Plan { return fault.Plan{DenyKeys: keys} }

func seedName(seed uint64) string { return fmt.Sprintf("seed-%d", seed) }

func TestSeedName(t *testing.T) {
	if got := seedName(22); got != "seed-22" {
		t.Fatalf("seedName = %q", got)
	}
}

func TestParseSeedList(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []uint64
		wantErr bool
	}{
		{"single", "22", []uint64{22}, false},
		{"plural", "21,22,23", []uint64{21, 22, 23}, false},
		{"spaces", " 7 , 8 ", []uint64{7, 8}, false},
		{"empty", "", nil, true},
		{"empty entry", "22,,23", nil, true},
		{"non-number", "abc", nil, true},
		{"negative", "-1", nil, true},
		{"overflow", "18446744073709551616", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSeedList(tc.in)
			if tc.wantErr != (err != nil) {
				t.Fatalf("parseSeedList(%q) err = %v", tc.in, err)
			}
			if err == nil && fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tc.want) {
				t.Fatalf("parseSeedList(%q) = %v (want %v)", tc.in, got, tc.want)
			}
		})
	}
}

func TestSimSeeds(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("WALHUB_SIM_SEED", "")
		t.Setenv("WALHUB_SIM_SEEDS", "")
		got, err := simSeeds()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", defaultSeeds) {
			t.Fatalf("default seeds = %v (want %v)", got, defaultSeeds)
		}
	})
	t.Run("single", func(t *testing.T) {
		t.Setenv("WALHUB_SIM_SEED", "99")
		t.Setenv("WALHUB_SIM_SEEDS", "")
		got, err := simSeeds()
		if err != nil || len(got) != 1 || got[0] != 99 {
			t.Fatalf("single seed = %v err=%v", got, err)
		}
	})
	t.Run("plural wins", func(t *testing.T) {
		t.Setenv("WALHUB_SIM_SEED", "99")
		t.Setenv("WALHUB_SIM_SEEDS", "1,2")
		got, err := simSeeds()
		if err != nil || fmt.Sprintf("%v", got) != "[1 2]" {
			t.Fatalf("plural seeds = %v err=%v", got, err)
		}
	})
	t.Run("bad", func(t *testing.T) {
		t.Setenv("WALHUB_SIM_SEED", "nope")
		t.Setenv("WALHUB_SIM_SEEDS", "")
		if _, err := simSeeds(); err == nil {
			t.Fatal("bad seed env accepted")
		}
	})
}

func TestLinkSeed(t *testing.T) {
	if linkSeed(22, 0) == linkSeed(22, 1) {
		t.Fatal("link seeds collide across instance indexes")
	}
	if linkSeed(22, 0) == linkSeed(23, 0) {
		t.Fatal("link seeds collide across run seeds")
	}
	if linkSeed(22, 0) != linkSeed(22, 0) {
		t.Fatal("link seeds not deterministic")
	}
}

func TestOidFor(t *testing.T) {
	a, b := oidFor(7, 0, 0), oidFor(7, 0, 1)
	if len(a) != 40 || len(b) != 40 || a == b {
		t.Fatalf("oids not distinct 40-hex: %q %q", a, b)
	}
	if strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("oid not hex: %q", a)
	}
	if strings.Trim(a, "0") == "" {
		t.Fatal("oid must never be the zero oid")
	}
}

func TestRepoManifestKey(t *testing.T) {
	if got := repoManifestKey("acme/api"); got != "repos/acme/api/manifest.pb" {
		t.Fatalf("manifest key = %q", got)
	}
}

func TestVerifySegments(t *testing.T) {
	seg := func(key string, first, last uint64) *proto.LogSegmentRef {
		return &proto.LogSegmentRef{Key: key, FirstSeq: first, LastSeq: last}
	}
	cp := func(seq uint64) *proto.CheckpointRef { return &proto.CheckpointRef{Seq: seq} }
	cases := []struct {
		name string
		m    *proto.Manifest
		want int // number of problems
	}{
		{"nil", nil, 1},
		{"empty at zero", &proto.Manifest{}, 0},
		{"empty at head", &proto.Manifest{HeadSeq: 2}, 1},
		{"single", &proto.Manifest{HeadSeq: 1, LogSegments: []*proto.LogSegmentRef{seg("a", 1, 1)}}, 0},
		{"contiguous", &proto.Manifest{HeadSeq: 3, LogSegments: []*proto.LogSegmentRef{seg("a", 1, 2), seg("b", 3, 3)}}, 0},
		{"burn gap allowed", &proto.Manifest{HeadSeq: 3, LogSegments: []*proto.LogSegmentRef{seg("a", 1, 1), seg("b", 3, 3)}}, 0},
		{"overlap", &proto.Manifest{HeadSeq: 3, LogSegments: []*proto.LogSegmentRef{seg("a", 1, 2), seg("b", 2, 3)}}, 1},
		{"head beyond newest", &proto.Manifest{HeadSeq: 5, LogSegments: []*proto.LogSegmentRef{seg("a", 1, 3)}}, 1},
		{"inverted", &proto.Manifest{HeadSeq: 1, LogSegments: []*proto.LogSegmentRef{seg("a", 3, 1)}}, 1},
		{"checkpoint trimmed", &proto.Manifest{HeadSeq: 5, MinSeq: 4, Checkpoint: cp(3),
			LogSegments: []*proto.LogSegmentRef{seg("b", 4, 5)}}, 0},
		{"checkpoint at head empties", &proto.Manifest{HeadSeq: 3, MinSeq: 4, Checkpoint: cp(3)}, 0},
		{"empty below head with stale checkpoint", &proto.Manifest{HeadSeq: 5, MinSeq: 4, Checkpoint: cp(3)}, 1},
		{"untrimmed below min", &proto.Manifest{HeadSeq: 5, MinSeq: 4, Checkpoint: cp(3),
			LogSegments: []*proto.LogSegmentRef{seg("a", 1, 2), seg("b", 4, 5)}}, 1},
		{"min without checkpoint", &proto.Manifest{HeadSeq: 2, MinSeq: 2,
			LogSegments: []*proto.LogSegmentRef{seg("b", 2, 2)}}, 1},
		{"min checkpoint mismatch", &proto.Manifest{HeadSeq: 5, MinSeq: 9, Checkpoint: cp(3),
			LogSegments: []*proto.LogSegmentRef{seg("b", 4, 5)}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifySegments(tc.m); len(got) != tc.want {
				t.Fatalf("verifySegments = %v (want %d problems)", got, tc.want)
			}
		})
	}
}

func TestCheckRefs(t *testing.T) {
	ok := map[string]string{"refs/heads/main": strings.Repeat("a", 40)}
	if problems := checkRefs(ok, ok); len(problems) != 0 {
		t.Fatalf("matching refs flagged: %v", problems)
	}
	cases := []struct {
		name     string
		got      map[string]string
		expected map[string]string
		want     int
	}{
		{"missing", map[string]string{}, ok, 1},
		{"reverted", map[string]string{"refs/heads/main": strings.Repeat("b", 40)}, ok, 1},
		{"phantom", map[string]string{"refs/heads/main": strings.Repeat("a", 40), "refs/heads/evil": strings.Repeat("c", 40)}, ok, 1},
		{"empty both", map[string]string{}, map[string]string{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if problems := checkRefs(tc.got, tc.expected); len(problems) != tc.want {
				t.Fatalf("checkRefs = %v (want %d)", problems, tc.want)
			}
		})
	}
}

func TestWithCrash(t *testing.T) {
	if _, crashed := withCrash("x", func() error { return nil }); crashed {
		t.Fatal("clean call reported crash")
	}
	sentinel := errors.New("boom")
	if err, crashed := withCrash("x", func() error { return sentinel }); crashed || err != sentinel {
		t.Fatalf("error passthrough: err=%v crashed=%v", err, crashed)
	}
	if err, crashed := withCrash("x", func() error { panic("injected") }); !crashed || err == nil ||
		!strings.Contains(err.Error(), "instance x crashed") {
		t.Fatalf("panic conversion: err=%v crashed=%v", err, crashed)
	}
}

func TestUnwrapAll(t *testing.T) {
	if got := unwrapAll(nil); got != "" {
		t.Fatalf("nil chain = %q", got)
	}
	leaf := errors.New("leaf")
	mid := fmt.Errorf("mid: %w", leaf)
	if got := unwrapAll(leaf); got != "leaf" {
		t.Fatalf("single = %q", got)
	}
	if got := unwrapAll(mid); !strings.Contains(got, "mid") || !strings.Contains(got, "leaf") ||
		!strings.Contains(got, "<-") {
		t.Fatalf("chain = %q", got)
	}
	// A non-unwrapping error stops the walk (StoreError has no Unwrap... it
	// does; use a plain custom type).
	if got := unwrapAll(noUnwrap{"stop"}); got != "stop" {
		t.Fatalf("opaque = %q", got)
	}
}

type noUnwrap struct{ s string }

func (e noUnwrap) Error() string { return e.s }

func TestFailReport(t *testing.T) {
	c := &Cluster{ctx: context.Background(), truth: store.NewMemory()}
	if got := c.failReport("boom %d", 42); !strings.Contains(got, "boom 42") ||
		!strings.Contains(got, "truth manifest unreadable") {
		t.Fatalf("empty report:\n%s", got)
	}
	c2 := &Cluster{ctx: context.Background(), truth: store.NewMemory()}
	link := newFaultLink("x")
	c2.instances = []*Instance{{name: "x", link: link}}
	if got := c2.failReport("here"); !strings.Contains(got, "link x:") ||
		!strings.Contains(got, "here") {
		t.Fatalf("link report:\n%s", got)
	}
}

func TestFailReportHealthy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := setupCluster(t, ctx)
	got := c.failReport("boom")
	for _, want := range []string{"boom", "truth head=1", "log/0000000000000001.pb", "manifest.pb"} {
		if !strings.Contains(got, want) {
			t.Fatalf("healthy report missing %q:\n%s", want, got)
		}
	}
}

func TestAcquireLease(t *testing.T) {
	ctx := context.Background()
	newStore := func(t *testing.T) store.ObjectStore { return store.NewMemory() }

	t.Run("absent creates epoch 0", func(t *testing.T) {
		st := newStore(t)
		if _, err := acquireLease(ctx, st, "leases/a.pb", "me", time.Minute); err != nil {
			t.Fatal(err)
		}
		body, _, err := store.GetBytes(ctx, st, "leases/a.pb", store.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cur := &proto.Lease{}
		if err := cur.Unmarshal(body); err != nil || cur.Holder != "me" || cur.Epoch != 0 {
			t.Fatalf("lease = %+v err=%v", cur, err)
		}
	})
	t.Run("live lease held", func(t *testing.T) {
		st := newStore(t)
		if _, err := acquireLease(ctx, st, "leases/b.pb", "me", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st, "leases/b.pb", "other", time.Minute); err != ErrLeaseHeld {
			t.Fatalf("second acquire = %v (want held)", err)
		}
	})
	t.Run("expired steals with epoch+1", func(t *testing.T) {
		st := newStore(t)
		past := time.Now().UTC().Add(-time.Hour)
		acq, exp := proto.TimeFromGo(past), proto.TimeFromGo(past.Add(time.Minute))
		old := &proto.Lease{Holder: "old", Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 4}
		if _, err := st.Put(ctx, "leases/c.pb", store.PutBody{Bytes: old.Marshal()},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st, "leases/c.pb", "new", time.Minute); err != nil {
			t.Fatal(err)
		}
		body, _, _ := store.GetBytes(ctx, st, "leases/c.pb", store.GetOptions{})
		cur := &proto.Lease{}
		if err := cur.Unmarshal(body); err != nil || cur.Holder != "new" || cur.Epoch != 5 {
			t.Fatalf("stolen lease = %+v err=%v", cur, err)
		}
	})
	t.Run("within skew still held", func(t *testing.T) {
		st := newStore(t)
		// Expired 1s ago: inside the 2s skew tolerance → not stealable.
		then := time.Now().UTC().Add(-time.Second)
		acq, exp := proto.TimeFromGo(then.Add(-time.Minute)), proto.TimeFromGo(then)
		old := &proto.Lease{Holder: "old", Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 0}
		if _, err := st.Put(ctx, "leases/d.pb", store.PutBody{Bytes: old.Marshal()},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st, "leases/d.pb", "new", time.Minute); err != ErrLeaseHeld {
			t.Fatalf("skew-window acquire = %v (want held)", err)
		}
	})
	t.Run("corrupt fails", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.Put(ctx, "leases/e.pb", store.PutBody{Bytes: []byte{0xff, 0xff}},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st, "leases/e.pb", "new", time.Minute); err == nil {
			t.Fatal("corrupt lease acquired")
		}
	})
	t.Run("canceled ctx", func(t *testing.T) {
		st := newStore(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := acquireLease(cctx, st, "leases/f.pb", "new", time.Minute); err == nil {
			t.Fatal("canceled acquire succeeded")
		}
	})
}

func TestDumpTraces(t *testing.T) {
	c := &Cluster{}
	// Empty cluster renders empty (no panic, no links).
	if got := c.DumpTraces(); got != "" {
		t.Fatalf("empty cluster traces = %q", got)
	}
}

func TestDumpTracesWithLink(t *testing.T) {
	c := &Cluster{}
	link := newFaultLink("x")
	c.instances = []*Instance{{name: "x", link: link}}
	link.SetTrace(true)
	ctx := context.Background()
	if _, err := link.Put(ctx, "k", store.PutBody{Bytes: []byte("v")},
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	link.Set(denyPlan("k"))
	if _, err := link.Head(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	got := c.DumpTraces()
	for _, want := range []string{"link x:", "ops=2", "denied"} {
		if !strings.Contains(got, want) {
			t.Fatalf("traces missing %q:\n%s", want, got)
		}
	}
	// TakeTrace swaps: a second dump has stats but no trace lines (the stats
	// summary's "denied=1" counter persists — only the ": denied" trace
	// lines drain).
	if got2 := c.DumpTraces(); strings.Contains(got2, ": denied") {
		t.Fatalf("trace not drained by dump:\n%s", got2)
	}
}

func TestDumpTracesTruncates(t *testing.T) {
	c := &Cluster{ctx: context.Background(), truth: store.NewMemory()}
	link := newFaultLink("busy")
	c.instances = []*Instance{{name: "busy", link: link}}
	link.SetTrace(true)
	link.Set(denyPlan("k"))
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := link.Head(ctx, "k"); err != nil {
			t.Fatal(err)
		}
	}
	got := c.DumpTraces()
	lines := strings.Count(got, ": denied")
	if lines != 40 {
		t.Fatalf("dump kept %d trace lines (want the newest 40)", lines)
	}
}

func TestSimConfig(t *testing.T) {
	cfg := simConfig(t.TempDir())
	if cfg.Cache.Dir == "" {
		t.Fatal("cache dir unset")
	}
	if cfg.WAL.FreshnessTTL != 0 {
		t.Fatalf("sim freshness TTL = %v (want 0: every sync checks)", cfg.WAL.FreshnessTTL)
	}
}

// stubStore is a fault-injecting decorator for oracle/lease error-path unit
// tests: failing Gets/Heads/Lists, one-shot (or N-shot) conditional-PUT
// losses, and hard PUT errors.
type stubStore struct {
	store.ObjectStore
	failGetSubstr   string
	failUpdateOnce  bool
	failUpdateN     int
	failCreateOnce  bool
	failCreateHard  bool
	failUpdateHard  bool
	failGetHardKeys []string
	failHeadKeys    []string
	failList        bool
}

func (s *stubStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.failGetSubstr != "" && strings.Contains(key, s.failGetSubstr) {
		return nil, store.NewRetryable(key, errors.New("stub get failure"))
	}
	for _, k := range s.failGetHardKeys {
		if key == k {
			return nil, store.NewOther(key, errors.New("stub hard get failure"))
		}
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *stubStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	for _, k := range s.failHeadKeys {
		if key == k {
			return nil, store.NewOther(key, errors.New("stub hard head failure"))
		}
	}
	return s.ObjectStore.Head(ctx, key)
}

func (s *stubStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if s.failList {
		return store.NewOther(prefix, errors.New("stub list failure"))
	}
	return s.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (s *stubStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutUpdate {
		if s.failUpdateHard {
			return store.ObjectMeta{}, store.NewOther(key, errors.New("stub hard update failure"))
		}
		if s.failUpdateOnce {
			s.failUpdateOnce = false
			return store.ObjectMeta{}, store.NewPrecondition(key, "stub-winner")
		}
		if s.failUpdateN > 0 {
			s.failUpdateN--
			return store.ObjectMeta{}, store.NewPrecondition(key, "stub-winner")
		}
	}
	if opts.Mode == store.PutCreate {
		if s.failCreateHard {
			return store.ObjectMeta{}, store.NewOther(key, errors.New("stub hard create failure"))
		}
		if s.failCreateOnce {
			s.failCreateOnce = false
			return store.ObjectMeta{}, store.NewPrecondition(key, "stub-winner")
		}
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func TestPushRefFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("cap exhausted", func(t *testing.T) {
		t.Setenv("WALHUB_SIM_VERBOSE", "1")
		c := newCluster(t, defaultRepo)
		a := c.AddInstance("a", 50)
		wrong, other := strings.Repeat("9", 40), strings.Repeat("8", 40)
		if _, err := a.PushRef(ctx, "refs/heads/main", wrong, other); err == nil ||
			!strings.Contains(err.Error(), "did not converge") {
			t.Fatalf("wrong-old push = %v (want non-convergence)", err)
		}
		if _, ok := a.LocalRef("refs/heads/nope"); ok {
			t.Fatal("absent local ref reported present")
		}
		// Corrupt the local view on disk: snapshot reads fail cleanly.
		if err := os.RemoveAll(a.h.Repo().Path); err != nil {
			t.Fatal(err)
		}
		if _, ok := a.LocalRef("refs/heads/main"); ok {
			t.Fatal("unreadable local ref reported present")
		}
	})
	t.Run("canceled ctx", func(t *testing.T) {
		c := newCluster(t, defaultRepo)
		a := c.AddInstance("a", 51)
		cctx, stop := context.WithCancel(ctx)
		stop()
		if _, err := a.PushRef(cctx, "refs/heads/main", strings.Repeat("9", 40), strings.Repeat("8", 40)); err == nil {
			t.Fatal("canceled push succeeded")
		}
	})
	t.Run("nil handle crash", func(t *testing.T) {
		// A nil handle panics inside Publish; the boundary converts it to a
		// synthetic crash and PushRef aborts (covers the crash plumbing
		// without needing a wedged store).
		in := &Instance{name: "ghost"}
		if _, err, crashed := in.Publish(ctx, wal.PublishRequest{}); !crashed || err == nil {
			t.Fatalf("nil-handle publish: err=%v crashed=%v", err, crashed)
		}
		if _, err := in.PushRef(ctx, "refs/heads/main", zeroOid(), oidFor(1, 0, 0)); err == nil ||
			!strings.Contains(err.Error(), "crashed") {
			t.Fatalf("nil-handle push = %v (want crash abort)", err)
		}
	})
	t.Run("verbose transient", func(t *testing.T) {
		t.Setenv("WALHUB_SIM_VERBOSE", "1")
		c := newCluster(t, defaultRepo)
		a := c.AddInstance("a", 53)
		a.link.Set(blackHolePlan())
		sctx, stop := context.WithTimeout(ctx, 300*time.Millisecond)
		defer stop()
		if _, err := a.PushRef(sctx, "refs/heads/main", zeroOid(), oidFor(53, 0, 0)); err == nil {
			t.Fatal("black-holed push succeeded")
		}
	})
	t.Run("publish panic wedges handle, restart recovers", func(t *testing.T) {
		c := newCluster(t, defaultRepo)
		a := c.AddInstance("a", 52)
		if _, err := a.PushRef(ctx, "refs/heads/main", zeroOid(), oidFor(52, 0, 0)); err != nil {
			t.Fatal(err)
		}
		// The panic fires inside the publisher's publish-time Sync while it
		// holds syncMu; runBatch recovers into a batch error but the mutex
		// stays locked — the handle is wedged (process-death semantics) and
		// every later op hangs until the caller's context gives up.
		a.link.Set(panicPlan("get:manifest.pb"))
		sctx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if _, err := a.PushRef(sctx, "refs/heads/main", oidFor(52, 0, 0), oidFor(52, 0, 1)); err == nil {
			t.Fatal("push through a wedged handle succeeded")
		}
		a.Restart(c)
		a.Heal()
		if _, err := a.PushRef(ctx, "refs/heads/main", oidFor(52, 0, 0), oidFor(52, 0, 1)); err != nil {
			t.Fatalf("post-restart push: %v", err)
		}
	})
}

func panicPlan(patterns ...string) fault.Plan { return fault.Plan{PanicOnceKeys: patterns} }

func blackHolePlan() fault.Plan { return fault.BlackHole() }

// setupCluster builds a cluster with one committed ref (shared fixture for
// oracle failure-path tests).
func setupCluster(t *testing.T, ctx context.Context) *Cluster {
	t.Helper()
	c := newCluster(t, defaultRepo)
	a := c.AddInstance("a", 60)
	if _, err := a.PushRef(ctx, "refs/heads/main", zeroOid(), oidFor(60, 0, 0)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckTruthFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	setup := func(t *testing.T) *Cluster { return setupCluster(t, ctx) }
	t.Run("ref mismatch", func(t *testing.T) {
		c := setup(t)
		if err := c.CheckTruth(map[string]string{"refs/heads/main": oidFor(60, 0, 1)}); err == nil ||
			!strings.Contains(err.Error(), "want") {
			t.Fatalf("mismatched truth = %v", err)
		}
	})
	t.Run("phantom ref", func(t *testing.T) {
		c := setup(t)
		if err := c.CheckTruth(map[string]string{}); err == nil ||
			!strings.Contains(err.Error(), "unexpected ref") {
			t.Fatalf("phantom truth = %v", err)
		}
	})
	t.Run("missing segment", func(t *testing.T) {
		c := setup(t)
		m, err := c.TruthManifest()
		if err != nil || len(m.LogSegments) == 0 {
			t.Fatalf("setup manifest: %+v %v", m, err)
		}
		segKey := "repos/" + defaultRepo + "/" + m.LogSegments[0].Key
		if err := c.truth.Delete(ctx, segKey, ""); err != nil {
			t.Fatal(err)
		}
		if err := c.CheckTruth(map[string]string{"refs/heads/main": oidFor(60, 0, 0)}); err == nil ||
			!strings.Contains(err.Error(), "listed but absent") {
			t.Fatalf("missing-segment truth = %v", err)
		}
	})
	t.Run("verifier sync fails", func(t *testing.T) {
		c := setup(t)
		c.truth = &stubStore{ObjectStore: c.truth, failGetSubstr: "log/"}
		if err := c.CheckTruth(map[string]string{"refs/heads/main": oidFor(60, 0, 0)}); err == nil ||
			!strings.Contains(err.Error(), "verifier sync") {
			t.Fatalf("broken-tail truth = %v", err)
		}
	})
}

func TestTruthManifestAbsent(t *testing.T) {
	c := newCluster(t, "nope/none")
	if _, err := c.TruthManifest(); err == nil {
		t.Fatal("absent truth manifest read clean")
	}
}

func TestTruthManifestErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Run("store error", func(t *testing.T) {
		c := newCluster(t, defaultRepo)
		c.truth = &stubStore{ObjectStore: c.truth, failGetHardKeys: []string{repoManifestKey(defaultRepo)}}
		if _, err := c.TruthManifest(); err == nil {
			t.Fatal("failed manifest read clean")
		}
		if err := c.CheckTruth(map[string]string{}); err == nil {
			t.Fatal("CheckTruth over unreadable manifest clean")
		}
	})
	t.Run("corrupt bytes", func(t *testing.T) {
		c := newCluster(t, defaultRepo)
		if _, err := c.truth.Put(ctx, repoManifestKey(defaultRepo), store.PutBody{Bytes: []byte{0xff, 0xfe}},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.TruthManifest(); err == nil {
			t.Fatal("corrupt manifest decoded")
		}
	})
	t.Run("verifier open fails", func(t *testing.T) {
		c := setupCluster(t, ctx)
		// Point the verifier's cache root at a file: git init fails there.
		blocker := filepath.Join(c.root, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.root = blocker
		if err := c.CheckTruth(map[string]string{"refs/heads/main": oidFor(60, 0, 0)}); err == nil ||
			!strings.Contains(err.Error(), "verifier open") {
			t.Fatalf("broken verifier = %v", err)
		}
	})
	t.Run("segment head fails", func(t *testing.T) {
		c := setupCluster(t, ctx)
		m, err := c.TruthManifest()
		if err != nil || len(m.LogSegments) == 0 {
			t.Fatalf("setup manifest: %+v %v", m, err)
		}
		segKey := "repos/" + defaultRepo + "/" + m.LogSegments[0].Key
		c.truth = &stubStore{ObjectStore: c.truth, failHeadKeys: []string{segKey}}
		if err := c.CheckTruth(map[string]string{"refs/heads/main": oidFor(60, 0, 0)}); err == nil ||
			!strings.Contains(err.Error(), "HEAD error") {
			t.Fatalf("broken HEAD = %v", err)
		}
	})
	t.Run("key census fails", func(t *testing.T) {
		c := &Cluster{ctx: context.Background(), truth: &stubStore{ObjectStore: store.NewMemory(), failList: true}}
		if got := c.keySummary(); !strings.Contains(got, "list-error") {
			t.Fatalf("census = %q", got)
		}
	})
}

func TestAcquireLeaseRaces(t *testing.T) {
	ctx := context.Background()
	t.Run("create race retries to success", func(t *testing.T) {
		st := &stubStore{ObjectStore: store.NewMemory(), failCreateOnce: true}
		// Absent on first read; the first Create loses (412); the retry's
		// re-read is still absent and the second Create wins.
		if _, err := acquireLease(ctx, st, "leases/r.pb", "racer", time.Minute); err != nil {
			t.Fatalf("create race = %v (want win after retry)", err)
		}
		body, _, err := store.GetBytes(ctx, st, "leases/r.pb", store.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cur := &proto.Lease{}
		if err := cur.Unmarshal(body); err != nil || cur.Holder != "racer" {
			t.Fatalf("raced lease = %+v err=%v", cur, err)
		}
	})
	t.Run("update race retries to success", func(t *testing.T) {
		st := &stubStore{ObjectStore: store.NewMemory(), failUpdateOnce: true}
		past := time.Now().UTC().Add(-time.Hour)
		acq, exp := proto.TimeFromGo(past), proto.TimeFromGo(past.Add(time.Minute))
		old := &proto.Lease{Holder: "old", Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 0}
		if _, err := st.ObjectStore.Put(ctx, "leases/u.pb", store.PutBody{Bytes: old.Marshal()},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st, "leases/u.pb", "new", time.Minute); err != nil {
			t.Fatalf("update race = %v (want steal after retry)", err)
		}
	})
	t.Run("endless races give up held", func(t *testing.T) {
		st := &stubStore{ObjectStore: store.NewMemory(), failUpdateN: 100}
		past := time.Now().UTC().Add(-time.Hour)
		acq, exp := proto.TimeFromGo(past), proto.TimeFromGo(past.Add(time.Minute))
		old := &proto.Lease{Holder: "old", Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 0}
		if _, err := st.ObjectStore.Put(ctx, "leases/x.pb", store.PutBody{Bytes: old.Marshal()},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st, "leases/x.pb", "new", time.Minute); err != ErrLeaseHeld {
			t.Fatalf("endless race = %v (want held after ladder exhausts)", err)
		}
	})
	t.Run("hard errors surface", func(t *testing.T) {
		st := &stubStore{ObjectStore: store.NewMemory(), failCreateHard: true}
		if _, err := acquireLease(ctx, st, "leases/h1.pb", "me", time.Minute); err == nil {
			t.Fatal("hard create error swallowed")
		}
		st2 := &stubStore{ObjectStore: store.NewMemory(), failGetHardKeys: []string{"leases/h2.pb"}}
		if _, err := acquireLease(ctx, st2, "leases/h2.pb", "me", time.Minute); err == nil {
			t.Fatal("hard get error swallowed")
		}
		past := time.Now().UTC().Add(-time.Hour)
		acq, exp := proto.TimeFromGo(past), proto.TimeFromGo(past.Add(time.Minute))
		old := &proto.Lease{Holder: "old", Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 0}
		st3 := &stubStore{ObjectStore: store.NewMemory(), failUpdateHard: true}
		if _, err := st3.ObjectStore.Put(ctx, "leases/h3.pb", store.PutBody{Bytes: old.Marshal()},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(ctx, st3, "leases/h3.pb", "me", time.Minute); err == nil {
			t.Fatal("hard update error swallowed")
		}
	})
}

func zeroOid() string { return strings.Repeat("0", 40) }
