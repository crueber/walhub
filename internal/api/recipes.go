// recipes.go — the §9 git recipes of 07_api.md, run against the local serving
// copy that bind_wal.go materializes per request. This file exists so the
// RepoView binding stays readable: every command runs with GIT_DIR pointed at
// the serving copy and GIT_TERMINAL_PROMPT=0, honoring git.binary.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"git.packden.us/crueber/walhub/internal/git"
)

// errRemoteServed marks a repo whose pack set is served through the remote
// reader: the git subprocess recipes below need a local serving copy
// (doc 07 §1: the object renders are git recipes over the materialized copy).
// Mapped to 503 by mapViewErr's default arm.
var errRemoteServed = errors.New("repo is served through the remote reader; JSON object renders need a local serving copy (cache.mode disk or a smaller pack set)")

// maxBlobBytes is the §9.5 JSON-shape cap (raw ?raw= downloads bypass it).
const maxBlobBytes = 2 << 20 // 2 MiB

// maxReadmeBytes bounds the readme body probed on tree renders.
const maxReadmeBytes = 512 << 10

// gitCmd runs one git command in the serving copy and returns stdout.
func (v *walView) gitCmd(ctx context.Context, repo *git.LocalRepo, args ...string) ([]byte, error) {
	binary := v.binary
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = repo.Path
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_DIR="+repo.Path)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// revParse resolves a rev to a full commit sha ("" when unresolvable).
func (v *walView) revParse(ctx context.Context, repo *git.LocalRepo, rev string) (string, error) {
	out, err := v.gitCmd(ctx, repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", nil //nolint:nilerr // unresolvable rev is a miss, not an error
	}
	return strings.TrimSpace(string(out)), nil
}

// pathExists reports whether sha:path resolves (tree or blob).
func (v *walView) pathExists(ctx context.Context, repo *git.LocalRepo, sha, path string) bool {
	_, err := v.gitCmd(ctx, repo, "cat-file", "-e", sha+":"+path)
	return err == nil
}

// isBinary reports NUL-in-first-8KiB (git's heuristic).
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// readmeOf probes the tree entries for a README (case-insensitive) and returns
// its contents when valid UTF-8 (§9.4).
func (v *walView) readmeOf(ctx context.Context, repo *git.LocalRepo, entries []TreeEntry) *Readme {
	for _, e := range entries {
		if e.Type != "blob" || e.Size > maxReadmeBytes {
			continue
		}
		name := strings.TrimSuffix(e.Name, filepathExt(e.Name))
		if !strings.EqualFold(name, "readme") {
			continue
		}
		body, err := v.gitCmd(ctx, repo, "cat-file", "blob", e.SHA)
		if err != nil || len(body) == 0 || len(body) > maxReadmeBytes || !utf8.Valid(body) {
			continue
		}
		return &Readme{Name: e.Name, Contents: string(body)}
	}
	return nil
}

func filepathExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}
