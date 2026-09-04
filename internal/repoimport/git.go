// git.go — the git subprocess runner (docs/go/04_git.md §2 discipline):
// stock git only, pinned argv, GIT_TERMINAL_PROMPT=0 on every spawn, the
// bounded pool (never bare on request goroutines), ctx timeouts, token via
// child env only (S3: the credential -c argv is built dynamically per
// spawn with a per-task env name — static copy-paste is forbidden).
package repoimport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
)

// pool is the bounded semaphore of concurrent git processes (04_git.md §2:
// capacity 4 × GOMAXPROCS when non-positive — the pulls gitPool shape).
type pool struct{ sem chan struct{} }

func newPool(capacity int) *pool {
	if capacity <= 0 {
		capacity = 4 * runtime.GOMAXPROCS(0)
	}
	return &pool{sem: make(chan struct{}, capacity)}
}

func (p *pool) run(ctx context.Context, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Runner spawns stock git for the import task. Binary is git.binary;
// timeouts come from [import] (S8); scratch roots at cache.dir/import.
type Runner struct {
	Binary       string
	Pool         *pool
	CloneTimeout time.Duration
	GitTimeout   time.Duration
	CacheDir     string
}

func newRunner(binary string, cfg *config.Config) *Runner {
	r := &Runner{Binary: binary, Pool: newPool(0), CacheDir: cfg.Cache.Dir}
	r.CloneTimeout = time.Duration(cfg.Import.CloneTimeout)
	if r.CloneTimeout <= 0 {
		r.CloneTimeout = 1800 * time.Second
	}
	r.GitTimeout = time.Duration(cfg.Import.GitTimeout)
	if r.GitTimeout <= 0 {
		r.GitTimeout = 300 * time.Second
	}
	if r.CacheDir == "" {
		r.CacheDir = os.TempDir()
	}
	return r
}

// ProgressFunc receives parsed clone progress (label "clone",
// unit "objects"|"bytes"|"deltas").
type ProgressFunc func(label string, done, total uint64, unit string)

// ScratchDir creates the task-scoped mirror dir
// <cache.dir>/import/<owner>/<name>.<nanos>/ (unique per attempt; the
// caller defers os.RemoveAll — the 04 §3.1 pattern). Owner/name are
// ParseRepoId-validated upstream, so the path cannot escape.
func (r *Runner) ScratchDir(owner, name string) (string, error) {
	dir := filepath.Join(r.CacheDir, "import", owner, fmt.Sprintf("%s.%d", name, time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// CredentialEnv is the per-task child-env name carrying the token (S3:
// one name per spawn chain, never argv, never the bucket).
func CredentialEnv(taskID string) string {
	var b strings.Builder
	b.WriteString("WALGIT_IMPORT_TOKEN_")
	for _, c := range taskID {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// credentialArgv builds the inline config-pair helper, host-pinned to
// scheme://host (R1 S5: a redirect to another host never harvests the
// token — git matches credential.<url>.helper by prefix). The empty
// helper first clears inherited helpers (argv order significant, 04 §11).
// No token → no helper (public sources stay credential-free).
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
//	git -c credential.helper= -c credential.helper=!<helper> clone --mirror --progress -- <url> <dir>
//
// (credential pairs present only for token sources, host-pinned per
// credentialArgv). emit receives parsed --progress bars; beat fires at
// least every 15 s of git silence (law 7 — the heartbeat floor). The ctx
// carries CloneTimeout; cancel SIGKILLs git (exec.CommandContext).
func (r *Runner) CloneMirror(ctx context.Context, srcURL, dir, scheme, host, tokenEnv, token string, emit ProgressFunc, beat func()) error {
	cctx, cancel := context.WithTimeout(ctx, r.CloneTimeout)
	defer cancel()
	argv := append([]string{},
		append(credentialArgv(scheme, host, tokenEnv), "clone", "--mirror", "--progress", "--", srcURL, dir)...)
	var extraEnv []string
	if tokenEnv != "" {
		extraEnv = append(extraEnv, tokenEnv+"="+token)
	}
	return r.Pool.run(cctx, func() error {
		cmd := exec.CommandContext(cctx, r.Binary, argv...)
		cmd.Env = []string{"PATH=" + pathEnv(), "GIT_TERMINAL_PROMPT=0"}
		cmd.Env = append(cmd.Env, extraEnv...)
		// Clone writes nothing useful on stdout; stderr carries progress.
		cmd.Stdout = io.Discard
		var errBuf boundedStderr
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("clone: stderr pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("clone: start: %v", err)
		}
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			parseCloneProgress(stderr, &errBuf, emit)
		}()
		// Heartbeat: a quiet-but-alive clone must not look stalled (law 7).
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		go func() {
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					beat()
				case <-cctx.Done():
					return
				}
			}
		}()
		wg.Wait()
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("clone: %v: %s", err, scrubError(errBuf.String()))
		}
		return nil
	})
}

// cloneProgressRe parses `Receiving objects: 45% (123/456), 1.23 MiB`
// style lines (also "Resolving deltas" and "remote: Enumerating
// objects"/"Counting objects" which lack counts).
var cloneProgressRe = regexp.MustCompile(`(Receiving objects|Resolving deltas|Enumerating objects|Counting objects):\s+(\d+)%\s*(?:\((\d+)/(\d+)\))?`)

// parseCloneProgress drains r (git's stderr), forwarding parsed bars to
// emit and buffering the tail into errBuf (bounded 8 KiB, 04 §2).
func parseCloneProgress(r io.Reader, errBuf *boundedStderr, emit ProgressFunc) {
	buf := make([]byte, 4096)
	var tail []byte
	flush := func() {
		for len(tail) > 0 {
			i := bytes.IndexByte(tail, '\n')
			var line []byte
			if i < 0 {
				line, tail = tail, nil
			} else {
				line, tail = tail[:i], tail[i+1:]
			}
			// git rewrites progress with \r; split carriage returns.
			for _, part := range bytes.Split(line, []byte{'\r'}) {
				handleCloneLine(strings.TrimSpace(string(part)), errBuf, emit)
			}
		}
	}
	for {
		n, err := r.Read(buf)
		if n > 0 {
			tail = append(tail, buf[:n]...)
			flush()
		}
		if err != nil {
			if len(tail) > 0 {
				handleCloneLine(strings.TrimSpace(string(tail)), errBuf, emit)
			}
			return
		}
	}
}

// handleCloneLine routes one stderr line: progress bars to emit, anything
// else (scrubbed) into the bounded error tail.
func handleCloneLine(line string, errBuf *boundedStderr, emit ProgressFunc) {
	if line == "" {
		return
	}
	if m := cloneProgressRe.FindStringSubmatch(line); m != nil && emit != nil {
		label, unit := "clone", "objects"
		switch m[1] {
		case "Resolving deltas":
			unit = "deltas"
		case "Enumerating objects", "Counting objects":
			unit = "refs"
		}
		var done, total uint64
		if m[3] != "" {
			done, _ = strconv.ParseUint(m[3], 10, 64)
		}
		if m[4] != "" {
			total, _ = strconv.ParseUint(m[4], 10, 64)
		}
		emit(label, done, total, unit)
		return
	}
	errBuf.WriteString(scrubError(scrubURL(line)) + "\n")
}

// boundedStderr is the 8 KiB stderr ring (04 §2: kept for error text).
type boundedStderr struct {
	mu  sync.Mutex
	buf []byte
}

func (b *boundedStderr) WriteString(s string) { _, _ = b.Write([]byte(s)) }

// Write appends (io.Writer so the buffer plugs straight into cmd.Stderr).
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

// Ref is one enumerated source ref (for-each-ref row).
type Ref struct {
	Name   string // full refname, e.g. refs/heads/main
	Oid    string // object the ref points at (tag object for annotated tags)
	Peeled string // peeled commit for annotated tags ("" otherwise)
}

// ForEachRef runs the pinned enumeration (04 §12):
//
//	git --git-dir=<dir> for-each-ref --format=%(objectname) %(*objectname) %(refname)
//
// Refnames cannot contain spaces (04 §4.3 validation), so the three fields
// split unambiguously on the first two spaces ("%(*objectname)" renders
// empty for non-tags, giving two adjacent spaces). Runs under GitTimeout.
func (r *Runner) ForEachRef(ctx context.Context, dir string) ([]Ref, error) {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	out, errText, err := r.collect(cctx, dir, []string{"for-each-ref", "--format=%(objectname) %(*objectname) %(refname)"}, nil)
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: %v: %s", err, scrubError(errText))
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

// ShowObjectFormat reports the scratch repo's object format ("sha1" or
// "sha256" — the source's format, which the target follows).
func (r *Runner) ShowObjectFormat(ctx context.Context, dir string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	out, errText, err := r.collect(cctx, dir, []string{"rev-parse", "--show-object-format"}, nil)
	if err != nil {
		return "", fmt.Errorf("rev-parse --show-object-format: %v: %s", err, scrubError(errText))
	}
	f := strings.TrimSpace(out)
	if f != "sha1" && f != "sha256" {
		return "", fmt.Errorf("unknown object format %q", scrubError(f))
	}
	return f, nil
}

// HeadTarget reads the mirror's HEAD symref ("ref: <target>"); "" when
// detached or unreadable (caller falls back per 04 §1.2).
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

// EnsurePackIdx returns the .idx sibling of packPath, regenerating it
// with `git index-pack <pack>` when missing (a mirror clone always
// writes one, but the tier-0 reuse path must not assume it). The idx is
// load-bearing: LevelServe materialize needs it locally AND durably
// (wal/<checksum>.idx) — AddPack installs/uploads the .pack only, so the
// caller installs + uploads the idx around AddPack (no internal/wal
// touch, R1 S12). Runs under GitTimeout.
func (r *Runner) EnsurePackIdx(ctx context.Context, packPath string) (string, error) {
	idxPath := strings.TrimSuffix(packPath, ".pack") + ".idx"
	if _, err := os.Stat(idxPath); err == nil {
		return idxPath, nil
	}
	cctx, cancel := context.WithTimeout(ctx, r.GitTimeout)
	defer cancel()
	if _, errText, err := r.collect(cctx, filepath.Dir(packPath), []string{"index-pack", packPath}, nil); err != nil {
		return "", fmt.Errorf("index-pack: %v: %s", err, scrubError(errText))
	}
	if _, err := os.Stat(idxPath); err != nil {
		return "", fmt.Errorf("index-pack produced no idx: %v", err)
	}
	// index-pack may leave a .keep beside a regenerated index; the
	// repack path deletes stray markers first (04 §9) — sweep it here.
	_ = os.Remove(strings.TrimSuffix(packPath, ".pack") + ".keep")
	return idxPath, nil
}

// collect runs argv in dir with GIT_DIR=dir and buffers stdout (bounded
// stderr; pool-gated). extraEnv carries per-spawn secrets (S3).
func (r *Runner) collect(ctx context.Context, dir string, argv []string, extraEnv []string) (string, string, error) {
	var stdout, errText string
	var runErr error
	runErr = r.Pool.run(ctx, func() error {
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

// pathEnv is the minimal PATH for git's helpers.
func pathEnv() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return "/usr/bin:/bin"
}

// --- S4 refmap (decided) ---------------------------------------------------------------

// FilterRefs applies the S4 refmap: import branches + tags; always drop
// refs/replace/* (rewrites object identity), refs/meta/*,
// refs/keep-around/*; drop refs/pull/*, refs/changes/*, refs/review/*
// unless includePullHeads (heads only, verbatim — never /merge);
// drop refs/notes/* unless includeNotes. Requested refs (exact names)
// and defaultBranchOnly further narrow the survivors. Everything else is
// kept verbatim ("drop nothing else").
func FilterRefs(all []Ref, includePullHeads, includeNotes bool, requested []string, defaultBranchOnly bool, headTarget string) []Ref {
	keep := func(name string) bool {
		switch {
		case strings.HasPrefix(name, "refs/replace/"),
			strings.HasPrefix(name, "refs/meta/"),
			strings.HasPrefix(name, "refs/keep-around/"):
			return false
		case strings.HasPrefix(name, "refs/pull/"),
			strings.HasPrefix(name, "refs/changes/"),
			strings.HasPrefix(name, "refs/review/"):
			return includePullHeads && isPullHead(name)
		case strings.HasPrefix(name, "refs/notes/"):
			return includeNotes
		}
		return true
	}
	out := make([]Ref, 0, len(all))
	for _, r := range all {
		if keep(r.Name) {
			out = append(out, r)
		}
	}
	if defaultBranchOnly && headTarget != "" {
		filtered := out[:0]
		for _, r := range out {
			if r.Name == headTarget {
				filtered = append(filtered, r)
			}
		}
		out = filtered
	}
	if len(requested) > 0 {
		want := map[string]bool{}
		for _, q := range requested {
			want[q] = true
		}
		filtered := out[:0]
		for _, r := range out {
			if want[r.Name] {
				filtered = append(filtered, r)
			}
		}
		out = filtered
	}
	return out
}

// isPullHead allows refs/pull/<N>/head only (numeric N, verbatim import —
// never /merge, which are computed and often dangling).
func isPullHead(name string) bool {
	rest, ok := strings.CutPrefix(name, "refs/pull/")
	if !ok {
		return false
	}
	num, tail, ok := strings.Cut(rest, "/")
	if !ok || tail != "head" || num == "" {
		return false
	}
	for i := 0; i < len(num); i++ {
		if num[i] < '0' || num[i] > '9' {
			return false
		}
	}
	return true
}
