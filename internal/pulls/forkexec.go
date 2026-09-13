package pulls

import (
	"context"
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
// adopt check then decides ours-vs-theirs). No lock of any kind is held:
// the step is pure store round trips (one manifest GET, N pack HEADs,
// two checkpoint PUTs, one manifest Create), never across a lock, and the
// two checkpoint PUTs are independent keys issued sequentially (a crash
// between them leaves garbage objects, never a hazard — same rule as
// checkpoint round 1).

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
			return err
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
	if err := e.putCreatePB(ctx, childKey(cOwner, cName, store.CheckpointRefsKey(pm.HeadSeq)), snap.Marshal()); err != nil {
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
	if err := e.putCreatePB(ctx, childKey(cOwner, cName, store.CheckpointKey(pm.HeadSeq)), cp.Marshal()); err != nil {
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
		return err
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
// manifest is an error, never a delete.
//
// Exact keys only, no LIST: the bootstrapped access.json (skipped when a
// fork.json disputes the prefix — adopted, not created, so hands off),
// the checkpoint pair at the shared seq (HeadSeq 0 forks wrote no
// checkpoints), and the manifest last (its presence is what blocks retry
// — deleting it last keeps the taken-signal until the prefix is otherwise
// clean). Never packs (shared packs live under the PARENT prefix), never
// the parent index (the child was never listed — the fork-network GC walk
// in internal/maintain cannot reference it; a racing pass reads
// manifest-404 and skips the subtree).
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
// prefix — which is exactly the name-reuse the rollback exists for.
func (e *ShareExecutor) RollbackShare(ctx context.Context, parent, child string) error {
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
	if !disputed {
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
