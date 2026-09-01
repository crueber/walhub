package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWeightedBasic(t *testing.T) {
	w := NewWeighted(3)
	if err := w.Acquire(context.Background(), 2); err != nil {
		t.Fatalf("Acquire(2): %v", err)
	}
	if w.TryAcquire(2) {
		t.Fatal("TryAcquire(2) succeeded with only 1 permit free")
	}
	if !w.TryAcquire(1) {
		t.Fatal("TryAcquire(1) failed with 1 permit free")
	}
	if w.TryAcquire(1) {
		t.Fatal("TryAcquire on exhausted semaphore succeeded")
	}
	w.Release(3)
	if !w.TryAcquire(3) {
		t.Fatal("permits not returned by Release")
	}
	w.Release(3)
}

func TestWeightedZeroAndClamp(t *testing.T) {
	w := NewWeighted(0) // clamped to capacity 1
	if err := w.Acquire(context.Background(), 0); err != nil {
		t.Fatalf("Acquire(0): %v", err)
	}
	if err := w.Acquire(context.Background(), -5); err != nil {
		t.Fatalf("Acquire(negative): %v", err)
	}
	if !w.TryAcquire(0) {
		t.Fatal("TryAcquire(0)")
	}
	if !w.TryAcquire(-1) {
		t.Fatal("TryAcquire(negative)")
	}
	if err := w.Acquire(context.Background(), 1); err != nil {
		t.Fatalf("Acquire(1) on capacity-1 semaphore: %v", err)
	}
	w.Release(1)
}

func TestWeightedCancelMidAcquire(t *testing.T) {
	w := NewWeighted(2)
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Acquire(ctx, 1); err != nil {
		t.Fatalf("seed acquire: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- w.Acquire(ctx, 2) }() // blocks: only 1 permit free
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire after cancel: %v", err)
	}
	// Nothing may leak: after releasing the seed permit the full capacity
	// must be takeable again.
	w.Release(1)
	if !w.TryAcquire(2) {
		t.Fatal("permits leaked across a cancelled Acquire")
	}
	w.Release(2)
}

func TestWeightedReleaseWithoutHoldIsSafe(t *testing.T) {
	w := NewWeighted(1)
	w.Release(5) // over-release must not panic nor go negative
	if !w.TryAcquire(1) {
		t.Fatal("semaphore unusable after over-release")
	}
}

func TestAcquireBulk(t *testing.T) {
	w := NewWeighted(1)
	wait, release, err := AcquireBulk(context.Background(), w, "wal/x.pack")
	if err != nil || wait < 0 {
		t.Fatalf("AcquireBulk: wait=%v err=%v", wait, err)
	}
	release()
	// Cancelled context: error, and the returned release func is a no-op.
	// The permit is held so the blocked Acquire cannot complete before
	// noticing the cancellation.
	if err := w.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, release2, err := AcquireBulk(ctx, w, "wal/y.pack")
	if err == nil {
		t.Fatal("AcquireBulk on cancelled ctx succeeded")
	}
	release2() // must not panic
}

func TestGroupSuccessAndCancel(t *testing.T) {
	g, gctx := WithContext(context.Background())
	var live, max int64
	var mu sync.Mutex
	g.SetLimit(3)
	for range 9 {
		g.Go(func() error {
			n := atomic.AddInt64(&live, 1)
			mu.Lock()
			if n > max {
				max = n
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&live, -1)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if max > 3 {
		t.Fatalf("SetLimit(3) breached: %d concurrent", max)
	}
	select {
	case <-gctx.Done():
	default:
		t.Fatal("gctx not cancelled after Wait")
	}
}

func TestGroupFirstErrorCancels(t *testing.T) {
	g, gctx := WithContext(context.Background())
	sentinel := errors.New("boom")
	blocker := make(chan struct{})
	g.Go(func() error {
		<-blocker // released by Wait via group cancel
		return nil
	})
	g.Go(func() error { return sentinel })
	// Wait for the error to cancel the derived context.
	deadline := time.After(2 * time.Second)
	for gctx.Err() == nil {
		select {
		case <-deadline:
			t.Fatal("derived context never cancelled on first error")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(blocker)
	if err := g.Wait(); !errors.Is(err, sentinel) {
		t.Fatalf("Wait: %v", err)
	}
}

func TestGroupSetLimitZeroIsUnbounded(t *testing.T) {
	g, _ := WithContext(context.Background())
	var started atomic.Int64
	done := make(chan struct{})
	for range 4 {
		g.Go(func() error {
			started.Add(1)
			return nil
		})
	}
	go func() { g.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unbounded group deadlocked")
	}
	if started.Load() != 4 {
		t.Fatalf("started=%d", started.Load())
	}
}
