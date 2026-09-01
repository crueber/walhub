package git

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors (04_git.md header): callers map these with errors.Is.
var (
	// ErrMaxBytes — the pushed pack exceeded max_bytes while streaming.
	ErrMaxBytes = errors.New("pack exceeds max_bytes")
	// ErrPackRejected — index-pack/fsck rejected the pack.
	ErrPackRejected = errors.New("pack rejected")
	// ErrMissingObject — connectivity check found unreachable/missing objects.
	ErrMissingObject = errors.New("missing object")
	// ErrRefConflict — a compare-and-swap on a ref failed.
	ErrRefConflict = errors.New("ref conflict")
	// ErrTooManyWants — the fetch exceeded git.max_wants.
	ErrTooManyWants = errors.New("too many wants")
)

// RefConflictError carries the CAS detail (04_git.md §4.3): the ref name, the
// expected old value and the actual current value.
type RefConflictError struct {
	Ref      string
	Expected string // "" when the ref must not exist (old = zero)
	Actual   string // "" when the ref does not exist
}

func (e *RefConflictError) Error() string {
	return fmt.Sprintf("ref conflict on %s: expected %q, actual %q", e.Ref, e.Expected, e.Actual)
}

func (e *RefConflictError) Unwrap() error { return ErrRefConflict }

func refConflict(ref, expected, actual string) *RefConflictError {
	return &RefConflictError{Ref: ref, Expected: expected, Actual: actual}
}

// MissingObjectError names the missing oids (capped at 16, 04_git.md §7.1).
type MissingObjectError struct {
	Oids []string
}

func (e *MissingObjectError) Error() string {
	return fmt.Sprintf("missing objects: %s", strings.Join(e.Oids, ", "))
}

func (e *MissingObjectError) Unwrap() error { return ErrMissingObject }

func missingObjects(oids []string) *MissingObjectError { return &MissingObjectError{Oids: oids} }

// TooManyWantsError carries the configured cap.
type TooManyWantsError struct{ Cap int }

func (e *TooManyWantsError) Error() string {
	return fmt.Sprintf("fetch wants more than git.max_wants=%d objects", e.Cap)
}

func (e *TooManyWantsError) Unwrap() error { return ErrTooManyWants }

// PackRejectedError wraps index-pack's verdict.
type PackRejectedError struct{ Detail string }

func (e *PackRejectedError) Error() string { return "pack rejected: " + e.Detail }

func (e *PackRejectedError) Unwrap() error { return ErrPackRejected }

func errSubprocess(argv []string, stderr string) *GitError {
	return &GitError{Kind: GitErrSubprocess, Cmd: strings.Join(argv, " "), Stderr: stderr}
}

func errInvalidInput(format string, args ...any) *GitError {
	return &GitError{Kind: GitErrInvalidInput, Detail: fmt.Sprintf(format, args...)}
}
