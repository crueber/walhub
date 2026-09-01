// metrics.go — the maintain-owned observability counters (§12). The
// Prometheus text exposition lives in the server; these are the atomic values
// it renders. Nothing here blocks.
package maintain

import (
	"sync"
	"sync/atomic"
)

type unitKey struct {
	kind    string
	outcome Outcome
}

// maintainMetrics are the §3.2 step 6 / §12 series sources.
type maintainMetrics struct {
	passes          atomic.Int64
	lastPassNanos   atomic.Int64
	heartbeatWrites atomic.Int64

	mu        sync.Mutex
	units     map[unitKey]int64
	unitNanos map[string]int64 // per kind, cumulative

	// last-seen gauge values (§11 lag gauges; §9.1 missing objects).
	checkpointLagEntries atomic.Int64
	checkpointAgeSecs    atomic.Int64
	missingObjects       atomic.Int64
	repairObjects        atomic.Int64
	followRounds         map[string]int64 // repo,outcome → count (mutex-guarded)
	followRefs           atomic.Int64
}

// MetricsSnapshot is a point-in-time copy of the maintain counters.
type MetricsSnapshot struct {
	Passes                int64
	LastPassNanos         int64
	HeartbeatWrites       int64
	Units                 map[string]map[Outcome]int64 // kind → outcome → count
	UnitNanos             map[string]int64
	CheckpointLagEntries  int64
	CheckpointAgeSeconds  int64
	MissingObjects        int64
	RepairObjects         int64
	FollowRoundsByOutcome map[string]int64 // "<repo> <outcome>" → count
	FollowRefs            int64
}

func newMaintainMetrics() *maintainMetrics {
	return &maintainMetrics{
		units:        map[unitKey]int64{},
		unitNanos:    map[string]int64{},
		followRounds: map[string]int64{},
	}
}

func (m *maintainMetrics) recordUnit(kind string, outcome Outcome, nanos int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.units[unitKey{kind, outcome}]++
	m.unitNanos[kind] += nanos
}

func (m *maintainMetrics) recordFollow(repo, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.followRounds[repo+" "+outcome]++
}

func (m *maintainMetrics) snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	units := make(map[string]map[Outcome]int64, len(m.units))
	for k, n := range m.units {
		if units[k.kind] == nil {
			units[k.kind] = map[Outcome]int64{}
		}
		units[k.kind][k.outcome] += n
	}
	unitNanos := make(map[string]int64, len(m.unitNanos))
	for k, v := range m.unitNanos {
		unitNanos[k] = v
	}
	rounds := make(map[string]int64, len(m.followRounds))
	for k, v := range m.followRounds {
		rounds[k] = v
	}
	return MetricsSnapshot{
		Passes:                m.passes.Load(),
		LastPassNanos:         m.lastPassNanos.Load(),
		HeartbeatWrites:       m.heartbeatWrites.Load(),
		Units:                 units,
		UnitNanos:             unitNanos,
		CheckpointLagEntries:  m.checkpointLagEntries.Load(),
		CheckpointAgeSeconds:  m.checkpointAgeSecs.Load(),
		MissingObjects:        m.missingObjects.Load(),
		RepairObjects:         m.repairObjects.Load(),
		FollowRoundsByOutcome: rounds,
		FollowRefs:            m.followRefs.Load(),
	}
}
