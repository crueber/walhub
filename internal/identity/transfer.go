// transfer.go — repo transfer between owners (Forgejo #358).
//
// POST /{owner}/{repo}/api/transfer (both lanes) moves every object under
// repos/<srcOwner>/<srcRepo>/ to repos/<dstOwner>/<dstRepo>/, carrying
// access.json with the owner-subject rewrite below. v1 is direct and
// owner-initiated (no accept step): the caller must hold repo admin on the
// source (CheckRole admin — repo admin binding, source-org ownership, or
// host admin) and pass CheckCreateOwner admission on the destination
// (self, member org, or host admin). The service itself is principal-free
// (like DeleteOrg); the handler gates.
//
// ### Concurrency
//
// Hazard: two writers racing one transfer (a concurrent push landing new
// objects mid-move, or two transfers of the same repo). Avoidance: store
// CAS is the lock (13 §3) — every destination write is PutCreate, so a
// concurrent transfer 412s into an abort (409, best-effort cleanup of the
// keys this attempt created, source untouched). A push racing the move can
// land objects under the source prefix after the copy pass; the delete
// pass removes exactly the keys the copy pass saw, then re-lists the
// source prefix — leftovers mean a concurrent write, reported as 409 with
// the destination complete and named (the operator deletes the src residue,
// or deletes the destination and transfers back; nothing is ever deleted
// before its copy ACKs). No lock is
// held across any store call.
package identity

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// manifestName is the repo-existence signal (the same probe the invite
// paths gate on): a repo exists iff its manifest object exists.
const manifestName = "manifest.pb"

// TransferRepo moves the repo at srcOwner/srcRepo to dstOwner/dstRepo.
//
// Validation (400): both owner/repos must be bucket-legal ids (git
// charset, or an email owner for user namespaces), both names
// non-empty, source and destination must differ. Existence (404): the
// source manifest must exist. Collision (409): the destination manifest
// must be absent; a PutCreate loss mid-move (a concurrent transfer won
// the namespace) aborts with best-effort cleanup of this attempt's
// destination keys and the source untouched. The destination namespace
// itself needs no pre-registration: admission (CheckCreateOwner, applied
// by the handler) is the gate, and host admins may target unclaimed
// prefixes exactly like creation (#346 admin bypass) — a mistyped
// destination is recoverable (transfer back; nothing is deleted before
// its copy ACKs).
//
// The move is copy-then-delete over opaque bytes (law 4: never lose
// objects — no source key is deleted before its destination copy ACKs),
// streamed (Get body → Put Stream, never whole packs in memory).
// access.json is the one interpreted object: it moves with the
// owner-subject rewrite (rewriteTransferAccess); a corrupt access.json
// moves as opaque bytes (transfer never fails on it — reads already
// gate on it). A missing access.json materializes the destination's
// synthesized default for user-owned destinations (so the new owner has
// the same admin binding EnsureRepoAccess would have written); org-owned
// destinations synthesize on read and need no object. Repo invitations
// under meta/invitations/ move with the prefix, so pending invites
// survive the transfer.
//
// Cost (law 6): two manifest HEAD probes + one LIST + two ops per key —
// a human-rate admin path, never a git hot path.
func (s *Service) TransferRepo(ctx context.Context, srcOwner, srcRepo, dstOwner, dstRepo string) error {
	srcOwner = strings.TrimSpace(srcOwner)
	srcRepo = strings.TrimSpace(srcRepo)
	dstOwner = strings.TrimSpace(dstOwner)
	dstRepo = strings.TrimSpace(dstRepo)
	if !validRepoID(srcOwner, srcRepo) {
		return fmt.Errorf("%w: invalid source repo %q", ErrInvalid, srcOwner+"/"+srcRepo)
	}
	if !validRepoID(dstOwner, dstRepo) {
		return fmt.Errorf("%w: invalid destination repo %q", ErrInvalid, dstOwner+"/"+dstRepo)
	}
	if srcOwner == dstOwner && srcRepo == dstRepo {
		return fmt.Errorf("%w: source and destination are identical", ErrInvalid)
	}
	srcPrefix := "repos/" + srcOwner + "/" + srcRepo + "/"
	dstPrefix := "repos/" + dstOwner + "/" + dstRepo + "/"

	srcExists, err := store.Exists(ctx, s.Store, srcPrefix+manifestName)
	if err != nil {
		return err
	}
	if !srcExists {
		return fmt.Errorf("%w: unknown repo %q", ErrNotFound, srcOwner+"/"+srcRepo)
	}
	dstExists, err := store.Exists(ctx, s.Store, dstPrefix+manifestName)
	if err != nil {
		return err
	}
	if dstExists {
		return fmt.Errorf("%w: repo already exists at %q", ErrConflict, dstOwner+"/"+dstRepo)
	}

	var keys []string
	if err := s.Store.List(ctx, srcPrefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return err
	}

	now := s.nowUTC().Format(time.RFC3339)
	var copied []string
	cleanup := func() {
		for _, k := range copied {
			_ = s.Store.Delete(ctx, k, "")
		}
	}
	fail := func(err error) error {
		cleanup()
		return err
	}
	movedAccess := false
	for _, key := range keys {
		rel := strings.TrimPrefix(key, srcPrefix)
		dstKey := dstPrefix + rel
		if rel == "access.json" {
			raw, _, gerr := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
			if gerr != nil {
				return fail(gerr)
			}
			next := s.rewriteTransferAccess(ctx, raw, srcOwner, dstOwner, now)
			if next == nil {
				// Corrupt access.json: carry the bytes untouched.
				next = raw
			}
			if _, perr := store.PutBytes(ctx, s.Store, dstKey, next,
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); perr != nil {
				if store.IsPreconditionFailed(perr) {
					return fail(fmt.Errorf("%w: repo already exists at %q", ErrConflict, dstOwner+"/"+dstRepo))
				}
				return fail(perr)
			}
			copied = append(copied, dstKey)
			movedAccess = true
			continue
		}
		if cerr := s.copyObject(ctx, key, dstKey); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				return fail(fmt.Errorf("%w: repo already exists at %q", ErrConflict, dstOwner+"/"+dstRepo))
			}
			return fail(cerr)
		}
		copied = append(copied, dstKey)
	}
	if !movedAccess && s.isUserNamespace(ctx, dstOwner) {
		// No access.json at the source: materialize the destination's
		// synthesized default so a user new-owner holds the admin
		// binding creation would have written. Never for org
		// destinations (org-owner resolution covers them — a
		// user:<orgslug> binding would be a latent grant). 412 =
		// someone raced us (adopt, never overwrite).
		def := s.SynthesizeOwner(ctx, dstOwner)
		def.Version = 1
		if _, perr := store.PutBytes(ctx, s.Store, dstPrefix+"access.json", encodeAccess(def),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); perr != nil &&
			!store.IsPreconditionFailed(perr) {
			return fail(perr)
		}
	}

	for _, key := range keys {
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
	}
	// A push racing the move lands keys the copy pass never saw: the
	// delete pass removed exactly the copied set, so anything left is a
	// concurrent write — report it honestly (destination complete) instead
	// of silently stranding it.
	var leftover int
	if lerr := s.Store.List(ctx, srcPrefix, "", func(m store.ObjectMeta) error {
		leftover++
		return nil
	}); lerr != nil {
		return lerr
	}
	s.access.invalidate(srcOwner, srcRepo)
	s.access.invalidate(dstOwner, dstRepo)
	if leftover > 0 {
		return fmt.Errorf("%w: repo changed during transfer; %d object(s) remain at %q",
			ErrConflict, leftover, srcOwner+"/"+srcRepo)
	}
	return nil
}

// copyObject streams one object to a destination key that must not exist
// (PutCreate). The content type restores the writer convention readers
// never consult (sidecars like the release asset entries carry serving
// MIME; object metadata is write-only) — .json/.pb keep their types, all
// other bytes travel as octet-stream.
func (s *Service) copyObject(ctx context.Context, srcKey, dstKey string) error {
	res, err := s.Store.Get(ctx, srcKey, store.GetOptions{})
	if err != nil {
		return err
	}
	obj, ok := res.(store.Object)
	if !ok {
		return fmt.Errorf("identity: unexpected GetResult for %s", srcKey)
	}
	defer obj.Body.Close()
	_, err = s.Store.Put(ctx, dstKey,
		store.PutBody{Stream: obj.Body, StreamLen: obj.Meta.Size},
		store.PutOptions{Mode: store.PutCreate, ContentType: transferContentType(dstKey)})
	return err
}

// validRepoID reports whether owner/repo is a bucket-legal repo id: the
// git charset (ParseRepoId), or an email owner for user namespaces (@ is
// outside the git charset but bucket-legal — user repos live under
// repos/<email>/<repo>/) with a git-legal repo name.
func validRepoID(owner, repo string) bool {
	if owner == "" || repo == "" {
		return false
	}
	if _, err := git.ParseRepoId(owner + "/" + repo); err == nil {
		return true
	}
	if !ValidPrincipal(owner) {
		return false
	}
	_, err := git.ParseRepoId("owner/" + repo)
	return err == nil
}

// transferContentType restores the writer content-type convention for a
// moved key (see copyObject).
func transferContentType(key string) string {
	switch {
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	case strings.HasSuffix(key, ".pb"):
		return "application/x-protobuf"
	default:
		return "application/octet-stream"
	}
}

// rewriteTransferAccess moves an access.json across owners: visibility and
// every binding survive untouched, except the owner-subject —
// user:<srcOwner> bindings are dropped (the seller keeps no admin), and a
// user:<dstOwner> admin binding is added when the destination is a user
// namespace without one (org destinations need none: org-owner resolution
// covers them, and a user:<orgslug> binding would be a latent grant to
// whoever later claims that username). team: subjects name teams that
// still exist and move untouched. Returns nil when raw does not parse
// (the caller carries the bytes opaquely); the moved doc keeps its
// version (a move, not an edit) with a fresh timestamp.
func (s *Service) rewriteTransferAccess(ctx context.Context, raw []byte, srcOwner, dstOwner, now string) []byte {
	doc, err := parseAccess(raw)
	if err != nil {
		return nil
	}
	var kept []AccessBinding
	if ValidPrincipal(srcOwner) && !s.orgExists(ctx, srcOwner) {
		want := "user:" + normPrincipal(srcOwner)
		for _, b := range doc.RoleBindings {
			if strings.ToLower(b.Subject) == want {
				continue
			}
			kept = append(kept, b)
		}
	} else {
		kept = doc.RoleBindings
	}
	if s.isUserNamespace(ctx, dstOwner) {
		want := "user:" + normPrincipal(dstOwner)
		found := false
		for _, b := range kept {
			if strings.ToLower(b.Subject) == want {
				found = true
				break
			}
		}
		if !found {
			kept = append(kept, AccessBinding{Subject: want, Role: RoleAdmin})
		}
	}
	doc.RoleBindings = kept
	doc.UpdatedAt = now
	return encodeAccess(doc)
}
