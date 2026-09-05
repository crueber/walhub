// doc.go — meta/import.json provenance (Create-once-then-CAS'd, the
// frozen overwritable family — 14_extensibility.md §14.11 rule 2, same
// family as fork.json in docs/features/03 §7) + the importer-admin
// access.json write (S7: explicit via the identity service,
// SynthesizeDefault stays the backstop only).
//
// The sidecar doubles as the import CLAIM (fix #79): the in-progress
// claim (Complete=false) is PutCreated BEFORE the manifest CAS, so a
// manifest without a sidecar is unambiguously foreign (B3 409 stands),
// while a same-source retry over an in-progress claim resumes to
// completion instead of wedging on "delete and retry". The manifest
// Create stays the name-ownership arbitration (exactly-one-winner via
// the store CAS); completion is a version-checked CAS electing exactly
// one completer, and any post-claim failure rolls back to a resumable
// (or clean) state — never a caller-unfixable one.
package repoimport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
)

// ImportDoc is the provenance sidecar (bucket JSON, UTF-8, []-not-null,
// RFC 3339, full SHAs — the P1 family rules). Complete=false marks the
// in-progress CLAIM (fix #79): written before the manifest CAS, carrying
// the lease expiry after which a manifest-less target may be taken over
// by a different source; the terminal write flips Complete=true with
// the imported heads.
type ImportDoc struct {
	Version       int               `json:"version"`
	SourceURL     string            `json:"source_url"`  // canonical, token scrubbed
	SourceKind    string            `json:"source_kind"` // github|generic|file
	RequestedRefs []string          `json:"requested_refs"`
	ImportedAt    string            `json:"imported_at"` // RFC3339 UTC
	HeadSHAs      map[string]string `json:"head_shas"`   // ref → full sha
	Importer      string            `json:"importer"`
	Format        string            `json:"format"` // sha1|sha256
	// Complete reports a landed import. Pre-fix sidecars decode false
	// and are converged (then completed) by the next same-source run —
	// never mistaken for a no-op.
	Complete bool `json:"complete"`
	// ClaimExpiresAt bounds the in-progress claim (RFC3339 UTC, set on
	// claims only). Expiry gates NOTHING for the claiming source
	// (same-source resume always proceeds); it only lets a DIFFERENT
	// source take over a target that still has no manifest.
	ClaimExpiresAt string `json:"claim_expires_at,omitempty"`
}

// readImportDoc loads the provenance sidecar; (nil, "", nil) when absent.
func readImportDoc(ctx context.Context, st store.ObjectStore, owner, repo string) (*ImportDoc, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, st, importKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("read import.json: %v", scrubError(err.Error()))}
	}
	var doc ImportDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Corrupt/unreadable provenance never adopts (B3): 409 names
		// the delete-and-retry fix; a store failure stays 500.
		return nil, "", &StatusError{Status: 409, Message: fmt.Sprintf("import.json corrupt (target %s/%s was not cleanly imported; delete and retry, or pick another name): %v", owner, repo, scrubError(err.Error()))}
	}
	if doc.RequestedRefs == nil {
		doc.RequestedRefs = []string{}
	}
	if doc.HeadSHAs == nil {
		doc.HeadSHAs = map[string]string{}
	}
	return &doc, meta.Version, nil
}

// claimMode is the claim-stage outcome (fix #79).
type claimMode int

const (
	// claimFresh: this run PutCreated the in-progress claim (it owns the
	// claim; version is the Put meta).
	claimFresh claimMode = iota
	// claimResume: a pre-existing in-progress claim names the same
	// canonical source — converge to completion (version is current).
	claimResume
	// claimAdopt: a pre-existing COMPLETE sidecar names the same source
	// — idempotent no-op, zero pack traffic.
	claimAdopt
)

// claimTTL bounds an in-progress claim: clone_timeout + git_timeout
// covers the slowest legitimate body (clone + enumeration + repack
// under the layer timeout). No new knob: expiry only gates
// different-source takeover of a manifest-less target, never
// same-source resume.
func claimTTL(cfg *config.Config) time.Duration {
	if cfg == nil {
		cfg = config.Defaults()
	}
	return time.Duration(cfg.Import.CloneTimeout) + time.Duration(cfg.Import.GitTimeout)
}

// claimImportDoc PutCreates the in-progress claim BEFORE the manifest
// CAS (fix #79). On a 412 it resolves the race: complete + same source
// → claimAdopt; in-progress + same source → claimResume; in-progress +
// different source + expired + manifest absent → version-checked
// takeover (claimFresh); anything else → 409, never silently adopt a
// foreign target (B3). The returned version is the CAS base for the
// terminal complete (fresh/takeover) or the current version (resume);
// on claimAdopt the returned doc is the landed sidecar.
func claimImportDoc(ctx context.Context, st store.ObjectStore, owner, repo string, doc *ImportDoc) (claimMode, *ImportDoc, store.Version, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return claimFresh, nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("encode import.json: %v", err)}
	}
	meta, err := store.PutBytes(ctx, st, importKey(owner, repo), raw, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err == nil {
		return claimFresh, nil, meta.Version, nil
	}
	if !store.IsPreconditionFailed(err) {
		return claimFresh, nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("write import.json: %v", scrubError(err.Error()))}
	}
	return resolveClaim(ctx, st, owner, repo, doc, raw, false)
}

// resolveClaim maps a lost claim CAS onto adopt/resume/takeover/409.
// retried is true on the single takeover-loss retry (no unbounded loop).
func resolveClaim(ctx context.Context, st store.ObjectStore, owner, repo string, doc *ImportDoc, raw []byte, retried bool) (claimMode, *ImportDoc, store.Version, error) {
	cur, ver, rerr := readImportDoc(ctx, st, owner, repo)
	if rerr != nil {
		return claimFresh, nil, "", rerr
	}
	if cur == nil {
		// Lost the Create to a delete (rollback/takeover landed
		// between): retry the Create once, else 409.
		if retried {
			return claimFresh, nil, "", &StatusError{Status: 409, Message: fmt.Sprintf("target %s/%s was claimed by another import; delete and retry, or pick another name", owner, repo)}
		}
		meta, cerr := store.PutBytes(ctx, st, importKey(owner, repo), raw, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
		if cerr == nil {
			return claimFresh, nil, meta.Version, nil
		}
		if !store.IsPreconditionFailed(cerr) {
			return claimFresh, nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("write import.json: %v", scrubError(cerr.Error()))}
		}
		return resolveClaim(ctx, st, owner, repo, doc, raw, true)
	}
	if cur.Complete && cur.SourceURL == doc.SourceURL {
		// Landed + same source → adopt — unless the manifest is
		// gone (operator-deleted repo): then resume re-creates
		// and re-converges it instead of reporting success over
		// an absent repo.
		if manifestPresent(ctx, st, owner, repo) {
			return claimAdopt, cur, ver, nil
		}
		return claimResume, cur, ver, nil
	}
	if cur.Complete {
		return claimFresh, nil, "", &StatusError{Status: 409, Message: fmt.Sprintf("target %s/%s was imported from another source; delete and retry, or pick another name", owner, repo)}
	}
	if cur.SourceURL == doc.SourceURL {
		return claimResume, cur, ver, nil
	}
	// In-progress foreign claim: take over only a manifest-less target
	// past its lease (a live import, or any live repo, is never
	// touched); the manifest Create stays the backstop for races here.
	if claimExpired(cur) && !manifestPresent(ctx, st, owner, repo) {
		upMeta, uerr := store.PutBytes(ctx, st, importKey(owner, repo), raw,
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
		if uerr == nil {
			return claimFresh, nil, upMeta.Version, nil
		}
		if store.IsPreconditionFailed(uerr) && !retried {
			return resolveClaim(ctx, st, owner, repo, doc, raw, true)
		}
		if store.IsPreconditionFailed(uerr) {
			return claimFresh, nil, "", &StatusError{Status: 409, Message: fmt.Sprintf("target %s/%s was claimed by another import; delete and retry, or pick another name", owner, repo)}
		}
		return claimFresh, nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("write import.json: %v", scrubError(uerr.Error()))}
	}
	return claimFresh, nil, "", &StatusError{Status: 409, Message: fmt.Sprintf("target %s/%s has an import in progress from another source; wait for it, retry with the same source URL to resume it, or delete and retry", owner, repo)}
}

// manifestPresent probes the manifest key (probe, don't list — law 4).
func manifestPresent(ctx context.Context, st store.ObjectStore, owner, repo string) bool {
	meta, err := st.Head(ctx, store.RepoPrefix(owner, repo)+store.Manifest)
	return err == nil && meta != nil
}

// claimExpired reports whether an in-progress claim is past its lease.
// Undated/unparseable claims never expire (fail closed: no takeover).
func claimExpired(cur *ImportDoc) bool {
	if cur == nil || cur.ClaimExpiresAt == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, cur.ClaimExpiresAt)
	if err != nil {
		return false
	}
	return time.Now().UTC().After(exp)
}

// completeImportDoc flips the claim to the landed sidecar (version-
// checked CAS electing exactly one completer). A lost CAS re-reads: a
// same-source completion adopts (benign race → no-op), anything else
// is a loud 409/500 — never a silent overwrite.
func completeImportDoc(ctx context.Context, st store.ObjectStore, owner, repo string, doc *ImportDoc, base store.Version) (*ImportDoc, error) {
	doc.Complete = true
	doc.ClaimExpiresAt = ""
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, &StatusError{Status: 500, Message: fmt.Sprintf("encode import.json: %v", err)}
	}
	_, uerr := store.PutBytes(ctx, st, importKey(owner, repo), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: base, ContentType: "application/json"})
	if uerr == nil {
		return doc, nil
	}
	if !store.IsPreconditionFailed(uerr) {
		return nil, &StatusError{Status: 500, Message: fmt.Sprintf("write import.json: %v", scrubError(uerr.Error()))}
	}
	cur, _, rerr := readImportDoc(ctx, st, owner, repo)
	if rerr != nil {
		return nil, rerr
	}
	if cur != nil && cur.Complete && cur.SourceURL == doc.SourceURL {
		return cur, nil // benign race: the winner recorded the same source
	}
	return nil, &StatusError{Status: 409, Message: fmt.Sprintf("target %s/%s was claimed by another import; delete and retry, or pick another name", owner, repo)}
}

// deleteImportDoc removes the sidecar (rollback of our own claim —
// version-checked so a concurrent takeover is never deleted under).
func deleteImportDoc(ctx context.Context, st store.ObjectStore, owner, repo string, ver store.Version) error {
	return st.Delete(ctx, importKey(owner, repo), ver)
}

// writeImportDoc Creates a COMPLETE sidecar (test/seed path — the live
// protocol claims first via claimImportDoc, then CAS-completes via
// completeImportDoc).
func writeImportDoc(ctx context.Context, st store.ObjectStore, owner, repo string, doc *ImportDoc) error {
	doc.Complete = true
	doc.ClaimExpiresAt = ""
	raw, err := json.Marshal(doc)
	if err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("encode import.json: %v", err)}
	}
	_, err = store.PutBytes(ctx, st, importKey(owner, repo), raw, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err == nil {
		return nil
	}
	if !store.IsPreconditionFailed(err) {
		return &StatusError{Status: 500, Message: fmt.Sprintf("write import.json: %v", scrubError(err.Error()))}
	}
	return &StatusError{Status: 409, Message: fmt.Sprintf("target %s/%s was claimed by another import; delete and retry, or pick another name", owner, repo)}
}

// ensureImporterAdmin writes the importer as admin on access.json (S7):
// read-modify-write over the current (or synthesized) doc, bounded CAS
// retries, then the BootstrapRepo backstop. Visibility is preserved —
// import never flips a repo public/private. A non-email importer (auth
// none's "anon", service principals) cannot be bound (identity requires
// user:<email> subjects) — the backstop covers those; the caller narrates
// the skip.
func ensureImporterAdmin(ctx context.Context, roles RoleService, owner, repo, importer string) error {
	if roles == nil {
		return nil // no identity surface (CLI without wiring): the read
		// path synthesizes the legacy default; the bootstrap op
		// materializes it (documented backstop).
	}
	if !identity.ValidPrincipal(importer) {
		return nil // user:<email> subjects only — backstop covers the rest
	}
	doc, ver, err := roles.GetAccess(ctx, owner, repo)
	if err != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("read access.json: %v", scrubError(err.Error()))}
	}
	if doc == nil {
		doc = identity.SynthesizeDefault(owner)
		ver = ""
	}
	want := "user:" + strings.ToLower(strings.TrimSpace(importer))
	for _, b := range doc.RoleBindings {
		if strings.ToLower(b.Subject) == want && b.Role == identity.RoleAdmin {
			return nil // already present (re-run / bootstrap won)
		}
	}
	bindings := append(append([]identity.AccessBinding{}, doc.RoleBindings...), identity.AccessBinding{Subject: want, Role: identity.RoleAdmin})
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if _, last = roles.PutAccess(ctx, owner, repo, ver, doc.Visibility, bindings); last == nil {
			break
		}
		cur, v, rerr := roles.GetAccess(ctx, owner, repo)
		if rerr != nil {
			return &StatusError{Status: 500, Message: fmt.Sprintf("re-read access.json: %v", scrubError(rerr.Error()))}
		}
		doc, ver = cur, v
		present := false
		for _, b := range doc.RoleBindings {
			if strings.ToLower(b.Subject) == want && b.Role == identity.RoleAdmin {
				present = true
			}
		}
		if present {
			return nil
		}
		bindings = append(append([]identity.AccessBinding{}, doc.RoleBindings...), identity.AccessBinding{Subject: want, Role: identity.RoleAdmin})
	}
	if last != nil {
		return &StatusError{Status: 500, Message: fmt.Sprintf("write access.json: %v", scrubError(last.Error()))}
	}
	return nil
}

// importerBackstop runs the identity bootstrap after the explicit write
// (S7: SynthesizeDefault stays the backstop only — a no-op skip when the
// explicit write won).
func importerBackstop(ctx context.Context, roles RoleService, owner, repo string) {
	if roles == nil {
		return
	}
	_, _ = roles.BootstrapRepo(ctx, owner, repo)
}

// nowRFC3339 renders UTC RFC3339 (wire rule).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
