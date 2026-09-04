// task.go — the import body (shared by the HTTP task and the CLI
// headless runner): scratch `git clone --mirror` → for-each-ref enumerate
// + S4 refmap → ingest via the existing publish path (source packs as
// tier-0, full bitmap'd repack as the tier-2 base — the classic runImport
// shape) → manifest PutCreate commit point → importer-admin via the
// identity service → import.json provenance Create.
//
// ### Concurrency
// Hazard: the target may not exist yet, so the body holds NO repo locks
// (13 §2 rule 4 — there is no handle to lock); duplicate POSTs are
// deduped by the service single-flight, cross-instance races by the
// manifest Create CAS. Avoidance: unique scratch per attempt (defer
// RemoveAll), clone-concurrency semaphore (import.max_concurrent, S9),
// every git spawn in Pool.Run with ctx timeouts, bulk pack uploads through
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

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// runImport executes one import (ctx is the task/command ctx — drain or
// SIGINT cancels the clone mid-flight via CommandContext SIGKILL).
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

	// Commit point: manifest.pb PutCreate (the CAS decides ownership —
	// same arbitration as create, 13 §3, and forks, 03 §7).
	h, err := s.reg.Create(ctx, target, format)
	if err != nil {
		var we *wal.WalError
		if errors.As(err, &we) && we.Kind == wal.WalErrAlreadyExists {
			// Lost the Create race: adopt iff the winner recorded the
			// same source (B3 benign race → no-op outcome), else 409.
			if doc, _, rerr := readImportDoc(ctx, s.store, params.Owner, params.Name); rerr == nil && doc != nil && doc.SourceURL == params.Source.URL {
				n.Notice(fmt.Sprintf("target %s already imported from this source; adopting", target))
				n.setResult(&Outcome{Repo: target, SourceURL: doc.SourceURL, HeadSHAs: doc.HeadSHAs, Format: doc.Format, ImportedAt: doc.ImportedAt})
				return nil
			}
			return &StatusError{Status: 409, Message: fmt.Sprintf("target %s taken by another import or push; delete and retry, or pick another name", target)}
		}
		return &StatusError{Status: 500, Message: fmt.Sprintf("create %s: %v", target, scrubError(err.Error()))}
	}
	n.Notice(fmt.Sprintf("created %s (%s)", target, formatName))

	importer := paramsImporter(params)
	meta := map[string]string{"agent": KindRepoImport, "principal": importer, "imported_from": params.Source.URL}

	// Packs: reuse the source's packs as tier-0 entries (trailer
	// checksum), then a full bitmap'd repack as the tier-2 base — the
	// classic runImport shape (§2 step 3a). Each pack goes through
	// publishPack (idx install + durable upload around AddPack).
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
		n.Progress("ingest", uint64(len(imported)+1), uint64(len(packs)), "packs")
		if err := s.publishPack(ctx, n, h, params, pack, checksum, 0, meta); err != nil {
			return err
		}
		imported[checksum] = true
	}
	n.Notice(fmt.Sprintf("ingested %d packs", len(imported)))

	// Refs: creates through the WAL publish path (never hand-rolled
	// object writes); annotated tags carry peel; HEAD follows the source
	// (refs/heads/main fallback per 04 §1.2).
	kept := map[string]bool{}
	txn := &proto.RefTransaction{}
	for _, r := range refs {
		kept[r.Name] = true
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
	if head != "" {
		txn.Updates = append(txn.Updates, &proto.RefUpdate{Name: "HEAD", NewSymbolicTarget: head})
	}
	if _, err := h.Publish(ctx, wal.PublishRequest{Txn: txn, Meta: meta}); err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("publish refs: %v (safe to retry after delete)", scrubError(err.Error()))}
	}
	n.Notice(fmt.Sprintf("published %d refs", len(refs)))

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
	}
	if err := writeImportDoc(ctx, s.store, params.Owner, params.Name, doc); err != nil {
		return err
	}
	n.Notice(fmt.Sprintf("imported %s: %d refs, %d packs", target, len(refs), len(imported)))
	n.setResult(&Outcome{Repo: target, SourceURL: doc.SourceURL, HeadSHAs: headSHAs, Format: formatName, ImportedAt: doc.ImportedAt})
	return nil
}

// publishPack publishes one pack through AddPack plus the idx discipline
// AddPack lacks (git.go EnsurePackIdx): the .idx is installed into the
// serving copy BEFORE AddPack (its internal LevelServe Sync needs it
// locally) and uploaded to wal/<checksum>.idx after (a fresh instance
// materializes from the store alone — "warmth", law 4). The .idx upload
// is create-if-absent; a 412 loser is success (uploadPack's rule).
func (s *Service) publishPack(ctx context.Context, n *importNarr, h *wal.RepoHandle, params Params, packPath, checksum string, tier uint32, meta map[string]string) error {
	idxSrc, err := s.git.EnsurePackIdx(ctx, packPath)
	if err != nil {
		return &StatusError{Status: 502, Message: fmt.Sprintf("pack %s: %v", filepath.Base(packPath), scrubError(err.Error()))}
	}
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	if idxSrc != servingIdx {
		raw, rerr := os.ReadFile(idxSrc)
		if rerr != nil {
			return &StatusError{Status: 500, Message: fmt.Sprintf("read idx: %v", scrubError(rerr.Error()))}
		}
		if werr := os.WriteFile(servingIdx, raw, 0o644); werr != nil {
			return &StatusError{Status: 500, Message: fmt.Sprintf("install idx: %v", scrubError(werr.Error()))}
		}
	}
	if _, err := h.AddPack(ctx, packPath, checksum, tier, meta); err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("publish pack %s: %v (safe to retry after delete; orphan packs are inert)", checksum, scrubError(err.Error()))}
	}
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
