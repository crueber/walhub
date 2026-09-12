// placeholder.go — explicit create-repo placeholder (Forgejo #210, R1).
//
// A placeholder is a repo whose bucket state is indistinguishable from a
// freshly PUT-created repo (manifest HeadSeq:0, no refs) PLUS a marker
// recording that it was created through the placeholder flow and has never
// received a push. "Non-real" is a UI/UX designation, not a second storage
// state: no new manifest state, no new WAL kind.
//
// The marker lives at repos/<o>/<r>/meta/placeholder.json — Create-once
// (PutCreate; 412 = already a placeholder), Delete-on-transition. Lifecycle
// is Delete-then-Create ONLY, never Update (invitation class per
// 01_identity_permissions.md §7: Create-only, delete-on-terminal, NOT
// overwritable → no §14.11 frozen-list change required).
//
// ### Concurrency
// Hazard: two creators racing one name (manifest Create + sidecar Create +
// access.json Create, three independent keys). Avoidance: the manifest
// Create arbitrates (412 = ErrExists); the sidecar/access Creates are
// adopt-on-412 (P3 event-path rule). No lock is held across any store call;
// the two post-manifest PUTs run in parallel (independent keys, law 6).
// Hazard: marker clear racing a concurrent read. Avoidance: the clear is a
// post-commit unconditional Delete (idempotent); readers key placeholder
// affordances on refs==0 && marker, never marker alone, so a stale marker
// on a real repo renders as real.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// PlaceholderKeySuffix is the repo-relative marker key. Frozen classification:
// Create-then-delete, immutable while present (same class as
// meta/import.json provenance, but delete-on-transition makes it LESS than
// overwritable — no 14 §14.11 amendment; see Decisions).
const PlaceholderKeySuffix = "meta/placeholder.json"

// placeholderKey returns the bucket-relative marker key for id.
func placeholderKey(id git.RepoId) string { return id.StorePrefix() + PlaceholderKeySuffix }

// PlaceholderDoc is the marker body.
type PlaceholderDoc struct {
	Version      int     `json:"version"`
	CreatedBy    string  `json:"created_by"`
	CreatedAt    string  `json:"created_at"` // RFC 3339 UTC
	ObjectFormat string  `json:"object_format"`
	ExpiresAt    *string `json:"expires_at"` // always null in this change (TTL off, future)
}

// PlaceholderInfo is the summary wire projection (R1 B1): additive
// placeholder: {created_by, created_at, expires_at} | null, probed from the
// sidecar ONLY when the manifest shows HeadSeq==0 && refs==0 (real repos pay
// +0 round trips — the branch is on data in hand).
type PlaceholderInfo struct {
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
}

// probePlaceholder reads the marker by exact key (never LIST, law 4).
// Absent OR any error → (nil, false): a missing/unreadable marker is "not a
// placeholder", never damage. Callers branch on data in hand first (empty
// repos only).
func probePlaceholder(ctx context.Context, st store.ObjectStore, id git.RepoId) (*PlaceholderDoc, bool) {
	if st == nil {
		return nil, false
	}
	body, _, err := store.GetBytes(ctx, st, placeholderKey(id), store.GetOptions{})
	if err != nil || body == nil {
		return nil, false
	}
	var doc PlaceholderDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	return &doc, true
}

// CreateOwnerGate is the creation owner-admission gate (Forgejo #346):
// the owner must equal the principal's own username or be an org the
// principal belongs to (any roster role — v1 member-may-create); host
// admins bypass. Composition injects the identity service; nil →
// legacy-open (no gate). Deny shapes: anonymous → 401, foreign owner →
// 403 naming the allowed owners, probe errors → 503 (never 403-as-404).
type CreateOwnerGate interface {
	// CheckCreateOwner admits creation under owner for p (nil = admit).
	CheckCreateOwner(ctx context.Context, owner string, p auth.Principal) *auth.AuthError
}

// OrgLister lists org names for the owners/detailed is_org marker
// (Forgejo #348, Env.Orgs). Composition injects the identity service;
// nil → every row renders is_org=false. A list error fails open to
// all-false (display metadata must never fail the listing).
type OrgLister interface {
	// ListOrgs reports every org name, sorted.
	ListOrgs(ctx context.Context) ([]string, error)
}

// UserAvatar reports the stable avatar URL for a username (Forgejo
// #376, Env.Avatars behind GET /api/v1/me's avatar_url). Composition
// injects the identity service; nil → me() omits avatar_url (the
// navbar renders the username fallback). "" means the user has no
// avatar (display metadata must never fail the me() call).
type UserAvatar interface {
	// UserAvatarURL returns the stable avatar URL ("?v=" cache-busted)
	// or "" when the user has none.
	UserAvatarURL(ctx context.Context, username string) string
}

// AccessBootstrap materializes the eager access.json default at placeholder
// creation (01 §10 synthesized default, eagerly written). Create-wins,
// adopt-don't-overwrite (412 = someone raced us — adopt, never overwrite).
// Implementations skip the creator binding when the creator is not a valid
// user: subject (auth-none anonymous) and may skip materialization entirely,
// relying on read-time synthesis (R1 B5).
//
// visibility is the POST body's visibility
// ("public"|"authenticated"|"private"; "" from the
// PUT-flag path, which has no visibility concept) — "private" or
// "authenticated" materializes that doc, anything else the public
// default. Callers validate the spelling; implementations treat unknown
// as public (never fail creation on a visibility paraphrase).
type AccessBootstrap interface {
	EnsureRepoAccess(ctx context.Context, owner, repo, creator, visibility string) error
}

// PlaceholderHints is the same-process adoption hint set (push-budget
// guard): createPlaceholder records the id here; the server's
// adoptPlaceholder consumes it. A consumed id earns exactly one
// post-response marker Delete; an unconsumed id earns ZERO push-path store
// ops (the push budget test pins this: auto-created and pre-existing repos
// never touch the marker key).
//
// Correctness never depends on the set (law 4: memory is a cache —
// "if every instance is wiped, what is lost?" answers "timely cleanup"):
// a missed hint leaves a stale marker that readers ignore on real repos
// (affordances key on refs==0 && marker, never marker alone) until a later
// same-process push or a future maintainer sweep clears it.
type PlaceholderHints struct {
	mu  sync.Mutex
	ids map[string]bool
}

// Add records a placeholder creation (best-effort hint; nil-safe).
func (h *PlaceholderHints) Add(id git.RepoId) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ids == nil {
		h.ids = map[string]bool{}
	}
	h.ids[id.String()] = true
}

// Consume reports and clears a pending adoption hint (nil-safe → false:
// without a wired set the push path issues no marker ops at all).
func (h *PlaceholderHints) Consume(id git.RepoId) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.ids[id.String()] {
		return false
	}
	delete(h.ids, id.String())
	return true
}

// uiRouteCollisionWarning returns the non-blocking warning when the owner
// name collides with a reserved single-segment UI route (creation is
// allowed; only the /:owner UI page misroutes — git/API paths unaffected).
func uiRouteCollisionWarning(owner string) string {
	switch strings.ToLower(owner) {
	case "import", "api", "keys", "setup", "explore", "how-it-works", "new", "notifications":
		return "owner name collides with a UI route"
	}
	return ""
}

// checkCreateOwner enforces the owner-admission gate BEFORE any namespace
// write (a deny allocates no counter, writes no manifest). Returns true
// when the response is already written (deny or probe error).
func (h *handlers) checkCreateOwner(w http.ResponseWriter, r *http.Request, id git.RepoId, p auth.Principal) bool {
	gate := h.env.CreateOwnerGate
	if gate == nil {
		return false
	}
	if cerr := gate.CheckCreateOwner(r.Context(), id.Owner, p); cerr != nil {
		switch cerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, cerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, cerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, cerr.Why)
		}
		return true
	}
	return false
}

// createPlaceholder runs the one writer path: manifest already created by
// the caller (Repos.Create won), now Create the sidecar + eager access.json
// in parallel (independent keys, law 6 — one round-trip window). 412 on
// either is adopt (idempotent, P3 rule). Any other store error → error
// (the manifest-without-sidecar IS a valid empty repo per #209, so the
// caller leaves it; a retry re-Creates the sidecar idempotently).
//
// ### Concurrency
// Hazard: holding nothing across the network — both PUTs are independent.
// Avoidance: parallel goroutines joined on a WaitGroup; no lock of any
// kind is held (law 13: never hold a lock across a store call).
func (h *handlers) createPlaceholder(r *http.Request, id git.RepoId, format git.ObjectFormat, principal, visibility string) error {
	if h.env.Store == nil {
		return nil // no store → marker skipped (tests without a store)
	}
	now := h.env.Now()
	if now.IsZero() {
		now = time.Now()
	}
	doc := PlaceholderDoc{
		Version:      1,
		CreatedBy:    principal,
		CreatedAt:    now.UTC().Format(time.RFC3339),
		ObjectFormat: format.String(),
		ExpiresAt:    nil,
	}
	raw, _ := json.Marshal(doc)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := h.env.Store.Put(r.Context(), placeholderKey(id),
			store.PutBody{Bytes: raw},
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
		if err != nil && store.IsPreconditionFailed(err) {
			err = nil // 412 = already a placeholder — idempotent adopt
		}
		if err != nil {
			errCh <- err
		}
	}()
	go func() {
		defer wg.Done()
		if h.env.AccessBoot == nil {
			return
		}
		if err := h.env.AccessBoot.EnsureRepoAccess(r.Context(), id.Owner, id.Name, principal, visibility); err != nil {
			errCh <- err
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	// Record the adoption hint AFTER both Creates won: the first push to
	// this id will consume it and clear the marker post-response. A
	// failed create records nothing (no phantom hints → no phantom
	// push-path ops).
	h.env.PlaceholderHints.Add(id)
	return nil
}

// conflictBody renders the plain-text 409 body (B3: frozen 409-with-html_url
// stays plain-text via writePlain — never a JSON envelope). The winner URL
// rides inside the text so the UI can link the squatter.
func (h *handlers) conflictBody(r *http.Request, id git.RepoId) string {
	base := h.env.baseURL(r)
	return "repository already exists: " + base + "/" + id.Owner + "/" + id.Name
}

// writeCreateJSON renders the 201/200 create body. Added fields (B3 — all
// enumerated in 07_api.md, additive per 14 §14.12): placeholder, clone_url,
// ssh_clone_url, html_url, already, warning, expires_at.
func (h *handlers) writeCreateJSON(w http.ResponseWriter, r *http.Request, id git.RepoId, already bool, status int) {
	base := h.env.baseURL(r)
	body := map[string]any{
		"owner":       id.Owner,
		"name":        id.Name,
		"full_name":   id.Owner + "/" + id.Name,
		"placeholder": true,
		"clone_url":   base + "/" + id.Owner + "/" + id.Name + ".git",
		"html_url":    base + "/" + id.Owner + "/" + id.Name,
	}
	if sshURL := h.env.sshCloneURL(r, id.Owner, id.Name); sshURL != "" {
		body["ssh_clone_url"] = sshURL
	}
	if already {
		body["already"] = true
	}
	if warn := uiRouteCollisionWarning(id.Owner); warn != "" {
		body["warning"] = warn
	}
	writeJSON(w, status, body)
}

// --- POST /api/v1/repos twin (Seam 1, both lanes) ----------------------------
//
// ExposedTemplatesCreate lists the discovery endpoints[] entry (14 §14.12
// lane rule) — registered from composition via api.RegisterExposed in the
// same change (law 12, Feature 10 precedent). The handler serves BOTH lanes
// (the /api/v1 + /api-browser/v1 twins); discovery lists the /api/v1
// template only (same convention as repoimport.ExposedTemplates).
var ExposedTemplatesCreate = []string{"/api/v1/repos"}

// CreateHandler is the Seam 1 surface for POST /api/v1/repos. Composition
// chains it in front of the core mux via server.ChainExtra (NOT a
// core-table edit — law 8; R1 B2). It has no import of internal/server (the
// ExtraRoutes satisfaction is structural: Handle(w, r) bool).
type CreateHandler struct {
	Env *Env
}

// Handle answers one request; false when the path is not a create route.
func (ch *CreateHandler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitCreatePath(r)
	if len(segs) != 3 || (segs[0] != "api" && segs[0] != "api-browser") {
		return false
	}
	if segs[1] != "v1" || segs[2] != "repos" {
		return false
	}
	if r.Method != http.MethodPost {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	}
	ch.post(w, r)
	return true
}

// ServeHTTP answers create routes and 404s otherwise (httptest surface).
func (ch *CreateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !ch.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func splitCreatePath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if d, err := url.PathUnescape(s); err == nil {
			out = append(out, d)
		} else {
			out = append(out, s)
		}
	}
	return out
}

// createRequest is the POST body: placeholder defaults true for UI use.
type createRequest struct {
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	ObjectFormat string `json:"object_format,omitempty"`
	Placeholder  *bool  `json:"placeholder,omitempty"`
	Visibility   string `json:"visibility,omitempty"`
}

func (ch *CreateHandler) post(w http.ResponseWriter, r *http.Request) {
	e := ch.Env
	if e == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	h := &handlers{env: e}
	if !e.gate(w, r, AuthWrite) {
		return
	}
	body, err := readCreateBody(r)
	if err != nil {
		writePlain(w, http.StatusBadRequest, err.Error())
		return
	}
	id, perr := git.ParseRepoId(body.Owner + "/" + body.Name)
	if perr != nil {
		writePlain(w, http.StatusBadRequest, fmt.Sprintf("invalid repo id %q: %v", body.Owner+"/"+body.Name, perr))
		return
	}
	format := git.Sha1
	if body.ObjectFormat != "" {
		f, ferr := git.ObjectFormatFrom(body.ObjectFormat)
		if ferr != nil {
			writePlain(w, http.StatusBadRequest, ferr.Error())
			return
		}
		format = f
	}
	if body.Visibility != "" && body.Visibility != "public" && body.Visibility != "authenticated" && body.Visibility != "private" {
		writePlain(w, http.StatusBadRequest, `visibility must be public|authenticated|private`)
		return
	}
	placeholder := true
	if body.Placeholder != nil {
		placeholder = *body.Placeholder
	}
	p := e.PrincipalOf(r)
	if h.checkCreateOwner(w, r, id, p) {
		return
	}
	if e.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	if cerr := e.Repos.Create(r.Context(), id, format); cerr != nil {
		ch.createExists(w, r, id, p.Name, placeholder)
		return
	}
	if placeholder {
		if err := h.createPlaceholder(r, id, format, p.Name, body.Visibility); err != nil {
			mapViewErr(w, err)
			return
		}
		base := e.baseURL(r)
		w.Header().Set("Location", base+"/"+id.Owner+"/"+id.Name)
		h.writeCreateJSON(w, r, id, false, http.StatusCreated)
		return
	}
	base := e.baseURL(r)
	w.Header().Set("Location", base+"/"+id.Owner+"/"+id.Name)
	writeJSON(w, http.StatusCreated, map[string]string{
		"owner":     id.Owner,
		"name":      id.Name,
		"full_name": id.Owner + "/" + id.Name,
	})
}

// createExists handles Create contention (the fork-target rule, 03 §8: one
// Create wins, the other reports 409 with the winner's URL). Same-principal
// re-create of a STILL-UNBORN placeholder → idempotent 200 already:true;
// otherwise → 409 plain-text carrying the winner URL (B3). The unborn
// re-check matters: same-principal re-create AFTER the first push (now
// real) is 409 — it is a repo (§6).
func (ch *CreateHandler) createExists(w http.ResponseWriter, r *http.Request, id git.RepoId, principal string, placeholder bool) {
	e := ch.Env
	h := &handlers{env: e}
	if placeholder {
		if doc, ok := probePlaceholder(r.Context(), e.Store, id); ok && doc != nil {
			if samePrincipal(doc.CreatedBy, principal) && h.stillUnborn(r.Context(), id) {
				h.writeCreateJSON(w, r, id, true, http.StatusOK)
				return
			}
		}
	}
	writePlain(w, http.StatusConflict, h.conflictBody(r, id))
}

// stillUnborn reports the §6 create-twice predicate: no resolvable head and
// zero branches/tags. Fail-closed: any view error → false (a repo we cannot
// prove unborn is treated as real → 409, never a mistaken re-affirm).
func (h *handlers) stillUnborn(ctx context.Context, id git.RepoId) bool {
	if h.env.Repo == nil {
		return true // no view (tests without an engine): the sidecar decides
	}
	s, err := h.env.Repo.Summary(ctx, id)
	if err != nil {
		return false
	}
	return s.Head == nil && s.Branches == 0 && s.Tags == 0
}

// samePrincipal compares creator identity (case-insensitive: email
// principals normalize to lowercase; exact match otherwise).
func samePrincipal(a, b string) bool {
	if a == b {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func readCreateBody(r *http.Request) (*createRequest, error) {
	defer r.Body.Close()
	var req createRequest
	// Bound the body: a create request is a handful of fields.
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("unreadable body: %v", err)
	}
	req.Owner = strings.TrimSpace(req.Owner)
	req.Name = strings.TrimSpace(strings.TrimSuffix(req.Name, ".git"))
	if req.Owner == "" || req.Name == "" {
		return nil, fmt.Errorf("owner and name are required")
	}
	return &req, nil
}
