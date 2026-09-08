package identity

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- org-membership gate (#210 §3, S2) ---------------------------------------

func TestIsOrgMember(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	s := New(st, nil)
	// Unclaimed prefix → (false, false, nil): legacy-open.
	exists, member, err := s.IsOrgMember(ctx, "free", "alice@example.com")
	if err != nil || exists || member {
		t.Fatalf("unclaimed = %v %v %v", exists, member, err)
	}
	// Create an org; creator is owner AND member.
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	exists, member, err = s.IsOrgMember(ctx, "acme", "alice@example.com")
	if err != nil || !exists || !member {
		t.Fatalf("owner = %v %v %v", exists, member, err)
	}
	// Stranger → proven non-membership.
	exists, member, err = s.IsOrgMember(ctx, "acme", "mallory@example.com")
	if err != nil || !exists || member {
		t.Fatalf("stranger = %v %v %v", exists, member, err)
	}
	// Case-insensitive match.
	exists, member, err = s.IsOrgMember(ctx, "acme", "Alice@Example.com")
	if err != nil || !exists || !member {
		t.Fatalf("folded = %v %v %v", exists, member, err)
	}
	// Probe error → err (caller maps to 503, never 403).
	broken := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if _, _, err := broken.IsOrgMember(ctx, "acme", "alice@example.com"); err == nil {
		t.Fatal("probe error must surface")
	}
}

// --- eager access default (#210 §3, R1 B5) ------------------------------------

func TestEnsureRepoAccess(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	s := New(st, nil)
	// Valid creator → materialized with admin binding.
	if err := s.EnsureRepoAccess(ctx, "acme", "r1", "Alice@Example.com", ""); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	doc, _, err := s.GetAccess(ctx, "acme", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.RoleBindings) != 1 || doc.RoleBindings[0].Subject != "user:alice@example.com" || doc.RoleBindings[0].Role != RoleAdmin {
		t.Fatalf("bindings: %+v", doc.RoleBindings)
	}
	if doc.Visibility != VisibilityPublic {
		t.Fatalf("visibility: %q", doc.Visibility)
	}
	// Idempotent: second call adopts (no overwrite, no error).
	if err := s.EnsureRepoAccess(ctx, "acme", "r1", "bob@example.com", ""); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	doc, _, _ = s.GetAccess(ctx, "acme", "r1")
	if doc.RoleBindings[0].Subject != "user:alice@example.com" {
		t.Fatalf("adopt must not overwrite: %+v", doc.RoleBindings)
	}
	// Anonymous creator (auth-none) → visibility-only doc, no invalid
	// user:anonymous binding.
	if err := s.EnsureRepoAccess(ctx, "acme", "r2", "anon", ""); err != nil {
		t.Fatalf("anon: %v", err)
	}
	doc, _, err = s.GetAccess(ctx, "acme", "r2")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.RoleBindings) != 0 {
		t.Fatalf("anon bindings must be empty: %+v", doc.RoleBindings)
	}
	// Private request → private doc (the POST visibility toggle's last
	// mile — must not silently materialize public).
	if err := s.EnsureRepoAccess(ctx, "acme", "r3", "alice@example.com", "private"); err != nil {
		t.Fatalf("private: %v", err)
	}
	doc, _, err = s.GetAccess(ctx, "acme", "r3")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Visibility != VisibilityPrivate {
		t.Fatalf("private visibility: %q", doc.Visibility)
	}
	if len(doc.RoleBindings) != 1 || doc.RoleBindings[0].Subject != "user:alice@example.com" {
		t.Fatalf("private bindings: %+v", doc.RoleBindings)
	}
}
