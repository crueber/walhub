// git.go — the push-mirror git subprocess runner (docs/go/04_git.md §2
// discipline): stock git only, pinned argv, GIT_TERMINAL_PROMPT=0 on
// every spawn, a bounded pool (never bare on request goroutines), ctx
// timeouts, and credentials via child env / materialized key files only
// (never argv, never the bucket, never logs).
//
// Transfer shape: the serving copy (manifest refs already applied by
// Sync) pushes via forced namespace refspecs with --prune — the
// user-namespace refs the store publishes, never forge-internal ones
// (refs/pull/** stays out, the pull direction's FilterRefs discipline
// reversed).
package pushmirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// PushAuth carries the resolved credential material for one push fire
// (memory-only: loaded from the secret sidecar per fire, never stored
// elsewhere, never logged).
type PushAuth struct {
	Kind        string // none|password|token|ssh
	Username    string
	Password    string // password kind
	Token       string // token kind
	PrivateKey  string // ssh kind (OpenSSH PEM)
	KnownHosts  string // ssh kind ("" = accept-new)
	Scheme      string // lowercased URL scheme
	Host        string // URL host ("" for file)
	Fingerprint string // ssh public-key fingerprint (log narration only)
}

// Runner spawns stock git for push-mirror transfers. Binary defaults to
// "git"; key material lands under CacheDir (0600 files, swept per fire —
// no feature state on disk, law 1).
type Runner struct {
	Binary      string
	CacheDir    string
	PushTimeout time.Duration
	GitTimeout  time.Duration

	pool chan struct{} // bounded git-process semaphore (04 §2)
}

// NewRunner builds a Runner; non-positive timeouts fall back to
// push-safe defaults (pushes carry pack bytes — the clone-scale budget,
// not the 300 s ref-enumeration budget).
func NewRunner(binary, cacheDir string, pushTimeout, gitTimeout time.Duration) *Runner {
	if binary == "" {
		binary = "git"
	}
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	if pushTimeout <= 0 {
		pushTimeout = 1800 * time.Second
	}
	if gitTimeout <= 0 {
		gitTimeout = 300 * time.Second
	}
	return &Runner{
		Binary: binary, CacheDir: cacheDir,
		PushTimeout: pushTimeout, GitTimeout: gitTimeout,
		pool: make(chan struct{}, 4*runtime.GOMAXPROCS(0)),
	}
}

// run gates one spawn behind the pool (ctx cancel escapes the wait).
func (r *Runner) run(ctx context.Context, fn func() error) error {
	select {
	case r.pool <- struct{}{}:
		defer func() { <-r.pool }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Push transfers the serving copy's refs to the upstream, minus
// forge-internal namespaces (the pull-mirror FilterRefs discipline,
// reversed): refs/replace/*, refs/meta/*, refs/keep-around/* always;
// refs/pull/*, refs/changes/*, refs/review/* (walhub's own PR heads live
// here — shipping them would leak forge state, and hosts like GitHub
// refuse writes to refs/pull/*); refs/notes/* by default. Everything
// else ships verbatim ("drop nothing else").
//
// Transfer shape: the local refs are enumerated (ListRefs, the §12
// for-each-ref argv), filtered, grouped by top-two-segment namespace,
// and pushed as forced wildcard refspecs with --prune, so deletions
// propagate within live namespaces (walhub is the primary) without
// ever naming an internal ref:
//
//	git [-c credential.<scheme>://<host>.helper= -c credential.<scheme>://<host>.helper=!<helper>] push --prune -- <url> +refs/heads/*:refs/heads/* [...]
//
// (credential pairs present only for password/token pushes, host-pinned;
// SSH pushes ride GIT_SSH_COMMAND with a materialized key file; file://
// and anonymous https push bare.) refs/heads + refs/tags ride every
// fire (even when locally empty, so a fully-deleted namespace prunes
// upstream); other surviving namespaces ride only when populated. The
// ctx carries PushTimeout; cancel SIGKILLs git. Stderr is bounded
// (8 KiB) and scrubbed — secrets never land in task logs.
func (r *Runner) Push(ctx context.Context, dir, url string, auth PushAuth) error {
	cctx, cancel := context.WithTimeout(ctx, r.PushTimeout)
	defer cancel()

	var argv []string
	var extraEnv []string
	var cleanup []func()
	defer func() {
		for _, fn := range cleanup {
			fn()
		}
	}()

	switch auth.Kind {
	case AuthPassword, AuthToken:
		userEnv := "WALHUB_PUSHMIRROR_USER"
		secretEnv := "WALHUB_PUSHMIRROR_TOKEN"
		user := auth.Username
		secret := auth.Password
		if auth.Kind == AuthToken {
			if user == "" {
				user = "x-access-token"
			}
			secret = auth.Token
		}
		argv = append(argv, credentialArgv(auth.Scheme, auth.Host, secretEnv, userEnv)...)
		// Both halves ride child env, never argv: the username is
		// user-controlled text and the `!` helper runs through a
		// shell — interpolating it into the helper would be command
		// injection (metachar newlines, `;`, `$()` all execute).
		extraEnv = append(extraEnv, userEnv+"="+user, secretEnv+"="+secret)
	case AuthSSH:
		sshCmd, clean, err := r.sshCommand(auth)
		if err != nil {
			return err
		}
		cleanup = append(cleanup, clean)
		extraEnv = append(extraEnv, "GIT_SSH_COMMAND="+sshCmd)
	case AuthNone:
		// bare push — file:// e2e and public upstreams.
	default:
		return fmt.Errorf("pushmirror: unknown auth kind %q", scrubText(auth.Kind))
	}

	specs, err := r.pushRefspecs(cctx, dir)
	if err != nil {
		return err
	}
	argv = append(argv, "push", "--prune", "--", url)
	argv = append(argv, specs...)
	return r.run(cctx, func() error {
		cmd := exec.CommandContext(cctx, r.Binary, argv...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + pathEnv(), "GIT_TERMINAL_PROMPT=0"}
		cmd.Env = append(cmd.Env, extraEnv...)
		cmd.Stdout = io.Discard
		var errBuf boundedStderr
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("pushmirror push: %v: %s", err, scrubText(errBuf.String()))
		}
		return nil
	})
}

// pushRefspecs enumerates the serving copy's refs and renders the forced
// wildcard refspecs for the push: every surviving namespace (the
// FilterRefs mirror discipline — internal namespaces dropped) as
// `+<ns>/*:<ns>/*`, plus refs/heads + refs/tags unconditionally (an
// emptied namespace must prune upstream, not silently keep it). Bare
// two-segment names (`refs/<x>`, no slash below) ride as exact forced
// refspecs. Sorted for stable argv.
func (r *Runner) pushRefspecs(ctx context.Context, dir string) ([]string, error) {
	refs, err := r.ListRefs(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("pushmirror enumerate refs: %v", scrubText(err.Error()))
	}
	namespaces := map[string]bool{"refs/heads": true, "refs/tags": true}
	var exact []string
	for name := range refs {
		if !keepPushRef(name) {
			continue
		}
		rest, ok := strings.CutPrefix(name, "refs/")
		if !ok {
			exact = append(exact, name)
			continue
		}
		head, _, ok := strings.Cut(rest, "/")
		if !ok {
			exact = append(exact, name) // bare refs/<x>
			continue
		}
		namespaces["refs/"+head] = true
	}
	specs := make([]string, 0, len(namespaces)+len(exact))
	for ns := range namespaces {
		specs = append(specs, "+"+ns+"/*:"+ns+"/*")
	}
	for _, name := range exact {
		specs = append(specs, "+"+name+":"+name)
	}
	sort.Strings(specs)
	return specs, nil
}

// keepPushRef is the push-direction FilterRefs discipline (the S4 refmap,
// import branches + tags shape): drop rewrites/forge-internal namespaces
// always, pull/changes/review + notes by default; keep everything else
// verbatim. Shared shape with repoimport.FilterRefs (which the pull
// direction calls); duplicated as a predicate here because the push
// needs namespaces, not a ref list.
func keepPushRef(name string) bool {
	switch {
	case strings.HasPrefix(name, "refs/replace/"),
		strings.HasPrefix(name, "refs/meta/"),
		strings.HasPrefix(name, "refs/keep-around/"):
		return false
	case strings.HasPrefix(name, "refs/pull/"),
		strings.HasPrefix(name, "refs/changes/"),
		strings.HasPrefix(name, "refs/review/"),
		strings.HasPrefix(name, "refs/notes/"):
		return false
	}
	return true
}

// credentialArgv builds the inline config-pair helper, host-pinned to
// scheme://host (the pull-mirror credentialArgv shape: a redirect to
// another host never harvests the secret — git matches
// credential.<url>.helper by prefix). Empty helper first clears
// inherited helpers (argv order significant, 04 §11). Both the username
// and the secret ride child env (named by userEnv/secretEnv) — neither
// is interpolated into the helper text, because the `!` helper runs
// through a shell and the username is user-controlled.
func credentialArgv(scheme, host, secretEnv, userEnv string) []string {
	pin := "credential." + scheme + "://" + host + ".helper"
	helper := "!f(){ echo username=$" + userEnv + "; echo password=$" + secretEnv + "; };f"
	return []string{"-c", pin + "=", "-c", pin + "=" + helper}
}

// sshCommand materializes the private key (0600) and optional known_hosts
// into a per-fire scratch dir and returns the GIT_SSH_COMMAND value plus
// its cleanup. Without pinned known_hosts the command uses
// StrictHostKeyChecking=accept-new (first-use trust, recorded — never the
// silent-insecure `no`); BatchMode=yes never prompts. The key file is
// removed by the caller-deferred cleanup on every exit path.
func (r *Runner) sshCommand(auth PushAuth) (string, func(), error) {
	if strings.TrimSpace(auth.PrivateKey) == "" {
		return "", func() {}, fmt.Errorf("pushmirror: ssh auth needs a private key")
	}
	dir, err := os.MkdirTemp(filepath.Join(r.CacheDir, "pushmirror"), "ssh-*")
	if err != nil {
		// First fire on a fresh cache dir: the parent may not exist yet.
		if merr := os.MkdirAll(filepath.Join(r.CacheDir, "pushmirror"), 0o755); merr != nil {
			return "", func() {}, fmt.Errorf("pushmirror: ssh scratch: %v", scrubText(merr.Error()))
		}
		dir, err = os.MkdirTemp(filepath.Join(r.CacheDir, "pushmirror"), "ssh-*")
	}
	if err != nil {
		return "", func() {}, fmt.Errorf("pushmirror: ssh scratch: %v", scrubText(err.Error()))
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte(auth.PrivateKey), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("pushmirror: ssh key file: %v", scrubText(err.Error()))
	}
	parts := []string{"ssh", "-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}
	if strings.TrimSpace(auth.KnownHosts) != "" {
		khPath := filepath.Join(dir, "known_hosts")
		if err := os.WriteFile(khPath, []byte(auth.KnownHosts), 0o600); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("pushmirror: known_hosts file: %v", scrubText(err.Error()))
		}
		parts = append(parts, "-o", "UserKnownHostsFile="+khPath, "-o", "StrictHostKeyChecking=yes")
	} else {
		parts = append(parts, "-o", "StrictHostKeyChecking=accept-new")
	}
	return strings.Join(parts, " "), cleanup, nil
}

// ListRefs runs the pinned ref enumeration in dir (post-push narration
// and tests):
//
//	git for-each-ref --format=%(objectname) %(refname)
func (r *Runner) ListRefs(ctx context.Context, dir string) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	out, errText, err := r.collect(cctx, dir, []string{"for-each-ref", "--format=%(objectname) %(refname)"}, nil)
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: %v: %s", err, scrubText(errText))
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		refs[strings.TrimSpace(name)] = oid
	}
	return refs, nil
}

// collect runs argv in dir with GIT_DIR=dir and buffers stdout
// (bounded scrubbed stderr; pool-gated).
func (r *Runner) collect(ctx context.Context, dir string, argv []string, extraEnv []string) (string, string, error) {
	var stdout, errText string
	var runErr error
	runErr = r.run(ctx, func() error {
		cmd := exec.CommandContext(ctx, r.Binary, argv...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + pathEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
		cmd.Env = append(cmd.Env, extraEnv...)
		var out bytes.Buffer
		var errBuf boundedStderr
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		runErr = cmd.Run()
		stdout = out.String()
		errText = errBuf.String()
		return runErr
	})
	return stdout, errText, runErr
}

func pathEnv() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return "/usr/bin:/bin"
}

// boundedStderr is the 8 KiB scrubbed stderr ring (04 §2).
type boundedStderr struct {
	mu  sync.Mutex
	buf []byte
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 8192 {
		b.buf = b.buf[len(b.buf)-8192:]
	}
	return len(p), nil
}

func (b *boundedStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
