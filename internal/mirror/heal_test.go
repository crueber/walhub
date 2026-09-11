// heal_test.go — issue #320 mirror probe + self-heal: the post-publish
// servability probe gating last_result, the patient probe loop, heal
// backoff, heal task outcomes, and the loop's heal trigger.
// Table-driven where pure; real git + real registry elsewhere;
// `-race` mandatory.
package mirror

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// hangStore hangs pack-file GETs (the /wal/ objects) once armed — the
// outage shape for the probe-loop test. Always selects on the caller's
// ctx, so nothing outlives the test binary.
type hangStore struct {
	store.ObjectStore
	mu   sync.Mutex
	hung bool
	gate chan struct{}
}

func newHangStore(inner store.ObjectStore) *hangStore {
	return &hangStore{ObjectStore: inner, gate: make(chan struct{})}
}

func (s *hangStore) hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hung = true
}

func (s *hangStore) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hung {
		s.hung = false
		close(s.gate)
	}
}

func (s *hangStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.mu.Lock()
	hung, gate := s.hung, s.gate
	s.mu.Unlock()
	if hung && strings.Contains(key, "/wal/") {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate:
		}
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func testServiceWithStore(t *testing.T, st store.ObjectStore, reg *wal.Registry) *Service {
	t.Helper()
	registerOnce.Do(func() {
		RegisterKind(KindMirrorSyncOnce)
		RegisterKind(KindMirrorHeal)
	})
	return New(Deps{
		Store: st, Reg: reg,
		CacheDir: t.TempDir(), Hostname: "test-host",
		AllowFile: true, MaxBytes: 1 << 30,
	})
}

func TestProbeOid(t *testing.T) {
	head := "refs/heads/main"
	for _, tc := range []struct {
		name string
		kept []repoimport.Ref
		head string
		want string
	}{
		{name: "head tip wins", kept: []repoimport.Ref{{Name: "refs/heads/side", Oid: "s"}, {Name: head, Oid: "h"}}, head: head, want: "h"},
		{name: "missing head falls back to first", kept: []repoimport.Ref{{Name: "refs/heads/side", Oid: "s"}}, head: head, want: "s"},
		{name: "empty kept probes nothing", kept: nil, head: head, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeOid(tc.kept, tc.head); got != tc.want {
				t.Fatalf("probeOid = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeRetry(t *testing.T) {
	timeout := &wal.WalError{Kind: wal.WalErrTimeout, Detail: "x"}
	corrupt := &wal.WalError{Kind: wal.WalErrCorrupt, Detail: "x"}
	now := time.Now()
	for _, tc := range []struct {
		name     string
		err      error
		deadline time.Time
		want     bool
	}{
		{name: "timeout within budget rejoins", err: timeout, deadline: now.Add(time.Minute), want: true},
		{name: "timeout past budget stops", err: timeout, deadline: now.Add(-time.Second), want: false},
		{name: "corrupt is a verdict", err: corrupt, deadline: now.Add(time.Minute), want: false},
		{name: "cancel is a verdict", err: context.Canceled, deadline: now.Add(time.Minute), want: false},
		{name: "plain error is a verdict", err: errors.New("boom"), deadline: now.Add(time.Minute), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeRetry(tc.err, now, tc.deadline); got != tc.want {
				t.Fatalf("probeRetry = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProbeServeRealShapes(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "p", up)
	if _, err := svc.SyncNow(ctx, "acme", "p", "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	h, err := reg.Open(ctx, "acme/p")
	if err != nil {
		t.Fatal(err)
	}
	tip := servingTip(t, ctx, svc, "acme", "p", "refs/heads/main")
	// Empty oid passes on the Sync alone (nothing published provable).
	if err := svc.probeServe(ctx, h, ""); err != nil {
		t.Fatalf("empty-oid probe: %v", err)
	}
	// The head commit proves readable (the issue's one-blob-at-head).
	if err := svc.probeServe(ctx, h, tip); err != nil {
		t.Fatalf("head probe: %v", err)
	}
	// A missing object is a terminal verdict, not a hang.
	if err := svc.probeServe(ctx, h, strings.Repeat("0", 40)); err == nil {
		t.Fatal("missing-object probe passed")
	} else if !strings.Contains(err.Error(), "cat-file") {
		t.Fatalf("missing-object probe err = %v, want cat-file verdict", err)
	}
}

func TestProbeServeLoopHungStore(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	hs := newHangStore(inner)
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	// Tiny engine wait (per-attempt bound), modest probe budget: the
	// loop re-joins a few times, then fails bounded.
	cfg.Server.ServeSyncTimeout = config.Duration(150 * time.Millisecond)
	reg := wal.NewRegistry(ctx, hs, cfg)
	t.Cleanup(reg.Close)
	svc := testServiceWithStore(t, hs, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, hs, "acme", "loop", up)
	if _, err := svc.SyncNow(ctx, "acme", "loop", "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	h, err := reg.Open(ctx, "acme/loop")
	if err != nil {
		t.Fatal(err)
	}
	tip := servingTip(t, ctx, svc, "acme", "loop", "refs/heads/main")

	// Cold one pack: drop the local .pack so the probe must download it.
	m, _ := h.ManifestSnapshot()
	if len(m.Packs) == 0 {
		t.Fatal("no packs published by sync")
	}
	cs := m.Packs[0].Checksum
	if err := os.Remove(h.Repo().PackDir() + "/pack-" + cs + ".pack"); err != nil {
		t.Fatal(err)
	}

	// Outage: attempts time out and re-join until the probe budget, then
	// fail bounded (never the issue's indefinite hang).
	svc.probeTimeout = 600 * time.Millisecond
	hs.hang()
	start := time.Now()
	err = svc.probeServe(ctx, h, tip)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("probe passed against a hung store")
	}
	var we *wal.WalError
	if !errors.As(err, &we) || we.Kind != wal.WalErrTimeout {
		t.Fatalf("probe err = %v (%T), want WalErrTimeout", err, err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("probe took %v with a 600ms budget", elapsed)
	}
	// Recovery: the store answers, the probe passes on a fresh body.
	hs.resume()
	svc.probeTimeout = 30 * time.Second
	if err := svc.probeServe(ctx, h, tip); err != nil {
		t.Fatalf("probe after resume: %v", err)
	}
}

func TestSyncProbeGatesLastResult(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "g", up)

	// A stale marker does not survive a passing probe: the sync proves
	// servability and clears it, reporting ok.
	seedMarker(t, st, "acme", "g", "stale: earlier outage")
	rec, err := svc.SyncNow(ctx, "acme", "g", "", false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v, want ok", rec)
	}
	doc, _, derr := Load(ctx, st, "acme", "g")
	if derr != nil || doc.LastResult != "ok" || doc.ConsecutiveFailures != 0 {
		t.Fatalf("doc = %+v err=%v, want clean ok", doc, derr)
	}
	if _, ok := wal.LoadServeHealth(ctx, st, "acme", "g"); ok {
		t.Fatal("stale marker survived a passing probe")
	}

	// A failing probe is not ok: refs stay published but the outcome is
	// degraded, the failure counter moves, and the marker names it.
	commitFile(t, up, "g2.txt", "b\n", "b")
	svc.probe = func(ctx context.Context, h *wal.RepoHandle, oid string) error {
		return errors.New("cat-file -e: exit status 1: missing")
	}
	rec, err = svc.SyncNow(ctx, "acme", "g", "", false)
	if err == nil {
		t.Fatal("sync with failing probe succeeded")
	}
	doc, _, derr = Load(ctx, st, "acme", "g")
	if derr != nil {
		t.Fatal(derr)
	}
	if !strings.HasPrefix(doc.LastResult, "failed: servability probe:") {
		t.Fatalf("last_result = %q, want the probe verdict", doc.LastResult)
	}
	if doc.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d, want 1", doc.ConsecutiveFailures)
	}
	marker, ok := wal.LoadServeHealth(ctx, st, "acme", "g")
	if !ok || marker == nil || !strings.Contains(marker.Reason, "servability probe") {
		t.Fatalf("marker = %+v %v, want the probe reason", marker, ok)
	}
	// …yet the refs did publish (a degraded sync is still a sync).
	if got := servingTip(t, ctx, svc, "acme", "g", "refs/heads/main"); got == "" {
		t.Fatal("refs missing after probe-failed sync")
	}

	// A no-op fire never probes: with the probe still stubbed failing,
	// nothing moved, nothing to prove — still ok.
	svc.probe = func(ctx context.Context, h *wal.RepoHandle, oid string) error {
		t.Error("probe ran on a no-op sync")
		return errors.New("must not probe")
	}
	rec, err = svc.SyncNow(ctx, "acme", "g", "", false)
	if err != nil {
		t.Fatalf("no-op sync: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("no-op rec = %+v, want ok", rec)
	}
}

func TestHealDue(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		doc  *wal.ServeHealth
		want bool
	}{
		{name: "nil never fires", doc: nil, want: false},
		{name: "never healed fires now", doc: &wal.ServeHealth{Reason: "x"}, want: true},
		{name: "recent heal defers", doc: &wal.ServeHealth{Reason: "x", Attempts: 1, LastHealAt: now.Add(-time.Minute).Format(time.RFC3339)}, want: false},
		{name: "elapsed backoff refires", doc: &wal.ServeHealth{Reason: "x", Attempts: 1, LastHealAt: now.Add(-16 * time.Minute).Format(time.RFC3339)}, want: true},
		{name: "unparseable stamp fires", doc: &wal.ServeHealth{Reason: "x", Attempts: 9, LastHealAt: "nonsense"}, want: true},
		{name: "high attempts back off a day", doc: &wal.ServeHealth{Reason: "x", Attempts: 8, LastHealAt: now.Add(-time.Hour).Format(time.RFC3339)}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := healDue(tc.doc, now); got != tc.want {
				t.Fatalf("healDue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunHeal(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testServiceWithStore(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "heal", up)
	if _, err := svc.SyncNow(ctx, "acme", "heal", "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	runHealTask := func() (*wal.TaskRecord, error) {
		return reg.Tasks().Run(ctx, "acme/heal", KindMirrorHeal, nil,
			func(tctx context.Context, task *wal.Task) error {
				return svc.runHeal(tctx, task, "acme", "heal")
			})
	}

	// A passing heal clears a seeded marker and flips back to healthy
	// with no mirror-doc touch.
	seedMarker(t, st, "acme", "heal", "stale: earlier outage")
	before, _, _ := Load(ctx, st, "acme", "heal")
	rec, err := runHealTask()
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("heal rec = %+v, want ok", rec)
	}
	if _, ok := wal.LoadServeHealth(ctx, st, "acme", "heal"); ok {
		t.Fatal("marker survived a passing heal")
	}
	after, _, _ := Load(ctx, st, "acme", "heal")
	if after.LastResult != before.LastResult || after.ConsecutiveFailures != before.ConsecutiveFailures {
		t.Fatalf("heal touched the mirror doc: %+v -> %+v", before, after)
	}

	// A failing heal refreshes the marker (attempts+1, anchored now)
	// and fails the task — the backoff, not silence.
	svc.probe = func(ctx context.Context, h *wal.RepoHandle, oid string) error {
		return errors.New("cat-file -e: exit status 1: missing")
	}
	if _, err := runHealTask(); err == nil {
		t.Fatal("failing heal succeeded")
	}
	marker, ok := wal.LoadServeHealth(ctx, st, "acme", "heal")
	if !ok || marker.Attempts != 1 || marker.LastHealAt == "" {
		t.Fatalf("marker = %+v %v, want attempts=1 anchored", marker, ok)
	}
	if !strings.Contains(marker.Reason, "heal probe") {
		t.Fatalf("reason = %q, want the heal verdict", marker.Reason)
	}
	// …and the mirror doc still untouched.
	after, _, _ = Load(ctx, st, "acme", "heal")
	if after.LastResult != before.LastResult || after.ConsecutiveFailures != before.ConsecutiveFailures {
		t.Fatalf("failed heal touched the mirror doc: %+v", after)
	}
}

func TestRoundHealsDegraded(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testServiceWithStore(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "round", up)
	if _, err := svc.SyncNow(ctx, "acme", "round", "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Just synced on a daily preset: not due — the round may only heal.
	seedMarker(t, st, "acme", "round", "serve sync wait timed out")
	svc.Round(ctx, nil)
	// The heal fires async: poll for the flip-back (bounded wait).
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, ok := wal.LoadServeHealth(ctx, st, "acme", "round"); !ok {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("heal did not clear the marker in 15s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Exactly one heal task, terminal ok.
	var heals []*wal.TaskRecord
	for _, rec := range reg.Tasks().List("acme/round") {
		if rec != nil && rec.Kind == KindMirrorHeal {
			heals = append(heals, rec)
		}
	}
	if len(heals) != 1 || heals[0].OK == nil || !*heals[0].OK {
		t.Fatalf("heal records = %+v, want one ok", heals)
	}

	// Unmarked repos fire no heal (and the placement gate is honored).
	svc.Round(ctx, nil)
	time.Sleep(500 * time.Millisecond)
	for _, rec := range reg.Tasks().List("acme/round") {
		if rec != nil && rec.Kind == KindMirrorHeal && rec.Started > heals[0].Started {
			t.Fatalf("heal fired without a marker: %+v", rec)
		}
	}
	seedMarker(t, st, "acme", "round", "timed out again")
	svc.Round(ctx, func(string) bool { return false })
	time.Sleep(500 * time.Millisecond)
	n := 0
	for _, rec := range reg.Tasks().List("acme/round") {
		if rec != nil && rec.Kind == KindMirrorHeal {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("placement-gated round fired a heal: %d records", n)
	}
}

func TestRunnerProbeObject(t *testing.T) {
	ctx := context.Background()
	up := initUpstream(t)
	// Probe runs against bare repos in production (the serving copy):
	// mirror-clone the fixture first (the collect helper sets
	// GIT_DIR=<dir>, which is only valid for a bare repo).
	bare := t.TempDir() + "/bare"
	gitOut(t, t.TempDir(), "clone", "--mirror", "--", up, bare)
	r := NewRunner("", t.TempDir(), 0, 0)
	// The timeouts resolve to the production defaults (zero → default).
	if r.GitTimeout <= 0 {
		t.Fatalf("git timeout = %v, want positive default", r.GitTimeout)
	}
	oid := strings.TrimSpace(gitOut(t, up, "rev-parse", "HEAD"))
	if err := r.ProbeObject(ctx, bare, oid); err != nil {
		t.Fatalf("probe head: %v", err)
	}
	if err := r.ProbeObject(ctx, bare, strings.Repeat("0", 40)); err == nil {
		t.Fatal("probe missing object passed")
	}
}

// --- small helpers ------------------------------------------------------------

func seedMarker(t *testing.T, st store.ObjectStore, owner, name, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prev, _ := wal.LoadServeHealth(ctx, st, owner, name)
	if err := wal.WriteServeHealth(ctx, st, owner, name, reason, prev); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
}

func TestRunHealOpenFailure(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testServiceWithStore(t, st, reg)

	// Healing a repo that cannot even open still records the attempt
	// (attempts+1, anchored) and fails the task — never a silent skip,
	// or the loop would hammer it every round.
	rec, err := reg.Tasks().Run(ctx, "acme/nope", KindMirrorHeal, nil,
		func(tctx context.Context, task *wal.Task) error {
			return svc.runHeal(tctx, task, "acme", "nope")
		})
	if err == nil {
		t.Fatal("heal of an unopenable repo succeeded")
	}
	if rec == nil || rec.OK == nil || *rec.OK {
		t.Fatalf("heal rec = %+v, want terminal failure", rec)
	}
	marker, ok := wal.LoadServeHealth(ctx, st, "acme", "nope")
	if !ok || marker.Attempts != 1 || marker.LastHealAt == "" {
		t.Fatalf("marker = %+v %v, want attempts=1 anchored", marker, ok)
	}
}

func TestServingHeadOidError(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "oid", up)
	if _, err := svc.SyncNow(ctx, "acme", "oid", "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	h, err := reg.Open(ctx, "acme/oid")
	if err != nil {
		t.Fatal(err)
	}
	if got := servingHeadOid(ctx, svc, h); got == "" {
		t.Fatal("no head oid on a synced mirror")
	}
	// A torn-down serving copy fails the enumeration: the heal's object
	// check degrades to Sync-only (""), never an error.
	if err := os.RemoveAll(h.Dir()); err != nil {
		t.Fatal(err)
	}
	if got := servingHeadOid(ctx, svc, h); got != "" {
		t.Fatalf("head oid on a removed serving copy = %q, want empty", got)
	}
}

func TestServeDegraded(t *testing.T) {
	ctx := context.Background()
	_, st := testRegistry(t)
	if _, ok := ServeDegraded(ctx, st, "acme", "m"); ok {
		t.Fatal("unmarked repo reported degraded")
	}
	seedMarker(t, st, "acme", "m", "serve sync wait timed out")
	reason, ok := ServeDegraded(ctx, st, "acme", "m")
	if !ok || reason != "serve sync wait timed out" {
		t.Fatalf("verdict = %q %v, want the marker reason", reason, ok)
	}
}
