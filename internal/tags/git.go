package tags

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// GitRunner is the git subprocess seam (docs/go/04_git.md: exact argv, never
// a Go git library). The production implementation is SubprocessGit; tests
// substitute a fake. Every method shells out to the `git` binary through
// the bounded pool, never bare on request goroutines.

// gitPool is the bounded semaphore of concurrent git processes (same shape
// as internal/pulls: capacity defaults to 4 × GOMAXPROCS; a dedicated pool
// so tag probes never borrow control-plane capacity).
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

// SubprocessGit is the production GitRunner: stock git with exact argv, run
// under the bounded pool with the subprocess discipline (explicit env incl.
// GIT_TERMINAL_PROMPT=0, bounded 8 KiB stderr).
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

// boundedStderr caps captured stderr at 8 KiB.
type boundedStderr struct {
	buf bytes.Buffer
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	room := 8192 - b.buf.Len()
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		p = p[:room]
	}
	return b.buf.Write(p)
}

func (b *boundedStderr) String() string { return b.buf.String() }

// runCollect runs argv in dir and buffers stdout. On non-zero exit the
// returned error is a *gitExitError; backend failures (missing binary,
// timeout) propagate as ErrUnavailable-class errors.
func (g *SubprocessGit) runCollect(ctx context.Context, dir string, argv []string) (string, error) {
	var stdout string
	var runErr error
	var errText string
	pool := g.Pool
	if pool == nil {
		pool = newGitPool(0)
	}
	perr := pool.run(ctx, func() error {
		cctx, cancel := g.timeoutFor(ctx)
		defer cancel()
		cmd := exec.CommandContext(cctx, g.Binary, argv...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
		var out bytes.Buffer
		var errBuf boundedStderr
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		runErr = cmd.Run()
		stdout = out.String()
		if runErr != nil {
			errText = errBuf.String()
			return runErr
		}
		return nil
	})
	if perr != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: git %s: %v", ErrUnavailable, argv[0], ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return stdout, &gitExitError{argv: argv, errText: errText, err: runErr, stdout: stdout}
		}
		return stdout, fmt.Errorf("%w: git %s: %v (%s)", ErrUnavailable, argv[0], runErr, errText)
	}
	return stdout, nil
}

// gitExitError carries a non-zero git exit with its stderr tail.
type gitExitError struct {
	argv    []string
	errText string
	err     error
	stdout  string
}

func (e *gitExitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.argv, " "), e.err, e.errText)
}

// validateSHA requires a full 40/64-hex object id.
func validateSHA(sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return fmt.Errorf("bad sha %q", sha)
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return fmt.Errorf("bad sha %q", sha)
		}
	}
	return nil
}

// CommitExists resolves sha to its commit id:
// `git rev-parse --verify --quiet <sha>^{commit}` (the `^{commit}` peel
// rejects trees/blobs/tags-to-non-commits at the source). A genuine git
// failure (non-zero exit) is unknown-revision (404); a backend failure
// propagates as unavailable (503) — never misreported as unknown.
func (g *SubprocessGit) CommitExists(ctx context.Context, dir, sha string) (string, error) {
	out, err := g.runCollect(ctx, dir, []string{"rev-parse", "--verify", "--quiet", strings.TrimSpace(sha) + "^{commit}"})
	if err != nil {
		var ge *gitExitError
		if errors.As(err, &ge) {
			return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, sha)
		}
		return "", err
	}
	resolved := strings.TrimSpace(out)
	if verr := validateSHA(resolved); verr != nil {
		return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, sha)
	}
	return resolved, nil
}
