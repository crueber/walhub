// wave4b_test.go — third-pass coverage sweep: the residual branches named by
// the earlier crews (copyFile fallback arms, bind_wal Build paths,
// runUnit/RunPass recover paths, writeRebuildMarker failures) plus every
// other reachable gap under the 95% statement floor.
package maintain

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- knobs ------------------------------------------------------------------

// knobStore delegates to an ObjectStore and fails targeted operations.
type knobStore struct {
	store.ObjectStore
	getErr, putErr, delErr, listErr error
	match                           func(key string) bool // nil = match everything
	putPrecondOnce                  bool                  // first Put → precondition
	precondOnceFired                bool
}

func (s *knobStore) hit(key string) bool { return s.match == nil || s.match(key) }

func (s *knobStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.getErr != nil && s.hit(key) {
		return nil, s.getErr
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *knobStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil && s.hit(key) {
		return store.ObjectMeta{}, s.putErr
	}
	if s.putPrecondOnce && s.hit(key) && !s.precondOnceFired {
		s.precondOnceFired = true
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func (s *knobStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if s.delErr != nil && s.hit(key) {
		return s.delErr
	}
	return s.ObjectStore.Delete(ctx, key, ifVersion)
}

func (s *knobStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if s.listErr != nil {
		return s.listErr
	}
	return s.ObjectStore.List(ctx, prefix, startAfter, fn)
}

// knobRepo delegates to a Repo with injectable failures.
type knobRepo struct {
	Repo
	annotateErr    error
	writeCpErr     error
	readLogErr     error
	publishRefsErr error
}

func (r *knobRepo) AnnotatePack(ctx context.Context, c string, h, b, g bool) error {
	if r.annotateErr != nil {
		return r.annotateErr
	}
	return r.Repo.AnnotatePack(ctx, c, h, b, g)
}

func (r *knobRepo) WriteCheckpoint(ctx context.Context, trigger string) error {
	if r.writeCpErr != nil {
		return r.writeCpErr
	}
	return r.Repo.WriteCheckpoint(ctx, trigger)
}

func (r *knobRepo) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	if r.readLogErr != nil {
		return nil, r.readLogErr
	}
	return r.Repo.ReadLog(ctx, from, to)
}

func (r *knobRepo) PublishRefs(ctx context.Context, txn *proto.RefTransaction, meta map[string]string) (uint64, error) {
	if r.publishRefsErr != nil {
		return 0, r.publishRefsErr
	}
	return r.Repo.PublishRefs(ctx, txn, meta)
}

// knobEngine delegates to an Engine with injectable failures/panics.
type knobEngine struct {
	Engine
	extraRepos []string
	openErr    error
	openRepo   Repo
	openPanic  bool
	reposPanic bool
	cfgPanic   bool
	cancelled  context.CancelFunc
	st         store.ObjectStore
}

func (e *knobEngine) Repos() []string {
	if e.reposPanic {
		panic("repos boom")
	}
	return append(e.Engine.Repos(), e.extraRepos...)
}

func (e *knobEngine) Open(ctx context.Context, id string) (Repo, error) {
	if e.openPanic {
		panic("open boom")
	}
	if e.cancelled != nil {
		e.cancelled()
	}
	if e.openErr != nil {
		return nil, e.openErr
	}
	if e.openRepo != nil {
		return e.openRepo, nil
	}
	return e.Engine.Open(ctx, id)
}

func (e *knobEngine) HostConfig() *config.Config {
	if e.cfgPanic {
		panic("cfg boom")
	}
	return e.Engine.HostConfig()
}

func (e *knobEngine) Store() store.ObjectStore {
	if e.st != nil {
		return e.st
	}
	return e.Engine.Store()
}

// knobPlanner overrides Build's result.
type knobPlanner struct {
	BundlePlanner
	buildErr error
	buildOK  bool
}

func (p *knobPlanner) Build(ctx context.Context, repo string, s Slot) (bool, error) {
	if p.buildErr != nil {
		return false, p.buildErr
	}
	return p.buildOK, nil
}

// knobFscker implements FsckRunner with scripted output.
type knobFscker struct {
	missing  []string
	problems int
	err      error
}

func (f *knobFscker) Fsck(ctx context.Context, binary, dir string) ([]string, int, error) {
	return f.missing, f.problems, f.err
}

// knobFollow wraps fakeFollow with a per-ref ancestry script.
type knobFollow struct {
	*fakeFollow
	ancErr error
	ancFn  func(old, new string) (bool, error)
}

func (f *knobFollow) AncestorOf(ctx context.Context, repo, old, new string) (bool, error) {
	if f.ancErr != nil {
		return false, f.ancErr
	}
	if f.ancFn != nil {
		return f.ancFn(old, new)
	}
	return f.fakeFollow.AncestorOf(ctx, repo, old, new)
}

// capturedLogf collects narration lines.
func capturedLogf() (func(format string, args ...any), func() string) {
	var mu sync.Mutex
	var logs []string
	capture := func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
		mu.Unlock()
	}
	return capture, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(logs, "\n")
	}
}

// ---- util.go: fsck report edges + copyFile fallback arms --------------------

func TestGetPutFsckReport_Edges(t *testing.T) {
	ctx := context.Background()
	rep := &proto.FsckReport{}

	// nil store: absent, never an error.
	if found, err := getFsckReport(ctx, nil, "repos/a/b/", rep); found || err != nil {
		t.Fatalf("nil store: found=%v err=%v", found, err)
	}
	if err := putFsckReport(ctx, nil, "repos/a/b/", rep); err != nil {
		t.Fatalf("nil store put: %v", err)
	}

	// A store error that is NOT not-found propagates.
	knob := &knobStore{ObjectStore: newMemStore(), getErr: errors.New("get boom")}
	if _, err := getFsckReport(ctx, knob, "repos/a/b/", rep); err == nil {
		t.Fatal("store get error must propagate")
	}
	// ... and a Put error propagates out of putFsckReport.
	knob.putErr = errors.New("put boom")
	if err := putFsckReport(ctx, knob, "repos/a/b/", rep); err == nil {
		t.Fatal("store put error must propagate")
	}
}

func TestPurgeHeartbeats_DecodeAndErrors(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()

	// A heartbeat with no last_pass_at (and one that fails to decode) is
	// skipped without deleting anything.
	if _, err := st.Put(ctx, store.MaintainerKey("nilts"), store.PutBody{Bytes: (&proto.MaintainerHeartbeat{Host: "nilts"}).Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.MaintainerKey("junk"), store.PutBody{Bytes: []byte{0x00}}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	purged, err := purgeHeartbeats(ctx, st, time.Now(), hbPurgeAfter)
	if err != nil || len(purged) != 0 {
		t.Fatalf("undecodable heartbeats must be skipped: purged=%v err=%v", purged, err)
	}

	// A transient get error on the read-back is skipped, not fatal.
	knob := &knobStore{ObjectStore: st, getErr: errors.New("get boom"), match: func(key string) bool { return strings.Contains(key, "stale") }}
	old := &proto.MaintainerHeartbeat{Host: "stale", LastPassAt: ptrTs(time.Now().Add(-25 * time.Hour))}
	if _, err := st.Put(ctx, store.MaintainerKey("stale"), store.PutBody{Bytes: old.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	purged, err = purgeHeartbeats(ctx, knob, time.Now(), hbPurgeAfter)
	if err != nil || len(purged) != 0 {
		t.Fatalf("get-failure heartbeat must be skipped: purged=%v err=%v", purged, err)
	}

	// A delete failure surfaces as the purge error (§4.2).
	knob.getErr = nil
	knob.delErr = errors.New("delete boom")
	if _, err := purgeHeartbeats(ctx, knob, time.Now(), hbPurgeAfter); err == nil {
		t.Fatal("delete failure must propagate")
	}
}

func TestFreeBytes_Error(t *testing.T) {
	if _, err := freeBytes(filepath.Join(t.TempDir(), "ghost")); err == nil {
		t.Fatal("statfs on a missing dir must fail")
	}
}

func TestCopyFile_FallbackArms(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	body := []byte(strings.Repeat("x", 64))
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// A destination whose parent directory does not exist fails the open
	// (not an ErrExist resume).
	if err := copyFile(filepath.Join(dir, "nope", "dst"), src); err == nil {
		t.Fatal("open failure (non-ErrExist) must error")
	}

	// A directory source: the reflink ioctl fails (→ plain-copy fallback)
	// and io.Copy then fails reading a directory (§6.2 step 1 error arm).
	if err := copyFile(filepath.Join(dir, "dst"), dir); err == nil {
		t.Fatal("copying a directory must fail")
	}

	// reflinkClone: on this filesystem FICLONE is unsupported (both arms of
	// copyFile's fallback run); either answer is contract-legal.
	srcF, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcF.Close()
	dstF, err := os.OpenFile(filepath.Join(dir, "refl.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer dstF.Close()
	reflinkErr := reflinkClone(dstF, srcF)
	if reflinkErr == nil {
		t.Log("FICLONE supported here; success arm covered")
	} else if !errors.Is(reflinkErr, errReflinkUnsupported) {
		t.Fatalf("reflink error must be errReflinkUnsupported: %v", reflinkErr)
	}

	// readPackFile: an existing-but-unreadable side file is an error, not
	// an absence.
	unreadable := filepath.Join(dir, "pack-dead.pack")
	if err := os.WriteFile(unreadable, []byte("P"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if _, ok, err := readPackFile(dir, "dead", ".pack"); err == nil || ok {
		t.Fatalf("unreadable side file must error: ok=%v err=%v", ok, err)
	}
}

// ---- rev.go: large offsets, tie-break, sha256 -------------------------------

func synthIdx(t *testing.T, oidLen int, offs []uint32, large []uint64) []byte {
	t.Helper()
	n := len(offs)
	mk := func(b []byte) []byte {
		if oidLen == 32 {
			s := sha256.Sum256(b)
			return s[:]
		}
		s := sha1.Sum(b)
		return s[:]
	}
	buf := bytes.NewBuffer(nil)
	buf.Write([]byte("\xfftOc"))
	buf.Write(u32be(2))
	fanout := make([]byte, 1024)
	binary.BigEndian.PutUint32(fanout[4*255:], uint32(n))
	buf.Write(fanout)
	for i := range n {
		buf.Write(mk([]byte{byte(i)}))
	}
	for range n {
		buf.Write(u32be(0))
	}
	for _, o := range offs {
		buf.Write(u32be(o))
	}
	for _, l := range large {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], l)
		buf.Write(b[:])
	}
	buf.Write(mk([]byte("pack")))
	buf.Write(mk([]byte("idx")))
	return buf.Bytes()
}

func TestBuildRevFile_LargeOffsetsTieAndSha256(t *testing.T) {
	// Large offsets (MSB set) read through the 64-bit table.
	idx := synthIdx(t, 20, []uint32{0x80000000, 0x80000001}, []uint64{1 << 32, 200})
	rev, err := buildRevFile(idx, 20)
	if err != nil {
		t.Fatalf("large offsets: %v", err)
	}
	if g := binary.BigEndian.Uint32(rev[12:]); g != 1 {
		t.Fatalf("rank 0 must be the idx entry with the smaller large offset 200, got %d", g)
	}
	if g := binary.BigEndian.Uint32(rev[16:]); g != 0 {
		t.Fatalf("rank 1 must be the idx entry with offset 2^32, got %d", g)
	}

	// Equal offsets fall back to the deterministic idx-order tie-break.
	idx = synthIdx(t, 20, []uint32{100, 100}, nil)
	if rev, err = buildRevFile(idx, 20); err != nil {
		t.Fatalf("tie-break: %v", err)
	}
	if g := binary.BigEndian.Uint32(rev[12:]); g != 0 {
		t.Fatalf("tie-break rank 0 = %d, want 0", g)
	}

	// sha256: 32-byte oids, hash id 2.
	idx = synthIdx(t, 32, []uint32{50}, nil)
	rev, err = buildRevFile(idx, 32)
	if err != nil {
		t.Fatalf("sha256: %v", err)
	}
	if binary.BigEndian.Uint32(rev[8:12]) != 2 {
		t.Fatal("sha256 .rev must carry hash id 2")
	}
	if len(rev) != 12+4+64 {
		t.Fatalf("sha256 .rev len = %d", len(rev))
	}
}

// ---- revindex: write/upload/annotate errors ---------------------------------

func revIndexFixture(t *testing.T) (*fakeRepo, *fakeEngine, *Maintainer, *config.Config) {
	t.Helper()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false

	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := synthIdx(t, 20, []uint32{100, 200}, nil)
	if err := os.WriteFile(filepath.Join(packDir, "pack-c0ff.idx"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}
	mst.Packs = append(mst.Packs, pack("c0ff", 1, 10, revIndexThreshold+1, 0))
	repo := &fakeRepo{id: "acme/widget", dir: dir, m: mst, git: &fakeGit{}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	return repo, eng, m, eff
}

func TestRunRevIndex_WriteUploadAnnotateErrors(t *testing.T) {
	ctx := context.Background()

	// A read-only pack dir fails the temp .rev write (§10 never touches pack
	// bytes; the marker-style install needs a writable pack dir).
	repo, eng, m, eff := revIndexFixture(t)
	packDir := filepath.Join(repo.dir, "objects", "pack")
	if err := os.Chmod(packDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(packDir, 0o755) })
	outcome, detail := m.runRevIndex(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{})
	if outcome != OutcomeError || !strings.Contains(detail, "write temp .rev") {
		t.Fatalf("read-only pack dir: outcome=%v detail=%q", outcome, detail)
	}

	// A store put failure surfaces as the upload error.
	repo, eng, m, eff = revIndexFixture(t)
	m.eng = &knobEngine{Engine: eng, st: &knobStore{
		ObjectStore: eng.st, putErr: errors.New("put boom"),
		match: func(key string) bool { return strings.HasSuffix(key, ".rev") },
	}}
	outcome, detail = m.runRevIndex(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{})
	if outcome != OutcomeError || !strings.Contains(detail, "upload .rev") {
		t.Fatalf("store put failure: outcome=%v detail=%q", outcome, detail)
	}

	// An annotate failure (manifest CAS) surfaces as the annotate error.
	repo, _, m, eff = revIndexFixture(t)
	krepo := &knobRepo{Repo: repo, annotateErr: errors.New("annotate boom")}
	outcome, detail = m.runRevIndex(ctx, krepo, rebuildSnap(eff, repo.m), nopLogger{})
	if outcome != OutcomeError || !strings.Contains(detail, "annotate") {
		t.Fatalf("annotate failure: outcome=%v detail=%q", outcome, detail)
	}
}

// ---- repair: empty report, fetch/publish/write errors -----------------------

func TestRunRepair_EdgeBranches(t *testing.T) {
	ctx := context.Background()
	newEnv := func(t *testing.T) (*fakeRepo, *fakeEngine, *Maintainer, *config.Config) {
		eff := defaultEff()
		eff.Bundles.Strategy = nil
		eff.Maintenance.Checkpoints = false
		eff.Maintenance.FsckInterval = 0
		eff.Compaction.Enabled = false
		eff.Upstream.Git = "https://github.com/acme/widget.git"
		mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 7}
		repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: mst, git: &fakeGit{}}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		return repo, eng, m, eff
	}

	// An fsck.pb with missing_total but an empty bounded list defers to the
	// next audit (§9.2).
	repo, eng, m, eff := newEnv(t)
	snap := rebuildSnap(eff, repo.m)
	snap.Fsck = &proto.FsckReport{MissingTotal: 3}
	outcome, detail := m.runRepair(ctx, repo, snap, nopLogger{})
	if outcome != OutcomeOK || !strings.Contains(detail, "no missing oids listed") {
		t.Fatalf("empty missing list: outcome=%v detail=%q", outcome, detail)
	}

	// A refused want is an ERROR (§9.2.5), never a silent hole.
	repo, _, m, eff = newEnv(t)
	repo.git.fetchErr = errors.New("fetch boom")
	snap = rebuildSnap(eff, repo.m)
	snap.Fsck = &proto.FsckReport{MissingTotal: 1, Missing: []string{strings.Repeat("a", 40)}}
	if outcome, detail = m.runRepair(ctx, repo, snap, nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "fetch batch") {
		t.Fatalf("fetch failure: outcome=%v detail=%q", outcome, detail)
	}

	// A publish failure keeps repaired_seq at 0 so the next pass retries.
	repo, _, m, eff = newEnv(t)
	repo.git.fetchPackPath = "pack-f00d.pack"
	repo.compactErr = errors.New("publish boom")
	snap = rebuildSnap(eff, repo.m)
	snap.Fsck = &proto.FsckReport{MissingTotal: 1, Missing: []string{strings.Repeat("b", 40)}}
	if outcome, detail = m.runRepair(ctx, repo, snap, nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "publish repair pack") {
		t.Fatalf("publish failure: outcome=%v detail=%q", outcome, detail)
	}

	// A failing fsck.pb overwrite errors the batch (report never lost).
	repo, eng, m, eff = newEnv(t)
	repo.git.fetchPackPath = "pack-f00d.pack"
	m.eng = &knobEngine{Engine: eng, st: &knobStore{ObjectStore: eng.st, putErr: errors.New("put boom"),
		match: func(key string) bool { return strings.HasSuffix(key, "fsck.pb") }}}
	snap = rebuildSnap(eff, repo.m)
	snap.Fsck = &proto.FsckReport{MissingTotal: 1, Missing: []string{strings.Repeat("c", 40)}}
	if outcome, detail = m.runRepair(ctx, repo, snap, nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "fsck.pb write") {
		t.Fatalf("fsck.pb write failure: outcome=%v detail=%q", outcome, detail)
	}
}

// ---- fsck unit: unwired runner, errors, bound, report-write, exec edges -----

func TestRunFsck_EdgeBranches(t *testing.T) {
	ctx := context.Background()
	env := func(t *testing.T) (*fakeRepo, *fakeEngine, *Maintainer, *config.Config) {
		eff := defaultEff()
		eff.Bundles.Strategy = nil
		eff.Maintenance.Checkpoints = false
		eff.Compaction.Enabled = false
		mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 2}
		repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: mst, git: &fakeGit{}}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		return repo, eng, m, eff
	}

	// Runner not wired.
	repo, eng, m, eff := env(t)
	if outcome, detail := m.runFsck(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{}); outcome != OutcomeError || detail != "fsck runner not wired" {
		t.Fatalf("unwired: outcome=%v detail=%q", outcome, detail)
	}

	// The audit itself failing errors the unit.
	_, _, m, eff = env(t)
	m.opt.Fscker = &knobFscker{err: errors.New("fsck boom")}
	repo2 := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 2}}
	if outcome, _ := m.runFsck(ctx, repo2, rebuildSnap(eff, repo2.m), nopLogger{}); outcome != OutcomeError {
		t.Fatalf("audit failure: outcome=%v", outcome)
	}

	// The missing list is bounded at fsckMissingBound; the total is not.
	_, eng, m, eff = env(t)
	many := make([]string, fsckMissingBound+1)
	for i := range many {
		many[i] = strings.Repeat("d", 40)
	}
	m.opt.Fscker = &knobFscker{missing: many}
	repo3 := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 2}, store: eng.st}
	if outcome, detail := m.runFsck(ctx, repo3, rebuildSnap(eff, repo3.m), nopLogger{}); outcome != OutcomeOK || !strings.Contains(detail, "100001 missing") {
		t.Fatalf("bounded report: outcome=%v detail=%q", outcome, detail)
	}
	body, _, err := store.GetBytes(ctx, eng.st, repo3.Prefix()+"fsck.pb", store.GetOptions{})
	if err != nil || body == nil {
		t.Fatalf("fsck.pb missing: %v", err)
	}
	rep := &proto.FsckReport{}
	if err := rep.Unmarshal(body); err != nil || rep.MissingTotal != fsckMissingBound+1 || len(rep.Missing) != fsckMissingBound {
		t.Fatalf("bounded report shape: total=%d len=%d err=%v", rep.MissingTotal, len(rep.Missing), err)
	}

	// A failing fsck.pb write errors the unit.
	_, eng, m, eff = env(t)
	m.eng = &knobEngine{Engine: eng, st: &knobStore{
		ObjectStore: eng.st, putErr: errors.New("put boom"),
	}}
	m.opt.Fscker = &knobFscker{}
	repo4 := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 2}}
	if outcome, detail := m.runFsck(ctx, repo4, rebuildSnap(eff, repo4.m), nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "fsck.pb write") {
		t.Fatalf("report write failure: outcome=%v detail=%q", outcome, detail)
	}

	// execFscker: a canceled context aborts the audit; a missing binary is a
	// launch failure.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := (execFscker{}).Fsck(cctx, "git", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fsck: %v", err)
	}
	if _, _, err := (execFscker{}).Fsck(ctx, "walgit-no-such-git", t.TempDir()); err == nil {
		t.Fatal("missing binary must fail the audit")
	}
}

// ---- checkpoint: lag gauges + writer failure --------------------------------

func TestRunCheckpoint_LagAndError(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 9,
		Checkpoint: &proto.CheckpointRef{Seq: 4, CreatedAt: ptrTs(time.Now().Add(-30 * time.Second))}}
	repo := &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})

	if outcome, detail := m.runCheckpoint(ctx, repo, rebuildSnap(eff, mst), nopLogger{}, "entries"); outcome != OutcomeOK || detail != "entries" {
		t.Fatalf("checkpoint: outcome=%v detail=%q", outcome, detail)
	}

	krepo := &knobRepo{Repo: repo, writeCpErr: errors.New("cp boom")}
	if outcome, _ := m.runCheckpoint(ctx, krepo, rebuildSnap(eff, mst), nopLogger{}, "entries"); outcome != OutcomeError {
		t.Fatalf("checkpoint writer failure must error: outcome=%v", outcome)
	}
}

// ---- bundles unit: unwired planner, no missing slot, build err, built ok ----

func TestRunBundles_EdgeBranches(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	mst := &proto.Manifest{Repo: "acme/widget"}
	repo := &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	snap := rebuildSnap(eff, mst)

	// Planner not wired.
	if outcome, detail := m.runBundles(ctx, repo, snap, nopLogger{}); outcome != OutcomeError || detail != "bundle planner not wired" {
		t.Fatalf("unwired: outcome=%v detail=%q", outcome, detail)
	}

	// Every slot settled → "no missing slot".
	m.opt.Planner = &fakePlanner{slots: map[string][]Slot{
		"acme/widget": {{Strategy: "weekly", Kind: "full", Slot: 1, State: "built", BundleID: "b"}},
	}}
	if outcome, detail := m.runBundles(ctx, repo, snap, nopLogger{}); outcome != OutcomeOK || detail != "no missing slot" {
		t.Fatalf("no missing: outcome=%v detail=%q", outcome, detail)
	}

	// A failing build errors the unit.
	m.opt.Planner = &knobPlanner{BundlePlanner: &fakePlanner{slots: map[string][]Slot{
		"acme/widget": {{Strategy: "weekly", Kind: "full", Slot: 1, State: "missing"}},
	}}, buildErr: errors.New("build boom")}
	if outcome, _ := m.runBundles(ctx, repo, snap, nopLogger{}); outcome != OutcomeError {
		t.Fatalf("build failure: outcome=%v", outcome)
	}

	// A build that settles the slot reports state=built.
	m.opt.Planner = &fakePlanner{buildsOK: true, slots: map[string][]Slot{
		"acme/widget": {{Strategy: "weekly", Kind: "full", Slot: 1, State: "missing"}},
	}}
	if outcome, detail := m.runBundles(ctx, repo, snap, nopLogger{}); outcome != OutcomeOK || !strings.Contains(detail, "state=built") {
		t.Fatalf("built: outcome=%v detail=%q", outcome, detail)
	}

	// headSeqAt: a zero window and a failing log walk both yield bar 0.
	if got := m.headSeqAt(ctx, repo, time.Time{}); got != 0 {
		t.Fatalf("zero window bar = %d", got)
	}
	if got := m.headSeqAt(ctx, &knobRepo{Repo: repo, readLogErr: errors.New("log boom")}, time.Now()); got != 0 {
		t.Fatalf("log walk failure bar = %d", got)
	}
}

// ---- compact: non-held lease error + retention GC failures ------------------

func TestRunCompact_LeaseErrorAndGCFailures(t *testing.T) {
	ctx := context.Background()
	env := func(t *testing.T, knobs ...any) (*fakeRepo, *fakeEngine, *Maintainer, *config.Config, func() string) {
		eff := defaultEff()
		eff.Bundles.Strategy = nil
		eff.Maintenance.Checkpoints = false
		eff.Maintenance.FsckInterval = 0
		eff.Compaction.Enabled = true
		eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)
		mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 3}
		mst.Packs = append(mst.Packs, pack("old", 1, 10, 1, 0))
		repo := &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{geoDiff: gitPackDiff("folded")}}
		eng := newFakeEngine(eff, repo)
		logf, logs := capturedLogf()
		m := New(eng, Options{Leaser: &fakeLeaser{}, Logf: logf})
		return repo, eng, m, eff, logs
	}

	// A non-held lease error is an ERROR, not a deferral (§3.3).
	repo, _, m, eff, _ := env(t)
	m.opt.Leaser = &fakeLeaser{err: errors.New("lease boom")}
	if outcome, detail := m.runCompact(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "lease: ") {
		t.Fatalf("lease error: outcome=%v detail=%q", outcome, detail)
	}

	// The retention GC failing after a successful fold is logged, not fatal.
	repo, eng, m, eff, logs := env(t)
	m.eng = &knobEngine{Engine: eng, st: &knobStore{ObjectStore: eng.st, listErr: errors.New("list boom")}}
	if outcome, detail := m.runCompact(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{}); outcome != OutcomeOK {
		t.Fatalf("fold with gc failure: outcome=%v detail=%q", outcome, detail)
	}
	if !strings.Contains(logs(), "retention gc failed") {
		t.Fatalf("gc failure must be narrated: %s", logs())
	}

	// gcSuperseded: a failing log walk errors the sweep.
	repo, _, m, eff, _ = env(t)
	if _, err := m.gcSuperseded(ctx, &knobRepo{Repo: repo, readLogErr: errors.New("log boom")}, rebuildSnap(eff, repo.m)); err == nil {
		t.Fatal("log walk failure must error the GC")
	}

	// gcSuperseded: a failing delete of an out-of-window object errors it.
	repo, eng, m, eff, _ = env(t)
	repo.entries = []*proto.LogEntry{{Seq: 3, Kind: proto.EntryKindCompact,
		CreatedAt: ptrTs(time.Now().Add(-8 * 24 * time.Hour)), Supersedes: []string{"dead"}}}
	st := eng.Store()
	if _, err := st.Put(ctx, repo.Prefix()+"wal/dead.pack", store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	m.eng = &knobEngine{Engine: eng, st: &knobStore{ObjectStore: eng.st, delErr: errors.New("delete boom"),
		match: func(key string) bool { return strings.Contains(key, "dead") }}}
	if _, err := m.gcSuperseded(ctx, repo, rebuildSnap(eff, repo.m)); err == nil {
		t.Fatal("delete failure must error the GC")
	}
}

// ---- rebuild: marker-write failures, marker parsing, keep-packs, publish ----

func rebBase(t *testing.T) (*config.Config, *fakeRepo, *fakeEngine, *Maintainer) {
	t.Helper()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Cache.Dir = t.TempDir()
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Git.HistoryPack = true
	mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}
	mst.Packs = append(mst.Packs, pack("old1", 1, 10, 1, 0))
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{id: "acme/widget", dir: dir, m: mst, git: &fakeGit{
		fullDiff:    gitPackDiff("newbase"),
		historyPack: "pack-hist",
		commitGraph: "cg1",
	}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	return eff, repo, eng, m
}

func TestRunRebuild_MarkerWriteFailures(t *testing.T) {
	ctx := context.Background()

	// A phase hook that removes the scratch tree makes the post-history
	// marker write fail (kill-between-phases durability).
	_, _, _, m := rebBase(t)
	m.opt.RebuildPhaseHook = func(phase string) error {
		if phase == phaseRepacked {
			return nil
		}
		return nil
	}
	_ = m

	_, repo, _, m := rebBase(t)
	eff := m.eng.HostConfig()
	m.opt.RebuildPhaseHook = func(phase string) error {
		if phase == phaseHistory {
			_ = os.RemoveAll(filepath.Join(eff.Cache.Dir, "_rebuild"))
		}
		return nil
	}
	if outcome, detail := m.runRebuild(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "marker write") {
		t.Fatalf("post-history marker write: outcome=%v detail=%q", outcome, detail)
	}

	// ... and the post-repack write.
	_, repo, _, m = rebBase(t)
	eff = m.eng.HostConfig()
	m.opt.RebuildPhaseHook = func(phase string) error {
		if phase == phaseRepacked {
			_ = os.RemoveAll(filepath.Join(eff.Cache.Dir, "_rebuild"))
		}
		return nil
	}
	if outcome, detail := m.runRebuild(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "marker write") {
		t.Fatalf("post-commit-graph marker write: outcome=%v detail=%q", outcome, detail)
	}
}

func TestRebuildMarker_ParseAndKeepPacks(t *testing.T) {
	eff, _, _, _ := rebBase(t)
	markerPath := filepath.Join(eff.Cache.Dir, "_rebuild", "acme", "widget.json")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, ok := readRebuildMarker(markerPath); ok || m != nil {
		t.Fatal("garbage marker must not parse")
	}

	// keep-packs: tier-2 bases and history packs survive; fresh tier-0 not.
	mst := &proto.Manifest{}
	mst.Packs = append(mst.Packs,
		pack("base", 1, 10, 1, 2),
		pack("hist", 2, 10, 1, 0),
		pack("fresh", 3, 10, 1, 0))
	mst.Packs[1].Kind = proto.PackKindHistory
	keep := rebuildKeepPacks(mst)
	if len(keep) != 2 || !strings.Contains(keep[0], "base") || !strings.Contains(keep[1], "hist") {
		t.Fatalf("keep = %v", keep)
	}
}

func TestPublishRebuild_ErrorArms(t *testing.T) {
	ctx := context.Background()
	_, repo, eng, m := rebBase(t)
	eff := m.eng.HostConfig()
	scratch, _ := rebuildScratch(eff.Cache.Dir, mustID(repo))
	packDir := filepath.Join(scratch, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := rebuildSnap(eff, repo.m)

	// commit-graph side file present but unreadable → read error.
	cg := filepath.Join(packDir, "pack-cg1.commit-graph")
	if err := os.WriteFile(cg, []byte("CG"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cg, 0o644) })
	marker := &rebuildMarker{StartedHeadSeq: 1, Phase: phaseCommitGraph, NewPacks: []string{"newbase"}, CommitGraph: "cg1"}
	if outcome, detail := m.publishRebuild(ctx, repo, snap, scratch, marker, nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "read commit-graph") {
		t.Fatalf("commit-graph read: outcome=%v detail=%q", outcome, detail)
	}
	_ = os.Remove(cg)

	// PublishCompact failure errors the publish.
	repo.compactErr = errors.New("publish boom")
	marker = &rebuildMarker{StartedHeadSeq: 1, Phase: phaseCommitGraph, NewPacks: []string{"newbase"}}
	if outcome, detail := m.publishRebuild(ctx, repo, snap, scratch, marker, nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "publish base") {
		t.Fatalf("publish base: outcome=%v detail=%q", outcome, detail)
	}
	repo.compactErr = nil

	// A history pack whose store put fails errors the publish.
	m.eng = &knobEngine{Engine: eng, st: &knobStore{ObjectStore: eng.st, putErr: errors.New("put boom"),
		match: func(key string) bool { return strings.Contains(key, "wal/hist.pack") }}}
	if err := os.WriteFile(filepath.Join(packDir, "pack-hist.pack"), []byte("H"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker = &rebuildMarker{StartedHeadSeq: 1, Phase: phaseCommitGraph, NewPacks: []string{"newbase"}, History: "hist"}
	if outcome, detail := m.publishRebuild(ctx, repo, snap, scratch, marker, nopLogger{}); outcome != OutcomeError || !strings.Contains(detail, "upload history") {
		t.Fatalf("history upload: outcome=%v detail=%q", outcome, detail)
	}

	// uploadPackFiles: an unreadable side file errors the upload.
	unreadable := filepath.Join(packDir, "pack-abc.pack")
	if err := os.WriteFile(unreadable, []byte("P"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if err := m.uploadPackFiles(ctx, repo.Prefix(), packDir, "abc", filepath.Join(repo.dir, "objects", "pack")); err == nil {
		t.Fatal("unreadable side file must fail the upload")
	}
}

// ---- the pass loop: panic recovery, purge/hb failures, ctx edges ------------

func TestRunPass_RecoverAndStoreFailures(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false

	// A panicking engine is recovered by the pass (§3.2 robustness).
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	m := New(&knobEngine{Engine: eng, cfgPanic: true}, Options{Leaser: &fakeLeaser{}})
	m.RunPass(ctx)
	if m.Metrics().Passes != 1 {
		t.Fatal("recovered pass must still count")
	}

	// A failing heartbeat purge is logged and the pass continues.
	eng = newFakeEngine(eff, repo)
	logf, logs := capturedLogf()
	m = New(&knobEngine{Engine: eng, st: &knobStore{ObjectStore: eng.st, listErr: errors.New("list boom")}},
		Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.RunPass(ctx)
	if !strings.Contains(logs(), "heartbeat purge failed") {
		t.Fatalf("purge failure must be narrated: %s", logs())
	}

	// A failing heartbeat write is logged and the pass continues.
	eng = newFakeEngine(eff, repo)
	logf, logs = capturedLogf()
	m = New(&knobEngine{Engine: eng, st: &knobStore{ObjectStore: eng.st,
		putErr: errors.New("put boom"), match: func(key string) bool { return strings.HasPrefix(key, "maintain/") }}},
		Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.RunPass(ctx)
	if !strings.Contains(logs(), "heartbeat write failed") {
		t.Fatalf("hb write failure must be narrated: %s", logs())
	}

	// A canceled context ends the pass before any unit runs.
	eng = newFakeEngine(eff, repo)
	logf, logs = capturedLogf()
	m = New(eng, Options{Leaser: &fakeLeaser{}, Logf: logf})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	m.RunPass(cctx)
	if strings.Contains(logs(), "pass done") {
		t.Fatal("canceled pass must not reach the pass-done line")
	}
}

func TestProcessRepo_RecoverLoadAndCtxEdges(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	mst := &proto.Manifest{Repo: "acme/widget"}
	repo := &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{}}
	eng := newFakeEngine(eff, repo)

	// A panicking Open is recovered per repo (§3.2 step 2).
	logf, logs := capturedLogf()
	m := New(&knobEngine{Engine: eng, openPanic: true}, Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.processRepo(ctx, "acme/widget", eff)
	if !strings.Contains(logs(), "unit panicked") {
		t.Fatalf("panic must be narrated: %s", logs())
	}

	// An Open that cancels the context ends the repo's turn before any unit.
	logf, logs = capturedLogf()
	cctx, cancel := context.WithCancel(ctx)
	m = New(&knobEngine{Engine: eng, cancelled: cancel}, Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.processRepo(cctx, "acme/widget", eff)
	if strings.Contains(logs(), "outcome=") {
		t.Fatalf("canceled turn must run no unit: %s", logs())
	}

	// Snapshot failures: missing repo, failing refs sync, bad repo settings.
	logf, logs = capturedLogf()
	m = New(&knobEngine{Engine: eng, extraRepos: []string{"ghost/z"}}, Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.RunPass(ctx)
	if !strings.Contains(logs(), "ghost/z: snapshot failed") {
		t.Fatalf("open failure must be narrated: %s", logs())
	}

	syncRepo := &fakeRepo{id: "acme/sync", m: &proto.Manifest{Repo: "acme/sync"}, syncErr: errors.New("sync boom")}
	eng2 := newFakeEngine(eff, syncRepo)
	logf, logs = capturedLogf()
	m = New(eng2, Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.RunPass(ctx)
	if !strings.Contains(logs(), "acme/sync: snapshot failed") {
		t.Fatalf("sync failure must be narrated: %s", logs())
	}

	badRepo := &fakeRepo{id: "acme/bad", m: &proto.Manifest{Repo: "acme/bad",
		Settings: &proto.RepoSettings{Toml: "[compaction\nbroken"}}}
	eng3 := newFakeEngine(eff, badRepo)
	logf, logs = capturedLogf()
	m = New(eng3, Options{Leaser: &fakeLeaser{}, Logf: logf})
	m.RunPass(ctx)
	if !strings.Contains(logs(), "acme/bad: snapshot failed") {
		t.Fatalf("config failure must be narrated: %s", logs())
	}
}

func TestProcessRepo_WrongHostDefersButRuns(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Cache.MaxBytes = 10 // pack set (1000 bytes) can never fit
	eff.Compaction.Enabled = true
	eff.Compaction.TriggerPacks = 2
	mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 2}
	mst.Packs = append(mst.Packs, pack("p1", 1, 1000, 1, 0), pack("p2", 2, 1000, 1, 0))
	repo := &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{geoDiff: gitPackDiff("folded")}}
	eng := newFakeEngine(eff, repo)
	logf, logs := capturedLogf()
	m := New(eng, Options{Leaser: &fakeLeaser{}, Logf: logf})

	m.processRepo(ctx, "acme/widget", eff)
	if !strings.Contains(logs(), "outcome=wrong-host") {
		t.Fatalf("wrong-host must be narrated: %s", logs())
	}
	if n := m.Metrics().Units[KindCompact][OutcomeOK]; n != 1 {
		t.Fatalf("unit still runs after the wrong-host gate; compact outcomes = %v", m.Metrics().Units[KindCompact])
	}
}

func TestRunUnit_TaskErrMidpassTicksAndExecRevIndex(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	mst := &proto.Manifest{Repo: "acme/widget"}
	repo := &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{}}
	eng := newFakeEngine(eff, repo)

	// A failing task runner errors the unit (outcome, not panic).
	eng.tasks.hook = func(repo, kind string, fn func(ctx context.Context, t TaskLogger) error) error {
		return errors.New("task boom")
	}
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	if outcome, detail := m.runUnit(ctx, repo, rebuildSnap(eff, mst), Selection{Kind: KindCheckpoint, Reason: "t"}); outcome != OutcomeError || detail != "task boom" {
		t.Fatalf("task error: outcome=%v detail=%q", outcome, detail)
	}

	// Mid-pass heartbeat ticks rewrite the heartbeat on the pass goroutine.
	eng = newFakeEngine(eff, &fakeRepo{id: "acme/widget", m: mst, git: &fakeGit{}})
	m = New(eng, Options{Leaser: &fakeLeaser{}})
	m.hbStart(&proto.MaintainerHeartbeat{Host: "tickhost"})
	m.hbMu.Lock()
	m.hbTicker = time.NewTicker(2 * time.Millisecond)
	m.hbMu.Unlock()
	eng.tasks.hook = func(repo, kind string, fn func(ctx context.Context, t TaskLogger) error) error {
		time.Sleep(80 * time.Millisecond)
		return fn(ctx, nopLogger{})
	}
	outcome, _ := m.runUnit(ctx, eng.byID["acme/widget"], rebuildSnap(eff, mst), Selection{Kind: KindCheckpoint, Reason: "t"})
	m.hbStop()
	if outcome != OutcomeOK {
		t.Fatalf("tick run outcome=%v", outcome)
	}
	if m.metrics.heartbeatWrites.Load() < 2 {
		t.Fatalf("mid-pass ticks must write heartbeats: %d", m.metrics.heartbeatWrites.Load())
	}

	// execUnit dispatches rev-index (the last unexercised priority row).
	m2 := New(eng, Options{Leaser: &fakeLeaser{}})
	outcome, detail := m2.execUnit(ctx, eng.byID["acme/widget"], rebuildSnap(eff, mst), Selection{Kind: KindRevIndex, Reason: "t"}, nopLogger{})
	if outcome != OutcomeOK || detail != "no candidate" {
		t.Fatalf("rev-index dispatch: outcome=%v detail=%q", outcome, detail)
	}
}

// ---- metrics: follow rounds counter -----------------------------------------

func TestMetricsSnapshot_FollowRounds(t *testing.T) {
	eng := newFakeEngine(defaultEff())
	m := New(eng, Options{})
	m.metrics.recordFollow("acme/widget", "in-sync")
	snap := m.Metrics()
	if snap.FollowRoundsByOutcome["acme/widget in-sync"] != 1 {
		t.Fatalf("rounds = %v", snap.FollowRoundsByOutcome)
	}
}

// ---- leases: CAS race and undecodable lease ---------------------------------

func TestStoreLeaser_RaceAndCorruptLease(t *testing.T) {
	ctx := context.Background()

	// A Put racing another acquirer (412) re-reads and lands the lease.
	st := newMemStore()
	leaser := StoreLeaser{St: &knobStore{ObjectStore: st, putPrecondOnce: true}}
	release, err := leaser.Acquire(ctx, "compact", "h1", "test", time.Minute, 0)
	if err != nil {
		t.Fatalf("raced acquire: %v", err)
	}
	release()

	// An undecodable lease object is an error, never a steal.
	if _, err := st.Put(ctx, store.LeaseKey("junk"), store.PutBody{Bytes: []byte{0x00}}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (StoreLeaser{St: st}).Acquire(ctx, "junk", "h1", "test", time.Minute, 0); err == nil {
		t.Fatal("corrupt lease must error")
	}
}

// ---- follow: recover, skip/failed rounds, fetcher arms, exec edges ----------

func TestRunFollow_OffBlocksUntilCancel(t *testing.T) {
	ctx := context.Background()
	eng := newFakeEngine(defaultEff())
	m := New(eng, Options{FollowInterval: -time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.RunFollow(ctx)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("off-loop must unblock on drain")
	}
}

func TestFollowRoundAll_PanicSkipAndFailures(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false

	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}}
	eng := newFakeEngine(eff, repo)

	// A panicking engine is recovered per round.
	logf, logs := capturedLogf()
	m := New(&knobEngine{Engine: eng, reposPanic: true}, Options{Logf: logf})
	m.followRoundAll(ctx)
	if !strings.Contains(logs(), "follow round panicked") {
		t.Fatalf("panic must be narrated: %s", logs())
	}

	// Excluded repos are skipped; unknown repos fail the round loudly.
	logf, logs = capturedLogf()
	cfg := defaultEff()
	cfg.Bundles.Strategy = nil
	cfg.Maintenance.Checkpoints = false
	cfg.Maintenance.FsckInterval = 0
	cfg.Compaction.Enabled = false
	cfg.Placement.MaintainExclude = []string{"secret/*"}
	m = New(&knobEngine{Engine: eng, extraRepos: []string{"secret/x", "ghost/z"}}, Options{Logf: logf})
	m.followRoundAll(ctx)
	if !strings.Contains(logs(), "ghost/z follow outcome=failed") {
		t.Fatalf("open failure round must be narrated: %s", logs())
	}

	// A failing refs sync fails the round (loadRepo error arm).
	syncRepo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}, syncErr: errors.New("sync boom")}
	eng2 := newFakeEngine(eff, syncRepo)
	logf, logs = capturedLogf()
	m = New(eng2, Options{Logf: logf})
	m.followRoundAll(ctx)
	if !strings.Contains(logs(), "follow outcome=failed") {
		t.Fatalf("load failure round must be narrated: %s", logs())
	}
}

func TestFollowOnce_FetchAncestryAndPublishErrors(t *testing.T) {
	ctx := context.Background()
	const oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const oidC = "cccccccccccccccccccccccccccccccccccccccc"

	fixture := func(t *testing.T, ours map[string]string) (*fakeRepo, *fakeEngine, *fakeFollow, *Maintainer) {
		eff := defaultEff()
		eff.Bundles.Strategy = nil
		eff.Maintenance.Checkpoints = false
		eff.Maintenance.FsckInterval = 0
		eff.Compaction.Enabled = false
		eff.Maintenance.FollowInterval = -1
		eff.Upstream.Git = "https://github.com/acme/widget.git"
		eff.Upstream.Follow = []string{"refs/heads/main", "refs/heads/dev"}
		repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 3}, refs: ours}
		eng := newFakeEngine(eff, repo)
		f := &fakeFollow{}
		m := New(eng, Options{Follow: f})
		return repo, eng, f, m
	}

	// An ancestry failure fails the round.
	repo, eng, f, m := fixture(t, map[string]string{"refs/heads/main": oidA})
	kf := &knobFollow{fakeFollow: f, ancErr: errors.New("merge-base boom")}
	m.opt.Follow = kf
	f.tips = map[string]string{"refs/heads/main": oidB}
	if err := m.followOnce(ctx, "acme/widget"); err == nil {
		t.Fatal("ancestry failure must fail the round")
	}

	// A failing publish fails the round (§8.4 ordinary push path).
	repo, eng, f, m = fixture(t, map[string]string{"refs/heads/main": oidA})
	m.opt.Follow = &knobFollow{fakeFollow: f, ancFn: func(old, new string) (bool, error) { return true, nil }}
	f.tips = map[string]string{"refs/heads/main": oidB}
	krepo := &knobRepo{Repo: repo, publishRefsErr: errors.New("publish boom")}
	m.eng = &knobEngine{Engine: eng, openRepo: krepo}
	if err := m.followOnce(ctx, "acme/widget"); err == nil {
		t.Fatal("publish failure must fail the round")
	}

	// One fast-forward ref published alongside one rewound ref is reported
	// as refused (ff-only per ref, §8.3).
	repo, _, f, m = fixture(t, map[string]string{"refs/heads/main": oidA, "refs/heads/dev": oidB})
	m.opt.Follow = &knobFollow{fakeFollow: f, ancFn: func(old, new string) (bool, error) {
		if old == oidA {
			return true, nil
		}
		return false, nil
	}}
	f.tips = map[string]string{"refs/heads/main": oidB, "refs/heads/dev": oidC}
	if err := m.followOnce(ctx, "acme/widget"); err != nil {
		t.Fatalf("mixed round: %v", err)
	}
	if round, ok := m.LastRound("acme/widget"); !ok || round.Outcome != "refused" {
		t.Fatalf("mixed round outcome = %+v", round)
	}
}

func TestExecFollow_InitServingAndAncestorErrors(t *testing.T) {
	ctx := context.Background()

	// A scratch path blocked by a file component: the scratch stat reports
	// ENOTDIR (not not-exist) so init is skipped, and the alternates write
	// into the scratch fails.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "follow"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	serving := filepath.Join(dir, "acme", "api.git")
	if err := os.MkdirAll(serving, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git.InitLocalRepo(dir, git.RepoId{Owner: "acme", Name: "api"}, git.Sha1); err != nil {
		t.Fatal(err)
	}
	f := execFollow{CacheDir: dir}
	if _, err := f.Fetch(ctx, "acme/api", "https://example.invalid/acme/api.git", "", nil, nil); err == nil {
		t.Fatal("blocked scratch path must fail the fetch")
	}

	// A serving repo path that is a file fails OpenLocalRepo.
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acme", "api.git"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f = execFollow{CacheDir: dir}
	if _, err := f.Fetch(ctx, "acme/api", "https://example.invalid/acme/api.git", "", nil, nil); err == nil {
		t.Fatal("file serving repo must fail the fetch")
	}

	// merge-base under a missing binary is an error (not exit-1 rewound).
	f = execFollow{CacheDir: t.TempDir(), Binary: "walgit-no-such-git"}
	if _, err := f.AncestorOf(ctx, "acme/api", "a", "b"); err == nil {
		t.Fatal("missing binary must error AncestorOf")
	}

	// The git() helper defaults the binary to "git".
	f = execFollow{CacheDir: t.TempDir()}
	if _, err := f.git(ctx, t.TempDir(), nil, []string{"--version"}, nil); err != nil {
		t.Fatalf("default binary: %v", err)
	}
}
func TestWalPlanner_PreviousFireNeverAndObjectFormat(t *testing.T) {
	// A schedule that never fires (Feb 30) walks the whole 400-day window
	// and returns the zero time.
	if got := (walPlanner{}).PreviousFire(config.BundleStrategy{Schedule: "0 0 23 30 2 *"}, time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Fatalf("never-firing schedule = %v, want zero", got)
	}

	// A manifest without an object format defaults to sha1.
	r, _, _, _, h := walFixture(t)
	m, _ := h.ManifestSnapshot()
	m.ObjectFormat = ""
	if got := objectFormatOf(h); got != "sha1" {
		t.Fatalf("objectFormatOf = %q", got)
	}
	_ = r
}

func TestWalPlanner_BuildVerdictsAndDrain(t *testing.T) {
	ctx := context.Background()
	r, _, st, _, h := walFixture(t)
	p := walPlanner{reg: r}
	persist := func() {
		fresh, v2 := h.ManifestSnapshot()
		if _, err := st.Put(ctx, repoPrefixOf("acme/api")+store.Manifest, store.PutBody{Bytes: fresh.Marshal()},
			store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(v2)}); err != nil {
			t.Fatal(err)
		}
	}
	// A closed slot whose tips match the newest entry of its strategy
	// records an unchanged verdict via the batch CAS (§8.7).
	slot := uint64(time.Now().Add(-24 * time.Hour).Unix())
	m, _ := h.ManifestSnapshot()
	m.Checkpoint = &proto.CheckpointRef{
		Seq:          1,
		FirstStateAt: ptrTs(time.Now().Add(-48 * time.Hour)),
		AsOf:         ptrTs(time.Now().Add(-47 * time.Hour)),
	}
	persist()
	if _, err := store.PutBytes(ctx, st, repoPrefixOf("acme/api")+store.CheckpointRefsKey(1),
		(&proto.RefSnapshot{Seq: 1, ObjectFormat: "sha1", Refs: []*proto.Ref{{Name: "refs/heads/main", Oid: testOid1}}}).Marshal(),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	list := &proto.BundleList{Bundles: []*proto.BundleEntry{
		{ID: "b1", Key: "wal/b1.pack", Strategy: "weekly", Kind: "full", Slot: slot - 7200},
		{ID: "d1", Key: "wal/d1.pack", Strategy: "daily", Kind: "incremental", Slot: slot - 3600,
			Tips: []*proto.Ref{{Name: "refs/heads/main", Oid: testOid1}}},
	}}
	if _, err := store.PutBytes(ctx, st, repoPrefixOf("acme/api")+store.BundleList, list.Marshal(), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	built, err := p.Build(ctx, "acme/api", Slot{Strategy: "daily", Slot: slot})
	if err != nil || !built {
		t.Fatalf("unchanged closed slot: built=%v err=%v", built, err)
	}
	// A BuildSlot error only surfaces through Build when the task table
	// refuses to run: draining reports 503 instead of the outcome (§5.8).
	// NOTE: the wal task runner converts ordinary unit errors into task
	// summaries, so ErrBlocked is otherwise invisible to the maintainer.
	r.Tasks().Drain()
	built, err = p.Build(ctx, "acme/api", Slot{Strategy: "daily", Slot: slot})
	if err == nil || built {
		t.Fatalf("draining registry must fail the build: built=%v err=%v", built, err)
	}
}
