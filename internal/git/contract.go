// Package git: shared types (04_git.md §7.1). The git owner implements the machinery in
// sibling files; these are the cross-package contracts (store/wal/server import them).
package git

import "fmt"

// RepoId is "<owner>/<name>". Parts: ASCII [A-Za-z0-9._-], 1..=100 chars, no leading ".",
// not "..". ParseRepoId accepts "owner/name" and "owner/name.git" (suffix stripped).
type RepoId struct {
	Owner string
	Name  string
}

func validPart(s string) bool {
	if len(s) == 0 || len(s) > 100 || s == ".." || s[0] == '.' {
		return false
	}
	for i := range s {
		if !isIDChar(s[i]) {
			return false
		}
	}
	return true
}

func isIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// ParseRepoId parses "owner/name" or "owner/name.git".
func ParseRepoId(s string) (RepoId, error) {
	trimmed := s
	if len(trimmed) > 4 && trimmed[len(trimmed)-4:] == ".git" {
		trimmed = trimmed[:len(trimmed)-4]
	}
	for i := range trimmed {
		if trimmed[i] == '/' {
			owner, name := trimmed[:i], trimmed[i+1:]
			if !validPart(owner) || !validPart(name) {
				return RepoId{}, fmt.Errorf("invalid repo id %q", s)
			}
			return RepoId{Owner: owner, Name: name}, nil
		}
	}
	return RepoId{}, fmt.Errorf("repo id must be `owner/name`, got %q", s)
}

func (r RepoId) String() string      { return r.Owner + "/" + r.Name }
func (r RepoId) StorePrefix() string { return "repos/" + r.Owner + "/" + r.Name + "/" }
func (r RepoId) LocalDir(root string) string {
	return root + "/" + r.Owner + "/" + r.Name + ".git"
}

// ObjectFormat is the repository hash algorithm.
type ObjectFormat int

const (
	Sha1 ObjectFormat = iota
	Sha256
)

func (f ObjectFormat) String() string {
	if f == Sha256 {
		return "sha256"
	}
	return "sha1"
}

func ObjectFormatFrom(s string) (ObjectFormat, error) {
	if s == "sha256" {
		return Sha256, nil
	}
	if s == "sha1" {
		return Sha1, nil
	}
	return Sha1, fmt.Errorf("unknown object format %q (expected sha1 or sha256)", s)
}

// ZeroHex returns the all-zero oid string for the format (delete/absent marker).
func (f ObjectFormat) ZeroHex() string {
	if f == Sha256 {
		return zero64
	}
	return zero40
}

const (
	zero40 = "0000000000000000000000000000000000000000"
	zero64 = "0000000000000000000000000000000000000000000000000000000000000000"
)

// ValidOid reports whether s is empty/all-zero (absent marker) or 40/64 lowercase hex.
func ValidOid(s string) bool {
	if s == "" {
		return true
	}
	isZero := true
	for i := range s {
		c := s[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
		if !hex {
			return false
		}
		if c != '0' {
			isZero = false
		}
	}
	return isZero || len(s) == 40 || len(s) == 64
}

// GitError variants (04_git.md — one struct + kind instead of the Rust enum).
type GitError struct {
	Kind   GitErrorKind
	Detail string
	Cmd    string // Subprocess: the argv
	Stderr string // Subprocess: captured stderr
}

type GitErrorKind int

const (
	GitErrIo GitErrorKind = iota
	GitErrPack
	GitErrRefConflict
	GitErrMissingObject
	GitErrFsck
	GitErrSubprocess
	GitErrInvalidInput
	GitErrProtocol
)

func (e *GitError) Error() string {
	switch e.Kind {
	case GitErrSubprocess:
		return fmt.Sprintf("git %s failed: %s", e.Cmd, e.Stderr)
	case GitErrRefConflict:
		return "ref conflict: " + e.Detail
	case GitErrMissingObject:
		return "missing object: " + e.Detail
	case GitErrProtocol:
		return "protocol error: " + e.Detail
	case GitErrInvalidInput:
		return "invalid input: " + e.Detail
	default:
		return e.Detail
	}
}

// IngestOptions shape a pack ingest (04_git.md §7.2).
type IngestOptions struct {
	Fsck     bool
	MaxBytes int64 // 0 = unlimited
	Thin     bool  // receive-pack always true (--fix-thin)
}

// IngestedPack is the result of a successful ingest.
type IngestedPack struct {
	Checksum    string // pack trailing SHA, hex
	PackPath    string
	IdxPath     string
	RevPath     string
	PackSize    uint64
	IdxSize     uint64
	ObjectCount uint64
}

// LocalRepo is the on-disk bare repository handle (04_git.md §7.1). The git owner
// implements it; this stub exists so dependents compile against the shape.
type LocalRepo struct {
	Root string // cache root
	ID   RepoId
	Path string // <root>/<owner>/<name>.git
}

// OpenLocalRepo opens <root>/<owner>/<name>.git (nil, nil) when absent.
func OpenLocalRepo(root string, id RepoId) (*LocalRepo, error) { panic("unimplemented") }

// InitLocalRepo creates the bare repo with the §7.1 config keys.
func InitLocalRepo(root string, id RepoId, format ObjectFormat) (*LocalRepo, error) { panic("unimplemented") }
