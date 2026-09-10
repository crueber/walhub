package tags

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitTestRepo builds a real repo: c1 <- c2, lightweight tag v1 (on c1).
// Identity pinned via env (runners have no global git config).
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
	c1 := run("rev-parse", "HEAD")
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-02T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T00:00:00Z")
	run("commit", "-q", "--allow-empty", "-m", "c2")
	run("tag", "v1", c1)
	// The seam contract takes a bare repo path (GIT_DIR-ready); point at
	// .git for this non-bare fixture.
	return filepath.Join(dir, ".git")
}

func TestSubprocessGitCommitExists(t *testing.T) {
	dir := gitTestRepo(t)
	g := NewSubprocessGit("git")
	g.Timeout = 30 * time.Second
	ctx := context.Background()

	c1, err := g.CommitExists(ctx, dir, "HEAD~1")
	if err != nil || len(c1) != 40 {
		t.Fatalf("HEAD~1: %q %v", c1, err)
	}
	head, err := g.CommitExists(ctx, dir, "HEAD")
	if err != nil || len(head) != 40 {
		t.Fatalf("HEAD: %q %v", head, err)
	}
	if c1 == head {
		t.Fatal("c1 == head")
	}
	// Full sha round-trips identically.
	if again, err := g.CommitExists(ctx, dir, c1); err != nil || again != c1 {
		t.Fatalf("sha round-trip: %q %v", again, err)
	}
	// Tag names peel to the commit (the tag points at c1).
	if peeled, err := g.CommitExists(ctx, dir, "v1"); err != nil || peeled != c1 {
		t.Fatalf("tag peel: %q %v", peeled, err)
	}
	// Unknown sha is 404, never a backend failure.
	if _, err := g.CommitExists(ctx, dir, strings.Repeat("f", 40)); !isErr(err, errNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// Malformed input is 404 (git rejects it; the runner never panics).
	if _, err := g.CommitExists(ctx, dir, "not-a-sha!!"); !isErr(err, errNotFound) {
		t.Fatalf("malformed: %v", err)
	}
	// Missing repo dir is a backend outage (503), never unknown-revision.
	if _, err := g.CommitExists(ctx, dir+"/nope", c1); !isErr(err, errUnavailable) {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestSubprocessGitBackendFailures(t *testing.T) {
	dir := gitTestRepo(t)
	ctx := context.Background()
	// Missing binary ⇒ unavailable.
	g := NewSubprocessGit("walhub-no-such-git-binary")
	g.Timeout = 5 * time.Second
	if _, err := g.CommitExists(ctx, dir, strings.Repeat("0", 40)); !isErr(err, errUnavailable) {
		t.Fatalf("bad binary: %v", err)
	}
	// Canceled context while the pool is occupied ⇒ unavailable.
	g2 := NewSubprocessGit("git")
	g2.Pool.sem <- struct{}{} // occupy the single... (cap is GOMAXPROCS-wide; drain below instead)
	for len(g2.Pool.sem) < cap(g2.Pool.sem) {
		g2.Pool.sem <- struct{}{}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := g2.CommitExists(cctx, dir, strings.Repeat("0", 40)); !isErr(err, errUnavailable) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestCommitExistsNonSHAOutput(t *testing.T) {
	// A binary that answers non-sha output exercises the validateSHA
	// failure path (→ unknown revision, never a bad publish).
	g := NewSubprocessGit("echo")
	g.Timeout = 5 * time.Second
	if _, err := g.CommitExists(context.Background(), t.TempDir(), "HEAD"); !isErr(err, errNotFound) {
		t.Fatalf("echo output: %v", err)
	}
}

func TestSubprocessGitTagObjectRoundTrip(t *testing.T) {
	dir := gitTestRepo(t)
	g := NewSubprocessGit("git")
	g.Timeout = 30 * time.Second
	ctx := context.Background()

	head, err := g.CommitExists(ctx, dir, "HEAD")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	// Mint via mktag; the tagger line is server-rendered, the message exact.
	body := renderTagBody("v9", head, "t <t@walhub.local> 1789041600 +0000", "hello\n")
	oid, err := g.CreateTagObject(ctx, dir, body)
	if err != nil {
		t.Fatalf("CreateTagObject: %v", err)
	}
	if verr := validateSHA(oid); verr != nil {
		t.Fatalf("oid = %q", oid)
	}
	// git sees a real tag object carrying the tagger line and message.
	if typ, err := g.runCollect(ctx, dir, []string{"cat-file", "-t", oid}); err != nil || strings.TrimSpace(typ) != "tag" {
		t.Fatalf("cat-file -t = %q %v", typ, err)
	}
	if raw, err := g.runCollect(ctx, dir, []string{"cat-file", "tag", oid}); err != nil ||
		!strings.Contains(raw, "tagger t <t@walhub.local> 1789041600 +0000") ||
		!strings.Contains(raw, "hello") {
		t.Fatalf("cat-file tag = %q %v", raw, err)
	}
	// Pack the single object; the bytes are a real pack (PACK magic).
	pack, err := g.PackObject(ctx, dir, oid)
	if err != nil {
		t.Fatalf("PackObject: %v", err)
	}
	if len(pack) < 4 || string(pack[:4]) != "PACK" {
		t.Fatalf("pack magic = %q (len %d)", pack, len(pack))
	}
	// Malformed tag content is an mktag rejection (400-class), never stored.
	if _, err := g.CreateTagObject(ctx, dir, []byte("this is not a tag object\n")); !isErr(err, errInvalid) {
		t.Fatalf("garbage mktag err = %v, want ErrInvalid", err)
	}
	// Unknown object oid fails pack-objects (5xx-class, nothing published).
	if _, err := g.PackObject(ctx, dir, strings.Repeat("0", 40)); !isErr(err, errUnavailable) {
		t.Fatalf("zero-oid pack err = %v, want ErrUnavailable", err)
	}
	// Bad oid never spawns (400-class input validation).
	if _, err := g.PackObject(ctx, dir, "nope"); !isErr(err, errInvalid) {
		t.Fatalf("bad-oid pack err = %v, want ErrInvalid", err)
	}
}

func TestTagObjectBackendFailures(t *testing.T) {
	ctx := context.Background()
	g := NewSubprocessGit("walhub-no-such-git-binary")
	g.Timeout = 5 * time.Second
	if _, err := g.CreateTagObject(ctx, t.TempDir(), []byte("x")); !isErr(err, errUnavailable) {
		t.Fatalf("mktag bad binary: %v", err)
	}
	if _, err := g.PackObject(ctx, t.TempDir(), strings.Repeat("a", 40)); !isErr(err, errUnavailable) {
		t.Fatalf("pack bad binary: %v", err)
	}
	// A binary answering non-sha output exercises the validateSHA path.
	ge := NewSubprocessGit("echo")
	ge.Timeout = 5 * time.Second
	if _, err := ge.CreateTagObject(ctx, t.TempDir(), []byte("x")); !isErr(err, errInvalid) {
		t.Fatalf("echo mktag: %v", err)
	}
	// A binary answering empty output exercises the empty-pack path.
	gt := NewSubprocessGit("true")
	gt.Timeout = 5 * time.Second
	if _, err := gt.PackObject(ctx, t.TempDir(), strings.Repeat("a", 40)); !isErr(err, errUnavailable) {
		t.Fatalf("empty pack: %v", err)
	}
}

func TestValidateSHA(t *testing.T) {
	if err := validateSHA(strings.Repeat("a", 40)); err != nil {
		t.Fatalf("40-hex: %v", err)
	}
	if err := validateSHA(strings.Repeat("A", 64)); err != nil {
		t.Fatalf("64-hex: %v", err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("z", 40), strings.Repeat("0", 39), strings.Repeat("0", 65)} {
		if err := validateSHA(bad); err == nil {
			t.Fatalf("validateSHA(%q) = nil", bad)
		}
	}
}

func TestBoundedStderrTruncates(t *testing.T) {
	var b boundedStderr
	n, _ := b.Write([]byte(strings.Repeat("x", 9000)))
	if n != 8192 {
		t.Fatalf("n = %d", n)
	}
	if b.buf.Len() != 8192 {
		t.Fatalf("len = %d, want 8192", b.buf.Len())
	}
	if s := b.String(); len(s) != 8192 {
		t.Fatalf("string len = %d", len(s))
	}
}

func TestGitExitError(t *testing.T) {
	e := &gitExitError{argv: []string{"rev-parse", "x"}, errText: "bad", err: context.DeadlineExceeded, stdout: ""}
	if s := e.Error(); !strings.Contains(s, "rev-parse") || !strings.Contains(s, "bad") {
		t.Fatalf("Error() = %q", s)
	}
}
