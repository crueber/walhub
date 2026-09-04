package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Bad-dir matrix: every method surfaces a backend error (never a verdict,
// never a panic) when the repo dir does not exist.
func TestCoverGitBadDir(t *testing.T) {
	g := NewSubprocessGit("")
	g.Timeout = 30 * time.Second
	ctx := context.Background()
	bad := "/nonexistent/repo-dir"
	if _, err := g.ResolveRef(ctx, bad, "refs/heads/main"); err == nil {
		t.Fatal("resolve")
	}
	if _, err := g.MergeBase(ctx, bad, "a", "b"); err == nil {
		t.Fatal("merge-base")
	}
	if _, err := g.IsAncestor(ctx, bad, "a", "b"); err == nil {
		t.Fatal("is-ancestor with bad dir must error (not silently false)")
	}
	if _, _, err := g.TrialMerge(ctx, bad, "a", "b"); err == nil || errors.Is(err, errDirty) {
		t.Fatalf("trial: %v", err)
	}
	if _, err := g.CommitTree(ctx, bad, "t", []string{"p"}, "m", "a", "a@x", "c", "c@x", time.Now()); err == nil {
		t.Fatal("commit-tree")
	}
	if _, err := g.Replay(ctx, bad, "o", "b", "h", "w", "w@x"); err == nil {
		t.Fatal("replay")
	}
	if _, err := g.BehindCount(ctx, bad, "b", "h"); err == nil {
		t.Fatal("behind")
	}
	if _, err := g.Reachable(ctx, bad, "abc"); err == nil {
		t.Fatal("reachable")
	}
	if _, err := g.Diff(ctx, bad, "b", "h"); err == nil {
		t.Fatal("diff")
	}
	if _, err := g.LogRange(ctx, bad, "b", "h", 0, 10); err == nil {
		t.Fatal("log")
	}
	if _, err := g.Subject(ctx, bad, "abc"); err == nil {
		t.Fatal("subject")
	}
}

func TestCoverGitTimeoutAndBinary(t *testing.T) {
	dir, base, head, _ := realRepo(t)
	// Cancelled context while the pool slot is held ⇒ unavailable, even
	// for a valid command.
	g := NewSubprocessGit("")
	g.Timeout = 30 * time.Second
	g.Pool = newGitPool(1)
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = g.Pool.run(context.Background(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.ResolveRef(cctx, dir, "refs/heads/main"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
	close(release)
	// Missing binary ⇒ unavailable on every verb (never a false verdict).
	g2 := NewSubprocessGit("/nonexistent/git-binary")
	g2.Timeout = 30 * time.Second
	ctx := context.Background()
	if ok, err := g2.IsAncestor(ctx, dir, base, head); err == nil || ok {
		t.Fatalf("binary is-ancestor: %v %v", ok, err)
	}
	if _, _, err := g2.TrialMerge(ctx, dir, base, head); err == nil || errors.Is(err, errDirty) {
		t.Fatalf("binary trial: %v", err)
	}
	if _, err := g2.MergeBase(ctx, dir, base, head); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("binary merge-base: %v", err)
	}
	_, _, _ = base, head, dir
}

func TestCoverGitReplayBranches(t *testing.T) {
	dir, base, head, _ := realRepo(t)
	g := NewSubprocessGit("")
	g.Timeout = 60 * time.Second
	ctx := context.Background()
	// MergeBase failure ⇒ replay error (bad sha).
	if _, err := g.Replay(ctx, dir, "zzzz", base, head, "w", "w@x"); err == nil {
		t.Fatal("replay bad onto")
	}
	// Replay succeeds on stock git (experimental replay + temp branch +
	// --ref-action=print): the tip lands on base with the server committer.
	tip, err := g.Replay(ctx, dir, base, base, head, "walhub", "walhub@localhost")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if ok, _ := g.IsAncestor(ctx, dir, base, tip); !ok {
		t.Fatalf("tip %s must sit on base", tip)
	}
	out, err := g.runCollect(ctx, dir, []string{"log", "-1", "--format=%cn %ce", tip}, "", nil)
	if err != nil || strings.TrimSpace(out) != "walhub walhub@localhost" {
		t.Fatalf("tip committer = %q %v", out, err)
	}
}

func TestCoverBoundedStderr(t *testing.T) {
	var b boundedStderr
	if _, err := b.Write([]byte("short")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b.String() != "short" {
		t.Fatalf("s = %q", b.String())
	}
	// Oversize single write keeps the tail and marks full ("…" is 3 bytes).
	big := strings.Repeat("x", 9000)
	if _, err := b.Write([]byte(big)); err != nil {
		t.Fatalf("big: %v", err)
	}
	if s := b.String(); !strings.HasPrefix(s, "…") || len(s) != 3+8192 {
		t.Fatalf("tail len = %d", len(s))
	}
	// Incremental overflow keeps the tail too.
	var b2 boundedStderr
	for i := 0; i < 20; i++ {
		_, _ = b2.Write([]byte(strings.Repeat("y", 1000)))
	}
	if s := b2.String(); !strings.HasPrefix(s, "…") || len(s) != 3+8192 {
		t.Fatalf("tail2 len = %d", len(s))
	}
}

func TestCoverSplitMergeTreeEdge(t *testing.T) {
	if tree, paths := splitMergeTree(""); tree != "" || paths != nil {
		t.Fatalf("empty: %q %v", tree, paths)
	}
	if tree, paths := splitMergeTree("\n\n"); tree != "" || paths != nil {
		t.Fatalf("blank: %q %v", tree, paths)
	}
	tree, paths := splitMergeTree("abc123\nf.txt\n\ngarbage\n")
	if tree != "abc123" || len(paths) != 1 || paths[0] != "f.txt" {
		t.Fatalf("parse: %q %v", tree, paths)
	}
}
