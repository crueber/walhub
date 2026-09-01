// bridge_test.go — the catch-up loop (09 §5.1/§5.2): cold cursor, gap counting,
// lost-CAS-as-success, at-least-once replay after sink failures, lag gauge,
// wake coalescing/non-blocking, sweep backstop, and clean shutdown.
package events

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func newTestBridge(t *testing.T, src WalSource, sinks ...Sink) (*Bridge, *recMetrics, store.ObjectStore) {
	t.Helper()
	st := store.NewMemory()
	met := newRecMetrics()
	b := New(Deps{Source: src, Store: st, Sinks: sinks, Metrics: met})
	return b, met, st
}

func loadCursorAt(t *testing.T, st store.ObjectStore, repo string) (cursorDoc, bool, error) {
	t.Helper()
	key, err := cursorKey(repo)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, found, err := loadCursor(context.Background(), st, key)
	return doc, found, err
}

// casLossStore simulates losing the cursor CAS race (another bridge advanced it
// first): conditional puts answer PreconditionFailed without applying.
type casLossStore struct {
	store.ObjectStore
	loseUpdate bool
	loseCreate bool
}

func (s *casLossStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	switch {
	case opts.Mode == store.PutUpdate && s.loseUpdate:
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	case opts.Mode == store.PutCreate && s.loseCreate:
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func seedCursor(t *testing.T, st store.ObjectStore, repo string, seq uint64) {
	t.Helper()
	key, err := cursorKey(repo)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"published_seq":` + strconv.FormatUint(seq, 10) + `,"updated_at":"2026-01-01T00:00:00Z"}`
	if _, err := st.Put(context.Background(), key, store.PutBody{Bytes: []byte(body)}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func TestCatchUp_ColdCursorPublishesWindowOnce(t *testing.T) {
	a, b := "aaaa", "bbbb"
	repo := &fakeRepo{minSeq: 2, head: 4, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/old", a, b)), // folded: below min_seq
		mkEntry(2, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, a)),
		mkEntry(3, proto.EntryKindPush, nil, upd("refs/heads/dev", a, b)),
		mkEntry(4, proto.EntryKindPush, nil, upd("refs/heads/gone", b, testZero40)),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	br, met, st := newTestBridge(t, src, sink)

	n, err := br.catchUp(context.Background(), "owner/r0")
	if err != nil {
		t.Fatalf("catchUp: %v", err)
	}
	if n != 3 {
		t.Fatalf("published %d events, want 3", n)
	}
	if got := sink.lastBatch(); len(got) != 3 || got[0].Walgit.Seq != "2" {
		t.Errorf("cold cursor must publish everything still in the log window: %+v", got)
	}
	doc, found, err := loadCursorAt(t, st, "owner/r0")
	if err != nil || !found || doc.PublishedSeq != 4 {
		t.Fatalf("cursor = %+v found=%v err=%v; want published_seq=4", doc, found, err)
	}
	// Cold cursor defaults to readable_from = min_seq − 1 = 1 → lag = head − 1.
	if met.gauge(MetricLag, "owner/r0") != 3 {
		t.Errorf("lag gauge = %d, want 3", met.gauge(MetricLag, "owner/r0"))
	}

	// Second run: head == cursor → no re-publication.
	n, err = br.catchUp(context.Background(), "owner/r0")
	if err != nil || n != 0 {
		t.Fatalf("second catchUp = (%d, %v), want (0, nil)", n, err)
	}
	if sink.deliveries() != 1 {
		t.Errorf("sink deliveries = %d, want 1", sink.deliveries())
	}
}

func TestCatchUp_GapCountedNotRepaired(t *testing.T) {
	a, b := "aaaa", "bbbb"
	repo := &fakeRepo{minSeq: 3, head: 4, entries: []*proto.LogEntry{
		mkEntry(3, proto.EntryKindPush, nil, upd("refs/heads/main", a, b)),
		mkEntry(4, proto.EntryKindPush, nil, upd("refs/heads/dev", b, a)),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	br, met, st := newTestBridge(t, src, sink)
	seedCursor(t, st, "owner/r0", 0) // cursor 0 < readable_from 2 → gap

	n, err := br.catchUp(context.Background(), "owner/r0")
	if err != nil {
		t.Fatalf("catchUp: %v", err)
	}
	if n != 2 {
		t.Fatalf("published %d events, want the two readable entries", n)
	}
	if got := sink.lastBatch(); len(got) != 2 || got[0].Walgit.Seq != "3" {
		t.Errorf("gap read must start at min_seq=3, batch = %+v", got)
	}
	if met.count(MetricGap, "owner/r0") != 1 {
		t.Errorf("gap counter = %d, want 1", met.count(MetricGap, "owner/r0"))
	}
	// The cursor advances to head; the gap is never silently repaired — the
	// read simply never goes below min_seq.
	doc, found, _ := loadCursorAt(t, st, "owner/r0")
	if !found || doc.PublishedSeq != 4 {
		t.Errorf("cursor after gap run = %+v found=%v; want 4", doc, found)
	}
	// A second run is not another gap: the cursor now sits in the window.
	_, err = br.catchUp(context.Background(), "owner/r0")
	if err != nil {
		t.Fatal(err)
	}
	if met.count(MetricGap, "owner/r0") != 1 {
		t.Errorf("gap counter after second run = %d, want still 1", met.count(MetricGap, "owner/r0"))
	}
}

func TestCatchUp_LostCASAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loseUpdate bool
		loseCreate bool
		seeded     bool
	}{
		{"update_race", true, false, true},
		{"create_race", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{minSeq: 1, head: 1, entries: []*proto.LogEntry{
				mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, "aaaa")),
			}}
			src := newFakeSource(repo)
			sink := newFakeSink("webhook")
			st := &casLossStore{ObjectStore: store.NewMemory(), loseUpdate: tc.loseUpdate, loseCreate: tc.loseCreate}
			if tc.seeded {
				seedCursor(t, st, "owner/r0", 0)
			}
			met := newRecMetrics()
			br := New(Deps{Source: src, Store: st, Sinks: []Sink{sink}, Metrics: met})

			n, err := br.catchUp(context.Background(), "owner/r0")
			if err != nil {
				t.Fatalf("lost CAS must be treated as success, got error: %v", err)
			}
			if n != 1 || sink.deliveries() != 1 {
				t.Fatalf("duplicate emission delivered %d events over %d deliveries", n, sink.deliveries())
			}
			if met.count(MetricPublished, "webhook") != 1 {
				t.Errorf("published_total = %d, want 1 (the emission still happened)", met.count(MetricPublished, "webhook"))
			}
		})
	}
}

func TestCatchUp_SinkFailureReplaysAtLeastOnce(t *testing.T) {
	a, b := "aaaa", "bbbb"
	repo := &fakeRepo{minSeq: 1, head: 2, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, a)),
		mkEntry(2, proto.EntryKindPush, nil, upd("refs/heads/dev", a, b)),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	sink.failsLeft = 2
	br, met, st := newTestBridge(t, src, sink)
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		if _, err := br.catchUp(ctx, "owner/r0"); err == nil {
			t.Fatalf("run %d must fail at the sink", i)
		}
		if _, found, _ := loadCursorAt(t, st, "owner/r0"); found {
			t.Fatalf("run %d must leave the cursor untouched", i)
		}
		if met.count(MetricPublished, "webhook") != 0 {
			t.Fatalf("run %d must not count published events", i)
		}
		if met.gauge(MetricLag, "owner/r0") != 2 {
			t.Fatalf("run %d lag gauge = %d, want 2 (recorded even on failure)", i, met.gauge(MetricLag, "owner/r0"))
		}
	}

	n, err := br.catchUp(ctx, "owner/r0")
	if err != nil || n != 2 {
		t.Fatalf("third run = (%d, %v), want (2, nil)", n, err)
	}
	if got := sink.lastBatch(); len(got) != 2 || got[0].Walgit.Seq != "1" || got[1].Walgit.Seq != "2" {
		t.Errorf("replayed batch = %+v; the same range must replay after failures", got)
	}
	doc, found, _ := loadCursorAt(t, st, "owner/r0")
	if !found || doc.PublishedSeq != 2 {
		t.Errorf("cursor = %+v found=%v; want 2", doc, found)
	}
	if met.count(MetricPublished, "webhook") != 2 {
		t.Errorf("published_total = %d, want 2", met.count(MetricPublished, "webhook"))
	}
}

func TestCatchUp_NoEventsStillAdvancesCursor(t *testing.T) {
	repo := &fakeRepo{minSeq: 1, head: 1, entries: []*proto.LogEntry{
		{Seq: 1, Kind: proto.EntryKindCompact}, // emits nothing
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	br, _, st := newTestBridge(t, src, sink)

	n, err := br.catchUp(context.Background(), "owner/r0")
	if err != nil || n != 0 {
		t.Fatalf("catchUp = (%d, %v), want (0, nil)", n, err)
	}
	if sink.deliveries() != 0 {
		t.Errorf("no POST must happen for zero events")
	}
	doc, found, _ := loadCursorAt(t, st, "owner/r0")
	if !found || doc.PublishedSeq != 1 {
		t.Errorf("cursor = %+v found=%v; want advance to head 1", doc, found)
	}
}

func TestWake_CoalescesAndNeverBlocks(t *testing.T) {
	br, _, _ := newTestBridge(t, newFakeSource())
	for i := range workCap {
		if got := br.Wake("owner/r" + strconv.Itoa(i)); got != StatusQueued {
			t.Fatalf("wake %d = %q, want queued", i, got)
		}
	}
	if got := br.Wake("owner/overflow"); got != StatusDropped {
		t.Errorf("wake past capacity = %q, want dropped (channel full)", got)
	}
	if got := br.Wake("owner/r0"); got != StatusDropped {
		t.Errorf("wake of queued repo = %q, want dropped (coalesced)", got)
	}
}

// blockingView blocks inside SyncRefs until released (concurrency assertions).
type blockingView struct {
	once    sync.Once
	gate    chan struct{}
	entered chan struct{}
	state   RepoState
	entries []*proto.LogEntry
}

func newBlockingView(state RepoState, entries []*proto.LogEntry) *blockingView {
	return &blockingView{state: state, entries: entries}
}

func (r *blockingView) SyncRefs(ctx context.Context) (RepoState, error) {
	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
	}
	select {
	case <-r.gate:
	case <-ctx.Done():
		return RepoState{}, ctx.Err()
	}
	return r.state, nil
}

func (r *blockingView) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	var out []*proto.LogEntry
	for _, e := range r.entries {
		if e.Seq >= from && e.Seq <= to {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestRun_DrainsWorkAndExitsOnCancel(t *testing.T) {
	repo := &blockingView{gate: make(chan struct{}), entered: make(chan struct{}),
		state: RepoState{HeadSeq: 1, MinSeq: 1},
		entries: []*proto.LogEntry{
			mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, "aaaa")),
		}}
	src := newFakeSource()
	src.setView("owner/r0", repo)
	sink := newFakeSink("webhook")
	br, met, _ := newTestBridge(t, src, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { br.Run(ctx); close(done) }()

	if got := br.Wake("owner/r0"); got != StatusQueued {
		t.Fatalf("wake = %q, want queued", got)
	}
	<-repo.entered // catch-up in flight

	// A wake during the repo's own catch-up re-queues it (runs again once).
	if got := br.Wake("owner/r0"); got != StatusQueued {
		t.Errorf("wake during own catch-up = %q, want queued (at-least-once re-run)", got)
	}

	close(repo.gate)
	waitFor(t, time.Second, func() bool { return sink.deliveries() == 1 })
	if met.count(MetricPublished, "webhook") != 1 {
		t.Errorf("published_total = %d, want 1", met.count(MetricPublished, "webhook"))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

func TestRun_SweepBackstop(t *testing.T) {
	repo := &fakeRepo{minSeq: 1, head: 1, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, "aaaa")),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	st := store.NewMemory()
	met := newRecMetrics()
	br := New(Deps{Source: src, Store: st, Sinks: []Sink{sink}, Metrics: met, SweepInterval: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go br.Run(ctx)

	waitFor(t, time.Second, func() bool { return met.count(MetricSweepFound) >= 1 })
	doc, found, _ := loadCursorAt(t, st, "owner/r0")
	if !found || doc.PublishedSeq != 1 {
		t.Errorf("sweep must advance the cursor, got %+v found=%v", doc, found)
	}
}

func TestRun_SweepDisabledWhenZero(t *testing.T) {
	repo := &fakeRepo{minSeq: 1, head: 1, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, "aaaa")),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	br, _, _ := newTestBridge(t, src, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { br.Run(ctx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	if sink.deliveries() != 0 {
		t.Errorf("sweep_interval=0s must disable the sweep; sink saw %d deliveries", sink.deliveries())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestCatchUp_SourceErrorsSurface(t *testing.T) {
	repo := &fakeRepo{syncErr: errors.New("store down")}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	br, _, _ := newTestBridge(t, src, sink)

	if _, err := br.catchUp(context.Background(), "owner/r0"); err == nil {
		t.Fatal("sync error must surface")
	}
	repo.mu.Lock()
	repo.syncErr, repo.logErr, repo.head, repo.minSeq = nil, errors.New("log gone"), 1, 1
	repo.mu.Unlock()
	if _, err := br.catchUp(context.Background(), "owner/r0"); err == nil {
		t.Fatal("log read error must surface")
	}
	if _, err := br.catchUp(context.Background(), "owner/nope"); err == nil {
		t.Fatal("unknown repo must surface")
	}
}
