package social

import (
	"context"
	"fmt"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore tallies backend round trips (E8: the social half —
// starring is human-rate; the counts pin the constant, not the shape).
type countingStore struct {
	store.ObjectStore
	gets, puts, lists, deletes int
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.gets++
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.puts++
	return c.ObjectStore.Put(ctx, key, body, opts)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	c.lists++
	return c.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	c.deletes++
	return c.ObjectStore.Delete(ctx, key, v)
}

func (c *countingStore) String() string {
	return fmt.Sprintf("GETs=%d PUTs=%d LISTs=%d DELETEs=%d", c.gets, c.puts, c.lists, c.deletes)
}

func TestEvidenceSocialCosts(t *testing.T) {
	x := newHarness(t)
	cs := &countingStore{ObjectStore: x.svc.Store}
	x.svc.Store = cs

	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	t.Logf("star (record probe + Create + counter CAS): %s", cs)
	if cs.gets != 2 || cs.puts != 2 {
		t.Fatalf("star budget: %s", cs)
	}
	cs.gets, cs.puts, cs.lists, cs.deletes = 0, 0, 0, 0
	if _, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	t.Logf("unstar (record probe + Delete + counter CAS): %s", cs)
	if cs.gets != 2 || cs.puts != 1 || cs.deletes != 1 {
		t.Fatalf("unstar budget: %s", cs)
	}
	cs.gets, cs.puts, cs.lists, cs.deletes = 0, 0, 0, 0
	if err := x.svc.IncForks(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	t.Logf("fork increment (counter CAS only): %s", cs)
	if cs.gets != 1 || cs.puts != 1 {
		t.Fatalf("forks budget: %s", cs)
	}
	// Starred list at 3 and 60 entries: 1 LIST + 1 GET per record.
	for _, n := range []int{3, 60} {
		y := newHarness(t)
		cy := &countingStore{ObjectStore: y.svc.Store}
		y.svc.Store = cy
		for i := 0; i < n; i++ {
			if _, err := y.svc.Star(ctx(), jane(), "o", fmt.Sprintf("r%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		cy.gets, cy.puts, cy.lists, cy.deletes = 0, 0, 0, 0
		entries, more, err := y.svc.Starred(ctx(), "jane", 50, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("starred list (n=50) at %d stars: %s more=%v", n, cy, more)
		if cy.lists != 1 || cy.gets != n || len(entries) != min(n, 50) {
			t.Fatalf("starred budget at %d: %s", n, cy)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
