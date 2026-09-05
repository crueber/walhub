// sse_leak_test.go — sibling of #73: the api SSE envelope carried the
// same `for range ka.C` shape, so Close() (ticker Stop only) leaked one
// keepalive goroutine per finished task stream. The writer now exits via
// its done channel or the request context.
package api

import (
	"context"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func awaitSSEExit(t *testing.T, base int, what string) {
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

func TestSSEKeepaliveExitsOnClose(t *testing.T) {
	const streams = 5
	base := runtime.NumGoroutine()
	closers := make([]*SSE, 0, streams)
	for i := 0; i < streams; i++ {
		s, ok := NewSSE(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
		if !ok {
			t.Fatal("recorder must flush")
		}
		closers = append(closers, s)
	}
	if got := runtime.NumGoroutine(); got < base+streams {
		t.Fatalf("expected >= %d goroutines after attach, got %d", base+streams, got)
	}
	for _, s := range closers {
		s.Close()
	}
	awaitSSEExit(t, base, "api keepalive goroutine leaked after Close")
}

func TestSSEKeepaliveExitsOnContextCancel(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	s, ok := NewSSE(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer s.Close()
	if got := runtime.NumGoroutine(); got < base+1 {
		t.Fatalf("expected >= %d goroutines after attach, got %d", base+1, got)
	}
	cancel()
	awaitSSEExit(t, base, "api keepalive goroutine leaked after ctx cancel")
}
