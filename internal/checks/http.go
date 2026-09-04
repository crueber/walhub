package checks

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

// Wire conventions (07 §2, same as internal/api and the sibling feature
// packages): JSON success, plain-text errors, arrays [] never null, RFC
// 3339 UTC, per-segment decoding, no-store on every checks GET (statuses
// mutate in place, so neither cache class of 07 §9.2 applies —
// sha-addressed does NOT mean immutable here), both lanes everywhere.
// Anonymous-denied reads get a real 401 with WWW-Authenticate: Bearer
// (never a 200 with an in-band error).

// Handler is the Seam 1 surface: every §4 endpoint on both lanes.
// Composition chains it in front of the core mux: Handle reports false for
// non-checks paths so the core mux answers (the Wave A amendment in
// 14_extensibility.md Decisions — the `server.ExtraRoutes` chain, exactly
// like internal/identity, internal/issues, internal/pulls, and
// internal/review).
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// principal resolves the request principal via the injected Authenticator
// (Seam 2: the wct_ shape hook + the server chain); nil Authenticator
// falls back to anonymous (production always injects the wrapped chain).
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a checks route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if segs[0] == "api" || segs[0] == "api-browser" {
		return false // no top-level checks routes; all are repo-scoped
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

// ServeHTTP answers checks routes and 404s otherwise (httptest surface).
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
// verbatim (fail closed downstream: it won't match a sha or id shape).
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
		writePlain(w, http.StatusInternalServerError, "checks: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeStrict unmarshals body into v after rejecting unknown top-level
// keys (fail closed: unknown keys on write are 400, on read ignored).
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, allowed map[string]bool, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if keys == nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: expected an object")
		return false
	}
	for k := range keys {
		if !allowed[k] {
			writePlain(w, http.StatusBadRequest, "unknown field "+strconv.Quote(k))
			return false
		}
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

// --- routing (§4) ------------------------------------------------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 || rest[0] != "checks" {
		return false
	}
	rest = rest[1:]
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.listChecks(w, r, owner, repo)
		return true
	}
	switch rest[0] {
	case "statuses":
		if len(rest) != 2 {
			return false
		}
		switch r.Method {
		case http.MethodGet:
			h.getStatuses(w, r, owner, repo, rest[1])
			return true
		case http.MethodPost:
			h.reportStatus(w, r, owner, repo, rest[1])
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	case "tokens":
		if len(rest) == 1 {
			switch r.Method {
			case http.MethodGet:
				h.listTokens(w, r, owner, repo)
				return true
			case http.MethodPost:
				h.createToken(w, r, owner, repo)
				return true
			}
			methodNotAllowed(w, "GET", "POST")
			return true
		}
		if len(rest) == 2 {
			if r.Method != http.MethodDelete {
				methodNotAllowed(w, "DELETE")
				return true
			}
			h.revokeToken(w, r, owner, repo, rest[1])
			return true
		}
		return false
	default:
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getCombined(w, r, owner, repo, rest[0])
		return true
	}
}

// --- handlers ------------------------------------------------------------------

func (h *Handler) reportStatus(w http.ResponseWriter, r *http.Request, owner, repo, sha string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Context     string  `json:"context"`
		State       string  `json:"state"`
		TargetURL   string  `json:"target_url"`
		Description string  `json:"description"`
		StartedAt   *string `json:"started_at"`
		CompletedAt *string `json:"completed_at"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{
		"context": true, "state": true, "target_url": true,
		"description": true, "started_at": true, "completed_at": true,
	}, &body) {
		return
	}
	_, secret, _ := CISecretOf(r)
	st, err := h.Svc.ReportStatus(r.Context(), owner, repo, sha, p, secret, ReportInput{
		Context: body.Context, State: body.State, TargetURL: body.TargetURL,
		Description: body.Description, StartedAt: body.StartedAt, CompletedAt: body.CompletedAt,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) getStatuses(w http.ResponseWriter, r *http.Request, owner, repo, sha string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.GetStatuses(r.Context(), owner, repo, sha, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) getCombined(w http.ResponseWriter, r *http.Request, owner, repo, sha string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.Combined(r.Context(), owner, repo, sha, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) listChecks(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	after := q.Get("after")
	if after != "" {
		if _, err := normalizeSHA(after); err != nil {
			writePlain(w, http.StatusBadRequest, "invalid after: must be a full 40/64-hex sha")
			return
		}
	}
	n := 50
	if v := q.Get("n"); v != "" {
		parsed, cerr := strconv.Atoi(v)
		if cerr != nil || parsed < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n: must be a positive integer")
			return
		}
		if parsed > 200 {
			writePlain(w, http.StatusBadRequest, "invalid n: max 200")
			return
		}
		n = parsed
	}
	page, err := h.Svc.ListChecks(r.Context(), owner, repo, p, after, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"name": true, "scopes": true}, &body) {
		return
	}
	created, err := h.Svc.CreateToken(r.Context(), owner, repo, p, body.Name, body.Scopes)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	tokens, err := h.Svc.ListTokens(r.Context(), owner, repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.RevokeToken(r.Context(), owner, repo, id, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
