// eviction.go — registry eviction (doc 05 §5.1.6): budget and disk modes,
// LRU by repo-dir size, dual try-lock (syncMu then rw.TryWrite) — a busy repo
// is skipped this pass, never blocked.
package wal

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type repoUsage struct {
	id   string
	path string
	at   time.Time // last sync freshness touch (LRU proxy)
	size int64
}

// evictorLoop runs one pass per interval (13 §1 row 10: tickers; blocking
// `rw.Lock` is forbidden here — try-only).
func (r *Registry) evictorLoop(ctx context.Context) {
	defer r.wg.Done()
	interval := r.vals.evictIdleAfter / 4
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.evictIdle()
		}
	}
}

// evictIdle runs one pass (05 §5.1.6).
func (r *Registry) evictIdle() {
	switch r.vals.cacheMode {
	case "disk":
		r.evictDisk()
	case "auto":
		if r.overDiskWatermark() {
			r.evictDisk()
		}
		r.evictBudget()
	default: // "budget"
		r.evictBudget()
	}
}

// evictBudget: while the sum of repo sizes exceeds cache.max_bytes, evict
// least-recently-used repos idle longer than cache.evict_idle_after.
func (r *Registry) evictBudget() {
	if r.vals.cacheMaxBytes <= 0 {
		return
	}
	uses := r.repoUsages()
	var total int64
	for _, u := range uses {
		total += u.size
	}
	if total <= r.vals.cacheMaxBytes {
		return
	}
	// LRU order: oldest first.
	sort.Slice(uses, func(i, j int) bool { return uses[i].at.Before(uses[j].at) })
	idle := r.vals.evictIdleAfter
	for _, u := range uses {
		if total <= r.vals.cacheMaxBytes {
			return
		}
		if idle > 0 && time.Since(u.at) < idle {
			continue // not idle long enough
		}
		if r.evictOne(u.id, u.path) {
			total -= u.size
		}
	}
}

// evictDisk: statfs on cache.dir; evict (LRU) while above the high watermark,
// down to the target used = used − (used − (watermark−0.10))·total.
func (r *Registry) evictDisk() {
	high := r.vals.diskHighWatermark
	if high <= 0 {
		high = 0.9
	}
	if !r.overDiskWatermark() {
		return
	}
	uses := r.repoUsages()
	sort.Slice(uses, func(i, j int) bool { return uses[i].at.Before(uses[j].at) })
	for _, u := range uses {
		if !r.overDiskWatermark() {
			return
		}
		r.evictOne(u.id, u.path)
	}
}

func (r *Registry) overDiskWatermark() bool {
	st, err := statfs(r.vals.cacheDir)
	if err != nil {
		return false
	}
	used := float64(st.Blocks-st.Bfree) / float64(st.Blocks)
	return used > r.vals.diskHighWatermark
}

// repoUsages walks the open handles plus orphaned cache dirs, computing dir
// sizes: symlinks counted by link size (lstat); hard links counted once per
// (dev, ino) pair per walk.
func (r *Registry) repoUsages() []repoUsage {
	var out []repoUsage
	r.mu.Lock()
	for id, h := range r.repos {
		h.syncMu.Lock()
		at := h.freshAt
		h.syncMu.Unlock()
		out = append(out, repoUsage{id: id, path: h.repo.Path, at: at})
	}
	r.mu.Unlock()

	// Orphaned dirs (no handle) are always evictable.
	owners, err := os.ReadDir(r.vals.cacheDir)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, u := range out {
		seen[u.id] = true
	}
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(r.vals.cacheDir, o.Name()))
		if err != nil {
			continue
		}
		for _, n := range names {
			id := o.Name() + "/" // owner dir
			_ = id
			dir := filepath.Join(r.vals.cacheDir, o.Name(), n.Name())
			repoID := o.Name() + "/" + trimExt(n.Name())
			if seen[repoID] {
				continue
			}
			out = append(out, repoUsage{id: repoID, path: dir, at: time.Time{}})
		}
	}
	for i := range out {
		out[i].size = dirSize(out[i].path)
	}
	return out
}

// evictOne removes one repo: BOTH try-locks (syncMu first, then rw.TryWrite —
// the global nesting order); either failure → skip this pass (never evict
// under readers, never block the evictor). Locks are held only across
// os.RemoveAll — disk I/O, not network.
func (r *Registry) evictOne(id, path string) bool {
	r.mu.Lock()
	h := r.repos[id]
	r.mu.Unlock()
	if h != nil {
		if !h.syncMu.TryLock() {
			return false
		}
		defer h.syncMu.Unlock()
		if !h.rw.TryWriteLock() {
			return false
		}
		defer h.rw.WriteUnlock()
	}
	if err := os.RemoveAll(path); err != nil {
		logWarnf("evict %s: %v", id, err)
		return false
	}
	r.mu.Lock()
	if h := r.repos[id]; h != nil {
		delete(r.repos, id)
		go h.teardown() // publisher stop off the evictor path
	}
	r.mu.Unlock()
	logWarnf("evicted repo %s (%s)", id, path)
	return true
}

// dirSize walks a directory: symlink sizes via lstat; hard links deduped by
// (dev, ino) keyed dev<<32|ino per walk.
func dirSize(root string) int64 {
	var total int64
	seen := map[uint64]struct{}{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries don't fail the walk
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if li, err := os.Lstat(path); err == nil {
				total += li.Size()
			}
			return nil
		}
		if info.Mode().IsRegular() {
			// (dev, ino) dedup — hard links counted once per walk.
			key := hardlinkKey(info)
			if _, dup := seen[key]; dup {
				return nil
			}
			seen[key] = struct{}{}
			total += info.Size()
		}
		return nil
	})
	return total
}
