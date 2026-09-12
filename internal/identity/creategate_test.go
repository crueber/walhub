package identity

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

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

// --- creation/import owner admission (Forgejo #346) ----------------------------

// ownerFixture builds a service with: org "acme" (alice owner, bob member),
// org "solo" (carol owner), and no org for "free".
func ownerFixture(t *testing.T) (*Service, context.Context) {
	t.Helper()
	ctx := context.Background()
	s := New(store.NewMemory(), nil)
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("CreateOrg acme: %v", err)
	}
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgMember); err != nil {
		t.Fatalf("SetMember bob: %v", err)
	}
	if _, err := s.CreateOrg(ctx, "solo", "Solo", "", "carol@example.com"); err != nil {
		t.Fatalf("CreateOrg solo: %v", err)
	}
	return s, ctx
}

func TestCheckCreateOwner(t *testing.T) {
	s, ctx := ownerFixture(t)
	writer := func(name string) auth.Principal { return auth.Principal{Name: name, Write: true} }
	admin := func(name string) auth.Principal { return auth.Principal{Name: name, Write: true, Admin: true} }
	tests := []struct {
		name      string
		owner     string
		p         auth.Principal
		wantKind  auth.AuthErrorKind // -1 (via wantOK) when allowed
		wantOK    bool
		want403In []string // substrings required in the 403 message
	}{
		{"self", "alice@example.com", writer("alice@example.com"), 0, true, nil},
		{"self folded", "Alice@Example.com", writer("alice@example.com"), 0, true, nil},
		{"org owner role", "acme", writer("alice@example.com"), 0, true, nil},
		{"org member role may create", "acme", writer("bob@example.com"), 0, true, nil},
		{"nonmember org", "acme", writer("mallory@example.com"), auth.ErrForbidden, false,
			[]string{`"acme"`, "mallory@example.com"}},
		{"other user namespace", "carol@example.com", writer("alice@example.com"), auth.ErrForbidden, false,
			[]string{`"carol@example.com"`, "alice@example.com", "acme"}},
		{"unclaimed prefix by non-owner", "free", writer("mallory@example.com"), auth.ErrForbidden, false,
			[]string{`"free"`, "mallory@example.com"}},
		{"anonymous", "acme", auth.Anonymous(), auth.ErrUnauthorized, false, nil},
		{"admin bypass foreign owner", "solo", admin("root@example.com"), 0, true, nil},
		{"admin bypass unknown prefix", "free", admin("root@example.com"), 0, true, nil},
		{"auth-none admin-shaped bypass", "free", auth.None(), 0, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cerr := s.CheckCreateOwner(ctx, tc.owner, tc.p)
			if tc.wantOK {
				if cerr != nil {
					t.Fatalf("CheckCreateOwner(%q) = %v, want OK", tc.owner, cerr.Why)
				}
				return
			}
			if cerr == nil {
				t.Fatalf("CheckCreateOwner(%q) = OK, want %v", tc.owner, tc.wantKind)
			}
			if cerr.Kind != tc.wantKind {
				t.Fatalf("CheckCreateOwner(%q) kind = %v, want %v (%q)", tc.owner, cerr.Kind, tc.wantKind, cerr.Why)
			}
			for _, sub := range tc.want403In {
				if !strings.Contains(cerr.Why, sub) {
					t.Fatalf("403 message %q must name %q", cerr.Why, sub)
				}
			}
		})
	}
}

func TestCheckCreateOwnerProbeError(t *testing.T) {
	ctx := context.Background()
	broken := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	cerr := broken.CheckCreateOwner(ctx, "acme", auth.Principal{Name: "alice@example.com", Write: true})
	if cerr == nil || cerr.Kind != auth.ErrUnavailable {
		t.Fatalf("probe error = %v, want ErrUnavailable", cerr)
	}
}

func TestMemberOrgs(t *testing.T) {
	s, ctx := ownerFixture(t)
	for _, tc := range []struct {
		principal string
		want      []string
	}{
		{"alice@example.com", []string{"acme"}},
		{"bob@example.com", []string{"acme"}},
		{"carol@example.com", []string{"solo"}},
		{"mallory@example.com", []string{}},
		{"Alice@Example.com", []string{"acme"}},
	} {
		got, err := s.MemberOrgs(ctx, tc.principal)
		if err != nil {
			t.Fatalf("MemberOrgs(%q): %v", tc.principal, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("MemberOrgs(%q) = %v, want %v", tc.principal, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("MemberOrgs(%q) = %v, want %v", tc.principal, got, tc.want)
			}
		}
	}
	broken := New(&errStore{ObjectStore: store.NewMemory(), listErr: errBoom}, config.Defaults())
	if _, err := broken.MemberOrgs(ctx, "alice@example.com"); err == nil {
		t.Fatal("MemberOrgs probe error must surface")
	}
}
