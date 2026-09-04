package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// failAuthHandler yields an auth error for every request.
func failAuthHandler(s *Service) *Handler {
	return &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad cred"}
	}}
}

func TestAuthErrorEveryRoute(t *testing.T) {
	s := testService()
	h := failAuthHandler(s)
	routes := []struct{ method, target, body string }{
		{"GET", "/api/v1/users/jane%40example.com", ""},
		{"PUT", "/api/v1/users/jane%40example.com", `{}`},
		{"GET", "/api/v1/orgs", ""},
		{"POST", "/api/v1/orgs", `{}`},
		{"GET", "/api/v1/orgs/acme", ""},
		{"PUT", "/api/v1/orgs/acme", `{}`},
		{"DELETE", "/api/v1/orgs/acme", ""},
		{"GET", "/api/v1/orgs/acme/members", ""},
		{"GET", "/api/v1/orgs/acme/members/a%40b.c", ""},
		{"PUT", "/api/v1/orgs/acme/members/a%40b.c", `{}`},
		{"DELETE", "/api/v1/orgs/acme/members/a%40b.c", ""},
		{"GET", "/api/v1/orgs/acme/teams", ""},
		{"POST", "/api/v1/orgs/acme/teams", `{}`},
		{"GET", "/api/v1/orgs/acme/teams/s", ""},
		{"PUT", "/api/v1/orgs/acme/teams/s", `{}`},
		{"DELETE", "/api/v1/orgs/acme/teams/s", ""},
		{"PUT", "/api/v1/orgs/acme/teams/s/members/a%40b.c", ""},
		{"DELETE", "/api/v1/orgs/acme/teams/s/members/a%40b.c", ""},
		{"GET", "/api/v1/invitations", ""},
		{"GET", "/api/v1/invitations/x", ""},
		{"POST", "/api/v1/invitations/x/accept", ""},
		{"DELETE", "/api/v1/invitations/x", ""},
		{"GET", "/api/v1/orgs/acme/invitations", ""},
		{"POST", "/api/v1/orgs/acme/invitations", `{}`},
		{"DELETE", "/api/v1/orgs/acme/invitations/x", ""},
		{"GET", "/acme/repo/api/access", ""},
		{"PUT", "/acme/repo/api/access", `{}`},
		{"GET", "/acme/repo/api/invitations", ""},
		{"POST", "/acme/repo/api/invitations", `{}`},
		{"DELETE", "/acme/repo/api/invitations/x", ""},
	}
	for _, tc := range routes {
		w := doReq(h, tc.method, tc.target, tc.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.target, w.Code)
			continue
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: 401 must carry Bearer", tc.method, tc.target)
		}
	}
}

// badJSONHandler uses real auth; every write route gets malformed JSON.
func TestBadJSONEveryWriteRoute(t *testing.T) {
	s := testService()
	mustOrg(t, s)
	h := testHandler(s, admin)
	routes := []struct{ method, target string }{
		{"POST", "/api/v1/orgs"},
		{"PUT", "/api/v1/orgs/acme"},
		{"PUT", "/api/v1/orgs/acme/members/a%40b.c"},
		{"POST", "/api/v1/orgs/acme/teams"},
		{"PUT", "/api/v1/orgs/acme/teams/s"},
		{"POST", "/api/v1/orgs/acme/invitations"},
		{"PUT", "/api/v1/users/a%40b.c"},
		{"PUT", "/acme/repo/api/access"},
		{"POST", "/acme/repo/api/invitations"},
	}
	for _, tc := range routes {
		if w := doReq(h, tc.method, tc.target, `{bad`); w.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", tc.method, tc.target, w.Code)
		}
	}
}

func TestRoutingEdges(t *testing.T) {
	s := testService()
	h := testHandler(s, admin)
	for _, target := range []string{
		"/api/v1/users",
		"/api/v1/users/a/b",
		"/api/v1/orgs/acme/teams/platform/members",
		"/api/v1/orgs/acme/teams/platform/members/a/b",
	} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		if h.Handle(w, r) {
			t.Errorf("Handle(%q) = true, want false", target)
		}
	}
	// Teams collection only answers GET/POST.
	mustOrg(t, s)
	if w := doReq(h, "PUT", "/api/v1/orgs/acme/teams", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT teams = %d", w.Code)
	}
	// One-member GET on unknown org → 404.
	if w := doReq(h, "GET", "/api/v1/orgs/ghost/members/a%40b.c", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost member = %d", w.Code)
	}
	// Team PUT/DELETE as non-owner → 403.
	if _, err := s.CreateTeam(reqCtx(), "acme", "s", "S", ""); err != nil {
		t.Fatal(err)
	}
	hm := testHandler(s, carol)
	if _, err := s.SetMember(reqCtx(), "acme", "carol@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	if w := doReq(hm, "PUT", "/api/v1/orgs/acme/teams/s", `{"name":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("member PUT team = %d", w.Code)
	}
	if w := doReq(hm, "DELETE", "/api/v1/orgs/acme/teams/s", ""); w.Code != http.StatusForbidden {
		t.Errorf("member DELETE team = %d", w.Code)
	}
	// Repo invite POST as non-admin → 403.
	if w := doReq(hm, "POST", "/acme/repo/api/invitations", `{"subject":"x@y.z","role":"read"}`); w.Code != http.StatusForbidden {
		t.Errorf("member POST repo invite = %d", w.Code)
	}
	// Invite method edges.
	if w := doReq(h, "GET", "/api/v1/invitations/x/accept", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET accept = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/invitations/x", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST invite = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/api/v1/invitations/x", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT invite = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/api/v1/invitations/x/accept", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT accept = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/invitations", ""); w.Code != http.StatusBadRequest {
		t.Errorf("empty POST org invite = %d", w.Code)
	}
}

func TestFlaglessReaderGate(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	mustAccess(t, s, "acme", "priv", VisibilityPrivate, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
	})
	// Flag-less team member passes CheckRead via the binding.
	teamy := authPrincipal("teamy@example.com")
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "teamy@example.com"); err != nil {
		t.Fatal(err)
	}
	if aerr := s.CheckRead(ctx, "acme", "priv", teamy); aerr != nil {
		t.Errorf("bound flag-less reader denied: %v", aerr)
	}
	// Maintain-level role expansion skips below-want bindings.
	exp, w := s.ExpandMembers(ctx, []string{"role:acme/priv:maintain"})
	if len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	if len(exp) != 1 || exp[0] != "alice@example.com" {
		t.Errorf("maintain expansion = %v (org owner only)", exp)
	}
}

func TestNilCachesAndEviction(t *testing.T) {
	ctx := context.Background()
	s := &Service{Store: store.NewMemory(), Cfg: config.Defaults(), Now: testClock}
	// Successful read with nil caches hits the set-nil guard.
	if _, err := store.PutBytes(ctx, s.Store, AccessKey("o", "r"),
		encodeAccess(&AccessDoc{Version: 1, Visibility: VisibilityPublic}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetAccess(ctx, "o", "r"); err != nil {
		t.Errorf("nil-cache read: %v", err)
	}
	// PUT with nil caches hits the invalidate-nil guard.
	if _, err := s.PutAccess(ctx, "o", "r", "", VisibilityPrivate, nil); err != nil {
		t.Errorf("nil-cache write: %v", err)
	}
	// Eviction: two distinct reads over a cap-1 cache.
	s2 := New(store.NewMemory(), config.Defaults())
	s2.access = newAccessCache(1)
	s2.teams = newTeamCache(1)
	mustAccess(t, s2, "a", "one", VisibilityPublic, nil)
	mustAccess(t, s2, "a", "two", VisibilityPublic, nil)
	if _, _, err := s2.GetAccess(ctx, "a", "one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.GetAccess(ctx, "a", "two"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.GetAccess(ctx, "a", "one"); err != nil {
		t.Fatal(err)
	}
}

func TestInviteServiceEdges(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	// Invite PUT ok but inbox PUT fails → error surfaces.
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom, failPut: []string{"users/vic"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	if _, err := s2.CreateOrgInvite(ctx, "acme", "vic@example.com", "member", "alice@example.com", 3600); !strings.Contains(errText2(err), "boom") {
		t.Errorf("inbox failure must surface: %v", err)
	}
	ks2 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failPut: []string{"users/rick"}}
	s3 := New(ks2, config.Defaults())
	s3.Now = testClock
	if _, err := s3.CreateRepoInvite(ctx, "acme", "r", "rick@example.com", RoleRead, "alice@example.com", 3600); err == nil {
		t.Error("repo inbox failure must surface")
	}
	// Crafted inbox mismatch: mallory holds an entry for vic's invite.
	inv, err := s.CreateOrgInvite(ctx, "acme", "vic2@example.com", "member", "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(ctx, "mallory@example.com", InboxEntry{ID: inv.ID, Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	hm := testHandler(s, authPrincipal("mallory@example.com"))
	if w := doReq(hm, "DELETE", "/api/v1/invitations/"+inv.ID, ""); w.Code != http.StatusForbidden {
		t.Errorf("non-subject decline = %d", w.Code)
	}
	// PutProfile store error.
	s4 := New(&errStore{ObjectStore: store.NewMemory(), putErr: errBoom}, config.Defaults())
	if _, err := s4.PutProfile(ctx, "x@y.z", "n", ""); !isBoom(err) {
		t.Errorf("PutProfile error: %v", err)
	}
	// SetMember on corrupt roster via path already covered; org create with
	// failing members seed covered in edge_test; ensure owner check on
	// unknown org is false.
	if s.CheckOrgOwner(ctx, "ghost", alice) == nil {
		t.Error("owner check on ghost org must fail")
	}
}

func errText2(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func isBoom(err error) bool { return err != nil && strings.Contains(err.Error(), "boom") }
