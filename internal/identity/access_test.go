package identity

import (
	"context"
	"errors"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestGetAccessSynthesizes(t *testing.T) {
	s := testService()
	doc, ver, err := s.GetAccess(context.Background(), "acme", "repo")
	if err != nil {
		t.Fatalf("GetAccess: %v", err)
	}
	if ver != "" {
		t.Errorf("synthesized version must be empty, got %q", ver)
	}
	if doc.Visibility != VisibilityPublic || doc.Version != 0 {
		t.Errorf("bad synthesis: %+v", doc)
	}
	if len(doc.RoleBindings) != 0 {
		t.Errorf("non-email owner must synthesize empty bindings: %+v", doc.RoleBindings)
	}
	// An email owner namespace synthesizes the admin binding.
	email, _, err := s.GetAccess(context.Background(), "jane@example.com", "repo")
	if err != nil {
		t.Fatalf("GetAccess: %v", err)
	}
	if len(email.RoleBindings) != 1 || email.RoleBindings[0].Subject != "user:jane@example.com" || email.RoleBindings[0].Role != RoleAdmin {
		t.Errorf("bad owner binding: %+v", email.RoleBindings)
	}
}

func TestPutAccessRoundTrip(t *testing.T) {
	s := testService()
	ctx := context.Background()
	doc, err := s.PutAccess(ctx, "acme", "r", "", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
		{Subject: "user:jane@example.com", Role: RoleAdmin},
	})
	if err != nil {
		t.Fatalf("PutAccess: %v", err)
	}
	if doc.Version != 1 || doc.Visibility != VisibilityPrivate {
		t.Errorf("bad doc: %+v", doc)
	}
	if doc.RoleBindings[0].Subject != "team:acme/platform" {
		t.Errorf("bindings not sorted: %+v", doc.RoleBindings)
	}
	got, ver, err := s.GetAccess(ctx, "acme", "r")
	if err != nil {
		t.Fatalf("GetAccess: %v", err)
	}
	if ver == "" || got.Version != 1 || len(got.RoleBindings) != 2 {
		t.Errorf("re-read broken: ver=%q %+v", ver, got)
	}
	// Second conditional GET is an LRU hit (NotModified path).
	got2, ver2, err := s.GetAccess(ctx, "acme", "r")
	if err != nil || ver2 != ver || got2.Version != 1 {
		t.Errorf("conditional re-read broken: %v %q", err, ver2)
	}
	// Update bumps the version.
	doc2, err := s.PutAccess(ctx, "acme", "r", ver, VisibilityPublic, nil)
	if err != nil {
		t.Fatalf("PutAccess update: %v", err)
	}
	if doc2.Version != 2 || len(doc2.RoleBindings) != 0 {
		t.Errorf("update broken: %+v", doc2)
	}
}

func TestPutAccessValidation(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.PutAccess(ctx, "o", "r", "", "hidden", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad visibility: %v", err)
	}
	if _, err := s.PutAccess(ctx, "o", "r", "", VisibilityPublic,
		[]AccessBinding{{Subject: "nope", Role: RoleRead}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad subject: %v", err)
	}
}

func TestPutAccessStaleBase(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.PutAccess(ctx, "o", "r", "", VisibilityPublic, nil); err != nil {
		t.Fatal(err)
	}
	_, ver, _ := s.GetAccess(ctx, "o", "r")
	// Concurrent writer wins first.
	if _, err := s.PutAccess(ctx, "o", "r", ver, VisibilityPrivate, nil); err != nil {
		t.Fatal(err)
	}
	// Stale base now conflicts.
	if _, err := s.PutAccess(ctx, "o", "r", ver, VisibilityPublic, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("stale base must 409, got %v", err)
	}
}

func TestPutAccessStoreErrors(t *testing.T) {
	ctx := context.Background()
	s := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if _, err := s.PutAccess(ctx, "o", "r", "", VisibilityPublic, nil); !errors.Is(err, errBoom) {
		t.Errorf("get error must surface: %v", err)
	}
	s2 := New(&errStore{ObjectStore: store.NewMemory(), putErr: errBoom}, config.Defaults())
	if _, err := s2.PutAccess(ctx, "o", "r", "", VisibilityPublic, nil); !errors.Is(err, errBoom) {
		t.Errorf("put error must surface: %v", err)
	}
	s3 := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if _, _, err := s3.GetAccess(ctx, "o", "r"); !errors.Is(err, errBoom) {
		t.Errorf("GetAccess error must surface: %v", err)
	}
}

func TestGetAccessCorrupt(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := store.PutBytes(ctx, s.Store, AccessKey("o", "r"), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetAccess(ctx, "o", "r"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt access.json must be invalid: %v", err)
	}
	// PUT over corrupt also surfaces invalid.
	if _, err := s.PutAccess(ctx, "o", "r", "", VisibilityPublic, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("PUT over corrupt must be invalid: %v", err)
	}
}

func TestResolve(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	if _, err := s.PutAccess(ctx, "acme", "repo", "", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
		{Subject: "user:carol@example.com", Role: RoleTriage},
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		p    auth.Principal
		want Role
	}{
		{"team write", bob, RoleWrite},
		{"direct triage", carol, RoleTriage},
		{"org owner", alice, RoleAdmin},
		{"host admin flag", admin, RoleAdmin},
		{"flag write", writer, RoleWrite},
		{"stranger private", stranger, ""},
		{"anon private", anon, ""},
	}
	for _, tc := range cases {
		role, _ := s.Resolve(ctx, "acme", "repo", tc.p)
		if role != tc.want {
			t.Errorf("%s: role=%q want %q", tc.name, role, tc.want)
		}
	}
	// Public repo: stranger still denied (authed), anon reads.
	if _, err := s.PutAccess(ctx, "acme", "pub", "", VisibilityPublic, nil); err != nil {
		t.Fatal(err)
	}
	if role, _ := s.Resolve(ctx, "acme", "pub", stranger); role != "" {
		t.Errorf("authed stranger on public repo resolves %q (public read applies at the gate, not resolution)", role)
	}
	if role, _ := s.Resolve(ctx, "acme", "pub", anon); role != RoleRead {
		t.Errorf("anon on public repo resolves %q, want read", role)
	}
	// Max across bindings wins.
	if _, err := s.PutAccess(ctx, "acme", "multi", "", VisibilityPrivate, []AccessBinding{
		{Subject: "user:dave@example.com", Role: RoleRead},
		{Subject: "user:dave@example.com", Role: RoleMaintain},
	}); err == nil {
		t.Error("duplicate binding subjects must 400")
	}
	if _, err := s.PutAccess(ctx, "acme", "multi", "", VisibilityPrivate, []AccessBinding{
		{Subject: "user:dave@example.com", Role: RoleRead},
	}); err != nil {
		t.Fatal(err)
	}
	// Missing team reference does not match.
	if _, err := s.PutAccess(ctx, "acme", "ghost", "", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/nonexistent", Role: RoleAdmin},
	}); err != nil {
		t.Fatal(err)
	}
	if role, _ := s.Resolve(ctx, "acme", "ghost", stranger); role != "" {
		t.Errorf("ghost team must not match: %q", role)
	}
	// Resolve with a store error falls back to synthesis.
	sErr := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if role, _ := sErr.Resolve(ctx, "acme", "ghost", stranger); role != "" {
		t.Errorf("error fallback must synthesize: %q", role)
	}
}
