// git.go — the mirror git subprocess runner (docs/go/04_git.md §2
// discipline): stock git only, pinned argv, GIT_TERMINAL_PROMPT=0 on
// every spawn, a bounded pool (never bare on request goroutines), ctx
// timeouts, and tokens via child env only (import S3 discipline:
// per-task env names, host-pinned credential helper, never argv,
// never the bucket).
//
// Scheduled fires ALWAYS run anonymously (public-upstreams-only v1 —
// no stored secrets exist to pass). The token path exists only for the
// creation-time first sync and the manual "Sync now" POST body, both
// memory-only from the request.
package mirror

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

// Ref is one enumerated ref (for-each-ref row — the repoimport S4
// shape, reused so the filter below speaks the same type).
type Ref struct {
	Name   string
	Oid    string
	Peeled string
}

// Runner spawns stock git for mirror syncs. Binary defaults to "git";
// scratch roots at <cacheDir>/mirror (task-scoped subdirs, swept per
// attempt — no feature state on disk, law 1).
type Runner struct {
	Binary       string
	CacheDir     string
	CloneTimeout time.Duration
	GitTimeout   time.Duration

	pool chan struct{} // bounded git-process semaphore (04 §2)
}

// NewRunner builds a Runner; non-positive timeouts fall back to the
// import-section defaults (the [import] section owns SSRF + timeouts
// for both flows — one gate, one clock).
func NewRunner(binary, cacheDir string, cloneTimeout, gitTimeout time.Duration) *Runner {
	if binary == "" {
		binary = "git"
	}
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	if cloneTimeout <= 0 {
		cloneTimeout = 1800 * time.Second
	}
	if gitTimeout <= 0 {
		gitTimeout = 300 * time.Second
	}
	return &Runner{
		Binary: binary, CacheDir: cacheDir,
		CloneTimeout: cloneTimeout, GitTimeout: gitTimeout,
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

// ScratchDir creates the task-scoped mirror dir
// <cacheDir>/mirror/<owner>/<name>.<nanos>/ (unique per attempt; the
// caller defers os.RemoveAll). Owner/name are ParseRepoId-validated
// upstream, so the path cannot escape.
func (r *Runner) ScratchDir(owner, name string) (string, error) {
	dir := filepath.Join(r.CacheDir, "mirror", owner, fmt.Sprintf("%s.%d", name, time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// CredentialEnv is the per-sync child-env name carrying the
// memory-only token (import S3: one name per spawn chain, never argv,
// never the bucket, never logs).
func CredentialEnv(syncID string) string {
	var b strings.Builder
	b.WriteString("WALGIT_MIRROR_TOKEN_")
	for _, c := range syncID {
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// credentialArgv builds the inline config-pair helper, host-pinned to
// scheme://host (repoimport R1 S5: a redirect to another host never
// harvests the token — git matches credential.<url>.helper by
// prefix). Empty helper first clears inherited helpers (argv order
// significant, 04 §11). Empty envName → nil (public sources stay
// credential-free — the scheduled-fire shape).
func credentialArgv(scheme, host, envName string) []string {
	if envName == "" {
		return nil
	}
	pin := "credential." + scheme + "://" + host + ".helper"
	helper := "!f(){ echo username=x-access-token; echo password=$" + envName + "; };f"
	return []string{"-c", pin + "=", "-c", pin + "=" + helper}
}

// CloneMirror runs the pinned mirror clone (04 §12):
//
//	git -c credential.helper= -c credential.helper=!<helper> clone --mirror -- <url> <dir>
//
// (credential pairs present only for token-bearing first/manual
// syncs, host-pinned per credentialArgv). The ctx carries
// CloneTimeout; cancel SIGKILLs git (exec.CommandContext). Stderr is
// bounded (8 KiB) and scrubbed — tokens never land in task logs.
func (r *Runner) CloneMirror(ctx context.Context, srcURL, dir, scheme, host, tokenEnv, token string) error {
	cctx, cancel := context.WithTimeout(ctx, r.CloneTimeout)
	defer cancel()
	argv := append([]string{},
		append(credentialArgv(scheme, host, tokenEnv), "clone", "--mirror", "--", srcURL, dir)...)
	var extraEnv []string
	if tokenEnv != "" {
		extraEnv = append(extraEnv, tokenEnv+"="+token)
	}
	return r.run(cctx, func() error {
		cmd := exec.CommandContext(cctx, r.Binary, argv...)
		cmd.Env = []string{"PATH=" + pathEnv(), "GIT_TERMINAL_PROMPT=0"}
		cmd.Env = append(cmd.Env, extraEnv...)
		cmd.Stdout = io.Discard
		var errBuf boundedStderr
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("mirror clone: %v: %s", err, scrubText(errBuf.String()))
		}
		return nil
	})
}

// ForEachRef runs the pinned enumeration (04 §12):
//
//	git --git-dir=<dir> for-each-ref --format=%(objectname) %(*objectname) %(refname)
func (r *Runner) ForEachRef(ctx context.Context, dir string) ([]Ref, error) {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	out, errText, err := r.collect(cctx, dir, []string{"for-each-ref", "--format=%(objectname) %(*objectname) %(refname)"}, nil)
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: %v: %s", err, scrubText(errText))
	}
	var refs []Ref
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		oid, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		peeled, name, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		refs = append(refs, Ref{Name: strings.TrimSpace(name), Oid: oid, Peeled: peeled})
	}
	return refs, nil
}

// MergeBaseIsAncestor runs `git merge-base --is-ancestor old new` in
// dir (the §8.3 ff rule): exit 0 = ancestor (ff-ok), exit 1 =
// rewound (refuse + narrate, never sticky-silenced).
func (r *Runner) MergeBaseIsAncestor(ctx context.Context, dir, old, new string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	_, _, err := r.collect(cctx, dir, []string{"merge-base", "--is-ancestor", old, new}, nil)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if isExitError(err, &exit) {
		return false, nil // exit 1: not an ancestor
	}
	return false, err
}

// ProbeObject checks one object exists in dir (04 §12, issue #320):
//
//	git --git-dir=<dir> cat-file -e <oid>
//
// Exit 0 = present; exit 1/128 = missing/corrupt (an error carrying the
// bounded stderr — the servability probe treats any error as
// unservable). The oid is a hex object id from our own ref enumeration
// (never upstream free-text beyond the S2 scrub at the call site).
func (r *Runner) ProbeObject(ctx context.Context, dir, oid string) error {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	_, errText, err := r.collect(cctx, dir, []string{"cat-file", "-e", oid}, nil)
	if err != nil {
		return fmt.Errorf("cat-file -e: %v: %s", err, scrubText(errText))
	}
	return nil
}

func isExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// HeadTarget reads the mirror's HEAD symref ("ref: <target>"); ""
// when detached or unreadable (caller falls back per 04 §1.2).
func HeadTarget(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if t, ok := strings.CutPrefix(s, "ref: "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// ShowObjectFormat reports the scratch repo's object format ("sha1"
// or "sha256" — the source's format, which the trailer checksum
// needs to size the pack trailer).
func (r *Runner) ShowObjectFormat(ctx context.Context, dir string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	out, errText, err := r.collect(cctx, dir, []string{"rev-parse", "--show-object-format"}, nil)
	if err != nil {
		return "", fmt.Errorf("rev-parse --show-object-format: %v: %s", err, scrubText(errText))
	}
	f := strings.TrimSpace(out)
	if f != "sha1" && f != "sha256" {
		return "", fmt.Errorf("unknown object format %q", scrubText(f))
	}
	return f, nil
}

// EnsurePackIdx returns the .idx sibling of packPath, regenerating it
// with `git index-pack <pack>` when missing (a mirror clone always
// writes one, but the tier-0 path must not assume it). Load-bearing
// twice: LevelServe Sync needs it locally AND durably
// (wal/<checksum>.idx) — AddPack installs/uploads the .pack only.
func (r *Runner) EnsurePackIdx(ctx context.Context, packPath string) (string, error) {
	idxPath := strings.TrimSuffix(packPath, ".pack") + ".idx"
	if _, err := os.Stat(idxPath); err == nil {
		return idxPath, nil
	}
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	if _, errText, err := r.collect(cctx, filepath.Dir(packPath), []string{"index-pack", packPath}, nil); err != nil {
		return "", fmt.Errorf("index-pack: %v: %s", err, scrubText(errText))
	}
	if _, err := os.Stat(idxPath); err != nil {
		return "", fmt.Errorf("index-pack produced no idx: %v", err)
	}
	_ = os.Remove(strings.TrimSuffix(packPath, ".pack") + ".keep")
	return idxPath, nil
}

// collect runs argv in dir with GIT_DIR=dir and buffers stdout
// (bounded scrubbed stderr; pool-gated). extraEnv carries per-spawn
// secrets (memory-only tokens).
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

// scrubText redacts credential-shaped secrets from git stderr before
// it reaches task logs, errors, or the sidecar (import S2: the
// canonical URL never carries credentials; this guards concatenated
// strings — clone argv echo, git stderr, helper output).
func scrubText(s string) string {
	out := redactKV(s, "password=")
	out = redactKV(out, "passwd=")
	out = redactKV(out, "token=")
	// userinfo-shaped secrets (scheme://user:pass@host)
	if i := strings.Index(out, "://"); i >= 0 {
		if j := strings.Index(out[i:], "@"); j >= 0 {
			out = out[:i+3] + "[redacted]@" + out[i+j+1:]
		}
	}
	return out
}

// redactKV cuts `key<value>` at the next delimiter (whitespace,
// quote, semicolon, or end of string).
func redactKV(s, key string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(key)])
		b.WriteString("[redacted]")
		j := i + len(key)
		for j < len(s) && !strings.ContainsRune(" \t\n\r\"';", rune(s[j])) {
			j++
		}
		s = s[j:]
	}
}
