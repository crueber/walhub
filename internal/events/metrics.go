// metrics.go — the bridge's narrow metrics seam (09 §8). Prometheus exposition
// is the shared text renderer of internal/server; the bridge only
// registers/increments through this interface, so the composition pass can wire
// the server registry without a package cycle.
package events

// Metrics records the bridge's four series:
//
//	events_published_total{sink}       counter — events delivered per sink
//	events_bridge_lag_entries{repo}    gauge   — head_seq − cursor at each catch-up
//	events_bridge_gap_total{repo}      counter — cursor below readable_from
//	events_bridge_sweep_found_total    counter — events published by a sweep
type Metrics interface {
	// Inc adds one to counter name with the given ordered label values.
	Inc(name string, labelValues ...string)
	// Add adds n to counter name.
	Add(name string, n int64, labelValues ...string)
	// Set sets gauge name to value.
	Set(name string, value int64, labelValues ...string)
}

// NoopMetrics discards everything (default when the composition pass wires
// nothing).
type NoopMetrics struct{}

func (NoopMetrics) Inc(string, ...string)        {}
func (NoopMetrics) Add(string, int64, ...string) {}
func (NoopMetrics) Set(string, int64, ...string) {}

// Metric series names (09 §8, normative).
const (
	MetricPublished  = "events_published_total"
	MetricLag        = "events_bridge_lag_entries"
	MetricGap        = "events_bridge_gap_total"
	MetricSweepFound = "events_bridge_sweep_found_total"
)
