package pulls

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// This file owns the §7 manifest-sharing step: the fork's manifest.pb
// references the parent's pack set verbatim plus a fresh refs snapshot,
// in already-on-bucket mode (no pack byte is copied or re-uploaded —
// dedup is a property of content addressing, not a step).
//
// ### Concurrency
//
// Hazard: two forks racing one target name (or a fork racing a repo
// create). Avoidance: the child manifest.pb Create arbitrates — one
// winner, every loser gets a 412 mapped to ErrConflict (the service's
// adopt check then decides ours-vs-theirs). A crashed attempt's orphan
// checkpoint pair (issue #458) is adopted-or-409'd by semantic compare,
// never overwritten: overwriting would corrupt the rival retry that
// adopts by the same rule. No lock of any kind is held: the step is pure
// store round trips (one manifest GET, N pack HEADs, two checkpoint PUTs,
// one manifest Create, plus exact-key GETs only on the 412 failure path),
// never across a lock, and the two checkpoint PUTs are independent keys
// issued sequentially (a crash between them leaves garbage objects, never
// a hazard — same rule as checkpoint round 1).

// ForkRef is one live parent ref for the fresh snapshot.
type ForkRef struct {
	Name   string
	Oid    string
	Peeled string
}

// ForkRefsReader supplies the parent's live refs (one refs-level sync at
// composition; the executor never touches the WAL directly, law 8).
type ForkRefsReader interface {
	// ParentRefs returns the parent's live refs plus its HEAD target.
	ParentRefs(ctx context.Context, parent string) (refs []ForkRef, headTarget string, err error)
}

// ShareExecutor is the production ForkExecutor: store + a refs reader,
// wired at composition (cmd/walhub) over the WAL registry.
type ShareExecutor struct {
	Store store.ObjectStore
	Refs  ForkRefsReader
	// Host is the writer identity recorded on the child objects.
	Host string
	Now  func() time.Time
}

var _ ForkExecutor = (*ShareExecutor)(nil)

func (e *ShareExecutor) nowUTC() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// ShareManifest implements ForkExecutor: copy the parent manifest
// referencing the shared pack set verbatim, write a fresh refs snapshot +
// checkpoint under the child prefix, then Create the child manifest.pb
// with min_seq = seq+1 (the fully-checkpointed shape — trimManifest's own
// precedent: an empty segment range with state in the checkpoint).
func (e *ShareExecutor) ShareManifest(ctx context.Context, parent, child string, opt ForkOptions) error {
	pOwner, pName, ok := splitRepo(parent)
	if !ok {
		return fmt.Errorf("%w: invalid parent %q", ErrInvalid, parent)
	}
	cOwner, cName, ok := splitRepo(child)
	if !ok {
		return fmt.Errorf("%w: invalid child %q", ErrInvalid, child)
	}
	if e.Store == nil {
		return fmt.Errorf("%w: fork store not wired", ErrUnavailable)
	}
	// 1. Consistent parent state (manifest + refs): a push racing the
	// fork must never tear the pair (refs newer than the pack set would
	// name objects the child manifest does not reference). Read manifest
	// → refs → manifest; proceed only when HeadSeq and Revision match on
	// both reads, retrying boundedly while the parent is hot.
	pm, live, headTarget, err := e.consistentParent(ctx, parent, pOwner, pName)
	if err != nil {
		return err
	}
	if pm.ObjectFormat != "sha1" && pm.ObjectFormat != "sha256" {
		return fmt.Errorf("%w: parent object format %q", ErrCorrupt, pm.ObjectFormat)
	}
	now := e.nowUTC()
	host := e.Host
	if host == "" {
		host = "walhub"
	}
	settings := childSettings(opt.Description, opt.Creator, parent)
	// 2. Empty parent (HeadSeq 0): the child is a fresh empty manifest —
	// the Registry.Create shape (MinSeq 0, no checkpoint, no packs). A
	// branch anchored on an unborn parent is rejected explicitly: there
	// are no refs to select.
	if pm.HeadSeq == 0 {
		if opt.Branch != "" {
			return fmt.Errorf("%w: parent %s has no refs", ErrUnprocessable, parent)
		}
		cm := &proto.Manifest{
			FormatVersion: proto.WALFormatVersion,
			Repo:          child,
			ObjectFormat:  pm.ObjectFormat,
			HeadSeq:       0,
			MinSeq:        0,
			Revision:      1,
			Writer:        host,
			UpdatedAt:     tsPtr(now),
			Settings:      settings,
		}
		if err := e.putCreatePB(ctx, manifestKey(cOwner, cName), cm.Marshal()); err != nil {
			// Same orphan discipline as the non-empty path (issue
			// #458): a present fork.json defers to the service's
			// adopt check; an empty-parent orphan with no fork.json
			// fails closed (byte-identical to a fresh repo, #432).
			if !errors.Is(err, ErrConflict) {
				return err
			}
			return e.adoptOrphanManifest(ctx, child, cOwner, cName, pm, nil)
		}
		return nil
	}
	if e.Refs == nil {
		return fmt.Errorf("%w: fork refs reader not wired", ErrUnavailable)
	}
	// 3. Fresh snapshot head: the requested branch (existence-checked
	// against the consistent refs) or the parent HEAD.
	head := headTarget
	if opt.Branch != "" {
		found := false
		for _, r := range live {
			if r.Name == opt.Branch {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: branch %q not in %s", ErrUnprocessable, opt.Branch, parent)
		}
		head = opt.Branch
	}
	// 4. Closure verification (already-on-bucket mode): every referenced
	// pack must exist — a missing pack fails the task loud, never a
	// half-born child.
	for _, p := range pm.Packs {
		if p == nil {
			continue
		}
		key := "repos/" + pOwner + "/" + pName + "/" + store.WalDir + p.Checksum + ".pack"
		meta, herr := e.Store.Head(ctx, key)
		if herr != nil {
			return herr
		}
		if meta == nil {
			return fmt.Errorf("%w: shared pack %s absent on %s", ErrUnprocessable, p.Checksum, parent)
		}
	}
	// 5. Fresh refs snapshot + checkpoint under the child prefix (same
	// key shapes the checkpoint writer uses, so every reader resolves
	// them without knowing this is a fork).
	prefs := make([]*proto.Ref, 0, len(live))
	for _, r := range live {
		prefs = append(prefs, &proto.Ref{Name: r.Name, Oid: r.Oid, Peeled: r.Peeled})
	}
	sortForkRefs(prefs)
	snap := &proto.RefSnapshot{
		Seq:          pm.HeadSeq,
		ObjectFormat: pm.ObjectFormat,
		Refs:         prefs,
		HeadTarget:   head,
		CreatedAt:    tsPtr(now),
	}
	if err := e.putCreateOrAdoptSnapshot(ctx, childKey(cOwner, cName, store.CheckpointRefsKey(pm.HeadSeq)), snap); err != nil {
		return err
	}
	packs := make([]*proto.PackRef, 0, len(pm.Packs))
	for _, p := range pm.Packs {
		if p == nil {
			continue
		}
		cp := *p
		packs = append(packs, &cp)
	}
	cp := &proto.Checkpoint{
		Seq:          pm.HeadSeq,
		ObjectFormat: pm.ObjectFormat,
		Packs:        packs,
		RefsKey:      store.CheckpointRefsKey(pm.HeadSeq),
		RefCount:     uint64(len(prefs)),
		CreatedAt:    tsPtr(now),
		Writer:       host,
	}
	if err := e.putCreateOrAdoptCheckpoint(ctx, childKey(cOwner, cName, store.CheckpointKey(pm.HeadSeq)), cp); err != nil {
		return err
	}
	// 6. Child manifest: packs verbatim, empty segment range
	// (min_seq = seq+1), state in the checkpoint, revision 1.
	cm := &proto.Manifest{
		FormatVersion: proto.WALFormatVersion,
		Repo:          child,
		ObjectFormat:  pm.ObjectFormat,
		HeadSeq:       pm.HeadSeq,
		MinSeq:        pm.HeadSeq + 1,
		Checkpoint: &proto.CheckpointRef{
			Seq:          pm.HeadSeq,
			Key:          store.CheckpointKey(pm.HeadSeq),
			CreatedAt:    tsPtr(now),
			FirstStateAt: tsPtr(now),
			AsOf:         tsPtr(now),
		},
		LogSegments: nil,
		Packs:       packs,
		UpdatedAt:   tsPtr(now),
		Writer:      host,
		Revision:    1,
		Settings:    settings,
	}
	if err := e.putCreatePB(ctx, manifestKey(cOwner, cName), cm.Marshal()); err != nil {
		// A 412 with no fork.json on the prefix may be a crashed
		// attempt's orphan manifest (issue #458: checkpoints were
		// adopted-or-created above, so they match this parent) — adopt
		// it when the evidence holds, else 409. A fork.json on the
		// prefix defers to the service's adopt check (ours-vs-theirs by
		// Parent), which already handles that case. Non-412 failures
		// propagate untouched (a store outage is never an adoption).
		if !errors.Is(err, ErrConflict) {
			return err
		}
		return e.adoptOrphanManifest(ctx, child, cOwner, cName, pm, packs)
	}
	return nil
}

// RollbackShare implements ForkExecutor (issue #432): release a share
// reservation won by this attempt when a later pre-commit step (access
// bootstrap, provenance write) fails. Without it the child manifest
// strands the target name — no data loss, but the fork pre-check 409s
// every retry until someone hand-deletes the prefix.
//
// Ownership proof (law 4: never delete live objects): the manifest Create
// arbitrates the name — this attempt's ShareManifest succeeded only by
// winning it — and Revision 1 proves no WAL publish has advanced the
// child since. Anything else refuses: an absent manifest is a nil no-op;
// a foreign (Repo mismatch), WAL-advanced (Revision != 1), or corrupt
// manifest is an error, never a delete. The bootstrapped access.json is
// deleted only when this attempt created it (accessCreated — the
// EnsureRepoAccessCreated result, issue #458): an adopted pre-existing
// access.json is someone else's object and survives the rollback, to be
// re-adopted by the retry.
//
// Exact keys only, no LIST: the bootstrapped access.json (when created by
// this attempt and no fork.json disputes the prefix — adopted, not
// created, so hands off), the checkpoint pair at the shared seq (HeadSeq
// 0 forks wrote no checkpoints), and the manifest last (its presence is
// what blocks retry — deleting it last keeps the taken-signal until the
// prefix is otherwise clean). Never packs (shared packs live under the
// PARENT prefix), never the parent index (the child was never listed —
// the fork-network GC walk in internal/maintain cannot reference it; a
// racing pass reads manifest-404 and skips the subtree).
//
// ### Concurrency
//
// Hazard: the rollback racing a repo-create (or a second fork) for the
// same name. Avoidance: the rollback only deletes a manifest that is
// provably this attempt's (Repo == child, Revision == 1 — verified by a
// fresh GET immediately before the deletes). A racing creator necessarily
// lost the manifest Create against this attempt (412) and backed off; any
// writer that advanced the manifest past Revision 1 aborts the rollback.
// A racing creator that arrives AFTER the deletes wins a clean, empty
// prefix — which is exactly the name-reuse the rollback exists for. The
// access.json delete rides the same proof (created-by-this-attempt); a
// concurrently re-created access.json after a delete is re-adopted by the
// retry's bootstrap, never a blocker (Create-wins/adopt).
func (e *ShareExecutor) RollbackShare(ctx context.Context, parent, child string, accessCreated bool) error {
	cOwner, cName, ok := splitRepo(child)
	if !ok {
		return fmt.Errorf("%w: invalid child %q", ErrInvalid, child)
	}
	if e.Store == nil {
		return fmt.Errorf("%w: fork store not wired", ErrUnavailable)
	}
	// A fork.json disputes the prefix (a stale foreign reservation, or a
	// concurrently committed fork): the share keys are still ours to
	// release, but the bootstrapped access.json may be adopted, not
	// created — leave it.
	disputed := false
	if praw, _, perr := store.GetBytes(ctx, e.Store, "repos/"+cOwner+"/"+cName+"/fork.json", store.GetOptions{}); perr != nil {
		if !store.IsNotFound(perr) {
			return perr
		}
	} else if praw != nil {
		disputed = true
	}
	raw, _, err := store.GetBytes(ctx, e.Store, manifestKey(cOwner, cName), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	if raw == nil {
		return nil
	}
	cm, uerr := proto.UnmarshalManifest(raw)
	if uerr != nil {
		return fmt.Errorf("%w: child manifest: %v", ErrCorrupt, uerr)
	}
	if cm.Repo != child || cm.Revision != 1 {
		return fmt.Errorf("%w: child manifest for %s is not this attempt's reservation", ErrConflict, child)
	}
	keys := []string{}
	// The bootstrapped access.json goes only when this attempt created
	// it (issue #458) and no fork.json disputes the prefix: an adopted
	// pre-existing access.json is hands-off either way.
	if accessCreated && !disputed {
		keys = append(keys, "repos/"+cOwner+"/"+cName+"/access.json")
	}
	if cm.HeadSeq != 0 {
		keys = append(keys,
			childKey(cOwner, cName, store.CheckpointRefsKey(cm.HeadSeq)),
			childKey(cOwner, cName, store.CheckpointKey(cm.HeadSeq)),
		)
	}
	keys = append(keys, manifestKey(cOwner, cName))
	for _, k := range keys {
		if derr := e.Store.Delete(ctx, k, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
	}
	return nil
}

// maxForkConsistentReads bounds the manifest/refs stability retry while
// the parent is hot (a push racing the share re-reads; continuous push
// floods fail loud and retryable, never spin).
const maxForkConsistentReads = 10

// consistentParent reads a stable (manifest, refs, head) triple: manifest
// → live refs → manifest, accepting only when HeadSeq and Revision match
// on both manifest reads. A moved parent retries (bounded); an unreadable
// parent fails with the first error.
func (e *ShareExecutor) consistentParent(ctx context.Context, parent, pOwner, pName string) (*proto.Manifest, []ForkRef, string, error) {
	readManifest := func() (*proto.Manifest, error) {
		praw, _, err := store.GetBytes(ctx, e.Store, manifestKey(pOwner, pName), store.GetOptions{})
		if err != nil || praw == nil {
			if err != nil && !store.IsNotFound(err) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: parent %s", ErrNotFound, parent)
		}
		pm, err := proto.UnmarshalManifest(praw)
		if err != nil {
			return nil, fmt.Errorf("%w: parent manifest: %v", ErrCorrupt, err)
		}
		return pm, nil
	}
	for attempt := 0; ; attempt++ {
		pm, err := readManifest()
		if err != nil {
			return nil, nil, "", err
		}
		if e.Refs == nil {
			return nil, nil, "", fmt.Errorf("%w: fork refs reader not wired", ErrUnavailable)
		}
		live, head, rerr := e.Refs.ParentRefs(ctx, parent)
		if rerr != nil {
			return nil, nil, "", rerr
		}
		again, err := readManifest()
		if err != nil {
			return nil, nil, "", err
		}
		if again.HeadSeq == pm.HeadSeq && again.Revision == pm.Revision {
			return pm, live, head, nil
		}
		if attempt+1 >= maxForkConsistentReads {
			return nil, nil, "", fmt.Errorf("%w: parent %s changed under the fork; retry the fork", ErrConflict, parent)
		}
	}
}

// childSettings builds the child manifest's inline settings from the fork
// description ("" = none, the host default). The summary description reads
// this exact TOML, so threading it at Create is atomic — no post-create
// write, no partial state. The TOML is constructed, never parsed from
// input: the escaper below makes it valid by construction.
func childSettings(description, creator, parent string) *proto.RepoSettings {
	if strings.TrimSpace(description) == "" {
		return nil
	}
	toml := "description = \"" + escapeTOMLBasic(description) + "\"\n"
	author := strings.ToLower(strings.TrimSpace(creator))
	return &proto.RepoSettings{
		Toml:      toml,
		Revision:  1,
		Author:    author,
		UpdatedAt: tsPtr(time.Now().UTC()),
		Message:   "fork of " + parent,
	}
}

// escapeTOMLBasic escapes a string for a TOML basic string (double-quoted,
// single-line): backslash and quote escaped; C0 controls other than \n
// and \t are rejected upstream (StartFork), so the remaining text cannot
// break the document.
func escapeTOMLBasic(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

// putCreateOrAdoptSnapshot Creates one refs snapshot; a 412 whose
// existing bytes are semantically identical to want (timestamps ignored —
// a retry stamps a new CreatedAt) adopts the crashed attempt's orphan and
// the retry converges. A 412 with different bytes is a rival attempt's
// reservation: ErrConflict, never overwritten.
func (e *ShareExecutor) putCreateOrAdoptSnapshot(ctx context.Context, key string, want *proto.RefSnapshot) error {
	return e.putCreateOrAdopt(ctx, key, want.Marshal(), func(raw []byte) bool {
		got := &proto.RefSnapshot{}
		if err := got.Unmarshal(raw); err != nil {
			return false
		}
		return sameForkSnapshot(got, want)
	})
}

// putCreateOrAdoptCheckpoint Creates one checkpoint object; same
// adopt-or-conflict rule as putCreateOrAdoptSnapshot.
func (e *ShareExecutor) putCreateOrAdoptCheckpoint(ctx context.Context, key string, want *proto.Checkpoint) error {
	return e.putCreateOrAdopt(ctx, key, want.Marshal(), func(raw []byte) bool {
		got := &proto.Checkpoint{}
		if err := got.Unmarshal(raw); err != nil {
			return false
		}
		return sameForkCheckpoint(got, want)
	})
}

// putCreateOrAdopt Creates one object; on a 412 the existing bytes decide:
// semantically identical (same) adopts, anything else (rival bytes,
// corrupt bytes, an unreadable key) is ErrConflict. Sweeping (delete +
// recreate) was rejected: without a manifest the occupying bytes are
// unowned garbage to us but a live reservation to a racing rival —
// deleting them would corrupt the rival's retry, which adopts by this
// same rule. Doubt keeps objects (law 4); the loser fails loud with 409.
func (e *ShareExecutor) putCreateOrAdopt(ctx context.Context, key string, body []byte, same func([]byte) bool) error {
	_, err := store.PutBytes(ctx, e.Store, key, body, store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
	if err == nil {
		return nil
	}
	if !store.IsPreconditionFailed(err) {
		return err
	}
	raw, _, gerr := store.GetBytes(ctx, e.Store, key, store.GetOptions{})
	if gerr != nil || raw == nil || !same(raw) {
		return fmt.Errorf("%w: %s already exists", ErrConflict, key)
	}
	return nil
}

// sameForkSnapshot reports whether two refs snapshots carry the same fork
// state (timestamps ignored — retries re-stamp).
func sameForkSnapshot(a, b *proto.RefSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Seq != b.Seq || a.ObjectFormat != b.ObjectFormat || a.HeadTarget != b.HeadTarget {
		return false
	}
	if len(a.Refs) != len(b.Refs) {
		return false
	}
	for i := range a.Refs {
		ar, br := a.Refs[i], b.Refs[i]
		if ar == nil || br == nil {
			if ar != br {
				return false
			}
			continue
		}
		if ar.Name != br.Name || ar.Oid != br.Oid || ar.Peeled != br.Peeled {
			return false
		}
	}
	return true
}

// sameForkCheckpoint reports whether two checkpoints carry the same fork
// state (timestamps and writer ignored — retries re-stamp).
func sameForkCheckpoint(a, b *proto.Checkpoint) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Seq != b.Seq || a.ObjectFormat != b.ObjectFormat || a.RefsKey != b.RefsKey || a.RefCount != b.RefCount {
		return false
	}
	return sameForkPacks(a.Packs, b.Packs)
}

// sameForkPacks compares pack sets by value (nil and empty are equal —
// both marshal to no packs).
func sameForkPacks(a, b []*proto.PackRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		pa, pb := a[i], b[i]
		if pa == nil || pb == nil {
			if pa != pb {
				return false
			}
			continue
		}
		if *pa != *pb {
			return false
		}
	}
	return true
}

// adoptOrphanManifest adopts a 412-occupying child manifest left by a
// crashed attempt (issue #458): fork.json is absent (a present fork.json
// defers to the service's ours-vs-theirs adopt check), the manifest is
// provably this parent's reservation (Repo == child, Revision == 1 — no
// WAL publish has advanced it — HeadSeq/MinSeq track the just-read
// parent, packs verbatim), and the checkpoint pair was just
// adopted-or-created above (so it matches this parent too).
//
// Anything else is ErrConflict, hands off: a foreign or WAL-advanced
// manifest may be live; an empty-parent orphan (HeadSeq 0) is
// byte-identical to a fresh repo manifest (#432), so it fails closed —
// adopting would risk hijacking a live repo's prefix, and deleting would
// risk wiping one. Repair there is the documented exact-key delete, and a
// parent that moved under the crash (orphan seq != current seq) likewise
// 409s with the residue named — replacing a moved-under orphan would need
// a delete this layer must not issue on doubt.
func (e *ShareExecutor) adoptOrphanManifest(ctx context.Context, child, cOwner, cName string, pm *proto.Manifest, packs []*proto.PackRef) error {
	conflict := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrConflict}, args...)...)
	}
	if pm.HeadSeq == 0 {
		return conflict("fork target %s already exists", child)
	}
	if praw, _, perr := store.GetBytes(ctx, e.Store, "repos/"+cOwner+"/"+cName+"/fork.json", store.GetOptions{}); perr != nil {
		if !store.IsNotFound(perr) {
			return conflict("fork target %s already exists", child)
		}
	} else if praw != nil {
		return conflict("fork target %s already exists", child)
	}
	raw, _, gerr := store.GetBytes(ctx, e.Store, manifestKey(cOwner, cName), store.GetOptions{})
	if gerr != nil || raw == nil {
		return conflict("fork target %s already exists", child)
	}
	cm, uerr := proto.UnmarshalManifest(raw)
	if uerr != nil {
		return conflict("fork target %s already exists", child)
	}
	if cm.Repo != child || cm.Revision != 1 || cm.HeadSeq != pm.HeadSeq || cm.MinSeq != pm.HeadSeq+1 {
		if cm.Repo == child && cm.Revision == 1 && cm.HeadSeq != pm.HeadSeq {
			return conflict("fork target %s holds a stale reservation from a crashed fork of an older parent state (seq %d, parent now %d); delete the child prefix keys to retry", child, cm.HeadSeq, pm.HeadSeq)
		}
		return conflict("fork target %s already exists", child)
	}
	if cm.ObjectFormat != pm.ObjectFormat || !sameForkPacks(cm.Packs, packs) {
		return conflict("fork target %s already exists", child)
	}
	if cm.Checkpoint == nil || cm.Checkpoint.Seq != pm.HeadSeq || cm.Checkpoint.Key != store.CheckpointKey(pm.HeadSeq) {
		return conflict("fork target %s already exists", child)
	}
	return nil
}

// putCreatePB Creates one object; a 412 maps to ErrConflict (the service
// adopt check decides ours-vs-theirs).
func (e *ShareExecutor) putCreatePB(ctx context.Context, key string, body []byte) error {
	_, err := store.PutBytes(ctx, e.Store, key, body, store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
	if err != nil {
		if store.IsPreconditionFailed(err) {
			return fmt.Errorf("%w: %s already exists", ErrConflict, key)
		}
		return err
	}
	return nil
}

// childKey joins a child owner/name with a repo-relative key.
func childKey(owner, name, rel string) string {
	return "repos/" + owner + "/" + name + "/" + rel
}

// splitRepo splits "owner/name" (lowercased owners already resolved
// upstream; this only checks shape).
func splitRepo(id string) (string, string, bool) {
	o, n, ok := strings.Cut(id, "/")
	if !ok || o == "" || n == "" || strings.Contains(n, "/") {
		return "", "", false
	}
	if _, err := git.ParseRepoId(o + "/" + n); err != nil {
		return "", "", false
	}
	return o, n, true
}

// sortForkRefs sorts snapshot refs by name (the checkpoint reader's
// contract: sorted, no duplicates — the parent's live set has none).
func sortForkRefs(refs []*proto.Ref) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j-1].Name > refs[j].Name; j-- {
			refs[j-1], refs[j] = refs[j], refs[j-1]
		}
	}
}

// tsPtr renders a protobuf timestamp.
func tsPtr(t time.Time) *proto.Timestamp {
	ts := proto.TimeFromGo(t.UTC())
	return &ts
}
