package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestOverflowDefersToFanoutTask(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	recips := []string{}
	for i := 0; i < MaxSyncRecipients+10; i++ {
		p := fmt.Sprintf("u%03d@example.com", i)
		x.addProfile(p)
		recips = append(recips, p)
	}
	x.writeThread(t, "acme", "repo", 7, "T", "bob@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", recips)
	// Overflow: activity landed synchronously…
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	if ev == nil {
		t.Fatal("overflow must still append the activity event")
	}
	var payload activityPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	if len(payload.Recipients) != MaxSyncRecipients+10 {
		t.Fatalf("payload recipients = %d", len(payload.Recipients))
	}
	// …and the fanout task drains the full set (poll: background task).
	deadline := time.Now().Add(10 * time.Second)
	for {
		done := true
		for _, p := range recips {
			if countNotifs(t, x, p) != 1 {
				done = false
				break
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fanout task did not drain the overflow set")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := x.svc.TaskStatus("acme/repo", TaskKindFanout)
	if rec == nil || rec.State != TaskFinished {
		t.Fatalf("fanout record = %+v", rec)
	}
	// Tray SSE still published per completed recipient (bus is live-only;
	// here we assert the objects + index, frames covered in stream tests).
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey(recips[0]), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if ix.UnreadCount != 1 {
		t.Fatalf("overflow index = %+v", ix)
	}
}

func TestRetentionCompactsReads(t *testing.T) {
	x := newHarness(t)
	x.svc.RetentionDays = 30
	seedRepo(t, x, "acme", "repo")
	x.addProfile("amy@example.com", "bob@example.com")
	old := x.now.AddDate(0, 0, -60).Format(dateTimeFmt)
	recent := x.now.Format(dateTimeFmt)
	// Seed: one old read, one recent read, one unread.
	seed := func(id, state, at string) {
		n := Notification{ID: id, Repo: "acme/repo", Num: 7, Kind: "issue", Reason: ReasonSubscribed, State: state, CreatedAt: at}
		if err := x.svc.putCreate(ctx(), NotifKey("amy@example.com", id), mustEncode(t, n)); err != nil {
			t.Fatal(err)
		}
		if err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{ID: id, Repo: "acme/repo", Num: 7, Kind: "issue", Reason: ReasonSubscribed, State: state, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	seed(strings.Repeat("a", 32), StateRead, old)
	seed(strings.Repeat("b", 32), StateRead, recent)
	seed(strings.Repeat("c", 32), StateUnread, old) // unread never swept, however old
	x.svc.RunRetention(ctx())

	if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", strings.Repeat("a", 32)), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatal("old read must be deleted")
	}
	if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", strings.Repeat("b", 32)), store.GetOptions{}); err != nil {
		t.Fatalf("recent read must survive: %v", err)
	}
	if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", strings.Repeat("c", 32)), store.GetOptions{}); err != nil {
		t.Fatalf("unread must survive: %v", err)
	}
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if ix.UnreadCount != 1 || len(ix.Entries) != 2 || ix.SweptAt == "" {
		t.Fatalf("compacted index = %+v", ix)
	}
	if ix.CompactedThrough != strings.Repeat("a", 32) {
		t.Fatalf("compacted_through = %q", ix.CompactedThrough)
	}
	// Second pass within a day: skipped (swept_at fresh).
	before := raw
	x.svc.RunRetention(ctx())
	raw, _, _ = store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	if string(raw) != string(before) {
		t.Fatal("fresh sweep must be a no-op")
	}
}

func TestRetentionCompactsActivityFloor(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	// Old activity (backdate by writing directly), no hooks → floor is
	// the head; everything past the floor age compacts.
	oldAt := x.now.AddDate(0, 0, -30).Format(dateTimeFmt)
	for seq := 1; seq <= 3; seq++ {
		if _, err := x.svc.casUpdate(ctx(), CollabStateKey("acme", "repo"), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			return mustEncode(t, CollabState{NextSeq: seq}), true, nil
		}); err != nil {
			t.Fatal(err)
		}
		ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", Kind: "issue", At: oldAt}
		if err := x.svc.putCreate(ctx(), ActivityKey("acme", "repo", seq), mustEncode(t, ev)); err != nil {
			t.Fatal(err)
		}
	}
	x.svc.RunRetention(ctx())
	for seq := 1; seq <= 2; seq++ {
		if x.svc.readActivity(ctx(), "acme", "repo", seq) != nil {
			t.Fatalf("seq %d must compact (hookless, past floor)", seq)
		}
	}
	if x.svc.readActivity(ctx(), "acme", "repo", 3) == nil {
		t.Fatal("head seq survives (nothing is below itself)")
	}
	// With a hook whose cursor guards seq 2, only seq 1 compacts.
	for seq := 1; seq <= 3; seq++ {
		ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", Kind: "issue", At: oldAt}
		_ = x.svc.putCreate(ctx(), ActivityKey("acme", "repo", seq), mustEncode(t, ev))
	}
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	x.svc.advanceCursor(ctx(), "acme", "repo", hk.ID, 2)
	x.svc.RunRetention(ctx())
	if x.svc.readActivity(ctx(), "acme", "repo", 1) != nil {
		t.Fatal("seq 1 below min cursor must compact")
	}
	if x.svc.readActivity(ctx(), "acme", "repo", 2) == nil {
		t.Fatal("seq 2 at min cursor must survive")
	}
}

// TestRetentionActivityScanBound pins the §9 maintainer-pass bound: a repo
// whose minimum webhook cursor sits thousands of seqs out compacts in
// bounded reads per pass (converging across passes), never O(minCursor)
// GETs in one pass.
func TestRetentionActivityScanBound(t *testing.T) {
	x := newHarness(t)
	c := &opCounts{}
	x.svc.Store = countingStore{ObjectStore: x.svc.Store, c: c}
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	// Cursor far out, no events beneath it (pure gaps): the old unbounded
	// loop would read every seq from 1.
	x.svc.advanceCursor(ctx(), "acme", "repo", hk.ID, 5000)
	before := c.snapshot()
	x.svc.retainRepoEvents(ctx(), "acme", "repo", x.now)
	cost := c.snapshot()
	if gets := cost.get - before.get; gets > 1000 {
		t.Fatalf("retention pass GETs = %d, want bounded (< 1000) for a 5000-seq window", gets)
	}
	if dels := cost.del - before.del; dels != 0 {
		t.Fatalf("gaps must never delete: dels = %d", dels)
	}
}

func TestUserBusDropOldest(t *testing.T) {
	x := newHarness(t)
	ch, unsub := x.svc.ubus.subscribe("amy@example.com")
	defer unsub()
	// Overflow the 16-buffer: publish never blocks, oldest shed.
	for i := 0; i < 40; i++ {
		x.svc.ubus.publish("amy@example.com", Notification{ID: fmt.Sprintf("%032d", i)})
	}
	if got := x.svc.ubus.liveCount("amy@example.com"); got != 1 {
		t.Fatalf("subs = %d", got)
	}
	unsub()
	if got := x.svc.ubus.liveCount("amy@example.com"); got != 0 {
		t.Fatalf("unsub leaked: %d", got)
	}
	// Drain: must hold the NEWEST 16, in order.
	var ids []string
	for {
		select {
		case n, ok := <-ch:
			if !ok {
				goto drained
			}
			ids = append(ids, n.ID)
		default:
			goto drained
		}
	}
drained:
	if len(ids) != 16 || ids[0] != fmt.Sprintf("%032d", 24) || ids[15] != fmt.Sprintf("%032d", 39) {
		t.Fatalf("ring = %v", ids)
	}
}

func TestRepoBusSubscribeAndRing(t *testing.T) {
	x := newHarness(t)
	x.svc.PublishStream("issue", "acme/repo", "", "", "", 7)
	ch, recent, unsub := x.svc.SubscribeRepo("acme/repo")
	defer unsub()
	if len(recent) != 1 || recent[0].Name != "issue" || recent[0].Num != 7 || recent[0].At == "" {
		t.Fatalf("recent = %+v", recent)
	}
	x.svc.PublishStream("pull", "acme/repo", "opened", "T", "open", 9)
	select {
	case f := <-ch:
		if f.Name != "pull" || f.Action != "opened" || f.Num != 9 {
			t.Fatalf("frame = %+v", f)
		}
	default:
		t.Fatal("live frame not delivered")
	}
}

func TestTaskJoinSemantics(t *testing.T) {
	x := newHarness(t)
	a := x.svc.StartWebhooks(ctx(), "acme/repo")
	b := x.svc.StartWebhooks(ctx(), "acme/repo")
	if a.ID != b.ID {
		t.Fatalf("join must share the record: %q vs %q", a.ID, b.ID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec := x.svc.TaskStatus("acme/repo", TaskKindWebhooks); rec != nil && rec.State == TaskFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("webhooks task did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRetentionDropsDeadRepos pins the #63 retention contract: tray rows
// naming a deleted repo are dropped (any state) with their objects, the
// unread count reconciles, and overflow objects past the hot window are
// swept too — while live-repo rows are untouched.
func TestRetentionDropsDeadRepos(t *testing.T) {
	x := newHarness(t)
	x.svc.RetentionDays = 30
	seedRepo(t, x, "acme", "live")
	recent := x.now.Format(dateTimeFmt)
	seed := func(who, id, repo, state string) {
		n := Notification{ID: id, Repo: repo, Num: 7, Kind: "issue", Reason: ReasonSubscribed, State: state, CreatedAt: recent}
		if err := x.svc.putCreate(ctx(), NotifKey(who, id), mustEncode(t, n)); err != nil {
			t.Fatal(err)
		}
		if err := x.svc.indexAdd(ctx(), who, IndexEntry{ID: id, Repo: repo, Num: 7, Kind: "issue", Reason: ReasonSubscribed, State: state, At: recent}); err != nil {
			t.Fatal(err)
		}
	}
	// Window: one dead unread, one dead read, one live unread.
	seed("amy@example.com", strings.Repeat("d", 32), "acme/gone", StateUnread)
	seed("amy@example.com", strings.Repeat("e", 32), "acme/gone", StateRead)
	seed("amy@example.com", strings.Repeat("f", 32), "acme/live", StateUnread)
	// Overflow: a dead-repo object with no index row (trim the window by
	// hand — indexAdd caps at TrayPageSize, so evict via direct write).
	overflow := Notification{ID: strings.Repeat("0", 32), Repo: "acme/gone", Num: 9, Kind: "issue", Reason: ReasonMentioned, State: StateUnread, CreatedAt: recent}
	if err := x.svc.putCreate(ctx(), NotifKey("amy@example.com", strings.Repeat("0", 32)), mustEncode(t, overflow)); err != nil {
		t.Fatal(err)
	}
	x.svc.RunRetention(ctx())
	// Dead objects gone (window + overflow), live object kept.
	for _, id := range []string{strings.Repeat("d", 32), strings.Repeat("e", 32), strings.Repeat("0", 32)} {
		if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", id), store.GetOptions{}); !store.IsNotFound(err) {
			t.Fatalf("dead object %s must be deleted: %v", id, err)
		}
	}
	if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", strings.Repeat("f", 32)), store.GetOptions{}); err != nil {
		t.Fatalf("live object must survive: %v", err)
	}
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != 1 || ix.Entries[0].ID != strings.Repeat("f", 32) {
		t.Fatalf("window = %+v", ix.Entries)
	}
	if ix.UnreadCount != 1 {
		t.Fatalf("count = %d, want 1", ix.UnreadCount)
	}
}

func TestRetainOverflowEdges(t *testing.T) {
	x := newHarness(t)
	at := x.now.Format(dateTimeFmt)
	put := func(id, repo string, body []byte) {
		t.Helper()
		if body == nil {
			n := Notification{ID: id, Repo: repo, Num: 1, Kind: "issue", Reason: ReasonSubscribed, State: StateUnread, CreatedAt: at}
			body = mustEncode(t, n)
		}
		if err := x.svc.putCreate(ctx(), NotifKey("amy@example.com", id), body); err != nil {
			t.Fatal(err)
		}
	}
	// Zero budget: no scan, nothing deleted.
	if n := x.svc.retainOverflow(ctx(), "amy@example.com", map[string]bool{}, 0); n != 0 {
		t.Fatalf("zero budget = %d", n)
	}
	// Corrupt objects and live-repo objects are never touched; the
	// already-handled id is skipped; the dead object goes.
	put(strings.Repeat("1", 32), "acme/gone", []byte("{bad"))
	put(strings.Repeat("2", 32), "acme/live", nil)
	put(strings.Repeat("3", 32), "acme/gone", nil)
	seedRepo(t, x, "acme", "live")
	dead := map[string]bool{strings.Repeat("2", 32): true}
	if n := x.svc.retainOverflow(ctx(), "amy@example.com", dead, 200); n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	for _, id := range []string{strings.Repeat("1", 32), strings.Repeat("2", 32)} {
		if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", id), store.GetOptions{}); err != nil {
			t.Fatalf("kept object %s: %v", id, err)
		}
	}
	if _, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey("amy@example.com", strings.Repeat("3", 32)), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("dead object must go: %v", err)
	}
}

// TestFanoutTerminalDrainKeepsLateSeq reproduces issue #72's
// late-attach-vs-terminal-drain interleaving at the entry discipline:
//
//  1. the leader's terminal drain observes an empty attachment;
//  2. a concurrent enqueueFanout still sees the running entry, joins
//     it, and attaches seq 7;
//  3. the leader must NOT end the task with seq 7 pending.
//
// Pre-fix end() never re-checked e.seqs, so step 3 removed the task
// while seq 7 orphaned on the detached entry — no worker ever drained
// it again (silent notification loss). Drain-then-end via
// endIfQuiescent refuses while seqs are pending; the leftover drains
// first and only then may the task end.
func TestFanoutTerminalDrainKeepsLateSeq(t *testing.T) {
	x := newHarness(t)
	const repo = "acme/repo"
	e, joined := x.svc.tasks.begin(repo, TaskKindFanout, x.now)
	if joined {
		t.Fatal("first begin must lead")
	}
	// 1. terminal drain observes empty.
	if seqs := e.drain(); len(seqs) != 0 {
		t.Fatalf("fresh task drains %v", seqs)
	}
	// 2. late joiner attaches while the entry is still registered.
	late, joined := x.svc.tasks.begin(repo, TaskKindFanout, x.now)
	if !joined || late != e {
		t.Fatal("late enqueue must join the still-running task")
	}
	late.attach(7)
	if !x.svc.tasks.current(repo, TaskKindFanout, e) {
		t.Fatal("joined entry must still be registered")
	}
	// 3. the end must refuse while seq 7 is pending.
	if x.svc.tasks.endIfQuiescent(repo, TaskKindFanout, "fanout drained", x.now) {
		t.Fatal("drain-then-end ended the task with seq 7 pending (issue #72)")
	}
	if rec := x.svc.TaskStatus(repo, TaskKindFanout); rec == nil || rec.State != TaskRunning {
		t.Fatalf("refused end must leave the task running: %+v", rec)
	}
	// The leftover drains; only then may the task end.
	if seqs := e.drain(); len(seqs) != 1 || seqs[0] != 7 {
		t.Fatalf("leftover drain = %v", seqs)
	}
	if !x.svc.tasks.endIfQuiescent(repo, TaskKindFanout, "fanout drained", x.now) {
		t.Fatal("quiescent task must end")
	}
	if rec := x.svc.TaskStatus(repo, TaskKindFanout); rec == nil || rec.State != TaskFinished {
		t.Fatalf("task record = %+v", rec)
	}
	// Edges: ending an unknown key is a no-op success; a detached
	// entry is no longer current.
	if !x.svc.tasks.endIfQuiescent("acme/other", TaskKindFanout, "n", x.now) {
		t.Fatal("unknown key must be a no-op success")
	}
	if x.svc.tasks.current(repo, TaskKindFanout, e) {
		t.Fatal("ended entry must not be current")
	}
}

// TestFanoutConcurrentEnqueueLosesNothing hammers the real
// enqueueFanout/drainFanout paths: N seqs (one activity event + one
// recipient each) each enqueued from R concurrent goroutines, so
// attaches race terminal drains continuously. Every seq must still
// produce its notification — pre-fix, an attach landing between the
// terminal drain and end orphaned the seq (issue #72).
func TestFanoutConcurrentEnqueueLosesNothing(t *testing.T) {
	x := newHarness(t)
	const repo = "acme/repo"
	const n = 60
	const per = 6
	principals := make([]string, n)
	for i := 0; i < n; i++ {
		seq := i + 1
		p := fmt.Sprintf("u%03d@example.com", i)
		principals[i] = p
		ev := ActivityEvent{Seq: seq, Repo: repo, Action: "commented", Num: seq, Kind: "issue",
			Actor: "b", Title: "T", At: x.now.Format(dateTimeFmt),
			Payload: mustEncode(t, activityPayload{Class: "subscribed", Recipients: []activityRecipient{{Principal: p, Reason: ReasonSubscribed}}})}
		writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", seq), mustEncode(t, ev))
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		seq := i + 1
		for j := 0; j < per; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x.svc.enqueueFanout(repo, seq)
			}()
		}
	}
	wg.Wait()
	deadline := time.Now().Add(15 * time.Second)
	for {
		done := true
		for _, p := range principals {
			if countNotifs(t, x, p) != 1 {
				done = false
				break
			}
		}
		if done {
			if rec := x.svc.TaskStatus(repo, TaskKindFanout); rec != nil && rec.State == TaskFinished {
				break
			}
			done = false
		}
		if time.Now().After(deadline) {
			var missing []string
			for _, p := range principals {
				if countNotifs(t, x, p) != 1 {
					missing = append(missing, p)
				}
			}
			t.Fatalf("lost %d/%d fan-out seqs: %v", len(missing), n, missing)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
