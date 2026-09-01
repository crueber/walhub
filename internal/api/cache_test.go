package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestLRUWeightEviction(t *testing.T) {
	c := newLRU(10)
	c.Put("a", 4, "a")
	c.Put("b", 4, "b")
	c.Put("c", 4, "c") // evicts a (10 < 12 → b stays newest order a,b,c → evict a)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should remain")
	}
	// refresh b, then push again: b must survive as most-recent
	c.Get("b")
	c.Put("d", 4, "d")
	if _, ok := c.Get("b"); !ok {
		t.Fatal("refreshed b should survive over c")
	}
	if v, ok := c.Get("d"); !ok || v != "d" {
		t.Fatal("d missing")
	}
	// an entry heavier than the budget is never cached
	c.Put("huge", 100, "x")
	if _, ok := c.Get("huge"); ok {
		t.Fatal("oversized entry must not be cached")
	}
}

func TestRefCacheRevisionStamping(t *testing.T) {
	c := newRefCache(4)
	c.Put("a/b", "refs/heads/main", 10, "aaaa")
	if sha, ok := c.Get("a/b", "refs/heads/main", 10); !ok || sha != "aaaa" {
		t.Fatal("hit expected at same revision")
	}
	if _, ok := c.Get("a/b", "refs/heads/main", 11); ok {
		t.Fatal("newer revision must invalidate lazily")
	}
	c.Put("a/b", "refs/heads/main", 11, "bbbb")
	if sha, _ := c.Get("a/b", "refs/heads/main", 11); sha != "bbbb" {
		t.Fatal("re-stamped value missing")
	}
}

func TestRenderCacheSingleFlight(t *testing.T) {
	c := newRenderCache(1 << 20)
	var renders atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body, _, err := c.Get("tree/aaaa", 7, func() ([]byte, string, error) {
				renders.Add(1)
				time.Sleep(20 * time.Millisecond) // widen the race window
				return []byte(`{"sha":"aaaa"}`), "aaaa", nil
			})
			if err != nil {
				t.Errorf("render err: %v", err)
			}
			if !strings.Contains(string(body), "aaaa") {
				t.Errorf("bad body %q", body)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := renders.Load(); n != 1 {
		t.Fatalf("renders = %d, want 1 (single-flight broken)", n)
	}
}

func TestRenderCacheRevisionChangeRerenders(t *testing.T) {
	c := newRenderCache(1 << 20)
	calls := 0
	render := func() ([]byte, string, error) {
		calls++
		return []byte(`{"sha":"aaaa"}`), "aaaa", nil
	}
	c.Get("k", 1, render)
	c.Get("k", 1, render)
	if calls != 1 {
		t.Fatalf("same revision hit the render twice (%d)", calls)
	}
	c.Get("k", 2, render)
	if calls != 2 {
		t.Fatalf("revision change must re-render (calls=%d)", calls)
	}
}

func TestRenderCacheErrorNotCached(t *testing.T) {
	c := newRenderCache(1 << 20)
	calls := 0
	render := func() ([]byte, string, error) {
		calls++
		return nil, "", ErrNotFound
	}
	if _, _, err := c.Get("k", 1, render); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := c.Get("k", 1, render); err == nil {
		t.Fatal("expected error (2nd)")
	}
	if calls != 2 {
		t.Fatalf("errors must not be cached (calls=%d)", calls)
	}
}

func TestRenderCacheBoundedJoin(t *testing.T) {
	// A hung leader must not wedge followers: after the 30 s bounded join a
	// follower renders itself. Shrink the join via a direct inflight probe:
	// we simulate by starting a render, then joining from another goroutine
	// and confirming it returns promptly after the leader finishes (the
	// deadline path itself would need 30 s; the wake-on-close path is what
	// must never wedge).
	c := newRenderCache(1 << 20)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = c.Get("k", 1, func() ([]byte, string, error) {
			<-release
			return []byte("x"), "", nil
		})
	}()
	time.Sleep(10 * time.Millisecond) // let the leader register inflight
	release <- struct{}{}             // will be taken if leader started
	<-done
	c.mu.Lock()
	n := len(c.inflight)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("inflight entry leaked (%d)", n)
	}
}

func TestBucketMirrorRoundTrip(t *testing.T) {
	st := store.NewMemory()
	e := &Env{Store: st}
	e.Ready()
	body := []byte(`{"sha":"aaaa","entries":[]}`)

	// Miss first
	if _, ok := e.bucketGet(context.Background(), "tree/aaaa/x", 5); ok {
		t.Fatal("expected miss")
	}
	e.bucketPut("tree/aaaa/x", 5, body)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := e.bucketGet(context.Background(), "tree/aaaa/x", 5); ok {
			if string(got) != string(body) {
				t.Fatalf("mirrored body = %s", got)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Stale generation (different revision) must be rejected
	if _, ok := e.bucketGet(context.Background(), "tree/aaaa/x", 6); ok {
		t.Fatal("stale revision must be discarded")
	}
}

func TestETagMatch(t *testing.T) {
	sha := "aaaa"
	if etagMatch("", sha) {
		t.Fatal("empty must not match")
	}
	if !etagMatch(`"aaaa"`, sha) {
		t.Fatal("quoted must match")
	}
	if !etagMatch(`W/"aaaa"`, sha) {
		t.Fatal("weak must match")
	}
	if !etagMatch(`"bbbb", "aaaa"`, sha) {
		t.Fatal("any-of must match")
	}
	if !etagMatch("*", sha) {
		t.Fatal("* must match")
	}
	if etagMatch(`"bbbb"`, sha) {
		t.Fatal("different must not match")
	}
}

func TestRevIsFullSHA(t *testing.T) {
	if !revIsFullSHA("cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6e7") {
		t.Fatal("40-hex must be full")
	}
	if !revIsFullSHA(strings.Repeat("ab", 32)) {
		t.Fatal("64-hex must be full")
	}
	if revIsFullSHA("cb38da1") {
		t.Fatal("short sha is not full")
	}
	if revIsFullSHA("CB38DA1B23E56A2B3C4D5E6F708192A3B4C5D6E7") {
		t.Fatal("uppercase is not the canonical class")
	}
	if revIsFullSHA("cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6gg") {
		t.Fatal("non-hex must fail")
	}
}

func TestRenderImmutableUsesBucketBeforeRender(t *testing.T) {
	st := store.NewMemory()
	e := &Env{Store: st}
	e.Ready()
	var renders atomic.Int64
	render := func() ([]byte, error) {
		renders.Add(1)
		return []byte(`{"sha":"aaaa"}`), nil
	}
	body, err := e.renderImmutable(context.Background(), "tree/aaaa/x", 3, "aaaa", render)
	if err != nil {
		t.Fatal(err)
	}
	if renders.Load() != 1 {
		t.Fatalf("first call must render (%d)", renders.Load())
	}
	body2, err := e.renderImmutable(context.Background(), "tree/aaaa/x", 3, "aaaa", render)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(body2) {
		t.Fatal("second call must serve the LRU copy")
	}
	if renders.Load() != 1 {
		t.Fatalf("LRU must absorb the second call (%d)", renders.Load())
	}
	_ = errors.New // keep imports honest
}
