// creategate.go — the explicit create-repo placeholder seams (Forgejo #210,
// R1 B5/S2): the org-membership gate and the eager access.json default.
//
// Both methods are injected into internal/api as the OrgGate /
// AccessBootstrap interfaces (defined there — this package never imports
// api, law 8). api.Env.OrgGate = identity service structurally.
//
// ### Concurrency
// Hazard: mutating access.json under a concurrent admin edit. Avoidance:
// Create-wins, adopt-don't-overwrite (PutCreate; 412 = someone raced us —
// adopt, never overwrite or CAS-loop). A racing PUT that wins first leaves
// a valid doc; the placeholder page synthesizes deterministically either
// way. No lock is held across any store call.
package identity

import (
	"context"

	"git.packden.us/crueber/walhub/internal/store"
)

// IsOrgMember implements the #210 §3 org-namespace create gate: one
// exact-key GET of orgs/<org>/members.json (same cost class as the P6 team
// expansion probes — human-rate path, never hot).
//
//   - exists=false → the owner prefix is unclaimed; today's open behavior
//     persists (back-compat).
//   - exists=true, member=false → the caller answers 403.
//   - probe errors (store down, corrupt roster) → err; the caller answers
//     503, never 403-as-404 (S2 TOCTOU rule).
func (s *Service) IsOrgMember(ctx context.Context, org, principal string) (exists, member bool, err error) {
	m, _, gerr := s.getMembers(ctx, org)
	if gerr != nil {
		return false, false, gerr
	}
	if m == nil {
		return false, false, nil
	}
	want := normPrincipal(principal)
	for _, e := range m.Members {
		if normPrincipal(e.Principal) == want {
			return true, true, nil
		}
	}
	return true, false, nil
}

// EnsureRepoAccess materializes the 01 §10 synthesized default eagerly at
// placeholder creation (visibility + [{subject:"user:<creator>",
// role:"admin"}]), so the placeholder page has a deterministic visibility +
// an admin for the Danger-Zone delete. Uses the SAME Create-with-synthesis
// writer shape as 01 §10 Concurrency (412 = someone raced us — adopt, don't
// overwrite).
//
// visibility is the create request's visibility ("public"|"private"; "" from
// the PUT-flag path means the public default). Unknown spellings fall back
// to public — creation never fails on a visibility paraphrase (the POST
// twin 400s unknown spellings before this runs).
//
// Auth-none behavior (R1 B5): no eager "user:anonymous"/"user:anon"
// binding — user: subjects are emails and such a binding fails subject
// validation. When the creator is not a valid principal, materialize a
// visibility-only doc (no bindings); when even that races, adopt. Callers
// with no store-backed need pass a nil Store at their own risk (no-op).
func (s *Service) EnsureRepoAccess(ctx context.Context, owner, repo, creator, visibility string) error {
	if s == nil || s.Store == nil {
		return nil
	}
	vis := VisibilityPublic
	if visibility == string(VisibilityPrivate) {
		vis = VisibilityPrivate
	}
	doc := &AccessDoc{Version: 1, Visibility: vis, RoleBindings: []AccessBinding{}}
	if ValidPrincipal(creator) {
		doc.RoleBindings = append(doc.RoleBindings, AccessBinding{
			Subject: "user:" + normPrincipal(creator),
			Role:    RoleAdmin,
		})
	}
	raw := encodeAccess(doc)
	_, err := s.Store.Put(ctx, AccessKey(owner, repo), store.PutBody{Bytes: raw},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err != nil && store.IsPreconditionFailed(err) {
		return nil // 412 = someone raced us — adopt, don't overwrite
	}
	if err != nil && store.IsNotFound(err) {
		return nil // backend quirk on missing prefix — synthesis covers reads
	}
	return err
}
