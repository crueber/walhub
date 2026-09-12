// profile.go — GET/PUT /api/v1/owners/{owner}/profile (Forgejo #234).
//
// Owner profiles (display name, location, IANA timezone, markdown bio) hang
// off the owner namespace: owners/<owner>/profile.json (store.OwnerProfileKey),
// a CAS'd sidecar in the frozen overwritable family (14 §14.11 rule 2,
// amended in the same change). Owner scope has no WAL or manifest, so the
// bounded CAS loop (13_concurrency.md §3: read, replace, Create-or-Update,
// retry-on-412, 5 attempts then 409) IS the commit point — the §14.10.2
// issues-index shape, not the settings publish path. No lock is held across
// any store call; PUTs are idempotent full-document replaces at human rate,
// so the loop converges without a client-carried version (unlike
// access.json, whose concurrent admin edits need the 409-to-client).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Owner profile field budgets (400 on overflow; plain-text errors per §2).
const (
	maxProfileName     = 200   // display_name / location, runes
	maxProfileTimezone = 64    // timezone, bytes (longest IANA names are ~30)
	maxProfileBio      = 65536 // bio_markdown, bytes (64 KiB of prose is plenty)
	maxProfileBody     = 128 << 10
)

// tzShape is the server-side timezone contract: one to four slash-separated
// IANA-style segments (Area/City, Etc/GMT+5, UTC). It is deliberately a SHAPE
// check, not time.LoadLocation: the runtime image carries no tz database, so
// LoadLocation would false-reject valid zones. IANA validity is enforced by
// the UI picker (Intl.supportedValuesOf('timeZone')); an unknown-but-shaped
// zone is accepted (forward-compatible with tz-db updates) and renders
// verbatim.
var tzShape = regexp.MustCompile(`^[A-Za-z0-9_+\-]{1,32}(/[A-Za-z0-9_+\-]{1,32}){0,3}$`)

// OwnerProfile is the wire + stored shape (additive per 14 §14.12: new
// optional fields only). can_edit is request-scoped (never stored): whether
// the caller may PUT this profile.
type OwnerProfile struct {
	Owner       string `json:"owner"`
	DisplayName string `json:"display_name"`
	Location    string `json:"location"`
	Timezone    string `json:"timezone"`
	BioMarkdown string `json:"bio_markdown"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	CanEdit     bool   `json:"can_edit,omitempty"`
}

// OwnerEditor decides org-role profile edits (docs/features/01 P6). Core
// grants host admins and name-matched principals itself; the wired editor
// adds org-owner-role grants without core importing the identity package
// (law 8 — same shape as CreateOwnerGate/AccessBoot). Nil → core default only.
// Probe errors fail closed (the caller 503s, never 403-as-404).
type OwnerEditor interface {
	CanEditOwnerProfile(ctx context.Context, owner string, p auth.Principal) (bool, error)
}

// defaultCanEdit is the core rule: host admin, or the principal's own name
// matches the owner slug (case-insensitive — slugs are lowercase by
// convention). Anonymous principals never reach here with write (the
// AuthWrite gate 403s them first), so no anonymous-name collision is
// possible.
func defaultCanEdit(p auth.Principal, owner string) bool {
	if p.Admin {
		return true
	}
	return strings.EqualFold(p.Name, owner)
}

// validProfileOwner reuses the frozen repo-id charset for the owner segment
// (no leading dot, not "..", [A-Za-z0-9._-], ≤100 chars): owners/<owner>/
// must stay one path segment. ParseRepoId on a synthetic id validates the
// owner part without new charset code ("" fails validPart too).
func validProfileOwner(owner string) bool {
	_, err := git.ParseRepoId(owner + "/x")
	return err == nil
}

// emptyProfile is the §8 unknown-owner convention (same as ownerRepos'
// 200 []): an unknown owner reads as an empty profile, never 404. Only a
// syntactically invalid slug 404s.
func emptyProfile(owner string) OwnerProfile {
	return OwnerProfile{Owner: owner}
}

// readProfile fetches the stored doc; nil when absent. Corrupt JSON surfaces
// as a store error (the bucket is wrong — 503 via mapViewErr, same class as
// the detailed listing's corrupt-catalog rule).
func readProfile(ctx context.Context, st store.ObjectStore, owner string) (*OwnerProfile, error) {
	raw, _, err := store.GetBytes(ctx, st, store.OwnerProfileKey(owner), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc OwnerProfile
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		return nil, uerr
	}
	doc.Owner = owner // the key is truth, not the body
	return &doc, nil
}

// validateProfile enforces the field budgets on a decoded PUT body (unknown
// fields are rejected by the decoder before this runs). Empty strings are
// "unset" — clearing a field is PUTting "".
func validateProfile(doc *OwnerProfile) string {
	if runeCount(doc.DisplayName) > maxProfileName {
		return "display_name too long (max 200 characters)"
	}
	if runeCount(doc.Location) > maxProfileName {
		return "location too long (max 200 characters)"
	}
	if len(doc.Timezone) > maxProfileTimezone || (doc.Timezone != "" && !tzShape.MatchString(doc.Timezone)) {
		return "timezone must be an IANA zone name (e.g. Europe/Berlin)"
	}
	if len(doc.BioMarkdown) > maxProfileBio || !utf8.ValidString(doc.BioMarkdown) {
		return "bio_markdown too long (max 64 KiB)"
	}
	return ""
}

func runeCount(s string) int { return len([]rune(s)) }

// canEditProfile resolves the full PUT rule: AuthWrite gate first (401/403),
// then host-admin / name-match / wired org-owner grant. Editor probe errors
// fail closed with 503.
func (h *handlers) canEditProfile(w http.ResponseWriter, r *http.Request, owner string) (auth.Principal, bool) {
	if !h.env.gate(w, r, AuthWrite) {
		return auth.Principal{}, false
	}
	p := h.env.PrincipalOf(r)
	if defaultCanEdit(p, owner) {
		return p, true
	}
	if h.env.OwnerEdit != nil {
		ok, err := h.env.OwnerEdit.CanEditOwnerProfile(r.Context(), owner, p)
		if err != nil {
			writePlain(w, http.StatusServiceUnavailable, "profile authorization unavailable")
			return auth.Principal{}, false
		}
		if ok {
			return p, true
		}
	}
	writePlain(w, http.StatusForbidden, "only this owner may edit the profile")
	return auth.Principal{}, false
}

// ownerProfileGet serves the profile (AuthRead — public bio). Unknown owners
// read as an empty profile (200, §8 convention); can_edit is computed
// best-effort (editor probe failure → false, never a read error).
func (h *handlers) ownerProfileGet(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	owner := r.PathValue("owner")
	if !validProfileOwner(owner) {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	if h.env.Store == nil {
		writePlain(w, http.StatusServiceUnavailable, "profile store not configured")
		return
	}
	doc, err := readProfile(r.Context(), h.env.Store, owner)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	out := emptyProfile(owner)
	if doc != nil {
		out = *doc
	}
	p := h.env.PrincipalOf(r)
	// Anonymous principals never edit (the PUT AuthWrite gate 403s them
	// first), so they never get can_edit — without this, an owner slug
	// literally named "anonymous" would name-match the anon principal and
	// advertise an Edit affordance whose save always 403s.
	if p.Anonymous {
		out.CanEdit = false
	} else {
		out.CanEdit = defaultCanEdit(p, owner)
		if !out.CanEdit && h.env.OwnerEdit != nil {
			if ok, oerr := h.env.OwnerEdit.CanEditOwnerProfile(r.Context(), owner, p); oerr == nil {
				out.CanEdit = ok
			}
		}
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, out)
}

// ownerProfilePut replaces the profile (AuthWrite + owner rule). The body is
// the profile fields; owner/updated_at/can_edit in the body are ignored (the
// key names the owner, the server stamps the write).
func (h *handlers) ownerProfilePut(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !validProfileOwner(owner) {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	if _, ok := h.canEditProfile(w, r, owner); !ok {
		return
	}
	if h.env.Store == nil {
		writePlain(w, http.StatusServiceUnavailable, "profile store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProfileBody)
	var body struct {
		DisplayName string `json:"display_name"`
		Location    string `json:"location"`
		Timezone    string `json:"timezone"`
		BioMarkdown string `json:"bio_markdown"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if derr := dec.Decode(&body); derr != nil {
		writePlain(w, http.StatusBadRequest, "invalid profile: "+derr.Error())
		return
	}
	next := OwnerProfile{
		Owner: owner, DisplayName: body.DisplayName, Location: body.Location,
		Timezone: body.Timezone, BioMarkdown: body.BioMarkdown,
		UpdatedAt: h.env.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if msg := validateProfile(&next); msg != "" {
		writePlain(w, http.StatusBadRequest, msg)
		return
	}
	raw, _ := json.Marshal(next)
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		_, meta, gerr := store.GetBytes(r.Context(), h.env.Store, store.OwnerProfileKey(owner), store.GetOptions{})
		if gerr != nil && !store.IsNotFound(gerr) {
			mapViewErr(w, gerr)
			return
		}
		opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}
		if gerr == nil {
			opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		}
		if _, perr := store.PutBytes(r.Context(), h.env.Store, store.OwnerProfileKey(owner), raw, opts); perr == nil {
			writeJSON(w, http.StatusOK, next)
			return
		} else if !store.IsPreconditionFailed(perr) {
			mapViewErr(w, perr)
			return
		}
	}
	writePlain(w, http.StatusConflict, "profile changed concurrently; reload and retry")
}
