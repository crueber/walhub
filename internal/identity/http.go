package identity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Wire conventions (07 §2, same as internal/api): plain-text errors,
// []-never-null, RFC 3339 UTC, per-segment decoding, SWR/ETag or no-store,
// both lanes everywhere. Anonymous-denied reads get a real 401 with
// WWW-Authenticate: Bearer.

// ExposedTemplates lists the discovery endpoints[] entries this surface
// serves (14 §14.12 lane rule) — registered from composition via
// api.RegisterExposed in the same change (law 12; Forgejo #272). One entry
// per distinct path shape: methods share a template (GET+PUT on
// users/{principal} is one entry). Top-level twins use their literal
// /api/v1 spelling; repo lanes use the /{owner}/{repo}/api spelling.
// Pinned by TestExposedTemplatesExact + TestExposedCoversRoutes (no
// phantoms, nothing missing).
var ExposedTemplates = []string{
	"/api/v1/users/{principal}",
	"/api/v1/users/{principal}/avatar",
	"/api/v1/orgs",
	"/api/v1/orgs/{org}",
	"/api/v1/orgs/{org}/avatar",
	"/api/v1/orgs/{org}/members",
	"/api/v1/orgs/{org}/members/{principal}",
	"/api/v1/orgs/{org}/teams",
	"/api/v1/orgs/{org}/teams/{slug}",
	"/api/v1/orgs/{org}/teams/{slug}/members/{principal}",
	"/api/v1/orgs/{org}/invitations",
	"/api/v1/orgs/{org}/invitations/{id}",
	"/api/v1/invitations",
	"/api/v1/invitations/{id}",
	"/api/v1/invitations/{id}/accept",
	"/{owner}/{repo}/api/access",
	"/{owner}/{repo}/api/permissions",
	"/{owner}/{repo}/api/collaborators",
	"/{owner}/{repo}/api/assignables",
	"/{owner}/{repo}/api/invitations",
	"/{owner}/{repo}/api/invitations/{id}",
	"/{owner}/{repo}/api/transfer",
}

// Handler is the Seam 1 surface: every §8 endpoint on both lanes.
// Composition chains it in front of the core api mux: Handle reports false
// for non-identity paths so the core mux answers.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// principal resolves the request principal via the injected Authenticator
// (Seam 2); nil Authenticator falls back to the mode default.
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	if h.Svc != nil {
		return h.Svc.principalOf(r.Context()), nil
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not an identity route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r) // always at least one element
	if segs[0] == "api" || segs[0] == "api-browser" {
		return h.handleTop(w, r, segs[1:])
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		return h.handleRepo(w, r, owner, repo, segs[3:])
	}
	return false
}

// ServeHTTP answers identity routes and 404s otherwise.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// splitPath splits the escaped path and decodes each segment separately.
func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		d, err := url.PathUnescape(s)
		if err != nil {
			d = s
		}
		out = append(out, d)
	}
	return out
}

// --- writers ---------------------------------------------------------------

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Del("ETag")
	if status == http.StatusUnauthorized {
		hdr.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		hdr.Set("Retry-After", "15")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*auth.AuthError); ok {
		switch aerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, aerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, aerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, aerr.Why)
		}
		return
	}
	writePlain(w, statusFor(err), err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "identity: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeCached writes JSON with a cache class + ETag/304 path.
func writeCached(w http.ResponseWriter, r *http.Request, class, etag string, status int, v any) {
	hdr := w.Header()
	hdr.Set("Cache-Control", class)
	if etag != "" {
		hdr.Set("ETag", `"`+etag+`"`)
		if matchETag(r.Header.Get("If-None-Match"), etag) {
			hdr.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, status, v)
}

func matchETag(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.Trim(p, `"`)
		if p == etag {
			return true
		}
	}
	return false
}

const (
	// ccMutable is the mutable-collab freshness contract (issue #280;
	// docs/go/07_api.md §4 third class): profiles, orgs, members, and
	// teams mutate via direct user action, so every GET revalidates on
	// EVERY read instead of serving a stale-while-revalidate window. The
	// version-keyed ETags keep the revalidation cheap (304 when
	// unchanged); collection/singleton GETs without a version token take
	// the class without an ETag (always 200, never stale).
	ccMutable = "private, no-cache"
	ccNoStore = "no-store"
)

func readBodyJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// --- top-level routes (/api/v1 + /api-browser/v1 twins) --------------------

func (h *Handler) handleTop(w http.ResponseWriter, r *http.Request, rest []string) bool {
	if len(rest) == 0 || rest[0] != "v1" {
		return false
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "users":
		return h.routeUsers(w, r, rest[1:])
	case "orgs":
		return h.routeOrgs(w, r, rest[1:])
	case "invitations":
		return h.routeInvites(w, r, rest[1:])
	}
	return false
}

// routeUsers: GET/PUT /api/v1/users/{principal},
// GET/POST/DELETE /api/v1/users/{principal}/avatar (Forgejo #376).
func (h *Handler) routeUsers(w http.ResponseWriter, r *http.Request, rest []string) bool {
	if len(rest) == 0 || rest[0] == "" {
		return false
	}
	principal := normPrincipal(rest[0])
	if !ValidPrincipal(principal) {
		writePlain(w, http.StatusBadRequest, "invalid principal")
		return true
	}
	if len(rest) == 2 && rest[1] == "avatar" {
		return h.routeUserAvatar(w, r, principal)
	}
	if len(rest) != 1 {
		return false
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		prof, err := h.Svc.GetProfile(r.Context(), principal)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if prof == nil {
			writePlain(w, http.StatusNotFound, "unknown principal")
			return true
		}
		// Forgejo #370: the verified email is visible to its owner
		// alone. A username subject resolves via the registry; a
		// legacy email spelling IS the address (the caller matched
		// it, so it is theirs). Every other view omits it.
		if !p.Anonymous && matchPrincipal(principal, p) {
			if auth.ValidUsername(principal) {
				if em, emerr := h.Svc.EmailForUsername(r.Context(), principal); emerr == nil {
					prof.Email = em
				}
			} else {
				prof.Email = principal
			}
		}
		writeCached(w, r, ccMutable, etagOf("user", prof.Version), http.StatusOK, prof)
		return true
	case http.MethodPut:
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		if !p.Admin && normPrincipal(p.Name) != principal {
			writePlain(w, http.StatusForbidden, "self or admin")
			return true
		}
		var body struct {
			DisplayName string `json:"display_name"`
			Bio         string `json:"bio"`
		}
		if !readBodyJSON(w, r, 64<<10, &body) {
			return true
		}
		prof, err := h.Svc.PutProfile(r.Context(), principal, body.DisplayName, body.Bio)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, prof)
		return true
	}
	methodNotAllowed(w, "GET", "PUT")
	return true
}

// routeUserAvatar: GET/POST/DELETE /api/v1/users/{principal}/avatar
// (Forgejo #376). GET is public (same anonymous-read rule as the
// profile itself) and serves the generated SVG with the sniffed
// Content-Type; POST regenerates (explicit opt-back-in, self-or-admin);
// DELETE removes the avatar and opts out (self-or-admin — the same
// gate as PUT on the profile).
func (h *Handler) routeUserAvatar(w http.ResponseWriter, r *http.Request, principal string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		raw, prof, err := h.Svc.GetUserAvatar(r.Context(), principal)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if raw == nil || prof == nil {
			writePlain(w, http.StatusNotFound, "no avatar")
			return true
		}
		hdr := w.Header()
		hdr.Set("Content-Type", prof.AvatarContentType)
		hdr.Set("Content-Length", strconv.Itoa(len(raw)))
		// Immutable-until-regenerate: the pointer's updated_at changes
		// on every install, and clients cache-bust with
		// ?v=<avatar_updated_at> (UserAvatarURL), so a long max-age
		// is safe (the #359 org-avatar reasoning). The ETag names the
		// same version for cheap revalidation.
		hdr.Set("Cache-Control", "public, max-age=86400, immutable")
		etag := "user-avatar-" + prof.AvatarUpdatedAt
		hdr.Set("ETag", `"`+etag+`"`)
		if matchETag(r.Header.Get("If-None-Match"), etag) {
			hdr.Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return true
	case http.MethodPost, http.MethodDelete:
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		if !p.Admin && normPrincipal(p.Name) != principal {
			writePlain(w, http.StatusForbidden, "self or admin")
			return true
		}
		var prof *Profile
		var err error
		if r.Method == http.MethodDelete {
			prof, err = h.Svc.DeleteUserAvatar(r.Context(), principal)
		} else {
			prof, err = h.Svc.RegenerateUserAvatar(r.Context(), principal)
		}
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, prof)
		return true
	}
	methodNotAllowed(w, "GET", "POST", "DELETE")
	return true
}

// routeOrgs: /api/v1/orgs[/...].
func (h *Handler) routeOrgs(w http.ResponseWriter, r *http.Request, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			p, aerr := h.principal(r)
			if aerr != nil {
				writeErr(w, aerr)
				return true
			}
			if p.Anonymous && !h.Svc.anonymousRead() {
				writePlain(w, http.StatusUnauthorized, "authentication required")
				return true
			}
			orgs, err := h.Svc.ListOrgs(r.Context())
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccMutable, "", http.StatusOK, orgs)
			return true
		case http.MethodPost:
			p, aerr := h.principal(r)
			if aerr != nil {
				writeErr(w, aerr)
				return true
			}
			if p.Anonymous {
				writePlain(w, http.StatusUnauthorized, "authentication required")
				return true
			}
			if !p.Write && !p.Admin {
				writePlain(w, http.StatusForbidden, "write access required")
				return true
			}
			var body struct {
				Org         string `json:"org"`
				DisplayName string `json:"display_name"`
				Description string `json:"description"`
			}
			if !readBodyJSON(w, r, 64<<10, &body) {
				return true
			}
			org, err := h.Svc.CreateOrg(r.Context(), strings.ToLower(strings.TrimSpace(body.Org)), body.DisplayName, body.Description, p.Name)
			if err != nil {
				writeErr(w, err)
				return true
			}
			// Best-effort: auth-none's "anon" is not an email and has no
			// profile; invitation/role paths validate their own subjects.
			if ValidPrincipal(p.Name) {
				if _, err := h.Svc.EnsureProfile(r.Context(), normPrincipal(p.Name)); err != nil {
					writeErr(w, err)
					return true
				}
			}
			writeCached(w, r, ccNoStore, "", http.StatusCreated, map[string]string{"org": org.Org})
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	org := strings.ToLower(rest[0])
	if !ValidOrg(org) {
		writePlain(w, http.StatusNotFound, "not found")
		return true
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return h.routeOrg(w, r, org)
	}
	switch rest[0] {
	case "avatar":
		return h.routeOrgAvatar(w, r, org, rest[1:])
	case "members":
		return h.routeMembers(w, r, org, rest[1:])
	case "teams":
		return h.routeTeams(w, r, org, rest[1:])
	case "invitations":
		return h.routeOrgInvites(w, r, org, rest[1:])
	}
	return false
}

// routeOrg: GET/PUT/DELETE /api/v1/orgs/{org}.
func (h *Handler) routeOrg(w http.ResponseWriter, r *http.Request, org string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		o, err := h.Svc.GetOrg(r.Context(), org)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if o == nil {
			writePlain(w, http.StatusNotFound, "unknown org")
			return true
		}
		writeCached(w, r, ccMutable, etagOf("org", o.Version), http.StatusOK, o)
		return true
	case http.MethodPut, http.MethodDelete:
		if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		if r.Method == http.MethodDelete {
			if err := h.Svc.DeleteOrg(r.Context(), org); err != nil {
				writeErr(w, err)
				return true
			}
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		var body struct {
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
			Location    string `json:"location"`
			Timezone    string `json:"timezone"`
			BioMarkdown string `json:"bio_markdown"`
		}
		if !readBodyJSON(w, r, 128<<10, &body) {
			return true
		}
		o, err := h.Svc.PutOrg(r.Context(), org, OrgEdit{
			DisplayName: body.DisplayName,
			Description: body.Description,
			Location:    body.Location,
			Timezone:    body.Timezone,
			BioMarkdown: body.BioMarkdown,
		})
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, o)
		return true
	}
	methodNotAllowed(w, "GET", "PUT", "DELETE")
	return true
}

// routeOrgAvatar: GET/PUT/DELETE /api/v1/orgs/{org}/avatar (Forgejo
// #359). GET is public (same anonymous-read rule as the org itself) and
// serves the raw bytes with the sniffed Content-Type; PUT/DELETE are
// owner-only via CheckOrgOwner (the same gate as PUT/DELETE on the org).
func (h *Handler) routeOrgAvatar(w http.ResponseWriter, r *http.Request, org string, rest []string) bool {
	if len(rest) != 0 {
		return false
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		raw, ct, err := h.Svc.GetOrgAvatar(r.Context(), org)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if raw == nil {
			writePlain(w, http.StatusNotFound, "no avatar")
			return true
		}
		hdr := w.Header()
		hdr.Set("Content-Type", ct)
		hdr.Set("Content-Length", strconv.Itoa(len(raw)))
		// Immutable-until-PUT: the pointer's updated_at changes on every
		// upload, and the UI cache-busts with ?v=<avatar_updated_at>, so
		// a long max-age is safe (same reasoning as hashed UI assets).
		hdr.Set("Cache-Control", "public, max-age=86400, immutable")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return true
	case http.MethodPut, http.MethodDelete:
		if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		if r.Method == http.MethodDelete {
			o, err := h.Svc.DeleteOrgAvatar(r.Context(), org)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusOK, o)
			return true
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOrgAvatarBytes+1))
		if err != nil {
			writePlain(w, http.StatusRequestEntityTooLarge, "avatar too large")
			return true
		}
		if int64(len(raw)) > maxOrgAvatarBytes {
			writePlain(w, http.StatusRequestEntityTooLarge, "avatar too large")
			return true
		}
		o, verr := h.Svc.PutOrgAvatar(r.Context(), org, raw)
		if verr != nil {
			writeErr(w, verr)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, o)
		return true
	}
	methodNotAllowed(w, "GET", "PUT", "DELETE")
	return true
}

// routeMembers: GET /orgs/{org}/members (collection, read) and
// GET/PUT/DELETE /orgs/{org}/members/{principal}.
func (h *Handler) routeMembers(w http.ResponseWriter, r *http.Request, org string, rest []string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		m, err := h.Svc.GetMembers(r.Context(), org)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if m == nil {
			writePlain(w, http.StatusNotFound, "unknown org")
			return true
		}
		writeCached(w, r, ccMutable, etagOf("members", m.Version), http.StatusOK, m)
		return true
	}
	if len(rest) != 1 {
		return false
	}
	target := normPrincipal(rest[0])
	if !ValidPrincipal(target) {
		writePlain(w, http.StatusBadRequest, "invalid principal")
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		m, err := h.Svc.GetMembers(r.Context(), org)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if m == nil {
			writePlain(w, http.StatusNotFound, "unknown org")
			return true
		}
		for _, e := range m.Members {
			if normPrincipal(e.Principal) == target {
				writeCached(w, r, ccMutable, "", http.StatusOK, e)
				return true
			}
		}
		writePlain(w, http.StatusNotFound, "not a member")
		return true
	case http.MethodPut, http.MethodDelete:
		if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		if r.Method == http.MethodDelete {
			m, err := h.Svc.RemoveMember(r.Context(), org, target)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusOK, m)
			return true
		}
		var body struct {
			Role string `json:"role"`
		}
		if !readBodyJSON(w, r, 64<<10, &body) {
			return true
		}
		m, err := h.Svc.SetMember(r.Context(), org, target, OrgRole(strings.ToLower(body.Role)))
		if err != nil {
			writeErr(w, err)
			return true
		}
		if _, err := h.Svc.EnsureProfile(r.Context(), target); err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, m)
		return true
	}
	methodNotAllowed(w, "GET", "PUT", "DELETE")
	return true
}

// routeTeams: GET/POST /orgs/{org}/teams, GET/PUT/DELETE
// /orgs/{org}/teams/{slug}, PUT/DELETE .../members/{principal}.
func (h *Handler) routeTeams(w http.ResponseWriter, r *http.Request, org string, rest []string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			if p.Anonymous && !h.Svc.anonymousRead() {
				writePlain(w, http.StatusUnauthorized, "authentication required")
				return true
			}
			n := 100
			if q := r.URL.Query().Get("n"); q != "" {
				if v, verr := strconv.Atoi(q); verr == nil && v > 0 && v <= 1000 {
					n = v
				}
			}
			teams, err := h.Svc.ListTeams(r.Context(), org, n)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccMutable, "", http.StatusOK, teams)
			return true
		case http.MethodPost:
			if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
				writeErr(w, cerr)
				return true
			}
			var body struct {
				Slug        string `json:"slug"`
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if !readBodyJSON(w, r, 64<<10, &body) {
				return true
			}
			t, err := h.Svc.CreateTeam(r.Context(), org, strings.ToLower(strings.TrimSpace(body.Slug)), body.Name, body.Description)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusCreated, t)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	slug := strings.ToLower(rest[0])
	if !ValidSlug(slug) {
		writePlain(w, http.StatusNotFound, "not found")
		return true
	}
	rest = rest[1:]
	if len(rest) >= 2 && rest[0] == "members" && len(rest) == 2 {
		target := normPrincipal(rest[1])
		if !ValidPrincipal(target) {
			writePlain(w, http.StatusBadRequest, "invalid principal")
			return true
		}
		if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		switch r.Method {
		case http.MethodPut:
			t, err := h.Svc.SetTeamMember(r.Context(), org, slug, target)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusOK, t)
			return true
		case http.MethodDelete:
			t, err := h.Svc.RemoveTeamMember(r.Context(), org, slug, target)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusOK, t)
			return true
		}
		methodNotAllowed(w, "PUT", "DELETE")
		return true
	}
	if len(rest) != 0 {
		return false
	}
	switch r.Method {
	case http.MethodGet:
		if p.Anonymous && !h.Svc.anonymousRead() {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		t, _, err := h.Svc.GetTeam(r.Context(), org, slug)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if t == nil {
			writePlain(w, http.StatusNotFound, "unknown team")
			return true
		}
		writeCached(w, r, ccMutable, etagOf("team", t.Version), http.StatusOK, t)
		return true
	case http.MethodPut, http.MethodDelete:
		if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		if r.Method == http.MethodDelete {
			if err := h.Svc.DeleteTeam(r.Context(), org, slug); err != nil {
				writeErr(w, err)
				return true
			}
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if !readBodyJSON(w, r, 64<<10, &body) {
			return true
		}
		t, err := h.Svc.PutTeam(r.Context(), org, slug, body.Name, body.Description)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, t)
		return true
	}
	methodNotAllowed(w, "GET", "PUT", "DELETE")
	return true
}

func etagOf(prefix string, version int) string {
	return prefix + "-v" + strconv.Itoa(version)
}
