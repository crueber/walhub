package maintain

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRevWriter_MatchesGit is the hard correctness gate for unit 5: build a
// real pack with git, generate the .rev both ways, compare bytes. Skipped
// when git is unavailable.
func TestRevWriter_MatchesGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) []byte {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return out
	}
	run("init", "-q", "-b", "main", ".")
	for i := range 12 {
		f := filepath.Join(dir, "f"+string(rune('a'+i))+".txt")
		if werr := os.WriteFile(f, bytes.Repeat([]byte{byte('a' + i)}, 200+i), 0o644); werr != nil {
			t.Fatal(werr)
		}
		run("add", ".")
		run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "c")
	}
	run("gc", "-q")
	matches, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "pack-*.idx"))
	if len(matches) == 0 {
		t.Fatal("no pack produced")
	}
	idxPath := matches[0]
	idx, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	base := idxPath[:len(idxPath)-4]

	// git's own .rev: index-pack --rev-index on a copy of the pack in a
	// scratch (index-pack refuses in-repo re-runs).
	scratch := t.TempDir()
	pk := filepath.Join(scratch, "p.pack")
	if werr := os.WriteFile(pk, packBytes(t, base), 0o644); werr != nil {
		t.Fatal(werr)
	}
	run("-C", scratch, "init", "-q", "--bare")
	run("-C", scratch, "index-pack", "--rev-index", pk)
	want := mustRead(t, filepath.Join(scratch, "p.rev"))

	oidLen := 20
	got, err := buildRevFile(idx, oidLen)
	if err != nil {
		t.Fatalf("buildRevFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf(".rev mismatch:\n got (%d bytes) head=%x tail=%x\nwant (%d bytes) head=%x tail=%x",
			len(got), head(got, 16), tail(got, 40), len(want), head(want, 16), tail(want, 40))
	}
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func packBytes(t *testing.T, base string) []byte {
	t.Helper()
	p, err := os.ReadFile(base + ".pack")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRevWriter_SyntheticRoundTrip: a hand-built idx validates the writer's
// ordering + trailer math without git (offset order → idx order mapping).
func TestRevWriter_SyntheticRoundTrip(t *testing.T) {
	// Build a v2 idx with 5 objects at known offsets (descending insertion,
	// offsets scattered).
	const n = 5
	offsets := []uint32{900, 100, 500, 300, 700}
	oids := make([][]byte, n)
	for i := range oids {
		oids[i] = []byte{byte(i)}
		oids[i] = sha1Sum(oids[i])
	}
	var buf bytes.Buffer
	buf.Write([]byte("\xfftOc"))
	buf.Write(u32be(2))
	// The writer only reads fanout[255], the offset table, and the trailer;
	// fill the fanout with zeros + the total count at 255.
	fanout := make([]byte, 1024)
	binary.BigEndian.PutUint32(fanout[4*255:], n)
	buf2 := bytes.NewBuffer(nil)
	buf2.Write([]byte("\xfftOc"))
	buf2.Write(u32be(2))
	buf2.Write(fanout)
	for _, o := range oids {
		buf2.Write(o)
	}
	for range n { // crc table (unused)
		buf2.Write(u32be(0))
	}
	for _, off := range offsets {
		buf2.Write(u32be(off))
	}
	// trailer: pack checksum + idx checksum (writer only needs the pack half)
	packChecksum := sha1Sum([]byte("pack"))
	buf2.Write(packChecksum)
	buf2.Write(sha1Sum([]byte("idx")))
	idx := buf2.Bytes()

	got, err := buildRevFile(idx, 20)
	if err != nil {
		t.Fatalf("buildRevFile: %v", err)
	}
	// Offsets ascending: idx-index 1 (100), 3 (300), 2 (500), 4 (700), 0 (900).
	wantRev := []uint32{1, 3, 2, 4, 0}
	if len(got) != 12+4*n+40 {
		t.Fatalf("len = %d, want %d", len(got), 12+4*n+40)
	}
	if string(got[:4]) != string(revMagic[:]) {
		t.Fatalf("magic = %x", got[:4])
	}
	for i, w := range wantRev {
		g := binary.BigEndian.Uint32(got[12+4*i:])
		if g != w {
			t.Fatalf("entry %d = %d, want %d", i, g, w)
		}
	}
	// Trailer: pack checksum + sha1 of everything before it.
	if !bytes.Equal(got[len(got)-40:len(got)-20], packChecksum) {
		t.Fatal("trailer must carry the pack checksum")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func u32be(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}
