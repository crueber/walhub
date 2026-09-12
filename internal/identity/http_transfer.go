package identity

import (
	"net/http"
	"strings"
)

// routeTransfer answers POST /{owner}/{repo}/api/transfer (both lanes):
// direct owner-initiated repo transfer (Forgejo #358, v1 — no accept
// step). The caller must hold repo admin on the source (CheckRole admin:
// binding, source-org ownership, or host admin — the source ownership
// check) and pass CheckCreateOwner admission on the destination (the #346
// rule, verbatim). Success is 201 {owner, repo} at the new address.
//
// Anonymous callers get a real 401 (CheckRole maps anonymous-denied to
// 401 — law 9); unresolvable destination orgs 404; an occupied
// destination 409.
func (h *Handler) routeTransfer(w http.ResponseWriter, r *http.Request, owner, repo string) bool {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return true
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if cerr := h.Svc.CheckRole(r.Context(), owner, repo, p, RoleAdmin); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	var body struct {
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
	}
	if !readBodyJSON(w, r, 64<<10, &body) {
		return true
	}
	dstOwner := strings.TrimSpace(body.Owner)
	dstRepo := strings.TrimSpace(body.Repo)
	if dstOwner == "" {
		writePlain(w, http.StatusBadRequest, "destination owner required")
		return true
	}
	if dstRepo == "" {
		dstRepo = repo
	}
	if cerr := h.Svc.CheckCreateOwner(r.Context(), dstOwner, p); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	if err := h.Svc.TransferRepo(r.Context(), owner, repo, dstOwner, dstRepo); err != nil {
		writeErr(w, err)
		return true
	}
	writeCached(w, r, ccNoStore, "", http.StatusCreated, map[string]string{"owner": dstOwner, "repo": dstRepo})
	return true
}
