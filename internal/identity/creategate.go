// creategate.go — the explicit create-repo admission seam (Forgejo #346):
// the creation/import owner-binding rule and the eager access.json
// default.
//
// The admission helper is injected into internal/api as the
// CreateOwnerGate interface and into internal/repoimport as the
// RoleService.CheckCreateOwner method (both defined there — this package
// never imports api/repoimport/mirror, law 8). api.Env.CreateOwnerGate,
// repoimport checkCreate, and the mirror create-from-URL twin all consult
// the ONE rule below; the #347 push guardrail reuses it verbatim (see the
// reuse contract on CheckCreateOwner).
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
	"fmt"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// CheckCreateOwner is the single creation/import admission rule (Forgejo
// #346): a logged-in principal may create or import a repo under owner iff
// the owner is their own username, an org they belong to (any roster role —
// v1 member-may-create; tighten to owner-only if a future decision says
// so), or they are a host admin (global-admin bypass, documented). Anything
// else fails closed: anonymous → ErrUnauthorized (real 401, so clients erase
// creds — law 9); a bound principal naming a foreign owner → ErrForbidden
// naming the allowed owners (their username + member orgs); a roster probe
// failure → ErrUnavailable (callers answer 503, never 403-as-404).
//
// Callers invoke it BEFORE any namespace write (before allocNum/manifest
// create — a deny allocates no counter, writes no manifest, leaves no
// partial state). It costs one exact-key members.json GET on the
// non-self, non-admin path (human-rate create/import only, never hot),
// plus a LIST + per-org probes on the DENY path only (law 6: verification
// rides the failure path) to name the allowed orgs in the 403.
//
// ### Reuse contract (Forgejo #347)
//
// The push guardrail reuses this helper verbatim: a push that would
// auto-create a repo gates the owner segment of the pushed path through
// CheckCreateOwner BEFORE creating (same 403 message shape), and SSH/HTTP
// receive-pack keeps this as the create-side rule while adding its own
// repo-scoped write check for existing repos. #347 must not fork the rule —
// any policy change (e.g. member-may-create → owner-only) lands here once
// and both gates inherit it.
//
// ### Concurrency
// Hazard: a roster edit racing the admission probe. Avoidance: the probe is
// a single whole-object read; a concurrent demotion affects the next
// request, never tears this one. No lock is held across any store call.
func (s *Service) CheckCreateOwner(ctx context.Context, owner string, p auth.Principal) *auth.AuthError {
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if p.Admin {
		return nil
	}
	if normPrincipal(owner) == normPrincipal(p.Name) {
		return nil
	}
	m, _, gerr := s.getMembers(ctx, owner)
	if gerr != nil {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "org membership unavailable"}
	}
	if m != nil {
		for _, e := range m.Members {
			if matchPrincipal(e.Principal, p) {
				return nil
			}
		}
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: s.ownerDenyMessage(ctx, owner, p)}
}

// ownerDenyMessage names the allowed owners for the 403: the principal's
// own username plus their member orgs (deny-path only cost). A roster LIST
// failure degrades to the generic shape — the deny itself never depends on
// the enumeration. The message carries the username only — never the
// email (leak axis, fail closed).
func (s *Service) ownerDenyMessage(ctx context.Context, owner string, p auth.Principal) string {
	self := normPrincipal(p.Name)
	orgs, err := s.MemberOrgsFor(ctx, p)
	if err != nil || len(orgs) == 0 {
		return fmt.Sprintf("owner %q not permitted: use %q or an org you belong to", owner, self)
	}
	return fmt.Sprintf("owner %q not permitted: use %q or one of your orgs (%s)", owner, self, strings.Join(orgs, ", "))
}

// MemberOrgs returns the sorted names of every org whose roster contains
// principal (any role). Collaboration/UI support for the #346 admission
// rule (the 403 message above; the New/Import owner dropdowns resolve
// their options server-side through the orgs surface). Cost is one LIST
// over orgs/ plus one exact-key members.json GET per org — human-rate
// callers only (deny messages, form loads), never a git hot path.
func (s *Service) MemberOrgs(ctx context.Context, principal string) ([]string, error) {
	return s.MemberOrgsFor(ctx, auth.Principal{Name: principal})
}

// MemberOrgsFor is MemberOrgs over a full principal: stored email
// spellings match the username principal carrying that email (the #370
// migration alias).
func (s *Service) MemberOrgsFor(ctx context.Context, p auth.Principal) ([]string, error) {
	orgs, err := s.ListOrgs(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, org := range orgs {
		m, _, gerr := s.getMembers(ctx, org)
		if gerr != nil {
			return nil, gerr
		}
		if m == nil {
			continue
		}
		for _, e := range m.Members {
			if matchPrincipal(e.Principal, p) {
				out = append(out, org)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
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
// binding — user: subjects are usernames and such a binding fails subject
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
	// The creator binding is a GRANT: only for user namespaces (a
	// registry-backed username, or a legacy email). Synthetic
	// principals, orgs, and unclaimed names materialize a
	// visibility-only doc (see isUserNamespace).
	if s.isUserNamespace(ctx, creator) {
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
