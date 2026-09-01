// rw_extra_test.go — the reader spin path: a reader arriving while a
// try-writer holds the writer flag yields briefly and then proceeds (the
// hazard this lock type exists for).
package rw

import (
	"testing"
	"time"
)

func TestTryRWMutex_ReaderYieldsToMidFlightWriter(t *testing.T) {
	var l TryRWMutex

	// Simulate a try-writer mid-flight: the flag is set while it checks for
	// active readers. A concurrent RLock must spin until the flag clears.
	l.writer.Store(true)

	done := make(chan struct{})
	go func() {
		l.RLock()
		close(done)
	}()

	// Give the reader time to enter the spin loop, then release the flag the
	// way a failed TryWriteLock would.
	time.Sleep(time.Millisecond)
	l.writer.Store(false)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader still blocked after the writer flag cleared")
	}
	l.RUnlock()
	if n := l.Readers(); n != 0 {
		t.Fatalf("readers = %d after release", n)
	}

	// A successful TryWriteLock still excludes new readers.
	if !l.TryWriteLock() {
		t.Fatal("TryWriteLock on an idle lock failed")
	}
	acquired := make(chan struct{})
	go func() {
		l.RLock()
		l.RUnlock()
		close(acquired)
	}()
	time.Sleep(time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("reader acquired the guard during an active writer")
	default:
	}
	l.WriteUnlock()
	<-acquired
}
