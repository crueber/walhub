package wal

import (
	"context"
	"encoding/json"
	"os"

	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the fork read fallback (docs/features/03 §7, issue
// #424): a fork's manifest references the parent's pack set verbatim —
// same checksums under the PARENT prefix, nothing copied — so pack
// content reads fall back across the fork ancestry when the child's own
// prefix misses. The fallback runs on the FAILURE path only: the own
// key is always tried first with byte-identical semantics for non-forks
// (no chain → the own result returns untouched, messages and all), and
// a hit costs zero extra round trips (law 6 — hot paths are unchanged
// when the object is where the manifest says it is).
//
// Ancestry comes from fork.json (Parent + Root at every level — the Root
// short-circuits dead middles: a deleted intermediate fork never strands
// a live grandchild while the root still holds the packs). Depth is
// capped (maxForkDepth) and revisited ids are skipped (fail-closed).
//
// ### Concurrency
//
// Hazard: chain resolution racing across goroutines (a cold-fork sync
// misses N packs at once). Avoidance: resolution is idempotent exact-key
// GETs — concurrent resolvers converge on the same chain and the last
// write wins under a leaf mutex. forkMu is a LEAF lock (taken alone,
// never with syncMu/packMu/rw, never held across a store call —
// resolution happens outside the lock, 13 §2 rule 4).

// maxForkDepth caps ancestry resolution (probes stay bounded; real
// networks are 1–3 deep).
const maxForkDepth = 8

// forkChain returns ancestor repo ids ("owner/name"), immediate parent
// outward. Empty for non-forks. Cached per handle: fork.json's parent
// pointer never moves (its CAS only touches merged_upstream_at), so a
// handle-lifetime cache is safe — and a cache is all it is (law 4: a
// wiped instance re-resolves from the bucket).
func (h *RepoHandle) forkChain(ctx context.Context) []string {
	h.forkMu.Lock()
	if h.forkLoaded {
		chain := h.forkChainCached
		h.forkMu.Unlock()
		return chain
	}
	h.forkMu.Unlock()
	chain := h.resolveForkChain(ctx)
	h.forkMu.Lock()
	h.forkChainCached, h.forkLoaded = chain, true
	h.forkMu.Unlock()
	return chain
}

// forkDoc is the fork.json shape fork reads need (owned by
// internal/pulls — mirrored here so core never imports upward, law 8).
type forkDoc struct {
	Parent string `json:"parent"`
	Root   string `json:"root"`
}

// readForkDoc reads one fork.json by exact key (never LIST, law 4).
// Absent/unreadable/corrupt → ok=false (the chain simply ends there — a
// deleted ancestor contributes nothing).
func (h *RepoHandle) readForkDoc(ctx context.Context, id string) (forkDoc, bool) {
	var doc forkDoc
	raw, _, err := store.GetBytes(ctx, h.reg.st, "repos/"+id+"/fork.json", store.GetOptions{})
	if err != nil || raw == nil {
		return doc, false
	}
	if jerr := json.Unmarshal(raw, &doc); jerr != nil || doc.Parent == "" {
		return forkDoc{}, false
	}
	return doc, true
}

// resolveForkChain walks Parent+Root pointers outward from the handle's
// own fork.json (BFS, visited set, depth cap).
func (h *RepoHandle) resolveForkChain(ctx context.Context) []string {
	own, ok := h.readForkDoc(ctx, h.ID)
	if !ok {
		return nil
	}
	var chain []string
	seen := map[string]bool{h.ID: true}
	queue := []string{}
	push := func(id string) {
		if id == "" || seen[id] || len(chain)+len(queue) >= maxForkDepth {
			return
		}
		seen[id] = true
		queue = append(queue, id)
	}
	push(own.Parent)
	push(own.Root)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		chain = append(chain, id)
		if doc, ok := h.readForkDoc(ctx, id); ok {
			push(doc.Parent)
			push(doc.Root)
		}
	}
	return chain
}

// ancestorKeys returns the fork-ancestor store keys for a repo-relative
// object key, immediate parent outward (empty for non-forks).
func (h *RepoHandle) ancestorKeys(ctx context.Context, rel string) []string {
	var out []string
	for _, a := range h.forkChain(ctx) {
		out = append(out, "repos/"+a+"/"+rel)
	}
	return out
}

// sharedGet GETs a small object with fork fallback: the own result
// returns verbatim (non-forks are byte-identical to a direct GET,
// messages included); on an own-NotFound each ancestor is tried in
// order — first hit wins, transport errors fail at once (never masked),
// a total miss reports the OWN miss verbatim.
func (h *RepoHandle) sharedGet(ctx context.Context, rel string) ([]byte, error) {
	own := h.repoKey(rel)
	body, _, err := store.GetBytes(ctx, h.reg.st, own, store.GetOptions{})
	if err == nil || !store.IsNotFound(err) {
		return body, err
	}
	for _, k := range h.ancestorKeys(ctx, rel) {
		abody, _, aerr := store.GetBytes(ctx, h.reg.st, k, store.GetOptions{})
		if aerr == nil {
			return abody, nil
		}
		if !store.IsNotFound(aerr) {
			return nil, aerr
		}
	}
	return nil, err
}

// downloadShared downloads a pack file with fork fallback. Known sizes
// ride the manifest (zero extra trips); unknown sizes HEAD in prefix
// order. The own attempt keeps today's exact semantics (including the
// vacuous empty download when no HEAD answers anywhere); ancestors run
// only after an own-NotFound. First success wins.
func (h *RepoHandle) downloadShared(ctx context.Context, rel string, f *os.File, size int64) error {
	own := h.repoKey(rel)
	if size > 0 {
		if derr := store.DownloadFileParallel(ctx, h.reg.st, own, f, size); derr == nil {
			return nil
		} else if !store.IsNotFound(derr) {
			return &WalError{Kind: WalErrStore, Detail: own, Wrapped: derr}
		}
		for _, k := range h.ancestorKeys(ctx, rel) {
			if derr := store.DownloadFileParallel(ctx, h.reg.st, k, f, size); derr != nil {
				if store.IsNotFound(derr) {
					continue
				}
				return &WalError{Kind: WalErrStore, Detail: k, Wrapped: derr}
			}
			return nil
		}
		return &WalError{Kind: WalErrStore, Detail: own, Wrapped: store.NewNotFound(own)}
	}
	keys := append([]string{own}, h.ancestorKeys(ctx, rel)...)
	onlyOwn := len(keys) == 1
	for _, k := range keys {
		meta, err := h.reg.st.Head(ctx, k)
		if err != nil {
			if !store.IsNotFound(err) {
				if onlyOwn {
					break // today's ignore-and-download-empty
				}
				return &WalError{Kind: WalErrStore, Detail: k, Wrapped: err}
			}
			continue
		}
		if meta == nil {
			continue
		}
		if derr := store.DownloadFileParallel(ctx, h.reg.st, k, f, meta.Size); derr != nil {
			// HEAD-hit then GET-miss = raced delete; keep walking.
			if store.IsNotFound(derr) {
				continue
			}
			return &WalError{Kind: WalErrStore, Detail: k, Wrapped: derr}
		}
		return nil
	}
	// Today's vacuous path (no HEAD answered anywhere): download nothing
	// from own, succeeding empty — preserved verbatim for non-forks.
	if derr := store.DownloadFileParallel(ctx, h.reg.st, own, f, 0); derr != nil {
		return &WalError{Kind: WalErrStore, Detail: own, Wrapped: derr}
	}
	return nil
}
