// Package wal: shared types for the WAL engine (05_wal_engine.md). The engine owner
// implements the machinery in sibling files against these frozen contracts.
package wal

import (
	"context"
	"fmt"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// SyncLevel — how much of a repo a request materializes (doc 05 §2).
type SyncLevel int

const (
	LevelRefs SyncLevel = iota
	LevelServe
	LevelFull
)

func (l SyncLevel) String() string {
	switch l {
	case LevelRefs:
		return "refs"
	case LevelServe:
		return "serve"
	case LevelFull:
		return "full"
	default:
		return "objects"
	}
}

// ObjectAccess is what a request uses for object reads after a sync.
type ObjectAccess struct {
	// Local: packs are on disk; run git against local.Repo.
	Local *git.LocalRepo
	// Remote: the pack set does not fit; reads go through the remote reader.
	Remote *RemoteReader
}

func (o ObjectAccess) IsRemote() bool { return o.Remote != nil }

// RemoteReader is the remote-objects accessor (built by the engine owner; doc 05 §6.7).
// CONTRACT: methods below are the surface server/api call.
type RemoteReader struct {
	Revision uint64 // manifest revision this reader was built for
}

// Locate returns (packIndex, offset) for oid; ok=false when absent.
func (r *RemoteReader) Locate(oid string) (int, int64, bool) { panic("unimplemented") }

// Header returns kind + inflated size without materializing.
func (r *RemoteReader) Header(oid string) (kind string, size int64, err error) {
	panic("unimplemented")
}

// Decode returns the full object contents (iterative delta resolution, LRU-cached).
func (r *RemoteReader) Decode(ctx context.Context, oid string) (kind string, data []byte, err error) {
	panic("unimplemented")
}

// RefError is a per-ref rejection reason.
type RefError struct {
	Kind   RefErrorKind
	Ref    string
	Detail string
}

type RefErrorKind int

const (
	RefErrNonFastForward RefErrorKind = iota
	RefErrConflict
	RefErrRejected
	RefErrMissing
)

func (e *RefError) Error() string {
	switch e.Kind {
	case RefErrNonFastForward:
		return fmt.Sprintf("non-fast-forward: %s %s", e.Ref, e.Detail)
	case RefErrConflict:
		return fmt.Sprintf("conflict: %s %s", e.Ref, e.Detail)
	case RefErrMissing:
		return fmt.Sprintf("missing object: %s %s", e.Ref, e.Detail)
	default:
		return fmt.Sprintf("rejected: %s %s", e.Ref, e.Detail)
	}
}

// WalError is the engine's error type.
type WalError struct {
	Kind    WalErrorKind
	Detail  string
	Ref     *RefError // for KindWalRefConflict
	Wrapped error
}

type WalErrorKind int

const (
	WalErrNotFound WalErrorKind = iota
	WalErrAlreadyExists
	WalErrRefConflict
	WalErrStore
	WalErrGit
	WalErrCorrupt
	WalErrInvalid
	WalErrRetry
	WalErrTooLarge
	WalErrIo
)

func (e *WalError) Error() string {
	switch e.Kind {
	case WalErrNotFound:
		return "repository not found: " + e.Detail
	case WalErrAlreadyExists:
		return "repository already exists: " + e.Detail
	case WalErrTooLarge:
		return "pack set too large for this instance: " + e.Detail
	case WalErrRetry:
		return fmt.Sprintf("CAS retries exhausted (%s)", e.Detail)
	default:
		return e.Detail
	}
}

func (e *WalError) Unwrap() error { return e.Wrapped }

func ErrNotFound(repo string) *WalError { return &WalError{Kind: WalErrNotFound, Detail: repo} }
func ErrExists(repo string) *WalError   { return &WalError{Kind: WalErrAlreadyExists, Detail: repo} }
func ErrTooLarge(bytes, max int64) *WalError {
	return &WalError{Kind: WalErrTooLarge, Detail: fmt.Sprintf(
		"repository pack set is %d bytes, larger than this instance's cache limit (%d bytes); clone via bundle-uri", bytes, max)}
}

// PublishResult — per-ref results of a publish; seq is the commit point (0 = nothing committed).
type PublishResult struct {
	Seq    uint64
	PerRef []RefResult
}

type RefResult struct {
	Name string
	Err  *RefError // nil = ok
}

// CheckpointTrigger names why a checkpoint fired (doc 05 §5).
type CheckpointTrigger string

const (
	TriggerEntries   CheckpointTrigger = "entries"
	TriggerAge       CheckpointTrigger = "age"
	TriggerTailBytes CheckpointTrigger = "tail-bytes"
	TriggerManual    CheckpointTrigger = "manual"
)

// TaskRecord — a narrated unit of long work (doc 05 §7; wire shape per MASTER_RUST_SPEC §2.7/§9.4).
type TaskRecord struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Repo      string            `json:"repo"`
	Hostname  string            `json:"hostname"`
	Started   string            `json:"started"`            // RFC3339 millis UTC
	Finished  string            `json:"finished,omitempty"` // present when done
	ElapsedMS int64             `json:"elapsed_ms"`
	OK        *bool             `json:"ok,omitempty"` // nil = running
	Summary   string            `json:"summary"`
	Progress  *Progress         `json:"progress,omitempty"`
	LogTail   []string          `json:"log_tail"`
	Params    map[string]string `json:"params,omitempty"`
}

// Progress packets (SSE envelope payloads; MASTER_RUST_SPEC §9.3).
type Progress struct {
	Kind    string      `json:"-"` // "notice" | "progress" | "task"
	Text    string      `json:"text,omitempty"`
	Label   string      `json:"label,omitempty"`
	Done    uint64      `json:"done,omitempty"`
	Total   *uint64     `json:"total,omitempty"`
	Unit    string      `json:"unit,omitempty"`
	Percent *float64    `json:"percent,omitempty"`
	Task    *TaskRecord `json:"task,omitempty"`
}

// RepoState — the persisted per-repo cache state (doc 05 §1, file walgit-state.json).
type RepoState struct {
	ManifestVersion     string   `json:"manifest_version"`
	AppliedSeq          uint64   `json:"applied_seq"`
	Revision            uint64   `json:"revision"`
	PacksRevision       uint64   `json:"packs_revision"`
	PendingPackRemovals []string `json:"pending_pack_removals"`
	RemoteServed        []string `json:"remote_served"`
}

func (s *RepoState) PacksReady() bool {
	return s.PacksRevision == s.Revision && len(s.PendingPackRemovals) == 0
}

// Time helpers shared by replay/checkpoints (doc 05 §6: monotonic created_at floor).
func MaxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func TsPtr(t time.Time) *proto.Timestamp { p := proto.TimeFromGo(t); return &p }
