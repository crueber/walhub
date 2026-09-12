// stream_leak_test.go — #73 regression: the per-user SSE keepalive
// goroutine (#13) must terminate on stream teardown. close() used to only
// Stop the ticker, whose channel Stop does not close, so `for range ka.C`
// blocked forever — one leaked goroutine per disconnected stream.
package notify

import (
	"context"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// awaitGoroutines polls until the count returns to base (or the deadline
// hits — a fixed sleep-then-assert would flake under -race load).
func awaitGoroutines(t *testing.T, base int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if runtime.NumGoroutine() <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%s: goroutines still %d > baseline %d after 5s\n%s",
				what, runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitGoroutinesUp polls until at least n goroutines exist (or the
// deadline hits). A `go` statement's goroutine is not guaranteed
// countable the instant the statement returns, so an immediate
// NumGoroutine assertion pins scheduling, not behavior — under CI load
// the spawned keepalives had not all started when counted (Forgejo
// #397: "expected >= 8 goroutines after attach, got 7"). Same
// poll-don't-sleep treatment as #178.
func awaitGoroutinesUp(t *testing.T, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := runtime.NumGoroutine(); got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: goroutines still %d, want >= %d after 5s",
				what, runtime.NumGoroutine(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSEWriterKeepaliveExitsOnClose(t *testing.T) {
	const streams = 5
	base := runtime.NumGoroutine()
	writers := make([]*sseWriter, 0, streams)
	for i := 0; i < streams; i++ {
		r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil)
		w, ok := newSSEWriter(httptest.NewRecorder(), r)
		if !ok {
			t.Fatal("recorder must flush")
		}
		writers = append(writers, w)
	}
	awaitGoroutinesUp(t, base+streams, "keepalive goroutines after attach")
	// The handler path: defer s.close() on disconnect.
	for _, w := range writers {
		w.close()
	}
	awaitGoroutines(t, base, "keepalive goroutine leaked after close")
}

func TestSSEWriterKeepaliveExitsOnContextCancel(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil).WithContext(ctx)
	w, ok := newSSEWriter(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer w.close()
	awaitGoroutinesUp(t, base+1, "keepalive goroutine after attach")
	cancel()
	awaitGoroutines(t, base, "keepalive goroutine leaked after ctx cancel")
}
