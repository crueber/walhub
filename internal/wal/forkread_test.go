package wal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func putForkDoc(t *testing.T, st store.ObjectStore, id, parent, root string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"parent": parent, "root": root, "forked_at": "2026-09-13T12:00:00Z", "version": 1})
	if _, err := store.PutBytes(context.Background(), st, "repos/"+id+"/fork.json", raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("fork.json %s: %v", id, err)
	}
}

func TestForkChain(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"a/b", "o/r", "f/c"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "o/r", "a/b", "a/b")
	putForkDoc(t, st, "f/c", "o/r", "a/b")

	h, err := r.Open(ctx, "f/c")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	chain := h.forkChain(ctx)
	if len(chain) != 2 || chain[0] != "o/r" || chain[1] != "a/b" {
		t.Fatalf("chain = %v", chain)
	}
	// Cached: a second call issues no store traffic (same slice back).
	if chain2 := h.forkChain(ctx); len(chain2) != 2 {
		t.Fatalf("cached chain = %v", chain2)
	}

	// Non-forks resolve empty.
	hp, _ := r.Open(ctx, "a/b")
	if c := hp.forkChain(ctx); len(c) != 0 {
		t.Fatalf("root chain = %v", c)
	}
}

func TestForkChainDeadMiddle(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"a/b", "o/r", "f/c", "g/h"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// g/h forked from f/c (root a/b); f/c's whole prefix is gone
	// (deleted intermediate) — the Root short-circuit still reaches a/b.
	putForkDoc(t, st, "g/h", "f/c", "a/b")
	putForkDoc(t, st, "o/r", "a/b", "a/b")
	h, _ := r.Open(ctx, "g/h")
	chain := h.forkChain(ctx)
	found := false
	for _, id := range chain {
		if id == "a/b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("root must survive a dead middle: %v", chain)
	}
}

func TestForkChainCycleTerminates(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"x/y", "y/z"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "x/y", "y/z", "y/z")
	putForkDoc(t, st, "y/z", "x/y", "x/y")
	h, _ := r.Open(ctx, "x/y")
	if c := h.forkChain(ctx); len(c) > maxForkDepth {
		t.Fatalf("cycle escaped the cap: %v", c)
	}
}

func TestSharedGetFallback(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"o/r", "f/c"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "f/c", "o/r", "o/r")
	if _, err := store.PutBytes(ctx, st, "repos/o/r/wal/p1.pack", []byte("shared-bytes"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, _ := r.Open(ctx, "f/c")

	// Ancestor hit.
	body, err := h.sharedGet(ctx, "wal/p1.pack")
	if err != nil || string(body) != "shared-bytes" {
		t.Fatalf("fallback: %q %v", body, err)
	}
	// Own prefix wins when present.
	if _, err := store.PutBytes(ctx, st, "repos/f/c/wal/p1.pack", []byte("own-bytes"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed own: %v", err)
	}
	// New handle (fresh chain cache) sees the own object first.
	h2, _ := r.Open(ctx, "f/c")
	body, err = h2.sharedGet(ctx, "wal/p1.pack")
	if err != nil || string(body) != "own-bytes" {
		t.Fatalf("own-first: %q %v", body, err)
	}
	// Total miss reports NotFound (the degraded/fsck contract).
	if _, err := h2.sharedGet(ctx, "wal/nope.pack"); !store.IsNotFound(err) {
		t.Fatalf("miss: %v", err)
	}
}

func TestDownloadSharedFallback(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"o/r", "f/c"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "f/c", "o/r", "o/r")
	payload := []byte("pack-content-12345")
	if _, err := store.PutBytes(ctx, st, "repos/o/r/wal/p1.pack", payload,
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, _ := r.Open(ctx, "f/c")
	dst := filepath.Join(t.TempDir(), "out.pack")
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := h.downloadShared(ctx, "wal/p1.pack", f, int64(len(payload))); err != nil {
		t.Fatalf("download: %v", err)
	}
	f.Close()
	got, _ := os.ReadFile(dst)
	if string(got) != string(payload) {
		t.Fatalf("bytes = %q", got)
	}
}

func TestDownloadSharedUnknownSize(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"o/r", "f/c"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "f/c", "o/r", "o/r")
	payload := []byte("sized-by-head")
	if _, err := store.PutBytes(ctx, st, "repos/o/r/wal/p2.pack", payload,
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, _ := r.Open(ctx, "f/c")
	dst := filepath.Join(t.TempDir(), "out.pack")
	mkfile := func() *os.File {
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// Unknown size HEAD-resolves across the prefix order (own miss →
	// ancestor hit) with zero manifest knowledge.
	f := mkfile()
	if err := h.downloadShared(ctx, "wal/p2.pack", f, 0); err != nil {
		t.Fatalf("download: %v", err)
	}
	f.Close()
	if got, _ := os.ReadFile(dst); string(got) != string(payload) {
		t.Fatalf("bytes = %q", got)
	}
	// Total miss with unknown size keeps today's vacuous empty success
	// (no HEAD answered anywhere — preserved verbatim for non-forks).
	f = mkfile()
	if err := h.downloadShared(ctx, "wal/gone.pack", f, 0); err != nil {
		t.Fatalf("vacuous: %v", err)
	}
	f.Close()
	if got, _ := os.ReadFile(dst); len(got) != 0 {
		t.Fatalf("vacuous bytes = %q", got)
	}
}

// TestForkChildRefsSync proves the executor-produced manifest shape
// (HeadSeq P, MinSeq P+1, empty segments, state in the checkpoint) opens
// and syncs at refs level on the REAL sync path — the shape contract
// between internal/pulls and the engine, pinned here so either side
// cannot drift.
func TestForkChildRefsSync(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.Create(ctx, "f/c", git.Sha1); err != nil {
		t.Fatal(err)
	}
	now := TsPtr(time.Now().UTC())
	snap := &proto.RefSnapshot{
		Seq:          7,
		ObjectFormat: "sha1",
		Refs:         []*proto.Ref{{Name: "refs/heads/main", Oid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		HeadTarget:   "refs/heads/main",
		CreatedAt:    now,
	}
	putPB := func(key string, body []byte) {
		t.Helper()
		if _, err := store.PutBytes(ctx, st, key, body,
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	putPB("repos/f/c/"+store.CheckpointRefsKey(7), snap.Marshal())
	cp := &proto.Checkpoint{Seq: 7, ObjectFormat: "sha1", Packs: nil,
		RefsKey: store.CheckpointRefsKey(7), RefCount: 1, CreatedAt: now, Writer: "test"}
	putPB("repos/f/c/"+store.CheckpointKey(7), cp.Marshal())
	cm := &proto.Manifest{
		FormatVersion: proto.WALFormatVersion,
		Repo:          "f/c",
		ObjectFormat:  "sha1",
		HeadSeq:       7,
		MinSeq:        8,
		Checkpoint: &proto.CheckpointRef{Seq: 7, Key: store.CheckpointKey(7),
			CreatedAt: now, FirstStateAt: now, AsOf: now},
		Revision:  1,
		Writer:    "test",
		UpdatedAt: now,
	}
	// Overwrite the empty birth manifest with the fork shape (the
	// executor Creates it; here the repo was born empty first).
	_, meta, err := store.GetBytes(ctx, st, "repos/f/c/manifest.pb", store.GetOptions{})
	if err != nil || meta.Version == "" {
		t.Fatalf("birth manifest: %v", err)
	}
	if _, err := store.PutBytes(ctx, st, "repos/f/c/manifest.pb", cm.Marshal(),
		store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatalf("swap manifest: %v", err)
	}
	// Reopen against the swapped manifest on a FRESH registry over the
	// same store (no handle/memory of the birth — the cold-open path a
	// fork child takes on first serve).
	r2 := NewRegistry(ctx, st, testConfig(t))
	defer r2.Close()
	h, err := r2.Open(ctx, "f/c")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	g, err := h.Sync(ctx, LevelRefs)
	if err != nil {
		t.Fatalf("refs sync: %v", err)
	}
	g.Release()
	live, err := h.Layer().Snapshot(h.Repo())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if live.HeadTarget != "refs/heads/main" || len(live.Refs) != 1 {
		t.Fatalf("refs = %+v", live)
	}
}
