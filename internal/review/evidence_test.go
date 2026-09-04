package review

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore wraps an ObjectStore and counts ops per class (the E5
// budget harness: review-summary recompute + gate scan prove the
// event-scan cost is bounded by review count, with no LIST outside the
// low-volume collaboration subtree).
type countingStore struct {
	inner store.ObjectStore
	gets  int64
	puts  int64
	lists int64
	heads int64
}

func (c *countingStore) Backend() string { return c.inner.Backend() }

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	atomic.AddInt64(&c.gets, 1)
	return c.inner.Get(ctx, key, opts)
}

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	atomic.AddInt64(&c.heads, 1)
	return c.inner.Head(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	atomic.AddInt64(&c.puts, 1)
	return c.inner.Put(ctx, key, body, opts)
}

func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	return c.inner.Delete(ctx, key, v)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	atomic.AddInt64(&c.lists, 1)
	return c.inner.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	atomic.AddInt64(&c.lists, 1)
	return c.inner.ListPrefixes(ctx, prefix, fn)
}

func (c *countingStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return c.inner.SignedGetURL(ctx, key, ttl)
}

func (c *countingStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return c.inner.AccelTarget(ctx, key)
}

func (c *countingStore) SupportsCompose() bool { return c.inner.SupportsCompose() }
func (c *countingStore) ComposeIsNative() bool { return c.inner.ComposeIsNative() }

func (c *countingStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	atomic.AddInt64(&c.puts, 1)
	return c.inner.Compose(ctx, dst, sources, opts)
}

func (c *countingStore) snapshot() (gets, puts, lists int64) {
	return atomic.LoadInt64(&c.gets), atomic.LoadInt64(&c.puts), atomic.LoadInt64(&c.lists)
}

func (c *countingStore) reset() {
	atomic.StoreInt64(&c.gets, 0)
	atomic.StoreInt64(&c.puts, 0)
	atomic.StoreInt64(&c.lists, 0)
}

// seedReviews posts n approvals (rotating reviewers) plus t open threads,
// returning the service. All submitters carry roles so the measured path
// is the store cost, not auth.
func seedReviews(t *testing.T, svc *Service, n, threads int) {
	t.Helper()
	ctx := context.Background()
	seedPR(t, svc)
	for i := 0; i < n; i++ {
		who := testPrincipal(fmt.Sprintf("rev%02d", i%10))
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, who,
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < threads; i++ {
		a := testAnchor()
		a.Path = fmt.Sprintf("src/f%02d.go", i)
		if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), a, "nit"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEvidenceReviewSummaryBudget pins the §6 cost model (E5): one summary
// recompute costs 2 LIST (reviews prefix + threads prefix) + one GET per
// review event + one GET per thread header + 2 GETs (requests, PR header)
// + 1 CAS PUT — linear in review/thread count (the collaboration subtree,
// human-scale), zero LIST anywhere else, zero git calls on every path.
func TestEvidenceReviewSummaryBudget(t *testing.T) {
	for _, tc := range []struct{ reviews, threads int }{{5, 2}, {100, 20}} {
		cs := &countingStore{inner: store.NewMemory()}
		svc, roles := testSvc()
		roles.Public = true // revXX carry no bindings; the budget is the store cost, not auth
		svc.Store = cs
		svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
		seedReviews(t, svc, tc.reviews, tc.threads)
		cs.reset()
		sum, err := svc.refreshSummary(context.Background(), testOwner, testRepo, testPR)
		if err != nil {
			t.Fatal(err)
		}
		gets, puts, lists := cs.snapshot()
		t.Logf("recompute at %d reviews/%d threads: %d GETs, %d PUTs, %d LISTs (decision %s, approvals %d)",
			tc.reviews, tc.threads, gets, puts, lists, sum.Decision, sum.Approvals)
		// 2 LIST (reviews + thread headers) + 1 GET per review + 1 GET
		// per thread header + requests GET + header read; the CAS write
		// is the single PUT (the loop converges first try here).
		wantGets := int64(tc.reviews + tc.threads + 2)
		if gets != wantGets || puts != 1 || lists != 2 {
			t.Fatalf("at %d reviews: got %d GETs, %d PUTs, %d LISTs; want %d/1/2",
				tc.reviews, gets, puts, lists, wantGets)
		}
		wantApprovals := tc.reviews
		if wantApprovals > 10 {
			wantApprovals = 10
		}
		if sum.Approvals != wantApprovals || sum.ThreadsTotal != tc.threads {
			t.Fatalf("summary wrong: %+v", sum)
		}
	}
}

// TestEvidenceGateScanBudget pins the gate cost inside the merge task
// (E5): one gate verdict costs the policy GET + the review-event scan
// (1 LIST + 1 GET per review) + the PR header/sidecar reads — bounded by
// review count with its own deadline, no summary trust, no git.
func TestEvidenceGateScanBudget(t *testing.T) {
	for _, n := range []int{5, 100} {
		cs := &countingStore{inner: store.NewMemory()}
		svc, roles := testSvc()
		roles.Public = true // revXX carry no bindings; the budget is the store cost, not auth
		svc.Store = cs
		svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
		seedReviews(t, svc, n, 0)
		put(t, svc, "repos/"+testOwner+"/"+testRepo+"/policy.json", []byte(`{"version":1,"rules":[
			{"name":"pr-gate","match":{"refs":["refs/heads/main"]},
			 "effect":{"required-reviews":{"min_approvals":2}}}]}
		`), store.PutCreate, "")
		cs.reset()
		if err := svc.CheckRequiredReviews(context.Background(), testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); err != nil {
			t.Fatal(err)
		}
		gets, puts, lists := cs.snapshot()
		t.Logf("gate at %d reviews: %d GETs, %d PUTs, %d LISTs", n, gets, puts, lists)
		// policy GET + header GET + sidecar GET + 1 LIST + 1 GET per review.
		if want := int64(n + 3); gets != want || puts != 0 || lists != 1 {
			t.Fatalf("at %d reviews: got %d GETs, %d PUTs, %d LISTs; want %d/0/1",
				n, gets, puts, lists, want)
		}
	}
}
