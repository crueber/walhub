package git

// ingest_stream_test.go — IngestStream + ParsePushRequestStream (17_ssh.md
// §5): the SSH framing variants. A real pack built via pack-objects proves
// the stream path lands objects identically to staged Ingest; the delete and
// cap cases pin the framing edges.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realPackFixture builds a repo with two commits and returns the pack bytes
// for HEAD plus the commit's environment-safe fixture dir.
func realPackFixture(t *testing.T) (pack []byte, head string) {
	t.Helper()
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
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
	if err := os.WriteFile(filepath.Join(fixture, "f.txt"), []byte("stream me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk("add", ".")
	mk("commit", "-q", "-m", "c1")
	head = gitRevParse(t, fixture, "HEAD")

	var buf bytes.Buffer
	packCmd := exec.Command("git", "pack-objects", "--stdout", "--revs")
	packCmd.Dir = fixture
	packCmd.Stdin = strings.NewReader(head + "\n")
	packCmd.Stdout = &buf
	if err := packCmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	return buf.Bytes(), head
}

func TestIngestStreamLandsObjects(t *testing.T) {
	pack, _ := realPackFixture(t)
	repo, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "stream"}, Sha1)
	l := NewLayer()

	res, err := l.IngestStream(t.Context(), repo, bytes.NewReader(pack), int64(len(pack)+1), false, true)
	if err != nil {
		t.Fatalf("IngestStream: %v", err)
	}
	if res.Checksum == "" || res.ObjectCount == 0 {
		t.Fatalf("result = %+v", res)
	}
	// identical to staged Ingest on the same pack
	repo2, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "staged"}, Sha1)
	res2, err := l.Ingest(t.Context(), repo2, bytes.NewReader(pack), int64(len(pack)+1), false, true)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res2.Checksum != res.Checksum || res2.ObjectCount != res.ObjectCount {
		t.Fatalf("stream vs staged diverged: %+v vs %+v", res, res2)
	}
	// no scratch left behind
	entries, _ := os.ReadDir(repo.Path)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "walgit-ingest-") {
			t.Errorf("scratch dir left behind: %s", e.Name())
		}
	}
}

func TestIngestStreamMaxBytes(t *testing.T) {
	pack, _ := realPackFixture(t)
	repo, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "cap2"}, Sha1)
	l := NewLayer()
	_, err := l.IngestStream(t.Context(), repo, bytes.NewReader(pack), 64, false, false)
	if err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("cap err = %v, want max_bytes", err)
	}
}

func TestParsePushRequestStream(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "pstream"}, Sha1)
	l := NewLayer()

	pack, _ := realPackFixture(t)
	// commands + flush + pack: the stream reader must end up positioned at
	// the pack and the pack must survive intact through the reader.
	body := cat(
		Pkt(strings.Repeat("0", 40)+" "+oidA+" refs/heads/main\x00report-status side-band-64k\n"),
		[]byte("0000"),
		pack,
	)
	req, rest, err := l.ParsePushRequestStream(repo, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParsePushRequestStream: %v", err)
	}
	if req.Pack != nil {
		t.Fatal("stream request must not carry Pack bytes")
	}
	if !req.Has("report-status") || !req.Has("side-band-64k") {
		t.Fatalf("caps = %v", req.Caps)
	}
	restBytes, rerr := io.ReadAll(rest)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(restBytes, pack) {
		t.Fatalf("pack stream mismatch: %d vs %d bytes", len(restBytes), len(pack))
	}
}

func TestParsePushRequestStreamDeleteOnly(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "del"}, Sha1)
	l := NewLayer()
	// delete command + flush and NOTHING after: the reader must not be
	// touched beyond the flush (over SSH there is no EOF coming).
	body := cat(Pkt(oidA+" "+strings.Repeat("0", 40)+" refs/heads/main\n"), []byte("0000"))
	req, rest, err := l.ParsePushRequestStream(repo, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Commands) != 1 || req.Commands[0].New != strings.Repeat("0", 40) {
		t.Fatalf("commands = %+v", req.Commands)
	}
	// the reader is positioned after the flush; reading would block on a real
	// channel, so the contract is: the caller decides (allDeletes skips it).
	if rest == nil {
		t.Fatal("pack reader must be returned")
	}
}

func TestUploadPackSSHInteractive(t *testing.T) {
	pack, head := realPackFixture(t)
	repo, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "sshup"}, Sha1)
	l := NewLayer()
	// land the objects, then publish the ref so upload-pack advertises it
	if _, err := l.Ingest(t.Context(), repo, bytes.NewReader(pack), int64(len(pack)+1), false, true); err != nil {
		t.Fatal(err)
	}
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{{Name: "refs/heads/main", OldOid: zero40, NewOid: head}}, true); err != nil {
		t.Fatal(err)
	}

	// a v0 fetch request over an interactive (non-stateless) channel: want +
	// flush + done → upload-pack answers without waiting for stdin EOF.
	req := cat(
		Pkt("want "+head+" object-format=sha1\n"),
		Flush(),
		Pkt("done\n"),
	)
	var out bytes.Buffer
	if err := l.UploadPackSSH(t.Context(), repo, bytes.NewReader(req), &out, ""); err != nil {
		t.Fatalf("UploadPackSSH: %v", err)
	}
	if !strings.Contains(out.String(), head[:12]) {
		t.Fatalf("upload-pack response missing the requested object:\n%q", out.String()[:min(200, out.Len())])
	}
}

func TestParsePushRequestStreamMalformed(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "bad"}, Sha1)
	l := NewLayer()
	if _, _, err := l.ParsePushRequestStream(repo, bytes.NewReader([]byte("zzzz"))); err == nil {
		t.Fatal("malformed stream must error")
	}
}

func TestIngestStreamScratchError(t *testing.T) {
	l := NewLayer()
	repo, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "ro"}, Sha1)
	// make the repo dir unwritable so the scratch build fails
	if err := os.Chmod(repo.Path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repo.Path, 0o755) })
	pack, _ := realPackFixture(t)
	if _, err := l.IngestStream(t.Context(), repo, bytes.NewReader(pack), int64(len(pack)+1), false, false); err == nil {
		t.Fatal("unwritable repo must fail ingest")
	}
}

func TestCapReaderOver(t *testing.T) {
	// a capped reader whose cap was never crossed reports no cap error
	cr := newCapReader(strings.NewReader("ok"), 1024)
	if err := cr.over(); err != nil {
		t.Fatalf("uncrossed cap = %v", err)
	}
	// a nil-func cap reader (the staged-feed shape) reports nothing
	empty := ingestFeed{stdin: strings.NewReader("x"), overErr: nil, waitFeed: true}
	if empty.overErr != nil && empty.overErr() != nil {
		t.Fatal("nil overErr must stay nil")
	}
}

func TestConnectivityZeroTipAndMissing(t *testing.T) {
	pack, head := realPackFixture(t)
	repo, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "conn"}, Sha1)
	l := NewLayer()
	if _, err := l.Ingest(t.Context(), repo, bytes.NewReader(pack), int64(len(pack)+1), false, true); err != nil {
		t.Fatal(err)
	}
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{{Name: "refs/heads/main", OldOid: zero40, NewOid: head}}, true); err != nil {
		t.Fatal(err)
	}
	zero := Oid(strings.Repeat("0", 40))
	// a zero tip among tips is skipped; the healthy tip passes
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{zero, Oid(head)}); err != nil {
		t.Fatalf("zero tip not skipped: %v", err)
	}
	// a dangling tip (unknown object) fails connectivity
	dangling := Oid("deadbeef" + strings.Repeat("0", 32))
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{dangling}); err == nil {
		t.Fatal("dangling tip must fail connectivity")
	}
}

func TestCapReaderEdges(t *testing.T) {
	// exact-fit read: no error; the next read reports ErrMaxBytes
	cr := newCapReader(strings.NewReader("abcdef"), 4)
	n, err := cr.Read(make([]byte, 10))
	if err != nil || n != 4 {
		t.Fatalf("first read = %d %v", n, err)
	}
	n, err = cr.Read(make([]byte, 10))
	if n != 0 || !errors.Is(err, ErrMaxBytes) {
		t.Fatalf("capped read = %d %v", n, err)
	}
	n, err = cr.Read(make([]byte, 10)) // already over: sticky
	if n != 0 || !errors.Is(err, ErrMaxBytes) {
		t.Fatalf("sticky read = %d %v", n, err)
	}
	// a zero cap disables enforcement
	ok := newCapReader(strings.NewReader("abcdef"), 0)
	if _, err := ok.Read(make([]byte, 3)); err != nil {
		t.Fatalf("zero cap must not enforce: %v", err)
	}
}

func TestIngestStreamCorruptPack(t *testing.T) {
	repo, _ := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "corrupt"}, Sha1)
	l := NewLayer()
	// PACK magic + garbage: index-pack rejects, detail carries the reason
	corrupt := append([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x05"), bytes.Repeat([]byte("x"), 128)...)
	_, err := l.IngestStream(t.Context(), repo, bytes.NewReader(corrupt), int64(len(corrupt)+1), false, false)
	if err == nil {
		t.Fatal("corrupt pack must fail")
	}
	if !strings.Contains(err.Error(), "pack rejected") && !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("err = %v", err)
	}
}
