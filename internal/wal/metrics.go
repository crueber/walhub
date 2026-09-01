// metrics.go — the engine's in-process observability counters (doc 05 §5.3/§5.9).
// The Prometheus text exposition lives in the server package (doc 08); these are
// the atomic counters it renders. Nothing here blocks or allocates on hot paths
// beyond one atomic add.
package wal

import (
	"log"
	"sync/atomic"
)

var logWarnf = func(format string, args ...any) { log.Printf("WARN "+format, args...) }

// SetWarnLogger redirects engine warnings (the server wires its slog in).
func SetWarnLogger(f func(format string, args ...any)) {
	if f != nil {
		logWarnf = f
	}
}

var (
	publishLocalApplyFailed atomic.Int64 // walgit_publish_local_apply_failed_total
	publishCASRetries       atomic.Int64 // walgit_publish_cas_retries_total (412s are normal)
	orphansBurned           atomic.Int64 // walgit_orphans_burned_total
	orphansSwept            atomic.Int64 // walgit_orphans_swept_total
)

// MetricsSnapshot is a point-in-time copy of the engine counters.
type MetricsSnapshot struct {
	PublishLocalApplyFailed int64
	PublishCASRetries       int64
	OrphansBurned           int64
	OrphansSwept            int64
}

// Metrics returns the current counter values (test/ops helper).
func Metrics() MetricsSnapshot {
	return MetricsSnapshot{
		PublishLocalApplyFailed: publishLocalApplyFailed.Load(),
		PublishCASRetries:       publishCASRetries.Load(),
		OrphansBurned:           orphansBurned.Load(),
		OrphansSwept:            orphansSwept.Load(),
	}
}
