package pulls

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file exercises SubprocessGit against the REAL git binary (exact argv
// per 03 §4/§5): it proves the documented commands exist with the flags we
// pass and behave the way the mergeability/merge logic assumes (exit codes,
// output shapes). Repos are temp bare repos; commits are built with stock
// plumbing (commit-tree over an empty tree), so no worktree is ever created.

// testGitEnv is the explicit child env for fixture setup (identity pinned
// like TestMain; production SubprocessGit sets author/committer per call).
func testGitEnv(dir string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + dir, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
}

// realRepo builds a bare repo with main=A---B and topic=A---C (diverged),
// returning the dir and shas.
func realRepo(t *testing.T) (dir, base, head, ancestor string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	mustInit := exec.Command("git", "init", "--bare")
	mustInit.Dir = dir
	mustInit.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0"}
	if out, err := mustInit.CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	empty := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	commit := func(tree, parent, msg string) string {
		argv := []string{"commit-tree", tree, "-m", msg}
		if parent != "" {
			argv = append(argv, "-p", parent)
		}
		return run(argv...)
	}
	a := commit(empty, "", "root")
	b := commit(empty, a, "base tip")
	c := commit(empty, a, "head tip")
	for ref, sha := range map[string]string{"refs/heads/main": b, "refs/heads/topic": c} {
		cmd := exec.Command("git", "update-ref", ref, sha)
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("update-ref: %v %s", err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		t.Fatalf("bare repo: %v", err)
	}
	return dir, b, c, a
}

func TestSubprocessGitReal(t *testing.T) {
	dir, base, head, ancestor := realRepo(t)
	g := NewSubprocessGit("")
	g.Timeout = 60 * time.Second
	ctx := context.Background()

	sha, err := g.ResolveRef(ctx, dir, "refs/heads/main")
	if err != nil || sha != base {
		t.Fatalf("resolve = %q %v", sha, err)
	}
	if _, err := g.ResolveRef(ctx, dir, "refs/heads/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref err = %v", err)
	}
	mb, err := g.MergeBase(ctx, dir, base, head)
	if err != nil || mb != ancestor {
		t.Fatalf("merge-base = %q %v", mb, err)
	}
	if _, err := g.MergeBase(ctx, dir, base, "0000000000000000000000000000000000000000"); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("bad merge-base err = %v", err)
	}
	// Ancestry both directions (exit 0 ⇒ true, exit 1 ⇒ false).
	if ok, _ := g.IsAncestor(ctx, dir, ancestor, head); !ok {
		t.Fatal("ancestor must be ancestor of head")
	}
	if ok, _ := g.IsAncestor(ctx, dir, head, base); ok {
		t.Fatal("head must not be ancestor of base")
	}
	// Trial merge of two empty-tree commits: clean (both sides empty ⇒ the
	// would-be tree is the empty tree).
	tree, conflicts, err := g.TrialMerge(ctx, dir, base, head)
	if err != nil {
		t.Fatalf("trial = %v (conflicts %v)", err, conflicts)
	}
	if tree != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" || len(conflicts) != 0 {
		t.Fatalf("trial = %q %v", tree, conflicts)
	}
	// Behind: base has 1 commit head lacks (B), head has 1 base lacks (C)
	// ⇒ rev-list --count head..base = 1.
	behind, err := g.BehindCount(ctx, dir, base, head)
	if err != nil || behind != 1 {
		t.Fatalf("behind = %d %v", behind, err)
	}
	// Reachability: pushed tips reachable; unknown sha not.
	if ok, _ := g.Reachable(ctx, dir, head); !ok {
		t.Fatal("head must be reachable")
	}
	if ok, _ := g.Reachable(ctx, dir, "ffffffffffffffffffffffffffffffffffffffff"); ok {
		t.Fatal("unknown sha must not be reachable")
	}
	// Diff of identical trees is empty but well-formed (exit 0).
	patch, err := g.Diff(ctx, dir, base, head)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if strings.Contains(patch, "fatal") {
		t.Fatalf("patch = %q", patch)
	}
	// Log range base..head lists exactly the head-side commit.
	rows, err := g.LogRange(ctx, dir, base, head, 0, 50)
	if err != nil || len(rows) != 1 || rows[0].Subject != "head tip" {
		t.Fatalf("log = %+v %v", rows, err)
	}
	rows, err = g.LogRange(ctx, dir, base, head, 5, 50)
	if err != nil || len(rows) != 0 {
		t.Fatalf("skip past end = %+v %v", rows, err)
	}
	// LogRange clamps n > 200 without error.
	if _, err := g.LogRange(ctx, dir, base, head, 0, 500); err != nil {
		t.Fatalf("clamp: %v", err)
	}
	subj, err := g.Subject(ctx, dir, head)
	if err != nil || subj != "head tip" {
		t.Fatalf("subject = %q %v", subj, err)
	}
	// CommitTree with explicit identity (author = merger, committer = server).
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	sha, err = g.CommitTree(ctx, dir, tree, []string{base, head}, "Merge it", "merger", "merger@x", "walhub", "walhub@localhost", now)
	if err != nil || len(sha) != 40 {
		t.Fatalf("commit-tree = %q %v", sha, err)
	}
	// Replay is the §5 rebase plumbing verbatim; stock git has no `replay`
	// command, so against real git it errors — the task surfaces the
	// narration (FakeGit covers the success shape).
	if _, err := g.Replay(ctx, dir, base, base, head); err == nil {
		t.Fatal("replay against stock git must fail (no such plumbing)")
	}
	// Unknown binary surfaces unavailable, never a silent result.
	g2 := NewSubprocessGit("/nonexistent/git-binary")
	if _, err := g2.ResolveRef(ctx, dir, "refs/heads/main"); err == nil {
		t.Fatal("bad binary must error")
	}
	// Cancelled context while waiting for a pool slot returns ctx.Err.
	pool := newGitPool(1)
	hold := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = pool.run(context.Background(), func() error {
			close(hold)
			<-release
			return nil
		})
	}()
	<-hold
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pool.run(cctx, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait err = %v", err)
	}
	close(release)
}

func TestSubprocessGitDirtyReal(t *testing.T) {
	// Real conflicting trial merge: both sides add the same path with
	// different content ⇒ exit 1 with conflict paths on stdout.
	dir := t.TempDir()
	must := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	mustInit := exec.Command("git", "init", "--bare")
	mustInit.Dir = dir
	mustInit.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0"}
	if out, err := mustInit.CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	hashObj := func(content string) string {
		cmd := exec.Command("git", "hash-object", "-w", "--stdin")
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	mktree := func(blob, name string) string {
		cmd := exec.Command("git", "mktree")
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		cmd.Stdin = strings.NewReader("100644 blob " + blob + "\t" + name + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("mktree: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(tree, parent, msg string) string {
		argv := []string{"commit-tree", tree, "-m", msg}
		if parent != "" {
			argv = append(argv, "-p", parent)
		}
		return must(argv...)
	}
	root := commit(mktree(hashObj("root\n"), "f.txt"), "", "root")
	b := commit(mktree(hashObj("base\n"), "f.txt"), root, "base")
	h := commit(mktree(hashObj("head\n"), "f.txt"), root, "head")
	g := NewSubprocessGit("")
	g.Timeout = 60 * time.Second
	ctx := context.Background()
	_, conflicts, err := g.TrialMerge(ctx, dir, b, h)
	if !errors.Is(err, errDirty) {
		t.Fatalf("dirty trial err = %v (conflicts %v)", err, conflicts)
	}
	if len(conflicts) == 0 || conflicts[0] != "f.txt" {
		t.Fatalf("conflicts = %v", conflicts)
	}
}
