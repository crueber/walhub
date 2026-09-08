package server

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// countingDeleteStore records Delete calls by key.
type countingDeleteStore struct {
	store.ObjectStore
	mu      sync.Mutex
	deletes map[string]int
}

func (c *countingDeleteStore) Delete(ctx context.Context, key string, v store.Version) error {
	c.mu.Lock()
	if c.deletes == nil {
		c.deletes = map[string]int{}
	}
	c.deletes[key]++
	c.mu.Unlock()
	return c.ObjectStore.Delete(ctx, key, v)
}

func (c *countingDeleteStore) deleteCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deletes[key]
}

func adoptServer(st store.ObjectStore, hints *api.PlaceholderHints) *Server {
	return &Server{store: st, placeholderHints: hints, log: slog.Default()}
}

// Without hints (nil) the push path issues no marker ops at all — the
// push budget default (nil-safe → Consume false).
func TestAdoptPlaceholderNilHintsNoOp(t *testing.T) {
	cs := &countingDeleteStore{ObjectStore: store.NewMemory()}
	s := adoptServer(cs, nil)
	s.adoptPlaceholder(git.RepoId{Owner: "acme", Name: "r"})
	time.Sleep(50 * time.Millisecond)
	if len(cs.deletes) != 0 {
		t.Fatalf("nil hints must issue no deletes: %v", cs.deletes)
	}
}

// Without a consumed hint (auto-created / pre-existing repo) no store op
// issues — the budget-test pin at unit level.
func TestAdoptPlaceholderNoHintNoOp(t *testing.T) {
	cs := &countingDeleteStore{ObjectStore: store.NewMemory()}
	s := adoptServer(cs, &api.PlaceholderHints{})
	s.adoptPlaceholder(git.RepoId{Owner: "acme", Name: "r"})
	time.Sleep(50 * time.Millisecond)
	if len(cs.deletes) != 0 {
		t.Fatalf("unhinted adopt must issue no deletes: %v", cs.deletes)
	}
}

// A consumed hint fires exactly one marker Delete (post-response,
// fire-and-forget); the hint is single-use.
func TestAdoptPlaceholderHintedDeletesOnce(t *testing.T) {
	cs := &countingDeleteStore{ObjectStore: store.NewMemory()}
	hints := &api.PlaceholderHints{}
	id := git.RepoId{Owner: "acme", Name: "r"}
	hints.Add(id)
	s := adoptServer(cs, hints)
	s.adoptPlaceholder(id)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cs.deleteCount(id.StorePrefix()+"meta/placeholder.json") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := cs.deleteCount(id.StorePrefix() + "meta/placeholder.json"); n != 1 {
		t.Fatalf("hinted adopt deletes = %d, want exactly 1", n)
	}
	// Second adopt without re-Add: no further op (hint consumed).
	s.adoptPlaceholder(id)
	time.Sleep(50 * time.Millisecond)
	if n := cs.deleteCount(id.StorePrefix() + "meta/placeholder.json"); n != 1 {
		t.Fatalf("consumed hint must not refire: %d", n)
	}
}

// PlaceholderHints unit: Add/Consume lifecycle, nil-safety.
func TestPlaceholderHintsLifecycle(t *testing.T) {
	var nilHints *api.PlaceholderHints
	nilHints.Add(git.RepoId{Owner: "a", Name: "b"}) // must not panic
	if nilHints.Consume(git.RepoId{Owner: "a", Name: "b"}) {
		t.Fatal("nil hints must never consume")
	}
	h := &api.PlaceholderHints{}
	id := git.RepoId{Owner: "a", Name: "b"}
	if h.Consume(id) {
		t.Fatal("absent hint must not consume")
	}
	h.Add(id)
	h.Add(id) // idempotent
	if !h.Consume(id) {
		t.Fatal("present hint must consume")
	}
	if h.Consume(id) {
		t.Fatal("consumed hint must not refire")
	}
}
