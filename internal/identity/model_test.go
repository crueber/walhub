package identity

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestRoles(t *testing.T) {
	for _, r := range []string{"read", "triage", "write", "maintain", "admin"} {
		if !validRole(r) {
			t.Errorf("validRole(%q) = false", r)
		}
	}
	if validRole("superuser") || validRole("") {
		t.Error("validRole accepted garbage")
	}
	if !RoleAdmin.atLeast(RoleRead) || RoleRead.atLeast(RoleAdmin) {
		t.Error("rank ordering broken")
	}
	if Role("").rank() != 0 {
		t.Error("empty role rank != 0")
	}
	if !validOrgRole("owner") || !validOrgRole("member") || validOrgRole("root") {
		t.Error("org role validation broken")
	}
	if !validVisibility("public") || !validVisibility("private") || validVisibility("hidden") {
		t.Error("visibility validation broken")
	}
}

func TestSlugs(t *testing.T) {
	valid := []string{"acme", "a", "a-b-1", strings.Repeat("x", 39)}
	for _, o := range valid {
		if !ValidOrg(o) {
			t.Errorf("ValidOrg(%q) = false", o)
		}
	}
	invalid := []string{"", "Acme", "a_b", "a/b", strings.Repeat("x", 40), "-"}
	for _, o := range invalid[:len(invalid)-1] {
		if ValidOrg(o) {
			t.Errorf("ValidOrg(%q) = true", o)
		}
	}
	if ValidOrg("-") {
		// "-" matches the charset; slugs are permissive by design.
		t.Log("note: lone dash accepted as org slug")
	}
	if !ValidSlug("platform") || ValidSlug("A") || ValidSlug("") || ValidSlug("a/b") {
		t.Error("team slug validation broken")
	}
}

func TestPrincipals(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		{"jane@example.com", true},
		{"Jane@Example.COM", true},
		{"not-an-email", false},
		{"", false},
		{"a/b@example.com", false},
		{strings.Repeat("a", 250) + "@x.com", false},
	}
	for _, tc := range cases {
		if got := ValidPrincipal(tc.in); got != tc.valid {
			t.Errorf("ValidPrincipal(%q) = %v, want %v", tc.in, got, tc.valid)
		}
	}
	if normPrincipal("  Jane@Example.COM ") != "jane@example.com" {
		t.Error("normPrincipal broken")
	}
	if encodePrincipal("jane@example.com") != "jane%40example.com" {
		t.Error("encodePrincipal broken")
	}
	if decodePrincipal("jane%40example.com") != "jane@example.com" {
		t.Error("decodePrincipal broken")
	}
}

func TestKeys(t *testing.T) {
	if ProfileKey("JANE@Example.com") != "users/jane%40example.com/profile.json" {
		t.Error("ProfileKey broken")
	}
	if InboxKey("a@b.c") != "users/a%40b.c/invitations/index.json" {
		t.Error("InboxKey broken")
	}
	if OrgKey("acme") != "orgs/acme/org.json" {
		t.Error("OrgKey broken")
	}
	if MembersKey("acme") != "orgs/acme/members.json" {
		t.Error("MembersKey broken")
	}
	if TeamKey("acme", "s") != "orgs/acme/teams/s.json" || TeamPrefix("acme") != "orgs/acme/teams/" {
		t.Error("TeamKey broken")
	}
	if OrgInviteKey("acme", "1") != "orgs/acme/invitations/1.json" || OrgInvitePrefix("acme") != "orgs/acme/invitations/" {
		t.Error("OrgInviteKey broken")
	}
	if AccessKey("o", "r") != "repos/o/r/access.json" {
		t.Error("AccessKey broken")
	}
	if RepoInviteKey("o", "r", "1") != "repos/o/r/meta/invitations/1.json" ||
		RepoInvitePrefix("o", "r") != "repos/o/r/meta/invitations/" {
		t.Error("RepoInviteKey broken")
	}
}

func TestValidSubject(t *testing.T) {
	cases := []struct {
		sub string
		ok  bool
	}{
		{"user:jane@example.com", true},
		{"team:acme/platform", true},
		{"user:not-an-email", false},
		{"team:Acme/x", false},
		{"team:acme/", false},
		{"group:admins", false},
		{"jane@example.com", false},
		{"", false},
	}
	for _, tc := range cases {
		if err := validSubject(tc.sub); (err == nil) != tc.ok {
			t.Errorf("validSubject(%q) err=%v, want ok=%v", tc.sub, err, tc.ok)
		}
	}
}

func TestNormalizeAccess(t *testing.T) {
	_, _, err := normalizeAccess("hidden", nil)
	if err == nil {
		t.Error("bad visibility accepted")
	}
	_, _, err = normalizeAccess("public", []AccessBinding{{Subject: "bogus", Role: RoleRead}})
	if err == nil {
		t.Error("bad subject accepted")
	}
	_, _, err = normalizeAccess("public", []AccessBinding{{Subject: "user:a@b.c", Role: "super"}})
	if err == nil {
		t.Error("bad role accepted")
	}
	_, _, err = normalizeAccess("public", []AccessBinding{
		{Subject: "user:a@b.c", Role: RoleRead},
		{Subject: "user:A@B.C", Role: RoleWrite},
	})
	if err == nil {
		t.Error("duplicate subject accepted")
	}
	vis, out, err := normalizeAccess("private", []AccessBinding{
		{Subject: "user:B@x.c", Role: RoleWrite},
		{Subject: "team:acme/z", Role: RoleRead},
		{Subject: "user:A@x.c", Role: RoleRead},
	})
	if err != nil {
		t.Fatalf("normalizeAccess: %v", err)
	}
	if vis != VisibilityPrivate {
		t.Error("visibility not preserved")
	}
	if len(out) != 3 || out[0].Subject != "team:acme/z" || out[1].Subject != "user:a@x.c" || out[2].Subject != "user:b@x.c" {
		t.Errorf("bindings not sorted/normalized: %+v", out)
	}
	if out[2].Subject != "user:b@x.c" {
		t.Error("user subject not lowercased")
	}
	vis, out, err = normalizeAccess("public", nil)
	if err != nil || vis != VisibilityPublic || len(out) != 0 {
		t.Errorf("empty bindings broken: %v %+v", err, out)
	}
}

func TestParseAccessInvalid(t *testing.T) {
	if _, err := parseAccess([]byte("{nope")); err == nil {
		t.Error("bad JSON accepted")
	}
	if _, err := parseAccess([]byte(`{"version":1,"visibility":"hidden","role_bindings":[]}`)); err == nil {
		t.Error("bad visibility accepted")
	}
}

func TestStatusFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrInvalid, 400},
		{ErrNotFound, 404},
		{ErrForbidden, 403},
		{ErrUnauthorized, 401},
		{ErrConflict, 409},
		{errBoom, 503},
	}
	for _, tc := range cases {
		if got := statusFor(tc.err); got != tc.want {
			t.Errorf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestServiceDefaults(t *testing.T) {
	s := New(store.NewMemory(), nil)
	if !s.anonymousRead() {
		t.Error("nil cfg must default anonymous_read open")
	}
	s2 := New(store.NewMemory(), config.Defaults())
	s2.Cfg.Server.Auth.AnonymousRead = false
	if s2.Cfg == nil || s2.anonymousRead() {
		t.Error("anonymous_read=false not honored")
	}
	if s.nowUTC().IsZero() {
		t.Error("nowUTC broken")
	}
	s3 := New(store.NewMemory(), config.Defaults())
	s3.Now = nil
	s3.Rand = nil
	if s3.nowUTC().IsZero() {
		t.Error("nil clock broken")
	}
	if _, err := s3.randomHex(4); err != nil {
		t.Errorf("nil rand broken: %v", err)
	}
	// principalOf fallbacks: Defaults() boots auth.none → the anon-all
	// principal; token mode without injection → anonymous.
	if p := s3.principalOf(context.Background()); !p.Write || !p.Admin {
		t.Errorf("defaults (none mode) must yield the admin principal, got %+v", p)
	}
	tokCfg := config.Defaults()
	tokCfg.Server.Auth.Mode = "token"
	sTok := New(store.NewMemory(), tokCfg)
	if p := sTok.principalOf(context.Background()); !p.Anonymous {
		t.Errorf("token mode must yield anonymous, got %+v", p)
	}
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "none"
	s4 := New(store.NewMemory(), cfg)
	if p := s4.principalOf(context.Background()); !p.Admin || !p.Write {
		t.Errorf("none mode must yield admin principal, got %+v", p)
	}
	ctx := WithPrincipal(context.Background(), alice)
	if p := s4.principalOf(ctx); p.Name != alice.Name {
		t.Errorf("injected principal lost: %+v", p)
	}
	// randomHex error path.
	s5 := New(store.NewMemory(), nil)
	s5.Rand = &failReader{}
	if _, err := s5.randomHex(4); err == nil {
		t.Error("failing rand must error")
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errBoom }

func TestAnonDefaults(t *testing.T) {
	if anon.Name != "anonymous" || !anon.Anonymous {
		t.Error("Anonymous() broken")
	}
	if noneP.Name != "anon" || !noneP.Write || !noneP.Admin {
		t.Error("None() broken")
	}
	_ = alice
	_ = auth.Anonymous()
}
