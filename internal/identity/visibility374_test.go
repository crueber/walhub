package identity

// visibility374_test.go — Forgejo #374: the visibility modes split.
//
// Resolution matrix over (visibility × owner-type × principal-class),
// pinned through CheckRead (the require_read hook) with Resolve role
// spot-checks. Owner types: org-owned (acme, roster alice=owner +
// bob=member) vs user-owned (solo, no org object). Principals: anonymous,
// authed stranger, host-write-only outsider (writer), org member (bob),
// org owner (alice), owner-self (solo), bound outsider (reader), host
// admin.
//
// Rules under test:
//   - public: everyone reads (anonymous included).
//   - authenticated: any authenticated principal reads; anonymous 401.
//   - private org-owned: roster members read WITHOUT bindings; non-members
//     (even host-write-only) 403; owners admin; host admin passes.
//   - private user-owned: owner binding + explicit bindings + host admin
//     only (fail closed — no binding, no read, even for the owner).
//   - The host-wide write flag grants nothing on private repos (the #347
//     direction extended to reads); it reads public/authenticated repos
//     through visibility like any authenticated principal.

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func seedVisibility374(t *testing.T, s *Service) {
	t.Helper()
	seedOrg(t, s)
	// Org-owned: no bindings — roster + visibility decide alone.
	mustAccess(t, s, "acme", "pub", VisibilityPublic, nil)
	mustAccess(t, s, "acme", "auth", VisibilityAuthenticated, nil)
	mustAccess(t, s, "acme", "priv", VisibilityPrivate, nil)
	// User-owned: solo is not an org (no roster). The private repo
	// carries the EnsureRepoAccess-shaped owner binding; auth/pub carry
	// none; shared carries a per-repo outsider binding only.
	mustAccess(t, s, "solo", "pub", VisibilityPublic, nil)
	mustAccess(t, s, "solo", "auth", VisibilityAuthenticated, nil)
	mustAccess(t, s, "solo", "priv", VisibilityPrivate, []AccessBinding{
		{Subject: "user:solo", Role: RoleAdmin},
	})
	mustAccess(t, s, "solo", "shared", VisibilityPrivate, []AccessBinding{
		{Subject: "user:reader", Role: RoleRead},
	})
}

func TestVisibility374Matrix(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedVisibility374(t, s)
	solo := authPrincipal("solo")
	reader := authPrincipal("reader")
	cases := []struct {
		name  string
		owner string
		repo  string
		p     auth.Principal
		allow bool
		kind  auth.AuthErrorKind
	}{
		// --- org-owned, public: everyone reads.
		{"org public anon", "acme", "pub", anon, true, 0},
		{"org public stranger", "acme", "pub", stranger, true, 0},
		{"org public writer", "acme", "pub", writer, true, 0},
		{"org public member", "acme", "pub", bob, true, 0},
		{"org public owner", "acme", "pub", alice, true, 0},
		{"org public admin", "acme", "pub", admin, true, 0},
		// --- org-owned, authenticated: any signed-in user reads.
		{"org auth anon", "acme", "auth", anon, false, auth.ErrUnauthorized},
		{"org auth stranger", "acme", "auth", stranger, true, 0},
		{"org auth writer", "acme", "auth", writer, true, 0},
		{"org auth member", "acme", "auth", bob, true, 0},
		{"org auth owner", "acme", "auth", alice, true, 0},
		{"org auth admin", "acme", "auth", admin, true, 0},
		// --- org-owned, private: members read without bindings;
		// host-write-only outsiders lose (the #374 decision).
		{"org priv anon", "acme", "priv", anon, false, auth.ErrUnauthorized},
		{"org priv stranger", "acme", "priv", stranger, false, auth.ErrForbidden},
		{"org priv writer", "acme", "priv", writer, false, auth.ErrForbidden},
		{"org priv member", "acme", "priv", bob, true, 0},
		{"org priv owner", "acme", "priv", alice, true, 0},
		{"org priv admin", "acme", "priv", admin, true, 0},
		// --- user-owned, public / authenticated.
		{"user public anon", "solo", "pub", anon, true, 0},
		{"user public stranger", "solo", "pub", stranger, true, 0},
		{"user public owner", "solo", "pub", solo, true, 0},
		{"user auth anon", "solo", "auth", anon, false, auth.ErrUnauthorized},
		{"user auth stranger", "solo", "auth", stranger, true, 0},
		{"user auth writer", "solo", "auth", writer, true, 0},
		{"user auth owner", "solo", "auth", solo, true, 0},
		{"user auth admin", "solo", "auth", admin, true, 0},
		// --- user-owned, private: owner binding + explicit bindings +
		// host admin only.
		{"user priv anon", "solo", "priv", anon, false, auth.ErrUnauthorized},
		{"user priv stranger", "solo", "priv", stranger, false, auth.ErrForbidden},
		{"user priv writer", "solo", "priv", writer, false, auth.ErrForbidden},
		{"user priv owner", "solo", "priv", solo, true, 0},
		{"user priv bound-elsewhere", "solo", "priv", reader, false, auth.ErrForbidden},
		{"user priv admin", "solo", "priv", admin, true, 0},
		// --- per-repo bindings: reader reads shared, owner does not
		// (fail closed without a binding, even for the owner).
		{"user shared reader", "solo", "shared", reader, true, 0},
		{"user shared owner-unbound", "solo", "shared", solo, false, auth.ErrForbidden},
		{"user shared stranger", "solo", "shared", stranger, false, auth.ErrForbidden},
		{"user shared admin", "solo", "shared", admin, true, 0},
	}
	for _, tc := range cases {
		aerr := s.CheckRead(ctx, tc.owner, tc.repo, tc.p)
		if tc.allow && aerr != nil {
			t.Errorf("%s: denied: %v", tc.name, aerr)
			continue
		}
		if !tc.allow {
			if aerr == nil {
				t.Errorf("%s: allowed, want deny", tc.name)
			} else if aerr.Kind != tc.kind {
				t.Errorf("%s: kind=%v want %v", tc.name, aerr.Kind, tc.kind)
			}
		}
		// Denied anonymous reads are real 401s (law 9 — git erases the
		// credential), never in-band 200s.
		if !tc.allow && tc.kind == auth.ErrUnauthorized && aerr != nil && aerr.Kind != auth.ErrUnauthorized {
			t.Errorf("%s: anon deny kind=%v want unauthorized", tc.name, aerr.Kind)
		}
	}
}

func TestVisibility374ResolveRoles(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedVisibility374(t, s)
	solo := authPrincipal("solo")
	cases := []struct {
		name  string
		owner string
		repo  string
		p     auth.Principal
		want  Role
	}{
		{"member reads private org repo", "acme", "priv", bob, RoleRead},
		{"owner admins private org repo", "acme", "priv", alice, RoleAdmin},
		{"write-only outsider resolves nothing private", "acme", "priv", writer, ""},
		{"stranger reads authenticated", "acme", "auth", stranger, RoleRead},
		{"write-only outsider reads authenticated", "acme", "auth", writer, RoleRead},
		{"anon reads nothing authenticated", "acme", "auth", anon, ""},
		{"authed stranger resolves nothing public", "acme", "pub", stranger, ""},
		{"anon reads public", "acme", "pub", anon, RoleRead},
		{"owner binding admins user private", "solo", "priv", solo, RoleAdmin},
		{"write-only outsider resolves nothing user-private", "solo", "priv", writer, ""},
		{"host admin resolves admin anywhere", "solo", "priv", admin, RoleAdmin},
	}
	for _, tc := range cases {
		role, _ := s.Resolve(ctx, tc.owner, tc.repo, tc.p)
		if role != tc.want {
			t.Errorf("%s: role=%q want %q", tc.name, role, tc.want)
		}
	}
}

func TestVisibility374RosterMembership(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	// Owners count as members (ANY roster role); non-members, anonymous,
	// and non-org owners do not.
	for name, tc := range map[string]struct {
		p    auth.Principal
		want bool
	}{
		"owner":    {alice, true},
		"member":   {bob, true},
		"stranger": {stranger, false},
		"writer":   {writer, false},
		"anon":     {anon, false},
	} {
		if got := s.isOrgMemberFor(ctx, "acme", tc.p); got != tc.want {
			t.Errorf("%s member=%v want %v", name, got, tc.want)
		}
	}
	if s.isOrgMemberFor(ctx, "solo", bob) {
		t.Error("non-org owner must never report membership")
	}
	if s.isOrgMemberFor(ctx, "ghost", bob) {
		t.Error("missing org must never report membership")
	}
	// isOrgOwnerFor still distinguishes owners from members.
	if !s.isOrgOwnerFor(ctx, "acme", alice) {
		t.Error("alice must still be org owner")
	}
	if s.isOrgOwnerFor(ctx, "acme", bob) {
		t.Error("bob must not be org owner")
	}
}

func TestVisibility374ParseAndMaterialize(t *testing.T) {
	s := testService()
	ctx := context.Background()
	// normalizeAccess accepts the new spelling and still rejects junk.
	if _, _, err := normalizeAccess("authenticated", nil); err != nil {
		t.Errorf("authenticated must validate: %v", err)
	}
	for _, bad := range []string{"", "members-only", "INTERNAL", "PUBLIC"} {
		if _, _, err := normalizeAccess(bad, nil); err == nil {
			t.Errorf("visibility %q must be invalid", bad)
		}
	}
	// PutAccess round-trips the new value; projections report it.
	if _, err := s.PutAccess(ctx, "acme", "auth", "", VisibilityAuthenticated, nil); err != nil {
		t.Fatalf("PutAccess authenticated: %v", err)
	}
	if vis, ok := s.RepoVisibility(ctx, "acme", "auth"); vis != VisibilityAuthenticated || !ok {
		t.Errorf("RepoVisibility = %q,%v want authenticated,true", vis, ok)
	}
	// EnsureRepoAccess materializes the requested spelling.
	if err := s.EnsureRepoAccess(ctx, "solo", "newauth", "solo", "authenticated"); err != nil {
		t.Fatalf("EnsureRepoAccess: %v", err)
	}
	doc, _, err := s.GetAccess(ctx, "solo", "newauth")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Visibility != VisibilityAuthenticated {
		t.Errorf("materialized visibility = %q, want authenticated", doc.Visibility)
	}
	// CheckRole: member read reaches read but not triage; a write-only
	// outsider no longer reaches even read on a private repo.
	seedOrg(t, s)
	mustAccess(t, s, "acme", "priv", VisibilityPrivate, nil)
	if aerr := s.CheckRole(ctx, "acme", "priv", bob, RoleRead); aerr != nil {
		t.Errorf("member CheckRole(read): %v", aerr)
	}
	if aerr := s.CheckRole(ctx, "acme", "priv", bob, RoleTriage); aerr == nil {
		t.Error("member CheckRole(triage) must deny")
	}
	if aerr := s.CheckRole(ctx, "acme", "priv", writer, RoleRead); aerr == nil {
		t.Error("write-only outsider CheckRole(read) on private must deny")
	}
}
