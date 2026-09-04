package checks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore wraps an ObjectStore and counts ops per class (the E6
// budget harness: the report path and the combined-view read prove the
// per-sha cost is bounded by context count, with no LIST outside the
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
	return c.inner.Compose(ctx, dst, sources, opts)
}

func (c *countingStore) reset() {
	atomic.StoreInt64(&c.gets, 0)
	atomic.StoreInt64(&c.puts, 0)
	atomic.StoreInt64(&c.lists, 0)
	atomic.StoreInt64(&c.heads, 0)
}

func (c *countingStore) snapshot() (gets, puts, lists int64) {
	return atomic.LoadInt64(&c.gets), atomic.LoadInt64(&c.puts), atomic.LoadInt64(&c.lists)
}

// TestEvidenceReportBudget measures the report path at 1 and 20
// contexts: first report (Create + index Create + combined LIST + GETs)
// and re-report (CAS Update + index CAS + combined). The E6 entry quotes
// these numbers.
func TestEvidenceReportBudget(t *testing.T) {
	for _, nctx := range []int{1, 20} {
		e := newTestEnv()
		counting := &countingStore{inner: e.store}
		e.svc.Store = counting
		sha := hexSHA(60 + nctx)
		e.knowSHA(sha)
		contexts := make([]string, nctx)
		for i := range contexts {
			contexts[i] = "ctx/" + itoa(i)
		}
		// First reports (one per context), then one re-report.
		for _, c := range contexts {
			if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: c, State: StateSuccess}); err != nil {
				t.Fatalf("report: %v", err)
			}
		}
		gets, puts, lists := counting.snapshot()
		t.Logf("first reports ×%d: %d GETs, %d PUTs, %d LISTs", nctx, gets, puts, lists)
		counting.reset()
		if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: contexts[0], State: StateFailure}); err != nil {
			t.Fatalf("re-report: %v", err)
		}
		gets, puts, lists = counting.snapshot()
		t.Logf("re-report ×1 (of %d): %d GETs, %d PUTs, %d LISTs", nctx, gets, puts, lists)
		// Combined view: 1 LIST + 1 GET per context, 0 PUTs.
		counting.reset()
		view, err := e.svc.Combined(ctx(), "o", "r", sha, reader())
		if err != nil {
			t.Fatalf("combined: %v", err)
		}
		gets, puts, lists = counting.snapshot()
		t.Logf("combined (%d contexts): %d GETs, %d PUTs, %d LISTs", len(view.Statuses), gets, puts, lists)
		if puts != 0 || lists != 1 {
			t.Fatalf("combined shape: puts=%d lists=%d", puts, lists)
		}
		if int64(len(view.Statuses)) != int64(nctx) {
			t.Fatalf("contexts: %d", len(view.Statuses))
		}
	}
}
