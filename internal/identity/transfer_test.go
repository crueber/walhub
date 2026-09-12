package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// seedTransferRepo marks owner/repo as an existing repo with one payload
// object besides the manifest (so the move carries >1 key).
func seedTransferRepo(t *testing.T, s *Service, owner, repo string) {
	t.Helper()
	ctx := context.Background()
	seedRepo(t, s, owner, repo)
	if _, err := store.PutBytes(ctx, s.Store, "repos/"+owner+"/"+repo+"/packs/pack-1.pack", []byte("pack-bytes"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
}

// seedTransferAccess writes access.json directly (bypassing PutAccess so
// fixtures can carry any binding mix).
func seedTransferAccess(t *testing.T, s *Service, owner, repo string, doc *AccessDoc) {
	t.Helper()
	if _, err := store.PutBytes(context.Background(), s.Store, AccessKey(owner, repo), encodeAccess(doc),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func listKeys(t *testing.T, s *Service, prefix string) []string {
	t.Helper()
	var out []string
	if err := s.Store.List(context.Background(), prefix, "", func(m store.ObjectMeta) error {
		out = append(out, m.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTransferRepoValidation(t *testing.T) {
	s := testService()
	ctx := context.Background()
	cases := []struct {
		name     string
		srcOwner string
		srcRepo  string
		dstOwner string
		dstRepo  string
	}{
		{"bad source", "bad owner!", "r", "acme", "r"},
		{"bad dest owner", "acme", "r", "bad owner!", "r"},
		{"bad dest repo", "acme", "r", "acme", "bad repo!"},
		{"empty dest", "acme", "r", "", ""},
		{"identical", "acme", "r", "acme", "r"},
		{"at-sign repo", "alice@example.com", "bad repo!", "beta", "r"},
	}
	for _, tc := range cases {
		if err := s.TransferRepo(ctx, tc.srcOwner, tc.srcRepo, tc.dstOwner, tc.dstRepo); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s must be invalid: %v", tc.name, err)
		}
	}
}

func TestTransferRepoUnknownSource(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransferRepo(ctx, "acme", "ghost", "beta", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown source must 404: %v", err)
	}
}

func TestTransferRepoOrgToOrg(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "platform", "P", ""); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "acme", "r1")
	seedTransferAccess(t, s, "acme", "r1", &AccessDoc{Version: 3, Visibility: VisibilityPrivate,
		RoleBindings: []AccessBinding{
			{Subject: "team:acme/platform", Role: RoleWrite},
			{Subject: "user:bob@example.com", Role: RoleRead},
		}, UpdatedAt: "2026-09-03T12:00:00Z"})
	// A pending repo invite rides meta/ and must survive the move.
	if _, err := store.PutBytes(ctx, s.Store, RepoInviteKey("acme", "r1", "inv1"), []byte(`{"id":"inv1"}`),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}

	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	if keys := listKeys(t, s, "repos/acme/r1/"); len(keys) != 0 {
		t.Errorf("source prefix must be empty, got %v", keys)
	}
	raw, _, err := store.GetBytes(ctx, s.Store, "repos/beta/r1/packs/pack-1.pack", store.GetOptions{})
	if err != nil || string(raw) != "pack-bytes" {
		t.Errorf("pack bytes must move untouched: %q %v", raw, err)
	}
	raw, _, err = store.GetBytes(ctx, s.Store, RepoInviteKey("beta", "r1", "inv1"), store.GetOptions{})
	if err != nil || string(raw) != `{"id":"inv1"}` {
		t.Errorf("repo invite must survive: %q %v", raw, err)
	}
	doc, _, err := s.GetAccess(ctx, "beta", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Visibility != VisibilityPrivate || doc.Version != 3 {
		t.Errorf("visibility/version must survive: %+v", doc)
	}
	want := map[string]Role{"team:acme/platform": RoleWrite, "user:bob@example.com": RoleRead}
	if len(doc.RoleBindings) != len(want) {
		t.Fatalf("bindings must survive untouched: %+v", doc.RoleBindings)
	}
	for _, b := range doc.RoleBindings {
		if want[b.Subject] != b.Role {
			t.Errorf("binding %q = %q, want %q", b.Subject, b.Role, want[b.Subject])
		}
	}
	// Org-owner resolution follows the namespace: alice (source-org owner
	// only) loses implicit admin; carol (beta owner) holds it.
	if role, _ := s.Resolve(ctx, "beta", "r1", authPrincipal("alice@example.com")); role == RoleAdmin {
		t.Error("source org owner must not stay admin after the move")
	}
	if role, _ := s.Resolve(ctx, "beta", "r1", authPrincipal("carol@example.com")); role != RoleAdmin {
		t.Errorf("dest org owner must resolve admin, got %q", role)
	}
}

func TestTransferRepoOrgToUser(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "acme", "r1")
	seedTransferAccess(t, s, "acme", "r1", &AccessDoc{Version: 1, Visibility: VisibilityPublic,
		RoleBindings: []AccessBinding{{Subject: "user:bob@example.com", Role: RoleRead}},
		UpdatedAt:    "2026-09-03T12:00:00Z"})

	if err := s.TransferRepo(ctx, "acme", "r1", "carol@example.com", "r1"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	doc, _, err := s.GetAccess(ctx, "carol@example.com", "r1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Role{"user:bob@example.com": RoleRead, "user:carol@example.com": RoleAdmin}
	if len(doc.RoleBindings) != len(want) {
		t.Fatalf("dest user must gain admin, others kept: %+v", doc.RoleBindings)
	}
	for _, b := range doc.RoleBindings {
		if want[b.Subject] != b.Role {
			t.Errorf("binding %q = %q, want %q", b.Subject, b.Role, want[b.Subject])
		}
	}
}

func TestTransferRepoUserToOrg(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "alice@example.com", "r1")
	seedTransferAccess(t, s, "alice@example.com", "r1", &AccessDoc{Version: 2, Visibility: VisibilityPrivate,
		RoleBindings: []AccessBinding{
			{Subject: "user:alice@example.com", Role: RoleAdmin},
			{Subject: "user:bob@example.com", Role: RoleWrite},
		}, UpdatedAt: "2026-09-03T12:00:00Z"})

	if err := s.TransferRepo(ctx, "alice@example.com", "r1", "beta", "renamed"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	doc, _, err := s.GetAccess(ctx, "beta", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.RoleBindings) != 1 || doc.RoleBindings[0].Subject != "user:bob@example.com" {
		t.Errorf("seller binding must not follow; collaborator kept: %+v", doc.RoleBindings)
	}
	if role, _ := s.Resolve(ctx, "beta", "renamed", authPrincipal("alice@example.com")); role == RoleAdmin {
		t.Error("seller must not stay admin after the move")
	}
}

func TestTransferRepoMissingAccess(t *testing.T) {
	s := testService()
	ctx := context.Background()
	// User destination: the synthesized default materializes (v1, admin).
	seedRepo(t, s, "acme", "r1")
	if err := s.TransferRepo(ctx, "acme", "r1", "carol@example.com", "r1"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	doc, _, err := s.GetAccess(ctx, "carol@example.com", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || len(doc.RoleBindings) != 1 ||
		doc.RoleBindings[0] != (AccessBinding{Subject: "user:carol@example.com", Role: RoleAdmin}) {
		t.Errorf("synthesized default must materialize: %+v", doc)
	}
	// Org destination: reads synthesize, no object needed.
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	seedRepo(t, s, "acme", "r2")
	if err := s.TransferRepo(ctx, "acme", "r2", "beta", "r2"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	if _, _, err := store.GetBytes(ctx, s.Store, AccessKey("beta", "r2"), store.GetOptions{}); err == nil {
		t.Error("org destination with no source access.json must stay object-free")
	}
}

func TestTransferRepoCorruptAccessMovesOpaquely(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "acme", "r1")
	if _, err := store.PutBytes(ctx, s.Store, AccessKey("acme", "r1"), []byte("{corrupt"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("corrupt access.json must not block the move: %v", err)
	}
	raw, _, err := store.GetBytes(ctx, s.Store, AccessKey("beta", "r1"), store.GetOptions{})
	if err != nil || string(raw) != "{corrupt" {
		t.Errorf("corrupt access.json must move byte-identical: %q %v", raw, err)
	}
}

func TestTransferRepoDestOccupied(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "acme", "r1")
	seedRepo(t, s, "beta", "r1")
	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); !errors.Is(err, ErrConflict) {
		t.Errorf("occupied destination must 409: %v", err)
	}
	// Source untouched; destination manifest still the original.
	if keys := listKeys(t, s, "repos/acme/r1/"); len(keys) != 2 {
		t.Errorf("source must be intact: %v", keys)
	}
}

func TestTransferRepoMidMoveCollisionCleansUp(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "acme", "r1")
	// A destination key without a manifest: the manifest probe passes,
	// the first colliding PutCreate aborts — source intact, no residue.
	if _, err := store.PutBytes(ctx, s.Store, "repos/beta/r1/packs/pack-1.pack", []byte("other"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mid-move collision must 409: %v", err)
	}
	if keys := listKeys(t, s, "repos/acme/r1/"); len(keys) != 2 {
		t.Errorf("source must be intact: %v", keys)
	}
	for _, k := range listKeys(t, s, "repos/beta/r1/") {
		if k != "repos/beta/r1/packs/pack-1.pack" {
			t.Errorf("residue left at destination: %q", k)
		}
	}
	raw, _, _ := store.GetBytes(ctx, s.Store, "repos/beta/r1/packs/pack-1.pack", store.GetOptions{})
	if string(raw) != "other" {
		t.Errorf("pre-existing destination bytes must survive: %q", raw)
	}
}

func TestTransferRepoStoreFailures(t *testing.T) {
	ctx := context.Background()
	newSeeded := func(t *testing.T) *store.Memory {
		t.Helper()
		base := store.NewMemory()
		swap := New(base, config.Defaults())
		swap.Now = testClock
		seedTransferRepo(t, swap, "acme", "r1")
		return base
	}
	// Copy failure (Get boom): source intact, error surfaces.
	s := New(&errStore{ObjectStore: newSeeded(t), getErr: errBoom}, config.Defaults())
	s.Now = testClock
	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); !errors.Is(err, errBoom) {
		t.Errorf("copy failure must surface: %v", err)
	}
	// Delete failure: destination complete, error surfaces (data safe).
	s = New(&errStore{ObjectStore: newSeeded(t), delErr: errBoom}, config.Defaults())
	s.Now = testClock
	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); !errors.Is(err, errBoom) {
		t.Errorf("delete failure must surface: %v", err)
	}
	if _, _, err := store.GetBytes(ctx, s.Store, "repos/beta/r1/"+manifestName, store.GetOptions{}); err != nil {
		t.Errorf("destination must be complete on delete failure: %v", err)
	}
	// Lister failure on the source prefix.
	s = New(&errStore{ObjectStore: newSeeded(t), listErr: errBoom}, config.Defaults())
	s.Now = testClock
	if err := s.TransferRepo(ctx, "acme", "r1", "beta", "r1"); !errors.Is(err, errBoom) {
		t.Errorf("list failure must surface: %v", err)
	}
}

// noDeleteStore leaves deletes as no-ops so the post-move re-list finds
// leftovers (a concurrent push racing the move).
type noDeleteStore struct {
	store.ObjectStore
}

func (n *noDeleteStore) Delete(ctx context.Context, key string, v store.Version) error { return nil }

func TestTransferRepoConcurrentWriteDetected(t *testing.T) {
	base := store.NewMemory()
	s := New(base, config.Defaults())
	s.Now = testClock
	swap := New(base, config.Defaults())
	swap.Now = testClock
	seedTransferRepo(t, swap, "acme", "r1")
	s.Store = &noDeleteStore{ObjectStore: base}
	if err := s.TransferRepo(context.Background(), "acme", "r1", "beta", "r1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("leftover source keys must 409: %v", err)
	} else if !strings.Contains(err.Error(), "changed during transfer") {
		t.Errorf("conflict must name the race: %v", err)
	}
	if _, _, err := store.GetBytes(context.Background(), base, "repos/beta/r1/"+manifestName, store.GetOptions{}); err != nil {
		t.Errorf("destination must be complete: %v", err)
	}
}

// TestDeleteOrgNamesTransfer pins acceptance #2: now that transfer
// exists, the 409 names a real capability.
func TestDeleteOrgNamesTransfer(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"acme", "r1"}}, nil
	}
	if err := s.DeleteOrg(ctx, "acme"); !errors.Is(err, ErrConflict) {
		t.Fatalf("DeleteOrg with repos must 409: %v", err)
	} else if !strings.Contains(err.Error(), "transfer") {
		t.Errorf("409 must name transfer (it exists now): %v", err)
	}
}

// --- handler ---------------------------------------------------------------

func transferHandlerFixture(t *testing.T) (*Service, context.Context) {
	t.Helper()
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "solo", "S", "", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	seedTransferRepo(t, s, "acme", "r1")
	if _, err := s.PutAccess(ctx, "acme", "r1", "", VisibilityPrivate,
		[]AccessBinding{{Subject: "user:alice@example.com", Role: RoleAdmin}}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestTransferHandler(t *testing.T) {
	newH := func(s *Service, p auth.Principal) *Handler { return testHandler(s, p) }
	t.Run("happy path owner to org", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"beta"}`)
		if w.Code != 201 {
			t.Fatalf("POST transfer = %d (%s), want 201", w.Code, w.Body.String())
		}
		if got := w.Body.String(); !strings.Contains(got, `"repo":"r1"`) || !strings.Contains(got, `"owner":"beta"`) {
			t.Errorf("201 must name the new address: %s", got)
		}
	})
	t.Run("rename in flight", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"beta","repo":"r2"}`)
		if w.Code != 201 {
			t.Fatalf("POST transfer = %d (%s), want 201", w.Code, w.Body.String())
		}
		if _, _, err := store.GetBytes(context.Background(), s.Store, "repos/beta/r2/"+manifestName, store.GetOptions{}); err != nil {
			t.Errorf("renamed destination must exist: %v", err)
		}
	})
	t.Run("browser lane", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		w := doReq(newH(s, alice), "POST", "/acme/r1/api-browser/transfer", `{"owner":"beta"}`)
		if w.Code != 201 {
			t.Fatalf("browser lane = %d (%s), want 201", w.Code, w.Body.String())
		}
	})
	t.Run("anonymous is 401", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, anon), "POST", "/acme/r1/api/transfer", `{"owner":"beta"}`); w.Code != 401 {
			t.Errorf("anon = %d, want 401", w.Code)
		}
	})
	t.Run("non-admin is 403", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, stranger), "POST", "/acme/r1/api/transfer", `{"owner":"beta"}`); w.Code != 403 {
			t.Errorf("stranger = %d, want 403", w.Code)
		}
	})
	t.Run("foreign destination is 403", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"solo"}`); w.Code != 403 {
			t.Errorf("foreign org = %d (%s), want 403", w.Code, w.Body.String())
		}
	})
	t.Run("host admin bypasses admission", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, admin), "POST", "/acme/r1/api/transfer", `{"owner":"solo"}`); w.Code != 201 {
			t.Errorf("admin = %d (%s), want 201", w.Code, w.Body.String())
		}
	})
	t.Run("unknown source is 404", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, admin), "POST", "/acme/ghost/api/transfer", `{"owner":"beta"}`); w.Code != 404 {
			t.Errorf("ghost = %d, want 404", w.Code)
		}
	})
	t.Run("occupied destination is 409", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		seedRepo(t, s, "beta", "r1")
		if w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"beta"}`); w.Code != 409 {
			t.Errorf("occupied = %d (%s), want 409", w.Code, w.Body.String())
		}
	})
	t.Run("bad body is 400", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":""}`); w.Code != 400 {
			t.Errorf("empty owner = %d, want 400", w.Code)
		}
		if w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"beta","repo":"bad repo!"}`); w.Code != 400 {
			t.Errorf("bad repo = %d, want 400", w.Code)
		}
		if w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{oops`); w.Code != 400 {
			t.Errorf("bad json = %d, want 400", w.Code)
		}
	})
	t.Run("method gate", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		if w := doReq(newH(s, alice), "GET", "/acme/r1/api/transfer", ""); w.Code != 405 {
			t.Errorf("GET = %d, want 405", w.Code)
		}
	})
	t.Run("self transfer to own namespace", func(t *testing.T) {
		s, _ := transferHandlerFixture(t)
		w := doReq(newH(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"alice@example.com"}`)
		if w.Code != 201 {
			t.Fatalf("self-namespace = %d (%s), want 201", w.Code, w.Body.String())
		}
		doc, _, err := s.GetAccess(context.Background(), "alice@example.com", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.RoleBindings) != 1 || doc.RoleBindings[0].Subject != "user:alice@example.com" {
			t.Errorf("self transfer must converge on the owner binding: %+v", doc.RoleBindings)
		}
	})
}
