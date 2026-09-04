// http_perms.go — Feature 08 §§3.6/5: the permission-gating surface.
//
// GET /{o}/{r}/api/permissions → {role} (P6 resolution server-side; the
// UI reads one resolved role instead of re-implementing the order).
// Anonymous resolves to {role: null}, or "read" when anonymous_read
// admits them — exactly what Resolve returns.
// GET /{o}/{r}/api/collaborators → effective bindings + resolution
// source (direct | team:<org>/<slug> | org-owner).
// GET /{o}/{r}/api/assignables → [{principal, display}] (repo
// collaborators ∪ org members; the mentions autocomplete source).
//
// All three are read-gated (CheckRead: anonymous admitted only under
// anonymous_read + public visibility; otherwise a real 401, never a 200
// with an in-band error).
package identity

import (
	"net/http"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// routePerms answers the three gating endpoints; false when the path is
// not one of them. Every branch is GET-only.
func (h *Handler) routePerms(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) != 1 || r.Method != http.MethodGet {
		if len(rest) == 1 && (rest[0] == "permissions" || rest[0] == "collaborators" || rest[0] == "assignables") {
			methodNotAllowed(w, "GET")
			return true
		}
		return false
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if cerr := h.Svc.CheckRead(r.Context(), owner, repo, p); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	switch rest[0] {
	case "permissions":
		h.servePermissions(w, r, owner, repo, p)
	case "collaborators":
		h.serveCollaborators(w, r, owner, repo)
	case "assignables":
		h.serveAssignables(w, r, owner, repo)
	default:
		return false
	}
	return true
}

// servePermissions renders {role} (null when the caller holds no role).
func (h *Handler) servePermissions(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	role, doc := h.Svc.Resolve(r.Context(), owner, repo, p)
	var v any
	if role == "" {
		v = map[string]any{"role": nil}
	} else {
		v = map[string]any{"role": string(role)}
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, v)
	_ = doc
}

// collaborator is one effective binding with its resolution source.
type collaborator struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	Source    string `json:"source"`
}

// serveCollaborators renders the effective bindings: direct user
// bindings, team bindings expanded to members, and org owners at admin.
// []-never-null; sorted by principal.
func (h *Handler) serveCollaborators(w http.ResponseWriter, r *http.Request, owner, repo string) {
	ctx := r.Context()
	doc, _, err := h.Svc.GetAccess(ctx, owner, repo)
	if err != nil {
		doc = SynthesizeDefault(owner)
	}
	out := []collaborator{}
	seen := map[string]bool{}
	add := func(principal, role, source string) {
		principal = normPrincipal(principal)
		if principal == "" || seen[principal+"\x00"+role] {
			return
		}
		seen[principal+"\x00"+role] = true
		out = append(out, collaborator{Principal: principal, Role: role, Source: source})
	}
	for _, b := range doc.RoleBindings {
		if sub, ok := strings.CutPrefix(b.Subject, "user:"); ok {
			add(sub, string(b.Role), "direct")
		} else if team, ok := strings.CutPrefix(b.Subject, "team:"); ok {
			org, slug, ok := strings.Cut(team, "/")
			if !ok {
				continue
			}
			t, _, terr := h.Svc.GetTeam(ctx, org, slug)
			if terr != nil || t == nil {
				continue
			}
			for _, m := range t.Members {
				add(m, string(b.Role), "team:"+team)
			}
		}
	}
	if m, _, merr := h.Svc.getMembers(ctx, owner); merr == nil && m != nil {
		for _, e := range m.Members {
			if e.Role == OrgOwner {
				add(e.Principal, string(RoleAdmin), "org-owner")
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Principal != out[j].Principal {
			return out[i].Principal < out[j].Principal
		}
		return out[i].Role < out[j].Role
	})
	writeCached(w, r, ccNoStore, "", http.StatusOK, map[string]any{"collaborators": out})
}

// assignable is one mentions-autocomplete entry.
type assignable struct {
	Principal string `json:"principal"`
	Display   string `json:"display"`
}

// serveAssignables renders repo collaborators ∪ org members (principals
// sorted, deduped, []-never-null). Display prefers the profile display
// name; unknown profiles fall back to the principal.
func (h *Handler) serveAssignables(w http.ResponseWriter, r *http.Request, owner, repo string) {
	ctx := r.Context()
	set := map[string]bool{}
	doc, _, err := h.Svc.GetAccess(ctx, owner, repo)
	if err != nil {
		doc = SynthesizeDefault(owner)
	}
	for _, b := range doc.RoleBindings {
		if sub, ok := strings.CutPrefix(b.Subject, "user:"); ok {
			set[normPrincipal(sub)] = true
		} else if team, ok := strings.CutPrefix(b.Subject, "team:"); ok {
			org, slug, ok := strings.Cut(team, "/")
			if !ok {
				continue
			}
			if t, _, terr := h.Svc.GetTeam(ctx, org, slug); terr == nil && t != nil {
				for _, m := range t.Members {
					set[normPrincipal(m)] = true
				}
			}
		}
	}
	if m, _, merr := h.Svc.getMembers(ctx, owner); merr == nil && m != nil {
		for _, e := range m.Members {
			set[normPrincipal(e.Principal)] = true
		}
	}
	out := make([]assignable, 0, len(set))
	for principal := range set {
		if principal == "" {
			continue
		}
		display := principal
		if prof, perr := h.Svc.GetProfile(ctx, principal); perr == nil && prof != nil && prof.DisplayName != "" {
			display = prof.DisplayName
		}
		out = append(out, assignable{Principal: principal, Display: display})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Principal < out[j].Principal })
	writeCached(w, r, ccNoStore, "", http.StatusOK, map[string]any{"assignables": out})
}
