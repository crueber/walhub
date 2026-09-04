package pulls

import (
	"fmt"
	"sync"
	"time"
)

// This file owns the Seam 5 task surface: TaskRecords, the (repo, kind)
// single-flight task table (13_concurrency.md §3), and narration.

// TaskState is the lifecycle of a narrated task.
type TaskState string

const (
	TaskRunning TaskState = "running"
	TaskOK      TaskState = "ok"
	TaskError   TaskState = "error"
)

// TaskRecord is the wire shape of a narrated unit of long work (§8/SSE
// envelope, 07 §2 conventions: plain-text errors, RFC 3339). The leader
// mutates it while readers (SSE attach polls) marshal it — all access goes
// through notice/snapshot under mu (13: no data race, joiners never block).
type TaskRecord struct {
	mu       sync.Mutex
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Repo     string         `json:"repo"`
	Num      int            `json:"num,omitempty"`
	Strategy string         `json:"strategy,omitempty"`
	State    TaskState      `json:"state"`
	Started  string         `json:"started"`
	Finished string         `json:"finished,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	Error    string         `json:"error,omitempty"`
	Progress []string       `json:"progress"`
	Result   map[string]any `json:"result,omitempty"`
}

// notice appends a narration packet (P7: unique id, progress packets,
// attachable SSE stream — the record IS the replay buffer here).
func (r *TaskRecord) notice(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Progress = append(r.Progress, fmt.Sprintf(format, args...))
}

// initMerge records the leader's static fields (called before the record
// is shared with joiners).
func (r *TaskRecord) initMerge(num int, strategy string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Num = num
	r.Strategy = strategy
}

// setState records the terminal state transition (leader only).
func (r *TaskRecord) setState(st TaskState, summary, errText string, result map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.State = st
	r.Summary = summary
	r.Error = errText
	r.Result = result
}

// snapshot returns a race-safe copy for readers (SSE attach polls, tests).
func (r *TaskRecord) snapshot() *TaskRecord {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := &TaskRecord{
		ID: r.ID, Kind: r.Kind, Repo: r.Repo, Num: r.Num, Strategy: r.Strategy,
		State: r.State, Started: r.Started, Finished: r.Finished,
		Summary: r.Summary, Error: r.Error,
		Progress: append([]string(nil), r.Progress...),
	}
	if r.Result != nil {
		m := make(map[string]any, len(r.Result))
		for k, v := range r.Result {
			m[k] = v
		}
		cp.Result = m
	}
	return cp
}

// taskEntry is one running task: the record plus its completion. Joiners
// reuse the outcome (13 §3 bounded join is the caller's business; merge
// joins return the SHARED id immediately and poll, so no goroutine piles).
type taskEntry struct {
	wg   sync.WaitGroup
	rec  *TaskRecord
	err  error
	nums []int // mergeable batch: PRs attached while running
	mu   sync.Mutex
}

// attach adds a PR num to a running mergeable batch.
func (e *taskEntry) attach(num int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range e.nums {
		if n == num {
			return
		}
	}
	e.nums = append(e.nums, num)
}

// drain takes the attached nums (the leader consumes exactly once).
func (e *taskEntry) drain() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]int(nil), e.nums...)
	e.nums = nil
	return out
}

// taskTable is the (repo, kind) single-flight table: a second start JOINS
// the running task and reuses its outcome. Ownership: the table owns the
// map; leaders own their record until end(). Finished records stay visible
// in a bounded recent cache (terminal result/error packets are delivered
// from the task record, never the stream — 13 §6).
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

// begin starts or joins a task. ok=false ⇒ caller is the leader and MUST
// call end; ok=true ⇒ caller joined (use rec.ID to poll).
func (t *taskTable) begin(repo, kind string) (*taskEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.running[taskKey(repo, kind)]; ok {
		return e, true
	}
	e := &taskEntry{rec: &TaskRecord{
		ID:       fmt.Sprintf("%s-%d", kind, time.Now().UnixNano()),
		Kind:     kind,
		Repo:     repo,
		State:    TaskRunning,
		Started:  time.Now().UTC().Format(dateTimeFmt),
		Progress: []string{},
	}}
	e.wg.Add(1)
	t.running[taskKey(repo, kind)] = e
	return e, false
}

// end completes a task (leader only): records the outcome and wakes joiners.
// The finished record stays in the bounded recent cache for late attachers.
// The Finished stamp takes the RECORD mutex: production paths snapshot the
// live record directly (StartMerge/UpdateBranch return entry.rec.snapshot;
// merge/task polls read it), so writing it under the table mutex alone is
// a data race (09 audit). Lock order table → record matches every other
// path (no path takes record → table).
func (t *taskTable) end(repo, kind string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := taskKey(repo, kind)
	if e, ok := t.running[key]; ok {
		e.rec.mu.Lock()
		e.rec.Finished = time.Now().UTC().Format(dateTimeFmt)
		e.rec.mu.Unlock()
		e.wg.Done()
		delete(t.running, key)
		t.recent[key] = e.rec
		t.order = append(t.order, key)
		for len(t.order) > 128 {
			oldest := t.order[0]
			t.order = t.order[1:]
			// A re-started key still in order keeps its newer record.
			if _, running := t.running[oldest]; !running {
				if r, ok := t.recent[oldest]; ok && r.Finished != "" {
					delete(t.recent, oldest)
				}
			}
		}
	}
}

// get returns a snapshot of a running task's record, else the most recent
// finished record for the key (nil when never started).
func (t *taskTable) get(repo, kind string) *TaskRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := taskKey(repo, kind)
	if e, ok := t.running[key]; ok {
		return e.rec.snapshot()
	}
	if r, ok := t.recent[key]; ok {
		return r.snapshot()
	}
	return nil
}
