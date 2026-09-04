// stream.go — live delivery: the per-user SSE bus (§5.1), the repo
// frame bus (Streamer seam), and the hand-rolled SSE writer.
//
// Handler contract (13 §2 rule 4, 06 §4 step 5): publishing NEVER blocks
// the mutating handler. Both buses are drop-oldest: a slow subscriber
// loses frames, never stalls emission (07 §6 rule). Every subscriber
// exits via its request context (13 channel rule: the bus owns the
// channels; Unsubscribe closes).
package notify

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// --- per-user bus ------------------------------------------------------------

// userSub is one live tray subscription.
type userSub struct {
	ch   chan Notification
	done chan struct{}
}

// userBus multiplexes notification frames to per-user subscribers.
type userBus struct {
	mu   sync.Mutex
	subs map[string]map[*userSub]struct{}
}

func newUserBus() *userBus { return &userBus{subs: map[string]map[*userSub]struct{}{}} }

// publish delivers n to every live subscriber of principal (non-blocking,
// drop-oldest: a full buffer sheds its head so emission never waits).
func (b *userBus) publish(principal string, n Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs[principal] {
		select {
		case sub.ch <- n:
		default:
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- n:
			default:
			}
		}
	}
}

// subscribe attaches a live feed for principal; the caller MUST call the
// returned unsubscribe (context exit), which closes the channel.
func (b *userBus) subscribe(principal string) (<-chan Notification, func()) {
	b.mu.Lock()
	sub := &userSub{ch: make(chan Notification, 16), done: make(chan struct{})}
	set, ok := b.subs[principal]
	if !ok {
		set = map[*userSub]struct{}{}
		b.subs[principal] = set
	}
	set[sub] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[principal]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(b.subs, principal)
				}
			}
			b.mu.Unlock()
			close(sub.ch)
		})
	}
}

// liveCount reports current subscribers (tests/metrics only).
func (b *userBus) liveCount(principal string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[principal])
}

// --- repo frame bus (Streamer seam) --------------------------------------------

// RepoFrame is one repo-scoped live event (issues/pulls/review/checks
// StreamEvents translated by composition). It rides the in-process repo
// bus; 08's collab stream subscribes via SubscribeRepo. There is no v1
// HTTP reader — the normative live proof (notification object +
// notification frame) rides Emit (see Decisions in 06).
type RepoFrame struct {
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	Action string `json:"action,omitempty"`
	Num    int    `json:"num,omitempty"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
	At     string `json:"at"`
}

// repoBus multiplexes RepoFrames to per-repo subscribers with a bounded
// recent ring (last RepoRing frames) for late attachers.
type repoBus struct {
	mu   sync.Mutex
	subs map[string]map[chan RepoFrame]struct{}
	ring map[string][]RepoFrame
}

// RepoRing bounds the per-repo recent ring.
const RepoRing = 64

func newRepoBus() *repoBus {
	return &repoBus{subs: map[string]map[chan RepoFrame]struct{}{}, ring: map[string][]RepoFrame{}}
}

// PublishStream publishes one repo frame (non-blocking, drop-oldest).
// Composition calls this from each package's Streamer seam.
func (s *Service) PublishStream(name, repo, action, title, state string, num int) {
	s.rbus.publish(RepoFrame{
		Name: name, Repo: repo, Action: action, Num: num,
		Title: title, State: state, At: s.nowUTC().Format(dateTimeFmt),
	})
}

func (b *repoBus) publish(f RepoFrame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := append(b.ring[f.Repo], f)
	if len(ring) > RepoRing {
		ring = ring[len(ring)-RepoRing:]
	}
	b.ring[f.Repo] = ring
	for ch := range b.subs[f.Repo] {
		select {
		case ch <- f:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- f:
			default:
			}
		}
	}
}

// SubscribeRepo attaches a live feed for repo (recent ring replayed
// first); the caller MUST call unsubscribe, which closes the channel.
func (s *Service) SubscribeRepo(repo string) (<-chan RepoFrame, []RepoFrame, func()) {
	return s.rbus.subscribe(repo)
}

func (b *repoBus) subscribe(repo string) (<-chan RepoFrame, []RepoFrame, func()) {
	b.mu.Lock()
	ch := make(chan RepoFrame, 16)
	set, ok := b.subs[repo]
	if !ok {
		set = map[chan RepoFrame]struct{}{}
		b.subs[repo] = set
	}
	set[ch] = struct{}{}
	recent := append([]RepoFrame(nil), b.ring[repo]...)
	b.mu.Unlock()
	var once sync.Once
	return ch, recent, func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[repo]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(b.subs, repo)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}
}

// --- SSE writer (hand-rolled per the dependency budget) -------------------------

// sseWriter is the 07 §6 envelope verbatim: headers first, the
// ": walgit" opener flushed immediately, "event:"/"data:" packets, a
// ": keepalive" comment every 10 s while idle. Packet writes and
// keepalives share one mutex so they cannot tear; every write carries a
// 15 s deadline so a stalled client cannot pin the goroutine.
type sseWriter struct {
	w     http.ResponseWriter
	fl    http.Flusher
	rc    *http.ResponseController
	mu    sync.Mutex
	ka    *time.Ticker
	ended bool
}

// newSSEWriter starts the envelope; ok=false when the writer cannot flush.
func newSSEWriter(w http.ResponseWriter, r *http.Request) (*sseWriter, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": walgit\n\n")
	fl.Flush()
	s := &sseWriter{w: w, fl: fl, rc: http.NewResponseController(w)}
	s.ka = time.NewTicker(10 * time.Second)
	go func() {
		for range s.ka.C {
			if !s.comment(": keepalive") {
				return
			}
		}
	}()
	return s, true
}

// event writes one packet; false when the client is gone.
func (s *sseWriter) event(name, dataJSON string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	// Best-effort deadline: writers without Hijacker/deadline support
	// (tests, some middleware) report an error here — the write below
	// is still the liveness signal.
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(s.w, "event: "+name+"\ndata: "+dataJSON+"\n\n"); err != nil {
		s.stopLocked()
		return false
	}
	s.fl.Flush()
	return true
}

// comment writes a keepalive comment; false stops the ticker.
func (s *sseWriter) comment(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(s.w, c+"\n\n"); err != nil {
		s.stopLocked()
		return false
	}
	s.fl.Flush()
	return true
}

// close ends the stream (idempotent).
func (s *sseWriter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *sseWriter) stopLocked() {
	if !s.ended {
		s.ended = true
		s.ka.Stop()
	}
}
