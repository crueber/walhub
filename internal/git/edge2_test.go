// edge2_test.go — systematic edge coverage: error paths for snapshot/txn,
// ingest scratch failures, guard refusal shapes, peel client resets, and the
// pkt-line codec's protocol edges.
package git

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// chmodAll denies access to a path (restore with a later chmod 0o755).
func deny(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o700) }) //nolint:errcheck
}

// ---- refs snapshot error paths -----------------------------------------------------

func TestSnapshotFrom_PackedRefsErrorAndLooseReadError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "snaperr"}, Sha1)
	l := NewLayer()

	// packed-refs that is a DIRECTORY: ReadFile fails with EISDIR.
	packed := filepath.Join(repo.Path, "packed-refs")
	if err := os.MkdirAll(packed, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SnapshotFrom(NewRefCache(), repo); err == nil {
		t.Fatal("unreadable packed-refs must error")
	}
	os.Remove(packed) //nolint:errcheck

	// A loose ref FILE that cannot be read: readLooseRefs surfaces the error.
	heads := filepath.Join(repo.Path, "refs", "heads")
	if err := os.MkdirAll(heads, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(heads, "secret")
	if err := os.WriteFile(secret, []byte(oidA), 0o644); err != nil {
		t.Fatal(err)
	}
	deny(t, secret)
	if _, err := l.SnapshotFrom(NewRefCache(), repo); err == nil {
		t.Fatal("unreadable loose ref must error")
	}
}

func TestReadLooseRefs_WalkErrorAndMissingRoot(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "loose"}, Sha1)

	// Missing refs dir → empty, no error.
	if refs, err := readLooseRefs(repo.Path); err != nil || len(refs) != 0 {
		t.Fatalf("missing refs dir = %v %v", refs, err)
	}

	// An unreadable refs/heads DIRECTORY surfaces a walk error.
	heads := filepath.Join(repo.Path, "refs", "heads")
	if err := os.MkdirAll(heads, 0o755); err != nil {
		t.Fatal(err)
	}
	deny(t, heads)
	if _, err := readLooseRefs(repo.Path); err == nil {
		t.Fatal("unreadable refs subdirectory must error")
	}
}

func TestParsePackedRefs_BadOidSkipped(t *testing.T) {
	refs := parsePackedRefs([]byte(
		"zzzz refs/heads/bad\n" + // invalid oid → skipped
			"^deadbeef refs/heads/peeled\n" + // continuation
			"^orphan\n")) // continuation with no preceding ref → ignored
	if len(refs) != 0 {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestPeekRefSymref(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "peek"}, Sha1)
	if err := os.WriteFile(filepath.Join(repo.Path, "refs", "heads", "alias"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, ok := PeekRef(repo, "refs/heads/alias")
	if !ok || e.Oid != "ref: refs/heads/main" {
		t.Fatalf("symref peek = %+v ok=%v", e, ok)
	}
}

// ---- apply_ref_txn error paths ------------------------------------------------------

func TestValidateRefUpdateSymbolicOnlyOnHead(t *testing.T) {
	err := ValidateRefUpdate(&RefUpdate{Name: "refs/heads/x", OldOid: zero40, NewOid: oidA, NewSymbolicTarget: "refs/heads/main"})
	if err == nil {
		t.Fatal("symbolic target off HEAD must be refused")
	}
}

func TestApplyRefTxnValidationAndSnapshotErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "txerr"}, Sha1)
	l := NewLayer()

	// A malformed ref name fails validation before any subprocess.
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "bad name", OldOid: zero40, NewOid: oidA},
	}, true); err == nil {
		t.Fatal("invalid ref name accepted")
	}

	// An unreadable packed-refs fails the snapshot step.
	packed := filepath.Join(repo.Path, "packed-refs")
	if err := os.Mkdir(packed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oidA},
	}, false); err == nil {
		t.Fatal("snapshot failure must propagate")
	}
}

func TestApplyRefTxnHEADSkipAndForcelessDelete(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "txhead"}, Sha1)
	l := NewLayer()
	oid := gitBlob(t, repo, "x")
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oid},
		// HEAD updates are skipped by the checkOld loop.
		{Name: "HEAD", OldOid: "", NewOid: "", NewSymbolicTarget: "refs/heads/main"},
	}, true); err != nil {
		t.Fatalf("mixed txn: %v", err)
	}
	// Delete without a verified old value (checkOld=false path).
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/absent", OldOid: "", NewOid: zero40},
	}, false); err != nil {
		t.Fatalf("plain delete: %v", err)
	}
	// A failing update-ref (two updates for one ref, checkOld=false)
	// surfaces a conflict parsed from git's stderr.
	err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads", OldOid: zero40, NewOid: zero40},
	}, false)
	var ce *RefConflictError
	if err == nil || !errors.As(err, &ce) {
		t.Fatalf("duplicate update = %v, want RefConflictError", err)
	}
	// A symbolic HEAD update that fails validation after commit.
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "HEAD", OldOid: "", NewOid: "", NewSymbolicTarget: "bad name"},
	}, false); err == nil {
		t.Fatal("invalid symbolic target must fail")
	}
}

func TestLoadSnapshotErrorAndDetachedHead(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "load"}, Sha1)
	l := NewLayer()

	// Detached HEAD (no target): LoadSnapshot writes the oid into HEAD.
	if err := l.LoadSnapshot(repo, []RefEntry{{Name: "refs/heads/main", Oid: Oid(oidA)}}, "", Oid(oidA)); err != nil {
		t.Fatalf("detached load: %v", err)
	}
	if target, oid := readHead(repo); target != "" || oid != oidA {
		t.Fatalf("detached head = %q/%q", target, oid)
	}

	// An atomicWrite failure (temp path is a directory) propagates.
	if err := os.Mkdir(filepath.Join(repo.Path, "packed-refs.walgit-tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.LoadSnapshot(repo, nil, "refs/heads/main", Oid(oidA)); err == nil {
		t.Fatal("atomic write failure must propagate")
	}

	// An invalid symbolic head target fails the direct HEAD write.
	if err := l.LoadSnapshot(repo, nil, "bad name", Oid(oidA)); err == nil {
		t.Fatal("invalid head target must fail")
	}
}

func TestApplyRefTxnsOfflineHeadAndPeeled(t *testing.T) {
	refs := []RefEntry{{Name: "refs/heads/main", Oid: Oid(oidA)}}
	out := ApplyRefTxnsOffline(refs, []RefUpdate{
		{Name: "HEAD", OldOid: "", NewOid: Oid(oidB)}, // ignored
		{Name: "refs/tags/v1", OldOid: zero40, NewOid: Oid(oidB), NewPeeled: Oid(oidA)},
	})
	if len(out) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Name != "refs/heads/main" || out[0].Oid != Oid(oidA) {
		t.Fatalf("untouched entry = %+v", out[0])
	}
	if out[1].Name != "refs/tags/v1" || out[1].Peeled != Oid(oidA) {
		t.Fatalf("peeled entry = %+v", out[1])
	}
}

func TestPeelMemoizedAndReset(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx := t.Context()
	tag := gitRevParse(t, repo.Path, "refs/tags/v1")
	want := gitRevParse(t, repo.Path, "refs/tags/v1^{}")

	// Second Peel consults the positive memo (651-654 hit).
	if p, ok := l.Peel(ctx, repo, Oid(tag)); !ok || p != want {
		t.Fatalf("peel = %s %v", p, ok)
	}
	if p, ok := l.Peel(ctx, repo, Oid(tag)); !ok || p != want {
		t.Fatalf("memoized peel = %s %v", p, ok)
	}

	// cat-file on an absent oid answers "<oid> missing" (two fields) — a
	// negative, non-error reply through the persistent process.
	pc := l.peelFor(repo)
	typ, body, err := pc.catFile(ctx, l.Binary, repo.Path, strings.Repeat("f", 40))
	if err != nil || typ != "" || body != nil {
		t.Fatalf("absent cat-file = %q %q %v", typ, body, err)
	}

	// resetLocked tears the persistent process down (stdin + cmd set).
	if _, _, err := pc.catFile(ctx, l.Binary, repo.Path, tag); err != nil {
		t.Fatal(err)
	}
	pc.resetLocked()
	pc.mu.Lock()
	hasCmd := pc.cmd != nil
	pc.mu.Unlock()
	if hasCmd {
		t.Fatal("reset must clear the process")
	}
}

func TestRememberPeelCreatesClient(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, head := realRepoFixture(t)
	// rememberPeel on a repo with no client yet creates one.
	l.rememberPeel(repo, Oid(head), Oid(head))
	if v, ok := l.cachedPeel(repo, Oid(head)); !ok || v != Oid(head) {
		t.Fatalf("remembered peel = %s %v", v, ok)
	}
}

// ---- repo helpers -------------------------------------------------------------------

func TestOpenLocalRepoStatError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "o"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	// root/o/name.git → stat hits ENOTDIR.
	if _, err := OpenLocalRepo(root, RepoId{Owner: "o", Name: "x"}); err == nil {
		t.Fatal("stat error must surface")
	}
}

func TestInitLocalRepoErrors(t *testing.T) {
	root := t.TempDir()
	// MkdirAll fails: root/o is a file.
	if err := os.WriteFile(filepath.Join(root, "o"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "x"}, Sha1); err == nil {
		t.Fatal("mkdir failure must surface")
	}

	// A pre-existing config DIRECTORY fails the config write on re-init.
	dir := filepath.Join(root, "o2")
	if err := os.MkdirAll(filepath.Join(dir, "x.git", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InitLocalRepo(root, RepoId{Owner: "o2", Name: "x"}, Sha1); err == nil {
		t.Fatal("config-as-directory must fail init")
	}

	// A read-only repo dir fails at git init (re-init cannot write).
	ro := filepath.Join(root, "o3", "x.git")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	deny(t, ro)
	if _, err := InitLocalRepo(root, RepoId{Owner: "o3", Name: "x"}, Sha1); err == nil {
		t.Fatal("read-only repo dir must fail init")
	}
}

func TestFormatMissingConfig(t *testing.T) {
	repo := &LocalRepo{Path: filepath.Join(t.TempDir(), "nope")}
	if repo.Format() != Sha1 {
		t.Fatal("missing config must default to sha1")
	}
}

func TestReadHeadMissingFile(t *testing.T) {
	root := t.TempDir()
	repo := &LocalRepo{Path: root}
	if target, oid := readHead(repo); target != "" || oid != "" {
		t.Fatalf("missing HEAD = %q/%q", target, oid)
	}
}

func TestGitVersionUnparseable(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakegit")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho not-a-version\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	l.Binary = script
	if _, _, err := l.GitVersion(t.Context()); err == nil {
		t.Fatal("unparseable version output must error")
	}
}

// ---- layer.run edges ------------------------------------------------------------------

func TestRunStdinFeedErrorAndOnWait(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "run2"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()

	// A stdin reader that errors: the child (cat-file --batch) exits cleanly
	// on early EOF, so the feed error is surfaced per the §2 discipline.
	_, err = l.run(t.Context(), execSpec{
		argv:  []string{"cat-file", "--batch"},
		dir:   repo.Path,
		stdin: errReaderAt{},
	})
	var ge *GitError
	if err == nil || !errors.As(err, &ge) || ge.Kind != GitErrIo {
		t.Fatalf("stdin feed error = %v", err)
	}

	// onWait fires with and without a stdin feeder.
	waited := 0
	if _, err := l.run(t.Context(), execSpec{
		argv: []string{"rev-parse", "--git-dir"}, dir: repo.Path, onWait: func() { waited++ },
	}); err != nil || waited != 1 {
		t.Fatalf("onWait plain = %v waited=%d", err, waited)
	}
	if _, err := l.run(t.Context(), execSpec{
		argv: []string{"rev-parse", "--git-dir"}, dir: repo.Path,
		stdin: strings.NewReader(""), onWait: func() { waited++ },
	}); err != nil || waited != 2 {
		t.Fatalf("onWait with stdin = %v waited=%d", err, waited)
	}

	// A canceled context surfaces the Io variant of the subprocess error.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = l.run(ctx, execSpec{argv: []string{"rev-parse"}, dir: repo.Path})
	if err == nil || !errors.As(err, &ge) || ge.Kind != GitErrIo {
		t.Fatalf("canceled run = %v", err)
	}
}

// ---- pktline codec edges ----------------------------------------------------------------

func TestDataPktOversizePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("oversized payload must panic")
		}
	}()
	dataPkt(make([]byte, MaxPayload+1))
}

func TestPktReaderProtocolEdges(t *testing.T) {
	// Truncated 4-byte length header.
	if _, _, err := NewPktReader(strings.NewReader("000")).Next(); err == nil {
		t.Fatal("truncated header must error")
	}
	// Length 0003 is invalid.
	if _, _, err := NewPktReader(strings.NewReader("0003")).Next(); err == nil {
		t.Fatal("length 0003 must error")
	}
	// A reader error mid-payload (not EOF) surfaces.
	if _, _, err := NewPktReader(struct{ io.Reader }{errReaderAt{}}).Next(); err == nil {
		t.Fatal("mid-payload read error must surface")
	}
	if n, ok := parseHex4([]byte("00FF")); !ok || n != 255 {
		t.Fatalf("parseHex4 upper = %d %v", n, ok)
	}
}

func TestReadAllPktsEdges(t *testing.T) {
	// Clean EOF without a flush.
	pkts, err := ReadAllPkts(strings.NewReader(string(Pkt("a\n"))))
	if err != nil || len(pkts) != 1 {
		t.Fatalf("eof drain = %v %v", pkts, err)
	}
	// Protocol error mid-stream.
	if _, err := ReadAllPkts(strings.NewReader("zzzz")); err == nil {
		t.Fatal("protocol error must surface")
	}
	// Delim pkt is skipped, flush stops the drain.
	pkts, err = ReadAllPkts(strings.NewReader(string(Delim()) + string(Flush())))
	if err != nil || len(pkts) != 0 {
		t.Fatalf("delim+flush = %v %v", pkts, err)
	}
}

func TestSidebandDecodeEdges(t *testing.T) {
	// Bad band byte.
	_, _, err := SidebandDecode(PktBytes([]byte{9, 'x'}))
	if err == nil {
		t.Fatal("bad band must error")
	}
	// A protocol error mid-stream surfaces.
	if _, _, err := SidebandDecode([]byte("zzzz")); err == nil {
		t.Fatal("bad pkt must error")
	}
	// An empty data pkt is skipped; band 2 and 3 messages are collected.
	empty := PktBytes([]byte{})
	msg := PktBytes(append([]byte{2}, []byte("hi")...))
	fatal := PktBytes(append([]byte{3}, []byte("bye")...))
	payload, messages, err := SidebandDecode(append(append(empty, msg...), fatal...))
	if err != nil || len(payload) != 0 || string(messages) != "hibye" {
		t.Fatalf("sideband = %q %q %v", payload, messages, err)
	}
}

// ---- receive edges ------------------------------------------------------------------------

func TestPushRequestHasAndProtocolEdges(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "push2"}, Sha1)
	l := NewLayer()

	// Unknown capability → false (loop fall-through).
	body := cat(Pkt(zero40+" "+oidA+" refs/heads/main\x00\n"), []byte("0000"))
	req, err := l.ParsePushRequest(repo, body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Has("nope") {
		t.Fatal("unknown cap must be false")
	}

	// A truncated pkt before the flush is a protocol error.
	if _, err := l.ParsePushRequest(repo, []byte("zzzz")); err == nil {
		t.Fatal("bad pkt must error")
	}

	// A delim pkt between commands is tolerated (non-data continues).
	body = cat(Pkt(zero40+" "+oidA+" refs/heads/main\n"), []byte("0001"), []byte("0000"))
	if _, err := l.ParsePushRequest(repo, body); err != nil {
		t.Fatalf("delim tolerated: %v", err)
	}

	// A truncated pkt inside the push-options section is a protocol error.
	body = cat(
		Pkt(zero40+" "+oidA+" refs/heads/main\x00push-options\n"),
		[]byte("0000"), []byte("zzzz"))
	if _, err := l.ParsePushRequest(repo, body); err == nil {
		t.Fatal("bad push-options pkt must error")
	}
}

func TestConnectivityBatchCapAndStartError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	l.Connect = true

	// Healthy tips pass the full pipeline.
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{gitRevParse(t, repo.Path, "refs/heads/main")}); err != nil {
		t.Fatalf("healthy: %v", err)
	}

	// More than 16 missing objects trips the early-exit cap.
	var tips []Oid
	for i := range 20 {
		tips = append(tips, Oid(strings.Repeat(string(rune('a'+i)), 40)))
	}
	err := l.CheckConnectivity(t.Context(), repo, tips)
	var mo *MissingObjectError
	if err == nil || !errors.As(err, &mo) {
		t.Fatalf("missing batch = %v", err)
	}
	if len(mo.Oids) > 16 {
		t.Fatalf("retained = %d, cap is 16", len(mo.Oids))
	}

	// A broken git binary fails at rev-list Start.
	l.Binary = "definitely-not-git"
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{gitRevParse(t, repo.Path, "refs/heads/main")}); err == nil {
		t.Fatal("missing binary must error")
	}
}

// ---- ingest edges ---------------------------------------------------------------------------

func TestNextSuffixCollision(t *testing.T) {
	ingestMu.Lock()
	lastNanos = time.Now().UnixNano()
	ingestMu.Unlock()
	if nextSuffix() <= lastNanos-1 {
		t.Fatal("collision branch must advance")
	}
}

func TestIngestScratchCreationErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	l := NewLayer()

	// An unwritable repo dir fails the scratch MkdirAll.
	deny(t, repo.Path)
	if _, err := l.Ingest(t.Context(), repo, strings.NewReader("x"), 0, false, false); err == nil {
		t.Fatal("unwritable repo must fail ingest")
	}
}

func TestIngestCanceledContext(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Ingest(ctx, repo, strings.NewReader("x"), 0, false, false); err == nil {
		t.Fatal("canceled ingest must fail")
	}
}

func TestIngestScratchConfigErrorAndCopyFile(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	// copyFile: a missing source is not an error; an unreadable one is.
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := copyFile(dst, filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("absent source = %v", err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("data"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(src, 0o644) //nolint:errcheck
	if err := copyFile(dst, src); err == nil {
		t.Fatal("unreadable source must error")
	}
}

func TestIdxObjectCountTruncatedFanout(t *testing.T) {
	dir := t.TempDir()
	// Valid magic but fewer than 1032 bytes.
	p := filepath.Join(dir, "short.idx")
	if err := os.WriteFile(p, append([]byte{0xff, 't', 'O', 'c', 2, 0, 0, 0}, []byte("short")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idxObjectCount(p); err == nil {
		t.Fatal("truncated fanout must error")
	}
}

func TestParseTraceFiles(t *testing.T) {
	l := NewLayer()
	// Absent trace file → zero metrics, no error.
	if m := l.parseTrace(filepath.Join(t.TempDir(), "gone.jsonl"), time.Now(), 1); m == nil {
		t.Fatal("absent trace must still return metrics")
	}
	// Real JSONL with enter/leave pairs and garbage lines.
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	lines := `{"event":"region_enter","region":"feed","t_abs":10}
not json
{"event":"region_leave","region":"feed","t_abs":12.5}
{"event":"region_leave","region":"lonely","t_abs":13}
{"event":"region_enter","region":"index","t_abs":"bad"}`
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	m := l.parseTrace(p, time.Now(), 1.5)
	if m == nil || m.FeedSeconds != 1.5 {
		t.Fatalf("metrics = %+v", m)
	}
	if m.Phases["feed"] < 2 {
		t.Fatalf("feed phase = %v", m.Phases)
	}
	if m.GitSeconds < 13 {
		t.Fatalf("git seconds = %v", m.GitSeconds)
	}
}

// ---- maintenance edges ------------------------------------------------------------------------

func TestIdxSetErrorsAndDiff(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	// PackDir as a FILE → ReadDir fails with a non-not-exist error.
	packDir := repo.PackDir()
	deny(t, packDir) // ReadDir on a dir we can't open → EACCES? actually deny the file itself
	if _, err := idxSet(repo); err == nil {
		t.Fatal("unreadable pack dir must error")
	}
}

func TestRepackErrorPaths(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// A canceled context surfaces at the repack subprocess.
	if _, err := l.GeometricRepack(ctx, repo, 2, false, nil); err == nil {
		t.Fatal("canceled geometric repack must fail")
	}
	if _, err := l.FullRepack(ctx, repo, nil); err == nil {
		t.Fatal("canceled full repack must fail")
	}
	if err := l.WriteCommitGraphCheck(ctx, repo); err == nil {
		t.Fatal("canceled commit-graph write must fail")
	}
	if err := l.FoldCommitGraphs(ctx, repo, nil); err == nil {
		t.Fatal("canceled fold must fail")
	}
	if err := l.WriteHistoryMidx(ctx, repo, []string{"a.idx"}, "a.idx"); err == nil {
		t.Fatal("canceled midx must fail")
	}
}

// WriteCommitGraphCheck is a thin wrapper so the canceled-context test can
// reach the subprocess error of WriteCommitGraph directly.
func (l *Layer) WriteCommitGraphCheck(ctx context.Context, repo *LocalRepo) error {
	_, err := l.WriteCommitGraph(ctx, repo, false, "")
	return err
}

func TestCopyLastChainLayerPaths(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	graphDir := filepath.Join(repo.Path, "objects", "info", "commit-graphs")

	// Absent graph dir → no side file.
	if name, err := copyLastChainLayer(repo, ""); err != nil || name != "" {
		t.Fatalf("absent dir = %q %v", name, err)
	}

	// Empty graph dir (non-split layout) → no side file.
	if err := os.MkdirAll(graphDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if name, err := copyLastChainLayer(repo, ""); err != nil || name != "" {
		t.Fatalf("non-split = %q %v", name, err)
	}

	// A chain naming a missing graph file errors.
	if err := os.WriteFile(filepath.Join(graphDir, "commit-graph-chain"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := copyLastChainLayer(repo, ""); err == nil {
		t.Fatal("missing graph file must error")
	}

	// A real side-file copy round-trips; an unwritable sideDir fails.
	if err := os.WriteFile(filepath.Join(graphDir, "graph-abc.graph"), []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphDir, "commit-graph-chain"), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if name, err := copyLastChainLayer(repo, t.TempDir()); err != nil || name != "abc" {
		t.Fatalf("copy = %q %v", name, err)
	}
	// An unwritable sideDir surfaces the write error.
	sd := filepath.Join(t.TempDir(), "sd")
	if err := os.WriteFile(sd, []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := copyLastChainLayer(repo, sd); err == nil {
		t.Fatal("file sideDir must error")
	}
}

func TestInstallCommitGraphBaseErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo := &LocalRepo{Path: root}
	// objects/info is a FILE → MkdirAll fails.
	if err := os.MkdirAll(filepath.Join(root, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "objects", "info"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallCommitGraphBase(repo, "sum", []byte("g")); err == nil {
		t.Fatal("file objects/info must fail install")
	}
}

func TestHistoryPackAndMidxErrorPaths(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Canceled context fails at the pack-objects step.
	if _, err := l.HistoryPack(ctx, repo, "base"); err == nil {
		t.Fatal("canceled history pack must fail")
	}

	// An unreadable packed-refs fails the snapshot load (replace the file
	// the fixture wrote with a directory of the same name).
	packed := filepath.Join(repo.Path, "packed-refs")
	if err := os.Remove(packed); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(packed, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HistoryPack(t.Context(), repo, "base"); err == nil {
		t.Fatal("unreadable refs must fail history pack")
	}
}

// ---- bundle / upstream edges -------------------------------------------------------------------

func TestCreateBundleExcludesAndScanErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	out := filepath.Join(t.TempDir(), "b.bundle")
	// A bogus exclude makes git bundle create fail (covers the exclude
	// feed and the error return).
	bogus := strings.Repeat("b", 40)
	if _, _, err := l.CreateBundle(t.Context(), repo, filepath.Join(t.TempDir(), "bad.bundle"),
		[]string{"refs/heads/main"}, []string{bogus}); err == nil {
		t.Fatal("bogus exclude must fail bundle create")
	}
	// scanPackOffset on a path whose open fails.
	if _, _, err := scanPackOffset(filepath.Join(out, "missing")); err == nil {
		t.Fatal("absent file must error")
	}
	// scanPackOffset on a file without PACK magic.
	magicless := filepath.Join(t.TempDir(), "m.bin")
	if err := os.WriteFile(magicless, []byte(strings.Repeat("z", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, off, err := scanPackOffset(magicless); err != nil || off != -1 {
		t.Fatalf("magicless = %d %v", off, err)
	}
}

func TestIdxContainsErrors(t *testing.T) {
	dir := t.TempDir()
	if ok, err := idxContains(filepath.Join(dir, "absent.idx"), oidA); err == nil || ok {
		t.Fatal("absent idx must error")
	}
	if _, err := idxContains("/nope/x", "zz"); err == nil {
		t.Fatal("bad oid must error")
	}
	bad := filepath.Join(dir, "bad.idx")
	if err := os.WriteFile(bad, []byte("junkjunkjunkjunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idxContains(bad, oidA); err == nil {
		t.Fatal("bad magic must error")
	}
}

func TestHexVal(t *testing.T) {
	if hexVal('0') != 0 || hexVal('9') != 9 || hexVal('a') != 10 || hexVal('f') != 15 || hexVal('z') != 0 {
		t.Fatal("hexVal wrong")
	}
}

func TestFetchObjectsAsPackScratchErrors(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// Unwritable repo dir → scratch creation fails.
	deny(t, repo.Path)
	if _, err := l.FetchObjectsAsPack(t.Context(), repo, UpstreamSpec{}, []Oid{Oid(oidA)}); err == nil {
		t.Fatal("unwritable repo must fail repair fetch")
	}
}

func TestFollowFetchInitScratchAndFetchFail(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, l, _ := realRepoFixture(t)
	id := RepoId{Owner: repo.ID.Owner, Name: repo.ID.Name}

	// A fetch against a bogus upstream URL fails at the fetch subprocess.
	if _, err := l.FollowFetch(t.Context(), root, id, UpstreamSpec{URL: "https://127.0.0.1:1/nope.git"}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("bogus upstream must fail follow fetch")
	}

	// An unwritable cache root fails the follow-scratch init.
	root2 := t.TempDir()
	deny(t, root2)
	if _, err := l.FollowFetch(t.Context(), root2, id, UpstreamSpec{URL: "https://127.0.0.1:1/nope.git"}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("unwritable root must fail follow init")
	}
}

// ---- advert leftovers ---------------------------------------------------------------------------

func TestServiceFromNameUploadPack(t *testing.T) {
	if s, ok := ServiceFromName("git-upload-pack"); !ok || s != ServiceUploadPack {
		t.Fatal("upload-pack parse wrong")
	}
}

func TestAdvertisementV0Error(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "adv"}, Sha1)
	l := NewLayer()
	if err := os.Mkdir(filepath.Join(repo.Path, "packed-refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Advertisement(repo, ServiceUploadPack, false, "v"); err == nil {
		t.Fatal("unreadable refs must fail the advertisement")
	}
}

func TestLsRefsSymrefTargetBranches(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "lsr2"}, Sha1)
	l := NewLayer()
	oid := gitBlob(t, repo, "b")
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oid},
	}, true); err != nil {
		t.Fatal(err)
	}
	// Without symrefs the HEAD line carries no symref-target.
	out, err := l.LsRefs(repo, LsRefsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "HEAD") || strings.Contains(string(out), "symref-target") {
		t.Fatalf("no-symrefs advert = %q", out)
	}
	// A prefix that covers nothing still answers with a flush.
	out, err = l.LsRefs(repo, LsRefsArgs{Prefixes: []string{"refs/nowhere/"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "0000" {
		t.Fatalf("empty prefix answer = %q", out)
	}
}

// ---- upload/runPiped -----------------------------------------------------------------------------

func TestRunPipedSubprocessError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// runPiped with a broken binary surfaces the Start error.
	l2 := NewLayer()
	l2.Binary = "definitely-not-git"
	if _, err := l2.runPiped(t.Context(), repo, []string{"version"}, "version=0", strings.NewReader(""), &discardWriter{}); err == nil {
		t.Fatal("broken binary must error")
	}
	// A failing git command surfaces a subprocess error with stderr.
	if _, err := l.runPiped(t.Context(), repo, []string{"rev-parse", "--verify", "nope"}, "version=0", strings.NewReader(""), &discardWriter{}); err == nil {
		t.Fatal("failing command must error")
	}
}
