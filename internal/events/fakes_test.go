// fakes_test.go — deterministic fakes for the events bridge tests: a fake WAL
// source, a recording metrics seam, and a fake sink that can fail N times.
package events

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// fakeRepo is a RepoView over an in-memory entry list.
type fakeRepo struct {
	mu      sync.Mutex
	head    uint64
	minSeq  uint64
	sha256  bool
	entries []*proto.LogEntry
	syncErr error
	logErr  error
}

func (r *fakeRepo) SyncRefs(ctx context.Context) (RepoState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.syncErr != nil {
		return RepoState{}, r.syncErr
	}
	return RepoState{HeadSeq: r.head, MinSeq: r.minSeq, Sha256: r.sha256}, nil
}

func (r *fakeRepo) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logErr != nil {
		return nil, r.logErr
	}
	var out []*proto.LogEntry
	for _, e := range r.entries {
		if e.Seq >= from && e.Seq <= to {
			out = append(out, e)
		}
	}
	return out, nil
}

// fakeSource is a WalSource over named repos, with optional RepoView overrides
// for concurrency tests.
type fakeSource struct {
	mu    sync.Mutex
	repos map[string]*fakeRepo
	views map[string]RepoView
	order []string
}

func newFakeSource(repos ...*fakeRepo) *fakeSource {
	s := &fakeSource{repos: map[string]*fakeRepo{}, views: map[string]RepoView{}}
	for i, r := range repos {
		id := "owner/r" + strconv.Itoa(i)
		s.repos[id] = r
		s.order = append(s.order, id)
	}
	return s
}

func (s *fakeSource) setView(repo string, v RepoView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.views[repo] = v
}

func (s *fakeSource) Repos(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.order...)
	return out, nil
}

func (s *fakeSource) Handle(ctx context.Context, repo string) (RepoView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.views[repo]; ok {
		return v, nil
	}
	r, ok := s.repos[repo]
	if !ok {
		return nil, fmt.Errorf("no such repo %q", repo)
	}
	return r, nil
}

// recMetrics records counters and gauges keyed "name|label|label".
type recMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64
}

func newRecMetrics() *recMetrics {
	return &recMetrics{counts: map[string]int64{}, gauges: map[string]int64{}}
}

func metKey(name string, labels []string) string {
	return name + "|" + strings.Join(labels, "|")
}

func (m *recMetrics) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }

func (m *recMetrics) Add(name string, n int64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[metKey(name, labels)] += n
}

func (m *recMetrics) Set(name string, v int64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[metKey(name, labels)] = v
}

func (m *recMetrics) count(name string, labels ...string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[metKey(name, labels)]
}

func (m *recMetrics) gauge(name string, labels ...string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gauges[metKey(name, labels)]
}

// fakeSink delivers to memory; failsLeft simulates a webhook that fails N
// times before acking.
type fakeSink struct {
	mu        sync.Mutex
	name      string
	failsLeft int
	batches   [][]RefEvent
	repos     []string
}

func newFakeSink(name string) *fakeSink { return &fakeSink{name: name} }

func (s *fakeSink) Name() string { return s.name }

func (s *fakeSink) Deliver(ctx context.Context, repo string, batch []RefEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failsLeft > 0 {
		s.failsLeft--
		return fmt.Errorf("fake sink %s failing (%d left)", s.name, s.failsLeft+1)
	}
	cp := make([]RefEvent, len(batch))
	copy(cp, batch)
	s.batches = append(s.batches, cp)
	s.repos = append(s.repos, repo)
	return nil
}

func (s *fakeSink) deliveries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *fakeSink) lastBatch() []RefEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return nil
	}
	return s.batches[len(s.batches)-1]
}

// ---- entry builders -------------------------------------------------------------

var testZero40 = git.Sha1.ZeroHex()

func mkEntry(seq uint64, kind proto.EntryKind, meta map[string]string, updates ...*proto.RefUpdate) *proto.LogEntry {
	return &proto.LogEntry{
		Seq:  seq,
		Kind: kind,
		Txn:  &proto.RefTransaction{Updates: updates},
		Meta: meta,
	}
}

func upd(name, old, new string) *proto.RefUpdate {
	return &proto.RefUpdate{Name: name, OldOid: old, NewOid: new}
}
