package maintain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// This file owns the fork-network GC rule (docs/features/03 §7, issue
// #424): pack removal deletes a pack only when NO manifest in the fork
// network still references it. The parent's meta/forks.json lists direct
// children ([{repo, forked_at}]); the sweep consults the children's
// manifests' pack sets before deleting. Grand-children are discovered
// transitively through each child's own index, one level per pass —
// repairable, not blocking.
//
// ### Concurrency
//
// Hazard: holding maintenance locks across network calls. Avoidance: the
// expansion runs inside gcSuperseded, which holds only the TryLock-or-defer
// pack try-lock (never syncMu/packMu/rw — the 13 §2.1 protocol), and the
// probe count is bounded (maxForkNetworkProbes manifests per pass). No
// lock is acquired or held here at all — pure store round trips.

// maxForkNetworkProbes bounds one GC pass's network expansion: one
// exact-key GET per manifest/index read, never a LIST (law 4).
const maxForkNetworkProbes = 64

// forkIndexEntry mirrors one row of the parent-side fork index
// (meta/forks.json, owned by internal/pulls — mirrored here so maintain
// never imports upward, law 8).
type forkIndexEntry struct {
	Repo     string `json:"repo"`
	ForkedAt string `json:"forked_at"`
}

// forkIndex mirrors the parent-side fork index body.
type forkIndex struct {
	Version int              `json:"version"`
	Forks   []forkIndexEntry `json:"forks"`
}

// forkNetworkLive unions the pack checksums referenced by live
// fork-network manifests into live. The parent's own manifest is already
// in live (the caller built it); this adds every reachable child's set.
//
// Fail-closed where it matters: an unreadable or corrupt parent index
// aborts the sweep (the caller deletes nothing) — the index is the only
// map to the network, and guessing wrong deletes a live fork's packs. The
// same rule holds per child: a deleted fork (manifest 404) pins nothing
// and skips its subtree, but ANY other doubt — a transport error, a
// corrupt manifest, an unreadable child index, or a probe cap hit with
// unvisited children remaining — aborts the sweep (the caller deletes
// nothing; the next pass retries). Deleted packs are unrecoverable while
// a deferred sweep is merely retried, so doubt always keeps packs.
func (m *Maintainer) forkNetworkLive(ctx context.Context, rep Repo, live map[string]bool) error {
	st := m.store()
	if st == nil {
		return nil
	}
	owner, name, ok := splitRepoPrefix(rep.Prefix())
	if !ok {
		return nil
	}
	index, err := readForkIndex(ctx, st, owner, name)
	if err != nil || index == nil {
		if err != nil {
			return err
		}
		return nil // no index = no children
	}
	probes := 0
	visited := map[string]bool{owner + "/" + name: true}
	queue := make([]string, 0, len(index.Forks))
	for _, f := range index.Forks {
		queue = append(queue, f.Repo)
	}
	for len(queue) > 0 && probes < maxForkNetworkProbes {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		co, cn, ok := splitRepoID(id)
		if !ok {
			continue
		}
		probes++
		raw, _, gerr := store.GetBytes(ctx, st, "repos/"+co+"/"+cn+"/manifest.pb", store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				m.logf("%s: fork-network child %s deleted, skipping subtree", rep.ID(), id)
				continue
			}
			return fmt.Errorf("fork-network child %s manifest: %w", id, gerr)
		}
		if raw == nil {
			m.logf("%s: fork-network child %s manifest empty, skipping subtree", rep.ID(), id)
			continue
		}
		cm, uerr := proto.UnmarshalManifest(raw)
		if uerr != nil {
			return fmt.Errorf("fork-network child %s manifest corrupt: %w", id, uerr)
		}
		for _, p := range cm.Packs {
			if p != nil {
				live[p.Checksum] = true
			}
		}
		// Grand-children, one level per pass: the child's own index
		// extends the queue (bounded by the probe cap above). An
		// unreadable child index aborts the sweep (fail closed — its
		// grandchildren are unknown); an absent index is a leaf fork.
		probes++
		if probes >= maxForkNetworkProbes {
			break
		}
		cindex, cerr := readForkIndex(ctx, st, co, cn)
		if cerr != nil {
			return fmt.Errorf("fork-network child %s index: %w", id, cerr)
		}
		if cindex == nil {
			continue
		}
		for _, f := range cindex.Forks {
			if !visited[f.Repo] {
				queue = append(queue, f.Repo)
			}
		}
	}
	// Probe-cap exhaustion with unvisited children remaining aborts the
	// sweep (fail closed): the unvisited subtrees' pack sets are unknown,
	// so proceeding would delete packs a live fork still references. The
	// next pass retries; queue rows already visited are harmless.
	for _, id := range queue {
		if !visited[id] {
			return fmt.Errorf("fork network exceeds probe cap %d; deferring sweep", maxForkNetworkProbes)
		}
	}
	return nil
}

// readForkIndex reads one meta/forks.json by exact key (never LIST).
// Absent → (nil, nil). Corrupt → error (fail closed at the parent; the
// child path treats it as skip via the caller's continue).
func readForkIndex(ctx context.Context, st store.ObjectStore, owner, name string) (*forkIndex, error) {
	raw, _, err := store.GetBytes(ctx, st, "repos/"+owner+"/"+name+"/meta/forks.json", store.GetOptions{})
	if err != nil || raw == nil {
		if err != nil && !store.IsNotFound(err) {
			return nil, err
		}
		return nil, nil
	}
	var fx forkIndex
	if err := json.Unmarshal(raw, &fx); err != nil {
		return nil, err
	}
	if fx.Forks == nil {
		fx.Forks = []forkIndexEntry{}
	}
	return &fx, nil
}

// splitRepoPrefix parses "repos/<owner>/<repo>/" into owner/repo.
func splitRepoPrefix(prefix string) (string, string, bool) {
	rest, ok := strings.CutPrefix(prefix, "repos/")
	if !ok {
		return "", "", false
	}
	rest = strings.TrimSuffix(rest, "/")
	return splitRepoID(rest)
}

// splitRepoID parses "owner/name" (exactly two non-empty segments).
func splitRepoID(id string) (string, string, bool) {
	o, n, ok := strings.Cut(id, "/")
	if !ok || o == "" || n == "" || strings.Contains(n, "/") {
		return "", "", false
	}
	return o, n, true
}
