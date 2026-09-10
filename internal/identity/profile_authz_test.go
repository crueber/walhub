package identity

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// The OwnerEditor seam (Forgejo #234): org owners edit the org-namespace
// profile; everyone else falls through to the core default.
func TestCanEditOwnerProfile(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	owner := auth.Principal{Name: "alice@example.com", Write: true}
	member := auth.Principal{Name: "sam@example.com", Write: true}
	if err := func() error {
		_, err := s.SetMember(ctx, "acme", "sam@example.com", OrgMember)
		return err
	}(); err != nil {
		t.Fatal(err)
	}
	// Org owner (role owner, name unrelated to the slug) may edit.
	if ok, err := s.CanEditOwnerProfile(ctx, "acme", owner); err != nil || !ok {
		t.Fatalf("org owner = %v, %v; want true, nil", ok, err)
	}
	// Plain member may not.
	if ok, err := s.CanEditOwnerProfile(ctx, "acme", member); err != nil || ok {
		t.Fatalf("member = %v, %v; want false, nil", ok, err)
	}
	// Case-insensitive principal match.
	upper := auth.Principal{Name: "ALICE@EXAMPLE.COM", Write: true}
	if ok, err := s.CanEditOwnerProfile(ctx, "acme", upper); err != nil || !ok {
		t.Fatalf("case-insensitive owner = %v, %v", ok, err)
	}
	// Unclaimed prefix (no members.json): false — the core default decides.
	if ok, err := s.CanEditOwnerProfile(ctx, "nobody", owner); err != nil || ok {
		t.Fatalf("unclaimed prefix = %v, %v; want false, nil", ok, err)
	}
	// Nil service / nil store: false, nil (never a panic, never a grant).
	var nilSvc *Service
	if ok, err := nilSvc.CanEditOwnerProfile(ctx, "acme", owner); err != nil || ok {
		t.Fatalf("nil service = %v, %v", ok, err)
	}
}
