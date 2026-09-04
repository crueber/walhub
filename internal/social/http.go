package social

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Wire conventions (07 §2, same as internal/api and internal/notify):
// JSON success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, SWR+ETag on JSON GETs, both lanes everywhere.
// Anonymous-denied reads get a real 401 with WWW-Authenticate: Bearer.

// Handler is the Seam 1 surface: star/watch-adjacent social endpoints on
// both lanes plus the top-level starred twins. Composition chains it in
// front of the core api mux: Handle reports false for non-social paths so
// the core mux answers.
//
// Watch MUTATION stays in internal/notify (06 §6 is landed and tested —
// single HTTP owner; social.json converges through the field-scoped CAS
// loops). This handler serves star/unstar, GET social, and the starred
// lists; it only READS watch records (viewer flags).
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// principal resolves the request principal via the injected Authenticator
// (Seam 2); nil Authenticator falls back to anonymous (production always
// injects the server chain).
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a social route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) >= 3 && (segs[0] == "api" || segs[0] == "api-browser") && segs[1] == "v1" {
		switch {
		case len(segs) == 4 && segs[2] == "me" && segs[3] == "starred":
			h.myStarred(w, r)
			return true
		case len(segs) == 5 && segs[2] == "users" && segs[4] == "starred":
			h.userStarred(w, r, segs[3])
			return true
		}
		return false
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		if len(segs[3:]) == 1 && (segs[3] == "star" || segs[3] == "social") {
			h.handleRepo(w, r, owner, repo, segs[3])
			return true
		}
	}
	return false
}

// ServeHTTP answers social routes and 404s otherwise (httptest surface).
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
		out = append(out, decodeSegment(s))
	}
	return out
}

// decodeSegment decodes one path segment; an undecodable segment survives
// verbatim (fail closed downstream).
func decodeSegment(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return d
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
		writePlain(w, http.StatusInternalServerError, "social: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", ccNoStore)
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
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "social: encode: "+err.Error())
		return
	}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
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
	ccSWR     = "private, max-age=0, stale-while-revalidate=60"
	ccNoStore = "no-store"
)

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// --- repo routes ------------------------------------------------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo, leaf string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	switch leaf {
	case "star":
		switch r.Method {
		case http.MethodPut, http.MethodDelete:
			h.star(w, r, owner, repo, p, r.Method == http.MethodPut)
		default:
			methodNotAllowed(w, "PUT", "DELETE")
		}
	case "social":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.social(w, r, owner, repo, p)
	}
}

// star serves PUT/DELETE /{o}/{r}/api/star (idempotent both ways → {stars}).
// PUT needs authenticated + repo-visible; DELETE needs authenticated only
// (unstar must always work, §4).
func (h *Handler) star(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, on bool) {
	var (
		count int
		err   error
	)
	if on {
		count, err = h.Svc.Star(r.Context(), p, owner, repo)
	} else {
		count, err = h.Svc.Unstar(r.Context(), p, owner, repo)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stars": count})
}

// social serves GET /{o}/{r}/api/social →
// {stars, watchers, forks, viewer: {starred, watching}} (SWR+ETag).
func (h *Handler) social(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	d, err := h.Svc.Counts(r.Context(), p, owner, repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	starred, watching := h.Svc.ViewerState(r.Context(), p, owner, repo)
	writeCached(w, r, ccSWR, socialETag(d), http.StatusOK, map[string]any{
		"stars": d.Stars, "watchers": d.Watchers, "forks": d.Forks,
		"viewer": map[string]any{"starred": starred, "watching": watching},
	})
}

// socialETag folds the counters + updated_at into one token.
func socialETag(d SocialDoc) string {
	return strconv.Itoa(d.Stars) + "." + strconv.Itoa(d.Watchers) + "." +
		strconv.Itoa(d.Forks) + "." + d.UpdatedAt
}

// --- top-level starred twins -------------------------------------------------

// myStarred serves GET /api/v1/me/starred (authenticated, no-store —
// per-user private read).
func (h *Handler) myStarred(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if p.Anonymous {
		writePlain(w, http.StatusUnauthorized, "authentication required")
		return
	}
	h.starredList(w, r, normPrincipal(p.Name))
}

// userStarred serves GET /api/v1/users/{principal}/starred (read-level:
// public info, no private-read filtering per Decisions).
func (h *Handler) userStarred(w http.ResponseWriter, r *http.Request, principal string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	h.starredList(w, r, principal)
}

func (h *Handler) starredList(w http.ResponseWriter, r *http.Request, principal string) {
	q := r.URL.Query()
	n := ListDefaultPage
	if s := q.Get("n"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			writePlain(w, http.StatusBadRequest, "invalid n")
			return
		}
		n = v
	}
	entries, more, err := h.Svc.Starred(r.Context(), principal, n, q.Get("after"))
	if err != nil {
		writeErr(w, err)
		return
	}
	wire := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		wire = append(wire, map[string]any{"repo": e.Repo, "starred_at": e.StarredAt})
	}
	hdr := w.Header()
	hdr.Set("Cache-Control", ccNoStore)
	writeJSON(w, http.StatusOK, map[string]any{"starred": wire, "more": more})
}
