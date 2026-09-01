package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Pool is the bounded semaphore of concurrent git processes (04_git.md §2).
// Rust's spawn_blocking equivalent: capacity = git.max_git_procs (default 4 × GOMAXPROCS).
// Never blocks callers indefinitely — Run honors ctx cancellation while waiting.
type Pool struct{ sem chan struct{} }

func NewPool(capacity int) *Pool {
	if capacity <= 0 {
		capacity = 4 * runtime.GOMAXPROCS(0)
	}
	return &Pool{sem: make(chan struct{}, capacity)}
}

// Run executes fn while holding a pool slot. ctx cancellation while waiting for
// a slot returns ctx.Err() without running fn.
func (p *Pool) Run(ctx context.Context, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Layer owns every interaction with the git binary (04_git.md).
type Layer struct {
	Binary      string // git.binary (default "git")
	Scratch     string // cache.dir root — scratch/follow live under the caller's dirs
	Pool        *Pool
	Version     string // walgit agent version for capability lines
	MaxWants    int    // git.max_wants; 0 = unlimited
	Fsck        bool   // wal.fsck_objects
	Connect     bool   // wal.check_connectivity
	MaxPush     int64  // server.max_push_bytes (0 = unlimited)
	IngestTTL   time.Duration
	ConnectTTL  time.Duration
	MaintTTL    time.Duration
	UpstreamTTL time.Duration

	// D17 (04_git.md §8.2): repos that require bundle-uri clones, and the
	// ledger (owned by internal/bundle) consulted for the one-shot fallback.
	BundlesRequire []string
	BundleLedger   BundleLedger

	peelMu sync.Mutex
	peels  map[string]*peelClient // per repo path, lazily created
}

// BundleLedger is the bundle-uri fallback ledger (internal/bundle owns it; this
// package only queries it for D17).
type BundleLedger interface {
	// CanFallback reports whether (principal, repo) has a one-shot full-clone
	// grant available (fetched bundles/list within the last hour, no grant in 6h).
	CanFallback(principal, repo string) bool
	// RecordFallback consumes the one-shot grant.
	RecordFallback(principal, repo string)
}

func NewLayer() *Layer {
	return &Layer{
		Binary:      "git",
		Pool:        NewPool(0),
		Version:     "1.0.0",
		Fsck:        true,
		Connect:     true,
		IngestTTL:   600 * time.Second,
		ConnectTTL:  300 * time.Second,
		MaintTTL:    1800 * time.Second,
		UpstreamTTL: 900 * time.Second,
		peels:       map[string]*peelClient{},
	}
}

// --- subprocess discipline (04_git.md §2) -------------------------------------

// boundedBuffer keeps the TAIL of stderr (capped 8 KiB) for error text.
type boundedBuffer struct {
	mu   sync.Mutex
	buf  []byte
	cap  int
	full bool
}

func newBounded(n int) *boundedBuffer { return &boundedBuffer{cap: n} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.cap {
		b.buf = append(b.buf[:0], p[len(p)-b.cap:]...)
		b.full = true
		return len(p), nil
	}
	if len(b.buf)+len(p) > b.cap {
		b.full = true
		keep := b.cap - len(p)
		copy(b.buf, b.buf[len(b.buf)-keep:])
		b.buf = b.buf[:keep]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	if b.full {
		s = "…" + s
	}
	return s
}

// execSpec is one git subprocess. Env is EXPLICIT: nothing is inherited except
// PATH (04_git.md §2). stdin, when set, is fed (and closed) by a helper
// goroutine; stdout is drained by the caller's goroutine (or io.Discard);
// stderr drains into an 8 KiB bounded buffer; only then Wait.
type execSpec struct {
	argv    []string
	dir     string
	env     []string // extra vars; GIT_DIR/GIT_TERMINAL_PROMPT added when not present
	stdin   io.Reader
	stdout  io.Writer // nil → discard
	timeout time.Duration
	// extraEnvOnly: vars that must reach the child but were computed per-call
	onWait func() // optional cleanup hook run after Wait
}

func (l *Layer) baseEnv(gitDir string, extra ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0"}
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	env = append(env, extra...)
	return env
}

// run executes one git subprocess with the §2 discipline and returns the error
// text (stderr tail) for error wrapping. On non-zero exit the error is a
// *GitError{GitErrSubprocess}.
func (l *Layer) run(ctx context.Context, s execSpec) (string, error) {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, l.Binary, s.argv...)
	cmd.Env = l.baseEnv("", s.env...)
	if s.dir != "" {
		cmd.Dir = s.dir
	}
	cmd.Stdout = s.stdout
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	stderr := newBounded(8 << 10)
	cmd.Stderr = stderr

	if s.stdin != nil {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return "", &GitError{Kind: GitErrIo, Detail: "stdin pipe: " + err.Error()}
		}
		var feedErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer pipe.Close() //nolint:errcheck — the child may have exited
			_, feedErr = io.Copy(pipe, s.stdin)
		}()
		runErr := cmd.Run()
		wg.Wait()
		if runErr == nil && feedErr != nil && feedErr != io.EOF {
			// stdin copy failed but the child exited cleanly — surface it.
			return stderr.String(), &GitError{Kind: GitErrIo, Detail: "stdin feed: " + feedErr.Error()}
		}
		if runErr != nil {
			return stderr.String(), subErr(ctx, s.argv, runErr, stderr.String())
		}
		if s.onWait != nil {
			s.onWait()
		}
		return stderr.String(), nil
	}

	if err := cmd.Run(); err != nil {
		return stderr.String(), subErr(ctx, s.argv, err, stderr.String())
	}
	if s.onWait != nil {
		s.onWait()
	}
	return stderr.String(), nil
}

func subErr(ctx context.Context, argv []string, err error, stderr string) error {
	if ctx.Err() != nil {
		return &GitError{Kind: GitErrIo, Cmd: strings.Join(argv, " "),
			Detail: fmt.Sprintf("timed out or cancelled: %v", ctx.Err()), Stderr: stderr}
	}
	ge := errSubprocess(argv, stderr)
	ge.Detail = err.Error()
	return ge
}

// runPooled runs s inside the blocking pool. Every heavyweight exec
// (index-pack, upload-pack, repack, commit-graph, pack-objects, connectivity,
// bundle create) goes through here; light commands (init, update-ref,
// merge-base, config) count against the pool too for uniform accounting.
func (l *Layer) runPooled(ctx context.Context, s execSpec) (string, error) {
	var stderr string
	err := l.Pool.Run(ctx, func() error {
		var e error
		stderr, e = l.run(ctx, s)
		return e
	})
	return stderr, err
}

// feedAndCollect is the close-stdin-then-drain-stdout-then-Wait shape for
// callers that need the child's stdout fully buffered (update-ref, for-each-ref,
// merge-base). Large stdin still gets a feeder goroutine via run().
func (l *Layer) runCollect(ctx context.Context, s execSpec) (stdout []byte, stderr string, err error) {
	var out bytes.Buffer
	s.stdout = &out
	stderr, err = l.runPooled(ctx, s)
	return out.Bytes(), stderr, err
}
