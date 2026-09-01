package git

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real git integration (15_testing.md §5): init + ingest a fast-import pack +
// refs round trip, marked to skip when git < 2.47.

func TestIntegrationFastImportIngestAndRefs(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "int", Name: "flow"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	l := NewLayer()

	// Build a real pack with git fast-import (writes a pack into its own odb),
	// then feed that pack through Ingest by asking fast-import for a packfile
	// via `git fast-export` + `git pack-objects`. Simplest real pack: create a
	// commit in a scratch clone, then `git bundle create` → but we need raw
	// pack bytes: use `git pack-objects` over the commit.
	fixture := t.TempDir()
	mk := func(argv ...string) {
		cmd := exec.Command("git", argv...)
		cmd.Dir = fixture
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
	}
	mk("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(fixture, "f.txt"), []byte("hello walhub\n"), 0o644)
	mk("add", ".")
	mk("commit", "-q", "-m", "c1")
	os.WriteFile(filepath.Join(fixture, "g.txt"), []byte("second"), 0o644)
	mk("add", ".")
	mk("commit", "-m", "c2")
	head := gitRevParse(t, fixture, "HEAD")

	// pack the new commit + tree + blobs into a packfile
	var packBuf bytes.Buffer
	packCmd := exec.Command("git", "pack-objects", "--stdout", "--revs")
	packCmd.Dir = fixture
	packCmd.Stdin = strings.NewReader(head + "\n")
	packCmd.Stdout = &packBuf
	if err := packCmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}

	// Ingest into the repo (thin=false — complete pack; fsck=true).
	res, err := l.Ingest(t.Context(), repo, bytes.NewReader(packBuf.Bytes()), int64(packBuf.Len()+1), false, true)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Checksum == "" {
		t.Fatal("empty checksum")
	}
	if res.ObjectCount == 0 {
		t.Errorf("object count = 0, want > 0")
	}
	// scratch gone
	entries, _ := os.ReadDir(repo.Path)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "walgit-ingest-") {
			t.Errorf("scratch dir left behind: %s", e.Name())
		}
	}
	// pack files landed in the repo
	for _, ext := range []string{".pack", ".idx"} {
		if _, err := os.Stat(filepath.Join(repo.PackDir(), "pack-"+string(res.Checksum)+ext)); err != nil {
			t.Errorf("missing %s: %v", ext, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo.PackDir(), "pack-"+string(res.Checksum)+".keep")); err == nil {
		t.Error(".keep not discarded")
	}

	// refs round trip: apply a txn pointing main at the commit, then advertise
	oid := Oid(head)
	txn := []RefUpdate{{Name: "refs/heads/main", OldOid: zero40, NewOid: oid}}
	if err := l.ApplyRefTxn(t.Context(), repo, txn, true); err != nil {
		t.Fatalf("ApplyRefTxn: %v", err)
	}
	snap, err := l.Snapshot(repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if e, ok := snap.Get("refs/heads/main"); !ok || e.Oid != oid {
		t.Fatalf("main = %+v", e)
	}
	if snap.HeadOid != oid || snap.HeadTarget != "refs/heads/main" {
		t.Errorf("HEAD = %q/%q", snap.HeadTarget, snap.HeadOid)
	}

	// advertisement includes the ref and HEAD
	adv, err := l.Advertisement(repo, ServiceUploadPack, false, "1.0")
	if err != nil {
		t.Fatalf("Advertisement: %v", err)
	}
	if !strings.Contains(string(adv), string(oid)+" refs/heads/main") {
		t.Errorf("advert missing main: %q", adv)
	}
	if !strings.Contains(string(adv), string(oid)+" HEAD") {
		t.Errorf("advert missing HEAD: %q", adv)
	}

	// connectivity over the ingested pack
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{oid}); err != nil {
		t.Errorf("CheckConnectivity: %v", err)
	}
	// connectivity failure for a bogus tip
	bogus := Oid(strings.Repeat("7", 40))
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{bogus}); err == nil {
		t.Error("bogus tip passed connectivity")
	}

	// stock git can read the ingested repo (bare, readable)
	out, err := exec.Command("git", "-C", repo.Path, "rev-parse", "refs/heads/main^{commit}").Output()
	if err != nil {
		t.Fatalf("stock git read: %v", err)
	}
	if strings.TrimSpace(string(out)) != string(oid) {
		t.Errorf("rev-parse = %q, want %s", out, oid)
	}
}

func TestIngestMaxBytes(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "cap"}, Sha1)
	l := NewLayer()
	// 1 KiB cap against 64 KiB of noise → ErrMaxBytes before any subprocess.
	_, err := l.Ingest(t.Context(), repo, bytes.NewReader(make([]byte, 64<<10)), 1024, false, false)
	if err == nil {
		t.Fatal("max_bytes not enforced")
	}
	if !strings.Contains(err.Error(), "max_bytes") {
		t.Errorf("err = %v", err)
	}
}

func TestUploadPackStatelessRPC(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l := lsRefsFixture(t)
	// v2 ls-refs request through stock upload-pack — proves the driver works.
	var req bytes.Buffer
	req.Write(Pkt("command=ls-refs"))
	req.Write(Pkt("object-format=sha1"))
	req.Write(Delim())
	req.Write(Pkt("symrefs"))
	req.Write(Flush())
	var out bytes.Buffer
	if err := l.UploadPack(t.Context(), repo, &req, &out, "2"); err != nil {
		t.Fatalf("UploadPack: %v", err)
	}
	text := string(out.Bytes())
	if !strings.Contains(text, "refs/heads/main") {
		t.Errorf("ls-refs via stock upload-pack missing main: %q", text)
	}
}

func TestParseFetchGuards(t *testing.T) {
	var body bytes.Buffer
	body.Write(Pkt("command=fetch"))
	body.Write(Flush())
	body.Write(Pkt("thin-pack"))
	body.Write(Pkt("want " + oidA))
	body.Write(Pkt("want " + oidB))
	body.Write(Pkt("have " + oidP))
	body.Write(Pkt("deepen 1"))
	body.Write(Flush())
	g, err := ParseFetchGuards(body.Bytes())
	if err != nil {
		t.Fatalf("ParseFetchGuards: %v", err)
	}
	if g.Command != "fetch" || len(g.Wants) != 2 || len(g.Haves) != 1 || !g.Deepen {
		t.Errorf("guards = %+v", g)
	}
	// max_wants guard
	l := NewLayer()
	l.MaxWants = 1
	if err := l.CheckMaxWants(g); err == nil {
		t.Error("max_wants not enforced")
	}
}

func TestCheckBundleRequire(t *testing.T) {
	repo, l := lsRefsFixture(t)
	l.BundlesRequire = []string{repo.ID.String()}
	// unbounded zero-have → refusal
	g := &FetchGuards{Wants: []Oid{oidA}}
	err := l.CheckBundleRequire(repo, "alice", g)
	if err == nil {
		t.Fatal("unbounded zero-have fetch allowed for bundles.require repo")
	}
	if !strings.Contains(err.Error(), "requires bundle-uri clones") {
		t.Errorf("err = %v", err)
	}
	// bounded zero-have (depth) proceeds
	g.Deepen = true
	if err := l.CheckBundleRequire(repo, "other", g); err != nil {
		t.Errorf("bounded fetch refused: %v", err)
	}
	// fetch with haves proceeds
	g.Deepen = false
	g.Haves = []Oid{oidP}
	if err := l.CheckBundleRequire(repo, "other", g); err != nil {
		t.Errorf("have-fetch refused: %v", err)
	}
}

func gitRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}
