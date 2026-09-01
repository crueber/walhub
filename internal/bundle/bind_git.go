package bundle

// bind_git.go — the real binding of Primitives to internal/git (§8.2 seam,
// §7.8 primitives; exact argv normative in 08_bundles.md). Every call runs
// with GIT_TERMINAL_PROMPT=0 and the repo dir as GIT_DIR, via internal/git's
// configured binary.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
)

// GitPrimitives binds Primitives to a git.Layer.
type GitPrimitives struct {
	L *git.Layer
}

var _ Primitives = (*GitPrimitives)(nil)

// BundleCreate runs `git bundle create <out> --stdin` (§8.9.3) through the
// git layer's pooled exec, returning size + PACK-magic offset.
func (p *GitPrimitives) BundleCreate(ctx context.Context, repoDir, outPath string, refs []string) (int64, int64, error) {
	repo := &git.LocalRepo{Path: repoDir}
	// `git bundle create --stdin` accepts `<oid> <ref>` lines; the layer
	// feeds them verbatim (§8.9.3).
	return p.L.CreateBundle(ctx, repo, outPath, refs, nil)
}

// PackDelta runs (§8.9.2 — self-contained, never --thin):
//
//	git pack-objects --revs --delta-base-offset --stdout -q [--filter=blob:none]
//	stdin: <tip-oid>\n… ^<base-tip-oid>\n…
//
// TODO-INTEGRATION: internal/git has not exposed this §7.8 primitive yet; the
// direct exec below uses the configured binary via git.Layer's default and
// must be replaced when the layer grows PackDelta.
func (p *GitPrimitives) PackDelta(ctx context.Context, repoDir string, wants, excludes []string, filter string, w io.Writer) error {
	argv := []string{"pack-objects", "--revs", "--delta-base-offset", "--stdout", "-q"}
	if filter != "" {
		argv = append(argv, "--filter="+filter)
	}
	cmd := exec.CommandContext(ctx, gitBinary(), argv...)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = w
	var stdin bytes.Buffer
	for _, want := range wants {
		stdin.WriteString(want + "\n")
	}
	for _, ex := range excludes {
		stdin.WriteString("^" + ex + "\n")
	}
	cmd.Stdin = &stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %v: %w: %s", argv, err, tail(stderr.String(), 200))
	}
	return nil
}

// CountCommits runs (§8.7):
//
//	git rev-list --count <tip oids…> --not <base-tip oids…>
//
// over the local commit-graph. TODO-INTEGRATION: same seam as PackDelta —
// replace when internal/git exposes CountCommits.
func (p *GitPrimitives) CountCommits(ctx context.Context, repoDir string, tips, notTips []string) (int, error) {
	if len(tips) == 0 {
		return 0, nil
	}
	argv := []string{"rev-list", "--count"}
	argv = append(argv, tips...)
	if len(notTips) > 0 {
		argv = append(argv, "--not")
		argv = append(argv, notTips...)
	}
	cmd := exec.CommandContext(ctx, gitBinary(), argv...)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("git rev-list --count: %w: %s", err, tail(stderr.String(), 200))
	}
	n := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(out.String()), "%d", &n); err != nil {
		return 0, fmt.Errorf("bundle: rev-list --count output %q: %w", out.String(), err)
	}
	return n, nil
}

// gitBinary resolves the configured binary (§decision: all git invocations go
// through internal/git's configured binary — git.binary).
func gitBinary() string {
	if b := os.Getenv("WALGIT_GIT_BINARY"); b != "" {
		return b
	}
	return "git"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
