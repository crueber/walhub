// fanout_bound_test.go — issue #153: fan-out goroutines are bounded by
// construction (slots acquired BEFORE spawning). Each subtest parks a
// large fan-out on a gate with exactly FanoutParallel workers inside the
// work function, then asserts the process-wide goroutine delta stays
// small. Pre-fix (spawn-then-acquire) every subtest fails: one goroutine
// per recipient/hook spawns before any throttling applies.
//
// Determinism note: nothing here samples timing races. The gate holds
// workers inside the work function until FanoutParallel are observably
// in flight; only then is the goroutine count sampled. The bound (40)
// is far above the fixed code's footprint (~8 workers + test/harness
// slack) and far below the unfixed footprint (N workers).
package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
)

// fanoutGoroutineBound caps the process-wide goroutine delta observed
// while a large fan-out is parked with FanoutParallel workers in flight.
// Fixed code needs ~FanoutParallel workers plus harness slack; unfixed
// code needs one goroutine per recipient/hook (N ≥ 64 below).
const fanoutGoroutineBound = 40

// arrivalGate parks workers inside a work function until released.
// open is idempotent: tests defer it right after construction so every
// failure path still drains parked workers (a closed gate would hang
// deferred cleanup such as httptest Close).
type arrivalGate struct {
	release  chan struct{}
	inflight atomic.Int32
	once     sync.Once
}

func newArrivalGate() *arrivalGate { return &arrivalGate{release: make(chan struct{})} }

func (g *arrivalGate) arrive() {
	g.inflight.Add(1)
	<-g.release
}

// waitFor fails the test unless n workers arrive before the deadline.
func (g *arrivalGate) waitFor(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for g.inflight.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("gate: %d workers in flight, want %d", g.inflight.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *arrivalGate) open() { g.once.Do(func() { close(g.release) }) }

// settledGoroutines samples the process-wide count once background
// activity from earlier tests has drained.
func settledGoroutines() int {
	prev := runtime.NumGoroutine()
	for range 40 {
		time.Sleep(10 * time.Millisecond)
		if cur := runtime.NumGoroutine(); cur <= prev {
			return cur
		} else {
			prev = cur
		}
	}
	return prev
}

func maxGoroutines(samples int) int {
	max := 0
	for range samples {
		if n := runtime.NumGoroutine(); n > max {
			max = n
		}
		time.Sleep(5 * time.Millisecond)
	}
	return max
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish after gate opened", what)
	}
}

// gatePutStore blocks every Put on the gate: createOne workers park
// inside their first Create while the parent must not spawn beyond the
// acquired slots.
type gatePutStore struct {
	store.ObjectStore
	g *arrivalGate
}

func (s *gatePutStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	s.g.arrive()
	return s.ObjectStore.Put(ctx, key, body, opts)
}

// gateProfiles blocks mention validation: resolve workers park inside
// validPrincipal while the parent must not spawn beyond the slots.
type gateProfiles struct{ g *arrivalGate }

func (p *gateProfiles) GetProfile(_ context.Context, principal string) (*identity.Profile, error) {
	p.g.arrive()
	return &identity.Profile{Principal: principal}, nil
}

// TestFanoutCreateAllBounded parks 200 notification Creates on the gate
// (emit sync path — emit.go createAll) and bounds the goroutine delta.
func TestFanoutCreateAllBounded(t *testing.T) {
	x := newHarness(t)
	g := newArrivalGate()
	defer g.open() // drain parked workers on any failure path
	x.svc.Store = &gatePutStore{ObjectStore: x.svc.Store, g: g}

	const n = 200
	targets := make([]target, 0, n)
	for i := range n {
		targets = append(targets, target{principal: fmt.Sprintf("user%04d", i), reason: ReasonMentioned})
	}
	base := settledGoroutines()
	done := make(chan struct{})
	var created []completed
	var failed bool
	go func() {
		defer close(done)
		created, failed = x.svc.createAll(ctx(), "acme", "repo",
			Emission{Repo: "acme/repo", Num: 7, Kind: "issue"},
			"T", "actor", x.now.Format(dateTimeFmt), 1, targets)
	}()
	g.waitFor(t, FanoutParallel)
	// Open BEFORE asserting: on failure the parked workers must still
	// drain, or deferred cleanup (httptest Close) hangs on the gate.
	delta := maxGoroutines(10) - base
	g.open()
	if delta > fanoutGoroutineBound {
		t.Fatalf("createAll: goroutine delta = %d, want <= %d (200 recipients, %d slots)",
			delta, fanoutGoroutineBound, FanoutParallel)
	}
	waitDone(t, done, "createAll")
	if failed {
		t.Fatal("createAll: failed = true, want all 200 targets created")
	}
	if len(created) != n {
		t.Fatalf("createAll: created = %d, want %d", len(created), n)
	}
}

// TestFanoutResolveBounded parks 200 mention probes on the gate
// (emit.go resolve) and bounds the goroutine delta.
func TestFanoutResolveBounded(t *testing.T) {
	x := newHarness(t)
	g := newArrivalGate()
	defer g.open() // drain parked workers on any failure path
	x.svc.Profiles = &gateProfiles{g: g}

	const n = 200
	recips := make([]string, 0, n)
	for i := range n {
		recips = append(recips, fmt.Sprintf("user%04d", i))
	}
	base := settledGoroutines()
	type outcome struct{ tgts []target }
	resCh := make(chan outcome, 1)
	go func() {
		resCh <- outcome{x.svc.resolve(ctx(), "acme", "repo",
			Emission{Repo: "acme/repo", Class: "mentioned", Recipients: recips},
			"actor", "", nil)}
	}()
	g.waitFor(t, FanoutParallel)
	// Open BEFORE asserting: on failure the parked workers must still
	// drain, or deferred cleanup hangs on the gate.
	delta := maxGoroutines(10) - base
	g.open()
	if delta > fanoutGoroutineBound {
		t.Fatalf("resolve: goroutine delta = %d, want <= %d (200 recipients, %d slots)",
			delta, fanoutGoroutineBound, FanoutParallel)
	}
	var out outcome
	select {
	case out = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatal("resolve did not finish after gate opened")
	}
	if len(out.tgts) != n {
		t.Fatalf("resolve: targets = %d, want %d", len(out.tgts), n)
	}
}

// TestFanoutTaskBounded parks 200 backfill Creates on the gate
// (tasks.go fanoutOne) and bounds the goroutine delta.
func TestFanoutTaskBounded(t *testing.T) {
	x := newHarness(t)

	const n = 200
	targets := make([]target, 0, n)
	for i := range n {
		targets = append(targets, target{principal: fmt.Sprintf("user%04d", i), reason: ReasonMentioned})
	}
	seq, err := x.svc.reserveSeq(ctx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.svc.appendActivity(ctx(), "acme", "repo", seq,
		Emission{Repo: "acme/repo", Num: 7, Kind: "issue", Class: "mentioned"},
		"mentioned", "T", "actor", x.now.Format(dateTimeFmt), targets, false); err != nil {
		t.Fatal(err)
	}
	// Gate only the measured fan-out: setup above runs ungated.
	g := newArrivalGate()
	defer g.open() // drain parked workers on any failure path
	x.svc.Store = &gatePutStore{ObjectStore: x.svc.Store, g: g}

	base := settledGoroutines()
	type outcome struct {
		existed, complete bool
	}
	resCh := make(chan outcome, 1)
	go func() {
		existed, complete := x.svc.fanoutOne(ctx(), "acme", "repo", "acme/repo", seq)
		resCh <- outcome{existed, complete}
	}()
	g.waitFor(t, FanoutParallel)
	// Open BEFORE asserting: on failure the parked workers must still
	// drain, or deferred cleanup hangs on the gate.
	delta := maxGoroutines(10) - base
	g.open()
	if delta > fanoutGoroutineBound {
		t.Fatalf("fanoutOne: goroutine delta = %d, want <= %d (200 recipients, %d slots)",
			delta, fanoutGoroutineBound, FanoutParallel)
	}
	var out outcome
	select {
	case out = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatal("fanoutOne did not finish after gate opened")
	}
	if !out.existed || !out.complete {
		t.Fatalf("fanoutOne: existed=%v complete=%v, want true true", out.existed, out.complete)
	}
}

// TestFanoutDeliverRepoBounded parks 64 hook deliveries on a gated sink
// (webhooks.go DeliverRepo) and bounds the goroutine delta.
func TestFanoutDeliverRepoBounded(t *testing.T) {
	x := newHarness(t)
	g := newArrivalGate()
	defer g.open() // drain parked workers on any failure path
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.arrive()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const nhooks = 64
	x.addProfile("amy@example.com", "bob@example.com")
	for range nhooks {
		if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
			URL: strPtr(srv.URL), Events: []string{"commented"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One activity event, appended directly: EmitIssue would wake the
	// delivery loop and consume the gate before the measured pass.
	seq, err := x.svc.reserveSeq(ctx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.svc.appendActivity(ctx(), "acme", "repo", seq,
		Emission{Repo: "acme/repo", Num: 7, Kind: "issue", Class: "subscribed"},
		"commented", "T", "bob@example.com", x.now.Format(dateTimeFmt), nil, false); err != nil {
		t.Fatal(err)
	}

	base := settledGoroutines()
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.svc.DeliverRepo(ctx(), "acme", "repo")
	}()
	g.waitFor(t, FanoutParallel)
	// Open BEFORE asserting: on failure the parked handlers must still
	// drain, or the deferred srv.Close hangs on the gate.
	delta := maxGoroutines(10) - base
	g.open()
	if delta > fanoutGoroutineBound {
		t.Fatalf("DeliverRepo: goroutine delta = %d, want <= %d (%d hooks, %d slots)",
			delta, fanoutGoroutineBound, nhooks, FanoutParallel)
	}
	waitDone(t, done, "DeliverRepo")
}
