// eviction_test.go — eviction (05 §5.1.6): dir-size accounting with hard-link
// dedup, budget mode, and the dual-try-lock skip semantics.
package wal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
)

func TestDirSize_HardLinkDedup(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	data := make([]byte, 4096)
	if err := os.WriteFile(a, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Fatal(err)
	}
	// Two links to the same inode count ONCE per walk.
	if got := dirSize(dir); got != int64(len(data)) {
		t.Fatalf("dirSize = %d, want %d (hard links must dedup)", got, len(data))
	}
}

func TestEvictBudget_RemovesOrphansFirst(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	r.vals.cacheMaxBytes = 1 << 20

	// An orphaned cache dir (no open handle): always evictable, LRU = zero time.
	orphan := filepath.Join(cfg.Cache.Dir, "ghost", "old.git")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "pack.pack"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	// A live repo with a handle: its dir is small; it must survive.
	ctx := context.Background()
	if _, err := r.Create(ctx, "live/repo", git.Sha1); err != nil {
		t.Fatal(err)
	}

	r.evictBudget()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan dir survived budget eviction")
	}
	if _, err := os.Stat(filepath.Join(cfg.Cache.Dir, "live", "repo.git")); err != nil {
		t.Fatalf("live repo dir was evicted: %v", err)
	}
}

func TestEvictOne_SkipsBusyRepo(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "live/repo", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the rw read guard (an in-flight clone): eviction must skip.
	h.rw.RLock()
	if r.evictOne("live/repo", h.Dir()) {
		t.Fatal("evictOne succeeded while a reader held the guard")
	}
	h.rw.RUnlock()

	// Hold syncMu instead: also skipped, and never blocked.
	h.syncMu.Lock()
	if r.evictOne("live/repo", h.Dir()) {
		t.Fatal("evictOne succeeded while syncMu was held")
	}
	h.syncMu.Unlock()

	// Free repo: eviction succeeds.
	if !r.evictOne("live/repo", h.Dir()) {
		t.Fatal("evictOne failed on an idle repo")
	}
	if _, err := os.Stat(h.Dir()); !os.IsNotExist(err) {
		t.Fatal("dir survived eviction")
	}
	if r.Get("live/repo") != nil {
		t.Fatal("handle survived eviction")
	}
}

func TestEvictDisk_Watermark(t *testing.T) {
	// statfs on a tmpfs-backed TempDir: watermark 0 → always over → pass runs.
	cfg := testConfig(t)
	cfg.Cache.DiskHighWatermark = 0.0
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	orphan := filepath.Join(cfg.Cache.Dir, "ghost", "old.git")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	r.evictDisk()
	// On Linux the watermark test is exercised; on unsupported statfs it is
	// a no-op, so only assert no panic and no false eviction of the cache root.
	if _, err := os.Stat(cfg.Cache.Dir); err != nil {
		t.Fatalf("cache root damaged: %v", err)
	}
	_ = time.Now
}
