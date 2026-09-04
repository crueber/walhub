package identity

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- top-level invitations ---------------------------------------------------

// routeInvites: GET /api/v1/invitations (mine), GET/POST/DELETE
// /api/v1/invitations/{id}[/accept].
func (h *Handler) routeInvites(w http.ResponseWriter, r *http.Request, rest []string) bool {
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
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		entries, err := h.Svc.MyInvites(r.Context(), normPrincipal(p.Name))
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, entries)
		return true
	}
	id := rest[0]
	if id == "" || len(rest) > 2 {
		return false
	}
	if len(rest) == 2 && rest[1] != "accept" {
		return false
	}
	isAccept := len(rest) == 2
	switch r.Method {
	case http.MethodGet:
		if isAccept {
			methodNotAllowed(w, "POST")
			return true
		}
		// Preview requires authentication (the authed subject or the link
		// token authorizes it); the binding write always follows the
		// authed POST. Anonymous link holders log in first.
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		token := r.URL.Query().Get("token")
		inv, err := h.Svc.PreviewInvite(r.Context(), normPrincipal(p.Name), id, token)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, inviteSummary(inv))
		return true
	case http.MethodPost:
		if !isAccept {
			methodNotAllowed(w, "GET", "DELETE")
			return true
		}
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		bound, err := h.Svc.AcceptInvite(r.Context(), normPrincipal(p.Name), id)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, map[string]string{"bound": bound})
		return true
	case http.MethodDelete:
		if isAccept {
			methodNotAllowed(w, "GET", "DELETE")
			return true
		}
		if p.Anonymous {
			writePlain(w, http.StatusUnauthorized, "authentication required")
			return true
		}
		if err := h.cancelInvite(w, r, normPrincipal(p.Name), p, id); err != nil {
			writeErr(w, err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	methodNotAllowed(w, "GET", "POST", "DELETE")
	return true
}

// cancelInvite authorizes the top-level DELETE: the invitee (decline).
// Issuers cancel via the scoped endpoints (org/repo admin DELETE), which
// authorize by scope — the top-level route can only locate invites through
// the caller's own inbox.
func (h *Handler) cancelInvite(w http.ResponseWriter, r *http.Request, principal string, p auth.Principal, id string) error {
	_ = w
	_ = p
	inv, err := h.Svc.findInvite(r.Context(), principal, id)
	if err != nil {
		return err
	}
	if normPrincipal(inv.Subject) != principal {
		return ErrForbidden
	}
	_, cerr := h.Svc.CancelInvite(r.Context(), principal, id)
	return cerr
}

// routeOrgInvites: POST /api/v1/orgs/{org}/invitations (owner), the
// additive GET collection (owner), and DELETE …/{id} (owner cancel).
func (h *Handler) routeOrgInvites(w http.ResponseWriter, r *http.Request, org string, rest []string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if cerr := h.Svc.CheckOrgOwner(r.Context(), org, p); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	if len(rest) == 1 {
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE")
			return true
		}
		raw, _, gerr := store.GetBytes(r.Context(), h.Svc.Store, OrgInviteKey(org, rest[0]), store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				writePlain(w, http.StatusNotFound, "unknown invitation")
				return true
			}
			writeErr(w, gerr)
			return true
		}
		inv, perr := parseInvite(raw)
		if perr != nil {
			writeErr(w, perr)
			return true
		}
		if derr := h.Svc.Store.Delete(r.Context(), OrgInviteKey(org, rest[0]), ""); derr != nil && !store.IsNotFound(derr) {
			writeErr(w, derr)
			return true
		}
		_ = h.Svc.inboxRemove(r.Context(), normPrincipal(inv.Subject), rest[0])
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if len(rest) != 0 {
		return false
	}
	switch r.Method {
	case http.MethodGet:
		invs, err := h.Svc.ListOrgInvites(r.Context(), org, pageSize(r))
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, inviteSummaries(invs))
		return true
	case http.MethodPost:
		var body struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if !readBodyJSON(w, r, 64<<10, &body) {
			return true
		}
		inv, err := h.Svc.CreateOrgInvite(r.Context(), org, body.Email, strings.ToLower(body.Role), p.Name, 7*24*time.Hour)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusCreated, map[string]string{"id": inv.ID, "accept_url": acceptURL(inv)})
		return true
	}
	methodNotAllowed(w, "GET", "POST")
	return true
}

// --- repo-scoped routes (/{o}/{r}/api + /api-browser twin) ------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "access":
		if len(rest) != 1 {
			return false
		}
		return h.routeAccess(w, r, owner, repo)
	case "invitations":
		return h.routeRepoInvites(w, r, owner, repo, rest[1:])
	}
	return false
}

// routeAccess: GET (triage) / PUT (admin) …/access.
func (h *Handler) routeAccess(w http.ResponseWriter, r *http.Request, owner, repo string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if cerr := h.Svc.CheckRole(r.Context(), owner, repo, p, RoleTriage); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		doc, ver, err := h.Svc.GetAccess(r.Context(), owner, repo)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccSWR, etagOf("access", doc.Version)+"-"+string(ver), http.StatusOK, accessView(doc))
		return true
	case http.MethodPut:
		if cerr := h.Svc.CheckRole(r.Context(), owner, repo, p, RoleAdmin); cerr != nil {
			writeErr(w, cerr)
			return true
		}
		var body struct {
			Version      int             `json:"version"`
			Visibility   string          `json:"visibility"`
			RoleBindings []AccessBinding `json:"role_bindings"`
		}
		if !readBodyJSON(w, r, 1<<20, &body) {
			return true
		}
		if body.RoleBindings == nil {
			body.RoleBindings = []AccessBinding{}
		}
		cur, curVer, err := h.Svc.GetAccess(r.Context(), owner, repo)
		if err != nil {
			writeErr(w, err)
			return true
		}
		if cur.Version != body.Version {
			writePlain(w, http.StatusConflict, "access.json changed under you; reload")
			return true
		}
		doc, err := h.Svc.PutAccess(r.Context(), owner, repo, curVer, Visibility(body.Visibility), body.RoleBindings)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeCached(w, r, ccNoStore, "", http.StatusOK, map[string]int{"version": doc.Version})
		return true
	}
	methodNotAllowed(w, "GET", "PUT")
	return true
}

// routeRepoInvites: POST/GET …/invitations (admin), DELETE …/invitations/{id}.
func (h *Handler) routeRepoInvites(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if cerr := h.Svc.CheckRole(r.Context(), owner, repo, p, RoleAdmin); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			invs, err := h.Svc.ListRepoInvites(r.Context(), owner, repo, pageSize(r))
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusOK, inviteSummaries(invs))
			return true
		case http.MethodPost:
			var body struct {
				Subject string `json:"subject"`
				Role    string `json:"role"`
			}
			if !readBodyJSON(w, r, 64<<10, &body) {
				return true
			}
			inv, err := h.Svc.CreateRepoInvite(r.Context(), owner, repo, body.Subject, Role(strings.ToLower(body.Role)), p.Name, 7*24*time.Hour)
			if err != nil {
				writeErr(w, err)
				return true
			}
			writeCached(w, r, ccNoStore, "", http.StatusCreated, map[string]string{"id": inv.ID, "accept_url": acceptURL(inv)})
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	if len(rest) != 1 {
		return false
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, "DELETE")
		return true
	}
	id := rest[0]
	raw, _, gerr := store.GetBytes(r.Context(), h.Svc.Store, RepoInviteKey(owner, repo, id), store.GetOptions{})
	if gerr != nil {
		if store.IsNotFound(gerr) {
			writePlain(w, http.StatusNotFound, "unknown invitation")
			return true
		}
		writeErr(w, gerr)
		return true
	}
	inv, perr := parseInvite(raw)
	if perr != nil {
		writeErr(w, perr)
		return true
	}
	if derr := h.Svc.Store.Delete(r.Context(), RepoInviteKey(owner, repo, id), ""); derr != nil && !store.IsNotFound(derr) {
		writeErr(w, derr)
		return true
	}
	_ = h.Svc.inboxRemove(r.Context(), normPrincipal(inv.Subject), id)
	w.WriteHeader(http.StatusNoContent)
	return true
}

// accessView renders the GET …/access shape with []-never-null.
func accessView(doc *AccessDoc) map[string]any {
	return map[string]any{
		"version":       doc.Version,
		"visibility":    string(doc.Visibility),
		"role_bindings": nonNilBindings(doc.RoleBindings),
	}
}

// inviteSummary renders one invite with the token redacted.
func inviteSummary(inv *Invitation) map[string]any {
	return map[string]any{
		"id": inv.ID, "kind": inv.Kind, "org": inv.Org, "repo": inv.Repo,
		"role": inv.Role, "subject": inv.Subject, "invited_by": inv.InvitedBy,
		"state": inv.State, "created_at": inv.CreatedAt, "expires_at": inv.ExpiresAt,
	}
}

func inviteSummaries(invs []*Invitation) []map[string]any {
	out := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		out = append(out, inviteSummary(inv))
	}
	return out
}

// pageSize parses the ?n= page cap shared by the LIST-backed invite
// collections (P5; default 100, max 1000 — the team-list convention).
func pageSize(r *http.Request) int {
	n := 100
	if q := r.URL.Query().Get("n"); q != "" {
		if v, verr := strconv.Atoi(q); verr == nil && v > 0 && v <= 1000 {
			n = v
		}
	}
	return n
}

// acceptURL is the emailed-link path the UI renders.
func acceptURL(inv *Invitation) string {
	return "/api/v1/invitations/" + inv.ID + "?token=" + inv.Token
}
