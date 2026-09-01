// errgroup.go: hand-rolled concurrency helpers for the store layer
// (03_store_backends.md §6.2/§6.3). No golang.org/x/sync — dependency rule.
package store

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Weighted is a counting semaphore that hands out n permits per acquire.
// Channel-based: Acquire is ctx-aware (on ctx.Done nothing is taken and
// nothing released), TryAcquire never blocks, Release wakes waiters.
type Weighted struct {
	sem chan struct{}
}

// NewWeighted returns a semaphore with the given capacity.
func NewWeighted(capacity int64) *Weighted {
	if capacity < 1 {
		capacity = 1
	}
	return &Weighted{sem: make(chan struct{}, capacity)}
}

// Acquire takes n permits, blocking until available or ctx is done. On ctx
// cancellation mid-acquire, every permit taken so far is returned: the caller
// is left holding nothing (doc §6.2: "on ctx.Done returns ctx.Err() AND
// releases nothing").
func (w *Weighted) Acquire(ctx context.Context, n int64) error {
	if n <= 0 {
		return nil
	}
	taken := int64(0)
	for range n {
		select {
		case w.sem <- struct{}{}:
			taken++
		case <-ctx.Done():
			w.Release(taken)
			return ctx.Err()
		}
	}
	return nil
}

// TryAcquire takes n permits without blocking; false if not all are free.
func (w *Weighted) TryAcquire(n int64) bool {
	if n <= 0 {
		return true
	}
	if int64(len(w.sem))+n > int64(cap(w.sem)) {
		return false
	}
	for range n {
		w.sem <- struct{}{}
	}
	return true
}

// Release returns n permits to the pool.
func (w *Weighted) Release(n int64) {
	for range n {
		select {
		case <-w.sem:
		default:
		}
	}
}

// AcquireBulk takes one bulk permit and returns the wait time (measured from
// the call, per §6.2: queue time is measured from before Acquire to after
// acquisition) plus a release func. On error nothing was taken.
func AcquireBulk(ctx context.Context, w *Weighted, key string) (time.Duration, func(), error) {
	start := time.Now()
	if err := w.Acquire(ctx, 1); err != nil {
		return 0, func() {}, err
	}
	wait := time.Since(start)
	if wait > lockWaitWarn {
		slog.Warn("bulk permit wait", "wait", wait, "key", key)
	}
	return wait, func() { w.Release(1) }, nil
}

// lockWaitWarn mirrors telemetry.lock_wait_warn's role: past this wait the
// bulk-permit queue is loudly visible in logs (§3.6, metric
// walhub_store_bulk_queue_seconds is emitted by the metrics layer).
var lockWaitWarn = 30 * time.Second

// Group is the bounded errgroup of §6.3: first error cancels the derived
// context, SetLimit gates concurrent goroutines on an internal semaphore.
type Group struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	sem    chan struct{}
	limit  int

	errOnce sync.Once
	err     error
}

// WithContext returns a group whose derived context is cancelled on the first
// Go function error or on Wait.
func WithContext(ctx context.Context) (*Group, context.Context) {
	gctx, cancel := context.WithCancel(ctx)
	return &Group{ctx: gctx, cancel: cancel}, gctx
}

// SetLimit bounds the number of concurrently running goroutines. Zero/absent
// means unbounded. Must be called before the first Go.
func (g *Group) SetLimit(n int) {
	if n > 0 {
		g.limit = n
		g.sem = make(chan struct{}, n)
	}
}

// Go runs f in a goroutine, recording its first non-nil error and cancelling
// the group context.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	if g.sem != nil {
		g.sem <- struct{}{}
	}
	go func() {
		defer g.wg.Done()
		if g.sem != nil {
			defer func() { <-g.sem }()
		}
		if err := f(); err != nil {
			g.errOnce.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	}()
}

// Wait blocks for all goroutines, cancels the context, and returns the first
// error (nil if all succeeded).
func (g *Group) Wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}
