package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

// notFoundStore answers NotFound for listed keys.
type notFoundStore struct {
	store.ObjectStore
	keys map[string]bool
}

func (n *notFoundStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if n.keys[key] {
		return nil, store.NewNotFound(key)
	}
	return n.ObjectStore.Get(ctx, key, opts)
}

// flakyGetStore fails the first Get of a key, then delegates.
type flakyGetStore struct {
	store.ObjectStore
	failOnce map[string]bool
	err      error
}

func (f *flakyGetStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.failOnce[key] {
		delete(f.failOnce, key)
		return nil, f.err
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func TestNullCollections(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	// Explicit null arrays decode to [].
	if _, err := store.PutBytes(ctx, s.Store, MembersKey("acme"), []byte(`{"version":1,"members":null}`),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMembers(ctx, "acme")
	if err != nil || m.Members == nil || len(m.Members) != 0 {
		t.Errorf("null members: %v %+v", err, m)
	}
	if _, err := s.CreateTeam(ctx, "acme", "t", "T", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, s.Store, TeamKey("acme", "t"), []byte(`{"version":1,"org":"acme","slug":"t","members":null}`),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	s.teams.invalidate("acme", "t")
	tm, _, err := s.GetTeam(ctx, "acme", "t")
	if err != nil || tm.Members == nil || len(tm.Members) != 0 {
		t.Errorf("null team members: %v %+v", err, tm)
	}
	// Null inbox entries decode to [].
	if _, err := store.PutBytes(ctx, s.Store, InboxKey("null@x.c"), []byte(`{"version":1,"entries":null}`),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if entries, err := s.MyInvites(ctx, "null@x.c"); err != nil || len(entries) != 0 {
		t.Errorf("null inbox: %v %+v", err, entries)
	}
}

func TestListTeamsSkips(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "good", "G", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "victim", "V", ""); err != nil {
		t.Fatal(err)
	}
	// Victim vanishes between List and Get → skipped.
	s2 := New(&notFoundStore{ObjectStore: s.Store, keys: map[string]bool{TeamKey("acme", "victim"): true}}, config.Defaults())
	teams, err := s2.ListTeams(ctx, "acme", 0)
	if err != nil || len(teams) != 1 || teams[0].Slug != "good" {
		t.Errorf("vanished team skip: %v %+v", err, teams)
	}
	// Corrupt team object → skipped.
	if _, err := s.CreateTeam(ctx, "acme", "corrupt", "C", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, s.Store, TeamKey("acme", "corrupt"), []byte("{x"),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	teams, err = s.ListTeams(ctx, "acme", 0)
	if err != nil || len(teams) != 2 {
		t.Errorf("corrupt team skip: %v %+v", err, teams)
	}
	// Inner GET hard error surfaces after the pass.
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom, failGet: []string{TeamKey("acme", "good")}}
	s3 := New(ks, config.Defaults())
	if _, err := s3.ListTeams(ctx, "acme", 0); err == nil {
		t.Error("inner GET error must surface")
	}
}

func TestDeleteOrgEdges(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "t", "T", ""); err != nil {
		t.Fatal(err)
	}
	// Failing team listing is tolerated (org objects still removed).
	s2 := New(&errStore{ObjectStore: s.Store, listErr: errBoom}, config.Defaults())
	s2.Repos = func(ctx context.Context) ([][2]string, error) { return nil, nil }
	if err := s2.DeleteOrg(ctx, "acme"); err != nil {
		t.Errorf("DeleteOrg with failing team list: %v", err)
	}
	if o, _ := s.GetOrg(ctx, "acme"); o != nil {
		t.Error("org must be gone")
	}
	// Invite-object delete failure surfaces.
	if _, err := s.CreateOrg(ctx, "beta", "B", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrgInvite(ctx, "beta", "x@y.z", "member", "alice@example.com", 3600); err != nil {
		t.Fatal(err)
	}
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom, failDelete: []string{OrgInvitePrefix("beta")}}
	s3 := New(ks, config.Defaults())
	s3.Now = testClock
	s3.Repos = func(ctx context.Context) ([][2]string, error) { return nil, nil }
	if err := s3.DeleteOrg(ctx, "beta"); err == nil {
		t.Error("invite delete failure must surface")
	}
}

func TestTeamCacheNilAndEvict(t *testing.T) {
	ctx := context.Background()
	s := &Service{Store: store.NewMemory(), Cfg: config.Defaults(), Now: testClock}
	if _, _, err := s.GetTeam(ctx, "o", "t"); err != nil {
		t.Errorf("nil team cache read: %v", err)
	}
	s.teams = nil
	s.teams.invalidate("o", "t")
	if _, _, err := New(store.NewMemory(), config.Defaults()).GetTeam(ctx, "o", "t"); err != nil {
		t.Errorf("fresh team read: %v", err)
	}
	// Eviction over a cap-1 team cache.
	s2 := testService()
	if _, err := s2.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CreateTeam(ctx, "acme", "one", "O", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CreateTeam(ctx, "acme", "two", "T", ""); err != nil {
		t.Fatal(err)
	}
	s2.teams = newTeamCache(1)
	if _, _, err := s2.GetTeam(ctx, "acme", "one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.GetTeam(ctx, "acme", "two"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.GetTeam(ctx, "acme", "one"); err != nil {
		t.Fatal(err)
	}
}

func TestInviteLookupEdges(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	// findInvite inner-GET error (inbox ok, object unreadable).
	inv, err := s.CreateOrgInvite(ctx, "acme", "edge@example.com", "member", "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom, failGet: []string{OrgInviteKey("acme", inv.ID)}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	if _, err := s2.findInvite(ctx, "edge@example.com", inv.ID); err == nil {
		t.Error("inner GET error must surface")
	}
	// Token-match preview for a non-subject inbox holder.
	if err := s.inboxAdd(ctx, "mallory@example.com", InboxEntry{ID: inv.ID, Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.GetBytes(ctx, s.Store, OrgInviteKey("acme", inv.ID), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	full, err := parseInvite(raw)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := s.PreviewInvite(ctx, "mallory@example.com", inv.ID, full.Token)
	if err != nil || prev.Token != "" || prev.Subject != "edge@example.com" {
		t.Errorf("token preview: %v %+v", err, prev)
	}
	hm := testHandler(s, authPrincipal("mallory@example.com"))
	if w := doReq(hm, "GET", "/api/v1/invitations/"+inv.ID+"?token="+full.Token, ""); w.Code != http.StatusOK {
		t.Errorf("HTTP token preview = %d", w.Code)
	}
	// Non-subject accept → 403 even with a valid token-less inbox entry.
	if w := doReq(hm, "POST", "/api/v1/invitations/"+inv.ID+"/accept", ""); w.Code != http.StatusForbidden {
		t.Errorf("non-subject accept = %d", w.Code)
	}
	// Accept with failing profile ensure → error surfaces.
	ks2 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failPut: []string{"users/"}}
	s3 := New(ks2, config.Defaults())
	s3.Now = testClock
	inv2, err := s.CreateRepoInvite(ctx, "acme", "repo", "profail@example.com", RoleRead, "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	_ = inv2
	// The invite object + inbox live in s.Store; s3 shares it but profile
	// writes fail.
	if _, err := s3.AcceptInvite(ctx, "profail@example.com", inv2.ID); err == nil {
		t.Error("profile-ensure failure must surface")
	}
	// Crafted invalid role in a repo invite → addBinding normalization fails.
	bad := &Invitation{Version: 1, ID: "badrole", Token: "t", Kind: InviteRepo, Org: "acme",
		Repo: "acme/repo", Role: "super", Subject: "profail@example.com", State: "pending",
		CreatedAt: "2026-09-03T12:00:00Z", ExpiresAt: "2999-01-01T00:00:00Z"}
	if _, err := store.PutBytes(ctx, s.Store, RepoInviteKey("acme", "repo", "badrole"), encodeInvite(bad),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(ctx, "profail@example.com", InboxEntry{ID: "badrole", Repo: "acme/repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, "profail@example.com", "badrole"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid invite role: %v", err)
	}
	// Crafted invalid org role → rejected before any roster write.
	badOrg := &Invitation{Version: 1, ID: "badorgrole", Token: "t", Kind: InviteOrg, Org: "acme",
		Role: "super", Subject: "profail@example.com", State: "pending",
		CreatedAt: "2026-09-03T12:00:00Z", ExpiresAt: "2999-01-01T00:00:00Z"}
	if _, err := store.PutBytes(ctx, s.Store, OrgInviteKey("acme", "badorgrole"), encodeInvite(badOrg),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(ctx, "profail@example.com", InboxEntry{ID: "badorgrole", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, "profail@example.com", "badorgrole"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid org invite role: %v", err)
	}
	// List skips vanished objects, surfaces hard errors.
	s4 := New(&notFoundStore{ObjectStore: s.Store, keys: map[string]bool{RepoInviteKey("acme", "repo", inv2.ID): true}}, config.Defaults())
	if list, err := s4.ListRepoInvites(ctx, "acme", "repo"); err != nil {
		t.Errorf("vanished invite skip: %v", err)
	} else {
		for _, e := range list {
			if e.ID == inv2.ID {
				t.Errorf("vanished invite listed: %+v", list)
			}
		}
	}
	ks3 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failGet: []string{RepoInvitePrefix("acme", "repo")}}
	s5 := New(ks3, config.Defaults())
	// A live invite must exist for the inner GET to fail on.
	if _, err := s.CreateRepoInvite(ctx, "acme", "repo", "lister@example.com", RoleRead, "alice@example.com", 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := s5.ListRepoInvites(ctx, "acme", "repo"); err == nil {
		t.Error("list inner error must surface")
	}
	ks4 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failGet: []string{OrgInvitePrefix("acme")}}
	s6 := New(ks4, config.Defaults())
	if _, err := s6.ListOrgInvites(ctx, "acme"); err == nil {
		t.Error("org list inner error must surface")
	}
}

func TestEnsureProfileRace(t *testing.T) {
	ctx := context.Background()
	// GetProfile misses, the Create 412s, the re-read wins.
	mem := store.NewMemory()
	inner := &flakyGetStore{ObjectStore: mem, failOnce: map[string]bool{ProfileKey("racer@x.c"): true}, err: store.NewNotFound(ProfileKey("racer@x.c"))}
	if _, err := store.PutBytes(ctx, mem, ProfileKey("racer@x.c"), encodeProfile(&Profile{Version: 1, Principal: "racer@x.c"}),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	s2 := New(inner, config.Defaults())
	s2.Now = testClock
	p, err := s2.EnsureProfile(ctx, "racer@x.c")
	if err != nil || p == nil || p.Principal != "racer@x.c" {
		t.Errorf("ensure race: %v %+v", err, p)
	}
}

func TestOrgCreateProfileError(t *testing.T) {
	s := testService()
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom, failPut: []string{"users/"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	h := testHandler(s2, alice)
	if w := doReq(h, "POST", "/api/v1/orgs", `{"org":"failprof"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("org-create profile error = %d", w.Code)
	}
}

func TestScopedDeleteErrors(t *testing.T) {
	s := testService()
	mustOrg(t, s)
	h := testHandler(s, admin)
	// Scoped org DELETE: GetBytes ok but Delete fails.
	w := doReq(h, "POST", "/api/v1/orgs/acme/invitations", `{"email":"doomed@example.com","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("invite = %d", w.Code)
	}
	entries, _ := s.MyInvites(reqCtx(), "doomed@example.com")
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom, failDelete: []string{OrgInviteKey("acme", entries[0].ID)}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	if w := doReq(testHandler(s2, admin), "DELETE", "/api/v1/orgs/acme/invitations/"+entries[0].ID, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("scoped org delete error = %d", w.Code)
	}
	// Scoped repo DELETE: GetBytes ok but Delete fails.
	inv, err := s.CreateRepoInvite(reqCtx(), "acme", "repo", "doomed2@example.com", RoleRead, "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	ks2 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failDelete: []string{RepoInviteKey("acme", "repo", inv.ID)}}
	s3 := New(ks2, config.Defaults())
	s3.Now = testClock
	if w := doReq(testHandler(s3, admin), "DELETE", "/acme/repo/api/invitations/"+inv.ID, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("scoped repo delete error = %d", w.Code)
	}
	// Deep invitations path does not route.
	if w := doReq(h, "GET", "/api/v1/orgs/acme/invitations/a/b", ""); w.Code != http.StatusNotFound {
		t.Errorf("deep org invitations = %d", w.Code)
	}
	// Bare lane roots do not route.
	for _, target := range []string{"/acme/repo/api", "/acme/repo/api-browser"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		rw := httptest.NewRecorder()
		if h.Handle(rw, r) {
			t.Errorf("Handle(%q) = true", target)
		}
	}
	// PUT access without role_bindings defaults to [].
	w = doReq(h, "PUT", "/acme/repo2/api/access", `{"version":0,"visibility":"public"}`)
	if w.Code != http.StatusOK {
		t.Errorf("PUT without bindings = %d: %s", w.Code, w.Body.String())
	}
	// Non-owner org-invite list → 403.
	hm := testHandler(s, authPrincipal("mallory@example.com"))
	if w := doReq(hm, "GET", "/api/v1/orgs/acme/invitations", ""); w.Code != http.StatusForbidden {
		t.Errorf("member org-invite list = %d", w.Code)
	}
	// POST /invitations/{id} (no accept) → 405.
	if w := doReq(h, "POST", "/api/v1/invitations/x", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST invite = %d", w.Code)
	}
	// Anonymous accept/decline → 401.
	hanon := testHandler(s, anon)
	if w := doReq(hanon, "POST", "/api/v1/invitations/x/accept", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon accept = %d", w.Code)
	}
	if w := doReq(hanon, "DELETE", "/api/v1/invitations/x", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon decline = %d", w.Code)
	}
	// CreateOrgInvite on a store-failing roster read.
	ks3 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failGet: []string{MembersKey("acme")}}
	s4 := New(ks3, config.Defaults())
	if _, err := s4.CreateOrgInvite(reqCtx(), "acme", "x@y.z", "member", "a@b.c", 3600); err == nil {
		t.Error("roster read failure must surface")
	}
	// Create with failing randomness after the roster check.
	s5 := testService()
	mustOrg(t, s5)
	s5.Rand = failReader{}
	if _, err := s5.CreateOrgInvite(reqCtx(), "acme", "x@y.z", "member", "a@b.c", 3600); err == nil {
		t.Error("org invite rand failure must surface")
	}
	if _, err := s5.CreateRepoInvite(reqCtx(), "acme", "r", "x@y.z", RoleRead, "a@b.c", 3600); err == nil {
		t.Error("repo invite rand failure must surface")
	}
	// Owner check on a ghost org fails (non-admin).
	if s.CheckOrgOwner(reqCtx(), "ghost", alice) == nil {
		t.Error("ghost owner check must fail")
	}
	// Mine with failing store → 503.
	s6 := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom, putErr: errBoom, delErr: errBoom, listErr: errBoom}, config.Defaults())
	if w := doReq(testHandler(s6, admin), "GET", "/api/v1/invitations", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("mine error = %d", w.Code)
	}
	// Preview/accept/decline with failing store → 503.
	if w := doReq(testHandler(s6, admin), "GET", "/api/v1/invitations/x", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("preview error = %d", w.Code)
	}
	if w := doReq(testHandler(s6, admin), "POST", "/api/v1/invitations/x/accept", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("accept error = %d", w.Code)
	}
	if w := doReq(testHandler(s6, admin), "DELETE", "/api/v1/invitations/x", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("decline error = %d", w.Code)
	}
	// Org invite list/create with failing store → 503.
	if w := doReq(testHandler(s6, admin), "GET", "/api/v1/orgs/acme/invitations", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("org invite list error = %d", w.Code)
	}
	if w := doReq(testHandler(s6, admin), "POST", "/api/v1/orgs/acme/invitations", `{"email":"a@b.c","role":"member"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("org invite create error = %d", w.Code)
	}
	if w := doReq(testHandler(s6, admin), "DELETE", "/api/v1/orgs/acme/invitations/x", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("org invite delete error = %d", w.Code)
	}
	// Repo invite list/create/delete with failing store → 503.
	if w := doReq(testHandler(s6, admin), "GET", "/acme/repo/api/invitations", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("repo invite list error = %d", w.Code)
	}
	if w := doReq(testHandler(s6, admin), "POST", "/acme/repo/api/invitations", `{"subject":"a@b.c","role":"read"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("repo invite create error = %d", w.Code)
	}
	if w := doReq(testHandler(s6, admin), "DELETE", "/acme/repo/api/invitations/x", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("repo invite delete error = %d", w.Code)
	}
}
