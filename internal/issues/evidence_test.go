package issues

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// Counting store for the E3 evidence harness (docs/EVIDENCE.md): records
// per-operation store round trips by class so the issue-list/read path
// budgets are measured on the real code path, not modeled.
type countingStore struct {
	store.ObjectStore
	gets  int64
	lists int64
	puts  int64
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	atomic.AddInt64(&c.gets, 1)
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	atomic.AddInt64(&c.lists, 1)
	return c.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	atomic.AddInt64(&c.puts, 1)
	return c.ObjectStore.Put(ctx, key, body, opts)
}

func (c *countingStore) reset() (gets, lists, puts int64) {
	return atomic.SwapInt64(&c.gets, 0), atomic.SwapInt64(&c.lists, 0), atomic.SwapInt64(&c.puts, 0)
}

// TestEvidenceIssueReadPath measures the §2/§7 read-path budgets at two
// populations (10 vs 300 issues; a 60-comment thread vs a 5-comment one).
// Repro: go test ./internal/issues/ -run TestEvidenceIssueReadPath -v.
// Backend: memory store (isolates the package's algorithmic shape from
// network RTT; every op counted is one bucket round trip on any backend).
func TestEvidenceIssueReadPath(t *testing.T) {
	for _, n := range []int{10, 300} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			cs := &countingStore{ObjectStore: store.NewMemory()}
			s := New(cs, newFakeRoles())
			ctx := context.Background()
			// Seed: n issues; issue 1 gets a long thread at the large
			// population (60 comments) vs a short one (5) at small.
			long := 5
			if n > 10 {
				long = 60
			}
			for i := 0; i < n; i++ {
				th, _, err := s.CreateIssue(ctx, "acme", "repo", janeP, fmt.Sprintf("issue %d", i), "body")
				if err != nil {
					t.Fatal(err)
				}
				if th.Num == 1 {
					for c := 0; c < long; c++ {
						if _, err := s.AddComment(ctx, "acme", "repo", 1, bobP, fmt.Sprintf("comment %d", c)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			cs.reset()
			// 1. Default list page (index-first).
			res, err := s.ListIssues(ctx, "acme", "repo", janeP, ListFilter{N: 50})
			if err != nil {
				t.Fatal(err)
			}
			g, l, pu := cs.reset()
			t.Logf("list default: issues=%d more=%v | GET=%d LIST=%d PUT=%d", len(res.Issues), res.More, g, l, pu)
			if g != 2 || l != 0 || pu != 0 {
				t.Fatalf("index-first list not O(1): GET=%d LIST=%d PUT=%d", g, l, pu)
			}
			// 2. Thread read (header + events).
			view, err := s.GetThread(ctx, "acme", "repo", 1, janeP, 0, 50)
			if err != nil {
				t.Fatal(err)
			}
			g, l, pu = cs.reset()
			t.Logf("thread read: events=%d more=%v | GET=%d LIST=%d PUT=%d", len(view.Events), view.EventsMore, g, l, pu)
			// 3. LIST fallback: drop the index, the page must stay complete.
			if err := cs.Delete(ctx, IndexKey("acme", "repo"), ""); err != nil {
				t.Fatal(err)
			}
			res2, err := s.ListIssues(ctx, "acme", "repo", janeP, ListFilter{N: 50})
			if err != nil {
				t.Fatal(err)
			}
			g, l, pu = cs.reset()
			t.Logf("list fallback: issues=%d more=%v | GET=%d LIST=%d PUT=%d", len(res2.Issues), res2.More, g, l, pu)
			want := n
			if want > 50 {
				want = 50
			}
			if len(res2.Issues) != want {
				t.Fatalf("fallback page = %d, want %d", len(res2.Issues), want)
			}
		})
	}
}
