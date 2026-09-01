// registry_test.go — registry lifecycle + persisted-state behavior (05 §5.1).
package wal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

func store2() store.ObjectStore { return store.NewMemory() }

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0 // always check
	return cfg
}

func newTestRegistry(t *testing.T) (*Registry, store.ObjectStore) {
	t.Helper()
	st := store.NewMemory()
	r := NewRegistry(context.Background(), st, testConfig(t))
	t.Cleanup(r.Close)
	return r, st
}

func TestRegistry_CreateOpenDelete(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()

	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if h.ID != "acme/api" {
		t.Fatalf("id = %q", h.ID)
	}
	// Duplicate create → ErrAlreadyExists.
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err == nil {
		t.Fatal("duplicate create succeeded")
	}

	// Open returns the same handle (fast path + single-flight).
	h2, err := r.Open(ctx, "acme/api")
	if err != nil || h2 != h {
		t.Fatalf("open: %v (same=%v)", err, h2 == h)
	}

	// Manifest exists in the store.
	ok, err := store.Exists(ctx, st, "repos/acme/api/manifest.pb")
	if err != nil || !ok {
		t.Fatalf("manifest exists: %v %v", ok, err)
	}

	// Delete: manifest gone, local dir gone, opens fail.
	if _, err := r.Delete(ctx, "acme/api"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, _ := store.Exists(ctx, st, "repos/acme/api/manifest.pb"); ok {
		t.Fatal("manifest survived delete")
	}
	if _, err := os.Stat(filepath.Join(r.vals.cacheDir, "acme", "api.git")); !os.IsNotExist(err) {
		t.Fatal("local dir survived delete")
	}
	if _, err := r.Open(ctx, "acme/api"); err == nil {
		t.Fatal("open after delete succeeded")
	}
}

func TestRegistry_List(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"a/one", "a/two", "b/three"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	r.listing.mu.Lock() // force a refresh on next List
	r.listing.at = time.Time{}
	r.listing.mu.Unlock()
	got := r.List()
	want := map[string]bool{"a/one": true, "a/two": true, "b/three": true}
	if len(got) != len(want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected repo %q in %v", id, got)
		}
	}
}

func TestRegistry_StateCorruptionFallsBackToDefaults(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()

	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Corrupt the state file; the bucket is the truth so open must not fail.
	if err := os.WriteFile(filepath.Join(h.Dir(), "walgit-state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := NewRegistry(ctx, r.Store(), testConfig(t))
	defer r2.Close()
	h2, err := r2.Open(ctx, "acme/api")
	if err != nil {
		t.Fatalf("open with corrupt state: %v", err)
	}
	st := loadState(h2.Dir())
	if st.AppliedSeq != 0 || st.ManifestVersion != "" {
		t.Fatalf("state = %+v, want zero-value defaults", st)
	}
	if !st.PacksReady() {
		t.Fatal("zero state should count as packs_ready")
	}
}

func TestRegistry_OpenWithoutManifest(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.Open(context.Background(), "no/such"); err == nil {
		t.Fatal("open of unknown repo succeeded")
	}
}
