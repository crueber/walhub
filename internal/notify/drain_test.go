// drain_test.go — regression tests for issue #154: notify task
// goroutines must observe phase-1 drain. StartWebhooks used
// WithoutCancel and drainFanout used Background(), neither tracked in a
// WaitGroup — a wedged store call hung them forever, immune to
// drain/shutdown. Both leaders now run on the service drainCtx
// (cancelled by Drain) and are tracked in Service.wg.
//
// ### Concurrency
//
// Hazard: a wedged store hangs the leader past the test binary's
// patience. Avoidance: the wedge honors ctx (blocks on ctx.Done like a
// well-behaved backend under load), the test drains, and every wait is
// a bounded poll — a regression fails the test in seconds, never hangs
// the suite.
package notify

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// wedgeStore blocks Get on ctx.Done (a wedged-but-ctx-respecting
// backend): pre-fix leaders (WithoutCancel/Background) never observe
// Drain and hang; post-fix leaders run on drainCtx and terminate.
type wedgeStore struct {
	store.ObjectStore
	entered chan struct{}
}

func newWedgeStore(t *testing.T) *wedgeStore {
	t.Helper()
	return &wedgeStore{ObjectStore: store.NewMemory(), entered: make(chan struct{}, 64)}
}

func (w *wedgeStore) wedge(ctx context.Context) error {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return context.DeadlineExceeded
	}
}

func (w *wedgeStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if err := w.wedge(ctx); err != nil {
		return nil, err
	}
	return w.ObjectStore.Get(context.Background(), key, opts)
}

func (w *wedgeStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if err := w.wedge(ctx); err != nil {
		return nil, err
	}
	return w.ObjectStore.Head(context.Background(), key)
}

func (w *wedgeStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if err := w.wedge(ctx); err != nil {
		return err
	}
	return w.ObjectStore.List(context.Background(), prefix, startAfter, fn)
}

func (w *wedgeStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if err := w.wedge(ctx); err != nil {
		return err
	}
	return w.ObjectStore.ListPrefixes(context.Background(), prefix, fn)
}

func (w *wedgeStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if err := w.wedge(ctx); err != nil {
		return store.ObjectMeta{}, err
	}
	return w.ObjectStore.Put(context.Background(), key, body, opts)
}

func (w *wedgeStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if err := w.wedge(ctx); err != nil {
		return err
	}
	return w.ObjectStore.Delete(context.Background(), key, ifVersion)
}

// waitTaskFinished polls TaskStatus until the (repo, kind) record
// reports finished or the bound elapses.
func waitTaskFinished(t *testing.T, svc *Service, repo, kind string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if rec := svc.TaskStatus(repo, kind); rec != nil && rec.State == TaskFinished {
			return
		}
		if time.Now().After(deadline) {
			rec := svc.TaskStatus(repo, kind)
			t.Fatalf("timed out waiting for (%s, %s) to finish (last=%+v)", repo, kind, rec)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitWedged waits until the wedge has absorbed want Get calls.
func waitWedged(t *testing.T, w *wedgeStore, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < want; i++ {
		select {
		case <-w.entered:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timed out waiting for wedged store call %d/%d", i+1, want)
		}
	}
}

// TestDrainInterruptsWedgedWebhooks starts a webhooks task against a
// wedged store and drains: the leader must terminate promptly (the task
// ends) instead of hanging forever.
func TestDrainInterruptsWedgedWebhooks(t *testing.T) {
	w := newWedgeStore(t)
	svc := New(w, newFakeRoles())
	if rec := svc.StartWebhooks(context.Background(), "acme/repo"); rec == nil {
		t.Fatal("StartWebhooks refused before drain")
	}
	waitWedged(t, w, 1) // leader is inside ListHooks → wedged Get
	svc.Drain()
	if !svc.Draining() {
		t.Fatal("Draining=false after Drain")
	}
	waitTaskFinished(t, svc, "acme/repo", TaskKindWebhooks)
}

// TestDrainInterruptsWedgedFanout enqueues a fan-out seq against a
// wedged store and drains: the leader must terminate promptly (the task
// ends) instead of hanging forever.
func TestDrainInterruptsWedgedFanout(t *testing.T) {
	w := newWedgeStore(t)
	svc := New(w, newFakeRoles())
	svc.enqueueFanout("acme/repo", 1)
	waitWedged(t, w, 1) // leader is inside fanoutOne → wedged Get
	svc.Drain()
	waitTaskFinished(t, svc, "acme/repo", TaskKindFanout)
}

// TestDrainRefusesNewTasks pins the fail-fast half of the fix: after
// Drain, StartWebhooks returns nil and enqueueFanout mints no task, so
// shutdown never grows the WaitGroup it just cancelled.
func TestDrainRefusesNewTasks(t *testing.T) {
	w := newWedgeStore(t)
	svc := New(w, newFakeRoles())
	svc.Drain()
	if rec := svc.StartWebhooks(context.Background(), "acme/repo"); rec != nil {
		t.Fatalf("StartWebhooks after Drain = %+v, want nil", rec)
	}
	svc.enqueueFanout("acme/repo", 1)
	// No leader may exist: give a would-be goroutine 100 ms to appear,
	// then assert no task was ever registered.
	time.Sleep(100 * time.Millisecond)
	if rec := svc.TaskStatus("acme/repo", TaskKindFanout); rec != nil {
		t.Fatalf("fanout task after Drain = %+v, want nil", rec)
	}
	if rec := svc.TaskStatus("acme/repo", TaskKindWebhooks); rec != nil {
		t.Fatalf("webhooks task after Drain = %+v, want nil", rec)
	}
	var running int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.wg.Wait()
		atomic.AddInt64(&running, 1)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Service.wg never drained after Drain with no tasks")
	}
	if atomic.LoadInt64(&running) != 1 {
		t.Fatal("Service.wg.Wait did not return")
	}
}
