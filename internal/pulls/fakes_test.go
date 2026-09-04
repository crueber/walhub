package pulls

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- FakeRoles (P6) ------------------------------------------------------------

// FakeRoles is a scripted RoleService: roles by principal name, Public
// toggles anonymous reads.
type FakeRoles struct {
	Roles  map[string]string
	Public bool
}

func (f *FakeRoles) roleOf(name string) string {
	if f.Roles == nil {
		return ""
	}
	return f.Roles[strings.ToLower(name)]
}

func (f *FakeRoles) Resolve(_ context.Context, _, _ string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	if p.Admin {
		return identity.RoleAdmin, nil
	}
	if r := f.roleOf(p.Name); r != "" {
		return identity.Role(r), nil
	}
	if p.Anonymous {
		return "", nil
	}
	return identity.RoleRead, nil
}

func (f *FakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Admin || p.Write {
		return nil
	}
	if p.Anonymous {
		if f.Public {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if r := f.roleOf(p.Name); r == "" && !f.Public {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private repository"}
	}
	return nil
}

// --- FakeGit -------------------------------------------------------------------

// FakeGit is a scripted GitRunner: refs keyed dir+"\x00"+ref, ancestry by
// pair, trial/commit/replay knobs, reachability map (default true), and a
// concurrency gauge (inflight/max) proving bounded pool usage on the real
// path shape.
type FakeGit struct {
	Refs       map[string]string
	ResolveErr map[string]error
	// OnResolve fires on every ResolveRef (tests only: deterministic
	// mid-task races — the hook can move/delete refs between the task's
	// plan-time resolve and its replan resolve).
	OnResolve    func(dir, ref string)
	MergeBaseSHA string
	MergeBaseErr error
	// MergeBaseQueue scripts successive MergeBase outcomes (tests only:
	// covers paths where the stamp computation and the task disagree).
	MergeBaseQueue []MergeBaseOutcome
	Ancestors      map[string]bool
	// IsAncestorErr fails IsAncestor (tests only: ancestry outage).
	IsAncestorErr  error
	TrialTree      string
	TrialConflicts []string
	TrialErr       error
	// TrialQueue scripts successive TrialMerge outcomes (tests only: the
	// merge task re-verifies under itself, so the stamp trial and the
	// task trial can disagree — exactly the race §5 step 1 exists for).
	TrialQueue   []TrialOutcome
	CommitErr    error
	ReplaySHA    string
	ReplayErr    error
	Behind       int
	BehindErr    error
	ReachableMap map[string]bool
	ReachableErr error
	DiffText     string
	DiffErr      error
	LogRows      []CommitEntry
	LogErr       error
	Subjects     map[string]string

	// Barrier pauses the first TrialMerge until BarrierHold closes
	// (tests only: proves single-flight collapse under true concurrency).
	BarrierOnce sync.Once
	BarrierSeen chan struct{}
	BarrierHold chan struct{}

	commitSeq int64
	inflight  int64
	maxFlight int64

	mu    sync.Mutex
	Calls []string
}

func (f *FakeGit) key(dir, ref string) string { return dir + "\x00" + ref }

func (f *FakeGit) enter(op string) func() {
	n := atomic.AddInt64(&f.inflight, 1)
	for {
		m := atomic.LoadInt64(&f.maxFlight)
		if n <= m || atomic.CompareAndSwapInt64(&f.maxFlight, m, n) {
			break
		}
	}
	f.mu.Lock()
	f.Calls = append(f.Calls, op)
	f.mu.Unlock()
	return func() { atomic.AddInt64(&f.inflight, -1) }
}

// MaxFlight reports the peak concurrent git calls observed.
func (f *FakeGit) MaxFlight() int64 { return atomic.LoadInt64(&f.maxFlight) }

// CallLog returns a copy of the call log (race-safe for readers racing a
// background recompute pass).
func (f *FakeGit) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

func (f *FakeGit) ResolveRef(_ context.Context, dir, ref string) (string, error) {
	defer f.enter("resolve:" + ref)()
	// Full-body lock: test-side mutations (seedRefs/delGitRef, the
	// OnResolve hook below) never race a background recompute/merge pass.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.OnResolve != nil {
		f.OnResolve(dir, ref)
	}
	if err, ok := f.ResolveErr[f.key(dir, ref)]; ok {
		return "", err
	}
	if sha, ok := f.Refs[f.key(dir, ref)]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
}

func (f *FakeGit) MergeBase(_ context.Context, dir, a, b string) (string, error) {
	defer f.enter("merge-base")()
	_ = dir
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.MergeBaseQueue) > 0 {
		o := f.MergeBaseQueue[0]
		f.MergeBaseQueue = f.MergeBaseQueue[1:]
		return o.SHA, o.Err
	}
	if f.MergeBaseErr != nil {
		return "", f.MergeBaseErr
	}
	if f.MergeBaseSHA != "" {
		return f.MergeBaseSHA, nil
	}
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
}

func (f *FakeGit) IsAncestor(_ context.Context, dir, a, b string) (bool, error) {
	defer f.enter("is-ancestor")()
	_ = dir
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.IsAncestorErr != nil {
		return false, f.IsAncestorErr
	}
	if f.Ancestors == nil {
		return false, nil
	}
	return f.Ancestors[a+"\x00"+b], nil
}

func (f *FakeGit) TrialMerge(_ context.Context, dir, base, head string) (string, []string, error) {
	defer f.enter("merge-tree")()
	_, _, _ = dir, base, head
	// The barrier waits on channels (no fake state) and must NOT hold the
	// mutex: joiners pile on the lock while the leader waits for release.
	f.BarrierOnce.Do(func() {
		if f.BarrierHold != nil {
			close(f.BarrierSeen)
			<-f.BarrierHold
		}
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.TrialQueue) > 0 {
		o := f.TrialQueue[0]
		f.TrialQueue = f.TrialQueue[1:]
		return o.Tree, o.Conflicts, o.Err
	}
	if f.TrialErr != nil {
		return "", f.TrialConflicts, f.TrialErr
	}
	if f.TrialTree != "" {
		return f.TrialTree, []string{}, nil
	}
	return "44444444444444444444444444444444444444444444", []string{}, nil
}

func (f *FakeGit) CommitTree(_ context.Context, dir, tree string, parents []string, msg, _, _, _, _ string, _ time.Time) (string, error) {
	defer f.enter("commit-tree")()
	_, _, _, _ = dir, tree, parents, msg
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CommitErr != nil {
		return "", f.CommitErr
	}
	n := atomic.AddInt64(&f.commitSeq, 1)
	return fmt.Sprintf("c%039d", n), nil
}

func (f *FakeGit) Replay(_ context.Context, dir, onto, base, head, committerName, committerEmail string) (string, error) {
	defer f.enter("replay")()
	_, _, _, _, _, _ = dir, onto, base, head, committerName, committerEmail
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReplayErr != nil {
		return "", f.ReplayErr
	}
	if f.ReplaySHA != "" {
		return f.ReplaySHA, nil
	}
	return "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", nil
}

func (f *FakeGit) BehindCount(_ context.Context, dir, base, head string) (int, error) {
	defer f.enter("rev-list-count")()
	_, _, _ = dir, base, head
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Behind, f.BehindErr
}

func (f *FakeGit) Reachable(_ context.Context, dir, sha string) (bool, error) {
	defer f.enter("reachable")()
	_ = dir
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReachableErr != nil {
		return false, f.ReachableErr
	}
	if f.ReachableMap == nil {
		return true, nil
	}
	ok, known := f.ReachableMap[sha]
	if !known {
		return true, nil
	}
	return ok, nil
}

func (f *FakeGit) Diff(_ context.Context, dir, base, head string) (string, error) {
	defer f.enter("diff")()
	_, _, _ = dir, base, head
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DiffErr != nil {
		return "", f.DiffErr
	}
	if f.DiffText != "" {
		return f.DiffText, nil
	}
	return "diff --git a/f b/f\n", nil
}

func (f *FakeGit) LogRange(_ context.Context, dir, base, head string, skip, n int) ([]CommitEntry, error) {
	defer f.enter("log")()
	_, _, _, _, _ = dir, base, head, skip, n
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LogErr != nil {
		return nil, f.LogErr
	}
	rows := append([]CommitEntry(nil), f.LogRows...)
	if skip >= len(rows) {
		return []CommitEntry{}, nil
	}
	rows = rows[skip:]
	if len(rows) > n {
		rows = rows[:n]
	}
	if rows == nil {
		rows = []CommitEntry{}
	}
	return rows, nil
}

func (f *FakeGit) Subject(_ context.Context, dir, sha string) (string, error) {
	defer f.enter("subject")()
	_, _ = dir, sha
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.Subjects[sha]; ok {
		return s, nil
	}
	return "head subject", nil
}

// TrialOutcome scripts one TrialMerge call.
type TrialOutcome struct {
	Tree      string
	Conflicts []string
	Err       error
}

// MergeBaseOutcome scripts one MergeBase call.
type MergeBaseOutcome struct {
	SHA string
	Err error
}

// --- FakeDirs ------------------------------------------------------------------

// FakeDirs maps repo → dir ("dir:"+repo); UnknownErr fails Dir.
type FakeDirs struct {
	UnknownErr error
}

func (f *FakeDirs) Dir(_ context.Context, repo string) (string, error) {
	if f.UnknownErr != nil {
		return "", f.UnknownErr
	}
	return "dir:" + repo, nil
}

// --- FakeRefs -------------------------------------------------------------------

// RefCall records one publisher call (old captured to prove never-force).
type RefCall struct {
	Op   string
	Repo string
	Ref  string
	Old  string
	New  string
	Meta map[string]string
}

// FakeRefs is an in-memory RefPublisher: refs per repo, CAS-fail-once knob
// (UpdateConflictOnce), and a full call log.
type FakeRefs struct {
	mu                 sync.Mutex
	Refs               map[string]map[string]string
	UpdateConflictOnce bool
	UpdateErr          error // fails every UpdateRef (tests only: publish outage)
	DeleteErr          error
	CreateErr          error
	Calls              []RefCall
}

func (f *FakeRefs) get(repo, ref string) (string, bool) {
	m := f.Refs[repo]
	if m == nil {
		return "", false
	}
	sha, ok := m[ref]
	return sha, ok
}

func (f *FakeRefs) CreateRef(_ context.Context, repo, ref, sha string, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return f.CreateErr
	}
	f.Calls = append(f.Calls, RefCall{Op: "create", Repo: repo, Ref: ref, New: sha, Meta: meta})
	if f.Refs == nil {
		f.Refs = map[string]map[string]string{}
	}
	if f.Refs[repo] == nil {
		f.Refs[repo] = map[string]string{}
	}
	if cur, ok := f.Refs[repo][ref]; ok && cur == sha {
		return nil // idempotent: already matching is a no-op
	}
	f.Refs[repo][ref] = sha
	return nil
}

func (f *FakeRefs) UpdateRef(_ context.Context, repo, ref, old, newSHA string, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, RefCall{Op: "update", Repo: repo, Ref: ref, Old: old, New: newSHA, Meta: meta})
	if f.UpdateErr != nil {
		return f.UpdateErr
	}
	if f.UpdateConflictOnce {
		f.UpdateConflictOnce = false
		return fmt.Errorf("CAS conflict: %s moved", ref)
	}
	m := f.Refs[repo]
	if m == nil {
		return fmt.Errorf("CAS conflict: %s has no ref %s", repo, ref)
	}
	cur, ok := m[ref]
	if !ok || cur != old {
		return fmt.Errorf("CAS conflict: expected %s, found %s", old, cur)
	}
	m[ref] = newSHA
	return nil
}

func (f *FakeRefs) DeleteRef(_ context.Context, repo, ref string, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	f.Calls = append(f.Calls, RefCall{Op: "delete", Repo: repo, Ref: ref, Meta: meta})
	if m := f.Refs[repo]; m != nil {
		delete(m, ref)
	}
	return nil
}

// updatesFor returns update calls for a ref (never-force proof: Old != "").
func (f *FakeRefs) updatesFor(ref string) []RefCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RefCall
	for _, c := range f.Calls {
		if c.Op == "update" && c.Ref == ref {
			out = append(out, c)
		}
	}
	return out
}

// --- FakeCloser ------------------------------------------------------------------

// FakeCloser records ApplyClosingReferences inputs and returns Closed.
type FakeCloser struct {
	mu     sync.Mutex
	Closed []int
	Calls  int
	PRNum  int
	SHA    string
	Actor  string
	Texts  []string
	Err    error
}

func (f *FakeCloser) ApplyClosingReferences(_ context.Context, _, _ string, prNum int, mergedSHA, actor string, texts []string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.PRNum, f.SHA, f.Actor, f.Texts = prNum, mergedSHA, actor, texts
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]int(nil), f.Closed...), nil
}

// --- env builder -------------------------------------------------------------------

type testEnv struct {
	svc    *Service
	store  store.ObjectStore // raw memory backend (test setup bypasses faults)
	flaky  *flakyStore       // fault-injection wrapper (the Service's store)
	roles  *FakeRoles
	git    *FakeGit
	dirs   *FakeDirs
	refs   *FakeRefs
	closer *FakeCloser

	mu     sync.Mutex
	notify []NotifyEvent
	stream []StreamEvent
	h      *Handler
}

func newTestEnv() *testEnv {
	mem := store.NewMemory()
	fl := &flakyStore{inner: mem,
		failGets: map[string]error{}, failPuts: map[string]int{}, failPutsErr: map[string]error{}}
	roles := &FakeRoles{Roles: map[string]string{}, Public: true}
	g := &FakeGit{Refs: map[string]string{}, Ancestors: map[string]bool{}}
	dirs := &FakeDirs{}
	refs := &FakeRefs{Refs: map[string]map[string]string{}}
	closer := &FakeCloser{}
	e := &testEnv{store: mem, flaky: fl, roles: roles, git: g, dirs: dirs, refs: refs, closer: closer}
	svc := New(fl, roles)
	svc.Git = g
	svc.Dirs = dirs
	svc.Refs = refs
	svc.Closer = closer
	svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	svc.Notify = func(_ context.Context, ev NotifyEvent) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.notify = append(e.notify, ev)
	}
	svc.Stream = func(_ context.Context, ev StreamEvent) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.stream = append(e.stream, ev)
	}
	e.svc = svc
	e.h = &Handler{Svc: svc}
	return e
}

// streams returns a copy of the streamed events (race-safe against
// background recompute/merge passes).
func (e *testEnv) streams() []StreamEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]StreamEvent(nil), e.stream...)
}

// notifies returns a copy of the notify fan-out (race-safe, same reason).
func (e *testEnv) notifies() []NotifyEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]NotifyEvent(nil), e.notify...)
}

// failGet fails subsequent Gets of key with err (fault injection without
// swapping the Service's store — safe under background passes).
func (e *testEnv) failGet(key string, err error) {
	e.flaky.mu.Lock()
	defer e.flaky.mu.Unlock()
	e.flaky.failGets[key] = err
}

// failPut fails the next n Puts of key with a 412 (CAS contention).
func (e *testEnv) failPut(key string, n int) {
	e.flaky.mu.Lock()
	defer e.flaky.mu.Unlock()
	e.flaky.failPuts[key] = n
}

// failPutErr fails subsequent Puts of key with a non-412 backend error.
func (e *testEnv) failPutErr(key string, err error) {
	e.flaky.mu.Lock()
	defer e.flaky.mu.Unlock()
	e.flaky.failPutsErr[key] = err
}

// clearFails drops all injected faults.
func (e *testEnv) clearFails() {
	e.flaky.mu.Lock()
	defer e.flaky.mu.Unlock()
	e.flaky.failGets = map[string]error{}
	e.flaky.failPuts = map[string]int{}
	e.flaky.failPutsErr = map[string]error{}
}

// seedRefs installs ref→sha rows for repos ("o/r" → dir "dir:o/r").
func (e *testEnv) seedRefs(repo string, refs map[string]string) {
	e.git.mu.Lock()
	defer e.git.mu.Unlock()
	for ref, sha := range refs {
		e.git.Refs["dir:"+repo+"\x00"+ref] = sha
	}
}

// delGitRef removes one fake ref (locked: background passes may resolve).
func (e *testEnv) delGitRef(repo, ref string) {
	e.git.mu.Lock()
	defer e.git.mu.Unlock()
	delete(e.git.Refs, "dir:"+repo+"\x00"+ref)
}

// hexSHA renders a deterministic 40-hex sha from a small int.
func hexSHA(n int) string { return fmt.Sprintf("%040x", n) }

// waitTask polls fn until it reports done or the timeout fires.
func waitTask(timeout time.Duration, fn func() *TaskRecord) *TaskRecord {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec := fn(); rec != nil && rec.State != TaskRunning {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}
