// git.go — the push-mirror git subprocess runner (docs/go/04_git.md §2
// discipline): stock git only, pinned argv, GIT_TERMINAL_PROMPT=0 on
// every spawn, a bounded pool (never bare on request goroutines), ctx
// timeouts, and credentials via child env / materialized key files only
// (never argv, never the bucket, never logs).
//
// Transfer shape: the serving copy (manifest refs already applied by
// Sync) pushes via `git push --mirror` — refs+objects to the upstream
// in one transfer, mirroring the pull direction's ref reconstruction
// (refs live in the manifest store, not forge git refs — the Sync
// materializes what the store publishes, and the push ships it).
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

// Push transfers the serving copy's refs+objects to the upstream:
//
//	git [-c credential.<scheme>://<host>.helper= -c credential.<scheme>://<host>.helper=!<helper>] push --mirror -- <url>
//
// (credential pairs present only for password/token pushes, host-pinned;
// SSH pushes ride GIT_SSH_COMMAND with a materialized key file; file://
// and anonymous https push bare.) The ctx carries PushTimeout; cancel
// SIGKILLs git. Stderr is bounded (8 KiB) and scrubbed — secrets never
// land in task logs.
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
		envName := "WALHUB_PUSHMIRROR_TOKEN"
		user := auth.Username
		secret := auth.Password
		if auth.Kind == AuthToken {
			if user == "" {
				user = "x-access-token"
			}
			secret = auth.Token
		}
		argv = append(argv, credentialArgv(auth.Scheme, auth.Host, envName, user)...)
		extraEnv = append(extraEnv, envName+"="+secret)
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

	argv = append(argv, "push", "--mirror", "--", url)
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

// credentialArgv builds the inline config-pair helper, host-pinned to
// scheme://host (the pull-mirror credentialArgv shape: a redirect to
// another host never harvests the secret — git matches
// credential.<url>.helper by prefix). Empty helper first clears
// inherited helpers (argv order significant, 04 §11).
func credentialArgv(scheme, host, envName, username string) []string {
	pin := "credential." + scheme + "://" + host + ".helper"
	helper := "!f(){ echo username=" + username + "; echo password=$" + envName + "; };f"
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
