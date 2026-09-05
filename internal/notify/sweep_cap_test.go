// sweep_cap_test.go — issue #156 regression: per-repo hook cap +
// bounded minute sweep (quiet repos cost one collab_state GET no matter
// how many hooks exist; lagging repos still re-pass).
package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// seedHook bypasses CreateHook (pre-cap repos may hold more hooks than
// the cap allows) with a live, active hook.
func seedHook(t *testing.T, svc *Service, owner, repo, id, url string) {
	t.Helper()
	now := svc.nowUTC().Format(dateTimeFmt)
	h := &Hook{ID: id, URL: url, Events: []string{"commented"}, Active: true,
		CreatedBy: "a", CreatedAt: now, UpdatedAt: now, Version: 1}
	if err := svc.putCreate(ctx(), HookKey(owner, repo, id), mustEncode(t, h)); err != nil {
		t.Fatal(err)
	}
}

// seedActivity appends n comment events directly (reserve + Create, the
// P3 two-step) without waking delivery.
func seedEvents(t *testing.T, svc *Service, owner, repo string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seq, err := svc.reserveSeq(ctx(), owner, repo)
		if err != nil {
			t.Fatal(err)
		}
		ev := ActivityEvent{Seq: seq, Repo: owner + "/" + repo, Action: "commented",
			Kind: "issue", Actor: "a", At: svc.nowUTC().Format(dateTimeFmt)}
		if err := svc.putCreate(ctx(), ActivityKey(owner, repo, seq), mustEncode(t, ev)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebhookCapDefaults(t *testing.T) {
	x := newHarness(t)
	if got := x.svc.maxHooks(); got != DefaultMaxHooksPerRepo {
		t.Fatalf("zero MaxHooks = %d, want default %d", got, DefaultMaxHooksPerRepo)
	}
	x.svc.MaxHooks = -5
	if got := x.svc.maxHooks(); got != DefaultMaxHooksPerRepo {
		t.Fatalf("negative MaxHooks = %d, want default %d", got, DefaultMaxHooksPerRepo)
	}
	x.svc.MaxHooks = 7
	if got := x.svc.maxHooks(); got != 7 {
		t.Fatalf("MaxHooks = %d, want 7", got)
	}
}

func TestWebhookCapEnforced(t *testing.T) {
	x := newHarness(t)
	x.svc.MaxHooks = 3
	x.roles.grant("acme", "repo", "amy@example.com", "admin")
	url := "http://127.0.0.1:9/hook"
	for i := 0; i < 3; i++ {
		if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: &url}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: &url}); err == nil {
		t.Fatal("over-cap create must fail")
	} else if !isErr(err, ErrConflict) {
		t.Fatalf("over-cap create = %v, want conflict", err)
	}
	if got := statusFor(ErrConflict); got != http.StatusConflict {
		t.Fatalf("statusFor = %d, want 409", got)
	}
	// HTTP surface: 409 with a plain-text body (wire convention 07 §2).
	r := httptest.NewRequest("POST", "/acme/repo/api/webhooks", strings.NewReader(`{"url":"http://127.0.0.1:9/hook"}`))
	r.Header.Set("X-Test-Principal", "amy@example.com")
	rec := do(x.handler, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("over-cap POST = %d %q, want 409", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("error content-type = %q, want text/plain", ct)
	}
}

func mkConflict() error { return ErrConflict }

// TestSweepQuietRepoBounded is the #156 regression: a repo with MORE
// hooks than the cap allows (25 seeded directly) costs exactly one
// collab_state GET per minute sweep once drained — no hook LIST, no
// hook/cursor GETs, no writes — independent of total hook count.
func TestSweepQuietRepoBounded(t *testing.T) {
	c := &opCounts{}
	st := countingStore{ObjectStore: store.NewMemory(), c: c}
	svc := New(st, newFakeRoles())
	svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	var delivered int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&delivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seedEvents(t, svc, "acme", "repo", 3)
	const nhooks = 25 // deliberately above DefaultMaxHooksPerRepo
	for i := 0; i < nhooks; i++ {
		seedHook(t, svc, "acme", "repo", "seedhook"+pad16(i), srv.URL)
	}
	// Synchronous drain: delivers 3 events × 25 hooks, clears pending.
	svc.DeliverRepo(ctx(), "acme", "repo")
	if got := atomic.LoadInt64(&delivered); got != 3*nhooks {
		t.Fatalf("delivered = %d, want %d", got, 3*nhooks)
	}
	svc.hookMu.Lock()
	pending := svc.hookPending["acme/repo"]
	svc.hookMu.Unlock()
	if pending {
		t.Fatal("drained repo must not be pending")
	}

	before := c.snapshot()
	svc.sweepWebhooks(ctx())
	cost := c.snapshot()
	cost.get -= before.get
	cost.head -= before.head
	cost.put -= before.put
	cost.del -= before.del
	cost.list -= before.list
	cost.prefixes -= before.prefixes
	t.Logf("quiet sweep over %d hooks: %s", nhooks, cost.String())
	if cost.get != 1 || cost.list != 0 || cost.head != 0 || cost.put != 0 || cost.del != 0 {
		t.Fatalf("quiet sweep must cost exactly 1 GET (collab_state): %s", cost.String())
	}
	if cost.prefixes != 2 {
		t.Fatalf("enumeration = %d prefix-LISTs, want 2 (repos/ + repos/acme/)", cost.prefixes)
	}
}

func pad16(i int) string { return fmt.Sprintf("%016d", i) }

// TestSweepRetriesLaggingRepo preserves the at-least-once contract
// under the gate: a repo whose pass failed (cursor behind head) stays
// pending, and the next sweep re-passes it instead of skipping.
func TestSweepRetriesLaggingRepo(t *testing.T) {
	x := newHarness(t)
	seedEvents(t, x.svc, "acme", "repo", 1)
	dead := "http://127.0.0.1:9/hook" // refused: every POST fails
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: &dead})
	if err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 0 {
		t.Fatalf("failed pass must hold cursor at 0, got %d", cur)
	}
	x.svc.hookMu.Lock()
	pending := x.svc.hookPending["acme/repo"]
	x.svc.hookMu.Unlock()
	if !pending {
		t.Fatal("failed pass must leave the repo pending")
	}
	d0 := x.svc.ReadDeliveries(ctx(), "acme", "repo", hk.ID)
	x.svc.sweepWebhooks(ctx()) // must schedule a retry pass, not skip
	waitTaskFinished(t, x.svc, "acme/repo", TaskKindWebhooks)
	d1 := x.svc.ReadDeliveries(ctx(), "acme", "repo", hk.ID)
	if len(d1.Entries) != len(d0.Entries)+1 {
		t.Fatalf("retry pass must attempt delivery again: %d → %d entries", len(d0.Entries), len(d1.Entries))
	}
	x.svc.hookMu.Lock()
	still := x.svc.hookPending["acme/repo"]
	x.svc.hookMu.Unlock()
	if !still {
		t.Fatal("still-failing repo must stay pending")
	}
}

// TestCreateHookArmsQuietRepo: a hook created onto a quiet repo's
// backlog arms the sweep gate (the allocator does not advance, so
// without the mark the backlog would wait for the next emission).
func TestCreateHookArmsQuietRepo(t *testing.T) {
	x := newHarness(t)
	seedEvents(t, x.svc, "acme", "repo", 1)
	url := "http://127.0.0.1:9/hook"
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: &url}); err != nil {
		t.Fatal(err)
	}
	x.svc.hookMu.Lock()
	armed := x.svc.hookPending["acme/repo"]
	x.svc.hookMu.Unlock()
	if !armed {
		t.Fatal("new active hook must arm the sweep")
	}
	// An inactive hook arms nothing.
	inactive := false
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo2", "a",
		HookSpec{URL: &url, Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	x.svc.hookMu.Lock()
	armed2 := x.svc.hookPending["acme/repo2"]
	x.svc.hookMu.Unlock()
	if armed2 {
		t.Fatal("inactive hook must not arm the sweep")
	}
	// Activating it arms like a create.
	active := true
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo2", mustHookIDFor(t, x.svc, "repo2"),
		HookSpec{Active: &active}); err != nil {
		t.Fatal(err)
	}
	x.svc.hookMu.Lock()
	armed3 := x.svc.hookPending["acme/repo2"]
	x.svc.hookMu.Unlock()
	if !armed3 {
		t.Fatal("activation must arm the sweep")
	}
}

func mustHookIDFor(t *testing.T, svc *Service, repo string) string {
	t.Helper()
	hooks, err := svc.ListHooks(ctx(), "acme", repo)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("hooks = %+v, %v", hooks, err)
	}
	return hooks[0].ID
}

// failStateStore fails reads of the collab_state key (transient backend
// fault injection for the completion check); all other keys delegate.
type failStateStore struct {
	store.ObjectStore
	err error
}

func (s failStateStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if strings.HasSuffix(key, "meta/collab_state.json") {
		return nil, s.err
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

// TestSweepRepoEdges covers the gate branches: corrupt state, hookless
// repos (seen advances, stale pending disarms), all-inactive repos (no
// launch when quiet; one healing pass when pending), and an unreadable
// head at completion (fail closed toward retry).
func TestSweepRepoEdges(t *testing.T) {
	x := newHarness(t)

	// Corrupt collab_state: skip without touching watermarks.
	writeRaw(t, x.svc.Store, CollabStateKey("acme", "bad"), []byte("{bad"))
	x.svc.sweepWebhooksRepo(ctx(), "acme", "bad", "acme/bad")
	x.svc.hookMu.Lock()
	_, seenBad := x.svc.hookSeen["acme/bad"]
	x.svc.hookMu.Unlock()
	if seenBad {
		t.Fatal("corrupt state must not advance the watermark")
	}

	// Hookless repo with activity: one LIST, seen advances, stale
	// pending disarms (peer-instance delete heals here).
	seedEvents(t, x.svc, "acme", "bare", 2)
	x.svc.hookMu.Lock()
	x.svc.hookPending["acme/bare"] = true
	x.svc.hookMu.Unlock()
	x.svc.sweepWebhooksRepo(ctx(), "acme", "bare", "acme/bare")
	x.svc.hookMu.Lock()
	seen, pending := x.svc.hookSeen["acme/bare"], x.svc.hookPending["acme/bare"]
	x.svc.hookMu.Unlock()
	if seen != 2 || pending {
		t.Fatalf("hookless repo: seen=%d pending=%v, want 2 false", seen, pending)
	}
	// Second pass is gate-quiet (no hook LIST).
	hooks, err := x.svc.ListHooks(ctx(), "acme", "bare")
	if err != nil || len(hooks) != 0 {
		t.Fatalf("hooks = %+v, %v", hooks, err)
	}

	// All-inactive + quiet: no task launched, seen still advances.
	url := "http://127.0.0.1:9/hook"
	inactive := false
	if _, err := x.svc.CreateHook(ctx(), "acme", "parked", "a",
		HookSpec{URL: &url, Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	seedEvents(t, x.svc, "acme", "parked", 1)
	x.svc.hookMu.Lock()
	x.svc.hookPending["acme/parked"] = false
	x.svc.hookMu.Unlock()
	x.svc.sweepWebhooksRepo(ctx(), "acme", "parked", "acme/parked")
	if rec := x.svc.TaskStatus("acme/parked", TaskKindWebhooks); rec != nil {
		t.Fatalf("all-inactive repo must not launch a pass: %+v", rec)
	}

	// All-inactive + pending: one healing pass clears the mark.
	x.svc.hookMu.Lock()
	x.svc.hookPending["acme/parked"] = true
	x.svc.hookMu.Unlock()
	x.svc.sweepWebhooksRepo(ctx(), "acme", "parked", "acme/parked")
	waitTaskFinished(t, x.svc, "acme/parked", TaskKindWebhooks)
	x.svc.hookMu.Lock()
	healed := x.svc.hookPending["acme/parked"]
	x.svc.hookMu.Unlock()
	if healed {
		t.Fatal("healing pass must clear pending on an all-inactive repo")
	}

	// Unreadable head at completion: fail closed (pending stays true).
	st := failStateStore{ObjectStore: store.NewMemory(), err: fmt.Errorf("boom")}
	svc := New(st, newFakeRoles())
	svc.Now = x.svc.Now
	seedHook(t, svc, "acme", "flaky", "flakyhook00000000000001", url)
	ev := ActivityEvent{Seq: 1, Repo: "acme/flaky", Action: "commented",
		Kind: "issue", Actor: "a", At: svc.nowUTC().Format(dateTimeFmt)}
	if err := svc.putCreate(ctx(), ActivityKey("acme", "flaky", 1), mustEncode(t, ev)); err != nil {
		t.Fatal(err)
	}
	svc.DeliverRepo(ctx(), "acme", "flaky")
	svc.hookMu.Lock()
	flaky := svc.hookPending["acme/flaky"]
	_, flakySeen := svc.hookSeen["acme/flaky"]
	svc.hookMu.Unlock()
	if !flaky || flakySeen {
		t.Fatalf("unreadable head: pending=%v seen-set=%v, want true false", flaky, flakySeen)
	}
}
