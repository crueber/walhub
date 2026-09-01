package server

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Inflight is the global in-flight request gauge (§2.1). n increments in the
// request-id middleware and decrements ONLY when the response body is fully
// written (write-completion hook). high is the configured advisory cap
// server.max_concurrent_requests.
type Inflight struct {
	n    atomic.Int64
	high int64
}

// OverCap reports the advisory cap being exceeded (advisory: logged, never refused).
func (g *Inflight) OverCap() bool { return g.n.Load() > g.high }

// N snapshots the count.
func (g *Inflight) N() int64 { return g.n.Load() }

// RepoSemaphores is per-repo git concurrency, striped by repo id (§4). Taken
// inside handlers with TryAcquire → 503 Retry-After: 15 when full — never a
// blocking wait from a request goroutine.
type RepoSemaphores struct {
	cap  int
	mu   sync.Mutex
	m    map[string]*repoSem
	hits atomic.Int64
}

func NewRepoSemaphores(capacity int) *RepoSemaphores {
	if capacity <= 0 {
		capacity = 64
	}
	return &RepoSemaphores{cap: capacity, m: map[string]*repoSem{}}
}

// TryAcquire takes the repo slot; nil → 503 with Retry-After.
func (rs *RepoSemaphores) TryAcquire(repo string) func() {
	rs.mu.Lock()
	s, ok := rs.m[repo]
	if !ok {
		s = newRepoSem(rs.cap)
		rs.m[repo] = s
	}
	rs.mu.Unlock()
	if !s.tryAdd() {
		rs.hits.Add(1)
		return nil
	}
	return s.release
}

// BusyCount snapshots refusals (metrics).
func (rs *RepoSemaphores) BusyCount() int64 { return rs.hits.Load() }

type repoSem struct {
	n   atomic.Int64
	cap int64
}

func newRepoSem(capacity int) *repoSem { return &repoSem{cap: int64(capacity)} }

func (s *repoSem) tryAdd() bool {
	for {
		n := s.n.Load()
		if n >= s.cap {
			return false
		}
		if s.n.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (s *repoSem) release() { s.n.Add(-1) }

// DrainPhase is the two-phase drain state (§12).
type DrainPhase int

const (
	DrainNone   DrainPhase = iota
	DrainPhase1            // bounded 30 s: maintenance stops; serving stays up
	DrainPhase2            // readyz → 503 draining; new fetch/push/LFS refused
)

// DrainState is driven by the SIGTERM/SIGINT handler (§12).
type DrainState struct {
	mu       sync.RWMutex
	phase    DrainPhase
	drainCtx context.Context // cancelled at phase 1
	appCtx   context.Context // cancelled at phase 2 (governs remaining requests)
}

func NewDrainState() *DrainState { return &DrainState{} }

// Phase snapshots the current phase.
func (d *DrainState) Phase() DrainPhase {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.phase
}

// Draining reports phase ≥ 1.
func (d *DrainState) Draining() bool { return d.Phase() != DrainNone }

// Phase2 reports the final phase (readyz flips, new git/LFS refused).
func (d *DrainState) Phase2() bool { return d.Phase() == DrainPhase2 }

// Begin1 enters phase 1. The contexts come from the process supervisor:
// drainCtx cancelled now, appCtx cancelled at the phase-2 boundary with
// server.drain_timeout governing the remaining requests.
func (d *DrainState) Begin1(drainCtx, appCtx context.Context) {
	d.mu.Lock()
	d.phase = DrainPhase1
	d.drainCtx = drainCtx
	d.appCtx = appCtx
	d.mu.Unlock()
}

// Begin2 enters phase 2 (stop accepting; in-flight capped).
func (d *DrainState) Begin2() {
	d.mu.Lock()
	d.phase = DrainPhase2
	d.mu.Unlock()
}

// drainGate is the §4.3/§12 refusal gate: 503 + Retry-After: 15, and for
// git-ish clients (per §4.2) the pkt-ERR shape. Returns true when refused.
func (s *Server) drainGate(w http.ResponseWriter, gitish bool, pkt func(msg string)) bool {
	if s.drain.Phase() != DrainPhase2 {
		return false
	}
	w.Header().Set("Retry-After", "15")
	if gitish && pkt != nil {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		pkt("walgit: instance is draining; retry shortly")
		return true
	}
	plainStatus(w, http.StatusServiceUnavailable, "walgit: instance is draining; retry shortly")
	return true
}

// Run drives the two-phase drain around an already-serving http.Server (§12):
//
//	Phase 1 (bounded 30 s): maintenance stops (drainCtx cancelled); serving +
//	/readyz stay up; maintenance-ops endpoints answer the drain 503 shape.
//	Phase 2: http.Server.Shutdown stops accepting; readyz → 503 draining;
//	new fetch/push/LFS refused; in-flight capped at drainTimeout (appCtx);
//	exit when Inflight hits zero or the timeout fires.
//
// The caller supplies the contexts from its supervisor and the http.Server to
// shut down; Run blocks until phase 2 completes.
func (s *Server) RunPhase1(drainCtx, appCtx context.Context) {
	s.drain.Begin1(drainCtx, appCtx)
}

// RunPhase2 performs the phase-2 boundary: readyz flips, Shutdown stops
// accepting, and it blocks until the in-flight count hits zero or the
// server.drain_timeout deadline fires.
func (s *Server) RunPhase2(srv *http.Server) {
	s.drain.Begin2()
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(),
			time.Duration(s.cfg.Server.DrainTimeout))
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	deadline := s.Now().Add(time.Duration(s.cfg.Server.DrainTimeout))
	for s.inflight.N() > 0 && s.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}
