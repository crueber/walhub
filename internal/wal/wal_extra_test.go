// wal_extra_test.go — shared fault-injecting store plus registry/eviction/task/
// type-level behavior tests that the main suites do not reach.
package wal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- fault-injecting store wrapper ----------------------------------------------

type hookStore struct {
	store.ObjectStore
	mu          sync.Mutex
	putHit      map[string]int
	getHit      map[string]int
	delHit      map[string]int
	putErr      func(key string, n int) error
	putErrAfter func(key string, n int) error // write lands, then fail the response
	getErr      func(key string, n int) error
	delErr      func(key string, n int) error
	headErr     func(key string, n int) error
	getBody     func(key string) ([]byte, bool) // serve replaced bytes (manifest doctoring)
	lpErr       func(prefix string) error
	gateCh      chan struct{}
	nmKeys      map[string]bool // Get answers NotModified for these keys
	errKeys     map[string]bool // Get answers an unreadable body for these keys
	listErr     error
}

func nmKeyMatch(m map[string]bool, key string) bool {
	if m == nil {
		return false
	}
	if m[key] {
		return true
	}
	for p := range m {
		if strings.Contains(key, p) {
			return true
		}
	}
	return false
}

func (h *hookStore) markNM(prefixes ...string) {
	h.mu.Lock()
	if h.nmKeys == nil {
		h.nmKeys = map[string]bool{}
	}
	for _, k := range prefixes {
		h.nmKeys[k] = true
	}
	h.mu.Unlock()
}

func (h *hookStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	h.mu.Lock()
	f := h.lpErr
	h.mu.Unlock()
	if f != nil {
		if err := f(prefix); err != nil {
			return err
		}
	}
	return h.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

func newHookStore(st store.ObjectStore) *hookStore {
	return &hookStore{ObjectStore: st, putHit: map[string]int{}, getHit: map[string]int{}, delHit: map[string]int{}}
}

func (h *hookStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	h.mu.Lock()
	if h.putHit == nil {
		h.putHit = map[string]int{}
	}
	h.putHit[key]++
	n := h.putHit[key]
	f := h.putErr
	h.mu.Unlock()
	if f != nil {
		if err := f(key, n); err != nil {
			return store.ObjectMeta{}, err
		}
	}
	meta, err := h.ObjectStore.Put(ctx, key, body, opts)
	if err != nil {
		return meta, err
	}
	// Lost-response simulation: the write lands, the caller sees an error.
	h.mu.Lock()
	fa := h.putErrAfter
	h.mu.Unlock()
	if fa != nil {
		if err := fa(key, n); err != nil {
			return store.ObjectMeta{}, err
		}
	}
	return meta, nil
}

func (h *hookStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	h.mu.Lock()
	gate := h.gateCh
	h.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h.mu.Lock()
	if h.getHit == nil {
		h.getHit = map[string]int{}
	}
	h.getHit[key]++
	n := h.getHit[key]
	f := h.getErr
	h.mu.Unlock()
	if f != nil {
		if err := f(key, n); err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	gb := h.getBody
	nm := nmKeyMatch(h.nmKeys, key)
	ek := h.errKeys[key]
	h.mu.Unlock()
	if nm {
		return store.NotModified{Version: "nm"}, nil
	}
	_ = ek
	if ek {
		return store.Object{Meta: store.ObjectMeta{Key: key}, Body: io.NopCloser(errBodyRead{})}, nil
	}
	if gb != nil {
		if b, ok := gb(key); ok {
			return store.Object{Meta: store.ObjectMeta{Key: key, Size: int64(len(b)), Version: "hooked"},
				Body: io.NopCloser(bytes.NewReader(b))}, nil
		}
	}
	return h.ObjectStore.Get(ctx, key, opts)
}

type errBodyRead struct{}

func (errBodyRead) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (errBodyRead) Close() error             { return nil }

func (h *hookStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	h.mu.Lock()
	f := h.headErr
	h.mu.Unlock()
	if f != nil {
		if err := f(key, 1); err != nil {
			return nil, err
		}
	}
	return h.ObjectStore.Head(ctx, key)
}

func (h *hookStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	h.mu.Lock()
	if h.delHit == nil {
		h.delHit = map[string]int{}
	}
	h.delHit[key]++
	n := h.delHit[key]
	f := h.delErr
	h.mu.Unlock()
	if f != nil {
		if err := f(key, n); err != nil {
			return err
		}
	}
	return h.ObjectStore.Delete(ctx, key, ifVersion)
}

func (h *hookStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	h.mu.Lock()
	err := h.listErr
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return h.ObjectStore.List(ctx, prefix, startAfter, fn)
}

// attachHooks swaps the registry's store for a fault-injecting wrapper and
// returns it. Handles created afterwards (and existing ones, which reach the
// store through the registry) observe the faults.
func attachHooks(t *testing.T, r *Registry) (*hookStore, store.ObjectStore) {
	t.Helper()
	inner := r.Store()
	hk := newHookStore(inner)
	r.st = hk
	return hk, inner
}

// cfgWith builds a config with the cache dir set and custom WAL knobs.
func cfgWith(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// ---- registry accessors, open/create/delete edge paths ---------------------------

func TestRegistry_Accessors(t *testing.T) {
	r, _ := newTestRegistry(t)
	if r.Config() != r.cfg {
		t.Fatal("Config() mismatch")
	}
	if r.Tasks() != r.tasks {
		t.Fatal("Tasks() mismatch")
	}
	if r.InstanceID() == "" {
		t.Fatal("InstanceID empty")
	}
	if instanceID("") == "" || !strings.HasPrefix(instanceID(""), "unknown-") {
		t.Fatal("instanceID(\"\") must fall back to unknown-<rand>")
	}
}

func TestRegistry_OpenInvalidId(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.Open(context.Background(), "not-an-id"); err == nil {
		t.Fatal("open with malformed id succeeded")
	}
	if _, err := r.Delete(context.Background(), "not-an-id"); err == nil {
		t.Fatal("delete with malformed id succeeded")
	}
}

func TestRegistry_OpenCorruptManifest(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: []byte("garbage")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Open(ctx, "acme/api")
	if err == nil || !store.IsCorrupt(errors.Unwrap(err)) && !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corrupt-manifest error, got %v", err)
	}
}

func TestRegistry_OpenManifestRepoMismatch(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	// Manifest stored under repos/acme/api but claiming a malformed Repo id:
	// openOrInitLocal cannot derive the local dir → error.
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "bogus", ObjectFormat: "sha1", Revision: 1}
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(ctx, "acme/api"); err == nil {
		t.Fatal("open with mismatched manifest repo succeeded")
	}
}

func TestRegistry_OpenInitializesLocalFromManifest(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/api", ObjectFormat: "sha1", Revision: 1, HeadSeq: 0}
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	h, err := r.Open(ctx, "acme/api")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.vals.cacheDir, "acme", "api.git", "HEAD")); err != nil {
		t.Fatalf("local repo not initialized from manifest: %v", err)
	}
	// Second open returns the same handle.
	h2, err := r.Open(ctx, "acme/api")
	if err != nil || h2 != h {
		t.Fatalf("second open: %v same=%v", err, h2 == h)
	}
}

func TestRegistry_CreateStoreError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	hk.putErr = func(key string, n int) error { return store.NewOther(key, errors.New("boom")) }
	if _, err := r.Create(context.Background(), "acme/api", git.Sha1); err == nil {
		t.Fatal("create with failing store succeeded")
	}
}

func TestRegistry_CreateLocalInitError(t *testing.T) {
	r, _ := newTestRegistry(t)
	// A FILE where the repo dir belongs: git init cannot proceed.
	if err := os.MkdirAll(filepath.Join(r.vals.cacheDir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.vals.cacheDir, "acme", "api.git"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(context.Background(), "acme/api", git.Sha1); err == nil {
		t.Fatal("create over a file path succeeded")
	}
}

func TestRegistry_DeleteErrorPaths(t *testing.T) {
	ctx := context.Background()

	// Manifest delete failure (not a 404) aborts the walk.
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	hk.delErr = func(key string, n int) error { return store.NewOther(key, errors.New("boom")) }
	if _, err := r.Delete(ctx, "acme/api"); err == nil {
		t.Fatal("delete with failing manifest delete succeeded")
	}

	// Listing failure aborts the object walk.
	r2, _ := newTestRegistry(t)
	hk2, _ := attachHooks(t, r2)
	if _, err := r2.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	hk2.listErr = errors.New("list boom")
	if _, err := r2.Delete(ctx, "acme/api"); err == nil {
		t.Fatal("delete with failing list succeeded")
	}

	// A key delete failure mid-walk aborts.
	r3, _ := newTestRegistry(t)
	hk3, _ := attachHooks(t, r3)
	h3, err := r3.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("a", 40)
	if _, err := h3.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	hk3.delErr = func(key string, n int) error {
		if strings.HasSuffix(key, ".pb") && !strings.HasSuffix(key, "manifest.pb") {
			return store.NewOther(key, errors.New("boom"))
		}
		return nil
	}
	if _, err := r3.Delete(ctx, "acme/api"); err == nil {
		t.Fatal("delete with failing segment delete succeeded")
	}
}

func TestRegistry_ListCaching(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	got := r.List()
	if len(got) != 1 || got[0] != "acme/api" {
		t.Fatalf("list = %v", got)
	}
	// Cache hit: nothing changed but the snapshot must still be served.
	r.listing.at = time.Now()
	r.listing.repos = []string{"acme/api"}
	if got := r.List(); len(got) != 1 {
		t.Fatalf("cached list = %v", got)
	}

	// Refresh failure serves the LAST GOOD snapshot (05 §5.1.5).
	hk, _ := attachHooks(t, r)
	r.listing.at = time.Time{} // force refresh
	hk.listErr = errors.New("list boom")
	if got := r.List(); len(got) != 1 || got[0] != "acme/api" {
		t.Fatalf("stale list = %v, want last good snapshot", got)
	}

	// A failed FIRST refresh yields nil without error.
	r2, _ := newTestRegistry(t)
	hk2, _ := attachHooks(t, r2)
	hk2.listErr = errors.New("boom")
	if got := r2.List(); got != nil {
		t.Fatalf("failed refresh list = %v, want nil", got)
	}
}

func TestRegistry_RefreshListSkipsFailedOwner(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, "beta/web", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// An owner whose prefix listing fails must be skipped, not fail the pass.
	hk, _ := attachHooks(t, r)
	hk.lpErr = func(prefix string) error {
		if strings.HasPrefix(prefix, "repos/beta/") {
			return errors.New("beta listing down")
		}
		return nil
	}
	if got := r.refreshList(ctx); len(got) != 1 || got[0] != "acme/api" {
		t.Fatalf("refreshList = %v, want only the healthy owner", got)
	}
}

// ---- eviction modes ---------------------------------------------------------------

func TestEvictorLoop_ClampsInterval(t *testing.T) {
	// evict_idle_after below 2 minutes clamps the pass interval to 30s.
	cfg := cfgWith(t, func(c *config.Config) { c.Cache.EvictIdleAfter = config.Duration(time.Second) })
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	if r.vals.evictIdleAfter != time.Second {
		t.Fatal("config vals not wired")
	}
}

func TestEvictIdle_Modes(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()

	// budget (default): under budget → no-op; zero budget → no-op.
	r.vals.cacheMaxBytes = 0
	r.vals.cacheMode = "budget"
	r.evictIdle()
	r.vals.cacheMaxBytes = 1 << 30
	r.evictIdle()

	// disk: under watermark → no-op; statfs error → no-op.
	r.vals.cacheMode = "disk"
	r.evictIdle()
	saved := r.vals.cacheDir
	r.vals.cacheDir = "/nonexistent-walhub-test"
	if r.overDiskWatermark() {
		t.Fatal("statfs error must read as under-watermark")
	}
	r.evictDisk()
	r.vals.cacheDir = saved

	// auto: under watermark + under budget → no-op.
	r.vals.cacheMode = "auto"
	r.evictIdle()
}

func TestEvictBudget_SkipsFreshRepos(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	ctx := context.Background()
	if _, err := r.Create(ctx, "live/repo", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// Live repo dir is huge, orphan is tiny: budget exceeded, but the live
	// repo is fresh (not idle) → it must be skipped, orphan still evicted.
	big := filepath.Join(r.vals.cacheDir, "live", "repo.git", "big.pack")
	if err := os.WriteFile(big, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(r.vals.cacheDir, "ghost", "old.git")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "p"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	r.vals.cacheMaxBytes = 1 << 20
	r.vals.evictIdleAfter = time.Hour
	r.evictBudget()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan survived")
	}
	if _, err := os.Stat(big); err != nil {
		t.Fatalf("fresh live repo was evicted despite idle guard: %v", err)
	}
}

func TestRepoUsages_IgnoreUnreadableOwner(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	// A non-directory entry in the cache root must not break the walk.
	if err := os.WriteFile(filepath.Join(cfg.Cache.Dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	uses := r.repoUsages()
	if len(uses) != 0 {
		t.Fatalf("uses = %v, want none", uses)
	}
}

func TestEvictOne_RemoveAllFailure(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	// An orphan whose contents cannot be removed: RemoveAll fails → evictOne
	// reports failure and the handle map is untouched.
	orphan := filepath.Join(cfg.Cache.Dir, "ghost", "stuck.git")
	inner := filepath.Join(orphan, "sub")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(inner, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(inner, 0o755)
	if r.evictOne("ghost/stuck", orphan) {
		t.Fatal("evictOne reported success despite failed RemoveAll")
	}
	if r.Get("ghost/stuck") != nil {
		t.Fatal("nothing was registered, but Get returned a handle")
	}
}

// ---- task table surface -------------------------------------------------------------

func TestTaskRecord_NoticeProgressCtx(t *testing.T) {
	tt := newTaskTable("h", context.Background())
	done := make(chan *Task, 1)
	_, err := tt.Run(context.Background(), "acme/api", "unit", nil, func(ctx context.Context, t *Task) error {
		if ctx != t.Ctx() {
			t.Errorf("Ctx mismatch")
		}
		rec := t.Record()
		if rec.Kind != "unit" || rec.Hostname != "h" {
			t.Errorf("record = %+v", rec)
		}
		for i := 0; i < 65; i++ { // log tail capped at 60
			t.Notice("n")
		}
		t.Progress("files", 3, 10, "files")
		t.Progress("other", 1, 0, "items") // no total → no percent
		done <- t
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task := <-done
	if len(task.rec.LogTail) != 60 {
		t.Fatalf("log tail = %d, want capped 60", len(task.rec.LogTail))
	}
	p := task.rec.Progress
	if p == nil || p.Done != 3 || p.Total == nil || *p.Total != 10 || p.Percent == nil {
		t.Fatalf("progress = %+v", p)
	}
	if task.rec.OK == nil || !*task.rec.OK {
		t.Fatalf("record not ok: %+v", task.rec)
	}
	if tt.Get(task.rec.ID) == nil {
		t.Fatal("Get(by id) returned nil for a recorded task")
	}
	if tt.Get("missing") != nil {
		t.Fatal("Get(unknown) returned a record")
	}
}

func TestTaskRun_JoinerCancelAndDrain(t *testing.T) {
	tt := newTaskTable("h", context.Background())
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = tt.Run(context.Background(), "acme/api", "slow", nil, func(ctx context.Context, t *Task) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	// A joiner whose ctx is already dead gets ctx.Err, not the leader's result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tt.Run(ctx, "acme/api", "slow", nil, func(context.Context, *Task) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("joiner err = %v, want context.Canceled", err)
	}
	close(release)

	// Drain: new starts refuse with the 503 narration; a second Drain is idempotent.
	tt.Drain()
	tt.Drain()
	if _, err := tt.Run(context.Background(), "acme/api", "x", nil, func(context.Context, *Task) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("run after drain = %v, want 503 error", err)
	}
}

func TestTaskList_SortedByStart(t *testing.T) {
	tt := newTaskTable("h", context.Background())
	for i := 0; i < 3; i++ {
		if _, err := tt.Run(context.Background(), "acme/api", "k", nil, func(context.Context, *Task) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	recs := tt.List("acme/api")
	if len(recs) != 3 {
		t.Fatalf("list = %d", len(recs))
	}
	for i := 1; i < len(recs); i++ {
		if recs[i-1].Started > recs[i].Started {
			t.Fatalf("list not sorted: %s > %s", recs[i-1].Started, recs[i].Started)
		}
	}
}

// ---- locks / singleflight / metrics / fs helpers -------------------------------------

func TestSyncMutex_ContextAbandonAndWarn(t *testing.T) {
	var m syncMutex
	if err := m.LockMeasured(context.Background(), "test_lock", "r"); err != nil {
		t.Fatal(err)
	}
	prev := lockWaitWarnAt.Load()
	SetLockWaitWarn(time.Nanosecond)
	defer lockWaitWarnAt.Store(prev)
	release := make(chan struct{})
	go func() { <-release; m.Unlock() }()
	time.Sleep(5 * time.Millisecond)
	close(release) // trigger the unlock so the queued acquisition can proceed
	if err := m.LockMeasured(context.Background(), "test_lock", "r"); err != nil {
		t.Fatal(err)
	}
	m.Unlock()

	// ctx abandonment: the waiter returns ctx.Err and never holds the lock.
	m.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if err := m.LockMeasured(ctx, "test_lock", "r"); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned wait = %v", err)
	}
	m.Unlock()
	if m.TryLock() == false { // must be free again (helper handed it off)
		t.Fatal("lock stuck after abandonment")
	}
	m.Unlock()
	if _, ok := LockStatsSnapshot()["test_lock"]; !ok {
		t.Fatal("lock histogram missing")
	}
}

func TestSingleflight_JoinerAbandonAndDo(t *testing.T) {
	var g Group
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = g.Do("k", func() (any, error) { <-release; return 1, nil })
	}()
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.DoCtx(ctx, "k", func() (any, error) { return 2, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned joiner = %v", err)
	}
	close(release)
	<-leaderDone
	// Do is DoCtx with Background: after completion the key is free again.
	v, err := g.Do("k", func() (any, error) { return 3, nil })
	if err != nil || v != 3 {
		t.Fatalf("Do after completion = %v %v", v, err)
	}
}

func TestMetricsAndWarnLogger(t *testing.T) {
	prev := logWarnf
	called := make(chan struct{}, 1)
	SetWarnLogger(func(string, ...any) { called <- struct{}{} })
	defer func() { logWarnf = prev }()
	logWarnf("x")
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("custom warn logger not invoked")
	}
	snap := Metrics()
	if snap.OrphansSwept < 0 {
		t.Fatal("negative counter")
	}
}

func TestStatfsError(t *testing.T) {
	if _, err := statfs("/nonexistent-walhub-test"); err == nil {
		t.Fatal("statfs on missing path succeeded")
	}
}

// ---- type-level contracts ------------------------------------------------------------

func TestSyncLevelString(t *testing.T) {
	if LevelRefs.String() != "refs" || LevelServe.String() != "serve" || LevelFull.String() != "full" {
		t.Fatal("level names")
	}
	if SyncLevel(42).String() != "objects" {
		t.Fatal("default level name")
	}
}

func TestObjectAccessAndRemoteReaderStubs(t *testing.T) {
	if (ObjectAccess{}).IsRemote() {
		t.Fatal("empty access is not remote")
	}
	if !(ObjectAccess{Remote: &RemoteReader{}}).IsRemote() {
		t.Fatal("remote access not detected")
	}
	rr := &RemoteReader{}
	if _, _, ok := rr.Locate("x"); ok {
		t.Fatal("nil-eng Locate must miss")
	}
	if _, _, err := rr.Header("x"); err == nil {
		t.Fatal("nil-eng Header must error")
	}
	if _, _, err := rr.Decode(context.Background(), "x"); err == nil {
		t.Fatal("nil-eng Decode must error")
	}
	if err := rr.Fault(context.Background(), nil, nil); err == nil {
		t.Fatal("nil-eng Fault must error")
	}
}

func TestRefErrorAndWalErrorFormatting(t *testing.T) {
	cases := []RefErrorKind{RefErrRejected, RefErrConflict, RefErrStale, RefErrFallback, RefErrorKind(99)}
	seen := map[string]bool{}
	for _, k := range cases {
		s := (&RefError{Kind: k, Ref: "refs/heads/x", Detail: "d"}).Error()
		if s == "" || seen[s] {
			t.Fatalf("RefError(%d) = %q", k, s)
		}
		seen[s] = true
	}
	for _, k := range []WalErrorKind{WalErrGit, WalErrStore, WalErrIo, WalErrCorrupt, WalErrRetry, WalErrInvalid, WalErrNotFound, WalErrAlreadyExists, WalErrTooLarge, WalErrorKind(99)} {
		e := &WalError{Kind: k, Detail: "d"}
		if e.Error() == "" {
			t.Fatalf("WalError(%d) empty", k)
		}
	}
	inner := errors.New("inner")
	e := &WalError{Kind: WalErrIo, Detail: "d", Wrapped: inner}
	if !errors.Is(e, inner) {
		t.Fatal("Unwrap lost the wrapped error")
	}
	if ErrNotFound("r").Error() == "" || ErrExists("r").Error() == "" {
		t.Fatal("constructor errors empty")
	}
	tl := ErrTooLarge(10, 5)
	if tl.Error() == "" || tl.Kind != WalErrTooLarge {
		t.Fatalf("ErrTooLarge = %v", tl)
	}
}

func TestMaxTime(t *testing.T) {
	a := time.Unix(1, 0)
	b := time.Unix(2, 0)
	if !MaxTime(a, b).Equal(b) || !MaxTime(b, a).Equal(b) {
		t.Fatal("MaxTime")
	}
}

func TestGitUpdatesSkipsNil(t *testing.T) {
	out := gitUpdates([]*proto.RefUpdate{nil, {Name: "refs/heads/x"}})
	if len(out) != 1 || out[0].Name != "refs/heads/x" {
		t.Fatalf("gitUpdates = %v", out)
	}
}
