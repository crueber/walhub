// profile_authz.go — the OwnerEditor seam for owner profiles (Forgejo #234).
//
// api.OwnerEditor is defined in internal/api (this package never imports api,
// law 8); the composition wires *Service in as apiEnv.OwnerEdit (cmd/walhub).
// Core already grants host admins and name-matched principals; this adds the
// org-owner role: a principal holding role owner in orgs/<org>/members.json
// may PUT owners/<org>/profile.json.
//
// Non-org namespaces (no members.json) return false and let the core default
// decide. Probe errors (store down, corrupt roster) return err — the caller
// fails closed with 503, never 403-as-404 (the #210 creategate rule).
//
// ### Concurrency
// Hazard: none new — one exact-key roster GET per PUT (human-rate path,
// never hot; same cost class as the P6 team-expansion probes). No lock is
// held across the store call; the roster object is read whole, so a
// concurrent membership edit is either seen or takes effect on the next
// request — never torn.
package identity

import (
	"context"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// CanEditOwnerProfile implements the org-owner half of the profile PUT rule.
func (s *Service) CanEditOwnerProfile(ctx context.Context, owner string, p auth.Principal) (bool, error) {
	if s == nil || s.Store == nil {
		return false, nil
	}
	m, _, err := s.getMembers(ctx, owner)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	want := normPrincipal(p.Name)
	for _, e := range m.Members {
		if e.Role == OrgOwner && normPrincipal(e.Principal) == want {
			return true, nil
		}
	}
	return false, nil
}
