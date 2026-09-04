package identity

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func TestCheckRead(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	mustAccess(t, s, "acme", "priv", VisibilityPrivate, []AccessBinding{
		{Subject: "user:bob@example.com", Role: RoleWrite},
	})
	mustAccess(t, s, "acme", "pub", VisibilityPublic, nil)

	cases := []struct {
		name  string
		repo  string
		p     auth.Principal
		allow bool
		kind  auth.AuthErrorKind
	}{
		{"host admin", "priv", admin, true, 0},
		{"host write flag", "priv", writer, true, 0},
		{"bound writer", "priv", bob, true, 0},
		{"org owner", "priv", alice, true, 0},
		{"stranger private", "priv", stranger, false, auth.ErrForbidden},
		{"anon private", "priv", anon, false, auth.ErrUnauthorized},
		{"stranger public", "pub", stranger, true, 0},
		{"anon public", "pub", anon, true, 0},
		{"anon synthesized public", "newrepo", anon, true, 0},
		{"stranger synthesized", "newrepo", stranger, true, 0},
	}
	for _, tc := range cases {
		aerr := s.CheckRead(ctx, "acme", tc.repo, tc.p)
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
	// Host anonymous_read=false denies anon even on public repos.
	s.Cfg.Server.Auth.AnonymousRead = false
	if aerr := s.CheckRead(ctx, "acme", "pub", anon); aerr == nil || aerr.Kind != auth.ErrUnauthorized {
		t.Errorf("anon with anonymous_read=false: %v", aerr)
	}
	// ...but an authenticated public reader still passes.
	if aerr := s.CheckRead(ctx, "acme", "pub", stranger); aerr != nil {
		t.Errorf("authed public read: %v", aerr)
	}
}

func TestCheckRole(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	mustAccess(t, s, "acme", "r", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
		{Subject: "user:carol@example.com", Role: RoleTriage},
	})
	cases := []struct {
		name string
		p    auth.Principal
		want Role
		deny bool
		una  bool
	}{
		{"admin flag bypasses", admin, RoleAdmin, false, false},
		{"org owner is admin", alice, RoleAdmin, false, false},
		{"team write passes write", bob, RoleWrite, false, false},
		{"team write fails maintain", bob, RoleMaintain, true, false},
		{"triage passes triage", carol, RoleTriage, false, false},
		{"triage fails write", carol, RoleWrite, true, false},
		{"stranger fails read", stranger, RoleRead, true, false},
		{"anon fails read", anon, RoleRead, true, true},
	}
	for _, tc := range cases {
		aerr := s.CheckRole(ctx, "acme", "r", tc.p, tc.want)
		if !tc.deny && aerr != nil {
			t.Errorf("%s: denied: %v", tc.name, aerr)
		}
		if tc.deny {
			if aerr == nil {
				t.Errorf("%s: allowed", tc.name)
			} else if tc.una && aerr.Kind != auth.ErrUnauthorized {
				t.Errorf("%s: kind=%v want unauthorized", tc.name, aerr.Kind)
			} else if !tc.una && aerr.Kind != auth.ErrForbidden {
				t.Errorf("%s: kind=%v want forbidden", tc.name, aerr.Kind)
			}
		}
	}
}

func TestCheckOrgOwner(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	if aerr := s.CheckOrgOwner(ctx, "acme", admin); aerr != nil {
		t.Errorf("host admin: %v", aerr)
	}
	if aerr := s.CheckOrgOwner(ctx, "acme", alice); aerr != nil {
		t.Errorf("owner: %v", aerr)
	}
	if aerr := s.CheckOrgOwner(ctx, "acme", bob); aerr == nil || aerr.Kind != auth.ErrForbidden {
		t.Errorf("member: %v", aerr)
	}
	if aerr := s.CheckOrgOwner(ctx, "acme", anon); aerr == nil || aerr.Kind != auth.ErrUnauthorized {
		t.Errorf("anon: %v", aerr)
	}
}

func TestExpandMembers(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	mustAccess(t, s, "acme", "repo", VisibilityPrivate, []AccessBinding{
		{Subject: "user:carol@example.com", Role: RoleMaintain},
		{Subject: "team:acme/platform", Role: RoleWrite},
	})
	expanded, warnings := s.ExpandMembers(ctx, []string{
		"user:x@y.z",
		"team:acme/platform",
		"role:acme/repo:write",
		"team:acme/ghost",
		"team:bad",
		"role:acme/repo:super",
		"role:badshape",
		"group:admins",
	})
	if len(warnings) != 4 {
		t.Errorf("warnings = %v", warnings)
	}
	want := map[string]bool{
		"user:x@y.z": true, "bob@example.com": true, "carol@example.com": true,
		"alice@example.com": true, "group:admins": true,
	}
	for _, e := range expanded {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Errorf("missing expansions %v (got %v)", want, expanded)
	}
	// role: with an email owner synthesizes the admin binding.
	exp2, w2 := s.ExpandMembers(ctx, []string{"role:jane@example.com/repo:admin"})
	if len(w2) != 0 {
		t.Errorf("synthesis warnings: %v", w2)
	}
	if len(exp2) != 1 || exp2[0] != "jane@example.com" {
		t.Errorf("synthesis expansion = %v", exp2)
	}
	// role: with erroring store warns.
	sErr := testService()
	sErr.Store = &errStore{ObjectStore: sErr.Store, getErr: errBoom}
	if _, w := sErr.ExpandMembers(ctx, []string{"role:o/r:read"}); len(w) != 1 {
		t.Errorf("store-error expansion warnings = %v", w)
	}
	// ExpandGroups satisfies policy.Expander.
	ex, w := s.PolicyExpander().ExpandGroups(ctx, []string{"team:acme/platform"})
	if len(w) != 0 || len(ex) != 1 || ex[0] != "bob@example.com" {
		t.Errorf("PolicyExpander: %v %v", ex, w)
	}
}

func mustAccess(t *testing.T, s *Service, owner, repo string, vis Visibility, b []AccessBinding) {
	t.Helper()
	if _, err := s.PutAccess(context.Background(), owner, repo, "", vis, b); err != nil {
		t.Fatalf("PutAccess %s/%s: %v", owner, repo, err)
	}
}
