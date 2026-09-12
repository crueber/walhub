package identity

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestCheckPushMatrix pins the #347 repo-scoped push rule: owner,
// org-owner, team-bound member, and explicitly bound writers pass; a
// host-wide write flag alone (alice/bob ALL carry Write:true in this
// package's fixtures — the flag must not decide) grants nothing on a repo
// the principal has no relationship with; anonymous is 401; host admin
// bypasses.
func TestCheckPushMatrix(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	mustAccess(t, s, "acme", "r", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
		{Subject: "user:carol@example.com", Role: RoleTriage},
	})
	mustAccess(t, s, "acme", "r2", VisibilityPrivate, []AccessBinding{
		{Subject: "user:carol@example.com", Role: RoleWrite},
		{Subject: "user:dave@example.com", Role: RoleMaintain},
	})
	jane := authPrincipal("jane@example.com")

	cases := []struct {
		name  string
		owner string
		repo  string
		p     auth.Principal
		allow bool
		kind  auth.AuthErrorKind
	}{
		// Owner pushes to her own repo (synthesized default admin —
		// no access.json written for jane/r).
		{"owner self", "jane@example.com", "r", jane, true, 0},
		// Slug-namespace self (the #346 self rule mirrored — no
		// binding exists for a slug owner, yet the user owns it).
		{"slug owner self", "solo", "r", authPrincipal("solo"), true, 0},
		{"slug foreigner denied", "solo", "r", stranger, false, auth.ErrForbidden},
		// Org owner on an org repo (P6 step 2, no binding needed).
		{"org owner", "acme", "r", alice, true, 0},
		// Org member reaching write through the team binding.
		{"team-bound member", "acme", "r", bob, true, 0},
		// Explicit user binding reaching write / maintain.
		{"explicit write binding", "acme", "r2", carol, true, 0},
		{"explicit maintain binding", "acme", "r2", dave, true, 0},
		// Host admin bypasses on a foreign repo.
		{"host admin bypass", "acme", "r", admin, true, 0},
		// Auth-none anon carries admin: zero-config pushes pass.
		{"auth-none anon bypass", "acme", "r", noneP, true, 0},
		// THE FIX: host-wide write alone grants nothing repo-scoped.
		// writer carries Write:true and no relationship to acme/r.
		{"host-write-only foreigner denied", "acme", "r", writer, false, auth.ErrForbidden},
		{"stranger denied", "acme", "r", stranger, false, auth.ErrForbidden},
		// Triage binding does not reach write.
		{"triage denied", "acme", "r", carol, false, auth.ErrForbidden},
		// Anonymous fails closed with a real 401 (law 9).
		{"anonymous denied", "acme", "r", anon, false, auth.ErrUnauthorized},
		{"anonymous on missing repo denied", "ghost", "newrepo", anon, false, auth.ErrUnauthorized},
	}
	for _, tc := range cases {
		aerr := s.CheckPush(ctx, tc.owner, tc.repo, tc.p)
		if tc.allow && aerr != nil {
			t.Errorf("%s: denied: %v", tc.name, aerr)
		}
		if !tc.allow {
			if aerr == nil {
				t.Errorf("%s: allowed, want deny", tc.name)
			} else if aerr.Kind != tc.kind {
				t.Errorf("%s: kind=%v want %v", tc.name, aerr.Kind, tc.kind)
			}
		}
	}
}

func TestCheckPushDenyNamesRepo(t *testing.T) {
	s := testService()
	ctx := context.Background()
	aerr := s.CheckPush(ctx, "other", "repo", stranger)
	if aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Fatalf("deny = %v, want forbidden", aerr)
	}
	if !strings.Contains(aerr.Why, `"other/repo"`) {
		t.Errorf("deny must name the repo, got %q", aerr.Why)
	}
	if !strings.Contains(aerr.Why, "owner") {
		t.Errorf("deny must name the required relationship, got %q", aerr.Why)
	}
}
