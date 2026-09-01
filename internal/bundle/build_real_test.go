package bundle

// build_real_test.go — the git-backed build paths (§8.9) over a REAL bare repo:
// BuildSlot full + incremental, the §8.7 gates, buildAndPublish error arms,
// and the §8.9.4 compose-full path. Real git through the real Primitives
// binding (bind_git.go), fake WAL views, memory store.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// --- fixture -------------------------------------------------------------------

func wave4Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// wave4Repo builds a real bare serving repo: main at three commits. Returns
// the bare dir and the three commit oids (oldest first).
func wave4Repo(t *testing.T) (string, []string) {
	t.Helper()
	if _, err := exec.Command("git", "--version").CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	work := t.TempDir()
	wave4Git(t, work, "init", "-q", "-b", "main")
	var oids []string
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("f%d.txt", i)), []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		wave4Git(t, work, "add", "-A")
		wave4Git(t, work, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
		oids = append(oids, wave4Git(t, work, "rev-parse", "HEAD"))
	}
	bare := filepath.Join(t.TempDir(), "srv.git")
	wave4Git(t, t.TempDir(), "clone", "-q", "--bare", work, bare)
	wave4Git(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return bare, oids
}

// wave4Strats is the incremental gate fixture's strategy table: a full base
// plus the incremental under test.
func wave4Strats(t *testing.T, s *Strategy) []Strategy {
	return []Strategy{{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}, *s}
}

// errPrim is a fakePrim with injected failures.
type errPrim struct {
	fakePrim
	packErr   error
	bundleErr error
	countErr  error
}

func (p *errPrim) PackDelta(ctx context.Context, repoDir string, wants, excludes []string, filter string, w io.Writer) error {
	if p.packErr != nil {
		return p.packErr
	}
	return p.fakePrim.PackDelta(ctx, repoDir, wants, excludes, filter, w)
}

func (p *errPrim) BundleCreate(ctx context.Context, repoDir, outPath string, refs []string) (int64, int64, error) {
	if p.bundleErr != nil {
		return 0, 0, p.bundleErr
	}
	return p.fakePrim.BundleCreate(ctx, repoDir, outPath, refs)
}

func (p *errPrim) CountCommits(ctx context.Context, repoDir string, tips, notTips []string) (int, error) {
	if p.countErr != nil {
		return 0, p.countErr
	}
	return p.fakePrim.CountCommits(ctx, repoDir, tips, notTips)
}

// buildWrap fails selected puts with non-retryable errors.
type buildWrap struct {
	store.ObjectStore
	failObj  atomic.Int64 // next N puts of bundle objects → error
	failList atomic.Int64 // next N puts of bundles/list.pb → error
}

func (w *buildWrap) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == BundleListKey && w.failList.Load() > 0 {
		w.failList.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindOther, Key: key, Err: errors.New("list down")}
	}
	if key != BundleListKey && strings.HasPrefix(key, "bundles/") && w.failObj.Load() > 0 {
		w.failObj.Add(-1)
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindOther, Key: key, Err: errors.New("bucket down")}
	}
	return w.ObjectStore.Put(ctx, key, body, opts)
}

// --- bind_git primitives over real git --------------------------------------------

func TestWave4GitPrimitivesReal(t *testing.T) {
	bare, oids := wave4Repo(t)
	p := &GitPrimitives{L: git.NewLayer()}
	ctx := noCtx()

	// BundleCreate: writes a real bundle file, returns size + pack offset.
	out := filepath.Join(t.TempDir(), "b.bundle")
	size, off, err := p.BundleCreate(ctx, bare, out, []string{"refs/heads/main"})
	if err != nil || size <= 0 || off <= 0 {
		t.Fatalf("BundleCreate = %d, %d, %v", size, off, err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() != size {
		t.Fatalf("bundle file = %v, want size %d", err, size)
	}

	// PackDelta: streams a pack for wants-minus-excludes.
	var pack bytes.Buffer
	if err := p.PackDelta(ctx, bare, []string{oids[2]}, []string{oids[0]}, "", &pack); err != nil || pack.Len() == 0 {
		t.Fatalf("PackDelta = %d bytes, %v", pack.Len(), err)
	}
	if err := p.PackDelta(ctx, bare, []string{"deadbeef"}, nil, FilterBlobNone, &bytes.Buffer{}); err == nil {
		t.Fatal("PackDelta with a bad want must fail")
	}

	// CountCommits: commits in c3 but not in c1 → exactly 2 (c2, c3).
	n, err := p.CountCommits(ctx, bare, []string{oids[2]}, []string{oids[0]})
	if err != nil || n != 2 {
		t.Fatalf("CountCommits = %d, %v", n, err)
	}
	if n, err := p.CountCommits(ctx, bare, nil, nil); err != nil || n != 0 {
		t.Fatalf("CountCommits(empty) = %d, %v", n, err)
	}
	if _, err := p.CountCommits(ctx, bare, []string{"deadbeef"}, nil); err == nil {
		t.Fatal("CountCommits with a bad tip must fail")
	}

	// Binary resolution: layer config > WALGIT_GIT_BINARY > "git".
	t.Setenv("WALGIT_GIT_BINARY", "/custom/git")
	if got := gitBinary(); got != "/custom/git" {
		t.Fatalf("gitBinary = %q", got)
	}
	t.Setenv("WALGIT_GIT_BINARY", "")
	if got := gitBinary(); got != "git" {
		t.Fatalf("gitBinary default = %q", got)
	}
	binLayer := git.NewLayer()
	binLayer.Binary = "layer-git"
	if got := (&GitPrimitives{L: binLayer}).binary(); got != "layer-git" {
		t.Fatalf("layer binary = %q", got)
	}
	// tail keeps the last n bytes, or everything when shorter.
	if tail("abcdefgh", 3) != "fgh" || tail("ab", 5) != "ab" {
		t.Fatal("tail arms")
	}
}

// --- BuildSlot over real git ---------------------------------------------------------

func TestWave4BuildSlotFullRealGit(t *testing.T) {
	bare, oids := wave4Repo(t)
	slot := at("2026-03-10T00:00:00Z")
	fw := &fakeWal{asOf: map[time.Time]fold{slot: {refs: tipRefs(oids[2]), seq: 3}}}
	st := store.NewMemory()
	d := &Deps{Wal: fw, Prim: &GitPrimitives{L: git.NewLayer()}, St: st, RepoDir: bare,
		HostID: "host-a", List: protoBundleList()}
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}

	if err := BuildSlot(noCtx(), d, "o/r", []Strategy{*s}, s, slot); err != nil {
		t.Fatal(err)
	}
	list, _, err := storeGetList(t, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Bundles) != 1 {
		t.Fatalf("list = %+v, want one entry", list)
	}
	e := list.Bundles[0]
	if e.Kind != KindFull || e.Size == 0 || e.BaseID != "" || len(e.Tips) != 2 {
		t.Fatalf("entry = %+v", e)
	}
	for _, tip := range e.Tips {
		if tip.Oid != oids[2] {
			t.Fatalf("tip = %+v, want oid %s", tip, oids[2])
		}
	}
	body, _, err := store.GetBytes(noCtx(), st, e.Key, store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte("# v2 git bundle\n")) {
		t.Fatalf("bundle object prefix = %q", body[:min(len(body), 32)])
	}
}

func TestWave4BuildSlotIncrementalRealGit(t *testing.T) {
	bare, oids := wave4Repo(t)
	slotBase := at("2026-03-09T00:00:00Z")
	slot := at("2026-03-10T00:00:00Z")
	fw := &fakeWal{asOf: map[time.Time]fold{
		slotBase: {refs: tipRefs(oids[1]), seq: 2},
		slot:     {refs: tipRefs(oids[2]), seq: 3},
	}}
	base := entry("full", "full/"+FormatSlot(slotBase), KindFull, epoch(t, "2026-03-09T00:00:00Z"), epoch(t, "2026-03-09T00:00:00Z"), 2)
	for i := range base.Tips {
		base.Tips[i].Oid = oids[1]
	}
	ft := &fakeTasks{}
	st := store.NewMemory()
	d := &Deps{Wal: fw, Prim: &GitPrimitives{L: git.NewLayer()}, St: st, RepoDir: bare,
		HostID: "host-a", Tasks: ft, List: protoBundleList(base)}
	s := &Strategy{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily"), MinCommits: 1}

	if err := BuildSlot(noCtx(), d, "o/r", []Strategy{{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}, *s}, s, slot); err != nil {
		t.Fatal(err)
	}
	list, _, err := storeGetList(t, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Bundles) != 1 {
		t.Fatalf("list = %+v, want exactly the published incremental", list)
	}
	var e *proto.BundleEntry
	for _, b := range list.Bundles {
		if b.Strategy == "inc" {
			e = b
		}
	}
	if e == nil || e.Kind != KindIncremental || e.BaseID != base.ID || e.Seq != 2 {
		t.Fatalf("incremental entry = %+v", e)
	}
	body, _, err := store.GetBytes(noCtx(), st, e.Key, store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("-"+oids[1]+" \n")) {
		t.Fatal("incremental must carry the base oid as a prerequisite line")
	}
	// Task seam: the pass ran as a `bundle` task with narration.
	if len(ft.ran) != 1 || !strings.HasPrefix(ft.ran[0], "o/r/inc@20260310") {
		t.Fatalf("task runs = %v", ft.ran)
	}
	joined := strings.Join(ft.noted, "\n")
	if !strings.Contains(joined, "building inc slot 20260310") ||
		!strings.Contains(joined, "published "+e.ID) {
		t.Fatalf("narration = %q", joined)
	}
}

// --- gates + top-of-pipeline branches (fake primitives) -------------------------------

func wave4GateDeps(t *testing.T, prim Primitives, fw *fakeWal, list *proto.BundleList, now time.Time) *Deps {
	return &Deps{Wal: fw, Prim: prim, St: store.NewMemory(), RepoDir: "/unused",
		HostID: "h", List: list, Now: func() time.Time { return now }}
}

func TestWave4GatesVerdictArms(t *testing.T) {
	slot := at("2026-03-10T00:00:00Z")
	slotEpoch := epoch(t, "2026-03-10T00:00:00Z")
	prevSlot := epoch(t, "2026-03-09T00:00:00Z")
	s := &Strategy{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily"), MinCommits: 5}
	base := entry("full", "full/base", KindFull, prevSlot, prevSlot, 1)

	// a. Unchanged gate, closed slot → final verdict.
	fw := &fakeWal{asOf: map[time.Time]fold{slot: {refs: tipRefs("abc"), seq: 2}}}
	prev := entry("inc", "inc/prev", KindIncremental, prevSlot, prevSlot, 2)
	prev.Tips = tipsPtrs(tipRefs("abc"))
	d := wave4GateDeps(t, &fakePrim{counts: []int{9}}, fw, protoBundleList(base, prev), slot.Add(time.Hour))
	if err := BuildSlot(noCtx(), d, "o/r", wave4Strats(t, s), s, slot); err != nil {
		t.Fatal(err)
	}
	if len(d.Verdicts) != 1 || d.Verdicts[0].Reason != "unchanged since inc/prev" ||
		d.Verdicts[0].Slot != slotEpoch || d.Verdicts[0].BaseID != base.ID {
		t.Fatalf("verdicts = %+v", d.Verdicts)
	}

	// b. Unchanged gate, open slot → stop without a verdict.
	d = wave4GateDeps(t, &fakePrim{counts: []int{9}}, fw, protoBundleList(base, prev), slot.Add(30*time.Second))
	if err := BuildSlot(noCtx(), d, "o/r", wave4Strats(t, s), s, slot); err != nil {
		t.Fatal(err)
	}
	if len(d.Verdicts) != 0 {
		t.Fatalf("open slot must not record verdicts: %+v", d.Verdicts)
	}

	// c. Too-small gate, closed slot → verdict.
	fw = &fakeWal{asOf: map[time.Time]fold{slot: {refs: tipRefs("xyz"), seq: 3}}}
	d = wave4GateDeps(t, &fakePrim{counts: []int{1}}, fw, protoBundleList(base), slot.Add(time.Hour))
	if err := BuildSlot(noCtx(), d, "o/r", wave4Strats(t, s), s, slot); err != nil {
		t.Fatal(err)
	}
	if len(d.Verdicts) != 1 || !strings.HasPrefix(d.Verdicts[0].Reason, "too-small:") {
		t.Fatalf("verdicts = %+v", d.Verdicts)
	}

	// d. Gate not evaluable (count error) → proceed and publish.
	d = wave4GateDeps(t, &errPrim{fakePrim: fakePrim{counts: []int{9}}, countErr: errors.New("rev-list died")},
		fw, protoBundleList(base), slot.Add(time.Hour))
	if err := BuildSlot(noCtx(), d, "o/r", wave4Strats(t, s), s, slot); err != nil {
		t.Fatal(err)
	}
	list, _, err := storeGetList(t, d.St)
	if err != nil || len(list.Bundles) != 1 || list.Bundles[0].Strategy != "inc" {
		t.Fatalf("proceed-after-count-error: list = %+v, %v", list, err)
	}
}

func TestWave4BuildSlotTopBranches(t *testing.T) {
	slot := at("2026-03-10T00:00:00Z")
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}
	inc := &Strategy{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily")}

	// Unavailable: slot before first_state_at → notice, no verdict, no build.
	fw := &fakeWal{first: map[string]time.Time{"o/r": at("2026-03-11T00:00:00Z")}}
	d := wave4GateDeps(t, &fakePrim{}, fw, protoBundleList(), slot)
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err != nil {
		t.Fatal(err)
	}
	if len(d.Verdicts) != 0 {
		t.Fatalf("unavailable slot must record nothing: %+v", d.Verdicts)
	}

	// No state as of the slot → closed-slot verdict.
	fw = &fakeWal{}
	d = wave4GateDeps(t, &fakePrim{}, fw, protoBundleList(), slot)
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err != nil {
		t.Fatal(err)
	}
	if len(d.Verdicts) != 1 || d.Verdicts[0].Reason != "no state as of the slot" || d.Verdicts[0].BaseID != "" {
		t.Fatalf("verdicts = %+v", d.Verdicts)
	}

	// No refs selected (main_only over a dev-only tip set) → nothing to build.
	fw = &fakeWal{asOf: map[time.Time]fold{slot: {refs: Refs{{Name: "refs/heads/dev", Oid: "aa"}}, seq: 1}}}
	d = wave4GateDeps(t, &fakePrim{}, fw, protoBundleList(), slot)
	d.MainOnly = true
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err != nil {
		t.Fatal(err)
	}

	// Incremental with no resolvable base → ErrBlocked.
	fw = &fakeWal{asOf: map[time.Time]fold{slot: {refs: tipRefs("abc"), seq: 1}}}
	d = wave4GateDeps(t, &fakePrim{}, fw, protoBundleList(), slot)
	if err := BuildSlot(noCtx(), d, "o/r", wave4Strats(t, inc), inc, slot); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}

// --- buildAndPublish error arms -------------------------------------------------------

func TestWave4BuildSlotErrorArms(t *testing.T) {
	bare, oids := wave4Repo(t)
	_ = bare
	slot := at("2026-03-10T00:00:00Z")
	tips := tipRefs(oids[2])
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@daily")}
	inc := &Strategy{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily")}
	base := entry("full", "full/base", KindFull, epoch(t, "2026-03-09T00:00:00Z"), epoch(t, "2026-03-09T00:00:00Z"), 1)

	// CacheDir that cannot host temp files → mkdir error.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fw := &fakeWal{asOf: map[time.Time]fold{slot: {refs: tips, seq: 1}}}
	d := wave4GateDeps(t, &fakePrim{}, fw, protoBundleList(), slot)
	d.CacheDir = blocker
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err == nil {
		t.Fatal("bad CacheDir must fail")
	}

	// PackDelta failure on the incremental path.
	fw = &fakeWal{asOf: map[time.Time]fold{slot: {refs: tips, seq: 2}}}
	d = wave4GateDeps(t, &errPrim{fakePrim: fakePrim{counts: []int{30}}, packErr: errors.New("pack-objects died")}, fw, protoBundleList(base), slot)
	if err := BuildSlot(noCtx(), d, "o/r", wave4Strats(t, inc), inc, slot); err == nil || !strings.Contains(err.Error(), "pack-objects") {
		t.Fatalf("err = %v", err)
	}

	// git bundle create failure on the full path.
	d = wave4GateDeps(t, &errPrim{fakePrim: fakePrim{}, bundleErr: errors.New("bundle create died")}, fw, protoBundleList(), slot)
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err == nil || !strings.Contains(err.Error(), "git bundle create") {
		t.Fatalf("err = %v", err)
	}

	// Object upload failure (non-412) fails the build.
	d = wave4GateDeps(t, &GitPrimitives{L: git.NewLayer()}, fw, protoBundleList(), slot)
	d.RepoDir = bare
	wst := &buildWrap{ObjectStore: store.NewMemory()}
	wst.failObj.Store(1)
	d.St = wst
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err == nil || !strings.Contains(err.Error(), "put ") {
		t.Fatalf("err = %v", err)
	}

	// List CAS failure after a successful upload fails the build.
	d = wave4GateDeps(t, &GitPrimitives{L: git.NewLayer()}, fw, protoBundleList(), slot)
	d.RepoDir = bare
	wst = &buildWrap{ObjectStore: store.NewMemory()}
	wst.failList.Store(1)
	d.St = wst
	if err := BuildSlot(noCtx(), d, "o/r", nil, s, slot); err == nil || !strings.Contains(err.Error(), "list cas") {
		t.Fatalf("err = %v", err)
	}
}

// --- compose full (§8.9.4) -------------------------------------------------------------

func wave4ComposeFixture(t *testing.T) (*Deps, *fakeWal, *proto.BundleEntry, string) {
	t.Helper()
	_, oids := wave4Repo(t)
	slot := at("2026-03-10T00:00:00Z")
	fw := &fakeWal{atSeq: map[uint64]fold{7: {refs: tipRefs(oids[2]), seq: 7}}}
	base := entry("full", "full/old", KindFull, epoch(t, "2026-03-03T00:00:00Z"), epoch(t, "2026-03-03T00:00:00Z"), 7)
	base.Key = "wal/deadbeefcafe.pack"
	packPath := filepath.Join(t.TempDir(), "deadbeefcafe.pack")
	if err := os.WriteFile(packPath, []byte("FAKEPACKBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	if _, err := st.Put(noCtx(), base.Key, store.PutBody{Bytes: []byte("FAKEPACKBYTES")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	d := &Deps{Wal: fw, St: st, Now: func() time.Time { return slot }}
	return d, fw, base, packPath
}

func TestWave4ComposeFullSuccess(t *testing.T) {
	d, _, base, packPath := wave4ComposeFixture(t)
	slot := at("2026-03-10T00:00:00Z")
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@weekly")}

	e, err := d.composeFull(noCtx(), "o/r", s, slot, base, packPath)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindFull || e.Seq != 7 || e.BaseID != "" || e.Size == 0 {
		t.Fatalf("entry = %+v", e)
	}
	body, _, err := store.GetBytes(noCtx(), d.St, e.Key, store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte("# v2 git bundle\n")) || !bytes.Contains(body, []byte("FAKEPACKBYTES")) {
		t.Fatal("composed object must be header ∘ base pack")
	}
	// The scratch header was deleted after composition.
	if _, _, err := store.GetBytes(noCtx(), d.St, composeScratchKey("o/r", "full", slot), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("scratch get err = %v, want NotFound", err)
	}
	// The composed entry landed in the list.
	list, _, err := storeGetList(t, d.St)
	if err != nil || len(list.Bundles) != 1 || list.Bundles[0].ID != e.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestWave4ComposeFullErrorArms(t *testing.T) {
	_, fw, base, packPath := wave4ComposeFixture(t)
	slot := at("2026-03-10T00:00:00Z")
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@weekly")}
	now := func() time.Time { return slot }

	// RefsAtSeq failure: no refs at the base seq.
	if _, err := (&Deps{Wal: &fakeWal{}, St: store.NewMemory(), Now: now}).composeFull(noCtx(), "o/r", s, slot, base, packPath); err == nil || !strings.Contains(err.Error(), "refs at base seq") {
		t.Fatalf("err = %v", err)
	}

	// Base pack file missing.
	d := &Deps{Wal: fw, St: store.NewMemory(), Now: now}
	if _, err := d.composeFull(noCtx(), "o/r", s, slot, base, filepath.Join(t.TempDir(), "absent.pack")); err == nil || !strings.Contains(err.Error(), "base pack") {
		t.Fatalf("err = %v", err)
	}

	// Compose generic failure (after a successful scratch put).
	failSt := &wave4Store{ObjectStore: store.NewMemory()}
	failSt.composeErr.Store(1)
	d.St = failSt
	if _, err := d.composeFull(noCtx(), "o/r", s, slot, base, packPath); err == nil || !strings.Contains(err.Error(), "compose") {
		t.Fatalf("err = %v", err)
	}
}

func TestWave4ComposeFullLostRace(t *testing.T) {
	d, _, base, packPath := wave4ComposeFixture(t)
	slot := at("2026-03-10T00:00:00Z")
	s := &Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@weekly")}
	wst := &wave4Store{ObjectStore: d.St}
	wst.composeFail.Store(1) // another host won the Create race with identical bytes
	d.St = wst
	e, err := d.composeFull(noCtx(), "o/r", s, slot, base, packPath)
	if err != nil {
		t.Fatalf("lost-race compose must succeed: %v", err)
	}
	if e.Key == "" {
		t.Fatal("entry key empty")
	}
}

func TestWave4ComposeHelpers(t *testing.T) {
	if o, n := splitRepo("owner/repo"); o != "owner" || n != "repo" {
		t.Fatalf("splitRepo = %q, %q", o, n)
	}
	if o, n := splitRepo("bare"); o != "bare" || n != "" {
		t.Fatalf("splitRepo(bare) = %q, %q", o, n)
	}
	slot := at("2026-03-10T00:00:00Z")
	if got := composeScratchKey("o/r", "full", slot); got != "wal/_compose/o/r/full-20260310T000000Z.header" {
		t.Fatalf("scratch key = %q", got)
	}
	base := &proto.BundleEntry{Key: "wal/abc123.pack"}
	if got := baseChecksum(base); got != "abc123" {
		t.Fatalf("baseChecksum = %q", got)
	}
	base.Key = "bundles/full/x-y.bundle"
	if got := baseChecksum(base); got != "x-y" {
		t.Fatalf("baseChecksum(bundle) = %q", got)
	}
	base.Key = "plainkey"
	if got := baseChecksum(base); got != "plainkey" {
		t.Fatalf("baseChecksum(plain) = %q", got)
	}
	base.Key = "wal/seq7.pack"
	if got := baseSeqOrZero(base); got != 0 {
		t.Fatalf("baseSeqOrZero = %d", got)
	}
	base.Seq = 7
	if got := baseSeqOrZero(base); got != 7 || baseSeqOrZero(nil) != 0 {
		t.Fatal("baseSeqOrZero arms")
	}
	if refNames(Refs{{Name: "b"}, {Name: "a"}})[0] != "b" {
		t.Fatal("refNames order")
	}
}
