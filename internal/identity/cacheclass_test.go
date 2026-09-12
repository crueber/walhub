package identity

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/cachepolicy"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestCacheClassContract is the Forgejo #382 systemic guard for the
// identity surface: every served cacheable GET is pinned to its §4 cache
// class, and every pinned pair passes the shared mutability rule (a
// version-keyed ETag must never ride a stale-serve window). Nothing on
// this surface serves SWR: mutable docs take no-cache (+ version ETags
// where a token exists), single reads take no-store, avatar bytes are
// version-URL'd immutable. Coverage: every ExposedTemplates entry appears
// either as an exact row (cacheable GET) or in noGET (mutation-only). A
// new template without a row fails here; a new GET serving SWR with a
// version ETag fails cachepolicy.Check.
func TestCacheClassContract(t *testing.T) {
	s := testService()
	seedOrg(t, s) // acme org (alice owner), bob member, platform team
	seedRepo(t, s, "acme", "repo")
	if _, err := s.EnsureProfile(reqCtx(), "jane@example.com"); err != nil {
		t.Fatal(err)
	}
	// Access doc behind the access/permissions/collaborators/assignables
	// rows (admin writer; alice keeps maintain on the private repo).
	if _, err := s.PutAccess(reqCtx(), "acme", "repo", "", VisibilityPrivate, []AccessBinding{
		{Subject: "user:alice@example.com", Role: RoleMaintain},
	}); err != nil {
		t.Fatal(err)
	}
	adminH := testHandler(s, admin)
	aliceH := testHandler(s, alice)
	janeH := testHandler(s, auth.Principal{Name: "jane@example.com", Write: true})

	// Invite fixtures: one org invite for gone@example.com (visible in
	// her inbox, the org collection, and the preview GET).
	if w := doReq(adminH, "POST", "/api/v1/orgs/acme/invitations", `{"email":"gone@example.com","role":"member"}`); w.Code != http.StatusCreated {
		t.Fatalf("seed org invite = %d: %s", w.Code, w.Body.String())
	}
	entries, err := s.MyInvites(reqCtx(), "gone@example.com")
	if err != nil || len(entries) != 1 {
		t.Fatalf("seed inbox: %v %d", err, len(entries))
	}
	inviteID := entries[0].ID
	goneH := testHandler(s, auth.Principal{Name: "gone@example.com"})
	// One repo invite (the repo collection row).
	if w := doReq(adminH, "POST", "/acme/repo/api/invitations", `{"subject":"y@z.c","role":"read"}`); w.Code != http.StatusCreated {
		t.Fatalf("seed repo invite = %d: %s", w.Code, w.Body.String())
	}
	// Avatar fixtures: user self-regenerate + org-owner upload (the GETs
	// serve version-URL'd immutable bytes).
	if w := doReq(janeH, "POST", "/api/v1/users/jane%40example.com/avatar", ""); w.Code != http.StatusOK {
		t.Fatalf("seed user avatar = %d: %s", w.Code, w.Body.String())
	}
	if w := doReqBytes(aliceH, "PUT", "/api/v1/orgs/acme/avatar", testPNG, "image/png"); w.Code != http.StatusOK {
		t.Fatalf("seed org avatar = %d: %s", w.Code, w.Body.String())
	}

	rows := []struct {
		name     string
		template string
		h        *Handler
		path     string
		wantCC   string
		wantETag bool
	}{
		{"profile", "/api/v1/users/{principal}", adminH, "/api/v1/users/jane%40example.com", ccMutable, true},
		{"user orgs", "/api/v1/users/{principal}/orgs", adminH, "/api/v1/users/bob%40example.com/orgs", ccMutable, true},
		{"orgs", "/api/v1/orgs", adminH, "/api/v1/orgs", ccMutable, false},
		{"org", "/api/v1/orgs/{org}", adminH, "/api/v1/orgs/acme", ccMutable, true},
		{"members", "/api/v1/orgs/{org}/members", adminH, "/api/v1/orgs/acme/members", ccMutable, true},
		{"member", "/api/v1/orgs/{org}/members/{principal}", adminH, "/api/v1/orgs/acme/members/bob%40example.com", ccMutable, false},
		{"teams", "/api/v1/orgs/{org}/teams", adminH, "/api/v1/orgs/acme/teams", ccMutable, false},
		{"team", "/api/v1/orgs/{org}/teams/{slug}", adminH, "/api/v1/orgs/acme/teams/platform", ccMutable, true},
		{"access", "/{owner}/{repo}/api/access", adminH, "/acme/repo/api/access", ccMutable, true},
		{"my invites", "/api/v1/invitations", goneH, "/api/v1/invitations", ccNoStore, false},
		{"invite preview", "/api/v1/invitations/{id}", goneH, "/api/v1/invitations/" + inviteID, ccNoStore, false},
		{"org invites", "/api/v1/orgs/{org}/invitations", adminH, "/api/v1/orgs/acme/invitations", ccNoStore, false},
		{"repo invites", "/{owner}/{repo}/api/invitations", adminH, "/acme/repo/api/invitations", ccNoStore, false},
		{"permissions", "/{owner}/{repo}/api/permissions", aliceH, "/acme/repo/api/permissions", ccNoStore, false},
		{"collaborators", "/{owner}/{repo}/api/collaborators", aliceH, "/acme/repo/api/collaborators", ccNoStore, false},
		{"assignables", "/{owner}/{repo}/api/assignables", aliceH, "/acme/repo/api/assignables", ccNoStore, false},
		{"user avatar", "/api/v1/users/{principal}/avatar", janeH, "/api/v1/users/jane%40example.com/avatar", "public, max-age=86400, immutable", true},
		{"org avatar", "/api/v1/orgs/{org}/avatar", aliceH, "/api/v1/orgs/acme/avatar", "public, max-age=86400, immutable", false},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			covered[row.template] = true
			r := httptest.NewRequest(http.MethodGet, row.path, nil)
			w := httptest.NewRecorder()
			row.h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", row.path, w.Code, w.Body.String())
			}
			cc := w.Header().Get("Cache-Control")
			if cc != row.wantCC {
				t.Fatalf("GET %s class = %q, want %q", row.path, cc, row.wantCC)
			}
			etag := w.Header().Get("ETag")
			if row.wantETag && etag == "" {
				t.Fatalf("GET %s must carry an ETag", row.path)
			}
			if !row.wantETag && etag != "" {
				t.Fatalf("GET %s carries unexpected ETag %q", row.path, etag)
			}
			if err := cachepolicy.Check(cc, etag); err != nil {
				t.Fatalf("mutability rule: %v", err)
			}
		})
	}
	// Mutation-only templates serve no 200-GET: team-member (PUT/DELETE),
	// scoped invite cancel (DELETE), invite accept (POST), repo invite
	// cancel (DELETE), repo transfer (POST).
	noGET := map[string]bool{
		"/api/v1/orgs/{org}/teams/{slug}/members/{principal}": true,
		"/api/v1/orgs/{org}/invitations/{id}":                 true,
		"/api/v1/invitations/{id}/accept":                     true,
		"/{owner}/{repo}/api/invitations/{id}":                true,
		"/{owner}/{repo}/api/transfer":                        true,
	}
	for _, e := range ExposedTemplates {
		if !covered[e] && !noGET[e] {
			t.Errorf("template %q serves a cacheable GET with no pinned class row", e)
		}
	}
}
