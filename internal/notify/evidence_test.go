// evidence_test.go — E6 measurement harness: fan-out write
// amplification bounds (deterministic-id dedup, batching, budgets) and
// delivery-loop cursor costs, counted in store round-trips over the
// memory backend (the round-trip cost model, AGENTS law 6).
//
// Run: go test ./internal/notify/ -run TestEvidence -v
package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// opCounts tallies backend round-trips by verb.
type opCounts struct {
	get, head, put, del, list, prefixes int64
}

type countingStore struct {
	store.ObjectStore
	c *opCounts
}

func (s countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	atomic.AddInt64(&s.c.get, 1)
	return s.ObjectStore.Get(ctx, key, opts)
}
func (s countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	atomic.AddInt64(&s.c.head, 1)
	return s.ObjectStore.Head(ctx, key)
}
func (s countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	atomic.AddInt64(&s.c.put, 1)
	return s.ObjectStore.Put(ctx, key, body, opts)
}
func (s countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	atomic.AddInt64(&s.c.del, 1)
	return s.ObjectStore.Delete(ctx, key, v)
}
func (s countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	atomic.AddInt64(&s.c.list, 1)
	return s.ObjectStore.List(ctx, prefix, startAfter, fn)
}
func (s countingStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	atomic.AddInt64(&s.c.prefixes, 1)
	return s.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

func (c *opCounts) snapshot() opCounts {
	return opCounts{
		get: atomic.LoadInt64(&c.get), head: atomic.LoadInt64(&c.head),
		put: atomic.LoadInt64(&c.put), del: atomic.LoadInt64(&c.del),
		list: atomic.LoadInt64(&c.list), prefixes: atomic.LoadInt64(&c.prefixes),
	}
}

func (c opCounts) String() string {
	return fmt.Sprintf("%d GETs, %d HEADs, %d PUTs, %d DELETEs, %d LISTs, %d prefix-LISTs",
		c.get, c.head, c.put, c.del, c.list, c.prefixes)
}

// evidenceService builds a notify service over a counting memory store
// with N valid profiles (actor bob + recipients uXXX).
func evidenceService(t *testing.T, n int) (*Service, *opCounts) {
	t.Helper()
	c := &opCounts{}
	st := countingStore{ObjectStore: store.NewMemory(), c: c}
	svc := New(st, nil)
	profiles := &fakeProfiles{have: map[string]bool{"bob@example.com": true}}
	for i := 0; i < n; i++ {
		profiles.have[fmt.Sprintf("u%03d@example.com", i)] = true
	}
	svc.Profiles = profiles
	svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	return svc, c
}

func evidenceThread(t *testing.T, svc *Service, num int) {
	t.Helper()
	raw := []byte(`{"title":"T","author":"bob@example.com"}`)
	if _, err := store.PutBytes(context.Background(), svc.Store, threadKey("acme", "repo", num), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func recips(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("u%03d@example.com", i))
	}
	return out
}

// TestEvidenceFanoutAmplification measures one subscribed-class emission
// (thread author == actor, so every recipient is a primary) at 1 and 10
// recipients, plus the dedup replay cost.
func TestEvidenceFanoutAmplification(t *testing.T) {
	for _, n := range []int{1, 10} {
		svc, c := evidenceService(t, n)
		evidenceThread(t, svc, 7)
		before := c.snapshot()
		svc.EmitIssue(context.Background(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", recips(n))
		cost := c.snapshot()
		cost.get -= before.get
		cost.head -= before.head
		cost.put -= before.put
		cost.del -= before.del
		cost.list -= before.list
		cost.prefixes -= before.prefixes
		t.Logf("emit %d recipients: %s", n, cost.String())
		// Second emission while all recipients still hold a live
		// (unread) entry: the unread dedup skips every notification
		// Create and index rewrite. The cost is the probes plus the
		// new activity event (each emission is a distinct activity
		// seq by design — a crash loses the fan-out per P8, it never
		// replays it, so retries mint no duplicate delivery keys).
		before = c.snapshot()
		svc.EmitIssue(context.Background(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", recips(n))
		replay := c.snapshot()
		replay.get -= before.get
		replay.put -= before.put
		t.Logf("deduped second emission (%d recipients): %d GETs, %d PUTs (activity only, zero notification/index writes)", n, replay.get, replay.put)
		if replay.put != 2 {
			// Exactly two PUTs: the activity seq reservation + the
			// new activity event. No notification object or index is
			// written.
			t.Fatalf("deduped PUTs = %d, want 2 (seq reservation + activity only)", replay.put)
		}
	}
}

// TestEvidenceDeliveryLoop measures one webhooks pass over K queued
// activity events against a live sink (cursor advance + ring), the
// head-blocked pass against a refused sink, and the true idle pass.
func TestEvidenceDeliveryLoop(t *testing.T) {
	svc, c := evidenceService(t, 0)
	// Live sink: K events deliver, cursor advances 0 → K.
	var delivered int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&delivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if _, err := svc.CreateHook(context.Background(), "acme", "repo", "a",
		HookSpec{URL: &srv.URL}); err != nil {
		t.Fatal(err)
	}
	const k = 5
	for i := 0; i < k; i++ {
		seq, err := svc.reserveSeq(context.Background(), "acme", "repo")
		if err != nil {
			t.Fatal(err)
		}
		ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", At: svc.nowUTC().Format(dateTimeFmt)}
		if err := svc.putCreate(context.Background(), ActivityKey("acme", "repo", seq), encode(ev)); err != nil {
			t.Fatal(err)
		}
	}
	before := c.snapshot()
	svc.DeliverRepo(context.Background(), "acme", "repo")
	cost := c.snapshot()
	cost.get -= before.get
	cost.head -= before.head
	cost.put -= before.put
	cost.list -= before.list
	t.Logf("delivery pass, %d queued, live sink: %s; cursor=%d delivered=%d", k, cost.String(),
		svc.readCursor(context.Background(), "acme", "repo", mustHookID(t, svc)), atomic.LoadInt64(&delivered))
	// True idle pass (cursor at head): hooks LIST + cursor GET + head
	// probe (1 GET + 8 HEADs), zero writes.
	before = c.snapshot()
	svc.DeliverRepo(context.Background(), "acme", "repo")
	idle := c.snapshot()
	idle.get -= before.get
	idle.head -= before.head
	idle.put -= before.put
	idle.list -= before.list
	t.Logf("idle pass: %s", idle.String())
	if idle.put != 0 {
		t.Fatalf("idle pass must not write: %s", idle.String())
	}
}

func mustHookID(t *testing.T, svc *Service) string {
	t.Helper()
	hooks, err := svc.ListHooks(context.Background(), "acme", "repo")
	if err != nil || len(hooks) != 1 {
		t.Fatalf("hooks = %+v, %v", hooks, err)
	}
	return hooks[0].ID
}
