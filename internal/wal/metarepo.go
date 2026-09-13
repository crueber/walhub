// metarepo.go — fork-deletion safety (Forgejo #451): deleting a fork
// parent that still has live fork children converts the parent prefix
// into a storage-only META repository instead of wiping it.
//
// A meta repository is a prefix with no servable manifest: the wal/ pack
// set is preserved verbatim (children reference those checksums under
// the parent prefix — nothing is copied at fork time), the parent-side
// fork index (meta/forks.json) and the parent's own fork.json are kept
// so chain resolution and the GC walk keep working untouched, and a
// marker (meta/tombstone.json) records the conversion. All servable
// state — manifest, refs, checkpoints, log segments, issues, pulls,
// policy, access, settings, stats — is deleted.
//
// Re-create of a meta name ABSORBS the prefix (planner's call, issue
// #451): the manifest key is absent, so a plain PutCreate lands a fresh
// manifest with zero added round trips on the create path — the push
// fast-path budget (TestPushFastPathZeroCollabRoundTrips) forbids any
// tombstone probe there, and Open=NotFound + Create=412 cannot coexist
// on one key (presence is binary), so refusal is unimplementable without
// a probe. Absorption is safe: the children's packs, index rows, and
// fork.json pointers are all still valid, so the absorbed repo serves
// fresh while the old children keep reading; the stale tombstone is
// informational only (Delete never reads it — liveness re-derives from
// the index every time) and the next childless delete wipes it with the
// prefix. Known cost, stated plainly: the pre-delete generation's packs
// are unowned garbage until that wipe (GC only collects
// COMPACT-superseded checksums, never orphans).
//
// Consequences, all by construction:
//   - The parent id never changes, so children's fork.json Parent/Root
//     pointers stay textually correct: "re-pointing" is verification,
//     not a rewrite (no child write happens on parent delete).
//   - Manifest-gated discovery (registry refreshList, serve liveRepos,
//     Exists) treats a meta prefix as absent: meta repos are invisible
//     to listings, explore, and owner repo lists, and Open fails with
//     ErrNotFound — there is no servable manifest to open.
//   - Re-create absorbs: the manifest PutCreate lands on the absent key
//     with today's exact trip profile (no probe anywhere on the create
//     path — the push fast-path budget stays clean). The absorbed repo
//     serves fresh; live children keep reading through the preserved
//     prefix; the next delete re-derives liveness from the index.
//   - No GC pass ever runs on a meta prefix (maintain snapshots start
//     from a manifest, which a meta repo has none of), and per-repo GC
//     only lists the repo's OWN wal/ prefix — so preserved packs are
//     safe structurally, and the preserved index keeps the
//     fork-network walk working for any pass that reaches it.
//
// ### Concurrency
//
// Hazard: holding a lock across store calls (13 §2 rule 4). Avoidance:
// no lock of any kind is held here — pure exact-key store round trips
// (never LIST, law 4). Create-vs-delete races need no closing: both
// orders converge (create-then-convert sweeps around the doomed
// manifest exactly as today's delete-vs-create; convert-then-create
// absorbs), because no create path probes the tombstone.
package wal

import (
	"context"
	"encoding/json"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// Repo-relative keys of the meta-repo bookkeeping (bucket layout; the
// wal/ prefix and the two JSON docs below are the preserve set —
// everything else under the repo prefix is servable state and goes).
const (
	// TombstoneRel marks a converted parent (meta/tombstone.json).
	TombstoneRel = "meta/tombstone.json"
	// ForksIndexRel is the parent-side child list (meta/forks.json).
	ForksIndexRel = "meta/forks.json"
	// ForkDocRel is the fork-side provenance doc (fork.json).
	ForkDocRel = "fork.json"
	// WalRel is the shared pack prefix (wal/).
	WalRel = "wal/"
)

// tombstoneReasonForkParent is the only tombstone reason: the prefix
// was converted because live fork children still reference its packs.
const tombstoneReasonForkParent = "fork-parent"

// tombstoneDoc is the meta/tombstone.json body: what happened, when,
// and which live children pin the preserved packs.
type tombstoneDoc struct {
	Version  int      `json:"version"`
	Repo     string   `json:"repo"`
	Reason   string   `json:"reason"`
	Children []string `json:"children"`
	At       string   `json:"deleted_at"` // RFC 3339 UTC
}

// metaIndexEntry mirrors one row of the parent-side fork index
// (owned by internal/pulls — mirrored here so core never imports
// upward, law 8).
type metaIndexEntry struct {
	Repo string `json:"repo"`
}

// metaIndex mirrors the parent-side fork index body.
type metaIndex struct {
	Version int              `json:"version"`
	Forks   []metaIndexEntry `json:"forks"`
}

// tombstoneKey returns the bucket key of the repo's tombstone marker
// (informational only — no create path probes it, law 6).
func tombstoneKey(id string) string { return "repos/" + id + "/" + TombstoneRel }

// liveForkChildren returns the ids of fork children whose manifests
// still exist (exact-key probes, never LIST, law 4).
//
// Fail-closed: an absent index means no known children (nil, nil); a
// corrupt index is an error (the only map to the network is unreadable
// — wiping would orphan live children); a transport error on any probe
// is an error (doubt keeps packs). A child whose manifest is gone (404)
// is stale bookkeeping and pins nothing.
func (r *Registry) liveForkChildren(ctx context.Context, id string) ([]string, error) {
	raw, _, err := store.GetBytes(ctx, r.st, "repos/"+id+"/"+ForksIndexRel, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, &WalError{Kind: WalErrStore, Detail: "repos/" + id + "/" + ForksIndexRel, Wrapped: err}
	}
	// (No nil-body check: with empty GetOptions GetBytes returns a body
	// on nil error — absent surfaces as IsNotFound above.)
	var fx metaIndex
	if jerr := json.Unmarshal(raw, &fx); jerr != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: "repos/" + id + "/" + ForksIndexRel, Wrapped: jerr}
	}
	var live []string
	for _, f := range fx.Forks {
		if f.Repo == "" {
			continue
		}
		ok, herr := store.Exists(ctx, r.st, manifestKey(f.Repo))
		if herr != nil {
			return nil, &WalError{Kind: WalErrStore, Detail: manifestKey(f.Repo), Wrapped: herr}
		}
		if ok {
			live = append(live, f.Repo)
		}
	}
	return live, nil
}

// metaPreserved reports whether a repo-relative key survives the
// parent→meta conversion: the shared pack set, the child list, the
// parent's own chain doc (so forkread resolves untouched), and the
// tombstone marker itself.
func metaPreserved(rel string) bool {
	switch rel {
	case ForksIndexRel, ForkDocRel, TombstoneRel:
		return true
	}
	return len(rel) >= len(WalRel) && rel[:len(WalRel)] == WalRel
}

// writeTombstone records the conversion (blind overwrite — re-deleting
// a meta repo with live children refreshes the marker idempotently).
func (r *Registry) writeTombstone(ctx context.Context, id string, children []string) error {
	doc := tombstoneDoc{
		Version:  1,
		Repo:     id,
		Reason:   tombstoneReasonForkParent,
		Children: append([]string{}, children...),
		At:       time.Now().UTC().Format(time.RFC3339),
	}
	// (append onto the literal keeps Children non-nil even for a nil
	// input — the wire shape stays [] under every path.)
	raw, jerr := json.Marshal(doc)
	if jerr != nil {
		return &WalError{Kind: WalErrIo, Detail: tombstoneKey(id), Wrapped: jerr}
	}
	if _, perr := store.PutBytes(ctx, r.st, tombstoneKey(id), raw,
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); perr != nil {
		return &WalError{Kind: WalErrStore, Detail: tombstoneKey(id), Wrapped: perr}
	}
	return nil
}

// sweepPrefix deletes every key under prefix except metaPreserved ones
// (paged LIST + exact-key deletes — the delete path is not hot, law 6).
func (r *Registry) sweepPrefix(ctx context.Context, prefix string) error {
	var after string
	for {
		var keys []string
		if err := r.st.List(ctx, prefix, after, func(m store.ObjectMeta) error {
			keys = append(keys, m.Key)
			return nil
		}); err != nil {
			return &WalError{Kind: WalErrStore, Detail: prefix, Wrapped: err}
		}
		if len(keys) == 0 {
			break
		}
		for _, k := range keys {
			rel := k[len(prefix):]
			if metaPreserved(rel) {
				continue
			}
			if err := r.st.Delete(ctx, k, ""); err != nil && !store.IsNotFound(err) {
				return &WalError{Kind: WalErrStore, Detail: k, Wrapped: err}
			}
		}
		after = keys[len(keys)-1]
	}
	return nil
}
