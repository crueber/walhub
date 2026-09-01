// engine.go — the registry handle wrapper used by the wal/bundle/repo commands.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// walHandle pairs a repo handle with its registry cleanup.
type walHandleT struct {
	h       *wal.RepoHandle
	cleanup func()
}

func (w *walHandleT) close() { w.cleanup() }

func (w *walHandleT) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	return w.h.ReadLog(ctx, from, to)
}

func (w *walHandleT) AddPack(ctx context.Context, path, checksum string, tier uint32, meta map[string]string) (wal.PublishResult, error) {
	return w.h.AddPack(ctx, path, checksum, tier, meta)
}

func (w *walHandleT) AnnotatePack(ctx context.Context, checksum string, hasRev, hasBitmap, hasCommitGraph bool) error {
	return w.h.AnnotatePack(ctx, checksum, hasRev, hasBitmap, hasCommitGraph)
}

func (w *walHandleT) manifest() *proto.Manifest {
	m, _ := w.h.ManifestSnapshot()
	return m
}

// openHandle resolves config+store+registry and opens the repo, printing any
// error (callers just return exitErr).
func openHandle(ctx context.Context, c *cli, repoID string) (*walHandleT, error) {
	reg, _, cleanup := openEngine(ctx, c)
	h, err := reg.Open(ctx, repoID)
	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "walhub: open %s: %v\n", repoID, err)
		return nil, err
	}
	return &walHandleT{h: h, cleanup: cleanup}, nil
}

// notImplemented prints the honest esoteric-flow notice (§6.2) and exits 1.
func notImplemented(flow string) int {
	fmt.Fprintf(os.Stderr, "walhub: %s: not yet implemented in this build\n", flow)
	return exitErr
}

// fmtDur renders a config.Duration in the canonical TOML spelling.
func fmtDur(d time.Duration) string {
	return d.String()
}

// fmtBytes renders a byte size with the binary unit suffix.
func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%dGiB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
