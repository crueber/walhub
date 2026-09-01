// maintain.go — the Maintainer (§3): the loop goroutine, assignment, the
// pass scheduler, and the 120 s mid-pass heartbeat ticker lifecycle. The
// context is the ONLY shutdown signal: drain cancels it, the running unit's
// git subprocesses die via exec.CommandContext, and no new unit starts.
package maintain

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// defaultInterval is the §2 pass cadence; defaultFollowInterval the §8 cadence.
const (
	defaultInterval       = 60 * time.Second
	defaultFollowInterval = 30 * time.Second
)

// Options configures a Maintainer; zero fields fall back to the §2 defaults.
type Options struct {
	// RebuildPhaseHook is called after each base-rebuild phase completes
	// (tests inject cancellation here to prove the kill-resume invariant,
	// §6.2 step 6). Returning an error fails the unit at that phase.
	RebuildPhaseHook func(phase string) error
	// Logf receives the structured narration lines (§13 example shape).
	Logf func(format string, args ...any)
	// Interval is maintenance.interval (0 → 60s). Negative disables the loop
	// (the follow loop can still run).
	Interval time.Duration
	// FollowInterval is maintenance.follow_interval (0 → 30s; negative = off).
	FollowInterval time.Duration
	// UnitCap overrides the §3.2 step 4 one-hour wait cap (tests).
	UnitCap time.Duration
	// SkipCap overrides the §3.2 step 5 48-skip stale-slot cap (tests).
	SkipCap int
	// HostName overrides the heartbeat object name (maintenance.host);
	// default: the engine's instance id.
	HostName string
	// Plugs (nil → the bind_wal wiring supplies real ones; tests use fakes):
	Planner BundlePlanner
	Leaser  Leaser
	Fscker  FsckRunner
	Follow  FollowFetcher
}

// Maintainer owns the loop goroutine (§3.1) and the follow goroutine (§8),
// spawned independently by the server per §8.10 spawn order.
type Maintainer struct {
	eng Engine
	opt Options

	logf func(format string, args ...any)
	now  func() time.Time

	interval        time.Duration
	followInterval  time.Duration
	unitCap         time.Duration
	skipCap         int
	staleSkipStates map[string]bool

	metrics *maintainMetrics

	// freeSpace is the §6.2 disk-free pre-flight (statfsFree in prod;
	// injectable in tests).
	freeSpace func(dir string) (uint64, error)

	// Pass-goroutine-owned heartbeat state (§7): the ticker is created when
	// the first unit of a pass starts, stopped when the pass's last unit
	// ends, and ticks are handled on the pass goroutine itself — exactly one
	// heartbeat writer at any moment.
	hbMu      sync.Mutex
	hbTicker  *time.Ticker
	hbCurrent *proto.MaintainerHeartbeat
	hbStarted time.Time

	lastUnitMu sync.Mutex
	lastUnit   string

	roundMu sync.Mutex
	rounds  map[string]*FollowRound // §8.5: per-instance status, NOT the WAL
}

// New builds a Maintainer. The engine binding (bind_wal.go) supplies the
// wal-backed Engine; Options plugs test fakes and overrides cadences.
func New(eng Engine, opt Options) *Maintainer {
	m := &Maintainer{
		eng:             eng,
		opt:             opt,
		logf:            opt.Logf,
		now:             time.Now,
		metrics:         newMaintainMetrics(),
		rounds:          map[string]*FollowRound{},
		freeSpace:       statfsFree,
		staleSkipStates: map[string]bool{"pending": true, "blocked": true, "too-small": true, "skipped": true, "unavailable": true},
	}
	if m.logf == nil {
		m.logf = func(format string, args ...any) { log.Printf("maintain: "+format, args...) }
	}
	m.interval = opt.Interval
	if m.interval == 0 {
		m.interval = defaultInterval
	}
	m.followInterval = opt.FollowInterval
	if m.followInterval == 0 {
		m.followInterval = defaultFollowInterval
	}
	m.unitCap = opt.UnitCap
	if m.unitCap == 0 {
		m.unitCap = unitCap
	}
	m.skipCap = opt.SkipCap
	if m.skipCap == 0 {
		m.skipCap = 48
	}
	return m
}

// Metrics returns the current counter snapshot (§12; the server renders it).
func (m *Maintainer) Metrics() MetricsSnapshot { return m.metrics.snapshot() }

// Run is the loop goroutine (§3.1): ticker at maintenance.interval; each tick
// is one pass. ctx cancel (drain phase 1) stops between passes.
func (m *Maintainer) Run(ctx context.Context) {
	if m.interval < 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.RunPass(ctx)
		}
	}
}

// RunPass performs one pass (§3.2): purge, heartbeat pre-write, one bounded
// unit per assigned repo in registration order, heartbeat post-write, metrics.
// Exported for CLI --once and tests.
func (m *Maintainer) RunPass(ctx context.Context) {
	start := m.now()
	defer func() {
		if r := recover(); r != nil {
			m.logf("pass panicked: %v\n%s", r, debug.Stack())
		}
		m.metrics.lastPassNanos.Store(int64(m.now().Sub(start)))
		m.metrics.passes.Add(1)
		m.hbStop()
	}()

	cfg := m.eng.HostConfig()
	if cfg == nil {
		m.logf("pass skipped: no host config")
		return
	}
	repos := m.eng.Repos()
	m.hbStarted = start

	// Purge (§4.2): heartbeats older than 24 h are deleted by a
	// maintain-role host as a cheap prefix list at pass start.
	if st := m.store(); st != nil {
		if purged, err := purgeHeartbeats(ctx, st, m.now(), hbPurgeAfter); err != nil {
			m.logf("heartbeat purge failed: %v", err)
		} else if len(purged) > 0 {
			m.logf("purged %d stale heartbeats", len(purged))
		}
	}

	// Heartbeat pre-write (§4.2): last_pass_at = now, passes++, last_unit cleared.
	hb := newHeartbeat(m.host(), repos, cfg.Placement.MaintainExclude, cfg, start)
	hb.LastPassAt = ptrTs(m.now())
	hb.Passes = uint64(m.metrics.passes.Load()) + 1
	if st := m.store(); st != nil {
		if err := writeHeartbeat(ctx, st, hb); err != nil {
			m.logf("heartbeat write failed: %v", err)
		} else {
			m.metrics.heartbeatWrites.Add(1)
		}
	}
	m.hbStart(hb)
	m.setLastUnit("")

	nAssigned := 0
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		if !assigned(repo, cfg.Placement.Maintain, cfg.Placement.MaintainExclude) {
			continue
		}
		nAssigned++
		m.processRepo(ctx, repo, cfg)
	}

	// Heartbeat post-write (§4.2): last_unit = "<repo> <kind> <detail>".
	hb.LastUnit = m.getLastUnit()
	hb.LastPassAt = ptrTs(m.now())
	if st := m.store(); st != nil {
		if err := writeHeartbeat(ctx, st, hb); err == nil {
			m.metrics.heartbeatWrites.Add(1)
		}
	}
	m.logf("pass done host=%s nassigned=%d took=%s", m.host(), nAssigned, m.now().Sub(start).Round(time.Millisecond))
}

// processRepo is §3.2 steps 1–5 for one repo: repeat select→run until Select()
// returns idle, guarded by the stale-skip cap.
func (m *Maintainer) processRepo(ctx context.Context, repo string, cfg *config.Config) {
	defer func() {
		if r := recover(); r != nil {
			m.logf("%s: unit panicked: %v\n%s", repo, r, debug.Stack())
		}
	}()
	skips := 0
	snap, rep, err := m.loadRepo(ctx, repo, cfg)
	if err != nil {
		m.logf("%s: snapshot failed: %v", repo, err)
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		sel := snap.Select(m.now())
		if sel.Kind == "" {
			m.metrics.recordUnit(sel.Kind, OutcomeIdle, 0)
			return
		}
		// Wrong-host planning (§4.1): units 3–6 require the object bytes
		// locally; checkpoint (1) and repair (2) always run.
		if wrongHostUnit(sel.Kind) && !fits(snap.Eff, snap.Manifest) {
			m.logf("%s unit=%s outcome=wrong-host (pack set does not fit)", repo, sel.Kind)
			m.metrics.recordUnit(sel.Kind, OutcomeWrongHost, 0)
			snap.Skip[sel.Kind] = true
		}
		unitStart := m.now()
		outcome, detail := m.runUnit(ctx, rep, snap, sel)
		m.metrics.recordUnit(sel.Kind, outcome, int64(m.now().Sub(unitStart)))
		m.logf("%s unit=%s outcome=%s (%s)", repo, sel.Kind, outcome, detail)
		m.setLastUnit(repo + " " + sel.Kind + " " + detail)
		snap.Skip[sel.Kind] = true
		// §3.2 step 5: stale-slot skip cap — pathological bundle planning
		// ends the repo's turn.
		if sel.Kind == KindBundle && m.staleSkipStates[detailState(detail)] {
			skips++
			if skips >= m.skipCap {
				m.logf("%s: stale-slot skip cap (%d) reached; ending turn", repo, m.skipCap)
				return
			}
		}
		// Re-plan: after a bundle unit that built nothing (no slot settled,
		// retention no-op), the loop re-selects so a pending BaseRebuild or
		// compaction triggered by the same pass is not starved by bundle
		// bookkeeping (§3.2 step 5).
	}
}

// loadRepo builds the §4 snapshot for one repo: refs-level sync (cheap), the
// effective config (host config ⊕ repo settings, fresh each pass — D24), the
// cached fsck.pb, and local pack-dir state.
func (m *Maintainer) loadRepo(ctx context.Context, repo string, host *config.Config) (*Snapshot, Repo, error) {
	rep, err := m.eng.Open(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	if err := rep.SyncRefs(ctx); err != nil {
		return nil, nil, err
	}
	mst, _ := rep.Manifest()
	eff, err := effectiveConfig(host, mst)
	if err != nil {
		return nil, nil, err
	}
	fsck := &proto.FsckReport{}
	has, err := getFsckReport(ctx, m.store(), rep.Prefix(), fsck)
	if err != nil {
		return nil, nil, err
	}
	if !has {
		fsck = nil
	}
	return &Snapshot{
		ID:       repo,
		Manifest: mst,
		Eff:      eff,
		Fsck:     fsck,
		Local:    localPackState(rep),
	}, rep, nil
}

// runUnit is §3.2 steps 3–4: wrap the unit in a task ((repo,kind)
// single-flight join), run it on its OWN goroutine whose context is the
// maintainer's (NOT the wait cap's), and select on completion vs the cap. On
// timeout the task is NOT killed — the pass moves on and the task remains
// discoverable in the task table ("still running; will re-check next pass").
func (m *Maintainer) runUnit(ctx context.Context, rep Repo, snap *Snapshot, sel Selection) (Outcome, string) {
	type result struct {
		outcome Outcome
		detail  string
	}
	done := make(chan result, 1)
	unitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{OutcomeError, fmt.Sprintf("panic: %v", r)}
			}
		}()
		var res result
		err := m.eng.Tasks().Run(unitCtx, rep.ID(), sel.Kind,
			map[string]string{"reason": sel.Reason},
			func(tctx context.Context, t TaskLogger) error {
				res.outcome, res.detail = m.execUnit(tctx, rep, snap, sel, t)
				return nil // outcomes are values, not errors
			})
		if err != nil {
			res = result{OutcomeError, err.Error()}
		}
		done <- res
	}()

	// The 120 s heartbeat ticker (§7): the pass goroutine selects on
	// ticker.C alongside unit completion — the mid-pass tick and the
	// pre/post-pass writes all happen on the pass goroutine.
	ticks := m.hbTicks()
	timer := time.NewTimer(m.unitCap)
	defer timer.Stop()
	for {
		select {
		case res := <-done:
			return res.outcome, res.detail
		case <-timer.C:
			return OutcomeTimeout, "still running; will re-check next pass"
		case <-ticks:
			m.hbTick(ctx)
			ticks = m.hbTicks() // re-arm; C is left unread after Stop (§7)
		}
	}
}

// execUnit dispatches one unit (§4 priority rows).
func (m *Maintainer) execUnit(ctx context.Context, rep Repo, snap *Snapshot, sel Selection, t TaskLogger) (Outcome, string) {
	switch sel.Kind {
	case KindCheckpoint:
		return m.runCheckpoint(ctx, rep, snap, t, sel.Reason)
	case KindRepair:
		return m.runRepair(ctx, rep, snap, t)
	case KindBundle:
		return m.runBundles(ctx, rep, snap, t)
	case KindCompact:
		return m.runCompact(ctx, rep, snap, t)
	case KindRevIndex:
		return m.runRevIndex(ctx, rep, snap, t)
	case KindFsck:
		return m.runFsck(ctx, rep, snap, t)
	default:
		return OutcomeError, "unknown unit kind " + sel.Kind
	}
}

// ---- heartbeat ticker lifecycle (§7) ----------------------------------------

func (m *Maintainer) hbStart(hb *proto.MaintainerHeartbeat) {
	m.hbMu.Lock()
	defer m.hbMu.Unlock()
	m.hbCurrent = hb
	m.hbTicker = time.NewTicker(hbInterval)
}

func (m *Maintainer) hbStop() {
	m.hbMu.Lock()
	defer m.hbMu.Unlock()
	if m.hbTicker != nil {
		m.hbTicker.Stop()
		m.hbTicker = nil
	}
	m.hbCurrent = nil
}

func (m *Maintainer) hbTicks() <-chan time.Time {
	m.hbMu.Lock()
	defer m.hbMu.Unlock()
	if m.hbTicker == nil {
		return nil
	}
	return m.hbTicker.C
}

// hbTick rewrites the heartbeat mid-pass (same content, fresh put — Overwrite
// semantics, no CAS) so a pass lasting hours still looks alive (§4.2).
func (m *Maintainer) hbTick(ctx context.Context) {
	m.hbMu.Lock()
	hb := m.hbCurrent
	m.hbMu.Unlock()
	if hb == nil {
		return
	}
	hb.LastPassAt = ptrTs(m.now())
	if st := m.store(); st != nil {
		if err := writeHeartbeat(ctx, st, hb); err == nil {
			m.metrics.heartbeatWrites.Add(1)
		}
	}
}

// ---- small helpers -----------------------------------------------------------

func (m *Maintainer) store() store.ObjectStore {
	if m.eng == nil {
		return nil
	}
	return m.eng.Store()
}

func (m *Maintainer) host() string {
	if m.opt.HostName != "" {
		return m.opt.HostName
	}
	if m.eng == nil {
		return "unknown"
	}
	return m.eng.InstanceID()
}

func (m *Maintainer) setLastUnit(s string) {
	m.lastUnitMu.Lock()
	m.lastUnit = s
	m.lastUnitMu.Unlock()
}

func (m *Maintainer) getLastUnit() string {
	m.lastUnitMu.Lock()
	defer m.lastUnitMu.Unlock()
	return m.lastUnit
}

// detailState extracts the planner state token from a bundles detail line
// ("slot=<n> state=<state> …").
func detailState(detail string) string {
	for _, f := range strings.Fields(detail) {
		if s, ok := strings.CutPrefix(f, "state="); ok {
			return s
		}
	}
	return ""
}
