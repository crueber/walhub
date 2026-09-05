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
	"context"
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

// RepoFrame is one repo-scoped live event (feature StreamEvents
// translated by composition). It rides the in-process repo bus; 08's
// collab stream (collab.go) subscribes via SubscribeRepo and serves it
// over GET /{o}/{r}/api/collab/stream. Name is the 08 §4 kind
// (issue|issue_event|pull|review|thread|check|release|access); the
// optional fields carry the kind's extras (checks: SHA/Context/Combined;
// threads: TID; releases: Tag; pulls: HeadSHA; every frame: Actor).
type RepoFrame struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	Action   string `json:"action,omitempty"`
	Num      int    `json:"num,omitempty"`
	Title    string `json:"title,omitempty"`
	State    string `json:"state,omitempty"`
	At       string `json:"at"`
	Actor    string `json:"actor,omitempty"`
	Sha      string `json:"sha,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Tid      string `json:"tid,omitempty"`
	Context  string `json:"context,omitempty"`
	Combined string `json:"combined_state,omitempty"`
	Seq      int    `json:"seq,omitempty"`
}

// repoBus multiplexes RepoFrames to per-repo subscribers with a bounded
// recent ring (last RepoRing frames) for late attachers.
//
// Memory bound: at most RepoBusMaxRepos repo rings are retained, each at
// most RepoRing frames. A repo's ring is dropped when its last subscriber
// leaves (mirroring the subs cleanup in unsubscribe); repos that only ever
// see publishes (no live subscribers) are capped by LRU eviction. Eviction
// drops the replay ring only — live subscriber channels are never closed
// by it, and the ring rebuilds on the next publish.
//
// Replay nuance: a subscriber attaching while (or before the idle evict of)
// others are attached replays recent frames; one attaching after a fully
// idle period — or to an LRU-evicted repo — starts from the live tail only.
// Durable history is the feature timeline/API (08: frames invalidate
// caches, the timeline is the backfill truth), never this ring.
//
// ### Concurrency
// Hazard: publish/subscribe/unsubscribe racing on the ring maps, or an
// eviction closing a live channel (send-on-closed panic). Avoidance: the
// ring, recency, and subs maps are all guarded by the pre-existing b.mu
// (no new lock); eviction deletes ring entries only, never subs entries or
// channels; channel close stays solely in the unsubscribe path (the bus
// owns the channels; receivers never close — 13 §5).
type repoBus struct {
	mu   sync.Mutex
	subs map[string]map[chan RepoFrame]struct{}
	ring map[string][]RepoFrame
	last map[string]uint64 // LRU recency per retained repo (guarded by mu)
	clk  uint64            // LRU clock (guarded by mu)
}

// RepoRing bounds the per-repo recent ring.
const RepoRing = 64

// RepoBusMaxRepos bounds how many repo rings are retained process-wide
// (RepoBusMaxRepos * RepoRing frames worst case).
const RepoBusMaxRepos = 256

func newRepoBus() *repoBus {
	return &repoBus{
		subs: map[string]map[chan RepoFrame]struct{}{},
		ring: map[string][]RepoFrame{},
		last: map[string]uint64{},
	}
}

// PublishStream publishes one repo frame (non-blocking, drop-oldest).
// Composition calls this from each package's Streamer seam. The name is
// the 08 §4 kind; title/state carry the human summary.
func (s *Service) PublishStream(name, repo, action, title, state string, num int) {
	s.PublishFrame(RepoFrame{
		Name: name, Repo: repo, Action: action, Num: num,
		Title: title, State: state, At: s.nowUTC().Format(dateTimeFmt),
	})
}

// PublishFrame publishes a full repo frame (non-blocking, drop-oldest).
// Composition adapters prefer this when the feature event carries
// kind-specific extras (checks SHA/context, thread TID, release tag).
// The caller sets At; an empty At is stamped here.
func (s *Service) PublishFrame(f RepoFrame) {
	if f.At == "" {
		f.At = s.nowUTC().Format(dateTimeFmt)
	}
	s.rbus.publish(f)
}

func (b *repoBus) publish(f RepoFrame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := append(b.ring[f.Repo], f)
	if len(ring) > RepoRing {
		ring = ring[len(ring)-RepoRing:]
	}
	b.ring[f.Repo] = ring
	b.clk++
	b.last[f.Repo] = b.clk
	// One publish adds at most one repo key, so a single eviction restores
	// the bound; an `if` (not `for`) also guarantees no lock-held spin if
	// the ring/last invariant ever diverged (evictLRULocked no-ops then).
	if len(b.ring) > RepoBusMaxRepos {
		b.evictLRULocked()
	}
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

// repoLiveCount reports current repo subscribers (tests only).
func (s *Service) repoLiveCount(repo string) int {
	return s.rbus.liveCount(repo)
}

// evictLRULocked drops the least-recently-published retained ring so the
// bus stays within RepoBusMaxRepos repos. Repos with live subscribers are
// evicted only when every retained repo has subscribers; subscriber
// channels are never touched — the ring rebuilds on the next publish.
// Caller holds b.mu.
func (b *repoBus) evictLRULocked() {
	victim, victimLast, found := "", uint64(0), false
	for repo, last := range b.last {
		if len(b.subs[repo]) > 0 {
			continue // prefer evicting idle repos; live replay has an audience
		}
		if !found || last < victimLast {
			victim, victimLast, found = repo, last, true
		}
	}
	if !found {
		for repo, last := range b.last {
			if !found || last < victimLast {
				victim, victimLast, found = repo, last, true
			}
		}
	}
	if !found {
		return
	}
	delete(b.ring, victim)
	delete(b.last, victim)
}

// ringCount reports retained repo rings (tests only).
func (b *repoBus) ringCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.ring)
}

// liveCount reports current repo subscribers (tests only).
func (b *repoBus) liveCount(repo string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[repo])
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
	if _, ok := b.ring[repo]; ok {
		b.clk++
		b.last[repo] = b.clk
	}
	b.mu.Unlock()
	var once sync.Once
	return ch, recent, func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[repo]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(b.subs, repo)
					// Last subscriber out: drop the replay ring too, or the
					// map grows one entry per repo ever published.
					delete(b.ring, repo)
					delete(b.last, repo)
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
	done  chan struct{} // closed by stopLocked; the keepalive goroutine's exit signal (sender owns)
	ctx   context.Context
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
	s := &sseWriter{w: w, fl: fl, rc: http.NewResponseController(w), done: make(chan struct{}), ctx: r.Context()}
	s.ka = time.NewTicker(10 * time.Second)
	// Keepalive goroutine (#13): exits via done (teardown) or the request
	// context (client disconnect) — never by ranging over ka.C, whose
	// channel Stop does not close (13 channel rule: sender owns and closes).
	go func() {
		defer s.ka.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-s.ctx.Done():
				return
			case <-s.ka.C:
				if !s.comment(": keepalive") {
					return
				}
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
		close(s.done)
		s.ka.Stop()
	}
}
