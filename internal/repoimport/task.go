// task.go — the import body (shared by the HTTP task and the CLI
// headless runner): scratch `git clone --mirror` → for-each-ref enumerate
// + S4 refmap → in-progress import.json CLAIM (PutCreate, before the
// manifest) → provisional manifest PutCreate (the CAS arbitrates
// ownership) → idempotent converge (presence-probed packs, ref creates
// for absent refs only, repack base, importer-admin) → version-checked
// import.json completion CAS → rollback to resumable-or-clean on any
// failure (fix #79: a failed import never wedges the target).
//
// ### Concurrency
// Hazard: the target may not exist yet, so the body holds NO repo locks
// (13 §2 rule 4 — there is no handle to lock); duplicate POSTs are
// deduped by the service single-flight, cross-instance races by the
// store CAS operations. Exactly-one-winner is preserved at three
// points: the claim PutCreate serializes importers (a lost CAS resolves
// to adopt/resume/takeover/409 — never a silent overwrite); the
// manifest PutCreate still arbitrates name-ownership against pushes,
// forks, and creates (unchanged); the completion PutUpdate elects
// exactly one completer (a lost CAS adopts a same-source completion).
// Avoidance: unique scratch per attempt (defer RemoveAll),
// clone-concurrency semaphore (import.max_concurrent, S9), every git
// spawn in Pool.Run with ctx timeouts, bulk pack uploads through
// AddPack (the bulk path, never request goroutines), no lock held across
// any store/network call, sender-owns-close channels (the stream ring),
// every goroutine exits via context (clone drain, heartbeat ticker).
package repoimport

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// runImport executes one import (ctx is the task/command ctx — drain or
// SIGINT cancels the clone mid-flight via CommandContext SIGKILL; the
// table runCtx is cancelled by reg.Tasks().Drain() at phase 1, which
// composition calls alongside Service.Drain).
func (s *Service) runImport(ctx context.Context, n *importNarr, id string, params Params, token string) error {
	ctx = n.Ctx()
	target := params.target()
	maxBytes := int64(s.cfg.Import.MaxBytes)

	// Clone-concurrency gate (S9): 2 server-side clones; a full gate fails
	// the task loudly instead of queueing silently (law 7).
	select {
	case s.clones <- struct{}{}:
		defer func() { <-s.clones }()
	case <-ctx.Done():
		return &StatusError{Status: 503, Message: "import interrupted before clone; safe to retry"}
	default:
		return &StatusError{Status: 503, Message: "server is at import.max_concurrent imports; retry shortly"}
	}

	scratch, err := s.git.ScratchDir(params.Owner, params.Name)
	if err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("scratch: %v", scrubError(err.Error()))}
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort sweep (04 §3.1)

	n.Notice(fmt.Sprintf("clone start %s -> %s", params.Source.URL, target))
	envName := ""
	if token != "" {
		envName = CredentialEnv(id) // S3: per-task child-env name, never argv
	}
	cloneErr := s.git.CloneMirror(ctx, params.Source.URL, scratch, params.Source.Scheme, params.Source.Host, envName, token,
		func(label string, done, total uint64, unit string) { n.Progress(label, done, total, unit) },
		func() { n.Notice("clone in progress") })
	if cloneErr != nil {
		return classifyCloneError(ctx, cloneErr, s.cfg.Import.CloneTimeout.String())
	}
	n.Notice("clone done")

	// S1: post-clone scratch du gate (git writes scratch directly — no
	// streaming enforcement is possible) + publish-time pack gate below.
	if size, derr := dirSize(scratch); derr != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("measure scratch: %v", scrubError(derr.Error()))}
	} else if size > maxBytes {
		return &StatusError{Status: 413, Message: fmt.Sprintf("source too large (%d bytes > import.max_bytes=%d); import from a nearby mirror or seed via import --direct", size, maxBytes)}
	}

	all, err := s.git.ForEachRef(ctx, scratch)
	if err != nil {
		return &StatusError{Status: 502, Message: fmt.Sprintf("enumerate refs: %v", scrubError(err.Error()))}
	}
	if len(all) > s.cfg.Import.MaxRefs && s.cfg.Import.MaxRefs > 0 {
		return &StatusError{Status: 422, Message: fmt.Sprintf("source has %d refs (import.max_refs=%d); narrow with refs[] or raise the key", len(all), s.cfg.Import.MaxRefs)}
	}
	headTarget := HeadTarget(scratch)
	refs := FilterRefs(all, params.IncludePullHeads, params.IncludeNotes, params.Refs, params.DefaultBranchOnly, headTarget)
	if len(refs) == 0 {
		return &StatusError{Status: 422, Message: "source has no importable refs (empty repository, or every ref was filtered)"}
	}
	n.Notice(fmt.Sprintf("enumerated %d refs (%d kept after filter)", len(all), len(refs)))

	formatName, err := s.git.ShowObjectFormat(ctx, scratch)
	if err != nil {
		return &StatusError{Status: 502, Message: fmt.Sprintf("detect object format: %v", scrubError(err.Error()))}
	}
	if params.Format != "" && params.Format != formatName {
		return &StatusError{Status: 422, Message: fmt.Sprintf("source is %s, requested %s; imports never convert object formats", formatName, params.Format)}
	}
	format := git.Sha1
	if formatName == "sha256" {
		format = git.Sha256
	}
	s.lfsNotice(ctx, n, scratch)

	// Claim point (fix #79): PutCreate the in-progress import.json
	// BEFORE the manifest CAS, so a manifest without a sidecar is
	// unambiguously foreign (B3 409 stands) while a same-source retry
	// over our claim resumes instead of wedging. A claim attempted
	// after drain begins must fail, never land: the service flag covers
	// Drain (checked first — it is set even if the table drain races
	// behind), and the body ctx covers every other cancel (table Drain
	// at phase 1, CLI SIGINT). Both refuse with a retryable 503 (law
	// 7), before any bucket write, so no post-drain claim can exist.
	if s.Draining() {
		return interruptedErr()
	}
	if err := ctx.Err(); err != nil {
		return &StatusError{Status: 503, Message: "import interrupted; safe to retry"}
	}
	claim := &ImportDoc{
		Version:        1,
		SourceURL:      params.Source.URL,
		SourceKind:     string(params.Source.Kind),
		RequestedRefs:  append([]string{}, params.Refs...),
		ImportedAt:     nowRFC3339(),
		HeadSHAs:       map[string]string{},
		Importer:       paramsImporter(params),
		Format:         formatName,
		ClaimExpiresAt: time.Now().UTC().Add(claimTTL(s.cfg)).Format(time.RFC3339),
	}
	mode, landed, claimVer, cerr := claimImportDoc(ctx, s.store, params.Owner, params.Name, claim)
	if cerr != nil {
		return cerr
	}
	if mode == claimAdopt {
		// The winner recorded the same source (B3 benign race → no-op
		// outcome, zero pack traffic — heads are NOT re-fetched).
		n.Notice(fmt.Sprintf("target %s already imported from this source; adopting", target))
		n.setResult(&Outcome{Repo: target, SourceURL: landed.SourceURL, HeadSHAs: landed.HeadSHAs, Format: landed.Format, ImportedAt: landed.ImportedAt})
		return nil
	}
	resume := mode == claimResume
	if resume {
		n.Notice(fmt.Sprintf("resuming import of %s from this source", target))
	}

	// Provisional commit: manifest.pb PutCreate still arbitrates
	// name-ownership (the CAS elects exactly one winner — same
	// arbitration as create, 13 §3, and forks, 03 §7), but the repo
	// only persists when the terminal import.json CAS lands: any later
	// failure rolls back to a resumable-or-clean state below. Re-check
	// drain here (a claim-to-manifest race with phase 1 must refuse,
	// never land a post-drain manifest).
	if s.Draining() {
		s.rollbackImport(ctx, target, params.Owner, params.Name, false, true, claimVer)
		return interruptedErr()
	}
	if err := ctx.Err(); err != nil {
		s.rollbackImport(ctx, target, params.Owner, params.Name, false, true, claimVer)
		return &StatusError{Status: 503, Message: "import interrupted; safe to retry"}
	}
	var h *wal.RepoHandle
	ownedManifest := false
	if resume {
		// Resume opens the wedged (or crashed-pre-commit) target; a
		// missing manifest means the first attempt died before its
		// commit — Create it now (owned, rolled back on failure).
		var oerr error
		h, oerr = s.reg.Open(ctx, target)
		if oerr != nil {
			if !isNotFound(oerr) {
				s.rollbackImport(ctx, target, params.Owner, params.Name, false, false, claimVer)
				return &StatusError{Status: 500, Message: fmt.Sprintf("open %s: %v (safe to retry)", target, scrubError(oerr.Error()))}
			}
			h, oerr = s.reg.Create(ctx, target, format)
			if oerr != nil {
				var we *wal.WalError
				if errors.As(oerr, &we) && we.Kind == wal.WalErrAlreadyExists {
					h, oerr = s.reg.Open(ctx, target)
				}
			} else {
				ownedManifest = true
			}
			if oerr != nil {
				s.rollbackImport(ctx, target, params.Owner, params.Name, ownedManifest, false, claimVer)
				return &StatusError{Status: 500, Message: fmt.Sprintf("create %s: %v (safe to retry)", target, scrubError(oerr.Error()))}
			}
		}
	} else {
		var herr error
		h, herr = s.reg.Create(ctx, target, format)
		if herr != nil {
			var we *wal.WalError
			if errors.As(herr, &we) && we.Kind == wal.WalErrAlreadyExists {
				// Our claim won but the name is taken: a push, fork,
				// or plain create landed first (a concurrent import
				// would have held the claim instead). The claim is
				// left in place deliberately: it may be shared with
				// a live winner (takeover race), and deleting under
				// them would flip their completion into a 409. The
				// retry resumes off it and converges (a divergent
				// foreign ref aborts loud in completeBody) — never
				// a silent wedge.
				return &StatusError{Status: 409, Message: fmt.Sprintf("target %s taken by another import or push; delete and retry, or pick another name", target)}
			}
			// The claim survives a 500 here (no manifest exists):
			// the retry resumes off it — never a wedge.
			return &StatusError{Status: 500, Message: fmt.Sprintf("create %s: %v (safe to retry)", target, scrubError(herr.Error()))}
		}
		ownedManifest = true
	}
	n.Notice(fmt.Sprintf("created %s (%s)", target, formatName))

	if err := s.completeBody(ctx, n, h, params, scratch, refs, headTarget, formatName, format, claimVer); err != nil {
		s.rollbackImport(ctx, target, params.Owner, params.Name, ownedManifest, mode == claimFresh, claimVer)
		return err
	}
	return nil
}

// completeBody converges the provisional repo to the imported state and
// CAS-completes the claim (fix #79): packs (skipping checksums already
// durable — the resume path), refs (converged against the serving
// copy — creates only for absent refs, never overwrites), the tier-2
// repack base, importer-admin, and the terminal import.json CAS. Every
// step is idempotent, so a retry over an in-progress claim always
// converges instead of wedging. Any error triggers rollbackImport at
// the caller (never inline — the ownership flags live in runImport).
func (s *Service) completeBody(ctx context.Context, n *importNarr, h *wal.RepoHandle, params Params, scratch string, refs []Ref, headTarget, formatName string, format git.ObjectFormat, claimVer store.Version) error {
	maxBytes := int64(s.cfg.Import.MaxBytes)
	target := params.target()
	importer := paramsImporter(params)
	meta := map[string]string{"agent": KindRepoImport, "principal": importer, "imported_from": params.Source.URL}

	// Packs: reuse the source's packs as tier-0 entries (trailer
	// checksum), then a full bitmap'd repack as the tier-2 base — the
	// classic runImport shape (§2 step 3a). Each pack goes through
	// publishPack (idx install + durable upload around AddPack).
	// Checksums already durable in the store are skipped (the resume
	// path — re-AddPacking would duplicate log entries); their .idx
	// is still ensured (a crash between AddPack and the idx upload
	// leaves a pack without one).
	packs, _ := filepath.Glob(filepath.Join(scratch, "objects", "pack", "*.pack"))
	sort.Strings(packs)
	imported := map[string]bool{}
	for _, pack := range packs {
		if st, serr := os.Stat(pack); serr == nil && st.Size() > maxBytes {
			return &StatusError{Status: 413, Message: fmt.Sprintf("pack %s exceeds import.max_bytes=%d", filepath.Base(pack), maxBytes)}
		}
		checksum, cerr := packTrailerChecksum(pack, formatName)
		if cerr != nil {
			return &StatusError{Status: 502, Message: fmt.Sprintf("pack %s: %v", filepath.Base(pack), scrubError(cerr.Error()))}
		}
		if imported[checksum] {
			continue
		}
		present, perr := s.packPresent(ctx, params, checksum)
		if perr != nil {
			return perr
		}
		if present {
			n.Progress("ingest", uint64(len(imported)+1), uint64(len(packs)), "packs")
			if err := s.ensureIdxUploaded(ctx, h, params, pack, checksum); err != nil {
				return err
			}
			imported[checksum] = true
			continue
		}
		n.Progress("ingest", uint64(len(imported)+1), uint64(len(packs)), "packs")
		if err := s.publishPack(ctx, n, h, params, pack, checksum, 0, meta); err != nil {
			return err
		}
		imported[checksum] = true
	}
	n.Notice(fmt.Sprintf("ingested %d packs", len(imported)))

	// Refs: converge through the WAL publish path (never hand-rolled
	// object writes); annotated tags carry peel; HEAD follows the source
	// (refs/heads/main fallback per 04 §1.2). Absent refs are created,
	// tips already at the wanted oid are skipped, and a ref pointing
	// anywhere else aborts loud (a foreign writer — never overwrite).
	curTips, curPeeled, curHead, rerr := s.currentRefs(ctx, h)
	if rerr != nil {
		return rerr
	}
	kept := map[string]bool{}
	txn := &proto.RefTransaction{}
	converged := 0
	for _, r := range refs {
		kept[r.Name] = true
		if cur, ok := curTips[r.Name]; ok {
			if cur == r.Oid && curPeeled[r.Name] == r.Peeled {
				converged++
				continue
			}
			return &StatusError{Status: 409, Message: fmt.Sprintf("target %s changed during import (ref %s points elsewhere); delete and retry, or pick another name", target, scrubError(r.Name))}
		}
		u := &proto.RefUpdate{Name: r.Name, OldOid: format.ZeroHex(), NewOid: r.Oid}
		if r.Peeled != "" {
			u.NewPeeled = r.Peeled
		}
		txn.Updates = append(txn.Updates, u)
	}
	head := ""
	if headTarget != "" && kept[headTarget] {
		head = headTarget
	} else if kept["refs/heads/main"] {
		head = "refs/heads/main"
	}
	if head != "" && head != curHead {
		txn.Updates = append(txn.Updates, &proto.RefUpdate{Name: "HEAD", NewSymbolicTarget: head})
	}
	if len(txn.Updates) > 0 {
		if _, err := h.Publish(ctx, wal.PublishRequest{Txn: txn, Meta: meta}); err != nil {
			return &StatusError{Status: 500, Message: fmt.Sprintf("publish refs: %v (safe to retry)", scrubError(err.Error()))}
		}
	}
	n.Notice(fmt.Sprintf("published %d refs (%d already converged)", len(refs), converged))

	// Tier-2 base: full bitmap'd repack over the ingested objects. The
	// repack writes its own idx in the serving copy; publishPack uploads
	// it (same idx discipline as tier-0).
	if diff, err := h.Layer().FullRepack(ctx, h.Repo(), nil); err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("full repack: %v", scrubError(err.Error()))}
	} else {
		for _, idxBase := range diff.New {
			checksum := strings.TrimSuffix(idxBase, ".idx")
			if imported[checksum] {
				continue
			}
			pack := filepath.Join(h.Repo().PackDir(), checksum+".pack")
			if err := s.publishPack(ctx, n, h, params, pack, checksum, 2, map[string]string{"history": "base"}); err != nil {
				return err
			}
			imported[checksum] = true
		}
	}
	n.Notice("repack base published")

	// Access: importer-admin explicitly via the identity service (S7),
	// then the bootstrap backstop. Policy: absent IS the default
	// (allow-all — api.loadPolicy: missing = nil doc), so import writes
	// no policy.json; the effective default holds by construction.
	if err := ensureImporterAdmin(ctx, s.roles, params.Owner, params.Name, importer); err != nil {
		return err
	}
	if !identity.ValidPrincipal(importer) {
		n.Notice(fmt.Sprintf("access.json: importer %q is not an email principal; defaults left to the bootstrap", importer))
	}
	importerBackstop(ctx, s.roles, params.Owner, params.Name)

	headSHAs := make(map[string]string, len(refs))
	for _, r := range refs {
		headSHAs[r.Name] = r.Oid
	}
	doc := &ImportDoc{
		Version:       1,
		SourceURL:     params.Source.URL,
		SourceKind:    string(params.Source.Kind),
		RequestedRefs: append([]string{}, params.Refs...),
		ImportedAt:    nowRFC3339(),
		HeadSHAs:      headSHAs,
		Importer:      importer,
		Format:        formatName,
		Complete:      true,
	}
	landed, cerr := completeImportDoc(ctx, s.store, params.Owner, params.Name, doc, claimVer)
	if cerr != nil {
		return cerr
	}
	n.Notice(fmt.Sprintf("imported %s: %d refs, %d packs", target, len(refs), len(imported)))
	n.setResult(&Outcome{Repo: target, SourceURL: landed.SourceURL, HeadSHAs: landed.HeadSHAs, Format: landed.Format, ImportedAt: landed.ImportedAt})
	return nil
}

// rollbackImport restores a resumable-or-clean state after a body
// failure (fix #79): a manifest this run created is deleted (the repo
// never persists half-imported); the owned claim follows iff the
// manifest delete landed. A surviving manifest keeps its claim, so the
// retry resumes; a surviving claim with no manifest resumes from
// Create. Resume runs never delete (they own neither), and foreign
// manifests are never touched (only self-created ones delete).
func (s *Service) rollbackImport(ctx context.Context, target, owner, repo string, ownedManifest, ownedClaim bool, claimVer store.Version) {
	if ownedManifest && s.reg != nil {
		if _, derr := s.reg.Delete(ctx, target); derr != nil {
			return
		}
	}
	if ownedClaim && s.store != nil {
		_ = deleteImportDoc(ctx, s.store, owner, repo, claimVer)
	}
}

// isNotFound reports store-not-found and wal-not-found alike.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if store.IsNotFound(err) {
		return true
	}
	var we *wal.WalError
	return errors.As(err, &we) && we.Kind == wal.WalErrNotFound
}

// packPresent probes wal/<checksum>.pack (probe, don't list — law 4;
// the resume path skips already-durable packs instead of re-AddPacking
// them, which would duplicate log entries).
func (s *Service) packPresent(ctx context.Context, params Params, checksum string) (bool, error) {
	meta, err := s.store.Head(ctx, store.RepoPrefix(params.Owner, params.Name)+store.PackKey(checksum))
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, &StatusError{Status: 500, Message: fmt.Sprintf("probe pack %s: %v (safe to retry)", checksum, scrubError(err.Error()))}
	}
	return meta != nil, nil
}

// idxPresent probes wal/<checksum>.idx (same probe discipline).
func (s *Service) idxPresent(ctx context.Context, params Params, checksum string) (bool, error) {
	meta, err := s.store.Head(ctx, store.RepoPrefix(params.Owner, params.Name)+store.IdxKey(checksum))
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, &StatusError{Status: 500, Message: fmt.Sprintf("probe idx %s: %v (safe to retry)", checksum, scrubError(err.Error()))}
	}
	return meta != nil, nil
}

// currentRefs reads the serving copy's ref tips (+ annotated-tag peel
// and HEAD symref) for the converge: the handle's catchUp at Open has
// already replayed the manifest's refs phase, so for-each-ref sees the
// durable state including any prior attempt's publishes.
func (s *Service) currentRefs(ctx context.Context, h *wal.RepoHandle) (tips, peeled map[string]string, head string, err error) {
	tips = map[string]string{}
	peeled = map[string]string{}
	all, ferr := s.git.ForEachRef(ctx, h.Dir())
	if ferr != nil {
		return nil, nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("read refs: %v (safe to retry)", scrubError(ferr.Error()))}
	}
	for _, r := range all {
		tips[r.Name] = r.Oid
		if r.Peeled != "" {
			peeled[r.Name] = r.Peeled
		}
	}
	return tips, peeled, HeadTarget(h.Dir()), nil
}

// ensureIdxUploaded installs the pack's .idx into the serving copy and
// uploads it iff absent (the resume path for packs whose AddPack landed
// but whose idx upload never did — the §6.1 gap).
func (s *Service) ensureIdxUploaded(ctx context.Context, h *wal.RepoHandle, params Params, packPath, checksum string) error {
	present, err := s.idxPresent(ctx, params, checksum)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := s.installIdx(ctx, h, packPath, checksum); err != nil {
		return err
	}
	return s.uploadIdx(ctx, params, h, checksum)
}

// publishPack publishes one pack through AddPack plus the idx discipline
// AddPack lacks (git.go EnsurePackIdx): the .idx is installed into the
// serving copy BEFORE AddPack (its internal LevelServe Sync needs it
// locally) and uploaded to wal/<checksum>.idx after (a fresh instance
// materializes from the store alone — "warmth", law 4). The .idx upload
// is create-if-absent; a 412 loser is success (uploadPack's rule).
func (s *Service) publishPack(ctx context.Context, n *importNarr, h *wal.RepoHandle, params Params, packPath, checksum string, tier uint32, meta map[string]string) error {
	if _, err := s.installIdx(ctx, h, packPath, checksum); err != nil {
		return err
	}
	if _, err := h.AddPack(ctx, packPath, checksum, tier, meta); err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("publish pack %s: %v (safe to retry; orphan packs are inert)", checksum, scrubError(err.Error()))}
	}
	return s.uploadIdx(ctx, params, h, checksum)
}

// installIdx installs the pack's .idx sibling into the serving copy
// (regenerating via git index-pack when the source lacks one) and
// returns the serving-copy path. Load-bearing twice: LevelServe Sync
// needs it locally, and a fresh instance materializes from the store
// alone ("warmth", law 4).
func (s *Service) installIdx(ctx context.Context, h *wal.RepoHandle, packPath, checksum string) (string, error) {
	idxSrc, err := s.git.EnsurePackIdx(ctx, packPath)
	if err != nil {
		return "", &StatusError{Status: 502, Message: fmt.Sprintf("pack %s: %v", filepath.Base(packPath), scrubError(err.Error()))}
	}
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	if idxSrc != servingIdx {
		raw, rerr := os.ReadFile(idxSrc)
		if rerr != nil {
			return "", &StatusError{Status: 500, Message: fmt.Sprintf("read idx: %v", scrubError(rerr.Error()))}
		}
		if werr := os.WriteFile(servingIdx, raw, 0o644); werr != nil {
			return "", &StatusError{Status: 500, Message: fmt.Sprintf("install idx: %v", scrubError(werr.Error()))}
		}
	}
	return servingIdx, nil
}

// uploadIdx uploads the serving-copy .idx create-if-absent (a 412 loser
// is success — uploadPack's rule: the bytes are content-addressed).
func (s *Service) uploadIdx(ctx context.Context, params Params, h *wal.RepoHandle, checksum string) error {
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	idxBytes, rerr := os.ReadFile(servingIdx)
	if rerr != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("read idx: %v", scrubError(rerr.Error()))}
	}
	_, uerr := store.PutBytes(ctx, s.store, store.RepoPrefix(params.Owner, params.Name)+store.IdxKey(checksum), idxBytes,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"})
	if uerr != nil && !store.IsPreconditionFailed(uerr) {
		return &StatusError{Status: 500, Message: fmt.Sprintf("upload idx %s: %v", checksum, scrubError(uerr.Error()))}
	}
	return nil
}

// paramsImporter resolves the importing principal (threaded through Begin
// — never from params, which are scrubbed and comparable for B2).
func paramsImporter(params Params) string { return params.importer }

// classifyCloneError maps clone failures onto task statuses (the HTTP 401
// is only for walhub auth — upstream auth failure is a task error).
func classifyCloneError(ctx context.Context, err error, cloneTimeout string) *StatusError {
	if ctx.Err() != nil {
		return &StatusError{Status: 503, Message: fmt.Sprintf("clone interrupted (import.clone_timeout=%s); safe to retry", cloneTimeout)}
	}
	tail := scrubError(err.Error())
	lower := strings.ToLower(tail)
	switch {
	case strings.Contains(lower, "authentication failed"),
		strings.Contains(lower, "could not read username"),
		strings.Contains(lower, "401"),
		strings.Contains(lower, "403"),
		strings.Contains(lower, "access denied"):
		return &StatusError{Status: 401, Message: "source authentication failed: check the token scope (GitHub: contents:read on the source repo)"}
	case strings.Contains(lower, "not found"),
		strings.Contains(lower, "404"),
		strings.Contains(lower, "does not exist"):
		return &StatusError{Status: 422, Message: fmt.Sprintf("source not reachable: %s", tail)}
	default:
		return &StatusError{Status: 502, Message: fmt.Sprintf("clone failed: %s", tail)}
	}
}

// lfsNotice implements S11: clones work, checkouts yield pointer text —
// say so on the task when the source tracks LFS.
func (s *Service) lfsNotice(ctx context.Context, n *importNarr, scratch string) {
	cctx, cancel := context.WithTimeout(ctx, s.git.GitTimeout)
	defer cancel()
	out, _, err := s.git.collect(cctx, scratch, []string{"show", "HEAD:.gitattributes"}, nil)
	if err != nil {
		return // unborn HEAD or no attributes file: nothing to say
	}
	if strings.Contains(out, "filter=lfs") {
		n.Notice("note: source tracks Git LFS — pointer blobs are imported as-is, never smudged; checkouts yield pointer text")
	}
}

// dirSize sums regular-file bytes under root (S1 du gate; no symlink
// following — a clone mirror has no symlinks outside its control).
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// packTrailerChecksum reads the pack trailer (20 bytes sha1, 32 sha256 —
// the cmd/walhub packTrailerChecksum shape, format-aware).
func packTrailerChecksum(path, format string) (string, error) {
	size := 20
	if format == "sha256" {
		size = 32
	}
	f, err := os.Open(path) //nolint:gosec // scratch + serving-copy paths only
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only trailer probe
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() < int64(size) {
		return "", fmt.Errorf("%s: not a pack file", filepath.Base(path))
	}
	if _, err := f.Seek(-int64(size), 2); err != nil { //nolint:mnd // trailer at EOF
		return "", err
	}
	trailer := make([]byte, size)
	if _, err := f.Read(trailer); err != nil { //nolint:gosec // fixed-size read
		return "", err
	}
	return hex.EncodeToString(trailer), nil
}
