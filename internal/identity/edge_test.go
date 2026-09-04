package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	urlpkg "net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// keyFailStore fails operations on matching key prefixes.
type keyFailStore struct {
	store.ObjectStore
	failGet    []string
	failPut    []string
	failDelete []string
	err        error
}

func (k *keyFailStore) hit(key string, list []string) bool {
	for _, p := range list {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func (k *keyFailStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if k.hit(key, k.failGet) {
		return nil, k.err
	}
	return k.ObjectStore.Get(ctx, key, opts)
}

func (k *keyFailStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if k.hit(key, k.failPut) {
		return store.ObjectMeta{}, k.err
	}
	return k.ObjectStore.Put(ctx, key, body, opts)
}

func (k *keyFailStore) Delete(ctx context.Context, key string, v store.Version) error {
	if k.hit(key, k.failDelete) {
		return k.err
	}
	return k.ObjectStore.Delete(ctx, key, v)
}

// failBodyStore returns an Object with a failing body for one key.
type failBodyStore struct {
	store.ObjectStore
	key string
}

type failBody struct{}

func (failBody) Read([]byte) (int, error) { return 0, errBoom }
func (failBody) Close() error             { return nil }

func (f *failBodyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == f.key {
		return store.Object{Meta: store.ObjectMeta{Key: key, Version: "v1"}, Body: failBody{}}, nil
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

// oddStore is unused: store.GetResult is sealed (unexported method), so
// only NotModified and Object exist and the defensive error returns in
// GetAccess/GetTeam are unreachable by construction (kept for safety).

// conflictStore loses every CAS.
type conflictStore struct{ store.ObjectStore }

func (c *conflictStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutUpdate {
		return store.ObjectMeta{}, store.NewPrecondition(key, "other")
	}
	return c.ObjectStore.Put(ctx, key, body, opts)
}

func TestWriteErrMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, http.StatusForbidden},
		{&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, http.StatusServiceUnavailable},
		{&auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}, http.StatusUnauthorized},
		{&auth.AuthError{Kind: auth.ErrUnauthorized, Why: "auth"}, http.StatusUnauthorized},
		{ErrInvalid, http.StatusBadRequest},
		{errBoom, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		writeErr(w, tc.err)
		if w.Code != tc.code {
			t.Errorf("writeErr(%v) = %d, want %d", tc.err, w.Code, tc.code)
		}
	}
	// writeJSON encode failure.
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, func() {})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("writeJSON func = %d", w.Code)
	}
	// matchETag variants.
	if !matchETag("*", "x") || !matchETag(`"x", "y"`, "y") || !matchETag("W/\"x\"", "x") {
		t.Error("matchETag positive broken")
	}
	if matchETag("", "x") || matchETag(`"y"`, "x") {
		t.Error("matchETag negative broken")
	}
	// writeCached 304 path.
	w2 := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("If-None-Match", `"e1"`)
	writeCached(w2, r, ccSWR, "e1", http.StatusOK, map[string]int{"a": 1})
	if w2.Code != http.StatusNotModified {
		t.Errorf("304 = %d", w2.Code)
	}
	// readBodyJSON oversized.
	big := strings.Repeat("x", 65<<10)
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(big))
	if readBodyJSON(w3, r3, 64<<10, &struct{}{}) {
		t.Error("oversized body must fail")
	}
	if w3.Code != http.StatusBadRequest {
		t.Errorf("oversized = %d", w3.Code)
	}
	// principal() fallbacks.
	if p, _ := (&Handler{}).principal(httptest.NewRequest(http.MethodGet, "/", nil)); !p.Anonymous {
		t.Errorf("nil handler principal = %+v", p)
	}
	s := testService()
	r4 := httptest.NewRequest(http.MethodGet, "/", nil)
	if p, _ := (&Handler{Svc: s}).principal(r4); !p.Write || !p.Admin {
		t.Errorf("none-mode default = %+v", p)
	}
	// splitPath bad-encoding fallback: craft the URL directly since
	// httptest.NewRequest rejects malformed escapes.
	r5 := &http.Request{URL: &urlpkg.URL{Path: "/api/v1/users/%zz"}}
	if got := splitPath(r5); len(got) != 4 || got[3] != "%zz" {
		t.Errorf("splitPath fallback = %v", got)
	}
}

func TestHandlerStoreErrors(t *testing.T) {
	newErrSvc := func() (*Service, *Handler) {
		s := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom, putErr: errBoom, delErr: errBoom, listErr: errBoom}, config.Defaults())
		s.Now = testClock
		return s, testHandler(s, admin)
	}
	_, h := newErrSvc()
	cases := []struct{ method, target, body string }{
		{"GET", "/api/v1/users/jane%40example.com", ""},
		{"PUT", "/api/v1/users/jane%40example.com", `{}`},
		{"GET", "/api/v1/orgs", ""},
		{"POST", "/api/v1/orgs", `{"org":"acme"}`},
	}
	for _, tc := range cases {
		if w := doReq(h, tc.method, tc.target, tc.body); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.target, w.Code)
		}
	}
}

func TestOrgHandlerErrors(t *testing.T) {
	s := testService()
	mustOrg(t, s)
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom,
		failGet:    []string{"orgs/acme/org.json", "orgs/acme/members.json"},
		failPut:    []string{"orgs/acme/org.json"},
		failDelete: []string{"orgs/acme/org.json"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	h := testHandler(s2, admin)
	if w := doReq(h, "GET", "/api/v1/orgs/acme", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET org err = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/api/v1/orgs/acme", `{"display_name":"x"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT org err = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("DELETE org err = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/acme/members", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET members err = %d", w.Code)
	}
	// Anon org reads when closed.
	s2.Cfg.Server.Auth.AnonymousRead = false
	ha := testHandler(s2, anon)
	for _, target := range []string{"/api/v1/orgs", "/api/v1/orgs/acme", "/api/v1/orgs/acme/members",
		"/api/v1/orgs/acme/members/alice%40example.com", "/api/v1/orgs/acme/teams", "/api/v1/orgs/acme/teams/platform"} {
		if w := doReq(ha, "GET", target, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("anon GET %s = %d", target, w.Code)
		}
	}
	// Auth unavailable maps to 503.
	hb := &Handler{Svc: s2, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}}
	if w := doReq(hb, "GET", "/api/v1/orgs", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unavailable = %d", w.Code)
	}
}

func TestMemberTeamHandlerErrors(t *testing.T) {
	s := testService()
	mustOrg(t, s)
	if _, err := s.CreateTeam(reqCtx(), "acme", "platform", "P", ""); err != nil {
		t.Fatal(err)
	}
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom,
		failGet:    []string{"orgs/acme/members.json", "orgs/acme/teams/"},
		failPut:    []string{"orgs/acme/members.json", "orgs/acme/teams/"},
		failDelete: []string{"orgs/acme/teams/"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	h := testHandler(s2, admin)
	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/api/v1/orgs/acme/members/alice%40example.com", ""},
		{"PUT", "/api/v1/orgs/acme/members/x%40y.z", `{"role":"member"}`},
		{"DELETE", "/api/v1/orgs/acme/members/alice%40example.com", ""},
		{"GET", "/api/v1/orgs/acme/teams", ""},
		{"POST", "/api/v1/orgs/acme/teams", `{"slug":"s"}`},
		{"GET", "/api/v1/orgs/acme/teams/platform", ""},
		{"PUT", "/api/v1/orgs/acme/teams/platform", `{"name":"x"}`},
		{"DELETE", "/api/v1/orgs/acme/teams/platform", ""},
		{"PUT", "/api/v1/orgs/acme/teams/platform/members/x%40y.z", ""},
		{"DELETE", "/api/v1/orgs/acme/teams/platform/members/x%40y.z", ""},
	} {
		if w := doReq(h, tc.method, tc.target, tc.body); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.target, w.Code)
		}
	}
	if w := doReq(h, "PUT", "/api/v1/orgs/acme/teams/platform", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("PUT team bad json = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/teams", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("POST team bad json = %d", w.Code)
	}
	// EnsureProfile failure on member PUT (users/ writes fail).
	s3 := testService()
	mustOrg(t, s3)
	ks3 := &keyFailStore{ObjectStore: s3.Store, err: errBoom, failPut: []string{"users/"}}
	s4 := New(ks3, config.Defaults())
	s4.Now = testClock
	if w := doReq(testHandler(s4, admin), "PUT", "/api/v1/orgs/acme/members/fresh%40x.c", `{"role":"member"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("EnsureProfile err = %d", w.Code)
	}
}

func TestAccessHandlerErrors(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom,
		failGet: []string{"repos/acme/repo/access.json"},
		failPut: []string{"repos/acme/repo/access.json"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	h := testHandler(s2, admin)
	if w := doReq(h, "GET", "/acme/repo/api/access", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET access err = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/acme/repo/api/access", `{"version":0,"visibility":"public","role_bindings":[]}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT access err = %d", w.Code)
	}
	// PUT with nonzero version on missing doc → 409.
	if w := doReq(testHandler(s, admin), "PUT", "/acme/fresh/api/access", `{"version":3,"visibility":"public","role_bindings":[]}`); w.Code != http.StatusConflict {
		t.Errorf("PUT versioned missing = %d", w.Code)
	}
	// POST on an invite id → 405.
	if w := doReq(testHandler(s, admin), "POST", "/acme/repo/api/invitations/x", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST invite id = %d", w.Code)
	}
}

func TestRepoInviteHandlerErrors(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom,
		failGet:    []string{"repos/acme/repo/meta/invitations/"},
		failPut:    []string{"repos/acme/repo/meta/invitations/"},
		failDelete: []string{"repos/acme/repo/meta/invitations/"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	h := testHandler(s2, admin)
	// Seed one invite directly in the wrapped store.
	inv, err := s.CreateRepoInvite(reqCtx(), "acme", "repo", "x@y.z", RoleRead, "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	_ = inv
	if w := doReq(h, "GET", "/acme/repo/api/invitations", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET invites err = %d", w.Code)
	}
	if w := doReq(h, "POST", "/acme/repo/api/invitations", `{"subject":"y@z.c","role":"read"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("POST invite err = %d", w.Code)
	}
	if w := doReq(h, "POST", "/acme/repo/api/invitations", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("POST invite bad json = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/acme/repo/api/invitations/nonexistent", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("DELETE invite get err = %d", w.Code)
	}
	// Corrupt invite object → invalid.
	bad := []byte("{x")
	if _, err := store.PutBytes(reqCtx(), s.Store, RepoInviteKey("acme", "repo", "corrupt"), bad, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	ks2 := &keyFailStore{ObjectStore: s.Store, err: errBoom}
	s3 := New(ks2, config.Defaults())
	s3.Now = testClock
	if w := doReq(testHandler(s3, admin), "DELETE", "/acme/repo/api/invitations/corrupt", ""); w.Code != http.StatusBadRequest {
		t.Errorf("DELETE corrupt invite = %d", w.Code)
	}
	// Delete failure → 503.
	ks3 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failDelete: []string{"repos/acme/repo/meta/invitations/good"}}
	if _, err := store.PutBytes(reqCtx(), s.Store, RepoInviteKey("acme", "repo", "good"), encodeInvite(&Invitation{ID: "good", Subject: "x@y.z"}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	s5 := New(ks3, config.Defaults())
	s5.Now = testClock
	if w := doReq(testHandler(s5, admin), "DELETE", "/acme/repo/api/invitations/good", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("DELETE invite del err = %d", w.Code)
	}
}

func TestOrgInviteHandlerErrors(t *testing.T) {
	s := testService()
	mustOrg(t, s)
	h := testHandler(s, admin)
	if w := doReq(h, "POST", "/api/v1/orgs/acme/invitations", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("bad json = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/invitations", `{"email":"not-an-email","role":"member"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad email = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme/invitations", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE collection = %d", w.Code)
	}
	// Owner cancels a pending invite via the scoped DELETE.
	w := doReq(h, "POST", "/api/v1/orgs/acme/invitations", `{"email":"gone@example.com","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("invite = %d", w.Code)
	}
	entries, err := s.MyInvites(reqCtx(), "gone@example.com")
	if err != nil || len(entries) != 1 {
		t.Fatal("inbox")
	}
	w = doReq(h, "DELETE", "/api/v1/orgs/acme/invitations/"+entries[0].ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("scoped cancel = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme/invitations/"+entries[0].ID, ""); w.Code != http.StatusNotFound {
		t.Errorf("re-cancel = %d", w.Code)
	}
	// Corrupt invite object → invalid on scoped delete.
	if _, err := store.PutBytes(reqCtx(), s.Store, OrgInviteKey("acme", "corrupt"), []byte("{x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme/invitations/corrupt", ""); w.Code != http.StatusBadRequest {
		t.Errorf("corrupt scoped delete = %d", w.Code)
	}
	// Store error paths.
	ks := &keyFailStore{ObjectStore: s.Store, err: errBoom,
		failGet:    []string{"orgs/acme/invitations/"},
		failPut:    []string{"orgs/acme/invitations/"},
		failDelete: []string{"orgs/acme/invitations/good"}}
	s2 := New(ks, config.Defaults())
	s2.Now = testClock
	h2 := testHandler(s2, admin)
	if w := doReq(h2, "GET", "/api/v1/orgs/acme/invitations", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET org invites err = %d", w.Code)
	}
	if w := doReq(h2, "POST", "/api/v1/orgs/acme/invitations", `{"email":"a@b.c","role":"member"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("POST org invite err = %d", w.Code)
	}
	if _, err := store.PutBytes(reqCtx(), s.Store, OrgInviteKey("acme", "good"), encodeInvite(&Invitation{ID: "good", Subject: "a@b.c"}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if w := doReq(h2, "DELETE", "/api/v1/orgs/acme/invitations/good", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("scoped delete err = %d", w.Code)
	}
	if w := doReq(h2, "DELETE", "/api/v1/orgs/acme/invitations/missing", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("scoped delete get err = %d", w.Code)
	}
}

func TestTopInviteErrors(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	h := testHandler(s, admin)
	// Unknown id preview/accept/decline → 409.
	if w := doReq(h, "GET", "/api/v1/invitations/nope", ""); w.Code != http.StatusConflict {
		t.Errorf("preview unknown = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/invitations/nope/accept", ""); w.Code != http.StatusConflict {
		t.Errorf("accept unknown = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/api/v1/invitations/nope", ""); w.Code != http.StatusConflict {
		t.Errorf("decline unknown = %d", w.Code)
	}
	// Decline by a non-subject → forbidden.
	w := doReq(h, "POST", "/api/v1/orgs/acme/invitations", `{"email":"vic@example.com","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("invite = %d", w.Code)
	}
	entries, _ := s.MyInvites(reqCtx(), "vic@example.com")
	ho := testHandler(s, stranger)
	if w := doReq(ho, "DELETE", "/api/v1/invitations/"+entries[0].ID, ""); w.Code != http.StatusConflict && w.Code != http.StatusForbidden {
		t.Errorf("decline non-subject = %d", w.Code)
	}
	// Wrong method on accept path.
	if w := doReq(h, "PUT", "/api/v1/invitations/"+entries[0].ID+"/accept", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT accept = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/api/v1/invitations/"+entries[0].ID, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT invite = %d", w.Code)
	}
	// Accept with org-side failure: invite references a deleted org.
	bad := &Invitation{Version: 1, ID: "orggone", Token: "t", Kind: InviteOrg, Org: "gone", Role: "member",
		Subject: "vic@example.com", State: "pending", ExpiresAt: "2999-01-01T00:00:00Z"}
	if _, err := store.PutBytes(reqCtx(), s.Store, OrgInviteKey("gone", "orggone"), encodeInvite(bad), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(reqCtx(), "vic@example.com", InboxEntry{ID: "orggone", Org: "gone"}); err != nil {
		t.Fatal(err)
	}
	hv := testHandler(s, authPrincipal("vic@example.com"))
	if w := doReq(hv, "POST", "/api/v1/invitations/orggone/accept", ""); w.Code != http.StatusNotFound {
		t.Errorf("accept deleted org = %d", w.Code)
	}
	// Accept with corrupt access.json on the repo path.
	if _, err := store.PutBytes(reqCtx(), s.Store, AccessKey("acme", "corrupt"), []byte("{x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateRepoInvite(reqCtx(), "acme", "corrupt", "vic@example.com", RoleRead, "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if w := doReq(hv, "POST", "/api/v1/invitations/"+inv.ID+"/accept", ""); w.Code != http.StatusBadRequest {
		t.Errorf("accept corrupt access = %d", w.Code)
	}
}

func TestStoreEdgeCases(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	// GetAccess read-body failure.
	mustAccess(t, s, "acme", "r", VisibilityPublic, nil)
	s2 := New(&failBodyStore{ObjectStore: s.Store, key: AccessKey("acme", "body")}, config.Defaults())
	s2.Now = testClock
	if _, err := store.PutBytes(ctx, s.Store, AccessKey("acme", "body"), encodeAccess(&AccessDoc{Version: 1, Visibility: VisibilityPublic}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.GetAccess(ctx, "acme", "body"); !errors.Is(err, errBoom) {
		t.Errorf("failing body: %v", err)
	}
	// GetTeam read-body failure.
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "t", "T", ""); err != nil {
		t.Fatal(err)
	}
	s4 := New(&failBodyStore{ObjectStore: s.Store, key: TeamKey("acme", "t")}, config.Defaults())
	if _, _, err := s4.GetTeam(ctx, "acme", "t"); !errors.Is(err, errBoom) {
		t.Errorf("team failing body: %v", err)
	}
	// CAS loop exhaustion → 409: pre-seed so every attempt is an Update.
	mem := store.NewMemory()
	if _, err := store.PutBytes(ctx, mem, AccessKey("o", "r"), encodeAccess(&AccessDoc{Version: 1, Visibility: VisibilityPublic}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	s6 := New(&conflictStore{ObjectStore: mem}, config.Defaults())
	s6.Now = testClock
	if _, err := s6.PutAccess(ctx, "o", "r", "", VisibilityPublic, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("want conflict, got %v", err)
	}
	// Nil-cache service still works (no LRU).
	s7 := &Service{Store: store.NewMemory(), Cfg: config.Defaults(), Now: testClock}
	if _, _, err := s7.GetAccess(ctx, "o", "r"); err != nil {
		t.Errorf("nil caches GetAccess: %v", err)
	}
	if _, _, err := s7.GetTeam(ctx, "o", "t"); err != nil {
		t.Errorf("nil caches GetTeam: %v", err)
	}
	if hits, misses := s7.access.stats(); hits != 0 || misses != 0 {
		t.Errorf("nil stats = %d/%d", hits, misses)
	}
	// Tiny cache evicts.
	s8 := New(store.NewMemory(), config.Defaults())
	s8.access = newAccessCache(1)
	s8.teams = newTeamCache(1)
	mustAccess(t, s8, "a", "one", VisibilityPublic, nil)
	mustAccess(t, s8, "a", "two", VisibilityPublic, nil)
	if _, _, err := s8.GetAccess(ctx, "a", "one"); err != nil {
		t.Errorf("evicted re-read: %v", err)
	}
	if _, _, err := s8.GetAccess(ctx, "a", "one"); err != nil {
		t.Errorf("cached re-read: %v", err)
	}
	if hits, misses := s8.access.stats(); hits == 0 || misses == 0 {
		t.Errorf("stats must show both: %d/%d", hits, misses)
	}
	// readBody error.
	if _, err := readBody(failBody{}); err == nil {
		t.Error("readBody must fail")
	}
	// nonNilBindings nil.
	if got := nonNilBindings(nil); got == nil || len(got) != 0 {
		t.Errorf("nonNilBindings(nil) = %#v", got)
	}
	// CreateOrg members-seed failure.
	ks := &keyFailStore{ObjectStore: store.NewMemory(), err: errBoom, failPut: []string{"orgs/seed/members.json"}}
	s9 := New(ks, config.Defaults())
	s9.Now = testClock
	if _, err := s9.CreateOrg(ctx, "seed", "S", "", "a@b.c"); !errors.Is(err, errBoom) {
		t.Errorf("members seed error: %v", err)
	}
	// CreateOrg org-PUT failure.
	ks2 := &keyFailStore{ObjectStore: store.NewMemory(), err: errBoom, failPut: []string{"orgs/seed2/org.json"}}
	s10 := New(ks2, config.Defaults())
	if _, err := s10.CreateOrg(ctx, "seed2", "S", "", "a@b.c"); !errors.Is(err, errBoom) {
		t.Errorf("org create error: %v", err)
	}
	// CreateTeam PUT failure.
	s11 := testService()
	if _, err := s11.CreateOrg(ctx, "acme", "A", "", "a@b.c"); err != nil {
		t.Fatal(err)
	}
	ks3 := &keyFailStore{ObjectStore: s11.Store, err: errBoom, failPut: []string{"orgs/acme/teams/"}}
	s12 := New(ks3, config.Defaults())
	if _, err := s12.CreateTeam(ctx, "acme", "t", "T", ""); !errors.Is(err, errBoom) {
		t.Errorf("team create error: %v", err)
	}
	// CreateOrgInvite / CreateRepoInvite store failures + rand failure.
	ks4 := &keyFailStore{ObjectStore: s11.Store, err: errBoom,
		failPut: []string{"orgs/acme/invitations", "repos/acme/r/meta/invitations"}}
	s13 := New(ks4, config.Defaults())
	s13.Now = testClock
	if _, err := s13.CreateOrgInvite(ctx, "acme", "x@y.z", "member", "a@b.c", 3600); !errors.Is(err, errBoom) {
		t.Errorf("org invite put error: %v", err)
	}
	if _, err := s13.CreateRepoInvite(ctx, "acme", "r", "x@y.z", RoleRead, "a@b.c", 3600); !errors.Is(err, errBoom) {
		t.Errorf("repo invite put error: %v", err)
	}
	s14 := testService()
	s14.Rand = failReader{}
	if _, err := s14.CreateOrgInvite(ctx, "acme", "x@y.z", "member", "a@b.c", 3600); err == nil {
		t.Error("org invite rand error must fail")
	}
	if _, err := s14.CreateRepoInvite(ctx, "acme", "r", "x@y.z", RoleRead, "a@b.c", 3600); err == nil {
		t.Error("repo invite rand error must fail")
	}
	// inboxAdd put failure.
	ks5 := &keyFailStore{ObjectStore: store.NewMemory(), err: errBoom, failPut: []string{"users/"}}
	s15 := New(ks5, config.Defaults())
	if err := s15.inboxAdd(ctx, "x@y.z", InboxEntry{ID: "1"}); !errors.Is(err, errBoom) {
		t.Errorf("inboxAdd error: %v", err)
	}
	// CancelInvite delete failure.
	inv2, err := s.CreateOrgInvite(ctx, "acme", "cancelme@example.com", "member", "alice@example.com", 3600)
	if err != nil {
		t.Fatal(err)
	}
	ks6 := &keyFailStore{ObjectStore: s.Store, err: errBoom, failDelete: []string{"orgs/acme/invitations/" + inv2.ID}}
	s16 := New(ks6, config.Defaults())
	s16.Now = testClock
	if _, err := s16.CancelInvite(ctx, "cancelme@example.com", inv2.ID); !errors.Is(err, errBoom) {
		t.Errorf("cancel delete error: %v", err)
	}
	// DeleteOrg delete failure + invite-list failure.
	ks7 := &keyFailStore{ObjectStore: store.NewMemory(), err: errBoom, failDelete: []string{"orgs/del/org.json"}}
	s17 := New(ks7, config.Defaults())
	s17.Now = testClock
	if _, err := s17.CreateOrg(ctx, "del", "D", "", "a@b.c"); err != nil {
		t.Fatal(err)
	}
	s17.Store = &keyFailStore{ObjectStore: s17.Store, err: errBoom, failDelete: []string{"orgs/del/org.json"}}
	if err := s17.DeleteOrg(ctx, "del"); !errors.Is(err, errBoom) {
		t.Errorf("DeleteOrg delete error: %v", err)
	}
	// DeleteTeam: GetBytes error, corrupt skip, put-conflict tolerance.
	ks8 := &keyFailStore{ObjectStore: store.NewMemory(), err: errBoom, failGet: []string{"repos/"}}
	s18 := New(ks8, config.Defaults())
	s18.Repos = func(ctx context.Context) ([][2]string, error) { return [][2]string{{"o", "r"}}, nil }
	if err := s18.DeleteTeam(ctx, "o", "s"); !errors.Is(err, errBoom) {
		t.Errorf("DeleteTeam get error: %v", err)
	}
	s19 := testService()
	if _, err := s19.CreateOrg(ctx, "acme", "A", "", "a@b.c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s19.CreateTeam(ctx, "acme", "t", "T", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, s19.Store, AccessKey("acme", "corrupt"), []byte("{x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	s19.Repos = func(ctx context.Context) ([][2]string, error) { return [][2]string{{"acme", "corrupt"}}, nil }
	if err := s19.DeleteTeam(ctx, "acme", "t"); err != nil {
		t.Errorf("DeleteTeam corrupt skip: %v", err)
	}
	// DeleteTeam PUT conflict is tolerated (precondition → skip).
	s20 := testService()
	mustAccess(t, s20, "o", "r", VisibilityPrivate, []AccessBinding{{Subject: "team:o/s", Role: RoleRead}})
	ks9 := &conflictStore{ObjectStore: s20.Store}
	s20.Store = ks9
	s20.Repos = func(ctx context.Context) ([][2]string, error) { return [][2]string{{"o", "r"}}, nil }
	if err := s20.DeleteTeam(ctx, "o", "s"); err != nil {
		t.Errorf("DeleteTeam put conflict: %v", err)
	}
	// DeleteTeam PUT hard error surfaces.
	ks10 := &keyFailStore{ObjectStore: s20.Store, err: errBoom, failPut: []string{"repos/o/r/access.json"}}
	s21 := New(ks10, config.Defaults())
	s21.Repos = func(ctx context.Context) ([][2]string, error) { return [][2]string{{"o", "r"}}, nil }
	if err := s21.DeleteTeam(ctx, "o", "s"); !errors.Is(err, errBoom) {
		t.Errorf("DeleteTeam put error: %v", err)
	}
	// ListTeams GetBytes failure (seed a team on the wrapped store first).
	if _, err := s11.CreateTeam(ctx, "acme", "seeded", "S", ""); err != nil {
		t.Fatal(err)
	}
	ks11 := &keyFailStore{ObjectStore: s11.Store, err: errBoom, failGet: []string{"orgs/acme/teams/"}}
	s22 := New(ks11, config.Defaults())
	if _, err := s22.ListTeams(ctx, "acme", 0); !errors.Is(err, errBoom) {
		t.Errorf("ListTeams get error: %v", err)
	}
	// listRepos inner failure.
	inner := &innerFailStore{ObjectStore: store.NewMemory()}
	s23 := New(inner, config.Defaults())
	if _, err := s23.listRepos(ctx); !errors.Is(err, errBoom) {
		t.Errorf("listRepos inner error: %v", err)
	}
	// addBinding upgrade path + already-sufficient path.
	s24 := testService()
	mustAccess(t, s24, "o", "r", VisibilityPrivate, []AccessBinding{{Subject: "user:up@example.com", Role: RoleRead}})
	if err := s24.addBinding(ctx, "o", "r", "user:up@example.com", RoleWrite); err != nil {
		t.Fatal(err)
	}
	if role, _ := s24.Resolve(ctx, "o", "r", authPrincipal("up@example.com")); role != RoleWrite {
		t.Errorf("upgrade failed: %q", role)
	}
	if err := s24.addBinding(ctx, "o", "r", "user:up@example.com", RoleRead); err != nil {
		t.Errorf("sufficient re-add: %v", err)
	}
	if err := s24.addBinding(ctx, "o", "r", "user:up@example.com", RoleWrite); err != nil {
		t.Errorf("equal re-add: %v", err)
	}
	// addBinding over corrupt access.json.
	if _, err := store.PutBytes(ctx, s24.Store, AccessKey("o", "bad"), []byte("{x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s24.addBinding(ctx, "o", "bad", "user:x@y.z", RoleRead); !errors.Is(err, ErrInvalid) {
		t.Errorf("addBinding corrupt: %v", err)
	}
	// expandRole with dangling team binding (team deleted after binding).
	s25 := testService()
	if _, err := s25.CreateOrg(ctx, "acme", "A", "", "a@b.c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s25.CreateTeam(ctx, "acme", "t", "T", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s25.SetTeamMember(ctx, "acme", "t", "m@x.c"); err != nil {
		t.Fatal(err)
	}
	mustAccess(t, s25, "acme", "r", VisibilityPrivate, []AccessBinding{{Subject: "team:acme/t", Role: RoleWrite}})
	if err := s25.Store.Delete(ctx, TeamKey("acme", "t"), ""); err != nil {
		t.Fatal(err)
	}
	s25.teams.invalidate("acme", "t")
	exp, w := s25.ExpandMembers(ctx, []string{"role:acme/r:write"})
	if len(w) != 0 {
		t.Errorf("dangling team warnings: %v", w)
	}
	for _, e := range exp {
		if e == "m@x.c" {
			t.Errorf("deleted team member must not expand: %v", exp)
		}
	}
	// ExpandMembers bad shapes.
	if _, w := s25.ExpandMembers(ctx, []string{"role:no-colon-here"}); len(w) != 1 {
		t.Errorf("role no-colon warnings: %v", w)
	}
	if _, w := s25.ExpandMembers(ctx, []string{"role:/r:admin"}); len(w) != 1 {
		t.Errorf("role empty owner warnings: %v", w)
	}
	// role: over a missing repo synthesizes to the empty set silently (the
	// legacy default resolves successfully); only malformed references warn.
	if _, w := s25.ExpandMembers(ctx, []string{"role:o/r:admin", "team:o/bad-slug!"}); len(w) != 1 {
		t.Errorf("unresolvable warnings: %v", w)
	}
}

// innerFailStore fails only nested repo listings.
type innerFailStore struct{ store.ObjectStore }

func (i *innerFailStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if prefix != "repos/" {
		return errBoom
	}
	return fn("repos/empty/")
}

var _ = io.Discard
