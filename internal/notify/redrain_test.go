// redrain_test.go — issue #77: undrained notify-fanout is restart-safe.
//
// The activity payload is the only queue that survives a restart, but the
// (repo, kind) task table is in-memory: a notify-fanout task in flight at
// process death left its seqs attached to a table that no longer exists.
// These tests pin the redrain contract: overflow/shortfall emissions mark
// the payload pending, drains record per-seq completion, and the sweep
// re-enqueues pending undrained seqs — driven explicitly, no sleeps.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// waitFanoutQuiescent blocks until repo's fanout task finishes. After a
// sweep returns, every enqueued seq is either attached to a running entry
// (wait for its WaitGroup) or already drained by a finished leader
// (endIfQuiescent runs only after the last drain) — deterministic either
// way, no poll loop.
func waitFanoutQuiescent(t *testing.T, s *Service, repo string) {
	t.Helper()
	s.tasks.mu.Lock()
	e, ok := s.tasks.running[taskKey(repo, TaskKindFanout)]
	s.tasks.mu.Unlock()
	if ok {
		e.wg.Wait()
	}
}

// seedActivity writes one activity event directly (the durable state a
// crash leaves behind: event present, no task, no completion record).
// The allocator head is written separately (once per repo).
func seedActivity(t *testing.T, st store.ObjectStore, at, owner, repo string, seq int, pending bool, recips ...activityRecipient) {
	t.Helper()
	ev := ActivityEvent{Seq: seq, Repo: owner + "/" + repo, Action: "commented",
		Num: 7, Kind: "issue", Actor: "bob@example.com", Title: "T", At: at,
		Payload: mustEncode(t, activityPayload{Class: "subscribed", Recipients: recips, FanoutPending: pending})}
	writeRaw(t, st, ActivityKey(owner, repo, seq), mustEncode(t, ev))
}

// TestSweepRedrainsUndrainedFanoutAfterRestart seeds the exact durable
// state of a process death mid-drain (pending events, no task table, no
// completion records), builds a FRESH service over the same store (the
// restart — empty task table and empty sweep high-water), drives the
// sweep explicitly, and asserts every pending recipient is delivered
// exactly once while sync-complete events are never re-drained.
func TestSweepRedrainsUndrainedFanoutAfterRestart(t *testing.T) {
	st := store.NewMemory()
	at := "2026-09-04T12:00:00Z"
	recips := func(ps ...string) []activityRecipient {
		out := make([]activityRecipient, 0, len(ps))
		for _, p := range ps {
			out = append(out, activityRecipient{Principal: p, Reason: ReasonSubscribed})
		}
		return out
	}
	// Seq 1: overflow emission that never drained (2 recipients).
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 3}))
	seedActivity(t, st, at, "acme", "repo", 1, true, recips("amy@example.com", "carol@example.com")...)
	// Seq 2: sync-complete emission — already fanned out on the request
	// path. Its notification exists; the sweep must not touch it (no
	// completion record is ever written for non-pending seqs).
	seedActivity(t, st, at, "acme", "repo", 2, false, recips("dave@example.com")...)
	id2 := NotificationID("dave@example.com", "acme/repo", 7, ReasonSubscribed, 2)
	writeRaw(t, st, NotifKey("dave@example.com", id2), mustEncode(t, Notification{ID: id2, Repo: "acme/repo",
		Num: 7, Kind: "issue", Reason: ReasonSubscribed, State: StateUnread, CreatedAt: at}))
	// Seq 3: shortfall emission that never drained (1 recipient).
	seedActivity(t, st, at, "acme", "repo", 3, true, recips("erin@example.com")...)

	// The restart: a fresh service over the same bucket. No task table
	// entries, no sweep high-water — exactly what process death leaves.
	svc2 := New(st, nil)
	svc2.sweepFanout(ctx())
	waitFanoutQuiescent(t, svc2, "acme/repo")

	// Every pending recipient delivered exactly once (object + index).
	for _, p := range []string{"amy@example.com", "carol@example.com", "erin@example.com"} {
		n := 0
		_ = st.List(ctx(), NotifPrefix(p), "", func(m store.ObjectMeta) error {
			if len(m.Key) > len("/index.json") && m.Key[len(m.Key)-len("/index.json"):] != "/index.json" {
				n++
			}
			return nil
		})
		if n != 1 {
			t.Fatalf("%s notifications = %d, want 1", p, n)
		}
		raw, _, err := store.GetBytes(ctx(), st, NotifIndexKey(p), store.GetOptions{})
		if err != nil {
			t.Fatalf("%s index missing: %v", p, err)
		}
		var ix IndexDoc
		_ = json.Unmarshal(raw, &ix)
		if ix.UnreadCount != 1 {
			t.Fatalf("%s unread = %d, want 1", p, ix.UnreadCount)
		}
	}
	// Completion records exist for the drained seqs only.
	for _, seq := range []int{1, 3} {
		if !svc2.fanoutDone(ctx(), "acme", "repo", seq) {
			t.Fatalf("seq %d must carry a completion record after the redrain", seq)
		}
	}
	if svc2.fanoutDone(ctx(), "acme", "repo", 2) {
		t.Fatal("sync-complete seq 2 must never gain a completion record (it was never drained)")
	}
	if n := func() int {
		m := 0
		_ = st.List(ctx(), NotifPrefix("dave@example.com"), "", func(o store.ObjectMeta) error {
			if len(o.Key) > len("/index.json") && o.Key[len(o.Key)-len("/index.json"):] != "/index.json" {
				m++
			}
			return nil
		})
		return m
	}(); n != 1 {
		t.Fatalf("dave notifications = %d, want 1 (no redrain duplicate)", n)
	}
	if rec := svc2.TaskStatus("acme/repo", TaskKindFanout); rec == nil || rec.State != TaskFinished {
		t.Fatalf("redrain task record = %+v", rec)
	}

	// A second sweep is a no-op: high-water skips the window and the
	// completion records skip the pending seqs — zero writes, no task.
	c := &opCounts{}
	svc2.Store = countingStore{ObjectStore: st, c: c}
	svc2.sweepFanout(ctx())
	if got := c.snapshot(); got.put != 0 || got.del != 0 {
		t.Fatalf("quiescent re-sweep must write nothing: %s", got.String())
	}
	svc2.tasks.mu.Lock()
	_, running := svc2.tasks.running[taskKey("acme/repo", TaskKindFanout)]
	svc2.tasks.mu.Unlock()
	if running {
		t.Fatal("quiescent re-sweep must start no fanout task")
	}
}

// TestOverflowDrainLeavesCompletionRecord drives the real overflow path
// (emission → pending payload → task → completion record) and asserts the
// payload flag the sweep keys on.
func TestOverflowDrainLeavesCompletionRecord(t *testing.T) {
	x := newHarness(t)
	recips := []string{}
	for i := 0; i < MaxSyncRecipients+3; i++ {
		recips = append(recips, fmt.Sprintf("u%03d@example.com", i))
	}
	x.writeThread(t, "acme", "repo", 7, "T", "bob@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", recips)
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	if ev == nil {
		t.Fatal("overflow must append the activity event")
	}
	var payload activityPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	if !payload.FanoutPending {
		t.Fatal("overflow events must carry fanout_pending for the restart sweep")
	}
	waitFanoutQuiescent(t, x.svc, "acme/repo")
	if !x.svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("drained overflow seq must carry a completion record")
	}
	for _, p := range recips {
		if countNotifs(t, x, p) != 1 {
			t.Fatalf("%s notifications != 1", p)
		}
	}
}

// TestSweepFanoutEdges covers the redrain failure branches: corrupt and
// unreadable allocators never enqueue, and high-water for deleted repos
// is pruned by the next sweep.
func TestSweepFanoutEdges(t *testing.T) {
	x := newHarness(t)
	// Corrupt allocator: skipped, nothing enqueued.
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", 1),
		mustEncode(t, ActivityEvent{Seq: 1, Repo: "acme/repo", Action: "commented"}))
	writeRaw(t, x.svc.Store, CollabStateKey("acme", "repo"), []byte("{corrupt"))
	x.svc.sweepFanout(ctx())
	x.svc.tasks.mu.Lock()
	_, running := x.svc.tasks.running[taskKey("acme/repo", TaskKindFanout)]
	x.svc.tasks.mu.Unlock()
	if running {
		t.Fatal("corrupt allocator must enqueue nothing")
	}
	// Unreadable store: sweeps are best-effort, never fatal.
	broken := New(errStore{store.NewMemory(), fmt.Errorf("store down")}, nil)
	broken.sweepFanout(ctx())
	broken.RunRetention(ctx())
	// Deleted repo: its high-water is pruned by the next sweep.
	x.svc.fanoutMu.Lock()
	x.svc.fanoutSeen["acme/gone"] = 5
	x.svc.fanoutMu.Unlock()
	x.svc.sweepFanout(ctx())
	x.svc.fanoutMu.Lock()
	_, kept := x.svc.fanoutSeen["acme/gone"]
	x.svc.fanoutMu.Unlock()
	if kept {
		t.Fatal("deleted-repo high-water must be pruned")
	}
}

// TestSweepFanoutProbeBound pins the maintainer-pass bound: a repo whose
// allocator sits thousands of seqs out (pure crash-reserved gaps, no
// events) costs a bounded window of probes per pass and converges across
// passes — never O(head) GETs in one pass.
func TestSweepFanoutProbeBound(t *testing.T) {
	st := store.NewMemory()
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 5000}))
	c := &opCounts{}
	svc := New(countingStore{ObjectStore: st, c: c}, nil)
	before := c.snapshot()
	svc.sweepFanout(ctx())
	if got := c.snapshot(); got.get-before.get > maxFanoutRedrainSeqs+8 {
		t.Fatalf("redrain GETs = %d, want <= %d for a 5000-seq gap window",
			got.get-before.get, maxFanoutRedrainSeqs+8)
	}
	svc.tasks.mu.Lock()
	_, running := svc.tasks.running[taskKey("acme/repo", TaskKindFanout)]
	svc.tasks.mu.Unlock()
	if running {
		t.Fatal("pure gaps must enqueue nothing")
	}
}

// flakeActivity fails GETs of one key n times, then delegates (a transient
// store failure the next pass must recover from).
type flakeActivity struct {
	store.ObjectStore
	key string
	n   int
}

func (f *flakeActivity) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == f.key && f.n > 0 {
		f.n--
		return nil, errors.New("flake")
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

// TestSweepFanoutTransientProbeRetries pins the redrain failure direction:
// a transient store error on an activity probe must stop the window WITHOUT
// advancing the high-water past the unprobed seq — the healed next pass
// redrains it. (Before the fix, readActivity conflated errors with gaps and
// the high-water advanced past the pending seq, orphaning it forever.)
func TestSweepFanoutTransientProbeRetries(t *testing.T) {
	st := store.NewMemory()
	at := "2026-09-04T12:00:00Z"
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 1}))
	seedActivity(t, st, at, "acme", "repo", 1, true,
		activityRecipient{Principal: "amy@example.com", Reason: ReasonSubscribed})
	flake := &flakeActivity{ObjectStore: st, key: ActivityKey("acme", "repo", 1), n: 1}
	svc := New(flake, nil)

	svc.sweepFanout(ctx())
	svc.tasks.mu.Lock()
	_, running := svc.tasks.running[taskKey("acme/repo", TaskKindFanout)]
	svc.tasks.mu.Unlock()
	if running {
		t.Fatal("failed probe must enqueue nothing")
	}
	if svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("failed probe must write no completion record")
	}
	svc.fanoutMu.Lock()
	seen := svc.fanoutSeen["acme/repo"]
	svc.fanoutMu.Unlock()
	if seen != 0 {
		t.Fatalf("high-water = %d after a failed probe, want 0 (window must stop, not skip)", seen)
	}

	// Healed store: the next pass probes seq 1 again and drains it.
	svc.sweepFanout(ctx())
	waitFanoutQuiescent(t, svc, "acme/repo")
	if !svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("healed pass must drain the retried seq")
	}
	n := 0
	_ = st.List(ctx(), NotifPrefix("amy@example.com"), "", func(m store.ObjectMeta) error {
		if len(m.Key) > len("/index.json") && m.Key[len(m.Key)-len("/index.json"):] != "/index.json" {
			n++
		}
		return nil
	})
	if n != 1 {
		t.Fatalf("retried seq must deliver exactly once, got %d", n)
	}
}

// TestRetentionDeletesFanoutDoneMarkers pins the §9 cleanup: a compacted
// activity event takes its completion record with it (a redrain probe
// reads the event first, so orphaned markers are inert — but they must
// not accumulate one object per drained overflow emission forever).
func TestRetentionDeletesFanoutDoneMarkers(t *testing.T) {
	x := newHarness(t)
	oldAt := x.now.AddDate(0, 0, -30).Format(dateTimeFmt)
	writeRaw(t, x.svc.Store, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 2}))
	ev := ActivityEvent{Seq: 1, Repo: "acme/repo", Action: "commented", Kind: "issue", At: oldAt,
		Payload: mustEncode(t, activityPayload{Class: "subscribed", FanoutPending: true})}
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", 1), mustEncode(t, ev))
	writeRaw(t, x.svc.Store, FanoutDoneKey("acme", "repo", 1), mustEncode(t, FanoutDoneDoc{Seq: 1, At: oldAt}))
	x.svc.retainRepoEvents(ctx(), "acme", "repo", x.now)
	if x.svc.readActivity(ctx(), "acme", "repo", 1) != nil {
		t.Fatal("old event below the hookless floor must compact")
	}
	if x.svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("completion record must die with its event")
	}
}

// failPrincipalStore fails notification-object Creates for one principal
// only (reads + the index CAS + every other write delegate): a
// deterministic per-recipient fault injector for the honest-drain
// contract (issue #152). The done-marker Create never matches (different
// prefix), so marker behavior is exactly what the drain decides.
type failPrincipalStore struct {
	store.ObjectStore
	principal string
}

func (f failPrincipalStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutCreate &&
		strings.HasPrefix(key, NotifPrefix(f.principal)) &&
		!strings.HasSuffix(key, "index.json") {
		return store.ObjectMeta{}, errors.New("injected recipient failure")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// countPrincipalNotifs counts one principal's notification objects (the
// index is excluded — it is a row, not a delivery).
func countPrincipalNotifs(t *testing.T, st store.ObjectStore, principal string) int {
	t.Helper()
	n := 0
	_ = st.List(ctx(), NotifPrefix(principal), "", func(m store.ObjectMeta) error {
		if !strings.HasSuffix(m.Key, "/index.json") {
			n++
		}
		return nil
	})
	return n
}

// TestFanoutDrainSkipsDoneMarkerOnIncomplete pins issue #152's data-loss
// direction: an overflow drain that fails some recipients must NOT write
// the per-seq completion marker — the #77 redrain sweep keys on it, so a
// marker on an incomplete drain skips the seq forever. The healed sweep
// then converges idempotently (no duplicate for the healthy recipient).
// Driven explicitly (enqueue + WaitGroup join + sweep), no sleeps.
func TestFanoutDrainSkipsDoneMarkerOnIncomplete(t *testing.T) {
	st := store.NewMemory()
	at := "2026-09-04T12:00:00Z"
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 1}))
	seedActivity(t, st, at, "acme", "repo", 1, true,
		activityRecipient{Principal: "amy@example.com", Reason: ReasonSubscribed},
		activityRecipient{Principal: "zed@example.com", Reason: ReasonSubscribed})
	svc := New(failPrincipalStore{ObjectStore: st, principal: "zed@example.com"}, nil)

	svc.enqueueFanout("acme/repo", 1)
	waitFanoutQuiescent(t, svc, "acme/repo")

	if n := countPrincipalNotifs(t, st, "amy@example.com"); n != 1 {
		t.Fatalf("healthy recipient = %d, want 1", n)
	}
	if n := countPrincipalNotifs(t, st, "zed@example.com"); n != 0 {
		t.Fatalf("faulted recipient = %d, want 0", n)
	}
	if svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("incomplete drain must not write the done marker (issue #152: the redrain would skip the seq forever)")
	}

	// Heal the store: the sweep re-probes the unmarked seq and the drain
	// converges — zed delivered exactly once, amy not duplicated.
	svc.Store = st
	svc.sweepFanout(ctx())
	waitFanoutQuiescent(t, svc, "acme/repo")
	if !svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("healed redrain must mark the recovered seq")
	}
	if n := countPrincipalNotifs(t, st, "amy@example.com"); n != 1 {
		t.Fatalf("redrain duplicated the healthy recipient: %d", n)
	}
	if n := countPrincipalNotifs(t, st, "zed@example.com"); n != 1 {
		t.Fatalf("redrain delivered the faulted recipient %d times, want 1", n)
	}
}

// TestFanoutOneExpiredBudgetIsIncomplete pins the budget half of issue
// #152: recipients stranded by the FanoutBudget report complete=false
// (no silent loss), and a live-budget redrain converges to full delivery
// plus the marker. Deterministic: a canceled parent expires the budget
// before any recipient starts (the memory store honors zero-latency GETs
// under cancel, so the event still loads and every recipient strands at
// the budget precheck).
func TestFanoutOneExpiredBudgetIsIncomplete(t *testing.T) {
	st := store.NewMemory()
	at := "2026-09-04T12:00:00Z"
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 1}))
	seedActivity(t, st, at, "acme", "repo", 1, true,
		activityRecipient{Principal: "amy@example.com", Reason: ReasonSubscribed})
	svc := New(st, nil)

	cctx, cancel := context.WithCancel(ctx())
	cancel()
	existed, complete := svc.fanoutOne(cctx, "acme", "repo", "acme/repo", 1)
	if !existed || complete {
		t.Fatalf("expired budget = (%v, %v), want (true, false)", existed, complete)
	}
	if n := countPrincipalNotifs(t, st, "amy@example.com"); n != 0 {
		t.Fatalf("stranded recipient delivered %d, want 0", n)
	}

	// Live-budget redrain converges: full delivery, then the drain marks
	// the recovered seq.
	existed, complete = svc.fanoutOne(ctx(), "acme", "repo", "acme/repo", 1)
	if !existed || !complete {
		t.Fatalf("live redrain = (%v, %v), want (true, true)", existed, complete)
	}
	svc.enqueueFanout("acme/repo", 1)
	waitFanoutQuiescent(t, svc, "acme/repo")
	if !svc.fanoutDone(ctx(), "acme", "repo", 1) {
		t.Fatal("redrain must mark the recovered seq")
	}
	if n := countPrincipalNotifs(t, st, "amy@example.com"); n != 1 {
		t.Fatalf("converged delivery = %d, want exactly 1", n)
	}
}
