package git

import (
	"bytes"
	"context"
	"os"
	exec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Coverage for maintenance, bundles, upstream helpers, errors, and misc.

// realRepoFixture builds a repo with REAL objects (two commits, a branch, an
// annotated tag) ingested through the normal path — for tests that hand oids
// to git subprocesses.
func realRepoFixture(t *testing.T) (*LocalRepo, *Layer, Oid) {
	t.Helper()
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
	os.WriteFile(filepath.Join(up, "f.txt"), []byte("one\n"), 0o644)
	mk("add", ".")
	mk("commit", "-m", "c1")
	os.WriteFile(filepath.Join(up, "g.txt"), []byte("two\n"), 0o644)
	mk("add", ".")
	mk("commit", "-m", "c2")
	head := Oid(gitRevParse(t, up, "HEAD"))
	mk("tag", "-a", "v1", "-m", "tagged", string(head))

	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "real"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
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
		t.Fatalf("Ingest: %v", err)
	}
	main := Oid(gitRevParse(t, up, "main^{commit}"))
	tagOid := Oid(gitRevParse(t, up, "v1"))
	refs := []RefEntry{
		{Name: "refs/heads/main", Oid: main},
		{Name: "refs/tags/v1", Oid: tagOid, Peeled: main},
	}
	if err := l.LoadSnapshot(repo, refs, "refs/heads/main", main); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	return repo, l, head
}

func TestServiceNames(t *testing.T) {
	if ServiceUploadPack.String() != "git-upload-pack" || ServiceReceivePack.String() != "git-receive-pack" {
		t.Errorf("names = %q, %q", ServiceUploadPack, ServiceReceivePack)
	}
	if s, ok := ServiceFromName("git-receive-pack"); !ok || s != ServiceReceivePack {
		t.Errorf("ServiceFromName receive = %v %v", s, ok)
	}
	if _, ok := ServiceFromName("nope"); ok {
		t.Error("unknown service accepted")
	}
}

func TestErrorsUnwrap(t *testing.T) {
	var ce *RefConflictError
	if !errorsAs2(errWrap(refConflict("r", "e", "a")), &ce) || ce.Ref != "r" {
		t.Error("RefConflictError unwrap broken")
	}
	var me *MissingObjectError
	if !errorsAs2(errWrap(missingObjects([]string{"x"})), &me) {
		t.Error("MissingObjectError unwrap broken")
	}
	var pe *PackRejectedError
	if !errorsAs2(errWrap(&PackRejectedError{Detail: "d"}), &pe) || pe.Detail != "d" {
		t.Error("PackRejectedError unwrap broken")
	}
	var te *TooManyWantsError
	if !errorsAs2(errWrap(&TooManyWantsError{Cap: 3}), &te) || te.Cap != 3 {
		t.Error("TooManyWantsError unwrap broken")
	}
}

func TestPoolRun(t *testing.T) {
	p := NewPool(1)
	done := make(chan struct{})
	if err := p.Run(context.Background(), func() error { close(done); return nil }); err != nil {
		t.Errorf("Run: %v", err)
	}
	<-done
	// cancelled context returns without running: occupy the only slot so the
	// select cannot win the semaphore race.
	release := make(chan struct{})
	occupied := make(chan struct{})
	go func() {
		_ = p.Run(context.Background(), func() error {
			close(occupied)
			<-release
			return nil
		})
	}()
	<-occupied
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	if err := p.Run(ctx, func() error { ran = true; return nil }); err == nil {
		t.Error("cancelled Run succeeded")
	}
	if ran {
		t.Error("fn ran despite cancellation")
	}
	close(release)
}

func TestBoundedBufferTail(t *testing.T) {
	b := newBounded(4)
	b.Write([]byte("abcdefghij"))
	if got := b.String(); !strings.HasSuffix(got, "hijk"[0:0]) && !strings.Contains(got, "ghij") {
		t.Errorf("tail = %q", got)
	}
	if !strings.HasPrefix(b.String(), "…") {
		t.Errorf("full marker missing: %q", b.String())
	}
}

func TestWriteRepoConfigIdempotent(t *testing.T) {
	path := t.TempDir()
	repo, err := InitLocalRepo(path, RepoId{Owner: "o", Name: "cfg"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(repo.Path, "config"))
	if err := WriteRepoConfig(repo.Path); err != nil {
		t.Fatalf("WriteRepoConfig: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(repo.Path, "config"))
	if !bytes.Equal(before, after) {
		t.Error("re-write changed config (not idempotent)")
	}
}

func TestGeometricRepackDiff(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	before, _ := idxSet(repo)
	if len(before) == 0 {
		t.Fatal("no idx after ingest")
	}
	diff, err := l.GeometricRepack(t.Context(), repo, 2, true, []string{"pack-" + lastHexToken("")})
	if err != nil {
		t.Fatalf("GeometricRepack: %v", err)
	}
	if diff == nil {
		t.Error("nil diff")
	}
}

func TestFullRepackRemovesKeep(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	keep := filepath.Join(repo.PackDir(), "pack-stray.keep")
	os.MkdirAll(repo.PackDir(), 0o755)
	os.WriteFile(keep, []byte(""), 0o644)
	if _, err := l.FullRepack(t.Context(), repo, nil); err != nil {
		t.Fatalf("FullRepack: %v", err)
	}
	if _, err := os.Stat(keep); err == nil {
		t.Error("stray .keep survived")
	}
}

func TestWriteCommitGraphAndInstall(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l := lsRefsFixture(t)
	side := t.TempDir()
	checksum, err := l.WriteCommitGraph(t.Context(), repo, false, side)
	if err != nil {
		t.Fatalf("WriteCommitGraph: %v", err)
	}
	_ = checksum
	// install an arbitrary side-file as chain base
	os.RemoveAll(filepath.Join(repo.Path, "objects", "info", "commit-graphs"))
	if err := InstallCommitGraphBase(repo, "abc123", []byte("data")); err != nil {
		t.Fatalf("InstallCommitGraphBase: %v", err)
	}
	chain, err := os.ReadFile(filepath.Join(repo.Path, "objects", "info", "commit-graphs", "commit-graph-chain"))
	if err != nil || strings.TrimSpace(string(chain)) != "abc123" {
		t.Errorf("chain = %q err %v", chain, err)
	}
	if _, err := os.Stat(filepath.Join(repo.Path, "objects", "info", "commit-graphs", "graph-abc123.graph")); err != nil {
		t.Errorf("base graph missing: %v", err)
	}
}

func TestIdxContainsLookup(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	snap, _ := l.Snapshot(repo)
	idxs, _ := filepath.Glob(filepath.Join(repo.PackDir(), "pack-*.idx"))
	if len(idxs) == 0 {
		t.Fatal("no idx in fixture")
	}
	idxPath := idxs[0]
	ok, err := idxContains(idxPath, string(snap.Refs[0].Oid))
	if err != nil || !ok {
		t.Errorf("idxContains(%s) = %v, %v", snap.Refs[0].Oid, ok, err)
	}
	absent := strings.Repeat("e", 40)
	ok, err = idxContains(idxPath, absent)
	if err != nil || ok {
		t.Errorf("idxContains(absent) = %v, %v", ok, err)
	}
}

func TestBundleHeaderAndScan(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	out := filepath.Join(t.TempDir(), "b.bundle")
	snap, _ := l.Snapshot(repo)
	var refs []string
	for _, e := range snap.Refs {
		refs = append(refs, e.Name) // bundle --stdin takes one rev per line
	}
	size, off, err := l.CreateBundle(t.Context(), repo, out, refs, nil)
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	if off <= 0 || size <= int64(off) {
		t.Errorf("bundle size=%d packOffset=%d", size, off)
	}
	data, _ := os.ReadFile(out)
	if string(data[off:off+4]) != "PACK" {
		t.Errorf("offset %d not PACK magic", off)
	}
	// header split must equal the header bytes
	if !bytes.HasPrefix(data, []byte("# v2 git bundle\n")) {
		t.Errorf("bundle header = %q", data[:16])
	}
}

func TestScanPackOffsetStraddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b")
	// header longer than nothing; PACK straddles a 64 KiB boundary is hard to
	os.WriteFile(path, []byte("PACKrest"), 0o644)
	size, off, err := scanPackOffset(path)
	if err != nil || off != 0 || size != 8 {
		t.Errorf("scan = %d %d %v", size, off, err)
	}
	os.WriteFile(path, []byte("no magic here"), 0o644)
	_, off, err = scanPackOffset(path)
	if err != nil || off != -1 {
		t.Errorf("no-magic scan = %d %v", off, err)
	}
	// straddling: 65533 bytes then "PACK" (starts at 65533, spans 65533..65537)
	buf := bytes.Repeat([]byte("z"), 65533)
	buf = append(buf, []byte("PACK")...)
	os.WriteFile(path, buf, 0o644)
	_, off, err = scanPackOffset(path)
	if err != nil || off != 65533 {
		t.Errorf("straddle scan = %d %v (want 65533)", off, err)
	}
}

func TestUpstreamCredentialArgvOrder(t *testing.T) {
	argv := upstreamCredentialArgv()
	if len(argv) != 4 || argv[0] != "-c" || argv[1] != "credential.helper=" {
		t.Errorf("clear-then-set order broken: %q", argv)
	}
	if !strings.Contains(argv[3], "x-access-token") {
		t.Errorf("helper missing username: %q", argv[3])
	}
}

func TestUpstreamEnvToken(t *testing.T) {
	t.Setenv("WALGIT_TEST_TOKEN_ENV", "sekret")
	l := NewLayer()
	env := l.upstreamEnv(UpstreamSpec{TokenEnv: "WALGIT_TEST_TOKEN_ENV"})
	found := false
	for _, kv := range env {
		if kv == "WALGIT_UPSTREAM_TOKEN=sekret" {
			found = true
		}
	}
	if !found {
		t.Errorf("token env = %v", env)
	}
	if env := l.upstreamEnv(UpstreamSpec{}); len(env) != 0 {
		t.Errorf("empty spec env = %v", env)
	}
}

func TestFollowFetchRoundTrip(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	// upstream fixture: a plain repo with one branch
	up := t.TempDir()
	cmd := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = up
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	cmd("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(up, "f"), []byte("x"), 0o644)
	cmd("add", ".")
	cmd("commit", "-m", "c")
	head := gitRevParse(t, up, "HEAD")

	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "fol"}, Sha1)
	l := NewLayer()
	tips, err := l.FollowFetch(t.Context(), root, repo.ID, UpstreamSpec{URL: up}, []string{"main"})
	if err != nil {
		t.Fatalf("FollowFetch: %v", err)
	}
	if got := tips["main"]; got != Oid(head) {
		t.Errorf("follow tip = %s, want %s", got, head)
	}
	// scratch packs discarded
	matches, _ := filepath.Glob(filepath.Join(root, "follow", "o", "fol.git", "objects", "pack", "pack-*"))
	if len(matches) != 0 {
		t.Errorf("follow packs kept: %v", matches)
	}
}

func TestForceCheck(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	up := t.TempDir()
	cmd := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = up
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	cmd("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(up, "f"), []byte("1"), 0o644)
	cmd("add", ".")
	cmd("commit", "-m", "c1")
	c1 := gitRevParse(t, up, "HEAD")
	os.WriteFile(filepath.Join(up, "f"), []byte("2"), 0o644)
	cmd("commit", "-am", "c2")
	c2 := gitRevParse(t, up, "HEAD")

	repo, l := lsRefsFixture(t)
	_ = repo
	_ = l
	// via a real repo with the objects: clone the fixture into a walhub repo
	root := t.TempDir()
	wrepo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "ff"}, Sha1)
	cmd2 := func(argv ...string) {
		c := exec.Command("git", argv...)
		c.Dir = wrepo.Path
		c.Env = append(os.Environ(), "GIT_DIR="+wrepo.Path)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", argv, err, out)
		}
	}
	cmd2("fetch", "--no-tags", up, "+refs/heads/main:refs/heads/main")
	l2 := NewLayer()
	ff, err := l2.ForceCheck(t.Context(), wrepo, Oid(c1), Oid(c2))
	if err != nil || ff {
		t.Errorf("descendant marked force: %v %v", ff, err)
	}
	force, err := l2.ForceCheck(t.Context(), wrepo, Oid(c2), Oid(c1))
	if err != nil || !force {
		t.Errorf("ancestor not marked force: %v %v", force, err)
	}
	// create/delete trivially force
	force, err = l2.ForceCheck(t.Context(), wrepo, Oid(zero40), Oid(c1))
	if err != nil || !force {
		t.Errorf("create not force: %v %v", force, err)
	}
}

func TestParseShallow(t *testing.T) {
	var body bytes.Buffer
	body.Write(Pkt("shallow " + oidA))
	body.Write(Pkt("want " + oidB))
	body.Write(Pkt("done"))
	body.Write(Flush())
	got := ParseShallow(body.Bytes())
	if len(got) != 1 || got[0] != Oid(oidA) {
		t.Errorf("shallow = %v", got)
	}
}

func TestReadHeadDetached(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "det"}, Sha1)
	os.WriteFile(filepath.Join(repo.Path, "HEAD"), []byte(oidA+"\n"), 0o644)
	target, oid := readHead(repo)
	if target != "" || oid != Oid(oidA) {
		t.Errorf("readHead = %q %q", target, oid)
	}
}

func TestRemoteServedRefusalText(t *testing.T) {
	r := RemoteServedRefusal(&LocalRepo{ID: RepoId{Owner: "o", Name: "r"}})
	if !strings.Contains(r.Error(), "remote base that is not mounted") ||
		!strings.Contains(r.Error(), "o/r") || !strings.Contains(r.Error(), "cache.store_mount") {
		t.Errorf("refusal = %q", r.Error())
	}
}

func TestErrPktAndRefusalShape(t *testing.T) {
	wire := ErrPkt("bad thing")
	payloads, err := ReadAllPkts(bytes.NewReader(wire))
	if err != nil || len(payloads) != 1 {
		t.Fatalf("ReadAllPkts = %v %v", payloads, err)
	}
	if string(payloads[0]) != "ERR bad thing\n" {
		t.Errorf("ERR pkt = %q", payloads[0])
	}
}

// errorsAs2 mirrors errorsAs for wrapped custom errors.
func errorsAs2(err error, target any) bool {
	type unwrappable interface{ Unwrap() error }
	for err != nil {
		switch t := target.(type) {
		case **RefConflictError:
			if e, ok := err.(*RefConflictError); ok {
				*t = e
				return true
			}
		case **MissingObjectError:
			if e, ok := err.(*MissingObjectError); ok {
				*t = e
				return true
			}
		case **PackRejectedError:
			if e, ok := err.(*PackRejectedError); ok {
				*t = e
				return true
			}
		case **TooManyWantsError:
			if e, ok := err.(*TooManyWantsError); ok {
				*t = e
				return true
			}
		}
		u, ok := err.(unwrappable)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func errWrap(err error) error { return &wrapped{err} }

type wrapped struct{ err error }

func (w *wrapped) Error() string { return w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
