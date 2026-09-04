package review

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// flakyStore injects store failures: failGets maps key → persistent Get
// error; failGetsOnce maps key → one-shot Get error (for read-after-write
// arms); failPuts maps key → remaining 412 failures; failPutsErr maps
// key → a non-412 Put error; failList is a persistent List error.
type flakyStore struct {
	mu           sync.Mutex
	inner        store.ObjectStore
	failGets     map[string]error
	failGetsOnce map[string]error
	failGetSkip  map[string]int    // successful Gets to serve before the one-shot fires
	failGetBody  map[string][]byte // one-shot body override (e.g. corrupt bytes on re-read)
	failPuts     map[string]int
	failPutsErr  map[string]error
	failList     error
}

func (f *flakyStore) Backend() string { return f.inner.Backend() }

func (f *flakyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failGetsOnce[key]; ok {
		if n, skip := f.failGetSkip[key]; skip && n > 0 {
			f.failGetSkip[key] = n - 1
		} else {
			delete(f.failGetsOnce, key)
			delete(f.failGetSkip, key)
			if body, ok := f.failGetBody[key]; ok {
				delete(f.failGetBody, key)
				return store.Object{
					Meta: store.ObjectMeta{Key: key, Version: "v0"},
					Body: io.NopCloser(bytes.NewReader(body)),
				}, nil
			}
			return nil, err
		}
	}
	if err, ok := f.failGets[key]; ok {
		return nil, err
	}
	return f.inner.Get(ctx, key, opts)
}

func (f *flakyStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	return f.inner.Head(ctx, key)
}

func (f *flakyStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failPutsErr[key]; ok {
		return store.ObjectMeta{}, err
	}
	if n, ok := f.failPuts[key]; ok && n > 0 {
		f.failPuts[key] = n - 1
		return store.ObjectMeta{}, store.NewPrecondition(key, "v9")
	}
	return f.inner.Put(ctx, key, body, opts)
}

func (f *flakyStore) Delete(ctx context.Context, key string, v store.Version) error {
	return f.inner.Delete(ctx, key, v)
}

func (f *flakyStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList != nil {
		return f.failList
	}
	return f.inner.List(ctx, prefix, startAfter, fn)
}

func (f *flakyStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	return f.inner.ListPrefixes(ctx, prefix, fn)
}

func (f *flakyStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return f.inner.SignedGetURL(ctx, key, ttl)
}

func (f *flakyStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return f.inner.AccelTarget(ctx, key)
}

func (f *flakyStore) SupportsCompose() bool { return f.inner.SupportsCompose() }
func (f *flakyStore) ComposeIsNative() bool { return f.inner.ComposeIsNative() }

func (f *flakyStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	return f.inner.Compose(ctx, dst, sources, opts)
}

// flakyEnv is a Service over a flakyStore with fault toggles.
type flakyEnv struct {
	svc   *Service
	flaky *flakyStore
}

// failGetSkipN serves n successful Gets of key, then fires the one-shot.
func (e *flakyEnv) failGetSkipN(key string, n int, err error) {
	e.flaky.failGetsOnce[key] = err
	e.flaky.failGetSkip[key] = n
}

func newFlakyEnv() *flakyEnv {
	roles := &FakeRoles{Roles: map[string]string{
		"alice": "write", "bob": "maintain", "carol": "read", "dave": "triage", "erin": "admin",
		"fred": "read", "gina": "read",
	}, Public: true}
	fl := &flakyStore{inner: store.NewMemory(), failGets: map[string]error{},
		failGetsOnce: map[string]error{}, failGetSkip: map[string]int{}, failGetBody: map[string][]byte{},
		failPuts: map[string]int{}, failPutsErr: map[string]error{}}
	svc := New(fl, roles)
	return &flakyEnv{svc: svc, flaky: fl}
}

func (e *flakyEnv) failGet(key string, err error)     { e.flaky.failGets[key] = err }
func (e *flakyEnv) failGetOnce(key string, err error) { e.flaky.failGetsOnce[key] = err }
func (e *flakyEnv) failPut(key string, n int)         { e.flaky.failPuts[key] = n }
func (e *flakyEnv) failPutErr(key string, err error)  { e.flaky.failPutsErr[key] = err }
func (e *flakyEnv) clearFails() {
	e.flaky.failGets = map[string]error{}
	e.flaky.failGetsOnce = map[string]error{}
	e.flaky.failPuts = map[string]int{}
	e.flaky.failPutsErr = map[string]error{}
	e.flaky.failList = nil
}

func TestFlakyCASHelpers(t *testing.T) {
	ctx := context.Background()
	e := newFlakyEnv()
	// attempts <= 0 defaults; fn error aborts; get error aborts.
	if _, err := e.svc.casUpdate(ctx, "k", 0, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return nil, false, errors.New("boom")
	}); err == nil || err.Error() != "boom" {
		t.Fatalf("fn err: %v", err)
	}
	e.failGet("k", errors.New("disk down"))
	if _, err := e.svc.casUpdate(ctx, "k", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte("{}"), true, nil
	}); err == nil {
		t.Fatal("get err must abort")
	}
	if _, _, err := e.svc.getJSON(ctx, "k"); err == nil {
		t.Fatal("getJSON err must surface")
	}
	e.clearFails()
	// 412 then success converges.
	e.failPut("k", 1)
	m, err := e.svc.casUpdate(ctx, "k", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte(`{"v":1}`), true, nil
	})
	if err != nil || m.Key != "k" {
		t.Fatalf("retry: %+v %v", m, err)
	}
	// Persistent 412 exhausts to conflict.
	e.failPut("k2", 99)
	if _, err := e.svc.casUpdate(ctx, "k2", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhaust: %v", err)
	}
	// Non-412 put error aborts.
	e.clearFails()
	e.failPutErr("k3", errors.New("bad put"))
	if _, err := e.svc.casUpdate(ctx, "k3", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); err == nil || !strings.Contains(err.Error(), "bad put") {
		t.Fatalf("bad put: %v", err)
	}
	e.clearFails()
	// No-write returns current meta without a PUT.
	if _, err := e.svc.casUpdate(ctx, "k", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return nil, false, nil
	}); err != nil {
		t.Fatalf("no-write: %v", err)
	}
	// putCreate surfaces backend errors.
	e.failPutErr("kc", errors.New("bad create"))
	if err := e.svc.putCreate(ctx, "kc", []byte("{}")); err == nil {
		t.Fatal("create err must surface")
	}
	e.clearFails()
}

func TestFlakySubmitPaths(t *testing.T) {
	ctx := context.Background()
	hdr := ThreadKey(testOwner, testRepo, testPR)
	in := SubmitInput{State: StateApproved, CommitSHA: testHead}
	t.Run("get failure aborts the reservation", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failGet(hdr, errors.New("disk down"))
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); err == nil {
			t.Fatal("expected error")
		}
		e.clearFails()
	})
	t.Run("put failure aborts the reservation", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPutErr(hdr, errors.New("bad put"))
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); err == nil {
			t.Fatal("expected error")
		}
		e.clearFails()
	})
	t.Run("persistent 412 exhausts to conflict", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPut(hdr, 99)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); !errors.Is(err, ErrConflict) {
			t.Fatalf("err=%v", err)
		}
		e.clearFails()
	})
	t.Run("single 412 retries and converges", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPut(hdr, 1)
		ev, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in)
		if err != nil || ev.Seq != 1 {
			t.Fatalf("%+v %v", ev, err)
		}
		e.clearFails()
	})
	t.Run("event create failure surfaces", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPutErr(ReviewKey(testOwner, testRepo, testPR, 1), errors.New("bad create"))
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); err == nil {
			t.Fatal("expected error")
		}
		e.clearFails()
	})
	t.Run("list failure aborts the summary", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.flaky.failList = errors.New("list down")
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); err == nil {
			t.Fatal("expected error")
		}
		e.clearFails()
	})
}

func TestFlakyThreadPaths(t *testing.T) {
	ctx := context.Background()
	hdr := ThreadKey(testOwner, testRepo, testPR)
	mkThread := func(t *testing.T, e *flakyEnv) string {
		t.Helper()
		seedPR(t, e.svc)
		th, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		return th.TID
	}
	t.Run("reserveTID arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failGet(hdr, errors.New("disk down"))
		if _, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "x"); err == nil {
			t.Fatal("get err")
		}
		e.clearFails()
		e.failPut(hdr, 99)
		if _, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "x"); !errors.Is(err, ErrConflict) {
			t.Fatalf("exhaust: %v", err)
		}
		e.clearFails()
	})
	t.Run("comment arms", func(t *testing.T) {
		e := newFlakyEnv()
		tid := mkThread(t, e)
		tkey := ReviewThreadKey(testOwner, testRepo, testPR, tid)
		e.failGet(tkey, errors.New("disk down"))
		if _, err := e.svc.AddThreadComment(ctx, testOwner, testRepo, testPR, tid, testPrincipal("bob"), "x"); err == nil {
			t.Fatal("get err")
		}
		e.clearFails()
		e.failPut(tkey, 99)
		if _, err := e.svc.AddThreadComment(ctx, testOwner, testRepo, testPR, tid, testPrincipal("bob"), "x"); !errors.Is(err, ErrConflict) {
			t.Fatalf("exhaust: %v", err)
		}
		e.clearFails()
		e.failPutErr(tkey, errors.New("bad put"))
		if _, err := e.svc.AddThreadComment(ctx, testOwner, testRepo, testPR, tid, testPrincipal("bob"), "x"); err == nil {
			t.Fatal("put err")
		}
		e.clearFails()
	})
	t.Run("resolve arms", func(t *testing.T) {
		e := newFlakyEnv()
		tid := mkThread(t, e)
		tkey := ReviewThreadKey(testOwner, testRepo, testPR, tid)
		e.failGet(tkey, errors.New("disk down"))
		if _, err := e.svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("dave")); err == nil {
			t.Fatal("get err")
		}
		e.clearFails()
		e.failPut(tkey, 99)
		if _, err := e.svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("dave")); !errors.Is(err, ErrConflict) {
			t.Fatalf("exhaust: %v", err)
		}
		e.clearFails()
		// Read-after-write miss returns the committed header anyway
		// (skip=2: the loop read + refreshSummary's scan read succeed;
		// the fault lands on the re-read).
		e.failGetSkipN(tkey, 2, errors.New("disk down"))
		th, err := e.svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("dave"))
		if err != nil || !th.Resolved {
			t.Fatalf("%+v %v", th, err)
		}
		// Corrupt re-read also returns the committed header.
		e.flaky.failGetsOnce[tkey] = errors.New("unused")
		e.flaky.failGetSkip[tkey] = 2
		e.flaky.failGetBody[tkey] = []byte("{")
		th, err = e.svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("dave"))
		if err != nil || !th.Resolved {
			t.Fatalf("%+v %v", th, err)
		}
	})
	t.Run("summary refresh arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if _, err := e.svc.refreshSummary(ctx, testOwner, testRepo, 999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing header: %v", err)
		}
		e.failGet(hdr, errors.New("disk down"))
		if _, err := e.svc.refreshSummary(ctx, testOwner, testRepo, testPR); err == nil {
			t.Fatal("get err")
		}
		e.clearFails()
	})
	t.Run("scan arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.flaky.failList = errors.New("list down")
		if _, err := e.svc.scanReviews(ctx, testOwner, testRepo, testPR); err == nil {
			t.Fatal("scan err")
		}
		if _, err := e.svc.scanThreadHeaders(ctx, testOwner, testRepo, testPR); err == nil {
			t.Fatal("scan err")
		}
		if _, err := e.svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "", 10); err == nil {
			t.Fatal("list err")
		}
		e.clearFails()
	})
	t.Run("dismiss arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		e.failPut(hdr, 99)
		if _, _, err := e.svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("erin"), "x"); !errors.Is(err, ErrConflict) {
			t.Fatalf("exhaust: %v", err)
		}
		e.clearFails()
	})
}
