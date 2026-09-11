// servetimeout_test.go — issue #320 regression: the serve-materialization
// wedge. A stalled pack fetch must bound every serve-level Sync (never an
// indefinite hang), mark the serve-health sidecar, leave refs-level syncs
// instant, and resume cleanly once the store answers. Table-driven where
// the setup varies; `-race` mandatory; concurrency paths get the stress
// case at the bottom.
package wal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// hangStore hangs pack-file GETs (the /wal/ objects) while hung, released
// by resume (or by ctx cancel / registry close — it always selects on
// the caller's ctx, so a wedged body can never outlive the test). It
// starts unhung: tests build their fixture first, then call hang().
type hangStore struct {
	store.ObjectStore
	mu   sync.Mutex
	hung bool
	gate chan struct{}
}

func newHangStore(inner store.ObjectStore) *hangStore {
	return &hangStore{ObjectStore: inner, gate: make(chan struct{})}
}

// hang arms the hang (the outage starts after fixture setup — like the
// real wedge, where publishes succeed and serving later stalls).
func (s *hangStore) hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hung = true
}

func (s *hangStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.mu.Lock()
	hung, gate := s.hung, s.gate
	s.mu.Unlock()
	if hung && strings.Contains(key, "/wal/") {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate:
		}
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

// resume releases hung GETs (once).
func (s *hangStore) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hung {
		s.hung = false
		close(s.gate)
	}
}

// serveFixture builds a repo whose manifest lists one tier-0 pack with
// real bytes in the store, then deletes the local copies so the next
// serve Sync must download them (the cold-cache shape). A fake .idx is
// seeded beside the pack (AddPack publishes .pack bytes only).
func serveFixture(t *testing.T, r *Registry, st store.ObjectStore, id string) (h *RepoHandle, checksum string) {
	t.Helper()
	ctx := context.Background()
	hh, err := r.Create(ctx, id, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	checksum = strings.Repeat("c", 40)
	packBytes := []byte("PACK fixture-bytes-for-320")
	src := filepath.Join(t.TempDir(), "pack-"+checksum+".pack")
	if err := os.WriteFile(src, packBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hh.AddPack(ctx, src, checksum, 0, nil); err != nil {
		t.Fatalf("add pack: %v", err)
	}
	if _, err := store.PutBytes(ctx, st, "repos/"+id+"/wal/"+checksum+".idx", []byte("IDX fixture"),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed idx: %v", err)
	}
	// Publish a ref so the repo is non-empty (refs must resolve while
	// the pack fetch hangs).
	if _, err := hh.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("d", 40))}); err != nil {
		t.Fatalf("publish ref: %v", err)
	}
	// Cold serving copy: drop the installed locals so Sync downloads.
	for _, suf := range []string{".pack", ".idx"} {
		_ = os.Remove(filepath.Join(hh.Repo().PackDir(), "pack-"+checksum+suf))
	}
	return hh, checksum
}

// awaitServe runs Sync(LevelServe) and returns the elapsed time — the
// wedge shape is "never returns", so every assertion here is an upper
// bound, generous for CI but fatal for unbounded waits.
func awaitServe(ctx context.Context, h *RepoHandle) (time.Duration, error) {
	start := time.Now()
	g, err := h.Sync(ctx, LevelServe)
	if err == nil {
		g.Release()
	}
	return time.Since(start), err
}

func TestServeSyncWaitBound(t *testing.T) {
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	r.vals.serveSyncTimeout = 300 * time.Millisecond
	r.vals.serveMaterializeTimeout = 10 * time.Second // body outlives the test's wait
	h, _ := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	elapsed, err := awaitServe(context.Background(), h)
	if err == nil {
		t.Fatal("hung pack fetch succeeded, want timeout")
	}
	var we *WalError
	if !errors.As(err, &we) || we.Kind != WalErrTimeout {
		t.Fatalf("err = %v (%T), want WalErrTimeout", err, err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("serve sync took %v with a 300ms bound", elapsed)
	}
	// The failure is sticky: the sidecar names the wedge.
	doc, ok := LoadServeHealth(context.Background(), inner, "acme", "wedge")
	if !ok || doc == nil || doc.Status != ServeHealthDegraded || doc.Reason == "" {
		t.Fatalf("serve-health = %+v %v, want degraded with reason", doc, ok)
	}
	if !h.serveDegradedFlag().Load() {
		t.Fatal("in-handle degraded flag not set")
	}
}

func TestServeRefsStayInstantDuringWedge(t *testing.T) {
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	r.vals.serveSyncTimeout = 300 * time.Millisecond
	r.vals.serveMaterializeTimeout = 10 * time.Second
	h, _ := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	// Start a serve Sync that wedges on the hung pack fetch (background:
	// the leader holds packMu while its body hangs).
	serveDone := make(chan error, 1)
	go func() {
		_, err := awaitServe(context.Background(), h)
		serveDone <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the leader enter the body

	// Ref-level syncs (summary/refs) never touch packMu: instant despite
	// the wedge. This is the issue's exact divergence (refs 200 while
	// objects hang).
	start := time.Now()
	g, err := h.Sync(context.Background(), LevelRefs)
	if err != nil {
		t.Fatalf("refs sync during wedge: %v", err)
	}
	g.Release()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("refs sync took %v during serve wedge", d)
	}
	// And the wedged serve itself stays bounded.
	if err := <-serveDone; err == nil {
		t.Fatal("wedged serve succeeded, want timeout")
	}
}

func TestServeConcurrentJoinersBounded(t *testing.T) {
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	r.vals.serveSyncTimeout = 300 * time.Millisecond
	r.vals.serveMaterializeTimeout = 10 * time.Second
	h, _ := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	// N concurrent serve syncs (the thundering herd behind one wedged
	// materialize): every one returns bounded — none queues forever on
	// packMu or the single-flight join.
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	times := make([]time.Duration, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			times[i], errs[i] = awaitServe(context.Background(), h)
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] == nil {
			t.Fatalf("joiner %d succeeded against a hung store", i)
		}
		var we *WalError
		if !errors.As(errs[i], &we) || we.Kind != WalErrTimeout {
			t.Fatalf("joiner %d err = %v, want WalErrTimeout", i, errs[i])
		}
		if times[i] > 10*time.Second {
			t.Fatalf("joiner %d took %v with a 300ms bound", i, times[i])
		}
	}
}

func TestServeResumeAfterHang(t *testing.T) {
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	r.vals.serveSyncTimeout = 300 * time.Millisecond
	r.vals.serveMaterializeTimeout = 10 * time.Second
	h, checksum := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	if _, err := awaitServe(context.Background(), h); err == nil {
		t.Fatal("hung fetch succeeded, want timeout")
	}
	if _, ok := LoadServeHealth(context.Background(), inner, "acme", "wedge"); !ok {
		t.Fatal("no serve-health marker after timeout")
	}
	// The store answers again: the next serve starts a fresh body,
	// completes, and clears the marker (demand-driven self-heal).
	st.resume()
	elapsed, err := awaitServe(context.Background(), h)
	if err != nil {
		t.Fatalf("serve after resume: %v", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("serve after resume took %v", elapsed)
	}
	if _, ok := LoadServeHealth(context.Background(), inner, "acme", "wedge"); ok {
		t.Fatal("serve-health marker survived a successful serve")
	}
	if h.serveDegradedFlag().Load() {
		t.Fatal("in-handle degraded flag survived a successful serve")
	}
	// The pack landed whole (file-granularity resume, no .tmp orphans).
	data, err := os.ReadFile(filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".pack"))
	if err != nil || string(data) != "PACK fixture-bytes-for-320" {
		t.Fatalf("pack = %q err=%v", data, err)
	}
	if entries, _ := os.ReadDir(h.Repo().PackDir()); entries != nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("orphaned tmp survived: %s", e.Name())
			}
		}
	}
}

func TestServeClientCancelMarksNothing(t *testing.T) {
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	r.vals.serveSyncTimeout = 5 * time.Second
	r.vals.serveMaterializeTimeout = 10 * time.Second
	h, _ := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	// A client disconnect mid-materialize is not a serve failure: the
	// error propagates, but no marker brands the repo.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := awaitServe(ctx, h)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled serve succeeded")
	}
	if _, ok := LoadServeHealth(context.Background(), inner, "acme", "wedge"); ok {
		t.Fatal("client cancel wrote a serve-health marker")
	}
	if h.serveDegradedFlag().Load() {
		t.Fatal("client cancel set the degraded flag")
	}
}

func TestServeMaterializeBodyCap(t *testing.T) {
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	// Generous wait, tiny body cap: the Sync returns when the BODY dies
	// (not when the wait fires), and the wedged body is really gone —
	// the next serve starts fresh instead of joining a ghost.
	r.vals.serveSyncTimeout = 30 * time.Second
	r.vals.serveMaterializeTimeout = 300 * time.Millisecond
	h, _ := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	start := time.Now()
	_, err := awaitServe(context.Background(), h)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung body succeeded, want failure")
	}
	var we *WalError
	if !errors.As(err, &we) || we.Kind != WalErrTimeout {
		t.Fatalf("body-cap err = %v (%T), want WalErrTimeout passthrough", err, err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("body-cap failure took %v with a 300ms cap", elapsed)
	}
	if _, ok := LoadServeHealth(context.Background(), inner, "acme", "wedge"); !ok {
		t.Fatal("no serve-health marker after body-cap kill")
	}
	// The body is terminal (failed record), not lingering: unblock and
	// the next serve completes on a fresh body.
	foundTerminal := false
	for _, rec := range r.Tasks().List("acme/wedge") {
		if rec != nil && rec.Kind == "materialize" && rec.OK != nil && !*rec.OK {
			foundTerminal = true
		}
	}
	if !foundTerminal {
		t.Fatal("no terminal materialize record after body-cap kill")
	}
	st.resume()
	if _, err := awaitServe(context.Background(), h); err != nil {
		t.Fatalf("serve after body-cap kill + resume: %v", err)
	}
	if _, ok := LoadServeHealth(context.Background(), inner, "acme", "wedge"); ok {
		t.Fatal("marker survived post-cap recovery")
	}
}

// TestServeWedgeStress hammers the pack phase from N goroutines against
// a hung store: every waiter returns bounded (deadlock canary — run
// with -count=10 on touched paths).
func TestServeWedgeStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress: full tier only")
	}
	inner := store.NewMemory()
	st := newHangStore(inner)
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), st, cfg)
	defer r.Close()
	r.vals.serveSyncTimeout = 200 * time.Millisecond
	r.vals.serveMaterializeTimeout = 30 * time.Second
	h, _ := serveFixture(t, r, inner, "acme/wedge")
	st.hang()

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lvl := LevelServe
			if i%2 == 1 {
				lvl = LevelRefs // refs must never queue behind the wedge
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			g, err := h.Sync(ctx, lvl)
			if err != nil {
				return
			}
			g.Release()
		}(i)
	}
	wg.Wait()
}

// TestServeZeroConfigKeepsDefaults pins the fail-safe: a zero config
// (non-positive timeouts) falls back to the compiled defaults — there
// is no unbounded serve wait to configure by accident.
func TestServeZeroConfigKeepsDefaults(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.ServeSyncTimeout = 0
	cfg.Server.ServeMaterializeTimeout = 0
	r := NewRegistry(context.Background(), store.NewMemory(), cfg)
	defer r.Close()
	if r.vals.serveSyncTimeout != DefaultServeSyncTimeout {
		t.Fatalf("serveSyncTimeout = %v, want default %v", r.vals.serveSyncTimeout, DefaultServeSyncTimeout)
	}
	if r.vals.serveMaterializeTimeout != DefaultServeMaterializeTimeout {
		t.Fatalf("serveMaterializeTimeout = %v, want default %v", r.vals.serveMaterializeTimeout, DefaultServeMaterializeTimeout)
	}
}
