package pulls

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end proof for the §7 fork→base object bridge (issue #456) with the
// REAL git binary: a cross-fork PR whose head holds a fork-unique commit
// opens (no 503), diffs, computes mergeability, and merges — all git work
// running in the base serving copy after the on-demand fetch. The WAL ref
// publish stays faked (FakeRefs): the bridge is a serving-copy-local
// operation, orthogonal to bucket sync.

// staticDirs maps repo ids to prebuilt real bare dirs (no WAL sync — the
// serving-copy layout the bridge operates on).
type staticDirs struct{ m map[string]string }

func (d *staticDirs) Dir(_ context.Context, repo string) (string, error) {
	if dir, ok := d.m[repo]; ok {
		return dir, nil
	}
	return "", errNoSuchRepo(repo)
}

func errNoSuchRepo(repo string) error { return &repoMissing{repo} }

type repoMissing struct{ repo string }

func (e *repoMissing) Error() string { return "no repo " + e.repo }

// bridgeForge builds two real bare repos: base with main=A---B, fork sharing
// A with a unique topic commit C carrying real file content. Returns the
// dirs and shas (a, b, c).
func bridgeForge(t *testing.T) (baseDir, forkDir, a, b, c string) {
	t.Helper()
	baseDir = t.TempDir()
	forkDir = t.TempDir()
	for _, dir := range []string{baseDir, forkDir} {
		cmd := exec.Command("git", "init", "--bare", "-q")
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("init: %v %s", err, out)
		}
	}
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	writeFile := func(content string) string {
		p := filepath.Join(t.TempDir(), "blob")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mktree := func(dir, spec string) string {
		cmd := exec.Command("git", "mktree")
		cmd.Dir = dir
		cmd.Env = testGitEnv(dir)
		cmd.Stdin = strings.NewReader(spec)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("mktree: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	empty := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	a = run(baseDir, "commit-tree", empty, "-m", "root")
	baseBlob := run(baseDir, "hash-object", "-w", writeFile("base\n"))
	baseTree := mktree(baseDir, "100644 blob "+baseBlob+"\tf.txt\n")
	b = run(baseDir, "commit-tree", baseTree, "-p", a, "-m", "base tip")
	run(baseDir, "update-ref", "refs/heads/main", b)
	// Fork: same A (fetch the ref so the fork serving copy mirrors a real
	// fork's shared packs), then a UNIQUE commit C on topic.
	run(forkDir, "fetch", "-q", baseDir, "refs/heads/main:refs/heads/main")
	// UNIQUE commit C on topic: touches only g.txt (a disjoint addition),
	// so the trial merge against B is clean and the PR reads behind.
	extraBlob := run(forkDir, "hash-object", "-w", writeFile("new\n"))
	forkTree := mktree(forkDir, "100644 blob "+extraBlob+"\tg.txt\n")
	c = run(forkDir, "commit-tree", forkTree, "-p", a, "-m", "fork tip")
	run(forkDir, "update-ref", "refs/heads/topic", c)
	return baseDir, forkDir, a, b, c
}

// bridgeEnv wires a Service over the forged dirs with the REAL git runner.
func bridgeEnv(baseDir, forkDir string) *testEnv {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	real := NewSubprocessGit("")
	real.Timeout = 60 * time.Second
	e.svc.Git = real
	e.svc.Dirs = &staticDirs{m: map[string]string{"o/r": baseDir, "f/r": forkDir}}
	return e
}

func TestBridgeFetchIntoReal(t *testing.T) {
	baseDir, forkDir, _, _, c := bridgeForge(t)
	g := NewSubprocessGit("")
	g.Timeout = 60 * time.Second
	ctx := context.Background()
	// Unknown sha in the source: a bridge failure (never a verdict).
	if err := g.FetchInto(ctx, baseDir, forkDir, strings.Repeat("f", 40)); err == nil {
		t.Fatal("unknown sha must fail the bridge")
	}
	// Garbage sha: fail fast without spawning.
	if err := g.FetchInto(ctx, baseDir, forkDir, "nope"); !isInvalid(err) {
		t.Fatalf("garbage sha err = %v", err)
	}
	// Missing dirs: fail fast without spawning.
	if err := g.FetchInto(ctx, "", forkDir, c); !isInvalidDir(err) {
		t.Fatalf("empty dir err = %v", err)
	}
	// Cancelled context: the backend outage passes through (503-class),
	// never masked as a bridge verdict.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.FetchInto(cctx, baseDir, forkDir, c); !isUnavailable(err) {
		t.Fatalf("cancelled ctx err = %v", err)
	}
	// The fork-unique commit bridges; afterwards it resolves in the base.
	if err := g.FetchInto(ctx, baseDir, forkDir, c); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if sha, err := g.ResolveRef(ctx, baseDir, c); err != nil || sha != c {
		t.Fatalf("resolve after bridge = %q %v", sha, err)
	}
	// Idempotent: a second fetch is a no-op success.
	if err := g.FetchInto(ctx, baseDir, forkDir, c); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	// No ref was created or moved by the bridge.
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname)")
	cmd.Dir = baseDir
	cmd.Env = testGitEnv(baseDir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "refs/heads/main" {
		t.Fatalf("bridge must not touch refs: %q", got)
	}
}

func isInvalid(err error) bool {
	return err != nil && strings.Contains(err.Error(), "40/64-hex")
}

func isInvalidDir(err error) bool {
	return err != nil && strings.Contains(err.Error(), "needs both dirs")
}

func isUnavailable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "temporarily unavailable")
}

// TestBridgeForkUniqueEndToEnd opens, diffs, and merges a cross-fork PR
// whose head holds a fork-unique commit — every step on the real git
// binary, git work running in the base serving copy.
func TestBridgeForkUniqueEndToEnd(t *testing.T) {
	baseDir, forkDir, _, b, c := bridgeForge(t)
	g := NewSubprocessGit("")
	g.Timeout = 60 * time.Second
	ctx := context.Background()

	// The pre-fix shape (issue #456): the fork-unique tip is a rev-list
	// hard error in the base copy — the exact error OpenPR mapped to 503.
	if _, err := g.Reachable(ctx, baseDir, c); err == nil {
		t.Fatal("unbridged fork tip must error the reachability probe (the 503 shape)")
	}

	e := bridgeEnv(baseDir, forkDir)
	_, pr, err := e.svc.OpenPR(ctx, "o", "r", writer(), OpenInput{Title: "fork work", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic", Fork: &ForkInfo{Repo: "f/r"}}, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pr.Head.SHA != c {
		t.Fatalf("head = %q (want %q)", pr.Head.SHA, c)
	}
	if pr.HeadPublished {
		t.Fatal("bridged-only head must stay fork-local")
	}

	patch, err := e.svc.Diff(ctx, "o", "r", pr.Num, writer())
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(patch, "g.txt") || !strings.Contains(patch, "new") {
		t.Fatalf("patch misses fork content:\n%s", patch)
	}

	rows, _, err := e.svc.Commits(ctx, "o", "r", pr.Num, writer(), 0, 10)
	if err != nil {
		t.Fatalf("commits: %v", err)
	}
	if len(rows) != 1 || rows[0].SHA != c {
		t.Fatalf("commits = %+v", rows)
	}

	m, err := e.svc.ComputeMergeable(ctx, "o", "r", pr.Num)
	if err != nil {
		t.Fatalf("mergeable: %v", err)
	}
	// Base advanced past the fork point (B child of A, head child of A)
	// ⇒ behind, not unknown and not an error.
	if m.State != MergeableBehind {
		t.Fatalf("state = %q (want behind)", m.State)
	}

	// Merge: seed the base main ref, run the task, verify the merge commit
	// in the BASE copy with real git.
	if e.refs.Refs["o/r"] == nil {
		e.refs.Refs["o/r"] = map[string]string{}
	}
	e.refs.Refs["o/r"]["refs/heads/main"] = b
	if _, err := e.svc.StartMerge(ctx, "o", "r", pr.Num, maintainer(), MergeInput{Strategy: StrategyMerge}, ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := waitTask(60*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskOK {
		t.Fatalf("merge = %+v", done)
	}
	sha, _ := done.Result["sha"].(string)
	if len(sha) != 40 {
		t.Fatalf("sha = %q", sha)
	}
	// The merge commit and both parents resolve in the base copy; the
	// fork tip is an ancestor of the merge (a true merge, not a stub).
	if _, err := g.ResolveRef(ctx, baseDir, sha); err != nil {
		t.Fatalf("merge commit missing in base: %v", err)
	}
	for _, pair := range [][2]string{{c, sha}, {b, sha}} {
		ok, err := g.IsAncestor(ctx, baseDir, pair[0], pair[1])
		if err != nil || !ok {
			t.Fatalf("ancestor %s of %s: %v %v", pair[0][:7], pair[1][:7], ok, err)
		}
	}
	// Durability: the base move published WITH the server-made pack (the
	// bridged fork objects rode the pack to the bucket atomically —
	// UpdateRefWithPack, never ref-only).
	foundPack := false
	for _, rc := range e.refs.Calls {
		if (rc.Op == "update-pack") && rc.Ref == "refs/heads/main" && rc.Pack != "" {
			foundPack = true
		}
	}
	if !foundPack {
		t.Fatalf("merge must publish WITH pack: %+v", e.refs.Calls)
	}
	stored, _, _ := e.svc.loadPR(ctx, "o", "r", pr.Num)
	if !stored.Merged || stored.MergeCommitSHA == nil || *stored.MergeCommitSHA != sha {
		t.Fatalf("pr = %+v", stored)
	}
}
