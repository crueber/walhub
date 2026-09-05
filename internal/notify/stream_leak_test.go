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
	if got := runtime.NumGoroutine(); got < base+streams {
		t.Fatalf("expected >= %d goroutines after attach, got %d", base+streams, got)
	}
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
	if got := runtime.NumGoroutine(); got < base+1 {
		t.Fatalf("expected >= %d goroutines after attach, got %d", base+1, got)
	}
	cancel()
	awaitGoroutines(t, base, "keepalive goroutine leaked after ctx cancel")
}
