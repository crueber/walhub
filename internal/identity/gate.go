package identity

import (
	"context"
	"strings"

	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// principalKey carries the authenticated principal in the request context
// for the identity handler (set by composition after Seam 2 resolution).
type principalKey struct{}

// WithPrincipal attaches the authenticated identity to ctx.
func WithPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// CheckRead is the require_read hook (01 §4.1), consulted after principal
// resolution and before any handler body on the git read path, LFS reads,
// and every repo-scoped read endpoint:
//
//  1. authenticated principal → resolve per §4; role ≥ read → allow; else 403.
//  2. anonymous → access.json visibility public AND host anonymous_read →
//     allow, else 401 (WWW-Authenticate: Bearer — git must erase the
//     credential, 06 §8.4).
//
// A host admin/write flag always allows (P6 step 3); a missing access.json
// synthesizes the legacy default (§10).
func (s *Service) CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	if p.Admin || p.Write {
		return nil
	}
	role, doc := s.Resolve(ctx, owner, repo, p)
	if p.Anonymous {
		if doc.Visibility == VisibilityPublic && s.anonymousRead() {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if role.atLeast(RoleRead) {
		return nil
	}
	if doc.Visibility == VisibilityPublic {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
}

// CheckRole enforces a minimum repo role for the identity surface: host
// admin always passes; otherwise the P6 resolution must reach want. The
// mapping is the §5 matrix entry point (triage for access reads, admin for
// access writes and invites, owner for org writes).
func (s *Service) CheckRole(ctx context.Context, owner, repo string, p auth.Principal, want Role) *auth.AuthError {
	if p.Admin {
		return nil
	}
	role, _ := s.Resolve(ctx, owner, repo, p)
	if role.atLeast(want) {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "insufficient role: need " + string(want)}
}

// CheckOrgOwner enforces org ownership (or host admin) for org writes.
func (s *Service) CheckOrgOwner(ctx context.Context, org string, p auth.Principal) *auth.AuthError {
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if s.isOrgOwner(ctx, org, normPrincipal(p.Name)) {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "org owner required"}
}

// --- Seam 3: team:/role: group expansion (01 §6) ------------------------------
//
// policy.json stays the frozen envelope; effects are untouched. The
// amendment is to group member resolution: a groups[].members entry MAY be
// "team:<org>/<slug>" (union of the team roster) or
// "role:<owner>/<repo>:<role>" (principals holding ≥ role per §4, org
// owners included). Expansion happens at policy load under the existing
// per-repo policy cache single-flight; Evaluate stays pure and local.
// A reference that fails to resolve is a parse-time warning evaluating to
// the empty set (fail-closed for protect semantics).

// ExpandMembers resolves team:/role: spellings against identity state.
// Other spellings pass through untouched. Warnings name unresolvable
// references (they evaluate to the empty set).
func (s *Service) ExpandMembers(ctx context.Context, members []string) (expanded []string, warnings []string) {
	expanded = make([]string, 0, len(members))
	for _, m := range members {
		if team, ok := strings.CutPrefix(m, "team:"); ok {
			org, slug, ok := strings.Cut(team, "/")
			if !ok || !ValidOrg(org) || !ValidSlug(slug) {
				warnings = append(warnings, "unresolvable team reference "+m)
				continue
			}
			t, _, err := s.GetTeam(ctx, org, slug)
			if err != nil || t == nil {
				warnings = append(warnings, "unresolvable team reference "+m)
				continue
			}
			expanded = append(expanded, t.Members...)
			continue
		}
		if ref, ok := strings.CutPrefix(m, "role:"); ok {
			principals, w := s.expandRole(ctx, ref)
			expanded = append(expanded, principals...)
			if w != "" {
				warnings = append(warnings, w)
			}
			continue
		}
		expanded = append(expanded, m)
	}
	return expanded, warnings
}

// expandRole resolves role:<owner>/<repo>:<role> to principals holding at
// least that role: direct bindings, team members of team bindings, and org
// owners of the owning org.
func (s *Service) expandRole(ctx context.Context, ref string) ([]string, string) {
	rest, roleName, ok := strings.Cut(ref, ":")
	if !ok || !validRole(roleName) {
		return nil, "unresolvable role reference role:" + ref
	}
	owner, repo, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" {
		return nil, "unresolvable role reference role:" + ref
	}
	doc, _, err := s.GetAccess(ctx, owner, repo)
	if err != nil {
		return nil, "unresolvable role reference role:" + ref
	}
	want := Role(roleName)
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = normPrincipal(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, b := range doc.RoleBindings {
		if !b.Role.atLeast(want) {
			continue
		}
		if sub, ok := strings.CutPrefix(b.Subject, "user:"); ok {
			add(sub)
		} else if team, ok := strings.CutPrefix(b.Subject, "team:"); ok {
			// Subjects are validated at write and re-validated on read,
			// so team: always carries org/slug here.
			org, slug, _ := strings.Cut(team, "/")
			t, _, terr := s.GetTeam(ctx, org, slug)
			if terr != nil || t == nil {
				continue
			}
			for _, m := range t.Members {
				add(m)
			}
		}
	}
	if m, _, merr := s.getMembers(ctx, owner); merr == nil && m != nil {
		for _, e := range m.Members {
			if e.Role == OrgOwner {
				add(e.Principal)
			}
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, ""
}

// PolicyExpander adapts Service to policy.Expander (Seam 3 load-time
// expansion; Evaluate stays pure).
func (s *Service) PolicyExpander() policy.Expander { return s }

// ExpandGroups implements policy.Expander.
func (s *Service) ExpandGroups(ctx context.Context, members []string) ([]string, []string) {
	return s.ExpandMembers(ctx, members)
}
