// git_test.go — the mirror git runner: argv discipline, scrubbing,
// enumeration, ancestry, and format detection against fixture repos.
package mirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// initUpstream builds a one-commit fixture repo and returns its dir.
func initUpstream(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitOut(t, dir, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "-m", "a")
	return dir
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "-m", msg)
}

func TestCredentialShapes(t *testing.T) {
	if got := CredentialEnv("acme/m:1.2"); got != "WALGIT_MIRROR_TOKEN_acme_m_1_2" {
		t.Fatalf("env = %q", got)
	}
	if argv := credentialArgv("https", "example.com", ""); argv != nil {
		t.Fatalf("empty env argv = %v", argv)
	}
	argv := credentialArgv("https", "example.com", "WALGIT_MIRROR_TOKEN_x")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "credential.https://example.com.helper") || !strings.Contains(joined, "WALGIT_MIRROR_TOKEN_x") {
		t.Fatalf("argv = %v", argv)
	}
}

func TestScrubText(t *testing.T) {
	in := "clone https://user:password=hunter2@example.com/x.git failed token=abc passwd=def password=ghi"
	out := scrubText(in)
	for _, leak := range []string{"hunter2", "abc", "def", "ghi"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leak %q in %q", leak, out)
		}
	}
	if scrubText("clean line") != "clean line" {
		t.Fatal("clean line mangled")
	}
}

func TestRunnerGitOps(t *testing.T) {
	ctx := context.Background()
	up := initUpstream(t)
	commitFile(t, up, "g.txt", "b\n", "b")
	r := NewRunner("", t.TempDir(), 0, 0)
	if r.Binary != "git" {
		t.Fatalf("binary = %q", r.Binary)
	}
	scratch, err := r.ScratchDir("acme", "m")
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	defer os.RemoveAll(scratch)
	dir := filepath.Join(scratch, "one")
	if err := r.CloneMirror(ctx, "file://"+up, dir, "file", "", "", ""); err != nil {
		t.Fatalf("clone: %v", err)
	}
	refs, err := r.ForEachRef(ctx, dir)
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	var mainOid string
	for _, ref := range refs {
		if ref.Name == "refs/heads/main" {
			mainOid = ref.Oid
		}
	}
	if mainOid == "" {
		t.Fatalf("no main in %+v", refs)
	}
	if f, err := r.ShowObjectFormat(ctx, dir); err != nil || f != "sha1" {
		t.Fatalf("format = %q %v", f, err)
	}
	if ht := HeadTarget(dir); ht != "refs/heads/main" {
		t.Fatalf("head = %q", ht)
	}
	// Ancestry: main's parent is an ancestor; the reverse is not.
	parent := strings.TrimSpace(gitOut(t, up, "rev-parse", "main~1"))
	anc, err := r.MergeBaseIsAncestor(ctx, dir, parent, mainOid)
	if err != nil || !anc {
		t.Fatalf("ancestor = %v %v", anc, err)
	}
	anc, err = r.MergeBaseIsAncestor(ctx, dir, mainOid, parent)
	if err != nil || anc {
		t.Fatalf("reverse ancestor = %v %v", anc, err)
	}
	// ForEachRef on a non-repo errors; merge-base with a missing
	// binary errors (non-exit error, not a refusal).
	if _, err := r.ForEachRef(ctx, t.TempDir()); err == nil {
		t.Fatal("for-each-ref on empty dir succeeded")
	}
	if _, err := r.ShowObjectFormat(ctx, t.TempDir()); err == nil {
		t.Fatal("format on empty dir succeeded")
	}
	broken := NewRunner("nonexistent-git-binary-xyz", t.TempDir(), 0, 0)
	if _, err := broken.MergeBaseIsAncestor(ctx, dir, parent, mainOid); err == nil {
		t.Fatal("merge-base with missing binary succeeded")
	}
	// Clone of a missing source fails scrubbed.
	if err := r.CloneMirror(ctx, "file:///nonexistent-xyz-abc", filepath.Join(scratch, "two"), "file", "", "", ""); err == nil {
		t.Fatal("clone of missing source succeeded")
	}
	// ScratchDir under a file fails.
	f, _ := os.CreateTemp(t.TempDir(), "f")
	f.Close()
	rf := NewRunner("git", f.Name(), 0, 0)
	if _, err := rf.ScratchDir("a", "b"); err == nil {
		t.Fatal("scratch under file succeeded")
	}
	// Canceled ctx escapes the pool wait (deterministic: the pool is
	// filled first, so the select can only take the ctx branch).
	for i := 0; i < cap(r.pool); i++ {
		r.pool <- struct{}{}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := r.collect(cctx, dir, []string{"rev-parse", "HEAD"}, nil); err != context.Canceled {
		t.Fatalf("canceled collect = %v", err)
	}
	for i := 0; i < cap(r.pool); i++ {
		<-r.pool
	}
	// HeadTarget on missing HEAD.
	if ht := HeadTarget(t.TempDir()); ht != "" {
		t.Fatalf("head of empty = %q", ht)
	}
	// EnsurePackIdx regenerates a missing idx.
	packs, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if len(packs) == 0 {
		t.Fatal("no packs in scratch clone")
	}
	idx := strings.TrimSuffix(packs[0], ".pack") + ".idx"
	os.Remove(idx)
	if _, err := r.EnsurePackIdx(ctx, packs[0]); err != nil {
		t.Fatalf("ensure idx: %v", err)
	}
	if _, err := r.EnsurePackIdx(ctx, packs[0]); err != nil {
		t.Fatalf("ensure idx cached: %v", err)
	}
	if _, err := r.EnsurePackIdx(ctx, filepath.Join(t.TempDir(), "nope.pack")); err == nil {
		t.Fatal("ensure idx on missing pack succeeded")
	}
	_ = time.Now
}
