// doc.go — meta/import.json provenance (Create-once-then-CAS'd, the
// frozen overwritable family — 14_extensibility.md §14.11 rule 2, same
// family as fork.json in docs/features/03 §7) + the importer-admin
// access.json write (S7: explicit via the identity service,
// SynthesizeDefault stays the backstop only).
package repoimport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
)

// ImportDoc is the provenance sidecar (bucket JSON, UTF-8, []-not-null,
// RFC 3339, full SHAs — the P1 family rules).
type ImportDoc struct {
	Version       int               `json:"version"`
	SourceURL     string            `json:"source_url"`  // canonical, token scrubbed
	SourceKind    string            `json:"source_kind"` // github|generic|file
	RequestedRefs []string          `json:"requested_refs"`
	ImportedAt    string            `json:"imported_at"` // RFC3339 UTC
	HeadSHAs      map[string]string `json:"head_shas"`   // ref → full sha
	Importer      string            `json:"importer"`
	Format        string            `json:"format"` // sha1|sha256
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
		return nil, "", &StatusError{Status: 500, Message: fmt.Sprintf("import.json corrupt: %v", scrubError(err.Error()))}
	}
	if doc.RequestedRefs == nil {
		doc.RequestedRefs = []string{}
	}
	if doc.HeadSHAs == nil {
		doc.HeadSHAs = map[string]string{}
	}
	return &doc, meta.Version, nil
}

// writeImportDoc Creates the provenance sidecar (the commit tail after
// manifest Create + access bootstrap). A 412 means a concurrent import
// won: a matching source_url adopts (benign race → no-op), anything else
// is a 409 (never silently adopt a foreign target — B3).
func writeImportDoc(ctx context.Context, st store.ObjectStore, owner, repo string, doc *ImportDoc) error {
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
	cur, _, rerr := readImportDoc(ctx, st, owner, repo)
	if rerr != nil {
		return rerr
	}
	if cur != nil && cur.SourceURL == doc.SourceURL {
		return nil // benign race: the winner recorded the same source
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
