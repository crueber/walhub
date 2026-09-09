// faults248_test.go — Forgejo #248 fault-path coverage: retryable CAS
// ladder, 412 re-read, ctx cancel mid-sweep, store-enumeration errors.
package sizecatalog

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// flakyStore fails the first N Gets with retryable, then delegates; the
// first catalog PUT fails with 412 once (lost race), then delegates.
type flakyStore struct {
	store.ObjectStore
	getFails int32
	put412   int32
}

func (f *flakyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if atomic.AddInt32(&f.getFails, -1) >= 0 {
		return nil, store.NewRetryable(key, errors.New("flaky"))
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *flakyStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == CatalogKey && atomic.AddInt32(&f.put412, -1) >= 0 {
		return store.ObjectMeta{}, store.NewPrecondition(key, "other")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestWriteCatalogRetries(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner, getFails: 2, put412: 1}
	got, err := WriteCatalog(ctx, fl, func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{Repos: []string{"o/a"}}, nil
	})
	if err != nil || got == nil || len(got.Repos) != 1 {
		t.Fatalf("retried write: %v %+v", err, got)
	}
	// Persistent retryable GET exhausts the ladder.
	fl2 := &flakyStore{ObjectStore: store.NewMemory(), getFails: 1000}
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := WriteCatalog(cctx, fl2, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{}, nil
	}); err == nil {
		t.Fatal("exhausted ladder must error")
	}
}

func TestWriteCatalogExhaustion(t *testing.T) {
	ctx := context.Background()
	// Every catalog PUT loses the race → ErrRetriesExhausted (returns last).
	fl := &flakyStore{ObjectStore: store.NewMemory(), getFails: 0, put412: 1000}
	if _, err := WriteCatalog(ctx, fl, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{}, nil
	}); err == nil {
		t.Fatal("exhausted CAS ladder must error")
	}
	// Non-retryable PUT error surfaces immediately.
	bad := &putErrStore{ObjectStore: store.NewMemory(), err: errors.New("disk gone")}
	if _, err := WriteCatalog(ctx, bad, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{}, nil
	}); err == nil {
		t.Fatal("hard PUT error must surface")
	}
}

type putErrStore struct {
	store.ObjectStore
	err error
}

func (p *putErrStore) Put(context.Context, string, store.PutBody, store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, p.err
}

// catalogPutErrStore fails only the aggregate write (sidecars succeed).
type catalogPutErrStore struct {
	store.ObjectStore
}

func (c *catalogPutErrStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == CatalogKey {
		return store.ObjectMeta{}, errors.New("disk gone")
	}
	return c.ObjectStore.Put(ctx, key, body, opts)
}

// sidecarGetErrStore fails sidecar reads with a hard (non-NotFound) error.
type sidecarGetErrStore struct {
	store.ObjectStore
}

func (s *sidecarGetErrStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == store.StatsKey("o", "a") {
		return nil, errors.New("store down")
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func TestFoldOneSidecarGetError(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/a", Revision: 1}
	if _, err := inner.Put(ctx, "repos/o/a/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if _, _, _, _, _, ok := foldOne(ctx, &sidecarGetErrStore{ObjectStore: inner}, "o/a", now, nil, nil); ok {
		t.Fatal("hard sidecar GET error must fail the fold")
	}
}

func TestWriteCatalogBackoffCancel(t *testing.T) {
	// ctx cancelled exactly between the retryable GET and the backoff
	// sleep: the ladder must abort via the backoff's ctx check.
	ctx := context.Background()
	inner := store.NewMemory()
	cctx, cancel := context.WithCancel(ctx)
	fl := &cancelStore{ObjectStore: inner, cancel: cancel, fails: 1}
	if _, err := WriteCatalog(cctx, fl, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{}, nil
	}); err == nil {
		t.Fatal("cancelled backoff must error")
	}
	// Doubling + cap paths of the backoff sleep (live ctx, real sleeps).
	if err := sleepBackoff(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := sleepBackoff(ctx, 10); err != nil {
		t.Fatal(err)
	}
}

// cancelStore cancels cctx on its first GET, then returns retryable once.
type cancelStore struct {
	store.ObjectStore
	cancel context.CancelFunc
	fails  int32
}

func (c *cancelStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if atomic.AddInt32(&c.fails, -1) >= 0 {
		c.cancel()
		return nil, store.NewRetryable(key, errors.New("flaky"))
	}
	return c.ObjectStore.Get(ctx, key, opts)
}

func TestReadCatalogRetryable(t *testing.T) {
	ctx := context.Background()
	fl := &flakyStore{ObjectStore: store.NewMemory(), getFails: 1}
	if _, err := ReadCatalog(ctx, fl); err == nil {
		t.Fatal("retryable read must surface")
	}
	// Hard GET error surfaces too.
	if _, err := ReadCatalog(ctx, &getErrStore{ObjectStore: store.NewMemory()}); err == nil {
		t.Fatal("hard GET error must surface")
	}
	// WriteCatalog surfaces a hard GET error without writing.
	if _, err := WriteCatalog(ctx, &getErrStore{ObjectStore: store.NewMemory()}, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		t.Fatal("f must not run when the read fails")
		return nil, nil
	}); err == nil {
		t.Fatal("write GET error must surface")
	}
	// WriteCatalog surfaces a corrupt catalog (bucket is wrong).
	cst := store.NewMemory()
	if _, err := cst.Put(ctx, CatalogKey, store.PutBody{Bytes: []byte{0xff}}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCatalog(ctx, cst, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{}, nil
	}); err == nil {
		t.Fatal("corrupt catalog must error")
	}
}

type getErrStore struct {
	store.ObjectStore
}

func (g *getErrStore) Get(context.Context, string, store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("store down")
}

// errStore fails ListPrefixes (store-enumeration error path).
type errStore struct {
	store.ObjectStore
}

func (e *errStore) ListPrefixes(context.Context, string, func(string) error) error {
	return errors.New("list down")
}

// oneOwnerDownStore fails repo enumeration for exactly one owner.
type oneOwnerDownStore struct {
	store.ObjectStore
}

func (o *oneOwnerDownStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if prefix == "repos/bad/" {
		return errors.New("owner list down")
	}
	return o.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

func TestListIDsFailure(t *testing.T) {
	ctx := context.Background()
	if _, err := listIDs(ctx, &errStore{ObjectStore: store.NewMemory()}, SweepOptions{}); err == nil {
		t.Fatal("list failure must error")
	}
	if _, err := Sweep(ctx, &errStore{ObjectStore: store.NewMemory()}, SweepOptions{}); err == nil {
		t.Fatal("sweep list failure must error")
	}
	// One owner's repo listing down → that owner skipped, others fold.
	st := store.NewMemory()
	mk := func(id string) {
		m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, Revision: 1}
		if _, err := st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	mk("good/a")
	mk("bad/b")
	ids, err := listIDs(ctx, &oneOwnerDownStore{ObjectStore: st}, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "good/a" {
		t.Fatalf("owner skip: %v", ids)
	}
	lctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Sweep(lctx, store.NewMemory(), SweepOptions{ListRepos: func(ctx context.Context) ([]string, error) {
		return nil, ctx.Err()
	}}); err == nil {
		t.Fatal("cancelled sweep must error")
	}
	// Pre-cancelled ctx with a ctx-ignoring lister: the spawn loop breaks
	// before any worker starts (zero-value results count failed, no error).
	pctx, pcancel := context.WithCancel(ctx)
	pcancel()
	res, err := Sweep(pctx, store.NewMemory(), SweepOptions{ListRepos: func(context.Context) ([]string, error) {
		return []string{"o/a"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("broken spawn: %+v", res)
	}
	// Store-enumerated listing with a pre-cancelled ctx fails in the loop.
	st0 := store.NewMemory()
	m0 := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/a", Revision: 1}
	if _, err := st0.Put(ctx, "repos/o/a/manifest.pb", store.PutBody{Bytes: m0.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, err := listIDs(cctx, st0, SweepOptions{}); err == nil {
		t.Fatal("cancelled enumeration must error")
	}
	// Sweep whose aggregate CAS always fails surfaces the error.
	badCat := &catalogPutErrStore{ObjectStore: store.NewMemory()}
	m1 := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/a", Revision: 1}
	if _, err := badCat.ObjectStore.Put(ctx, "repos/o/a/manifest.pb", store.PutBody{Bytes: m1.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(ctx, badCat, SweepOptions{ListRepos: func(context.Context) ([]string, error) {
		return []string{"o/a"}, nil
	}}); err == nil {
		t.Fatal("catalog write failure must surface")
	}
}

// failPutStore fails sidecar PUTs (per-repo isolation: failed, not fatal).
type failPutStore struct {
	store.ObjectStore
	kind int // 0 = retryable, 1 = precondition, 2 = hard error
}

func (f *failPutStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == store.StatsKey("o", "bad") {
		switch f.kind {
		case 1:
			return store.ObjectMeta{}, store.NewPrecondition(key, "v")
		case 2:
			return store.ObjectMeta{}, errors.New("disk gone")
		default:
			return store.ObjectMeta{}, store.NewRetryable(key, errors.New("down"))
		}
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestSweepIsolatesPutFailures(t *testing.T) {
	for _, kind := range []int{0, 1, 2} {
		ctx := context.Background()
		inner := store.NewMemory()
		mk := func(id string) {
			m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, Revision: 1}
			if _, err := inner.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
				t.Fatal(err)
			}
		}
		mk("o/good")
		mk("o/bad")
		fixed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
		res, err := Sweep(ctx, &failPutStore{ObjectStore: inner, kind: kind}, SweepOptions{
			Now: func() time.Time { return fixed },
			ListRepos: func(context.Context) ([]string, error) {
				return []string{"o/good", "o/bad"}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Failed != 1 || res.Folded != 1 {
			t.Fatalf("kind=%d isolation: %+v", kind, res)
		}
	}
	// All repos fail → no catalog write, no error (empty rows early return).
	ctx := context.Background()
	inner := store.NewMemory()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/bad", Revision: 1}
	if _, err := inner.Put(ctx, "repos/o/bad/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	res, err := Sweep(ctx, &failPutStore{ObjectStore: inner}, SweepOptions{
		Now: func() time.Time { return fixed },
		ListRepos: func(context.Context) ([]string, error) {
			return []string{"o/bad"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Folded != 0 {
		t.Fatalf("all-fail: %+v", res)
	}
}

// slowStore blocks the first manifest GET until released (deterministic
// semaphore-block coverage for the sweep worker pool).
type slowStore struct {
	store.ObjectStore
	release chan struct{}
}

func (s *slowStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == "repos/o/first/manifest.pb" {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func TestSweepWorkerCancelWhileQueued(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	for _, id := range []string{"o/first", "o/second"} {
		m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, Revision: 1}
		if _, err := inner.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	slow := &slowStore{ObjectStore: inner, release: make(chan struct{})}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan SweepResult, 1)
	go func() {
		res, _ := Sweep(cctx, slow, SweepOptions{
			Parallel: 1,
			ListRepos: func(context.Context) ([]string, error) {
				return []string{"o/first", "o/second"}, nil
			},
		})
		done <- res
	}()
	time.Sleep(50 * time.Millisecond) // first worker holds the only slot; second queues
	cancel()
	close(slow.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not exit after cancel")
	}
}
