// util_extra_test.go — the small shared helpers left thin by the fake-based
// suites: config merge (D24), scratch copy (reflink/plain), side-file
// install, oid/byte helpers.
package maintain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestEffectiveConfig_MergeAndErrors(t *testing.T) {
	host := defaultEff()

	// No settings → the host config passes through untouched.
	if got, err := effectiveConfig(host, &proto.Manifest{}); err != nil || got != host {
		t.Fatalf("no settings: %v %v", got, err)
	}
	if got, err := effectiveConfig(host, nil); err != nil || got != host {
		t.Fatalf("nil manifest: %v %v", got, err)
	}

	// Repo settings merge over the host (D24).
	m := &proto.Manifest{Settings: &proto.RepoSettings{
		Toml: "[compaction]\nenabled = false\n",
	}}
	got, err := effectiveConfig(host, m)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.Compaction.Enabled {
		t.Fatal("repo setting must override compaction.enabled")
	}
	if got.Cache.Dir != host.Cache.Dir {
		t.Fatal("unrelated fields must stay host values")
	}

	// Bad TOML surfaces as an error, never a half-merged config.
	m.Settings.Toml = "[compaction\nbroken"
	if _, err := effectiveConfig(host, m); err == nil {
		t.Fatal("bad repo settings must fail")
	}
}

func TestCopyFile_PlainAndReflinkAndResume(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("walhub", 100)), 0o640); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")

	// First copy lands the bytes and keeps the mode.
	if err := copyFile(dst, src); err != nil {
		t.Fatalf("copy: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil || len(body) != 600 {
		t.Fatalf("copy content: len=%d err=%v", len(body), err)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode())
	}

	// A second copy over an existing dst is a resume no-op (§6.2 step 1).
	if err := copyFile(dst, src); err != nil {
		t.Fatalf("resume copy: %v", err)
	}
	if body2, _ := os.ReadFile(dst); string(body2) != string(body) {
		t.Fatal("resume copy must leave the destination as-is")
	}

	// Missing source errors.
	if err := copyFile(filepath.Join(dir, "dst2"), filepath.Join(dir, "ghost")); err == nil {
		t.Fatal("missing src must fail")
	}
}

func TestCopyDir_NestedSymlinkAndErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "repo.git")
	if err := os.MkdirAll(filepath.Join(src, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "objects", "pack", "pack-x.pack"), []byte("packdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../pack-x.pack", filepath.Join(src, "objects", "pack", "pack-x.keep")); err != nil {
		t.Skip("symlinks unsupported")
	}
	dst := filepath.Join(dir, "scratch.git")

	if err := copyDir(dst, src); err != nil {
		t.Fatalf("copyDir: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "objects", "pack", "pack-x.pack")); err != nil || string(body) != "packdata" {
		t.Fatalf("copied pack: %q %v", body, err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "objects", "pack", "pack-x.keep")); err != nil || !strings.HasSuffix(link, "pack-x.pack") {
		t.Fatalf("symlink: %q %v", link, err)
	}
	// Missing source errors (the §6.2 scratch-copy failure path).
	if err := copyDir(filepath.Join(dir, "n"), filepath.Join(dir, "ghost")); err == nil {
		t.Fatal("missing src must fail")
	}
}

func TestInstallSideFile(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "pack-abc.idx")
	if err := os.WriteFile(src, []byte("idxbytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same path is a no-op.
	if err := installSideFile(filepath.Dir(src), src); err != nil {
		t.Fatalf("same-path install: %v", err)
	}
	if err := installSideFile(packDir, src); err != nil {
		t.Fatalf("install: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(packDir, "pack-abc.idx")); err != nil || string(body) != "idxbytes" {
		t.Fatalf("installed file: %q %v", body, err)
	}
	// Missing source errors.
	if err := installSideFile(packDir, filepath.Join(dir, "ghost.idx")); err == nil {
		t.Fatal("missing src must fail")
	}
}

func TestZeroOidAndEqBytes(t *testing.T) {
	if got := zeroOid(&proto.Manifest{ObjectFormat: "sha1"}); got != strings.Repeat("0", 40) {
		t.Fatalf("sha1 zero = %q", got)
	}
	if got := zeroOid(&proto.Manifest{ObjectFormat: "sha256"}); got != strings.Repeat("0", 64) {
		t.Fatalf("sha256 zero = %q", got)
	}
	if got := zeroOid(&proto.Manifest{ObjectFormat: "bogus"}); got != "" {
		t.Fatalf("bogus format zero = %q", got)
	}
	if !eqBytes([]byte("a"), []byte("a")) || eqBytes([]byte("a"), []byte("b")) ||
		!eqBytes(nil, []byte{}) {
		t.Fatal("eqBytes semantics")
	}
}

func TestLocalPackStateAndHasObjectsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pack-aaa.pack", "pack-bbb.pack", "pack-aaa.idx", "unrelated"} {
		if err := os.WriteFile(filepath.Join(dir, "objects", "pack", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep := &fakeRepo{dir: dir}
	st := localPackState(rep)
	if !st.Present["aaa"] || !st.Present["bbb"] || st.Present["bbb.idx"] {
		t.Fatalf("local pack state = %v", st.Present)
	}
	if !hasObjectsDir(dir) || hasObjectsDir(filepath.Join(dir, "ghost")) {
		t.Fatal("hasObjectsDir contract")
	}
}

func TestWriteAtomicAndReadPackFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeAtomic(filepath.Join(dir, "m.json"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, "m.json"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "m.json")); string(body) != "v2" {
		t.Fatalf("writeAtomic overwrite = %q", body)
	}
	// readPackFile: present, absent, and a directory failure.
	packDir := filepath.Join(dir, "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-abc.pack"), []byte("P"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readPackFile(packDir, "ghost", ".pack"); err != nil || ok {
		t.Fatalf("absent side file: ok=%v err=%v", ok, err)
	}
}

func TestChecksumFromPackPathAndPutCreateIfAbsent(t *testing.T) {
	ctx := context.Background()
	// putCreateIfAbsent: nil store is a no-op; create-if-absent tolerates a
	// duplicate create (§6.2 step 5).
	if err := putCreateIfAbsent(ctx, nil, "k", []byte("v")); err != nil {
		t.Fatalf("nil store: %v", err)
	}
	st := newMemStore()
	if err := putCreateIfAbsent(ctx, st, "wal/a.pack", []byte("v1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := putCreateIfAbsent(ctx, st, "wal/a.pack", []byte("v2")); err != nil {
		t.Fatalf("duplicate create must be success: %v", err)
	}
	if body, _, err := store.GetBytes(ctx, st, "wal/a.pack", store.GetOptions{}); err != nil || string(body) != "v1" {
		t.Fatalf("first create must win: %q %v", body, err)
	}
	// checksumFromPackPath parses the canonical pack filename.
	if c := checksumFromPackPath("/x/objects/pack/pack-abc123.pack"); c != "abc123" {
		t.Fatalf("checksum = %q", c)
	}
}
