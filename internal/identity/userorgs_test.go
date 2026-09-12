package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// userorgs_test.go — Forgejo #423: the membership rail
// GET /api/v1/users/{principal}/orgs over Service.MemberOrgsFor.
//
// Sorted names, []-never-null, unknown principals answer empty (200, never
// 404), both lanes, GET-only, same anonymous_read gate as the profile and
// members reads, mutable-collab cache class with a content ETag. A bio edit
// rides the separate owner-profile route and can never stale this rail.

func getUserOrgs(t *testing.T, h *Handler, target string) (int, []string, http.Header) {
	t.Helper()
	w := doReq(h, "GET", target, "")
	if w.Code != http.StatusOK {
		return w.Code, nil, w.Header()
	}
	var orgs []string
	if err := json.Unmarshal(w.Body.Bytes(), &orgs); err != nil {
		t.Fatalf("GET %s: invalid JSON: %v (%s)", target, err, w.Body.String())
	}
	if orgs == nil {
		t.Fatalf("GET %s: null body, want []", target)
	}
	return w.Code, orgs, w.Header()
}

func TestUserOrgsRail(t *testing.T) {
	s := testService()
	seedOrg(t, s) // acme: alice owner, bob member
	h := testHandler(s, admin)

	// Owner + member both list acme (any roster role).
	for _, who := range []string{"alice%40example.com", "bob%40example.com"} {
		code, orgs, _ := getUserOrgs(t, h, "/api/v1/users/"+who+"/orgs")
		if code != http.StatusOK {
			t.Fatalf("GET %s/orgs = %d", who, code)
		}
		if len(orgs) != 1 || orgs[0] != "acme" {
			t.Errorf("GET %s/orgs = %v, want [acme]", who, orgs)
		}
	}

	// Unknown principal answers empty (200, never 404).
	code, orgs, _ := getUserOrgs(t, h, "/api/v1/users/ghost%40x.c/orgs")
	if code != http.StatusOK || len(orgs) != 0 {
		t.Errorf("GET ghost/orgs = %d %v, want 200 []", code, orgs)
	}

	// Browser lane twin.
	code, orgs, _ = getUserOrgs(t, h, "/api-browser/v1/users/bob%40example.com/orgs")
	if code != http.StatusOK || len(orgs) != 1 || orgs[0] != "acme" {
		t.Errorf("browser lane = %d %v, want 200 [acme]", code, orgs)
	}

	// Invalid principal → 400 (same shape as the profile read).
	if w := doReq(h, "GET", "/api/v1/users/bad!!principal/orgs", ""); w.Code != http.StatusBadRequest {
		t.Errorf("GET invalid/orgs = %d, want 400", w.Code)
	}

	// GET-only: PUT/DELETE/POST → 405.
	for _, m := range []string{"PUT", "POST", "DELETE"} {
		if w := doReq(h, m, "/api/v1/users/bob%40example.com/orgs", ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s user-orgs = %d, want 405", m, w.Code)
		}
	}

	// Anonymous gate mirrors the profile/members reads.
	s.Cfg.Server.Auth.AnonymousRead = false
	ha := testHandler(s, anon)
	if w := doReq(ha, "GET", "/api/v1/users/bob%40example.com/orgs", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon GET = %d, want 401", w.Code)
	}
	if w := doReq(ha, "GET", "/api/v1/users/bob%40example.com/orgs", ""); w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 must carry WWW-Authenticate")
	}
	s.Cfg.Server.Auth.AnonymousRead = true
	if w := doReq(ha, "GET", "/api/v1/users/bob%40example.com/orgs", ""); w.Code != http.StatusOK {
		t.Errorf("anon GET open = %d, want 200", w.Code)
	}
}

// TestUserOrgsErrors pins the failure paths: authenticator errors surface,
// and a store LIST failure answers 503 (probe failure is never a 403 or a
// misleading empty rail — law 9 fail-closed, the CheckCreateOwner precedent).
func TestUserOrgsErrors(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	hb := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad cred"}
	}}
	if w := doReq(hb, "GET", "/api/v1/users/bob%40example.com/orgs", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("auth error = %d, want 401", w.Code)
	}
	sErr := New(&errStore{ObjectStore: store.NewMemory(), listErr: errBoom}, config.Defaults())
	hErr := testHandler(sErr, admin)
	if w := doReq(hErr, "GET", "/api/v1/users/bob%40example.com/orgs", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("store error = %d, want 503", w.Code)
	}
}

// TestUserOrgsSorted pins multi-org order: sorted names, one entry per
// org.
func TestUserOrgsSorted(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	ctx := reqCtx()
	for _, org := range []string{"beta", "zeta"} {
		if _, err := s.CreateOrg(ctx, org, org, "", "alice@example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetMember(ctx, org, "bob@example.com", OrgMember); err != nil {
			t.Fatal(err)
		}
	}
	h := testHandler(s, admin)
	_, orgs, _ := getUserOrgs(t, h, "/api/v1/users/bob%40example.com/orgs")
	want := []string{"acme", "beta", "zeta"}
	if strings.Join(orgs, ",") != strings.Join(want, ",") {
		t.Errorf("orgs = %v, want %v", orgs, want)
	}
}

// TestUserOrgsETag pins the revalidation contract: an ETag rides the rail,
// If-None-Match 304s, and a roster change busts the tag AND moves the
// list (no stale-serve window).
func TestUserOrgsETag(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	h := testHandler(s, admin)

	first := doReq(h, "GET", "/api/v1/users/bob%40example.com/orgs", "")
	if first.Code != http.StatusOK {
		t.Fatalf("GET = %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("user-orgs must carry an ETag")
	}
	// ETag revalidation → 304.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/users/bob%40example.com/orgs", nil)
	r.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", rec.Code)
	}

	// Roster change: new org membership busts the tag and moves the list.
	if _, err := s.CreateOrg(reqCtx(), "beta", "Beta", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMember(reqCtx(), "beta", "bob@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	second := doReq(h, "GET", "/api/v1/users/bob%40example.com/orgs", "")
	if second.Code != http.StatusOK {
		t.Fatalf("GET after join = %d", second.Code)
	}
	if second.Header().Get("ETag") == etag {
		t.Error("roster change must bust the user-orgs ETag")
	}
	var orgs []string
	if err := json.Unmarshal(second.Body.Bytes(), &orgs); err != nil {
		t.Fatal(err)
	}
	if strings.Join(orgs, ",") != "acme,beta" {
		t.Errorf("orgs after join = %v, want [acme beta]", orgs)
	}
}

// TestUserOrgsFreshAfterBioEdit is the #423 ETag verification: a profile
// bio edit (PUT on the users/{principal} route) leaves the orgs rail
// fresh — separate route, separate ETag, no shared stale window.
func TestUserOrgsFreshAfterBioEdit(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	if _, err := s.EnsureProfile(reqCtx(), "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, admin)

	before := doReq(h, "GET", "/api/v1/users/bob%40example.com/orgs", "")
	if before.Code != http.StatusOK {
		t.Fatalf("GET orgs = %d", before.Code)
	}
	if w := doReq(h, "PUT", "/api/v1/users/bob%40example.com", `{"display_name":"Bob","bio":"new bio"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT bio = %d: %s", w.Code, w.Body.String())
	}
	after := doReq(h, "GET", "/api/v1/users/bob%40example.com/orgs", "")
	if after.Code != http.StatusOK {
		t.Fatalf("GET orgs after bio edit = %d", after.Code)
	}
	var orgs []string
	if err := json.Unmarshal(after.Body.Bytes(), &orgs); err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0] != "acme" {
		t.Errorf("orgs after bio edit = %v, want [acme]", orgs)
	}
}
