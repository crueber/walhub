// edge3_test.go — final-pass coverage: connectivity through the copier,
// local-upstream repair/follow fetches, and remaining single-branch edges.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upstreamFixture builds a non-bare git work tree with one commit on main;
// its .git serves fetches over the local path.
func upstreamFixture(t *testing.T) string {
	t.Helper()
	up := t.TempDir()
	mk := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = up
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	mk("init", "-q", "-b", "main", ".")
	mk("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(up+"/f.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk("add", ".")
	mk("commit", "-m", "c1")
	return up
}

func TestConnectivityDanglingTipCopiesLines(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := t.TempDir()
	mk := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = up
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	mk("init", "-q", "-b", "main", ".")
	os.WriteFile(up+"/a.txt", []byte("one"), 0o644)
	mk("add", ".")
	mk("commit", "-m", "c1")
	os.WriteFile(up+"/b.txt", []byte("two"), 0o644)
	mk("add", ".")
	mk("commit", "-m", "c2")
	mk("branch", "topic")
	os.WriteFile(up+"/c.txt", []byte("three"), 0o644)
	mk("add", ".")
	mk("commit", "-m", "c3-on-topic") // topic tip differs from main
	head := Oid(gitRevParse(t, up, "HEAD"))
	topic := Oid(gitRevParse(t, up, "topic"))

	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "dang"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	var packBuf bytes.Buffer
	pc := exec.Command("git", "pack-objects", "--stdout", "--revs", "--all")
	pc.Dir = up
	pc.Stdout = &packBuf
	if err := pc.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	if _, err := l.Ingest(t.Context(), repo, bytes.NewReader(packBuf.Bytes()), 0, false, true); err != nil {
		t.Fatal(err)
	}
	// Snapshot ONLY main: the topic commit stays in the pack but unreferenced,
	// so rev-list --objects <topic> --not --all emits lines for the copier.
	if err := l.LoadSnapshot(repo, []RefEntry{
		{Name: "refs/heads/main", Oid: head},
	}, "refs/heads/main", head); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{topic}); err != nil {
		t.Fatalf("dangling tip connectivity: %v", err)
	}
}

func TestFetchObjectsAsPackLocalUpstream(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := upstreamFixture(t)
	oid := Oid(gitRevParse(t, up, "HEAD"))

	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "repair"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	packPath, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{URL: up}, []Oid{oid})
	if err != nil {
		t.Fatalf("repair fetch: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(packPath), "pack-") {
		t.Fatalf("pack path = %q", packPath)
	}
	if _, err := os.Stat(packPath); err != nil {
		t.Fatalf("pack missing: %v", err)
	}
	// Every requested oid is verified present before install — a bogus want
	// must surface as MissingObjectError after the fetch succeeds for real
	// objects only.
	bogus := Oid(strings.Repeat("c", 40))
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{URL: up}, []Oid{oid, bogus}); err == nil {
		t.Fatal("bogus want must surface as missing")
	}
}

func TestFollowFetchLocalUpstream(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := upstreamFixture(t)
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "follow"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	tips, err := l.FollowFetch(t.Context(), root, repo.ID, UpstreamSpec{URL: up}, []string{"refs/heads/main"})
	if err != nil {
		t.Fatalf("follow fetch: %v", err)
	}
	if got, ok := tips["refs/heads/main"]; !ok || got == "" {
		t.Fatalf("tips = %v", tips)
	}
}

func TestNextSuffixForcedCollision(t *testing.T) {
	ingestMu.Lock()
	prev := time.Now().UnixNano() + int64(time.Hour)
	lastNanos = prev
	ingestMu.Unlock()
	if n := nextSuffix(); n != prev+1 {
		t.Fatalf("collision branch must return lastNanos+1: %d != %d", n, prev+1)
	}
}

// ---- single-branch edges --------------------------------------------------------------

func TestValidateRefUpdateInvalidNewOid(t *testing.T) {
	err := ValidateRefUpdate(&RefUpdate{Name: "refs/heads/x", OldOid: zero40, NewOid: "zz"})
	if err == nil {
		t.Fatal("invalid new oid must be refused")
	}
}

func TestApplyRefTxnLooseOnlyConflict(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "loosetxn"}, Sha1)
	l := NewLayer()
	oid := gitBlob(t, repo, "x")
	// A loose ref holding the zero oid is invisible to the snapshot (loose
	// refs skip zero values) but PeekRef still sees it — the create is
	// refused as a conflict via the loose fallback.
	zeroPath := filepath.Join(repo.Path, "refs", "heads", "zref")
	if err := os.MkdirAll(filepath.Dir(zeroPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zeroPath, []byte(zero40), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := PeekRef(repo, "refs/heads/zref"); !ok {
		t.Fatal("PeekRef must see the zero-oid loose ref")
	}
	err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/zref", OldOid: zero40, NewOid: oid},
	}, true)
	var ce *RefConflictError
	if err == nil || !errors.As(err, &ce) {
		t.Fatalf("zero-oid loose ref = %v, want RefConflictError", err)
	}
}

func TestValidateRefNameViaUpdatePaths(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "refv"}, Sha1)
	l := NewLayer()
	// An invalid oid on old AND new positions, plus a symbolic target on a
	// non-HEAD ref, each refuse before any subprocess.
	for _, txn := range [][]RefUpdate{
		{{Name: "refs/heads/x", OldOid: "nothex", NewOid: oidA}},
		{{Name: "refs/heads/x", OldOid: zero40, NewOid: "zz"}},
		{{Name: "refs/heads/x", OldOid: zero40, NewOid: oidA, NewSymbolicTarget: "refs/heads/main"}},
	} {
		if err := l.ApplyRefTxn(t.Context(), repo, txn, false); err == nil {
			t.Fatalf("txn %+v accepted", txn)
		}
	}
}

func TestLoadSnapshotHeadTargetOnFreshRepo(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "freshhead"}, Sha1)
	l := NewLayer()
	refs := []RefEntry{{Name: "refs/heads/main", Oid: Oid(oidA)}}
	if err := l.LoadSnapshot(repo, refs, "refs/heads/main", Oid(oidA)); err != nil {
		t.Fatalf("symbolic load: %v", err)
	}
	// readHead reports the symbolic target; the resolved oid lives in the
	// snapshot, not the HEAD file.
	if target, oid := readHead(repo); target != "refs/heads/main" || oid != "" {
		t.Fatalf("head = %q/%q", target, oid)
	}

	// RemoveAll failure: an unremovable entry under refs/ surfaces the error.
	sub := filepath.Join(repo.Path, "refs", "heads", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deny(t, filepath.Join(repo.Path, "refs"))
	if err := l.LoadSnapshot(repo, refs, "refs/heads/main", Oid(oidA)); err == nil {
		t.Fatal("unremovable refs tree must fail load")
	}
}

func TestKeepPackArgv(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// The keepPacks loop appends --keep-pack argv; use a real pack name.
	idxs, _ := idxSet(repo)
	var keep []string
	for name := range idxs {
		keep = append(keep, name)
	}
	if _, err := l.GeometricRepack(t.Context(), repo, 2, false, keep); err != nil {
		t.Fatalf("geometric with keep-pack: %v", err)
	}
	if _, err := l.FullRepack(t.Context(), repo, keep); err != nil {
		t.Fatalf("full with keep-pack: %v", err)
	}
}

func TestRepackIdxSetErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	deny(t, repo.PackDir())
	if _, err := l.GeometricRepack(t.Context(), repo, 2, false, nil); err == nil {
		t.Fatal("unreadable pack dir must fail geometric repack")
	}
	if _, err := l.FullRepack(t.Context(), repo, nil); err == nil {
		t.Fatal("unreadable pack dir must fail full repack")
	}
}

func TestRemoveKeepMarkersError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	// A non-empty .keep DIRECTORY cannot be os.Remove'd → surfaces the error.
	keepDir := filepath.Join(repo.PackDir(), "pack-x.keep")
	if err := os.MkdirAll(filepath.Join(keepDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "inner", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeKeepMarkers(repo); err == nil {
		t.Fatal("unremovable keep marker must error")
	}
}

func TestWriteHistoryMidxError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.WriteHistoryMidx(ctx, repo, []string{"pack-a.idx"}, "pack-a.idx"); err == nil {
		t.Fatal("canceled midx write must fail")
	}
}

func TestStagePackTempError(t *testing.T) {
	l := NewLayer()
	dir := t.TempDir()
	deny(t, dir)
	t.Setenv("TMPDIR", dir)
	if _, err := l.stagePack(strings.NewReader("x"), 0); err == nil {
		t.Fatal("unwritable TMPDIR must fail staging")
	}
}

func TestIngestUnreadableConfig(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	deny(t, filepath.Join(repo.Path, "config"))
	if _, err := l.Ingest(t.Context(), repo, strings.NewReader("x"), 0, false, false); err == nil {
		t.Fatal("unreadable repo config must fail ingest")
	}
}

func TestIdxObjectCountShortHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hdr.idx")
	if err := os.WriteFile(p, []byte{0xff, 't', 'O'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idxObjectCount(p); err == nil {
		t.Fatal("truncated header must error")
	}
}

func TestIdxContainsTruncatedAndMiss(t *testing.T) {
	dir := t.TempDir()
	makeName := func(fill byte) []byte {
		name := make([]byte, 20)
		for i := range name {
			name[i] = fill
		}
		return name
	}
	// fanout[0xab] = 2: an oid starting 0xab searches two entries, but only
	// one is present → the truncated check fires.
	var b []byte
	b = append(b, 0xff, 't', 'O', 'c', 2, 0, 0, 0)
	for i := 0; i < 256; i++ {
		w := uint32(0)
		if i == 0xab {
			w = 2
		}
		b = append(b, byte(w>>24), byte(w>>16), byte(w>>8), byte(w))
	}
	stored := append([]byte{0xab}, makeName(0xcd)...) // one entry, different oid
	b = append(b, stored...)
	p := filepath.Join(dir, "trunc.idx")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("ab", 20)
	if _, err := idxContains(p, oid); err == nil {
		t.Fatal("truncated idx must error")
	}

	// A well-formed idx whose stored name shares only the prefix answers
	// "not contained" without error.
	var c []byte
	c = append(c, 0xff, 't', 'O', 'c', 2, 0, 0, 0)
	for i := 0; i < 256; i++ {
		w := uint32(0)
		if i == 0xab {
			w = 1
		}
		c = append(c, byte(w>>24), byte(w>>16), byte(w>>8), byte(w))
	}
	c = append(c, makeName(0xac)...) // different second byte
	okPath := filepath.Join(dir, "one.idx")
	if err := os.WriteFile(okPath, c, 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := idxContains(okPath, oid)
	if err != nil || found {
		t.Fatalf("prefix-only match = %v %v", found, err)
	}
}

func TestRunPipedCopyError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	out := errWriter{}
	if _, err := l.runPiped(t.Context(), repo, []string{"rev-parse", "HEAD"}, "version=0", strings.NewReader(""), out); err == nil {
		t.Fatal("erroring consumer must surface")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("pipe broken") }

func TestLsRefsPeeledAndDetachedHead(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// Peel flag over a repo with an annotated tag emits the peeled line.
	out, err := l.LsRefs(repo, LsRefsArgs{Peel: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "peeled:") {
		t.Fatalf("tag peel line missing: %q", out)
	}
	// Detached HEAD (oid in HEAD, no target) advertises the oid.
	detached := gitRevParse(t, repo.Path, "refs/heads/main")
	if err := os.WriteFile(filepath.Join(repo.Path, "HEAD"), []byte(detached+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.cache().mu.Lock()
	repo.cache().base = nil
	repo.cache().mu.Unlock()
	out, err = l.LsRefs(repo, LsRefsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), detached+" HEAD") {
		t.Fatalf("detached HEAD advert = %q", out)
	}
}

func TestLsRefsPrefixLowerBoundMiss(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// A prefix past every ref name exercises the lower-bound continue/break.
	out, err := l.LsRefs(repo, LsRefsArgs{Prefixes: []string{"refs/zzz/"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "refs/") {
		t.Fatalf("non-matching prefix advertised refs: %q", out)
	}
}

func TestLsRefsSnapshotError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "lserr"}, Sha1)
	l := NewLayer()
	if err := os.Mkdir(filepath.Join(repo.Path, "packed-refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.LsRefs(repo, LsRefsArgs{}); err == nil {
		t.Fatal("unreadable refs must fail ls-refs")
	}
}

func TestReadAllPktsPropagatesParseError(t *testing.T) {
	// ParseShallow over a malformed stream hits the ReadAllPkts error path.
	if got := ParseShallow([]byte("zzzz")); got != nil {
		t.Fatalf("shallow = %v", got)
	}
}

// forceNextSuffix pins the ingest nanos counter to a future value so the next
// call returns a predictable scratch suffix.
func forceNextSuffix() int64 {
	ingestMu.Lock()
	lastNanos = time.Now().UnixNano() + int64(time.Hour)
	predicted := lastNanos + 1
	ingestMu.Unlock()
	return predicted
}

func TestIngestScratchObstructions(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	pack := strings.NewReader("x")

	// Scratch refs/ as a FILE: the second MkdirAll fails.
	n1 := forceNextSuffix()
	scratch1 := filepath.Join(repo.Path, fmt.Sprintf("walgit-ingest-%d-%d", os.Getpid(), n1))
	if err := os.MkdirAll(scratch1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch1, "refs"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ingest(t.Context(), repo, pack, 0, false, false); err == nil {
		t.Fatal("file refs/ must fail ingest")
	}

	// Scratch HEAD as a DIRECTORY: writeHeadSeed fails.
	n2 := forceNextSuffix()
	scratch2 := filepath.Join(repo.Path, fmt.Sprintf("walgit-ingest-%d-%d", os.Getpid(), n2))
	if err := os.MkdirAll(filepath.Join(scratch2, "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ingest(t.Context(), repo, pack, 0, false, false); err == nil {
		t.Fatal("dir HEAD must fail ingest")
	}

	// Scratch objects/info as a FILE: the third MkdirAll fails.
	n3 := forceNextSuffix()
	scratch3 := filepath.Join(repo.Path, fmt.Sprintf("walgit-ingest-%d-%d", os.Getpid(), n3))
	if err := os.MkdirAll(filepath.Join(scratch3, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch3, "objects", "info"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ingest(t.Context(), repo, pack, 0, false, false); err == nil {
		t.Fatal("file objects/info must fail ingest")
	}

	// Scratch objects/info/alternates as a DIRECTORY: the write fails.
	n4 := forceNextSuffix()
	scratch4 := filepath.Join(repo.Path, fmt.Sprintf("walgit-ingest-%d-%d", os.Getpid(), n4))
	if err := os.MkdirAll(filepath.Join(scratch4, "objects", "info", "alternates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ingest(t.Context(), repo, pack, 0, false, false); err == nil {
		t.Fatal("dir alternates must fail ingest")
	}
}

func TestHistoryPackScratchObstructions(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)

	n1 := forceNextSuffix()
	scratch1 := filepath.Join(repo.Path, fmt.Sprintf("walgit-history-%d-%d", os.Getpid(), n1))
	if err := os.MkdirAll(scratch1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch1, "refs"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HistoryPack(t.Context(), repo, "base"); err == nil {
		t.Fatal("file refs/ must fail history pack")
	}

	n2 := forceNextSuffix()
	scratch2 := filepath.Join(repo.Path, fmt.Sprintf("walgit-history-%d-%d", os.Getpid(), n2))
	if err := os.MkdirAll(filepath.Join(scratch2, "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HistoryPack(t.Context(), repo, "base"); err == nil {
		t.Fatal("dir HEAD must fail history pack")
	}

	n3 := forceNextSuffix()
	scratch3 := filepath.Join(repo.Path, fmt.Sprintf("walgit-history-%d-%d", os.Getpid(), n3))
	if err := os.MkdirAll(filepath.Join(scratch3, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch3, "objects", "info"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HistoryPack(t.Context(), repo, "base"); err == nil {
		t.Fatal("file objects/info must fail history pack")
	}

	n4 := forceNextSuffix()
	scratch4 := filepath.Join(repo.Path, fmt.Sprintf("walgit-history-%d-%d", os.Getpid(), n4))
	if err := os.MkdirAll(filepath.Join(scratch4, "objects", "info", "alternates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HistoryPack(t.Context(), repo, "base"); err == nil {
		t.Fatal("dir alternates must fail history pack")
	}
}

func TestFetchObjectsAsPackScratchObstructions(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	oids := []Oid{Oid(oidA)}

	n1 := forceNextSuffix()
	scratch1 := filepath.Join(repo.Path, fmt.Sprintf("walgit-repair-%d-%d", os.Getpid(), n1))
	if err := os.MkdirAll(scratch1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch1, "refs"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{}, oids); err == nil {
		t.Fatal("file refs/ must fail repair fetch")
	}

	n2 := forceNextSuffix()
	scratch2 := filepath.Join(repo.Path, fmt.Sprintf("walgit-repair-%d-%d", os.Getpid(), n2))
	if err := os.MkdirAll(filepath.Join(scratch2, "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{}, oids); err == nil {
		t.Fatal("dir HEAD must fail repair fetch")
	}

	n3 := forceNextSuffix()
	scratch3 := filepath.Join(repo.Path, fmt.Sprintf("walgit-repair-%d-%d", os.Getpid(), n3))
	if err := os.MkdirAll(filepath.Join(scratch3, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch3, "objects", "info"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{}, oids); err == nil {
		t.Fatal("file objects/info must fail repair fetch")
	}

	n4 := forceNextSuffix()
	scratch4 := filepath.Join(repo.Path, fmt.Sprintf("walgit-repair-%d-%d", os.Getpid(), n4))
	if err := os.MkdirAll(filepath.Join(scratch4, "objects", "info", "alternates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{}, oids); err == nil {
		t.Fatal("dir alternates must fail repair fetch")
	}
}

func TestLoadSnapshotHeadTargetError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "badhead"}, Sha1)
	l := NewLayer()
	// An invalid symbolic target fails the direct HEAD write.
	if err := l.LoadSnapshot(repo, nil, "bad name", Oid(oidA)); err == nil {
		t.Fatal("invalid head target must fail")
	}

	// A detached HEAD write fails when HEAD is a directory.
	repo2, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "dirhead"}, Sha1)
	if err := os.Remove(filepath.Join(repo2.Path, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo2.Path, "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.LoadSnapshot(repo2, nil, "", Oid(oidA)); err == nil {
		t.Fatal("dir HEAD must fail detached write")
	}
}

func TestDiffIdxUnreadablePackDir(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	deny(t, repo.PackDir())
	if _, err := diffIdx(map[string]bool{}, repo); err == nil {
		t.Fatal("unreadable pack dir must fail diffIdx")
	}
}

func TestRunPipedCanceledContext(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := l.runPiped(ctx, repo, []string{"rev-parse", "HEAD"}, "version=0", strings.NewReader(""), &discardWriter{})
	if err == nil {
		t.Fatal("canceled runPiped must fail")
	}
}

func TestNextPayloadReadError(t *testing.T) {
	// Four valid length bytes then a hard reader error: the payload read
	// surfaces the non-EOF error.
	r := &seqErrReader{head: []byte("0009ab")}
	if _, _, err := NewPktReader(r).Next(); err == nil {
		t.Fatal("payload read error must surface")
	}
}

type seqErrReader struct {
	head []byte
	done bool
}

func (r *seqErrReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.head), nil
	}
	return 0, errors.New("reader gone")
}

func TestIdxContainsBadOidOnFileAndSha256(t *testing.T) {
	dir := t.TempDir()
	// Bad oid against an existing file surfaces the input error.
	p := filepath.Join(dir, "x.idx")
	if err := os.WriteFile(p, []byte("junkjunkjunkjunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idxContains(p, strings.Repeat("z", 128)); err == nil {
		t.Fatal("bad oid length must error")
	}
	// A 64-hex oid uses the 32-byte entry length.
	var b []byte
	b = append(b, 0xff, 't', 'O', 'c', 2, 0, 0, 0)
	for i := 0; i < 256; i++ {
		w := uint32(0)
		if i == 0xab {
			w = 1
		}
		b = append(b, byte(w>>24), byte(w>>16), byte(w>>8), byte(w))
	}
	name := make([]byte, 32)
	for i := range name {
		name[i] = 0xab
	}
	b = append(b, name...)
	p2 := filepath.Join(dir, "sha256.idx")
	if err := os.WriteFile(p2, b, 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := idxContains(p2, strings.Repeat("ab", 32))
	if err != nil || !found {
		t.Fatalf("sha256 lookup = %v %v", found, err)
	}
}

func TestPeelDeepChainUnpeelable(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	mk := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = repo.Path
		c.Env = append(os.Environ(), "GIT_DIR="+repo.Path)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	// An 18-deep annotated-tag chain exceeds maxTagHops.
	mk("tag", "-a", "deep1", "refs/tags/v1", "-m", "d1")
	for i := 2; i <= 18; i++ {
		mk("tag", "-a", fmt.Sprintf("deep%d", i), fmt.Sprintf("refs/tags/deep%d", i-1), "-m", fmt.Sprintf("d%d", i))
	}
	if _, ok := l.Peel(t.Context(), repo, Oid(gitRevParse(t, repo.Path, "refs/tags/deep18"))); ok {
		t.Fatal("an 18-deep chain is unpeelable")
	}
}

func TestFollowFetchScratchObstructions(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := upstreamFixture(t)
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "follow"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()

	// First run succeeds and creates the follow scratch.
	if _, err := l.FollowFetch(t.Context(), root, repo.ID, UpstreamSpec{URL: up}, []string{"refs/heads/main"}); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	followDir := filepath.Join(root, "follow", "o", "follow.git")

	// A directory where alternates must be written fails the write.
	os.Remove(filepath.Join(followDir, "objects", "info", "alternates")) //nolint:errcheck
	if err := os.MkdirAll(filepath.Join(followDir, "objects", "info", "alternates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FollowFetch(t.Context(), root, repo.ID, UpstreamSpec{URL: up}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("dir alternates must fail follow fetch")
	}
	os.Remove(filepath.Join(followDir, "objects", "info", "alternates")) //nolint:errcheck

	// A bogus upstream with a healthy serving repo fails at the fetch step.
	if _, err := l.FollowFetch(t.Context(), root, repo.ID, UpstreamSpec{URL: "https://127.0.0.1:1/nope.git"}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("bogus upstream must fail follow fetch")
	}
}

func TestFollowFetchInitErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "follow2"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	id := repo.ID

	// A read-only follow/ parent (traversable but not writable) fails the
	// follow-scratch init after the absence check.
	follow := filepath.Join(root, "follow")
	if err := os.MkdirAll(follow, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FollowFetch(t.Context(), root, id, UpstreamSpec{URL: "up"}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("unwritable follow dir must fail init")
	}

	// objects/info as a FILE inside an existing scratch fails the mkdir.
	os.Chmod(follow, 0o755) //nolint:errcheck
	os.RemoveAll(follow)    //nolint:errcheck
	fd := filepath.Join(follow, "o", "follow2.git", "objects")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fd, "info"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FollowFetch(t.Context(), root, id, UpstreamSpec{URL: "up"}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("file objects/info must fail follow init")
	}
}

func TestCopyLastChainLayerSideWriteAndInstallErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	graphDir := filepath.Join(repo.Path, "objects", "info", "commit-graphs")
	if err := os.MkdirAll(graphDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphDir, "graph-abc.graph"), []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphDir, "commit-graph-chain"), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sideDir where the side-file path is a DIRECTORY fails the write.
	sd := filepath.Join(t.TempDir(), "side")
	if err := os.MkdirAll(filepath.Join(sd, "abc.commit-graph"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := copyLastChainLayer(repo, sd); err == nil {
		t.Fatal("dir side-file must fail copy")
	}

	// InstallCommitGraphBase with a checksum containing a slash fails the write.
	if err := InstallCommitGraphBase(repo, "a/b", []byte("g")); err == nil {
		t.Fatal("slash checksum must fail install")
	}
}

func TestHistoryPackUnwritableRepo(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// Readable but not writable: the snapshot loads, the scratch cannot be
	// created.
	if err := os.Chmod(repo.Path, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(repo.Path, 0o700) }) //nolint:errcheck
	if _, err := l.HistoryPack(t.Context(), repo, "base"); err == nil {
		t.Fatal("unwritable repo must fail history pack scratch")
	}
}

func TestInitLocalRepoPackDirFile(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	pre := filepath.Join(root, "o4", "x.git", "objects", "pack")
	if err := os.MkdirAll(filepath.Dir(pre), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pre, []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InitLocalRepo(root, RepoId{Owner: "o4", Name: "x"}, Sha1); err == nil {
		t.Fatal("pack-dir file must fail init")
	}
}

func TestFetchObjectsAsPackDeniedPackDir(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := upstreamFixture(t)
	oid := Oid(gitRevParse(t, up, "HEAD"))
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "denypack"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	deny(t, repo.PackDir())
	// The full repair pipeline succeeds upstream, but the final install
	// cannot write into the pack dir.
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{URL: up}, []Oid{oid}); err == nil {
		t.Fatal("unwritable pack dir must fail install")
	}
}

func TestIngestReaderError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	if _, err := l.Ingest(t.Context(), repo, errReaderAt{}, 0, false, false); err == nil {
		t.Fatal("failing pack reader must fail ingest")
	}
}

func TestTagObjectTargetSkipsTrailingLines(t *testing.T) {
	// The object header must appear before the first empty line.
	if _, ok := tagObjectTarget([]byte("type commit\n\nobject " + oidA + "\n")); ok {
		t.Fatal("header after the blank line must be ignored")
	}
}

func TestFullRepackKeepMarkerError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	keepDir := filepath.Join(repo.PackDir(), "pack-x.keep")
	if err := os.MkdirAll(filepath.Join(keepDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "inner", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FullRepack(t.Context(), repo, nil); err == nil {
		t.Fatal("unremovable keep marker must fail full repack")
	}
}

func TestPeelChainCacheHits(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	mk := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = repo.Path
		c.Env = append(os.Environ(), "GIT_DIR="+repo.Path)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	// Build a chain deeper than maxTagHops: walking it memoizes
	// deepN → deepN-1 for the deepest 16 hops.
	mk("tag", "-a", "deep1", "refs/tags/v1", "-m", "d1")
	for i := 2; i <= 18; i++ {
		mk("tag", "-a", fmt.Sprintf("deep%d", i), fmt.Sprintf("refs/tags/deep%d", i-1), "-m", fmt.Sprintf("d%d", i))
	}
	if _, ok := l.Peel(t.Context(), repo, Oid(gitRevParse(t, repo.Path, "refs/tags/deep18"))); ok {
		t.Fatal("deep18 stays unpeelable")
	}
	// A tag whose target chain is memoized resolves through the in-walk
	// cache hit.
	mk("tag", "-a", "deep19", "refs/tags/deep18", "-m", "d19")
	if _, ok := l.Peel(t.Context(), repo, Oid(gitRevParse(t, repo.Path, "refs/tags/deep19"))); !ok {
		t.Fatal("chained tag peel through the memo failed")
	}
}

func TestPeelDeadProcessRecovers(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	tag := gitRevParse(t, repo.Path, "refs/tags/v1")
	pc := l.peelFor(repo)
	if _, _, err := pc.catFile(t.Context(), l.Binary, repo.Path, tag); err != nil {
		t.Fatal(err)
	}
	// Kill the persistent cat-file: the next peel detects the dead process
	// via the read error and resets the client.
	pc.mu.Lock()
	if pc.cmd != nil {
		pc.cmd.Process.Kill()
	}
	pc.mu.Unlock()
	if _, ok := l.Peel(t.Context(), repo, Oid(tag)); ok {
		t.Fatal("peel over a dead process must fail")
	}
	// The failed peel negative-memoized the tag, but a fresh oid spawns a
	// new process and peels fine.
	c := exec.Command("git", "tag", "-a", "fresh", "refs/heads/main", "-m", "fresh")
	c.Dir = repo.Path
	c.Env = append(os.Environ(), "GIT_DIR="+repo.Path)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v\n%s", err, out)
	}
	fresh := gitRevParse(t, repo.Path, "refs/tags/fresh")
	want := gitRevParse(t, repo.Path, "refs/tags/fresh^{}")
	if p, ok := l.Peel(t.Context(), repo, Oid(fresh)); !ok || p != want {
		t.Fatalf("fresh peel = %s %v, want %s", p, ok, want)
	}
}
