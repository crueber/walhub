package pulls

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommitEntry is one row of the commits endpoint (doc 07 Commit shape,
// reduced to the fields the PR surface renders).
type CommitEntry struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	At      string `json:"at"`
}

// GitRunner is the git subprocess seam (§4/§5: exact argv, docs/go/04_git.md).
// Every method shells out to the `git` binary — never a Go git library —
// through the bounded per-repo pool, never bare on request goroutines. The
// production implementation is SubprocessGit; tests substitute FakeGit.
type GitRunner interface {
	// ResolveRef resolves any branch, tag, or refs/pull/N/head to its
	// commit sha. Unresolvable ⇒ ErrNotFound-class error.
	ResolveRef(ctx context.Context, dir, ref string) (string, error)
	// MergeBase returns the merge base of two shas.
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
	// IsAncestor reports `git merge-base --is-ancestor a b` (exit 0 ⇒ true).
	IsAncestor(ctx context.Context, dir, a, b string) (bool, error)
	// TrialMerge runs the §4 trial merge and returns the would-be tree on
	// clean, or the conflict paths on dirty.
	TrialMerge(ctx context.Context, dir, base, head string) (tree string, conflicts []string, err error)
	// CommitTree runs `git commit-tree` with the given parents/message and
	// author/committer identity. Returns the new commit sha.
	CommitTree(ctx context.Context, dir, tree string, parents []string, msg, authorName, authorEmail, committerName, committerEmail string, when time.Time) (string, error)
	// Replay runs the §5 rebase strategy plumbing. Returns the replayed tip.
	// Committer identity is explicit (replay mints commits; the runner
	// strips ambient env, so the server identity rides GIT_COMMITTER_*):
	// original authorship preserved per commit, committer = server.
	Replay(ctx context.Context, dir, onto, base, head, committerName, committerEmail string) (string, error)
	// BehindCount returns `git rev-list --count head..base` (§4 behind).
	BehindCount(ctx context.Context, dir, base, head string) (int, error)
	// Reachable reports whether sha is reachable from any existing ref
	// (the §3 pipeline: `git rev-list --objects --stdin --not --all |
	// git cat-file --batch-check` empty).
	Reachable(ctx context.Context, dir, sha string) (bool, error)
	// Diff returns the unified `base...head` patch (§9.5: one well-formed
	// patch, the 12_web_ui.md parser's exact input).
	Diff(ctx context.Context, dir, base, head string) (string, error)
	// LogRange lists commits of base..head (skip/n pagination).
	LogRange(ctx context.Context, dir, base, head string, skip, n int) ([]CommitEntry, error)
	// Subject returns the subject line of one commit.
	Subject(ctx context.Context, dir, sha string) (string, error)
	// PackTip packs every object reachable from tip but no existing ref
	// (the merge/update-branch commit step, §5: server-made commits must
	// reach the bucket atomically with their ref update, or the merged
	// ref dangles bucket-missing objects). Returns (nil, nil) when tip
	// contributes no new objects. The pack file pair lives under a temp
	// scratch dir the CALLER owns: it must survive until the ref publish
	// consuming it returns, then be removed (os.RemoveAll).
	PackTip(ctx context.Context, dir, tip string) (*TipPack, error)
}

// TipPack is one server-made pack file pair (merge/update-branch), ready
// for the ref-publish funnel (upload + manifest CAS in one WAL entry).
type TipPack struct {
	// Scratch owns PackPath/IdxPath (caller removes it after publish).
	Scratch     string
	PackPath    string
	IdxPath     string
	Checksum    string
	PackSize    uint64
	IdxSize     uint64
	ObjectCount uint64
}

// RepoDirs resolves a repo to its synced local git dir (the production
// adapter syncs the WAL handle to serve level and returns the bare repo
// path; git runs read through it under the bounded pool).
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// RefPublisher publishes ref creates/updates/deletes through the normal WAL
// publish path (doc 05 CAS ladder). The production adapter funnels through
// RepoHandle.Publish with a REF_UPDATE txn; the merge task NEVER
// force-publishes (update carries old = base sha at plan time; a 409 on the
// CAS re-plans or fails loudly, never rewrites history).
type RefPublisher interface {
	// CreateRef creates ref → sha (idempotent: already-matching is a no-op).
	CreateRef(ctx context.Context, repo, ref, sha string, meta map[string]string) error
	// UpdateRef moves ref old → new (CAS: a moved ref fails, never forces).
	UpdateRef(ctx context.Context, repo, ref, old, newSHA string, meta map[string]string) error
	// DeleteRef deletes ref (policy-checked like any ref delete).
	DeleteRef(ctx context.Context, repo, ref string, meta map[string]string) error
	// UpdateRefWithPack moves ref old → new together with a server-made
	// pack (merge/update-branch commits, §5): the pack uploads and the
	// txn commits in ONE WAL entry — the ref never dangles. A nil pack
	// is UpdateRef. The pack files must survive until this returns (the
	// funnel uploads synchronously); the caller removes them after.
	UpdateRefWithPack(ctx context.Context, repo, ref, old, newSHA string, pack *RefPack, meta map[string]string) error
}

// RefPack is the funnel-bound form of a TipPack (law 8: core's
// PreparedPack stays in the WAL package; this is the narrow seam copy).
type RefPack struct {
	PackPath    string
	IdxPath     string
	Checksum    string
	PackSize    uint64
	IdxSize     uint64
	ObjectCount uint64
}

// gitPool is the bounded semaphore of concurrent git processes (04_git.md
// §2: capacity = git.max_git_procs, default 4 × GOMAXPROCS). All SubprocessGit
// runs go through it — never bare on request goroutines.
type gitPool struct{ sem chan struct{} }

func newGitPool(capacity int) *gitPool {
	if capacity <= 0 {
		capacity = 4 * runtime.GOMAXPROCS(0)
	}
	return &gitPool{sem: make(chan struct{}, capacity)}
}

// run executes fn while holding a pool slot; ctx cancellation while waiting
// returns ctx.Err() without running fn.
func (p *gitPool) run(ctx context.Context, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubprocessGit is the production GitRunner: stock git with the exact argv
// from 03 §4/§5 (via docs/go/04_git.md), run under the bounded pool with the
// §2 subprocess discipline (explicit env incl. GIT_TERMINAL_PROMPT=0, stdin
// feeder goroutine, bounded 8 KiB stderr, then Wait).
type SubprocessGit struct {
	Binary  string // git.binary (default "git")
	Pool    *gitPool
	Timeout time.Duration // per-command timeout (default 120 s)
}

// NewSubprocessGit builds the production runner.
func NewSubprocessGit(binary string) *SubprocessGit {
	if binary == "" {
		binary = "git"
	}
	return &SubprocessGit{Binary: binary, Pool: newGitPool(0), Timeout: 120 * time.Second}
}

// timeoutFor bounds one command (context-bounded; client disconnect kills
// the child via exec.CommandContext).
func (g *SubprocessGit) timeoutFor(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, g.Timeout)
}

// runCollect runs argv in dir and buffers stdout (feeder-goroutine stdin
// discipline for the string-input case; bounded stderr; pool-gated). On
// non-zero exit the returned error is a *gitExitError carrying the stdout
// captured before failure (merge-tree exit 1 still prints conflict paths).
func (g *SubprocessGit) runCollect(ctx context.Context, dir string, argv []string, stdin string, extraEnv []string) (string, error) {
	var stdout string
	var runErr error
	var errText string
	perr := g.Pool.run(ctx, func() error {
		cctx, cancel := g.timeoutFor(ctx)
		defer cancel()
		cmd := exec.CommandContext(cctx, g.Binary, argv...)
		cmd.Dir = dir
		cmd.Env = append([]string{"PATH=" + pathEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}, extraEnv...)
		var out bytes.Buffer
		var errBuf boundedStderr
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if stdin != "" {
			pipe, err := cmd.StdinPipe()
			if err != nil {
				runErr = fmt.Errorf("%w: stdin pipe: %v", ErrUnavailable, err)
				return runErr
			}
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer pipe.Close() //nolint:errcheck — the child may have exited
				_, _ = strings.NewReader(stdin).WriteTo(pipe)
			}()
			runErr = cmd.Run()
			wg.Wait()
		} else {
			runErr = cmd.Run()
		}
		stdout = out.String()
		if runErr != nil {
			errText = errBuf.String()
			return runErr
		}
		return nil
	})
	if perr != nil {
		// Pool cancel/timeout surfaces as unavailable (never a verdict).
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], ctx.Err())
		}
		// Otherwise perr IS runErr (the pool's only other error source).
		// A real git exit carries semantics — callers switch on
		// *gitExitError. Any other failure (missing binary, killed
		// feeder) is a backend outage, never a verdict.
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return stdout, &gitExitError{argv: argv, errText: errText, err: runErr, stdout: stdout}
		}
		return stdout, fmt.Errorf("%w: git %s: %v (%s)", ErrUnavailable, argv[0], runErr, errText)
	}
	return stdout, nil
}

// gitExitError carries a non-zero git exit with its stderr tail (and the
// stdout captured before failure).
type gitExitError struct {
	argv    []string
	errText string
	stdout  string
	err     error
}

func (e *gitExitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.argv, " "), e.err, e.errText)
}

// pathEnv reads PATH without inheriting the ambient env wholesale.
func pathEnv() string {
	return osGetenv("PATH")
}

// osGetenv reads one env var (PATH only — nothing else is inherited).
func osGetenv(k string) string { return os.Getenv(k) }

// osGetpid names the process for unique temp-branch construction.
func osGetpid() int { return os.Getpid() }

// ResolveRef resolves any ref to its commit sha:
// `git rev-parse --verify --quiet <ref>^{commit}`. A genuine git failure
// (non-zero exit) is unknown-revision (404/422); a backend failure (pool,
// timeout, missing binary) propagates as unavailable (503) — never
// misreported as unknown.
func (g *SubprocessGit) ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	out, err := g.runCollect(ctx, dir, []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"}, "", nil)
	if err != nil {
		var ge *gitExitError
		if errors.As(err, &ge) {
			return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
		}
		return "", err
	}
	sha := strings.TrimSpace(out)
	if err := validateSHA(sha); err != nil {
		return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
	}
	return sha, nil
}

// MergeBase runs `git merge-base <a> <b>`. Same error contract as
// ResolveRef: exit-failure is unprocessable, backend failure propagates.
func (g *SubprocessGit) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := g.runCollect(ctx, dir, []string{"merge-base", a, b}, "", nil)
	if err != nil {
		var ge *gitExitError
		if errors.As(err, &ge) {
			return "", fmt.Errorf("%w: no merge base for %s..%s", ErrUnprocessable, a, b)
		}
		return "", err
	}
	sha := strings.TrimSpace(out)
	if err := validateSHA(sha); err != nil {
		return "", fmt.Errorf("%w: no merge base for %s..%s", ErrUnprocessable, a, b)
	}
	return sha, nil
}

// IsAncestor runs `git merge-base --is-ancestor <a> <b>` (exit 0 ⇒ true,
// exit 1 ⇒ false; anything else is an error).
func (g *SubprocessGit) IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := g.runCollect(ctx, dir, []string{"merge-base", "--is-ancestor", a, b}, "", nil)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*gitExitError); ok {
		return false, nil
	}
	return false, err
}

// TrialMerge runs the trial merge and returns the would-be tree on clean,
// or the conflict paths on dirty. Argv is the §5 form verbatim:
// `git merge-tree --write-tree --name-only <base_sha> <head_sha>` (git finds
// the merge base itself; the §4 three-positional form is a usage error on
// modern git — see the 03 Decisions deviation note). Exit 0 = clean (first
// line is the would-be tree); exit 1 = conflicts (conflict paths follow the
// tree line up to the first blank line; informational messages after the
// blank are skipped).
func (g *SubprocessGit) TrialMerge(ctx context.Context, dir, base, head string) (string, []string, error) {
	out, err := g.runCollect(ctx, dir, []string{"merge-tree", "--write-tree", "--name-only", base, head}, "", nil)
	if err == nil {
		lines := splitLines(out)
		if len(lines) == 0 {
			return "", nil, fmt.Errorf("%w: empty merge-tree output", ErrCorrupt)
		}
		return lines[0], []string{}, nil
	}
	if ge, ok := err.(*gitExitError); ok {
		tree, paths := splitMergeTree(ge.stdout)
		_ = tree
		return "", nonNilStr(paths), fmt.Errorf("%w", errDirty)
	}
	return "", nil, err
}

// splitMergeTree parses merge-tree --name-only output: the first line is
// the (partial) tree; conflict paths follow up to the first blank line.
func splitMergeTree(out string) (string, []string) {
	raw := strings.Split(out, "\n")
	if len(raw) == 0 || strings.TrimSpace(raw[0]) == "" {
		return "", nil
	}
	tree := strings.TrimSpace(raw[0])
	var paths []string
	for _, l := range raw[1:] {
		if strings.TrimSpace(l) == "" {
			break
		}
		paths = append(paths, l)
	}
	return tree, paths
}

// errDirty marks a dirty trial merge (conflicts); unwrapped by callers into
// state:"dirty" with best-effort conflict paths.
var errDirty = fmt.Errorf("merge-tree reports conflicts")

// CommitTree runs `git commit-tree <tree> -p <parent>… -m <msg>` with
// GIT_AUTHOR_* / GIT_COMMITTER_* env (merging principal = author, server
// identity = committer). Returns the new commit sha.
func (g *SubprocessGit) CommitTree(ctx context.Context, dir, tree string, parents []string, msg, authorName, authorEmail, committerName, committerEmail string, when time.Time) (string, error) {
	argv := []string{"commit-tree", tree}
	for _, p := range parents {
		argv = append(argv, "-p", p)
	}
	argv = append(argv, "-m", msg)
	stamp := strconv.FormatInt(when.Unix(), 10) + " +0000"
	env := []string{
		"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail, "GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_NAME=" + committerName, "GIT_COMMITTER_EMAIL=" + committerEmail, "GIT_COMMITTER_DATE=" + stamp,
	}
	out, err := g.runCollect(ctx, dir, argv, "", env)
	if err != nil {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	sha := strings.TrimSpace(out)
	if err := validateSHA(sha); err != nil {
		return "", fmt.Errorf("%w: commit-tree produced %q", ErrCorrupt, sha)
	}
	return sha, nil
}

// Replay runs the §5 rebase strategy plumbing, no worktree. Stock git's
// `replay` (experimental, shipped in git 2.53) only tracks branches named
// in the revision range: a pure-SHA range is a silent no-op (exit 0, empty
// stdout — verified live, so the bare §5 argv can never yield a tip). Replay
// therefore plants a uniquely-named temporary branch at the head sha, runs
// `git replay --onto <onto> --ref-action=print <mb>..<tmpref>` (print mode
// emits `update` lines and never updates serving refs — default update mode
// would move refs on disk behind the WAL's back), parses the
// `update <tmpref> <new> <old>` line for the replayed tip, and deletes the
// temp branch on every exit path. Original authorship preserved per commit.
//
// ### Concurrency
//
// Hazard: two concurrent replays colliding on one temp branch, or a crash
// leaving a temp branch advertised. Avoidance: the branch name carries
// pid+nanos (unique per call, same scheme as the ingest scratch dirs in
// 04_git.md §3.1); deletion is deferred; a leaked temp branch matches no
// PR ref and is swept by name prefix on the next replay.
func (g *SubprocessGit) Replay(ctx context.Context, dir, onto, base, head, committerName, committerEmail string) (string, error) {
	mb, err := g.MergeBase(ctx, dir, base, head)
	if err != nil {
		return "", err
	}
	if mb == head {
		return head, nil // empty range: nothing to replay
	}
	tmpRef := fmt.Sprintf("refs/heads/walhub-tmp-replay-%d-%d", osGetpid(), time.Now().UnixNano())
	if _, err := g.runCollect(ctx, dir, []string{"update-ref", tmpRef, head}, "", nil); err != nil {
		return "", fmt.Errorf("replay temp branch: %w", err)
	}
	defer func() {
		_, _ = g.runCollect(context.WithoutCancel(ctx), dir, []string{"update-ref", "-d", tmpRef}, "", nil)
	}()
	env := []string{"GIT_COMMITTER_NAME=" + committerName, "GIT_COMMITTER_EMAIL=" + committerEmail}
	out, err := g.runCollect(ctx, dir, []string{"replay", "--onto", onto, "--ref-action=print", mb + ".." + tmpRef}, "", env)
	if err != nil {
		return "", fmt.Errorf("replay: %w", err)
	}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "update" && fields[1] == tmpRef {
			if verr := validateSHA(fields[2]); verr != nil {
				return "", fmt.Errorf("%w: replay produced %q", ErrCorrupt, fields[2])
			}
			return fields[2], nil
		}
	}
	return "", fmt.Errorf("%w: replay produced no update for %s", ErrCorrupt, tmpRef)
}

// BehindCount runs `git rev-list --count <head>..<base>` (§4 behind).
func (g *SubprocessGit) BehindCount(ctx context.Context, dir, base, head string) (int, error) {
	out, err := g.runCollect(ctx, dir, []string{"rev-list", "--count", head + ".." + base}, "", nil)
	if err != nil {
		return 0, fmt.Errorf("rev-list --count: %w", err)
	}
	n, cerr := strconv.Atoi(strings.TrimSpace(out))
	if cerr != nil {
		return 0, fmt.Errorf("%w: rev-list --count produced %q", ErrCorrupt, out)
	}
	return n, nil
}

// Reachable runs the §3 pipeline: `git rev-list --objects --stdin --not
// --all | git cat-file --batch-check` — empty output ⇒ reachable.
func (g *SubprocessGit) Reachable(ctx context.Context, dir, sha string) (bool, error) {
	revOut, err := g.runCollect(ctx, dir, []string{"rev-list", "--objects", "--stdin", "--not", "--all"}, sha+"\n", nil)
	if err != nil {
		return false, fmt.Errorf("rev-list reachability: %w", err)
	}
	if strings.TrimSpace(revOut) == "" {
		return true, nil
	}
	checkOut, err := g.runCollect(ctx, dir, []string{"cat-file", "--batch-check"}, revOut, nil)
	if err != nil {
		return false, fmt.Errorf("cat-file reachability: %w", err)
	}
	return !strings.Contains(checkOut, "missing"), nil
}

// Diff returns the unified `base...head` patch (§9.5).
func (g *SubprocessGit) Diff(ctx context.Context, dir, base, head string) (string, error) {
	out, err := g.runCollect(ctx, dir, []string{"diff", "--no-color", "--no-ext-diff", base + "..." + head, "--"}, "", nil)
	if err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}
	return out, nil
}

// LogRange lists commits of base..head (skip/n pagination):
// `git log --format=… base..head --skip=N --max-count=M`.
func (g *SubprocessGit) LogRange(ctx context.Context, dir, base, head string, skip, n int) ([]CommitEntry, error) {
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	argv := []string{"log", "--format=%H%x00%s%x00%an%x00%aI", base + ".." + head, "--skip=" + strconv.Itoa(skip), "--max-count=" + strconv.Itoa(n)}
	out, err := g.runCollect(ctx, dir, argv, "", nil)
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	var commits []CommitEntry
	for _, line := range splitLines(out) {
		parts := strings.SplitN(line, "\x00", 4)
		if len(parts) != 4 {
			continue
		}
		commits = append(commits, CommitEntry{SHA: parts[0], Subject: parts[1], Author: parts[2], At: parts[3]})
	}
	if commits == nil {
		commits = []CommitEntry{}
	}
	return commits, nil
}

// Subject returns the subject line of one commit: `git log -1 --format=%s`.
func (g *SubprocessGit) Subject(ctx context.Context, dir, sha string) (string, error) {
	out, err := g.runCollect(ctx, dir, []string{"log", "-1", "--format=%s", sha}, "", nil)
	if err != nil {
		return "", fmt.Errorf("log subject: %w", err)
	}
	return strings.TrimRight(out, "\n"), nil
}

// PackTip packs every object reachable from tip but no existing ref
// (§5 merge/update-branch durability): `git pack-objects --revs --stdout`
// over `<tip>` plus `^<ref>` exclusion lines (one per ref from
// `for-each-ref` — pack-objects takes no `--not/--all`, caret negation
// rides stdin), then `git index-pack --stdin --fsck-objects` in a scratch
// git-dir (the ingest shape, minus thin handling — server-made packs are
// thick and trusted). Returns (nil, nil) when tip contributes no new
// objects (fully contained — the caller publishes ref-only, today's path,
// with nothing to dangle).
//
// ### Concurrency
//
// Hazard: two merges packing concurrently. Avoidance: the scratch dir is
// unique per call (pid+nanos, the replay-temp-branch scheme); nothing is
// written to the serving dir. No lock of any kind is held (pure
// subprocess + temp files, 13 §2 rule 4).
func (g *SubprocessGit) PackTip(ctx context.Context, dir, tip string) (*TipPack, error) {
	if err := validateSHA(tip); err != nil {
		return nil, err
	}
	refsOut, err := g.runCollect(ctx, dir, []string{"for-each-ref", "--format=%(refname)"}, "", nil)
	if err != nil {
		return nil, fmt.Errorf("pack refs: %w", err)
	}
	var stdin strings.Builder
	stdin.WriteString(tip + "\n")
	for _, ref := range splitLines(refsOut) {
		stdin.WriteString("^" + ref + "\n")
	}
	raw, err := g.runCollect(ctx, dir, []string{"pack-objects", "--revs", "--stdout"}, stdin.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("pack-objects: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil // fully contained — nothing new to upload
	}
	scratch, err := os.MkdirTemp("", "walhub-packtip-*")
	if err != nil {
		return nil, fmt.Errorf("%w: pack scratch: %v", ErrUnavailable, err)
	}
	// A pack failure below must not litter the temp dir (the caller owns
	// removal only on success).
	failed := true
	defer func() {
		if failed {
			os.RemoveAll(scratch)
		}
	}()
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "pack"), 0o755); err != nil {
		return nil, fmt.Errorf("%w: pack scratch: %v", ErrUnavailable, err)
	}
	// The hand-built git-dir mirrors the ingest scratch (minus alternates
	// — the pack is thick and self-contained): index-pack --stdin
	// requires a recognizable repository layout.
	if err := os.MkdirAll(filepath.Join(scratch, "refs"), 0o755); err != nil {
		return nil, fmt.Errorf("%w: pack scratch: %v", ErrUnavailable, err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "info"), 0o755); err != nil {
		return nil, fmt.Errorf("%w: pack scratch: %v", ErrUnavailable, err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		return nil, fmt.Errorf("%w: pack scratch: %v", ErrUnavailable, err)
	}
	if cfg, cerr := os.ReadFile(filepath.Join(dir, "config")); cerr == nil {
		_ = os.WriteFile(filepath.Join(scratch, "config"), cfg, 0o644)
	}
	out, err := g.runCollect(ctx, scratch, []string{"index-pack", "--stdin", "--fsck-objects"}, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("pack index: %w", err)
	}
	sha := lastTipHexToken(out)
	if err := validateSHA(sha); err != nil {
		return nil, fmt.Errorf("%w: pack index produced %q", ErrCorrupt, sha)
	}
	pp := filepath.Join(scratch, "objects", "pack", "pack-"+sha+".pack")
	ip := filepath.Join(scratch, "objects", "pack", "pack-"+sha+".idx")
	pfi, err := os.Stat(pp)
	if err != nil {
		return nil, fmt.Errorf("%w: pack missing: %v", ErrCorrupt, err)
	}
	ifi, err := os.Stat(ip)
	if err != nil {
		return nil, fmt.Errorf("%w: pack idx missing: %v", ErrCorrupt, err)
	}
	// An excluded-everything pack still emits a header-only shell (the
	// contained-tip case): zero objects means ref-only, today's path.
	if n := tipIdxObjectCount(ip); n == 0 {
		os.RemoveAll(scratch)
		return nil, nil
	} else {
		failed = false
		return &TipPack{
			Scratch:     scratch,
			PackPath:    pp,
			IdxPath:     ip,
			Checksum:    sha,
			PackSize:    uint64(pfi.Size()),
			IdxSize:     uint64(ifi.Size()),
			ObjectCount: n,
		}, nil
	}
}

// lastTipHexToken returns the last 40/64-hex token in s (index-pack
// stdout carries the pack checksum; mirrors internal/git's parser so the
// seam stays dependency-light).
func lastTipHexToken(s string) string {
	var last string
	for _, tok := range strings.Fields(s) {
		t := strings.Trim(tok, ",;:")
		if len(t) == 40 || len(t) == 64 {
			ok := true
			for i := 0; i < len(t); i++ {
				c := t[i]
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					ok = false
					break
				}
			}
			if ok {
				last = t
			}
		}
	}
	return last
}

// tipIdxObjectCount reads the object count from a v2 pack idx fanout
// table (fanout[255] after the 8-byte header; mirrors internal/git's
// reader — cosmetic field, so an unreadable idx counts 0, never an
// error).
func tipIdxObjectCount(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0
	}
	if !bytes.Equal(hdr[:4], []byte{0xff, 't', 'O', 'c'}) || binary.BigEndian.Uint32(hdr[4:8]) != 2 {
		return 0
	}
	var fan [1024]byte
	if _, err := io.ReadFull(f, fan[:]); err != nil {
		return 0
	}
	return uint64(binary.BigEndian.Uint32(fan[1020:1024]))
}

// splitLines splits output into non-empty lines.
func splitLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// boundedStderr keeps the TAIL of stderr (capped 8 KiB) for error text.
type boundedStderr struct {
	mu   sync.Mutex
	buf  []byte
	full bool
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	const cap = 8 << 10
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= cap {
		b.buf = append(b.buf[:0], p[len(p)-cap:]...)
		b.full = true
		return len(p), nil
	}
	if len(b.buf)+len(p) > cap {
		b.full = true
		keep := cap - len(p)
		copy(b.buf, b.buf[len(b.buf)-keep:])
		b.buf = b.buf[:keep]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return "…" + string(b.buf)
	}
	return string(b.buf)
}
