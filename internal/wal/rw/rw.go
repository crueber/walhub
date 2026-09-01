// Package rw: the reader-friendly RWMutex of doc 05 §5.1.3 (normative copy).
//
// Writers NEVER queue: the only write-class operation is TryWriteLock, which
// fails immediately when readers are active (the caller defers — e.g. pack
// removals stay in pending_pack_removals for the next sync). Readers block
// normally and never starve, because no writer is ever queued. Nested RLock
// is allowed (the count merely increments) but MUST be balanced; a queued
// sync.RWMutex writer would deadlock the same pattern — that hazard is why
// this type exists (13_concurrency.md §2.1, §7 incident 1).
package rw

import (
	"sync"
	"sync/atomic"
	"time"
)

// TryRWMutex is a reader-preferring lock whose writers are try-only.
type TryRWMutex struct {
	mu      sync.Mutex // protects the counters below; held briefly
	readers atomic.Int64
	writer  atomic.Bool
}

// RLock takes a read guard. May block briefly while a try-writer is
// mid-flight; cannot starve (writers are rare and transient).
func (l *TryRWMutex) RLock() {
	for {
		l.mu.Lock()
		if !l.writer.Load() {
			l.readers.Add(1)
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
		// A try-writer is mid-flight; yield briefly.
		time.Sleep(50 * time.Microsecond)
	}
}

// RUnlock releases one read guard (must balance RLock, including nested).
func (l *TryRWMutex) RUnlock() { l.readers.Add(-1) }
func (l *TryRWMutex) TryWriteLock() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writer.CompareAndSwap(false, true) {
		if l.readers.Load() == 0 {
			return true // caller owns the write guard; call WriteUnlock
		}
		l.writer.Store(false) // readers active: release the flag, do NOT queue
	}
	return false
}

// WriteUnlock releases the write guard.
func (l *TryRWMutex) WriteUnlock() { l.writer.Store(false) }

// Readers reports the current read-guard count (test/telemetry helper).
func (l *TryRWMutex) Readers() int64 { return l.readers.Load() }
