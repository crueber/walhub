// tray_cost_test.go — issue #157: pins the index-first tray cost model.
// A counting store decorates the memory backend and asserts per-request
// store calls: an index-covered page costs one GET (the index) + one HEAD
// per distinct repo in the examined window — no LIST, no per-entry
// probes; only pages reaching past the index window pay one LIST +
// overflow GETs.
package notify

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// countStore counts raw store calls (Get/Head/List) around a delegate.
type countStore struct {
	store.ObjectStore
	mu        sync.Mutex
	gets      int
	heads     int
	lists     int
	failHeads bool
	headErr   error
}

func (s *countStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *countStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	s.mu.Lock()
	s.heads++
	s.mu.Unlock()
	if s.failHeads {
		return nil, s.headErr
	}
	return s.ObjectStore.Head(ctx, key)
}

func (s *countStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (s *countStore) snapshot() (gets, heads, lists int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.heads, s.lists
}

func (s *countStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets, s.heads, s.lists = 0, 0, 0
}

// th is the minimal test-helper surface (*testing.T and *testing.B both
// satisfy it) so seeds can serve tests and benchmarks alike.
type th interface {
	Helper()
	Fatalf(string, ...any)
}

func costPut(h th, st store.ObjectStore, key string, v any) {
	h.Helper()
	raw, err := encode(v)
	if err != nil {
		h.Fatalf("costPut encode %s: %v", key, err)
	}
	if _, err := store.PutBytes(context.Background(), st, key, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		h.Fatalf("costPut %s: %v", key, err)
	}
}

func costSeedRepo(h th, st store.ObjectStore, owner, repo string) {
	h.Helper()
	if _, err := store.PutBytes(context.Background(), st, manifestKey(owner, repo), []byte("manifest"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		if !store.IsPreconditionFailed(err) {
			h.Fatalf("costSeedRepo %s/%s: %v", owner, repo, err)
		}
	}
}

// seedTrayWindow writes a full TrayPageSize index window (alternating two
// live repos, distinct timestamps) plus older overflow objects with no
// index row (the trimmed tail). It returns the newest-first index ids and
// overflow ids (newest first).
func seedTrayWindow(h th, svc *Service, now time.Time, who string, overflow int) (indexIDs, overflowIDs []string) {
	h.Helper()
	entries := make([]IndexEntry, 0, TrayPageSize)
	for i := 0; i < TrayPageSize; i++ {
		id := fmt.Sprintf("%032x", 1000+i)
		repo := "acme/alpha"
		if i%2 == 1 {
			repo = "acme/beta"
		}
		entries = append(entries, IndexEntry{
			ID: id, Repo: repo, Num: i + 1, Kind: "issue",
			Reason: ReasonSubscribed, Title: fmt.Sprintf("T%d", i),
			State: StateUnread, At: now.Add(-time.Duration(i) * time.Minute).Format(dateTimeFmt),
		})
		indexIDs = append(indexIDs, id)
	}
	costPut(h, svc.Store, NotifIndexKey(who), IndexDoc{
		Version: 1, UnreadCount: TrayPageSize, Entries: entries,
	})
	for i := 0; i < overflow; i++ {
		id := fmt.Sprintf("%032x", 2000+i)
		costPut(h, svc.Store, NotifKey(who, id), Notification{
			ID: id, Repo: "acme/alpha", Num: 1000 + i, Kind: "issue",
			Reason: ReasonSubscribed, Title: fmt.Sprintf("O%d", i),
			State:     StateUnread,
			CreatedAt: now.Add(-time.Duration(TrayPageSize+i) * time.Minute).Format(dateTimeFmt),
		})
		overflowIDs = append(overflowIDs, id)
	}
	return indexIDs, overflowIDs
}

func newCostService(h th, now time.Time) *Service {
	h.Helper()
	svc := New(store.NewMemory(), nil)
	svc.Now = func() time.Time { return now }
	costSeedRepo(h, svc.Store, "acme", "alpha")
	costSeedRepo(h, svc.Store, "acme", "beta")
	return svc
}

// TestTrayIndexCoveredNoList pins the hot path: a first page covered by
// the index costs one GET (the index) + one HEAD per distinct repo in the
// examined window — no LIST, no per-entry probes, newest-first.
func TestTrayIndexCoveredNoList(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := newCostService(t, now)
	who := "amy@example.com"
	indexIDs, _ := seedTrayWindow(t, svc, now, who, 30)
	c := &countStore{ObjectStore: svc.Store}
	svc.Store = c

	got, more := svc.Tray(ctx(), who, "", "", 10)
	if len(got) != 10 || !more {
		t.Fatalf("page = %d/%v, want 10/true", len(got), more)
	}
	for i, nt := range indexIDs[:10] {
		if got[i].ID != nt {
			t.Fatalf("row %d = %s, want %s", i, got[i].ID, nt)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt > got[i-1].CreatedAt {
			t.Fatalf("not newest-first at %d: %q after %q", i, got[i].CreatedAt, got[i-1].CreatedAt)
		}
	}
	gets, heads, lists := c.snapshot()
	if lists != 0 {
		t.Fatalf("covered page LISTed %d times, want 0", lists)
	}
	if gets != 1 {
		t.Fatalf("covered page GETs = %d, want 1 (index only)", gets)
	}
	if heads != 2 {
		t.Fatalf("covered page HEADs = %d, want 2 (one per distinct repo)", heads)
	}
}

// TestTrayIndexSecondPageNoList pins cursor paging inside the window: the
// second page is still index-covered (no LIST).
func TestTrayIndexSecondPageNoList(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := newCostService(t, now)
	who := "amy@example.com"
	indexIDs, _ := seedTrayWindow(t, svc, now, who, 30)
	c := &countStore{ObjectStore: svc.Store}
	svc.Store = c

	got, more := svc.Tray(ctx(), who, "", indexIDs[9], 10)
	if len(got) != 10 || !more {
		t.Fatalf("page = %d/%v, want 10/true", len(got), more)
	}
	for i, nt := range indexIDs[10:20] {
		if got[i].ID != nt {
			t.Fatalf("row %d = %s, want %s", i, got[i].ID, nt)
		}
	}
	if _, _, lists := c.snapshot(); lists != 0 {
		t.Fatalf("in-window cursor page LISTed %d times, want 0", lists)
	}
}

// TestTrayOverflowPageLists pins the overflow contract: a page past the
// index window pays exactly one LIST and serves the overflow tail.
func TestTrayOverflowPageLists(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := newCostService(t, now)
	who := "amy@example.com"
	indexIDs, overflowIDs := seedTrayWindow(t, svc, now, who, 30)
	c := &countStore{ObjectStore: svc.Store}
	svc.Store = c

	got, more := svc.Tray(ctx(), who, "", indexIDs[len(indexIDs)-1], 10)
	if len(got) != 10 || !more {
		t.Fatalf("page = %d/%v, want 10/true", len(got), more)
	}
	for i, nt := range overflowIDs[:10] {
		if got[i].ID != nt {
			t.Fatalf("row %d = %s, want %s", i, got[i].ID, nt)
		}
	}
	gets, heads, lists := c.snapshot()
	if lists != 1 {
		t.Fatalf("overflow page LISTs = %d, want 1", lists)
	}
	if gets != 1+30 {
		t.Fatalf("overflow page GETs = %d, want %d (index + overflow)", gets, 1+30)
	}
	if heads > 2 {
		t.Fatalf("overflow page HEADs = %d, want <= 2 (memoized per repo)", heads)
	}
}

// TestTrayMalformedRepoKept pins the fail-open liveness rule: an entry
// with a malformed repo reference is served (no probe possible).
func TestTrayMalformedRepoKept(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := newCostService(t, now)
	who := "amy@example.com"
	at := now.Format(dateTimeFmt)
	costPut(t, svc.Store, NotifIndexKey(who), IndexDoc{
		Version: 1, UnreadCount: 2,
		Entries: []IndexEntry{
			{ID: fmt.Sprintf("%032x", 1), Repo: "bogus", Num: 1, Kind: "issue", Reason: ReasonSubscribed, Title: "B", State: StateUnread, At: at},
			{ID: fmt.Sprintf("%032x", 2), Repo: "acme/alpha", Num: 2, Kind: "issue", Reason: ReasonSubscribed, Title: "L", State: StateUnread, At: at},
		},
	})
	got, more := svc.Tray(ctx(), who, "", "", 50)
	if len(got) != 2 || more {
		t.Fatalf("page = %d/%v, want 2/false", len(got), more)
	}
}

// TestTrayProbeErrorFailOpen pins fail-open liveness: HEAD faults keep
// entries instead of mass-hiding the tray (retention reconciles later).
func TestTrayProbeErrorFailOpen(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := newCostService(t, now)
	who := "amy@example.com"
	at := now.Format(dateTimeFmt)
	costPut(t, svc.Store, NotifIndexKey(who), IndexDoc{
		Version: 1, UnreadCount: 1,
		Entries: []IndexEntry{
			{ID: fmt.Sprintf("%032x", 1), Repo: "acme/alpha", Num: 1, Kind: "issue", Reason: ReasonSubscribed, Title: "L", State: StateUnread, At: at},
		},
	})
	c := &countStore{ObjectStore: svc.Store, failHeads: true, headErr: fmt.Errorf("head boom")}
	inner := svc.Store
	c.ObjectStore = inner
	svc.Store = c
	got, _ := svc.Tray(ctx(), who, "", "", 50)
	if len(got) != 1 {
		t.Fatalf("probe error hid the tray: %d rows", len(got))
	}
}

// TestSortNotificationsLarge pins the #157 sort replacement at scale:
// 2000 shuffled rows order newest-first by (CreatedAt desc, ID asc).
func TestSortNotificationsLarge(t *testing.T) {
	const n = 2000
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	all := make([]Notification, 0, n)
	for i := 0; i < n; i++ {
		all = append(all, Notification{
			ID:        fmt.Sprintf("%032x", i),
			CreatedAt: base.Add(time.Duration(i%97) * time.Minute).Format(dateTimeFmt),
		})
	}
	// Deterministic shuffle (fixed-stride Fisher-Yates).
	for i := n - 1; i > 0; i-- {
		j := (i * 31) % (i + 1)
		all[i], all[j] = all[j], all[i]
	}
	sortNotifications(all)
	for i := 1; i < n; i++ {
		a, b := all[i-1], all[i]
		if a.CreatedAt < b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID > b.ID) {
			t.Fatalf("misordered at %d: %+v before %+v", i, a, b)
		}
	}
}

// BenchmarkTrayIndexCovered measures the covered hot path (no LIST).
func BenchmarkTrayIndexCovered(b *testing.B) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := newCostService(b, now)
	who := "amy@example.com"
	_, _ = seedTrayWindow(b, svc, now, who, 500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got, _ := svc.Tray(context.Background(), who, "", "", 25); len(got) != 25 {
			b.Fatalf("page = %d", len(got))
		}
	}
}
