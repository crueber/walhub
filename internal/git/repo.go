package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Repo is the repo handle type used by every Layer method (04_git.md header).
// It is the frozen contract type: the on-disk bare repository handle.
type Repo = LocalRepo

// Type Oid is the oid string type for this package (04_git.md §1.2). It is an
// alias so the frozen contract's string-typed fields interoperate directly.
type Oid = string

const defaultHeadTarget = "refs/heads/main"

// headFileContent is the exact HEAD seed written by InitLocalRepo and Ingest
// scratch dirs (04_git.md §1.2/§3.1).
const headFileContent = "ref: refs/heads/main\n"

// OpenLocalRepo opens <root>/<owner>/<name>.git; (nil, nil) when absent.
func OpenLocalRepo(root string, id RepoId) (*LocalRepo, error) {
	path := id.LocalDir(root)
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !st.IsDir() {
		return nil, errInvalidInput("local repo path %s is not a directory", path)
	}
	return &LocalRepo{Root: root, ID: id, Path: path}, nil
}

// InitLocalRepo creates the bare repo with the §7.1 config keys:
//
//	git init --bare [--object-format=sha256]
//
// then appends the uploadpack/pack config keys (idempotent on re-init) and
// writes HEAD → refs/heads/main. `git init` runs with a 30 s timeout via the
// shared exec helper, counted against the pool for uniform accounting.
func InitLocalRepo(root string, id RepoId, format ObjectFormat) (*LocalRepo, error) {
	path := id.LocalDir(root)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	l := NewLayer()
	argv := []string{"init", "--bare"}
	if format == Sha256 {
		argv = append(argv, "--object-format=sha256")
	}
	argv = append(argv, ".")
	if _, err := l.runPooled(context.Background(), execSpec{
		argv:    argv,
		dir:     path,
		timeout: 30 * time.Second,
	}); err != nil {
		return nil, err
	}
	if err := WriteRepoConfig(path); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte(headFileContent), 0o644); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(path, "objects", "pack"), 0o755); err != nil {
		return nil, err
	}
	return &LocalRepo{Root: root, ID: id, Path: path}, nil
}

// repoConfigBlock is the §1.2 config keys, verbatim.
const repoConfigBlock = `[uploadpack]
	allowFilter = true
	allowAnySHA1InWant = true
	allowSidebandAll = true
[pack]
	writeReverseIndex = true
`

// WriteRepoConfig appends the §1.2 keys to <repo>/config (idempotent re-init
// rewrites the same keys — sections/keys already present are not duplicated).
func WriteRepoConfig(repoPath string) error {
	cfgPath := filepath.Join(repoPath, "config")
	existing, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var b strings.Builder
	b.Write(existing)
	text := b.String()
	for _, line := range strings.Split(strings.TrimRight(repoConfigBlock, "\n"), "\n") {
		if strings.Contains(text, line) {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(cfgPath, []byte(b.String()), 0o644)
}

// Format detects the repo's object format: extensions.objectformat in the repo
// config (sha256) or sha1. A file scan — no subprocess, never fails.
func (r *LocalRepo) Format() ObjectFormat {
	data, err := os.ReadFile(filepath.Join(r.Path, "config"))
	if err != nil {
		return Sha1
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.ToLower(strings.Trim(t, "[]"))
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		if section == "extensions" && strings.TrimSpace(k) == "objectformat" &&
			strings.TrimSpace(v) == "sha256" {
			return Sha256
		}
	}
	return Sha1
}

// ZeroOid is the all-zero oid for the repo's format.
func (r *LocalRepo) ZeroOid() Oid { return r.Format().ZeroHex() }

// ObjectsDir is the absolute objects directory.
func (r *LocalRepo) ObjectsDir() string { return filepath.Join(r.Path, "objects") }

// PackDir is the absolute objects/pack directory.
func (r *LocalRepo) PackDir() string { return filepath.Join(r.ObjectsDir(), "pack") }

// WriteHeadSeed writes the §1.2 HEAD seed (format-irrelevant).
func writeHeadSeed(dir string) error {
	return os.WriteFile(filepath.Join(dir, "HEAD"), []byte(headFileContent), 0o644)
}

// SetSymbolicHead applies a symbolic HEAD update by direct file write
// (04_git.md §4.3 — HEAD is never passed to update-ref).
func SetSymbolicHead(repo *LocalRepo, target string) error {
	if err := ValidateRefName(target); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(repo.Path, "HEAD"), []byte("ref: "+target+"\n"), 0o644)
}

// readHead reads HEAD: returns (target, oid). target non-empty → symbolic;
// otherwise oid is the detached value ("" when unparseable/unborn).
func readHead(repo *LocalRepo) (target string, oid Oid) {
	data, err := os.ReadFile(filepath.Join(repo.Path, "HEAD"))
	if err != nil {
		return "", ""
	}
	s := strings.TrimSpace(string(data))
	if t, ok := strings.CutPrefix(s, "ref: "); ok {
		return strings.TrimSpace(t), ""
	}
	if ValidOid(s) && !isZeroOid(s) {
		return "", s
	}
	return "", ""
}

func isZeroOid(s string) bool {
	for i := range s {
		if s[i] != '0' {
			return false
		}
	}
	return len(s) > 0
}

// GitVersion parses the running git binary's version (major, minor).
func (l *Layer) GitVersion(ctx context.Context) (int, int, error) {
	out, _, err := l.runCollect(ctx, execSpec{argv: []string{"--version"}, timeout: 10 * time.Second})
	if err != nil {
		return 0, 0, err
	}
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "git version %d.%d", &major, &minor); err != nil {
		return 0, 0, errInvalidInput("cannot parse git version from %q", string(out))
	}
	return major, minor, nil
}
