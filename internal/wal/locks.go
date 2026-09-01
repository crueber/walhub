// locks.go — the per-repo mutex flavors of doc 05 §5.1.2/§5.9.
//
// syncMu/packMu are plain sync.Mutex in substance but every request-path
// acquisition is try-first + measured (13_concurrency.md §2.2 rule 5):
// take the non-blocking attempt, and on failure measure the queue wait
// against telemetry.lock_wait_warn and record walgit_lock_wait_seconds.
// The rw lock lives in internal/wal/rw (try-write-only; see that package).
package wal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// syncMutex is sync.Mutex + instrumented acquisition (TryLock probe first).
type syncMutex struct {
	mu sync.Mutex
}

func (m *syncMutex) Lock()         { m.mu.Lock() }
func (m *syncMutex) Unlock()       { m.mu.Unlock() }
func (m *syncMutex) TryLock() bool { return m.mu.TryLock() }

// LockMeasured acquires the mutex try-first; a queued acquisition is timed
// and warned past telemetry.lock_wait_warn (§5.9). ctx aborts the wait.
func (m *syncMutex) LockMeasured(ctx context.Context, lock, repo string) error {
	if m.TryLock() {
		recordLockWait(lock, 0)
		return nil
	}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		m.mu.Lock()
		close(done)
	}()
	select {
	case <-done:
		wait := time.Since(start)
		recordLockWait(lock, wait)
		warnLockWait(lock, repo, wait)
		return nil
	case <-ctx.Done():
		// Abandonment: never leave the lock half-acquired — the helper
		// goroutine finishes its acquisition and hands it off (13 §5.9).
		go func() {
			<-done
			m.mu.Unlock()
		}()
		return ctx.Err()
	}
}

// ---- lock wait instrumentation (§5.9; full exposition lives in the server) ----

var lockWaitWarnAt atomic.Int64 // duration ns; 0 = disabled

// SetLockWaitWarn configures telemetry.lock_wait_warn.
func SetLockWaitWarn(d time.Duration) { lockWaitWarnAt.Store(int64(d)) }

// lockHistogram buckets a wait in µs: [0, 100µs, 1ms, 10ms, 100ms, 1s, 10s, ∞).
type lockHistogram struct {
	buckets [8]atomic.Int64
}

var (
	lockStatsMu sync.Mutex
	lockStats   = map[string]*lockHistogram{}
)

func recordLockWait(lock string, d time.Duration) {
	lockStatsMu.Lock()
	k, ok := lockStats[lock]
	if !ok {
		k = &lockHistogram{}
		lockStats[lock] = k
	}
	lockStatsMu.Unlock()

	µs := d.Microseconds()
	idx := len(k.buckets) - 1
	for i, b := range [...]int64{100, 1_000, 10_000, 100_000, 1_000_000, 10_000_000} {
		if µs < b {
			idx = i
			break
		}
	}
	k.buckets[idx].Add(1)
}

func warnLockWait(lock, repo string, wait time.Duration) {
	w := time.Duration(lockWaitWarnAt.Load())
	if w > 0 && wait > w {
		logWarnf("lock wait exceeded: lock=%s repo=%s wait_ms=%d", lock, repo, wait.Milliseconds())
	}
}

// LockStatsSnapshot returns per-lock histogram counts (test/ops helper).
func LockStatsSnapshot() map[string][]int64 {
	lockStatsMu.Lock()
	defer lockStatsMu.Unlock()
	out := make(map[string][]int64, len(lockStats))
	for name, k := range lockStats {
		b := make([]int64, len(k.buckets))
		for i := range k.buckets {
			b[i] = k.buckets[i].Load()
		}
		out[name] = b
	}
	return out
}
