package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is the hand-rolled Prometheus text exposition (§10.3): named
// collectors + a writer that emits # HELP, # TYPE, and samples in
// lexicographic family order; labels escaped per the text format. Counter
// updates are sync/atomic; the family mutex is taken only on scrape.
type Registry struct {
	mu   sync.Mutex
	fams map[string]*family
}

type family struct {
	name   string
	help   string
	typ    string // counter | gauge | histogram
	val    atomic.Int64
	series sync.Map // labels key (rendered) → *atomic.Int64
	extra  []string // histogram extra labels
	hist   struct {
		mu      sync.Mutex
		bounds  []float64
		buckets []int64
		sum     float64
		count   int64
		extra   string
	}
}

// Counter is a monotonically increasing family.
type Counter struct{ f *family }

// Gauge is a settable family.
type Gauge struct{ f *family }

// Histogram is a bucketed family.
type Histogram struct{ f *family }

func newRegistry() *Registry { return &Registry{fams: map[string]*family{}} }

func (r *Registry) fam(name, help, typ string) *family {
	r.fams[name] = &family{name: name, help: help, typ: typ}
	return r.fams[name]
}

// Counter registers a counter family (§10.3 inventory registered at startup).
func (r *Registry) Counter(name, help string) Counter {
	if f, ok := r.fams[name]; ok {
		return Counter{f}
	}
	return Counter{r.fam(name, help, "counter")}
}

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string) Gauge {
	if f, ok := r.fams[name]; ok {
		return Gauge{f}
	}
	return Gauge{r.fam(name, help, "gauge")}
}

// Histogram registers a histogram family.
func (r *Registry) Histogram(name, help string, bounds []float64) Histogram {
	if f, ok := r.fams[name]; ok {
		return Histogram{f}
	}
	f := r.fam(name, help, "histogram")
	f.hist.bounds = bounds
	f.hist.buckets = make([]int64, len(bounds))
	return Histogram{f}
}

// Inc adds one.
func (c Counter) Inc(labels ...string) { c.f.add(1, labels) }

// Add adds n.
func (c Counter) Add(n int64, labels ...string) { c.f.add(n, labels) }

// Set sets the value.
func (g Gauge) Set(n int64, labels ...string) { g.f.set(n, labels) }

// Observe records one histogram observation.
func (h Histogram) Observe(v float64, labels ...string) { h.f.observe(v, labels) }

func (f *family) add(n int64, labels []string) {
	if len(labels) == 0 {
		f.val.Add(n)
		return
	}
	f.seriesFor(labels).Add(n)
}

func (f *family) set(n int64, labels []string) {
	if len(labels) == 0 {
		f.val.Store(n)
		return
	}
	f.seriesFor(labels).Store(n)
}

func (f *family) seriesFor(labels []string) *atomic.Int64 {
	key := labelsKey(labels)
	if s, ok := f.series.Load(key); ok {
		return s.(*atomic.Int64)
	}
	v := &atomic.Int64{}
	loaded, _ := f.series.LoadOrStore(key, v)
	return loaded.(*atomic.Int64)
}

func (f *family) observe(v float64, labels []string) {
	f.hist.mu.Lock()
	defer f.hist.mu.Unlock()
	f.hist.sum += v
	f.hist.count++
	if len(labels) > 0 {
		f.hist.extra = labelsKey(labels)
	}
	for i := range f.hist.bounds {
		if v <= f.hist.bounds[i] {
			f.hist.buckets[i]++
			return
		}
	}
}

var defBounds = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

func labelsKey(labels []string) string {
	var b strings.Builder
	for i := 0; i+1 < len(labels); i += 2 {
		b.WriteString(labels[i])
		b.WriteByte('=')
		b.WriteString(labels[i+1])
		b.WriteByte(',')
	}
	return b.String()
}

func renderLabels(key string) string {
	parts := strings.Split(strings.TrimSuffix(key, ","), ",")
	var b strings.Builder
	b.WriteByte('{')
	for i := range parts {
		kv := strings.SplitN(parts[i], "=", 2)
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[0])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(kv[1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// Render produces the text format in lexicographic family order.
func (r *Registry) Render() string {
	names := make([]string, 0, len(r.fams))
	for name := range r.fams {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		f := r.fams[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, f.help, name, f.typ)
		if f.typ == "histogram" {
			f.writeHistogram(&b)
			continue
		}
		fmt.Fprintf(&b, "%s %d\n", f.name, f.val.Load())
		type row struct {
			labels string
			val    int64
		}
		rows := []row{}
		f.series.Range(func(k, v any) bool {
			rows = append(rows, row{labels: renderLabels(k.(string)), val: v.(*atomic.Int64).Load()})
			return true
		})
		sort.Slice(rows, func(i, j int) bool { return rows[i].labels < rows[j].labels })
		for _, rw := range rows {
			fmt.Fprintf(&b, "%s%s %d\n", f.name, rw.labels, rw.val)
		}
	}
	return b.String()
}

func (f *family) writeHistogram(b *strings.Builder) {
	f.hist.mu.Lock()
	defer f.hist.mu.Unlock()
	extra := ""
	if f.hist.extra != "" {
		extra = renderLabels(f.hist.extra)
		extra = extra[:len(extra)-1] + "," // "{a=\"b\"," → spliceable
	}
	cum := int64(0)
	for i := range f.hist.bounds {
		cum += f.hist.buckets[i]
		fmt.Fprintf(b, "%s_bucket%s{le=\"%s\"} %d\n", f.name, extra,
			strconv.FormatFloat(f.hist.bounds[i], 'g', -1, 64), cum)
	}
	fmt.Fprintf(b, "%s_bucket%s{le=\"+Inf\"} %d\n", f.name, extra, f.hist.count)
	fmt.Fprintf(b, "%s_sum%s %s\n", f.name, extra, strconv.FormatFloat(f.hist.sum, 'g', -1, 64))
	fmt.Fprintf(b, "%s_count%s %d\n", f.name, extra, f.hist.count)
}

// metricsHandler serves GET /metrics (`text/plain; version=0.0.4`).
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	body := s.metrics.Render()
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// registerInventory registers every §10.3 series at startup.
func registerInventory(r *Registry) {
	r.Gauge("walgit_http_inflight", "in-flight HTTP requests")
	r.Gauge("walgit_tasks_running", "tasks currently running")
	r.Counter("walgit_tasks_started_total", "tasks started")
	r.Counter("walgit_tasks_finished_total", "tasks finished")
	r.Counter("walgit_task_duration_seconds", "task duration")
	r.Gauge("walgit_lock_wait_seconds", "lock wait time")
	r.Gauge("walgit_store_bulk_queue_seconds", "bulk store queue wait")
	r.Gauge("walgit_store_bulk_inflight", "bulk store in-flight ops")
	r.Counter("walgit_remote_block_cache_hits_total", "remote block cache hits")
	r.Counter("walgit_remote_block_cache_misses_total", "remote block cache misses")
	r.Counter("walgit_remote_range_reads_total", "remote range reads")
	r.Counter("walgit_remote_bytes_total", "remote bytes read")
	r.Gauge("walgit_remote_delta_chain", "remote delta chain length")
	r.Counter("walgit_remote_faulted_objects_total", "remote faulted objects")
	r.Counter("walgit_sync_too_large_total", "sync-too-large refusals")
	r.Counter("walgit_publish_local_apply_failed_total", "publish local-apply failures")
	r.Histogram("walgit_checkpoint_seconds", "checkpoint duration", defBounds)
	r.Counter("walgit_checkpoints_total", "checkpoints")
	r.Counter("walgit_push_refused_total", "pushes refused")
	r.Counter("walgit_not_served_here_total", "requests refused by placement")
	r.Gauge("walgit_checkpoint_lag_entries", "WAL checkpoint lag (entries)")
	r.Gauge("walgit_checkpoint_age_seconds", "WAL checkpoint age")
	r.Counter("walgit_repo_miss_total", "unknown-repo requests")
}
