// bind_wal_test.go — the composition seam against the real WAL engine: source
// listing, handle open errors, and SyncRefs/ReadLog over a published txn.
package events

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

func walBindCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	return cfg
}

func bindTxn(name, old, new string) *proto.RefTransaction {
	return &proto.RefTransaction{Updates: []*proto.RefUpdate{{Name: name, OldOid: old, NewOid: new}}}
}

func TestWALSource_ReposAndHandles(t *testing.T) {
	ctx := context.Background()
	reg := wal.NewRegistry(ctx, store.NewMemory(), walBindCfg(t))
	defer reg.Close()
	if _, err := reg.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	src := NewRegistrySource(reg)

	repos, err := src.Repos(ctx)
	if err != nil || len(repos) != 1 || repos[0] != "acme/api" {
		t.Fatalf("repos = %v err = %v", repos, err)
	}

	// Bad repo id fails validation before touching the engine.
	if _, err := src.Handle(ctx, "no-slash"); err == nil {
		t.Fatal("bad repo id must error")
	}
	// Unknown repo surfaces the engine's not-found error.
	if _, err := src.Handle(ctx, "no/such"); err == nil {
		t.Fatal("unknown repo must error")
	}
	// Known repo yields a usable view.
	if _, err := src.Handle(ctx, "acme/api"); err != nil {
		t.Fatalf("open: %v", err)
	}
}

func TestWALRepo_SyncRefsAndReadLog(t *testing.T) {
	ctx := context.Background()
	reg := wal.NewRegistry(ctx, store.NewMemory(), walBindCfg(t))
	defer reg.Close()
	h, err := reg.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Publish(ctx, wal.PublishRequest{
		Txn: bindTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("a", 40)),
	}); err != nil {
		t.Fatal(err)
	}

	repo := &WALRepo{h: h}
	state, err := repo.SyncRefs(ctx)
	if err != nil {
		t.Fatalf("sync refs: %v", err)
	}
	if state.HeadSeq != 1 {
		t.Fatalf("head = %d, want 1", state.HeadSeq)
	}
	if state.Sha256 {
		t.Fatal("sha1 repo must report Sha256=false")
	}

	entries, err := repo.ReadLog(ctx, 1, state.HeadSeq)
	if err != nil || len(entries) != 1 || entries[0].Seq != 1 {
		t.Fatalf("read log = %d entries, err %v", len(entries), err)
	}
}

func TestWALRepo_SyncRefsSha256(t *testing.T) {
	ctx := context.Background()
	reg := wal.NewRegistry(ctx, store.NewMemory(), walBindCfg(t))
	defer reg.Close()
	h, err := reg.Create(ctx, "acme/big", git.Sha256)
	if err != nil {
		t.Fatal(err)
	}
	state, err := (&WALRepo{h: h}).SyncRefs(ctx)
	if err != nil {
		t.Fatalf("sync refs: %v", err)
	}
	if !state.Sha256 {
		t.Fatal("sha256 repo must report Sha256=true")
	}
}
