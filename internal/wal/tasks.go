// tasks.go — the task system (doc 05 §5.8): narrated long work with (repo,kind)
// single-flight join, the lag-tolerant Broadcast primitive, replay buffers, and
// drain hooks.
package wal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Packet is what a task publishes (the frozen Progress type carries the
// SSE envelope payloads: notice | progress | task | result | error).
type Packet = Progress

// Broadcast[T] is the lag-tolerant fan-out of §5.8: per-subscriber bounded
// channels, non-blocking Send (full channel = drop), and a 200-packet replay
// ring so late attachers get history. A subscriber channel is owned and
// closed by Unsubscribe only.
type Broadcast[T any] struct {
	mu   sync.Mutex
	subs map[uint64]chan T
	next uint64
	buf  []T // replay ring (200)
}

const (
	bcastSubCap    = 16
	bcastReplayLen = 200
)

// Subscribe registers a subscriber; returns its id, receive channel, and the
// replayed history (oldest first).
func (b *Broadcast[T]) Subscribe() (id uint64, ch <-chan T, replay []T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[uint64]chan T{}
	}
	id = b.next
	b.next++
	c := make(chan T, bcastSubCap)
	b.subs[id] = c
	replay = make([]T, len(b.buf))
	copy(replay, b.buf)
	return id, c, replay
}

// Unsubscribe closes and removes the subscriber's channel.
func (b *Broadcast[T]) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.subs[id]; ok {
		close(c)
		delete(b.subs, id)
	}
}

// Send delivers v to every subscriber without ever blocking; a slow consumer
// drops packets (progress bars are lossy by design; the replay ring covers
// reconnects). Also appended to the replay ring.
func (b *Broadcast[T]) Send(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, v)
	if len(b.buf) > bcastReplayLen {
		b.buf = b.buf[len(b.buf)-bcastReplayLen:]
	}
	for _, c := range b.subs {
		select {
		case c <- v:
		default: // lag-tolerant drop
		}
	}
}

// ---- TaskTable ---------------------------------------------------------------

// TaskTable is the per-instance task registry (§5.8), owned by the Registry.
type TaskTable struct {
	mu       sync.Mutex
	running  map[string]*runningTask  // key "repo/kind"
	recent   map[string][]*TaskRecord // per repo ring (30)
	byID     map[string]*TaskRecord
	hostname string
	ctx      context.Context // registry lifetime; cancel = drain phase 1
	drainMu  sync.Mutex
	draining bool
}

type runningTask struct {
	rec     *TaskRecord
	cancel  context.CancelFunc
	done    chan struct{} // closed exactly once by the task goroutine's defer
	drone   *Broadcast[Packet]
	outcome *TaskRecord // final record, set before done closes
	err     error       // body error, set before done closes (propagated to callers)
}

func newTaskTable(hostname string, ctx context.Context) *TaskTable {
	return &TaskTable{
		running:  map[string]*runningTask{},
		recent:   map[string][]*TaskRecord{},
		byID:     map[string]*TaskRecord{},
		hostname: hostname,
		ctx:      ctx,
	}
}

// Task is the reporter handed to a running task body.
type Task struct {
	rec   *TaskRecord
	table *TaskTable
	ctx   context.Context
	bcast *Broadcast[Packet]
	alarm *time.Timer // bounded-watchdog safety: task hard cap handled by ctx
}

// Ctx is the task's context — canceled by drain (phase 1) or registry close.
func (t *Task) Ctx() context.Context { return t.ctx }

// Record returns the live record (callers copy).
func (t *Task) Record() TaskRecord { return *t.rec }

// Errorf records a formatted error line in the task's narration (test and
// reporter convenience; failures are still returned by the task body).
func (t *Task) Errorf(format string, args ...any) {
	t.Notice("ERROR " + fmt.Sprintf(format, args...))
}

// Notice publishes a notice packet and appends to the record's log tail (60).
func (t *Task) Notice(text string) {
	t.rec.LogTail = append(t.rec.LogTail, text)
	if len(t.rec.LogTail) > 60 {
		t.rec.LogTail = t.rec.LogTail[len(t.rec.LogTail)-60:]
	}
	t.bcast.Send(Packet{Kind: "notice", Text: text})
	t.table.publishRecord(t)
}

// Progress publishes a progress bar (latest bar per label wins at the SSE layer).
func (t *Task) Progress(label string, done, total uint64, unit string) {
	p := Packet{Kind: "progress", Label: label, Done: done, Unit: unit}
	if total > 0 {
		p.Total = &total
		pct := float64(done) / float64(total) * 100
		p.Percent = &pct
	}
	if p.Total != nil { // the record's snapshot keeps the latest bar with a total
		t.rec.Progress = &p
	}
	t.bcast.Send(p)
}

// publishRecord mirrors the record into the repo broadcast (task packet).
func (t *TaskTable) publishRecord(task *Task) {
	rec := *task.rec
	task.bcast.Send(Packet{Kind: "task", Task: &rec})
}

// Run executes fn as task (repo, kind) — the (repo,kind) single-flight join:
// a second start JOINS the running one and reuses its outcome (bounded by the
// joiner's ctx). Params describe the task for the API surface.
func (t *TaskTable) Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, task *Task) error) (*TaskRecord, error) {
	key := repo + "/" + kind
	for {
		t.mu.Lock()
		if rt, ok := t.running[key]; ok {
			t.mu.Unlock()
			// Joiner: await completion up to the caller's ctx (bounded wait,
			// 13 §3), then reuse the outcome.
			select {
			case <-rt.done:
				return rt.outcome, rt.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if t.draining {
			t.mu.Unlock()
			return nil, fmt.Errorf("503 interrupted: instance shut down; will be retried by the next pass")
		}
		rt := &runningTask{
			rec: &TaskRecord{
				ID:       newTaskID(),
				Kind:     kind,
				Repo:     repo,
				Hostname: t.hostname,
				Started:  time.Now().UTC().Format(time.RFC3339Nano),
				Params:   params,
			},
			done:  make(chan struct{}),
			drone: &Broadcast[Packet]{},
		}
		t.running[key] = rt
		t.byID[rt.rec.ID] = rt.rec
		t.mu.Unlock()

		runCtx, cancel := context.WithCancel(t.ctx)
		rt.cancel = cancel
		task := &Task{rec: rt.rec, table: t, ctx: runCtx, bcast: rt.drone}
		t.publishRecord(task)

		go func() {
			var ok bool
			err := fn(runCtx, task)
			ok = err == nil
			fin := time.Now().UTC()
			rt.rec.Finished = fin.Format(time.RFC3339Nano)
			rt.rec.OK = &ok
			if err != nil {
				rt.rec.Summary = err.Error()
				rt.err = err
			}
			if st, serr := time.Parse(time.RFC3339Nano, rt.rec.Started); serr == nil {
				rt.rec.ElapsedMS = fin.Sub(st).Milliseconds()
			}

			// Terminal packet + table bookkeeping; done closes exactly once.
			t.mu.Lock()
			delete(t.running, key)
			res := *rt.rec
			t.recent[repo] = append(t.recent[repo], &res)
			if len(t.recent[repo]) > 30 {
				t.recent[repo] = t.recent[repo][len(t.recent[repo])-30:]
			}
			t.mu.Unlock()
			pkt := Packet{Kind: "task", Task: &res}
			task.bcast.Send(pkt)
			rt.outcome = &res
			close(rt.done)
			cancel()
		}()

		// The leader also awaits (its caller wants the outcome).
		select {
		case <-rt.done:
			return rt.outcome, rt.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func newTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// List returns the 30 most recent finished records for a repo (newest last).
func (t *TaskTable) List(repo string) []*TaskRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]*TaskRecord(nil), t.recent[repo]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
	return out
}

// Get returns a task record by id (janitor evicts finished records after 1h).
func (t *TaskTable) Get(id string) *TaskRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id]
}

// janitor evicts finished records after 1 h (§5.8 by_id janitor).
func (t *TaskTable) janitor(ctx context.Context) {
	tk := time.NewTicker(5 * time.Minute)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			cut := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
			t.mu.Lock()
			for id, rec := range t.byID {
				if rec.Finished != "" && rec.Finished < cut {
					delete(t.byID, id)
				}
			}
			t.mu.Unlock()
		}
	}
}

// Drain is the phase-1 drain hook: flip the flag and cancel every running
// task's context; each interrupted task records the 503 failure (§5.8).
func (t *TaskTable) Drain() {
	t.drainMu.Lock()
	t.mu.Lock()
	if t.draining {
		t.mu.Unlock()
		t.drainMu.Unlock()
		return
	}
	t.draining = true
	running := make([]*runningTask, 0, len(t.running))
	for _, rt := range t.running {
		running = append(running, rt)
	}
	t.mu.Unlock()

	for _, rt := range running {
		if rt.cancel != nil {
			rt.cancel()
		}
	}
	t.drainMu.Unlock()
}
