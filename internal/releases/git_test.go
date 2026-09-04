package releases

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitTestRepo builds a real repo: c1 <- c2, tags v1 (on c1) and v2
// (annotated, on c2). Identity pinned via env (runners have no global git
// config — the AGENTS field lesson).
func gitTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary absent")
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t.t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t.t")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")
	run("commit", "-q", "--allow-empty", "-m", "c1")
	run("tag", "v1")
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-02T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T00:00:00Z")
	run("commit", "-q", "--allow-empty", "-m", "c2")
	run("tag", "-a", "v2", "-m", "annotated")
	// The seam contract takes a bare repo path (GIT_DIR-ready); point at
	// .git for this non-bare fixture.
	return filepath.Join(dir, ".git")
}

func TestSubprocessGitReal(t *testing.T) {
	dir := gitTestRepo(t)
	g := NewSubprocessGit("git")
	g.Timeout = 30 * time.Second
	ctx := context.Background()

	v1, err := g.ResolveRef(ctx, dir, "refs/tags/v1")
	if err != nil || len(v1) != 40 {
		t.Fatalf("v1: %q %v", v1, err)
	}
	v2, err := g.ResolveRef(ctx, dir, "refs/tags/v2")
	if err != nil || len(v2) != 40 {
		t.Fatalf("annotated v2: %q %v", v2, err)
	}
	if v1 == v2 {
		t.Fatal("v1 == v2")
	}
	if _, err := g.ResolveRef(ctx, dir, "refs/tags/nope"); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	ok, err := g.IsAncestor(ctx, dir, v1, v2)
	if err != nil || !ok {
		t.Fatalf("ancestor: %v %v", ok, err)
	}
	ok, err = g.IsAncestor(ctx, dir, v2, v1)
	if err != nil || ok {
		t.Fatalf("non-ancestor: %v %v", ok, err)
	}
	tags, err := g.ListTags(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "v2" || tags[1] != "v1" {
		t.Fatalf("tag order: %v", tags)
	}
	// Canceled context never runs.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := g.ResolveRef(cctx, dir, "refs/tags/v1"); err == nil {
		t.Fatal("canceled ctx ran")
	}
}

func TestValidateSHA(t *testing.T) {
	if err := validateSHA(strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if err := validateSHA(strings.Repeat("A", 64)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("z", 40), strings.Repeat("a", 41)} {
		if validateSHA(bad) == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
