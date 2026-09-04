package social

import (
	"context"
	"fmt"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore tallies backend round trips (E8: the social half —
// starring is human-rate; the counts pin the constant, not the shape).
// HEADs are counted separately: the manifest-exists probe (#63
// miss-tolerance) rides HEAD, not GET.
type countingStore struct {
	store.ObjectStore
	gets, puts, lists, deletes, heads int
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

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	c.heads++
	return c.ObjectStore.Head(ctx, key)
}

func (c *countingStore) String() string {
	return fmt.Sprintf("GETs=%d HEADs=%d PUTs=%d LISTs=%d DELETEs=%d", c.gets, c.heads, c.puts, c.lists, c.deletes)
}

func (c *countingStore) reset() { c.gets, c.heads, c.puts, c.lists, c.deletes = 0, 0, 0, 0, 0 }

func TestEvidenceSocialCosts(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	cs := &countingStore{ObjectStore: x.svc.Store}
	x.svc.Store = cs

	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	t.Logf("star (manifest HEAD + record probe + Create + counter CAS): %s", cs)
	if cs.gets != 2 || cs.heads != 1 || cs.puts != 2 {
		t.Fatalf("star budget: %s", cs)
	}
	cs.reset()
	if _, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	t.Logf("unstar (record probe + Delete + manifest HEAD + counter CAS): %s", cs)
	if cs.gets != 2 || cs.heads != 1 || cs.puts != 1 || cs.deletes != 1 {
		t.Fatalf("unstar budget: %s", cs)
	}
	cs.reset()
	if err := x.svc.IncForks(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	t.Logf("fork increment (counter CAS only): %s", cs)
	if cs.gets != 1 || cs.puts != 1 {
		t.Fatalf("forks budget: %s", cs)
	}
	// Starred list (n=50): 1 LIST + 1 GET + 1 manifest HEAD per served
	// record (+1 aliveness probe to decide `more` exactly) — O(page),
	// flat in the total starred count. #65 replaced the full-prefix scan
	// with keyset pagination over the key space: the LIST aborts at the
	// page edge, and later pages resume after the previous page's last
	// entry instead of re-probing it.
	for _, n := range []int{3, 60, 600} {
		y := newHarness(t)
		for i := 0; i < n; i++ {
			seedRepo(t, y, "o", fmt.Sprintf("r%d", i))
		}
		cy := &countingStore{ObjectStore: y.svc.Store}
		y.svc.Store = cy
		for i := 0; i < n; i++ {
			if _, err := y.svc.Star(ctx(), jane(), "o", fmt.Sprintf("r%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		cy.reset()
		entries, more, err := y.svc.Starred(ctx(), "jane", 50, "")
		if err != nil {
			t.Fatal(err)
		}
		// Served records plus the one `more` probe; identical at 60
		// and 600 stars (flat), smaller only when stars run out.
		wantEntries, wantProbes := min(n, 50), min(n, 51)
		t.Logf("starred page1 (n=50) at %d stars: %s more=%v", n, cy, more)
		if cy.lists != 1 || cy.gets != wantProbes || cy.heads != wantProbes ||
			len(entries) != wantEntries || more != (n > 50) {
			t.Fatalf("starred budget at %d: %s", n, cy)
		}
		if n == 60 {
			// Page 2 resumes after page 1's last entry: it probes
			// only the remainder (10 records), never the first 50.
			cy.reset()
			last := entries[len(entries)-1]
			rest, more2, err := y.svc.Starred(ctx(), "jane", 50, last.StarredAt+"|"+last.Repo)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("starred page2 (n=50) at 60 stars: %s more=%v", cy, more2)
			if cy.lists != 1 || cy.gets != 10 || cy.heads != 10 || len(rest) != 10 || more2 {
				t.Fatalf("starred page2 at 60: %s", cy)
			}
			seen := map[string]bool{}
			for _, e := range entries {
				seen[e.Repo] = true
			}
			for _, e := range rest {
				if seen[e.Repo] {
					t.Fatalf("starred overlap at 60: %q on both pages", e.Repo)
				}
				seen[e.Repo] = true
			}
			if len(seen) != 60 {
				t.Fatalf("starred union at 60: %d of 60", len(seen))
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
