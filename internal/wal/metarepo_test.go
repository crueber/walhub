// metarepo_test.go — fork-deletion safety (Forgejo #451): deleting a
// fork parent with live children converts the prefix to a meta
// repository (packs + bookkeeping preserved, servable state swept);
// deleting a childless repo wipes the full prefix exactly as before.
package wal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// seedParentChild builds parent o/r (one pack + refs snapshot keys +
// access/policy sidecars) and child f/c (fork.json → o/r, own manifest),
// and lists o/r's meta/forks.json naming f/c. It returns nothing —
// callers probe the store.
func seedParentChild(t *testing.T, st store.ObjectStore, r *Registry, ctx context.Context) {
	t.Helper()
	for _, id := range []string{"o/r", "f/c"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "f/c", "o/r", "o/r")
	fx, _ := json.Marshal(&metaIndex{Version: 3, Forks: []metaIndexEntry{{Repo: "f/c"}}})
	if _, err := store.PutBytes(ctx, st, "repos/o/r/meta/forks.json", fx,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("forks.json: %v", err)
	}
	put := func(key, body string) {
		t.Helper()
		if _, err := store.PutBytes(ctx, st, key, []byte(body),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	// Shared pack set under the PARENT prefix (the fork references
	// these checksums verbatim — nothing copied).
	put("repos/o/r/wal/abc123.pack", "pack-bytes")
	put("repos/o/r/wal/abc123.idx", "idx-bytes")
	// Servable parent state (must go in the conversion).
	put("repos/o/r/refs.pb", "refs")
	put("repos/o/r/checkpoints/cp.pb", "cp")
	put("repos/o/r/log/000001.seg", "seg")
	put("repos/o/r/access.json", "{}")
	put("repos/o/r/policy.json", "{}")
}

// listKeys returns every key under prefix (test-only LIST).
func listKeys(t *testing.T, st store.ObjectStore, prefix string) []string {
	t.Helper()
	var out []string
	if err := st.List(context.Background(), prefix, "", func(m store.ObjectMeta) error {
		out = append(out, m.Key)
		return nil
	}); err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	return out
}

func hasKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// TestDelete451 is the acceptance table: the with-children conversion
// vs the byte-identical no-children wipe.
func TestDelete451(t *testing.T) {
	ctx := context.Background()

	t.Run("no-children/full-wipe", func(t *testing.T) {
		r, st := newTestRegistry(t)
		if _, err := r.Create(ctx, "solo/r", git.Sha1); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.PutBytes(ctx, st, "repos/solo/r/wal/p.pack", []byte("x"),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := r.Delete(ctx, "solo/r"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if keys := listKeys(t, st, "repos/solo/r/"); len(keys) != 0 {
			t.Fatalf("wipe left keys: %v", keys)
		}
		if _, err := r.Open(ctx, "solo/r"); err == nil {
			t.Fatal("open after delete succeeded")
		}
		// The name is free again (no tombstone on the childless path).
		if _, err := r.Create(ctx, "solo/r", git.Sha1); err != nil {
			t.Fatalf("re-create after childless delete: %v", err)
		}
	})

	t.Run("stale-index-row/full-wipe", func(t *testing.T) {
		r, st := newTestRegistry(t)
		if _, err := r.Create(ctx, "o/r", git.Sha1); err != nil {
			t.Fatalf("create: %v", err)
		}
		// Index names a child whose manifest is gone (deleted fork).
		fx, _ := json.Marshal(&metaIndex{Version: 1, Forks: []metaIndexEntry{{Repo: "f/ghost"}}})
		if _, err := store.PutBytes(ctx, st, "repos/o/r/meta/forks.json", fx,
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("index: %v", err)
		}
		if _, err := r.Delete(ctx, "o/r"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if keys := listKeys(t, st, "repos/o/r/"); len(keys) != 0 {
			t.Fatalf("stale-row delete left keys: %v", keys)
		}
	})

	t.Run("with-children/meta-conversion", func(t *testing.T) {
		r, st := newTestRegistry(t)
		seedParentChild(t, st, r, ctx)

		if _, err := r.Delete(ctx, "o/r"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		keys := listKeys(t, st, "repos/o/r/")
		// Preserved verbatim: the pack set, the child list, the
		// parent's own chain doc, and the new tombstone.
		for _, k := range []string{
			"repos/o/r/wal/abc123.pack",
			"repos/o/r/wal/abc123.idx",
			"repos/o/r/meta/forks.json",
			"repos/o/r/meta/tombstone.json",
		} {
			if !hasKey(keys, k) {
				t.Fatalf("conversion lost %s (left: %v)", k, keys)
			}
		}
		// Servable state is gone, manifest first.
		for _, k := range []string{
			"repos/o/r/manifest.pb",
			"repos/o/r/refs.pb",
			"repos/o/r/checkpoints/cp.pb",
			"repos/o/r/log/000001.seg",
			"repos/o/r/access.json",
			"repos/o/r/policy.json",
		} {
			if hasKey(keys, k) {
				t.Fatalf("conversion kept servable key %s", k)
			}
		}
		// The tombstone names the pinning children.
		raw, _, err := store.GetBytes(ctx, st, "repos/o/r/meta/tombstone.json", store.GetOptions{})
		if err != nil || raw == nil {
			t.Fatalf("tombstone read: %v", raw != nil)
		}
		var tomb tombstoneDoc
		if err := json.Unmarshal(raw, &tomb); err != nil {
			t.Fatalf("tombstone corrupt: %v", err)
		}
		if tomb.Reason != "fork-parent" || tomb.Repo != "o/r" || len(tomb.Children) != 1 || tomb.Children[0] != "f/c" {
			t.Fatalf("tombstone = %+v", tomb)
		}
		// No servable manifest: opens fail and listings skip — but the
		// name is NOT held: re-create absorbs the prefix (planner's
		// call, #451) with today's exact trip profile (no probe on
		// the create path — the push fast-path budget stays clean).
		if _, err := r.Open(ctx, "o/r"); err == nil {
			t.Fatal("open of meta repo succeeded")
		}
		if got := r.refreshList(ctx); containsID(got, "o/r") {
			t.Fatalf("meta repo listed: %v", got)
		}
		if _, err := r.Create(ctx, "o/r", git.Sha1); err != nil {
			t.Fatalf("re-create (absorb) of meta name: %v", err)
		}
		// The absorbed repo serves fresh while the child keeps
		// reading the preserved packs through the same prefix.
		h, err := r.Open(ctx, "o/r")
		if err != nil {
			t.Fatalf("open absorbed repo: %v", err)
		}
		_ = h
		hc, err := r.Open(ctx, "f/c")
		if err != nil {
			t.Fatalf("open child after absorb: %v", err)
		}
		if chain := hc.forkChain(ctx); len(chain) != 1 || chain[0] != "o/r" {
			t.Fatalf("child chain after absorb = %v", chain)
		}
		body, err := hc.sharedGet(ctx, "wal/abc123.pack")
		if err != nil || string(body) != "pack-bytes" {
			t.Fatalf("child read after absorb: %q %v", body, err)
		}
	})

	t.Run("corrupt-index/fails-closed", func(t *testing.T) {
		r, st := newTestRegistry(t)
		if _, err := r.Create(ctx, "o/r", git.Sha1); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.PutBytes(ctx, st, "repos/o/r/meta/forks.json", []byte("{nope"),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with corrupt index succeeded")
		}
		// The prefix is untouched — the manifest still serves.
		if ok, _ := store.Exists(ctx, st, "repos/o/r/manifest.pb"); !ok {
			t.Fatal("fail-closed delete removed the manifest")
		}
		if _, err := r.Open(ctx, "o/r"); err != nil {
			t.Fatalf("repo unusable after aborted delete: %v", err)
		}
	})
}

// TestDelete451Chain keeps multi-level chains working: deleting the
// root preserves packs for the grandchild through the dead middle, and
// deleting the middle converts it too — the leaf's fork.json pointers
// never change and every read resolves.
func TestDelete451Chain(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"a/b", "o/r", "f/c"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	putForkDoc(t, st, "o/r", "a/b", "a/b")
	putForkDoc(t, st, "f/c", "o/r", "a/b")
	fxRoot, _ := json.Marshal(&metaIndex{Version: 1, Forks: []metaIndexEntry{{Repo: "o/r"}}})
	fxMid, _ := json.Marshal(&metaIndex{Version: 1, Forks: []metaIndexEntry{{Repo: "f/c"}}})
	for key, body := range map[string][]byte{
		"repos/a/b/meta/forks.json":  fxRoot,
		"repos/o/r/meta/forks.json":  fxMid,
		"repos/a/b/wal/root.pack":    []byte("root-bytes"),
		"repos/o/r/wal/mid.pack":     []byte("mid-bytes"),
		"repos/o/r/checkpoints/c.pb": []byte("cp"),
	} {
		if _, err := store.PutBytes(ctx, st, key, body, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	// Delete the root: conversion (o/r is a live child of a/b).
	if _, err := r.Delete(ctx, "a/b"); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	h, err := r.Open(ctx, "f/c")
	if err != nil {
		t.Fatalf("open leaf: %v", err)
	}
	chain := h.forkChain(ctx)
	if len(chain) != 2 || chain[0] != "o/r" || chain[1] != "a/b" {
		t.Fatalf("leaf chain after root delete = %v", chain)
	}
	// Reads fall through the converted root to its preserved packs.
	body, err := h.sharedGet(ctx, "wal/root.pack")
	if err != nil || string(body) != "root-bytes" {
		t.Fatalf("leaf read through meta root: %q %v", body, err)
	}

	// Delete the middle: conversion too (f/c is live). The leaf's
	// fork.json is untouched and still resolves end-to-end.
	if _, err := r.Delete(ctx, "o/r"); err != nil {
		t.Fatalf("delete middle: %v", err)
	}
	raw, _, err := store.GetBytes(ctx, st, "repos/f/c/fork.json", store.GetOptions{})
	if err != nil || raw == nil || !strings.Contains(string(raw), `"parent":"o/r"`) {
		t.Fatalf("leaf fork.json rewritten by parent deletes: %s %v", raw, err)
	}
	h2, err := r.Open(ctx, "f/c")
	if err != nil {
		t.Fatalf("open leaf after middle delete: %v", err)
	}
	chain = h2.forkChain(ctx)
	if len(chain) != 2 || chain[0] != "o/r" || chain[1] != "a/b" {
		t.Fatalf("leaf chain after middle delete = %v", chain)
	}
	for key, want := range map[string]string{
		"wal/root.pack": "root-bytes",
		"wal/mid.pack":  "mid-bytes",
	} {
		got, err := h2.sharedGet(ctx, key)
		if err != nil || string(got) != want {
			t.Fatalf("leaf %s after both deletes: %q %v", key, got, err)
		}
	}

	// Tombstone GC: deleting the leaf frees it fully; deleting the
	// childless metas then wipes them INCLUDING their tombstones.
	// (Both metas are deleted before either re-create: a re-created
	// child manifest would read as live and pin the other meta.)
	if _, err := r.Delete(ctx, "f/c"); err != nil {
		t.Fatalf("delete leaf: %v", err)
	}
	if keys := listKeys(t, st, "repos/f/c/"); len(keys) != 0 {
		t.Fatalf("leaf wipe left: %v", keys)
	}
	for _, id := range []string{"o/r", "a/b"} {
		if _, err := r.Delete(ctx, id); err != nil {
			t.Fatalf("delete childless meta %s: %v", id, err)
		}
		if keys := listKeys(t, st, "repos/"+id+"/"); len(keys) != 0 {
			t.Fatalf("meta wipe %s left: %v", id, keys)
		}
	}
	// With the tombstones gone both names are free again.
	for _, id := range []string{"o/r", "a/b"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("re-create %s after tombstone GC: %v", id, err)
		}
	}
}

// TestDelete451ChildStaysServable proves the child's own lifecycle is
// untouched by the parent's conversion: sync-level reads, the summary
// inputs (fork.json + own index), and a child delete all behave.
func TestDelete451ChildStaysServable(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	seedParentChild(t, st, r, ctx)
	if _, err := r.Delete(ctx, "o/r"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	// Child opens and its ancestry resolves through the meta parent.
	h, err := r.Open(ctx, "f/c")
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	if chain := h.forkChain(ctx); len(chain) != 1 || chain[0] != "o/r" {
		t.Fatalf("child chain = %v", chain)
	}
	// The child's summary inputs are its own keys — intact.
	for _, k := range []string{"repos/f/c/fork.json", "repos/f/c/manifest.pb"} {
		if ok, _ := store.Exists(ctx, st, k); !ok {
			t.Fatalf("child key gone: %s", k)
		}
	}
	// Deleting the child is the plain full wipe (it has no children).
	if _, err := r.Delete(ctx, "f/c"); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if keys := listKeys(t, st, "repos/f/c/"); len(keys) != 0 {
		t.Fatalf("child wipe left: %v", keys)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// keyFailStore fails one operation on one key suffix (transport-outage
// shape for the fail-closed paths).
type keyFailStore struct {
	store.ObjectStore
	getSuffix  string
	getErr     error
	nilBody    bool // Get returns (nil, nil) — the absent-without-error shape
	headSuffix string
	headErr    error
	putSuffix  string
	putErr     error
	listErr    error
	delSuffix  string
	delErr     error
}

func (s *keyFailStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.getErr != nil && strings.HasSuffix(key, s.getSuffix) {
		return nil, s.getErr
	}
	if s.nilBody && strings.HasSuffix(key, s.getSuffix) {
		return nil, nil
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *keyFailStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if s.headErr != nil && strings.HasSuffix(key, s.headSuffix) {
		return nil, s.headErr
	}
	return s.ObjectStore.Head(ctx, key)
}

func (s *keyFailStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil && strings.HasSuffix(key, s.putSuffix) {
		return store.ObjectMeta{}, s.putErr
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func (s *keyFailStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if s.listErr != nil {
		return s.listErr
	}
	return s.ObjectStore.List(ctx, prefix, after, fn)
}

func (s *keyFailStore) Delete(ctx context.Context, key string, ver store.Version) error {
	if s.delErr != nil && strings.HasSuffix(key, s.delSuffix) {
		return s.delErr
	}
	return s.ObjectStore.Delete(ctx, key, ver)
}

func errTransport() error { return &store.StoreError{Kind: store.ErrKindOther, Key: "boom"} }

// TestDelete451FailClosed pins every doubt-keeps-packs branch: any
// probe/write doubt aborts the delete with the prefix servable.
func TestDelete451FailClosed(t *testing.T) {
	ctx := context.Background()
	newReg := func(t *testing.T, st store.ObjectStore) *Registry {
		t.Helper()
		r := NewRegistry(ctx, st, testConfig(t))
		t.Cleanup(r.Close)
		return r
	}
	seed := func(t *testing.T, st store.ObjectStore, r *Registry) {
		t.Helper()
		for _, id := range []string{"o/r", "f/c"} {
			if _, err := r.Create(ctx, id, git.Sha1); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
		}
		putForkDoc(t, st, "f/c", "o/r", "o/r")
		fx, _ := json.Marshal(&metaIndex{Version: 1, Forks: []metaIndexEntry{{Repo: "f/c"}}})
		if _, err := store.PutBytes(ctx, st, "repos/o/r/meta/forks.json", fx,
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("index: %v", err)
		}
		if _, err := store.PutBytes(ctx, st, "repos/o/r/wal/p.pack", []byte("x"),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("pack: %v", err)
		}
		if _, err := store.PutBytes(ctx, st, "repos/o/r/access.json", []byte("{}"),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("access: %v", err)
		}
	}
	manifestIntact := func(t *testing.T, st store.ObjectStore) {
		t.Helper()
		if ok, _ := store.Exists(ctx, st, "repos/o/r/manifest.pb"); !ok {
			t.Fatal("aborted delete removed the manifest")
		}
	}

	t.Run("index-GET-error", func(t *testing.T) {
		st := &keyFailStore{ObjectStore: store.NewMemory(),
			getSuffix: "meta/forks.json", getErr: errTransport()}
		r := newReg(t, st)
		seed(t, st, r)
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with unreadable index succeeded")
		}
		manifestIntact(t, st)
	})

	t.Run("child-probe-error", func(t *testing.T) {
		st := &keyFailStore{ObjectStore: store.NewMemory(),
			headSuffix: "f/c/manifest.pb", headErr: errTransport()}
		r := newReg(t, st)
		seed(t, st, r)
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with failing child probe succeeded")
		}
		manifestIntact(t, st)
	})

	t.Run("tombstone-write-error", func(t *testing.T) {
		st := &keyFailStore{ObjectStore: store.NewMemory(),
			putSuffix: "meta/tombstone.json", putErr: errTransport()}
		r := newReg(t, st)
		seed(t, st, r)
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with failing tombstone write succeeded")
		}
		manifestIntact(t, st)
	})

	t.Run("sweep-LIST-error", func(t *testing.T) {
		mem := store.NewMemory()
		r := newReg(t, mem)
		seed(t, mem, r)
		// Swap the store AFTER seeding (the seed needs a working LIST).
		r.st = &keyFailStore{ObjectStore: mem, listErr: errTransport()}
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with failing sweep LIST succeeded")
		}
		// The tombstone committed before the sweep failed; the
		// manifest delete already linearized — but no servable key
		// was swept and the packs survive.
		if ok, _ := store.Exists(ctx, mem, "repos/o/r/wal/p.pack"); !ok {
			t.Fatal("failed sweep lost packs")
		}
	})

	t.Run("sweep-DELETE-error", func(t *testing.T) {
		mem := store.NewMemory()
		r := newReg(t, mem)
		seed(t, mem, r)
		r.st = &keyFailStore{ObjectStore: mem,
			delSuffix: "manifest.pb", delErr: errTransport()}
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with failing manifest delete succeeded")
		}
	})

	t.Run("sweep-inner-DELETE-error", func(t *testing.T) {
		mem := store.NewMemory()
		r := newReg(t, mem)
		seed(t, mem, r)
		// The sweep must delete access.json (servable); failing that
		// exact delete aborts with the packs intact.
		r.st = &keyFailStore{ObjectStore: mem,
			delSuffix: "o/r/access.json", delErr: errTransport()}
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with failing sweep delete succeeded")
		}
		if ok, _ := store.Exists(ctx, mem, "repos/o/r/wal/p.pack"); !ok {
			t.Fatal("failed sweep lost packs")
		}
	})

	t.Run("nil-body-index", func(t *testing.T) {
		// GetBytes maps a nil GetResult to ErrKindOther (the store
		// contract never yields a nil body with empty options), so a
		// nil-body index surfaces as a transport doubt: fail closed.
		st := &keyFailStore{ObjectStore: store.NewMemory(),
			getSuffix: "meta/forks.json", nilBody: true}
		r := newReg(t, st)
		seed(t, st, r)
		if _, err := r.Delete(ctx, "o/r"); err == nil {
			t.Fatal("delete with unreadable index succeeded")
		}
		if ok, _ := store.Exists(ctx, st, "repos/o/r/manifest.pb"); !ok {
			t.Fatal("aborted delete removed the manifest")
		}
	})

	t.Run("empty-repo-row-skipped", func(t *testing.T) {
		r, st := newTestRegistry(t)
		if _, err := r.Create(ctx, "o/r", git.Sha1); err != nil {
			t.Fatalf("create: %v", err)
		}
		// An empty-repo row carries no child: skipped, and with no
		// other rows the delete is the full wipe.
		fx, _ := json.Marshal(&metaIndex{Version: 1, Forks: []metaIndexEntry{{Repo: ""}}})
		if _, err := store.PutBytes(ctx, st, "repos/o/r/meta/forks.json", fx,
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("index: %v", err)
		}
		if _, err := r.Delete(ctx, "o/r"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if keys := listKeys(t, st, "repos/o/r/"); len(keys) != 0 {
			t.Fatalf("empty-row delete left keys: %v", keys)
		}
	})

	t.Run("tombstone-nil-children", func(t *testing.T) {
		r, _ := newTestRegistry(t)
		if err := r.writeTombstone(ctx, "o/r", nil); err != nil {
			t.Fatalf("writeTombstone nil children: %v", err)
		}
		raw, _, err := store.GetBytes(ctx, r.st, tombstoneKey("o/r"), store.GetOptions{})
		if err != nil || raw == nil {
			t.Fatalf("tombstone read: %v", raw != nil)
		}
		var tomb tombstoneDoc
		if err := json.Unmarshal(raw, &tomb); err != nil {
			t.Fatalf("tombstone corrupt: %v", err)
		}
		if tomb.Children == nil || len(tomb.Children) != 0 {
			t.Fatalf("tombstone children = %+v, want empty non-nil", tomb)
		}
	})
}

// TestCreate451AbsorbsMeta pins the re-create rule (planner's call,
// #451): a meta name absorbs — the manifest lands fresh with today's
// exact trip profile (no probe on the create path), the absorbed repo
// serves, the live child keeps reading, and the stale tombstone waits
// for the next childless delete to GC it.
func TestCreate451AbsorbsMeta(t *testing.T) {
	ctx := context.Background()
	r, st := newTestRegistry(t)
	seedParentChild(t, st, r, ctx)
	if _, err := r.Delete(ctx, "o/r"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if _, err := r.Create(ctx, "o/r", git.Sha1); err != nil {
		t.Fatalf("re-create (absorb): %v", err)
	}
	// Absorbed repo serves fresh (Revision 1, empty).
	raw, _, err := store.GetBytes(ctx, st, "repos/o/r/manifest.pb", store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("absorbed manifest: %v", raw != nil)
	}
	m, err := proto.UnmarshalManifest(raw)
	if err != nil || m.Repo != "o/r" || m.Revision != 1 || m.HeadSeq != 0 {
		t.Fatalf("absorbed manifest = %+v %v", m, err)
	}
	// The stale tombstone is still there (informational only — the
	// next childless delete wipes it with the prefix).
	if ok, _ := store.Exists(ctx, st, tombstoneKey("o/r")); !ok {
		t.Fatal("tombstone should survive absorption as history")
	}
	// Exit the other way: delete the child, then the absorbed-then-
	// childless parent wipes fully (tombstone GC).
	if _, err := r.Delete(ctx, "f/c"); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if _, err := r.Delete(ctx, "o/r"); err != nil {
		t.Fatalf("delete absorbed parent: %v", err)
	}
	if keys := listKeys(t, st, "repos/o/r/"); len(keys) != 0 {
		t.Fatalf("absorbed-parent wipe left: %v", keys)
	}
}
