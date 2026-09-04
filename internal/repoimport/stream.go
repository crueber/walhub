// stream.go — the id-keyed replay rings (R1 B4: the pre-create task home).
//
// The core wal.TaskTable's progress broadcast lives on RepoHandle, which
// does not exist before manifest Create — so import progress/attach is
// served from THESE table-level rings, keyed by task id, always working
// (before Create, after Create, after finish within the retention
// window). Semantics mirror wal.Broadcast: 200-packet replay, 16-subscriber
// cap, lag-tolerant drop; the sender owns the ring, receivers never close;
// every goroutine exits via context. No internal/wal touch (R1 S12).
package repoimport

import (
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/wal"
)

const (
	streamReplayLen = 200
	streamSubCap    = 16
	// streamRetain is how long a finished import's ring + outcome stay
	// attachable (mirrors the wal byID janitor: 1h).
	streamRetain = time.Hour
)

// Outcome is the terminal value of an import (served exactly once as the
// SSE result|error packet, and inside GET after finish).
type Outcome struct {
	Repo       string            `json:"repo"`
	SourceURL  string            `json:"source_url"`
	HeadSHAs   map[string]string `json:"head_shas"`
	Format     string            `json:"format"`
	ImportedAt string            `json:"imported_at"`
	Err        *StatusError      `json:"-"`
}

// stream is one task's replay ring + subscriber set + terminal outcome.
type stream struct {
	mu       sync.Mutex
	target   string // "owner/name" (namespace gating for pre-create reads)
	replay   []wal.Progress
	subs     map[uint64]chan wal.Progress
	next     uint64
	done     chan struct{}
	rec      *wal.TaskRecord // latest record mirror (nil until the table starts)
	outcome  *Outcome        // set at finish, before done closes
	finished time.Time
}

// newStream builds a ring; done closes exactly once at finish.
func newStream() *stream {
	return &stream{subs: map[uint64]chan wal.Progress{}, done: make(chan struct{})}
}

// send appends to replay and fans out without ever blocking (slow
// consumers drop; progress bars are lossy by design).
func (s *stream) send(p wal.Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return // finished: replay frozen, terminal served from outcome
	default:
	}
	s.replay = append(s.replay, p)
	if len(s.replay) > streamReplayLen {
		s.replay = s.replay[len(s.replay)-streamReplayLen:]
	}
	for _, c := range s.subs {
		select {
		case c <- p:
		default:
		}
	}
}

// subscribe registers a consumer; replay is oldest-first.
func (s *stream) subscribe() (uint64, <-chan wal.Progress, []wal.Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	c := make(chan wal.Progress, streamSubCap)
	s.subs[id] = c
	replay := make([]wal.Progress, len(s.replay))
	copy(replay, s.replay)
	return id, c, replay
}

// unsubscribe closes and removes the consumer's channel (receiver-side
// close is owned here — the single exception to the channel rule,
// mirroring wal.Broadcast.Unsubscribe).
func (s *stream) unsubscribe(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.subs[id]; ok {
		close(c)
		delete(s.subs, id)
	}
}

// snapshot returns the latest record mirror (copied) and finish state.
func (s *stream) snapshot() (rec *wal.TaskRecord, outcome *Outcome, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		finished = true
	default:
	}
	if s.rec != nil {
		c := *s.rec
		rec = &c
	}
	return rec, s.outcome, finished
}

// setRecord mirrors the live table record (called after every narration).
func (s *stream) setRecord(rec wal.TaskRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := rec
	s.rec = &c
}

// finish records the terminal outcome and closes done exactly once.
func (s *stream) finish(outcome *Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	s.outcome = outcome
	s.finished = time.Now()
	close(s.done)
}

// doneChan exposes the finish channel for attach loops (select on it —
// polling snapshot would miss a terminal that lands with no further
// packet in flight).
func (s *stream) doneChan() <-chan struct{} { return s.done }

// expired reports whether a finished stream is past retention.
func (s *stream) expired(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		return false
	}
	return now.Sub(s.finished) > streamRetain
}
