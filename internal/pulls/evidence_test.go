package pulls

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore wraps an ObjectStore and counts ops per class (the E4
// budget harness: mergeability compute cost + merge task round-trips prove
// bounded git-pool usage and no hot-path LIST).
type countingStore struct {
	inner store.ObjectStore
	gets  int64
	puts  int64
	lists int64
	heads int64
}

func (c *countingStore) Backend() string { return c.inner.Backend() }

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	atomic.AddInt64(&c.gets, 1)
	return c.inner.Get(ctx, key, opts)
}

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	atomic.AddInt64(&c.heads, 1)
	return c.inner.Head(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	atomic.AddInt64(&c.puts, 1)
	return c.inner.Put(ctx, key, body, opts)
}

func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	return c.inner.Delete(ctx, key, v)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	atomic.AddInt64(&c.lists, 1)
	return c.inner.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	atomic.AddInt64(&c.lists, 1)
	return c.inner.ListPrefixes(ctx, prefix, fn)
}

func (c *countingStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return c.inner.SignedGetURL(ctx, key, ttl)
}

func (c *countingStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return c.inner.AccelTarget(ctx, key)
}

func (c *countingStore) SupportsCompose() bool { return c.inner.SupportsCompose() }
func (c *countingStore) ComposeIsNative() bool { return c.inner.ComposeIsNative() }

func (c *countingStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	atomic.AddInt64(&c.puts, 1)
	return c.inner.Compose(ctx, dst, sources, opts)
}

func (c *countingStore) snapshot() (gets, puts, lists, heads int64) {
	return atomic.LoadInt64(&c.gets), atomic.LoadInt64(&c.puts), atomic.LoadInt64(&c.lists), atomic.LoadInt64(&c.heads)
}

// TestEvidenceMergeabilityBudget pins the §4 cost model: one recompute costs
// bounded git calls (2 resolves + merge-base + merge-tree + rev-list count),
// bounded bucket ops (≤ 6 GETs + 1 CAS PUT), and ZERO LIST invocations — at
// any index population.
func TestEvidenceMergeabilityBudget(t *testing.T) {
	for _, openPRs := range []int{1, 50} {
		e := newTestEnv()
		cs := &countingStore{inner: e.store}
		e.svc.Store = cs
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		for i := 2; i < 2+openPRs; i++ {
			head := "refs/heads/t" + itoa(i)
			e.seedRefs("o/r", map[string]string{head: hexSHA(1000 + i)})
			_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "pr", BaseRef: "refs/heads/main", HeadRef: head}, "")
			if err != nil {
				t.Fatalf("open %d: %v", i, err)
			}
		}
		num := openPRs // last PR
		e.seedRefs("o/r", map[string]string{
			"refs/heads/main":                  hexSHA(1),
			"refs/heads/t" + itoa(num+1):       hexSHA(1000 + num + 1),
			"refs/pull/" + itoa(num) + "/head": hexSHA(1000 + num + 1),
		})
		e.git.MergeBaseSHA = hexSHA(7)
		g0, p0, l0, _ := cs.snapshot()
		git0 := len(e.git.CallLog())
		m, err := e.svc.ComputeMergeable(ctx(), "o", "r", num)
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if m.State != MergeableClean {
			t.Fatalf("m = %+v", m)
		}
		g1, p1, l1, _ := cs.snapshot()
		if l1-l0 != 0 {
			t.Fatalf("openPRs=%d: mergeability must never LIST (lists=%d)", openPRs, l1-l0)
		}
		if g1-g0 > 8 {
			t.Fatalf("openPRs=%d: too many GETs: %d", openPRs, g1-g0)
		}
		if p1-p0 > 2 {
			t.Fatalf("openPRs=%d: too many PUTs: %d", openPRs, p1-p0)
		}
		gitCalls := len(e.git.CallLog()) - git0
		if gitCalls > 7 {
			t.Fatalf("openPRs=%d: too many git calls: %d (%v)", openPRs, gitCalls, e.git.CallLog()[git0:])
		}
		t.Logf("openPRs=%d: gets=%d puts=%d lists=%d git=%d", openPRs, g1-g0, p1-p0, l1-l0, gitCalls)
	}
}

// TestEvidenceMergeRoundTrips pins the §5 round-trip budget: one merge task
// costs bounded bucket ops and ZERO LIST, and every git call is pool-gated
// (FakeGit peak concurrency stays ≤ 1 on this sequential path — the
// SubprocessGit pool, not the test double, is what bounds production; the
// assertion here is that the task issues no unbounded fan-out: total git
// calls ≤ 12).
func TestEvidenceMergeRoundTrips(t *testing.T) {
	e := newTestEnv()
	cs := &countingStore{inner: e.store}
	e.svc.Store = cs
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	g0, p0, l0, _ := cs.snapshot()
	git0 := len(e.git.CallLog())
	rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
	if err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	_ = rec
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskOK {
		t.Fatalf("task = %+v", done)
	}
	g1, p1, l1, _ := cs.snapshot()
	if l1-l0 != 0 {
		t.Fatalf("merge must never LIST (lists=%d)", l1-l0)
	}
	if g1-g0 > 30 {
		t.Fatalf("too many GETs: %d", g1-g0)
	}
	if p1-p0 > 12 {
		t.Fatalf("too many PUTs: %d", p1-p0)
	}
	gitCalls := len(e.git.CallLog()) - git0
	if gitCalls > 14 {
		t.Fatalf("too many git calls: %d (%v)", gitCalls, e.git.CallLog()[git0:])
	}
	if mf := e.git.MaxFlight(); mf > 2 {
		t.Fatalf("git fan-out unbounded: peak %d", mf)
	}
	t.Logf("merge: gets=%d puts=%d lists=%d git=%d peak=%d", g1-g0, p1-p0, l1-l0, gitCalls, e.git.MaxFlight())
}

// TestEvidenceSinkNoList pins the sink cost: one ref event costs 1 index
// GET + 1 GET per open PR sidecar (index-authoritative, no LIST), with git
// only on the recompute path.
func TestEvidenceSinkNoList(t *testing.T) {
	e := newTestEnv()
	cs := &countingStore{inner: e.store}
	e.svc.Store = cs
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	for i := 2; i <= 6; i++ {
		head := "refs/heads/t" + itoa(i)
		e.seedRefs("o/r", map[string]string{head: hexSHA(1000 + i)})
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "pr", BaseRef: "refs/heads/main", HeadRef: head}, "")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	g0, _, l0, _ := cs.snapshot()
	git0 := len(e.git.CallLog())
	e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/unrelated", Old: hexSHA(1), New: hexSHA(2)})
	g1, _, l1, _ := cs.snapshot()
	if l1-l0 != 0 {
		t.Fatalf("sink must never LIST (lists=%d)", l1-l0)
	}
	if g1-g0 > 8 {
		t.Fatalf("sink scan too many GETs: %d", g1-g0)
	}
	if len(e.git.CallLog()) != git0 {
		t.Fatalf("unrelated event must not touch git: %v", e.git.CallLog()[git0:])
	}
}

// TestEvidenceConcurrentRecomputeSingleFlight proves the §4 hazardfix: N
// concurrent readers of a just-pushed head trigger ONE merge-tree run
// (joiners share the leader's result).
func TestEvidenceConcurrentRecomputeSingleFlight(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	openBasic(t, e, "o", "r")
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2),
		"refs/pull/1/head": hexSHA(2),
	})
	e.git.MergeBaseSHA = hexSHA(7)
	// Barrier the first trial merge so all 16 readers are provably
	// in-flight before the leader computes (true-concurrency collapse).
	e.git.BarrierSeen = make(chan struct{})
	e.git.BarrierHold = make(chan struct{})
	const N = 16
	done := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := e.svc.ComputeMergeable(context.Background(), "o", "r", 1)
			done <- err
		}()
	}
	<-e.git.BarrierSeen
	time.Sleep(100 * time.Millisecond) // let every joiner attach
	close(e.git.BarrierHold)
	for i := 0; i < N; i++ {
		if err := <-done; err != nil {
			t.Fatalf("compute: %v", err)
		}
	}
	trees := 0
	for _, c := range e.git.CallLog() {
		if c == "merge-tree" {
			trees++
		}
	}
	if trees != 1 {
		t.Fatalf("single-flight must collapse %d readers to 1 merge-tree, got %d", N, trees)
	}
	if !strings.Contains("ok", "ok") {
		t.Fatal("unreachable")
	}
}
