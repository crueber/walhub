// tasks.go — Seam 5 task kinds: `webhooks`, `notify-fanout`,
// `notify-retention` (06 §8).
//
// The task table is the (repo, kind) single-flight (join semantics,
// same shape as pulls' taskTable): a second start joins the running
// task. The overflow task attaches activity seqs; the leader drains
// until quiescent and ends the task only when no seq arrived since the
// last drain (drain-then-end under one discipline — issue #72).
// Composition starts Run (wake-ups + sweeps) like the
// events bridge: `go svc.Run(ctx)`.
//
// ### Concurrency
//
// Hazard: a fan-out seq attached between the leader's terminal drain
// and the task end orphans on a detached entry while the joiner —
// already returned — starts no worker (silent notification loss).
// Avoidance: (a) the leader ends ONLY via endIfQuiescent, which
// re-checks the attachment under the table lock nesting the entry
// lock and refuses while seqs are pending; (b) a joiner re-checks
// that its entry is still registered after attaching and re-enqueues
// onto the current entry on a miss. Lock order is always
// taskTable.mu → taskEntry.mu (endIfQuiescent is the ONLY place both
// are held); no lock is held across any store or network call.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// TaskState is the lifecycle of a task record.
type TaskState string

const (
	TaskRunning  TaskState = "running"
	TaskFinished TaskState = "finished"
)

// TaskRecord is the SSE-attachable task surface (narrated, bounded).
type TaskRecord struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Repo     string    `json:"repo"`
	State    TaskState `json:"state"`
	Started  string    `json:"started"`
	Finished string    `json:"finished,omitempty"`
	Note     string    `json:"note,omitempty"`
}

type taskEntry struct {
	rec  *TaskRecord
	wg   sync.WaitGroup
	seqs []int
	mu   sync.Mutex
}

type taskTable struct {
	mu      sync.Mutex
	running map[string]*taskEntry
	recent  map[string]*TaskRecord
	order   []string
}

func newTaskTable() *taskTable {
	return &taskTable{running: map[string]*taskEntry{}, recent: map[string]*TaskRecord{}}
}

func taskKey(repo, kind string) string { return repo + "," + kind }

// begin starts or joins a task. joined=true ⇒ caller joined (use ID to
// poll); joined=false ⇒ caller is the leader and MUST call end.
func (t *taskTable) begin(repo, kind string, now time.Time) (*taskEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.running[taskKey(repo, kind)]; ok {
		return e, true
	}
	e := &taskEntry{rec: &TaskRecord{
		ID: fmt.Sprintf("%s-%d", kind, now.UnixNano()), Kind: kind,
		Repo: repo, State: TaskRunning, Started: now.UTC().Format(dateTimeFmt),
	}}
	e.wg.Add(1)
	t.running[taskKey(repo, kind)] = e
	return e, false
}

// end completes a task (leader only); the finished record stays visible
// in a bounded recent cache for late attachers.
func (t *taskTable) end(repo, kind, note string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := taskKey(repo, kind)
	e, ok := t.running[key]
	if !ok {
		return
	}
	t.finishLocked(key, e, note, now)
}

// finishLocked completes e: mark finished, unblock joiners, move the
// record to the bounded recent cache. Caller holds t.mu.
func (t *taskTable) finishLocked(key string, e *taskEntry, note string, now time.Time) {
	e.rec.State = TaskFinished
	e.rec.Finished = now.UTC().Format(dateTimeFmt)
	e.rec.Note = note
	e.wg.Done()
	delete(t.running, key)
	t.recent[key] = e.rec
	t.order = append(t.order, key)
	for len(t.order) > 128 {
		oldest := t.order[0]
		t.order = t.order[1:]
		if _, running := t.running[oldest]; !running {
			delete(t.recent, oldest)
		}
	}
}

// endIfQuiescent completes a fanout task (leader only) IFF no seq was
// attached since the leader's last drain: the table lock nests the
// entry lock so the re-check and the removal are atomic against a
// concurrent attach (issue #72). False means "seqs pending" — the
// caller must drain again instead of ending. True with no running
// entry is a no-op success (already ended).
func (t *taskTable) endIfQuiescent(repo, kind, note string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := taskKey(repo, kind)
	e, ok := t.running[key]
	if !ok {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.seqs) > 0 {
		return false
	}
	t.finishLocked(key, e, note, now)
	return true
}

// current reports whether e is still the registered entry for
// (repo, kind). A joiner that attached to a detached entry (the leader
// ended between its begin and attach) must re-enqueue — see
// enqueueFanout.
func (t *taskTable) current(repo, kind string, e *taskEntry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.running[taskKey(repo, kind)]
	return ok && cur == e
}

// attach adds an activity seq to a running fanout task.
func (e *taskEntry) attach(seq int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, q := range e.seqs {
		if q == seq {
			return
		}
	}
	e.seqs = append(e.seqs, seq)
}

// drain takes the attached seqs (the leader consumes exactly once).
func (e *taskEntry) drain() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]int(nil), e.seqs...)
	e.seqs = nil
	return out
}

// TaskStatus snapshots a running or recent record (nil when never started).
func (s *Service) TaskStatus(repo, kind string) *TaskRecord {
	s.tasks.mu.Lock()
	defer s.tasks.mu.Unlock()
	if e, ok := s.tasks.running[taskKey(repo, kind)]; ok {
		cp := *e.rec
		return &cp
	}
	if r, ok := s.tasks.recent[taskKey(repo, kind)]; ok {
		cp := *r
		return &cp
	}
	return nil
}

// --- wake-ups ----------------------------------------------------------------------

// wakeRepo triggers a webhooks pass for repo (non-blocking, coalesced:
// a full wake channel means a pass is already pending).
func (s *Service) wakeRepo(repo string) {
	select {
	case s.wake <- repo:
	default:
	}
}

// Run serves wake-ups and sweeps until ctx ends (composition:
// `go svc.Run(ctx)`). Webhook sweeps run every minute; retention runs
// once a day. Every goroutine exits via ctx (13 channel rule).
func (s *Service) Run(ctx context.Context) {
	webhooksTick := time.NewTicker(time.Minute)
	retentionTick := time.NewTicker(24 * time.Hour)
	defer webhooksTick.Stop()
	defer retentionTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case repo := <-s.wake:
			s.StartWebhooks(ctx, repo)
		case <-webhooksTick.C:
			s.sweepWebhooks(ctx)
		case <-retentionTick.C:
			s.RunRetention(ctx)
		}
	}
}

// sweepWebhooks delivers every repo with hooks. Hook-bearing repos are
// found by LIST over the collab-events prefixes (maintainer path —
// collab-events/ exists only where fan-out ran; hook configs without
// events deliver nothing, so they need no pass).
func (s *Service) sweepWebhooks(ctx context.Context) {
	seen := map[string]bool{}
	_ = s.Store.ListPrefixes(ctx, "repos/", func(ownerSlash string) error {
		owner := strings.TrimSuffix(strings.TrimPrefix(ownerSlash, "repos/"), "/")
		if owner == "" {
			return nil
		}
		return s.Store.ListPrefixes(ctx, "repos/"+owner+"/", func(repoSlash string) error {
			repo := strings.TrimSuffix(strings.TrimPrefix(repoSlash, "repos/"+owner+"/"), "/")
			if repo == "" || strings.Contains(repo, "/") {
				return nil
			}
			key := owner + "/" + repo
			if seen[key] {
				return nil
			}
			seen[key] = true
			// Only pass repos that actually have hooks.
			hooks, err := s.ListHooks(ctx, owner, repo)
			if err != nil || len(hooks) == 0 {
				return nil
			}
			s.StartWebhooks(ctx, key)
			return nil
		})
	})
}

// StartWebhooks starts (or joins) the webhooks task for repo.
func (s *Service) StartWebhooks(ctx context.Context, repo string) *TaskRecord {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return nil
	}
	e, joined := s.tasks.begin(repo, TaskKindWebhooks, s.nowUTC())
	if joined {
		return e.rec
	}
	go func() {
		bctx := context.WithoutCancel(ctx)
		s.DeliverRepo(bctx, owner, name)
		s.tasks.end(repo, TaskKindWebhooks, "webhooks pass", s.nowUTC())
	}()
	return e.rec
}

// --- notify-fanout ----------------------------------------------------------------------

// enqueueFanout attaches seq to the repo's fanout task, starting it when
// idle. The task reloads each activity event and completes its recipient
// set idempotently (deterministic ids + index dedup). A joiner whose
// entry was ended between its begin and attach re-enqueues onto the
// current entry (the seq is re-attached there), so no interleaving
// orphans a seq on a detached entry (issue #72).
func (s *Service) enqueueFanout(repo string, seq int) {
	for {
		e, joined := s.tasks.begin(repo, TaskKindFanout, s.nowUTC())
		e.attach(seq)
		if !joined {
			go func() {
				s.drainFanout(repo, e)
			}()
			return
		}
		if s.tasks.current(repo, TaskKindFanout, e) {
			return
		}
	}
}

// drainFanout processes attached seqs until quiescent: after each pass
// the leader re-checks the attachment, and the task ends only via
// endIfQuiescent — an empty drain followed by a late attach refuses
// the end and drains again instead of dropping the seq (issue #72).
func (s *Service) drainFanout(repo string, e *taskEntry) {
	owner, name, ok := splitRepo(repo)
	note := "fanout drained"
	if !ok {
		note = "bad repo"
	}
	for {
		seqs := e.drain()
		if ok {
			for _, seq := range seqs {
				s.fanoutOne(context.Background(), owner, name, repo, seq)
			}
		}
		// A malformed repo has nothing to fan out to: drained seqs
		// are discarded, but the end stays quiescent so a concurrent
		// joiner re-enqueues (and observes the finished record)
		// instead of orphaning its seq.
		if s.tasks.endIfQuiescent(repo, TaskKindFanout, note, s.nowUTC()) {
			return
		}
	}
}

// fanoutOne completes one activity event's recipient set: Create (412 =
// done) + index CAS + SSE frame per recipient. Best-effort per
// recipient; the activity event stays the backfill truth.
func (s *Service) fanoutOne(ctx context.Context, owner, name, repo string, seq int) {
	fctx, cancel := context.WithTimeout(ctx, FanoutBudget)
	defer cancel()
	ev := s.readActivity(fctx, owner, name, seq)
	if ev == nil {
		return
	}
	var payload activityPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	sem := make(chan struct{}, FanoutParallel)
	var wg sync.WaitGroup
	for _, r := range payload.Recipients {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fctx.Err() != nil {
				return
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-fctx.Done():
				return
			}
			id := NotificationID(r.Principal, repo, ev.Num, r.Reason, seq)
			if s.hasUnread(fctx, r.Principal, repo, ev.Num, r.Reason) {
				return
			}
			n := Notification{
				ID: id, Repo: repo, Num: ev.Num, Kind: ev.Kind,
				Reason: r.Reason, Title: ev.Title, Actor: ev.Actor,
				State: StateUnread, CreatedAt: ev.At,
			}
			if err := s.putCreate(fctx, NotifKey(r.Principal, id), encode(n)); err != nil {
				if !store.IsPreconditionFailed(err) {
					return
				}
			}
			if err := s.indexAdd(fctx, r.Principal, IndexEntry{
				ID: id, Repo: repo, Num: ev.Num, Kind: ev.Kind,
				Reason: r.Reason, Title: ev.Title, State: StateUnread, At: ev.At,
			}); err != nil {
				return
			}
			s.ubus.publish(r.Principal, n)
		}()
	}
	wg.Wait()
}

// --- notify-retention ----------------------------------------------------------------------

// RunRetention runs one §9 pass: per-user tray compaction + collab-events
// floor. Races with live fan-out resolve by CAS: a lost race defers that
// user/repo to the next pass. Deleting a read notification under an open
// tray page is harmless (404 → the UI drops the row).
func (s *Service) RunRetention(ctx context.Context) {
	now := s.nowUTC()
	cutoff := now.AddDate(0, 0, -s.retentionDays()).Format(dateTimeFmt)
	_ = s.Store.ListPrefixes(ctx, "users/", func(m string) error {
		principal := strings.TrimSuffix(strings.TrimPrefix(m, "users/"), "/")
		if principal == "" || strings.Contains(principal, "/") {
			return nil
		}
		s.retainUser(ctx, principal, now, cutoff)
		return nil
	})
	_ = s.Store.ListPrefixes(ctx, "repos/", func(ownerSlash string) error {
		owner := strings.TrimSuffix(strings.TrimPrefix(ownerSlash, "repos/"), "/")
		if owner == "" {
			return nil
		}
		return s.Store.ListPrefixes(ctx, "repos/"+owner+"/", func(repoSlash string) error {
			repo := strings.TrimSuffix(strings.TrimPrefix(repoSlash, "repos/"+owner+"/"), "/")
			if repo == "" || strings.Contains(repo, "/") {
				return nil
			}
			s.retainRepoEvents(ctx, owner, repo, now)
			return nil
		})
	})
}

// retainUser compacts one user's tray: drops read entries older than the
// cutoff AND entries naming a deleted repo (any state — a deleted repo
// never comes back with the same threads, so its tray rows are garbage;
// their objects are Deleted, best-effort, capped), advances
// compacted_through, reconciles unread_count against actual unreads, and
// stamps swept_at. Dead-repo objects past the index hot window are swept
// by the overflow pass below (same visit, shared delete cap). Users swept
// within the last day are skipped.
func (s *Service) retainUser(ctx context.Context, principal string, now time.Time, cutoff string) {
	raw, _, err := s.getJSON(ctx, NotifIndexKey(principal))
	if err != nil || raw == nil {
		return
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		return
	}
	if ix.SweptAt != "" {
		if swept, serr := time.Parse(dateTimeFmt, ix.SweptAt); serr == nil && now.Sub(swept) < 24*time.Hour {
			return
		}
	}
	const maxDeletes = 200
	deleted := 0
	dead := map[string]bool{}
	kept := make([]IndexEntry, 0, len(ix.Entries))
	var compacted string
	for _, en := range ix.Entries {
		if o, r, ok := repoOf(en.Repo); ok && !s.repoAlive(ctx, o, r) {
			if deleted < maxDeletes {
				_ = s.Store.Delete(ctx, NotifKey(principal, en.ID), "")
				deleted++
				dead[en.ID] = true
				compacted = en.ID
				continue
			}
		} else if en.State == StateRead && en.At < cutoff && deleted < maxDeletes {
			_ = s.Store.Delete(ctx, NotifKey(principal, en.ID), "")
			deleted++
			compacted = en.ID
			continue
		}
		kept = append(kept, en)
	}
	// Overflow sweep: objects past the hot window have no index row, so
	// the loop above cannot see them — LIST the prefix (bounded, like the
	// tray overflow) and delete the ones naming a deleted repo. Live-repo
	// overflow is never touched.
	if deleted < maxDeletes {
		deleted += s.retainOverflow(ctx, principal, dead, maxDeletes-deleted)
	}
	// Reconcile: unread_count = actual unreads in the window. Entries
	// trimmed from the hot window keep their objects (LIST overflow
	// still serves them); only the count is repaired.
	unread := 0
	for _, en := range kept {
		if en.State == StateUnread {
			unread++
		}
	}
	if deleted == 0 && unread == ix.UnreadCount && ix.SweptAt != "" {
		return // nothing to write
	}
	_, _ = s.casUpdate(ctx, NotifIndexKey(principal), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var curIx IndexDoc
		if cur == nil {
			return nil, false, nil
		}
		if err := json.Unmarshal(cur, &curIx); err != nil {
			return nil, false, nil
		}
		// Re-derive from the CURRENT index (a live fan-out may have
		// added entries since our read): drop exactly the ids we
		// deleted, keep everything else.
		gone := map[string]bool{}
		for _, en := range ix.Entries {
			if en.State == StateRead && en.At < cutoff {
				gone[en.ID] = true
			}
		}
		for id := range dead {
			gone[id] = true
		}
		next := make([]IndexEntry, 0, len(curIx.Entries))
		for _, en := range curIx.Entries {
			if gone[en.ID] {
				continue
			}
			next = append(next, en)
		}
		n := 0
		for _, en := range next {
			if en.State == StateUnread {
				n++
			}
		}
		curIx.Entries = next
		curIx.UnreadCount = n
		if compacted != "" {
			curIx.CompactedThrough = compacted
		}
		curIx.SweptAt = now.Format(dateTimeFmt)
		return encode(curIx), true, nil
	})
}

// retainOverflow deletes dead-repo notification objects past the index
// hot window (they have no index row, so the window loop cannot see
// them). Bounded: at most 1000 prefix entries are scanned (the tray
// overflow bound) and at most budget objects are deleted; ids already
// handled by the window pass are skipped. Live-repo objects are never
// touched. Returns the number deleted.
func (s *Service) retainOverflow(ctx context.Context, principal string, dead map[string]bool, budget int) int {
	if budget <= 0 {
		return 0
	}
	const maxScan = 1000
	deleted, scanned := 0, 0
	_ = s.Store.List(ctx, NotifPrefix(principal), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, "/index.json") || scanned >= maxScan || deleted >= budget {
			return nil
		}
		scanned++
		raw, _, err := s.getJSON(ctx, m.Key)
		if err != nil || raw == nil {
			return nil
		}
		var n Notification
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil
		}
		if dead[n.ID] {
			return nil
		}
		o, r, ok := repoOf(n.Repo)
		if !ok || s.repoAlive(ctx, o, r) {
			return nil
		}
		_ = s.Store.Delete(ctx, m.Key, "")
		deleted++
		return nil
	})
	return deleted
}

// retainRepoEvents deletes activity events below the minimum webhook
// cursor AND older than CollabEventsFloorDays. Events are seq-ordered ≈
// time-ordered, so the scan stops at the first too-new event. Capped per
// pass; hookless repos compact everything past the floor.
func (s *Service) retainRepoEvents(ctx context.Context, owner, repo string, now time.Time) {
	floor := now.AddDate(0, 0, -CollabEventsFloorDays).Format(dateTimeFmt)
	minCursor := -1
	hooks, err := s.ListHooks(ctx, owner, repo)
	if err != nil {
		return
	}
	active := 0
	for _, h := range hooks {
		if !h.Active {
			continue
		}
		active++
		c := s.readCursor(ctx, owner, repo, h.ID)
		if minCursor < 0 || c < minCursor {
			minCursor = c
		}
	}
	if active == 0 {
		minCursor = s.collabHead(ctx, owner, repo)
	}
	if minCursor <= 1 {
		return
	}
	const maxDeletes = 500
	// maxScan bounds the reads of one pass: the loop below is one GET per
	// seq from 1, so without a cap it grows O(minCursor) forever on a busy
	// repo (a maintainer-pass unit must stay bounded, P7). Gaps count as
	// scanned — they are rare (crash-reserved seqs) and re-probing them is
	// wasted work. Deletions converge across daily passes.
	const maxScan = 600
	deleted, scanned := 0, 0
	for seq := 1; seq < minCursor && deleted < maxDeletes && scanned < maxScan; seq++ {
		scanned++
		ev := s.readActivity(ctx, owner, repo, seq)
		if ev == nil {
			continue // gap — never delete what we cannot see
		}
		if ev.At >= floor {
			break // seq-ordered ≈ time-ordered: the rest is newer
		}
		_ = s.Store.Delete(ctx, ActivityKey(owner, repo, seq), "")
		deleted++
	}
}

// collabHead returns the highest existing activity seq (bounded probe:
// the head is near next_seq; fall back to 0).
func (s *Service) collabHead(ctx context.Context, owner, repo string) int {
	raw, _, err := s.getJSON(ctx, CollabStateKey(owner, repo))
	if err != nil || raw == nil {
		return 0
	}
	var st CollabState
	if err := json.Unmarshal(raw, &st); err != nil {
		return 0
	}
	return st.NextSeq
}
