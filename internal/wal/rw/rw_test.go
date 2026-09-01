package rw

import (
	"sync"
	"testing"
	"time"
)

func TestTryRWMutex_Basic(t *testing.T) {
	var l TryRWMutex
	l.RLock()
	l.RLock() // nested read is allowed, must be balanced
	if l.Readers() != 2 {
		t.Fatalf("readers = %d, want 2", l.Readers())
	}
	if l.TryWriteLock() {
		t.Fatal("TryWriteLock succeeded with readers active")
	}
	l.RUnlock()
	l.RUnlock()
	if !l.TryWriteLock() {
		t.Fatal("TryWriteLock failed with no readers")
	}
	if !l.writer.Load() {
		t.Fatal("writer flag not set")
	}
	if l.TryWriteLock() {
		l.WriteUnlock()
		t.Fatal("second TryWriteLock succeeded while writer held")
	}
	l.WriteUnlock()
	l.RLock()
	l.RUnlock()
}

func TestTryRWMutex_WritersNeverQueue(t *testing.T) {
	// The §7.1 incident: a queued writer must not block new readers.
	var l TryRWMutex
	l.RLock() // simulates a long clone's read guard
	defer l.RUnlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if l.TryWriteLock() { // removal attempt: must fail fast, never queue
			l.WriteUnlock()
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("TryWriteLock blocked — writers must never queue")
	}

	// New readers keep flowing while the read guard is held.
	got := make(chan struct{})
	go func() {
		l.RLock()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("new RLock blocked while a try-writer was mid-flight")
	}
}

func TestTryRWMutex_ConcurrentStress(t *testing.T) {
	var l TryRWMutex
	var wg sync.WaitGroup
	removed := 0
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.RLock()
				if l.TryWriteLock() { // never legal while we hold the read guard
					mu.Lock()
					removed++
					mu.Unlock()
					l.WriteUnlock()
				}
				l.RUnlock()
			}
		}()
	}
	wg.Wait()
	if removed != 0 {
		t.Fatalf("%d removals succeeded while the writer held a read guard", removed)
	}
}
