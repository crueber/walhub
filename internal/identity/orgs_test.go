package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestOrgCRUD(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "Bad Org!", "x", "", "alice@example.com"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad org slug: %v", err)
	}
	o, err := s.CreateOrg(ctx, "acme", "Acme Corp", "desc", "alice@example.com")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if o.Org != "acme" || o.Version != 1 || o.DisplayName != "Acme Corp" {
		t.Errorf("bad org: %+v", o)
	}
	if _, err := s.CreateOrg(ctx, "acme", "dup", "", "bob@example.com"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate org must 409: %v", err)
	}
	got, err := s.GetOrg(ctx, "acme")
	if err != nil || got == nil || got.Org != "acme" {
		t.Errorf("GetOrg: %v %+v", err, got)
	}
	if got, err := s.GetOrg(ctx, "ghost"); err != nil || got != nil {
		t.Errorf("GetOrg ghost: %v %+v", err, got)
	}
	sErr := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if _, err := sErr.GetOrg(ctx, "acme"); !errors.Is(err, errBoom) {
		t.Errorf("GetOrg error: %v", err)
	}
	// Corrupt org.json.
	if _, err := store.PutBytes(ctx, s.Store, OrgKey("bad"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOrg(ctx, "bad"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt org: %v", err)
	}
	upd, err := s.PutOrg(ctx, "acme", "Acme!", "new")
	if err != nil {
		t.Fatalf("PutOrg: %v", err)
	}
	if upd.Version != 2 || upd.DisplayName != "Acme!" {
		t.Errorf("PutOrg broken: %+v", upd)
	}
	if _, err := s.PutOrg(ctx, "ghost", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("PutOrg ghost: %v", err)
	}
	if _, err := s.PutOrg(ctx, "bad", "x", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("PutOrg corrupt: %v", err)
	}
	// Remove the corrupt fixture so the listing below is exact.
	if err := s.Store.Delete(ctx, OrgKey("bad"), ""); err != nil {
		t.Fatal(err)
	}
	// List.
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	orgs, err := s.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}
	if len(orgs) != 2 || orgs[0] != "acme" || orgs[1] != "beta" {
		t.Errorf("ListOrgs = %v", orgs)
	}
	if _, err := New(&errStore{ObjectStore: store.NewMemory(), listErr: errBoom}, config.Defaults()).ListOrgs(ctx); !errors.Is(err, errBoom) {
		t.Errorf("ListOrgs error: %v", err)
	}
	// Empty store lists [].
	if orgs, err := testService().ListOrgs(ctx); err != nil || len(orgs) != 0 {
		t.Errorf("empty ListOrgs = %v, %v", orgs, err)
	}
}

func TestMembers(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.SetMember(ctx, "ghost", "x@y.z", OrgMember); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetMember ghost org: %v", err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMember(ctx, "acme", "not-an-email", OrgMember); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetMember bad principal: %v", err)
	}
	if _, err := s.SetMember(ctx, "acme", "x@y.z", "root"); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetMember bad role: %v", err)
	}
	m, err := s.SetMember(ctx, "acme", "BOB@example.com", OrgMember)
	if err != nil {
		t.Fatalf("SetMember: %v", err)
	}
	if len(m.Members) != 2 || m.Members[0].Principal != "alice@example.com" || m.Members[1].Principal != "bob@example.com" {
		t.Errorf("roster not sorted/normalized: %+v", m.Members)
	}
	// Re-role.
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgOwner); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMembers(ctx, "acme")
	owners := 0
	for _, e := range got.Members {
		if e.Role == OrgOwner {
			owners++
		}
	}
	if owners != 2 {
		t.Errorf("re-role failed: %+v", got.Members)
	}
	// Remove one owner OK; removing the last owner 409s.
	if _, err := s.RemoveMember(ctx, "acme", "alice@example.com"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if _, err := s.RemoveMember(ctx, "acme", "bob@example.com"); !errors.Is(err, ErrConflict) {
		t.Errorf("last owner removal must 409: %v", err)
	}
	// Remove non-member is a no-op success.
	if _, err := s.RemoveMember(ctx, "acme", "ghost@example.com"); err != nil {
		t.Errorf("RemoveMember ghost: %v", err)
	}
	if _, err := s.RemoveMember(ctx, "ghost", "x@y.z"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveMember ghost org: %v", err)
	}
	if _, err := s.GetMembers(ctx, "ghost"); err != nil {
		t.Errorf("GetMembers ghost: %v", err)
	}
	// Corrupt roster.
	if _, err := store.PutBytes(ctx, s.Store, MembersKey("bad"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMembers(ctx, "bad"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt roster: %v", err)
	}
	if _, err := s.SetMember(ctx, "bad", "x@y.z", OrgMember); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetMember corrupt: %v", err)
	}
	if _, err := s.RemoveMember(ctx, "bad", "x@y.z"); !errors.Is(err, ErrInvalid) {
		t.Errorf("RemoveMember corrupt: %v", err)
	}
	// isOrgOwner false paths.
	if s.isOrgOwner(ctx, "ghost", "x@y.z") {
		t.Error("isOrgOwner ghost org must be false")
	}
	if s.isOrgOwner(ctx, "bad", "x@y.z") {
		t.Error("isOrgOwner corrupt must be false")
	}
	if s.isOrgOwner(ctx, "acme", "stranger@example.com") {
		t.Error("isOrgOwner stranger must be false")
	}
}

func TestTeams(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateTeam(ctx, "ghost", "s", "n", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateTeam ghost org: %v", err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "Bad Slug", "n", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateTeam bad slug: %v", err)
	}
	tm, err := s.CreateTeam(ctx, "acme", "platform", "Platform", "d")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if tm.Version != 1 || len(tm.Members) != 0 {
		t.Errorf("bad team: %+v", tm)
	}
	if _, err := s.CreateTeam(ctx, "acme", "platform", "dup", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("dup team must 409: %v", err)
	}
	// Get + conditional re-read.
	got, ver, err := s.GetTeam(ctx, "acme", "platform")
	if err != nil || got == nil || ver == "" {
		t.Fatalf("GetTeam: %v %+v %q", err, got, ver)
	}
	if _, _, err := s.GetTeam(ctx, "acme", "ghost"); err != nil {
		t.Errorf("GetTeam ghost: %v", err)
	}
	got2, ver2, err := s.GetTeam(ctx, "acme", "platform")
	if err != nil || ver2 != ver || got2.Version != 1 {
		t.Errorf("team re-read broken: %v %q", err, ver2)
	}
	// Membership.
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "not-an-email"); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetTeamMember bad principal: %v", err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "ghost", "x@y.z"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTeamMember ghost team: %v", err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "BOB@example.com"); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-add.
	tm2, err := s.SetTeamMember(ctx, "acme", "platform", "bob@example.com")
	if err != nil || len(tm2.Members) != 1 {
		t.Errorf("re-add must be idempotent: %v %+v", err, tm2)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	// Put.
	upd, err := s.PutTeam(ctx, "acme", "platform", "Plat", "d2")
	if err != nil {
		t.Fatalf("PutTeam: %v", err)
	}
	if upd.Name != "Plat" || len(upd.Members) != 2 {
		t.Errorf("PutTeam broken: %+v", upd)
	}
	if _, err := s.PutTeam(ctx, "acme", "ghost", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("PutTeam ghost: %v", err)
	}
	// Remove.
	rm, err := s.RemoveTeamMember(ctx, "acme", "platform", "alice@example.com")
	if err != nil || len(rm.Members) != 1 {
		t.Errorf("RemoveTeamMember: %v %+v", err, rm)
	}
	if _, err := s.RemoveTeamMember(ctx, "acme", "ghost", "x@y.z"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveTeamMember ghost: %v", err)
	}
	// List.
	if _, err := s.CreateTeam(ctx, "acme", "aaa", "A", ""); err != nil {
		t.Fatal(err)
	}
	teams, err := s.ListTeams(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(teams) != 2 || teams[0].Slug != "aaa" || teams[1].Slug != "platform" {
		t.Errorf("ListTeams sorted broken: %+v", teams)
	}
	one, err := s.ListTeams(ctx, "acme", 1)
	if err != nil || len(one) != 1 {
		t.Errorf("ListTeams n=1: %v %+v", err, one)
	}
	if _, err := New(&errStore{ObjectStore: store.NewMemory(), listErr: errBoom}, config.Defaults()).ListTeams(ctx, "acme", 0); !errors.Is(err, errBoom) {
		t.Errorf("ListTeams error: %v", err)
	}
	// inTeam false paths.
	if s.inTeam(ctx, "noslash", "x@y.z") || s.inTeam(ctx, "acme/ghost", "x@y.z") {
		t.Error("inTeam must be false for bad refs")
	}
	// Corrupt team object.
	if _, err := store.PutBytes(ctx, s.Store, TeamKey("acme", "corrupt"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetTeam(ctx, "acme", "corrupt"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt team: %v", err)
	}
	if _, err := s.PutTeam(ctx, "acme", "corrupt", "x", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("PutTeam corrupt: %v", err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "corrupt", "x@y.z"); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetTeamMember corrupt: %v", err)
	}
	if _, err := s.RemoveTeamMember(ctx, "acme", "corrupt", "x@y.z"); !errors.Is(err, ErrInvalid) {
		t.Errorf("RemoveTeamMember corrupt: %v", err)
	}
}

func TestDeleteOrg(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if err := s.DeleteOrg(ctx, "Bad!"); err == nil {
		t.Error("DeleteOrg bad slug must fail")
	}
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	// No repos: deletes cleanly.
	if err := s.DeleteOrg(ctx, "acme"); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if o, _ := s.GetOrg(ctx, "acme"); o != nil {
		t.Error("org.json must be gone")
	}
	if m, _ := s.GetMembers(ctx, "acme"); m != nil {
		t.Error("members.json must be gone")
	}
	// Owned repos block with 409 + count.
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"acme", "r1"}, {"acme", "r2"}, {"bob", "r3"}}, nil
	}
	if err := s.DeleteOrg(ctx, "acme"); err == nil || !errors.Is(err, ErrConflict) {
		t.Errorf("DeleteOrg with repos must 409: %v", err)
	} else if got := err.Error(); !strings.Contains(got, "2 repos") {
		t.Errorf("DeleteOrg must name the count: %v", err)
	}
	s.Repos = func(ctx context.Context) ([][2]string, error) { return nil, errBoom }
	if err := s.DeleteOrg(ctx, "acme"); !errors.Is(err, errBoom) {
		t.Errorf("DeleteOrg lister error: %v", err)
	}
}

func TestDeleteTeam(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	if _, err := s.PutAccess(ctx, "acme", "repo", "", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
		{Subject: "user:carol@example.com", Role: RoleRead},
	}); err != nil {
		t.Fatal(err)
	}
	// A flag-less team member resolves through the binding.
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "teamy@example.com"); err != nil {
		t.Fatal(err)
	}
	if role, _ := s.Resolve(ctx, "acme", "repo", authPrincipal("teamy@example.com")); role != RoleWrite {
		t.Fatalf("team member must resolve write, got %q", role)
	}
	// Registry-backed repo enumeration: stub two repos.
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"acme", "repo"}, {"bob", "other"}}, nil
	}
	if err := s.DeleteTeam(ctx, "acme", "platform"); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	if _, _, err := s.GetTeam(ctx, "acme", "platform"); err != nil {
		t.Fatalf("GetTeam after delete: %v", err)
	} else {
		// GetTeam returns nil,nil on missing — assert via second return.
		if tm, _, _ := s.GetTeam(ctx, "acme", "platform"); tm != nil {
			t.Error("team object must be gone")
		}
	}
	doc, _, err := s.GetAccess(ctx, "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range doc.RoleBindings {
		if b.Subject == "team:acme/platform" {
			t.Errorf("binding not stripped: %+v", doc.RoleBindings)
		}
	}
	if len(doc.RoleBindings) != 1 {
		t.Errorf("other bindings must survive: %+v", doc.RoleBindings)
	}
	// Resolution no longer matches the deleted team.
	if role, _ := s.Resolve(ctx, "acme", "repo", authPrincipal("teamy@example.com")); role != "" {
		t.Errorf("deleted team must not resolve: %q", role)
	}
	// Deleting again is idempotent.
	if err := s.DeleteTeam(ctx, "acme", "platform"); err != nil {
		t.Errorf("re-delete must succeed: %v", err)
	}
	// Lister error surfaces.
	s.Repos = func(ctx context.Context) ([][2]string, error) { return nil, errBoom }
	if err := s.DeleteTeam(ctx, "acme", "platform"); !errors.Is(err, errBoom) {
		t.Errorf("DeleteTeam lister error: %v", err)
	}
}
