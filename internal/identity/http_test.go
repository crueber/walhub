package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func TestHandleRouting(t *testing.T) {
	s := testService()
	h := testHandler(s, admin)
	// Non-identity paths report false.
	for _, target := range []string{
		"/api/v1/owners",
		"/api/v1",
		"/api/v2/orgs",
		"/other/path",
		"/acme/repo/api/tree/main",
		"/acme/repo/api-browser/unknown",
		"/Bad%20Owner/repo/api/access",
		"/acme/repo/api/access/extra",
		"/api/v1/orgs/acme/members/a/b",
		"/api/v1/orgs/acme/unknown",
		"/api/v1/invitations/x/accept/extra",
		"/api/v1/invitations/x/nope",
		"/acme/repo/api/invitations/x/y",
	} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		if h.Handle(w, r) {
			t.Errorf("Handle(%q) = true, want false", target)
		}
	}
	// ServeHTTP 404s otherwise.
	w := doReq(h, "GET", "/api/v1/owners", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("fallback = %d", w.Code)
	}
}

func TestUsersEndpoints(t *testing.T) {
	s := testService()
	if _, err := s.EnsureProfile(reqCtx(), "jane@example.com"); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, alice)
	// GET existing.
	w := doReq(h, "GET", "/api/v1/users/jane%40example.com", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET user = %d: %s", w.Code, w.Body.String())
	}
	// GET unknown → 404.
	if w := doReq(h, "GET", "/api/v1/users/ghost%40x.c", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET ghost = %d", w.Code)
	}
	// GET invalid principal → 400.
	if w := doReq(h, "GET", "/api/v1/users/not-an-email", ""); w.Code != http.StatusBadRequest {
		t.Errorf("GET invalid = %d", w.Code)
	}
	// Anon denied when anonymous_read=false.
	s.Cfg.Server.Auth.AnonymousRead = false
	ha := testHandler(s, anon)
	if w := doReq(ha, "GET", "/api/v1/users/jane%40example.com", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon GET = %d", w.Code)
	}
	if w := doReq(ha, "GET", "/api/v1/users/jane%40example.com", ""); w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 must carry WWW-Authenticate")
	}
	s.Cfg.Server.Auth.AnonymousRead = true
	if w := doReq(ha, "GET", "/api/v1/users/jane%40example.com", ""); w.Code != http.StatusOK {
		t.Errorf("anon GET open = %d", w.Code)
	}
	// PUT self.
	hs := testHandler(s, alice)
	if _, err := s.EnsureProfile(reqCtx(), "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	w = doReq(hs, "PUT", "/api/v1/users/alice%40example.com", `{"display_name":"Alice","bio":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT self = %d: %s", w.Code, w.Body.String())
	}
	// PUT other as non-admin → 403.
	if w := doReq(hs, "PUT", "/api/v1/users/jane%40example.com", `{}`); w.Code != http.StatusForbidden {
		t.Errorf("PUT other = %d", w.Code)
	}
	// PUT other as admin → 200.
	hadm := testHandler(s, admin)
	if w := doReq(hadm, "PUT", "/api/v1/users/jane%40example.com", `{"display_name":"J"}`); w.Code != http.StatusOK {
		t.Errorf("PUT admin = %d", w.Code)
	}
	// PUT anon → 401.
	if w := doReq(ha, "PUT", "/api/v1/users/jane%40example.com", `{}`); w.Code != http.StatusUnauthorized {
		t.Errorf("PUT anon = %d", w.Code)
	}
	// PUT bad JSON → 400.
	if w := doReq(hadm, "PUT", "/api/v1/users/jane%40example.com", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("PUT bad json = %d", w.Code)
	}
	// Wrong method → 405.
	if w := doReq(h, "DELETE", "/api/v1/users/jane%40example.com", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE user = %d", w.Code)
	}
	// Auth error surfaces.
	hb := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad cred"}
	}}
	if w := doReq(hb, "GET", "/api/v1/users/jane%40example.com", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("auth error = %d", w.Code)
	}
	// Nil authenticator + nil service falls back to anonymous.
	hn := &Handler{}
	if w := doReq(hn, "GET", "/api/v1/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("nil handler = %d", w.Code)
	}
}

func TestOrgsEndpoints(t *testing.T) {
	s := testService()
	h := testHandler(s, alice)
	// POST requires write: carol has no flags.
	hc := testHandler(s, carol)
	if w := doReq(hc, "POST", "/api/v1/orgs", `{"org":"acme"}`); w.Code != http.StatusForbidden {
		t.Errorf("POST no-write = %d", w.Code)
	}
	if w := doReq(testHandler(s, anon), "POST", "/api/v1/orgs", `{"org":"acme"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("POST anon = %d", w.Code)
	}
	w := doReq(h, "POST", "/api/v1/orgs", `{"org":"Acme","display_name":"Acme"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST org = %d: %s", w.Code, w.Body.String())
	}
	// Auth-none's "anon" creator is not an email: creation still succeeds
	// (profile ensure is best-effort).
	hn := testHandler(s, noneP)
	if w := doReq(hn, "POST", "/api/v1/orgs", `{"org":"ncorg"}`); w.Code != http.StatusCreated {
		t.Errorf("POST org anon-creator = %d: %s", w.Code, w.Body.String())
	}
	var created map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created["org"] != "acme" {
		t.Errorf("create body: %s", w.Body.String())
	}
	// Duplicate by the owning creator is idempotent (201, #75 resume);
	// a rival creator taking the same name gets 409.
	if w := doReq(h, "POST", "/api/v1/orgs", `{"org":"acme"}`); w.Code != http.StatusCreated {
		t.Errorf("idempotent re-create = %d", w.Code)
	}
	hb := testHandler(s, bob)
	if w := doReq(hb, "POST", "/api/v1/orgs", `{"org":"acme"}`); w.Code != http.StatusConflict {
		t.Errorf("dup org = %d", w.Code)
	}
	// Bad JSON → 400.
	if w := doReq(h, "POST", "/api/v1/orgs", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("bad json = %d", w.Code)
	}
	// GET list (both lanes).
	for _, target := range []string{"/api/v1/orgs", "/api-browser/v1/orgs"} {
		w := doReq(h, "GET", target, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "acme") {
			t.Errorf("GET %s = %d: %s", target, w.Code, w.Body.String())
		}
	}
	// GET org.
	w = doReq(h, "GET", "/api/v1/orgs/acme", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET org = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET ghost = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/BAD!", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET bad slug = %d", w.Code)
	}
	// PUT as member (non-owner) → 403.
	if _, err := s.SetMember(reqCtx(), "acme", "carol@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	if w := doReq(hc, "PUT", "/api/v1/orgs/acme", `{"display_name":"X"}`); w.Code != http.StatusForbidden {
		t.Errorf("PUT member = %d", w.Code)
	}
	w = doReq(h, "PUT", "/api/v1/orgs/acme", `{"display_name":"Acme!"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT org = %d: %s", w.Code, w.Body.String())
	}
	// DELETE blocked while org owns repos.
	s.Repos = func(ctx reqCtxT) ([][2]string, error) { return [][2]string{{"acme", "r"}}, nil }
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme", ""); w.Code != http.StatusConflict {
		t.Errorf("DELETE with repos = %d", w.Code)
	}
	s.Repos = func(ctx reqCtxT) ([][2]string, error) { return nil, nil }
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE org = %d: %s", w.Code, w.Body.String())
	}
	// Method → 405.
	if w := doReq(h, "POST", "/api/v1/orgs/acme", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST org = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/api/v1/orgs", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE orgs = %d", w.Code)
	}
}

// TestIdentityMutableCacheTable pins the issue-#280 freshness contract for
// every identity GET: the mutable-collab class (private, no-cache — no
// stale-serve window), version-keyed ETag economics where a token exists
// (stale → 200 fresh, fresh → 304), and token movement on mutation (a
// post-mutation refresh paints the new state on the first read).
// Collection/singleton GETs without a version token take the class without
// an ETag (always 200, never stale).
func TestIdentityMutableCacheTable(t *testing.T) {
	s := testService()
	seedOrg(t, s) // acme org (alice owner), bob member, platform team
	seedRepo(t, s, "acme", "repo")
	if _, err := s.EnsureProfile(reqCtx(), "jane@example.com"); err != nil {
		t.Fatal(err)
	}
	adminH := testHandler(s, admin)
	aliceH := testHandler(s, alice)
	janeH := testHandler(s, auth.Principal{Name: "jane@example.com", Write: true})
	get := func(h *Handler, target, inm string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	rows := []struct {
		name       string
		target     string
		etagPrefix string // "" = tokenless (class only, always 200)
		mutate     func(t *testing.T)
	}{
		{"profile", "/api/v1/users/jane%40example.com", "user-v", func(t *testing.T) {
			t.Helper()
			if w := doReq(janeH, "PUT", "/api/v1/users/jane%40example.com", `{"display_name":"Jane"}`); w.Code != 200 {
				t.Fatalf("PUT profile = %d: %s", w.Code, w.Body.String())
			}
		}},
		{"org", "/api/v1/orgs/acme", "org-v", func(t *testing.T) {
			t.Helper()
			if w := doReq(aliceH, "PUT", "/api/v1/orgs/acme", `{"display_name":"Acme2"}`); w.Code != 200 {
				t.Fatalf("PUT org = %d: %s", w.Code, w.Body.String())
			}
		}},
		{"members", "/api/v1/orgs/acme/members", "members-v", func(t *testing.T) {
			t.Helper()
			if w := doReq(aliceH, "PUT", "/api/v1/orgs/acme/members/bob%40example.com", `{"role":"owner"}`); w.Code != 200 {
				t.Fatalf("PUT member = %d: %s", w.Code, w.Body.String())
			}
		}},
		{"team", "/api/v1/orgs/acme/teams/platform", "team-v", func(t *testing.T) {
			t.Helper()
			if w := doReq(aliceH, "PUT", "/api/v1/orgs/acme/teams/platform", `{"name":"Plat2"}`); w.Code != 200 {
				t.Fatalf("PUT team = %d: %s", w.Code, w.Body.String())
			}
		}},
		{"access", "/acme/repo/api/access", "access-v", func(t *testing.T) {
			t.Helper()
			w := doReq(adminH, "GET", "/acme/repo/api/access", "")
			var doc struct {
				Version int `json:"version"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			put := `{"version":` + strconv.Itoa(doc.Version) + `,"visibility":"private","role_bindings":[]}`
			if w := doReq(adminH, "PUT", "/acme/repo/api/access", put); w.Code != 200 {
				t.Fatalf("PUT access = %d: %s", w.Code, w.Body.String())
			}
		}},
		{"orgs list", "/api/v1/orgs", "", nil},
		{"teams list", "/api/v1/orgs/acme/teams", "", nil},
		{"member single", "/api/v1/orgs/acme/members/bob%40example.com", "", nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			first := get(adminH, row.target, "")
			if first.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", row.target, first.Code, first.Body.String())
			}
			if cc := first.Header().Get("Cache-Control"); cc != ccMutable {
				t.Fatalf("class: %q, want %q", cc, ccMutable)
			}
			etag := first.Header().Get("ETag")
			if row.etagPrefix == "" {
				if etag != "" {
					t.Fatalf("tokenless GET %s carries ETag %q", row.target, etag)
				}
				return
			}
			if !strings.Contains(etag, row.etagPrefix) {
				t.Fatalf("ETag %q lacks prefix %q", etag, row.etagPrefix)
			}
			if stale := get(adminH, row.target, `"bogus"`); stale.Code != http.StatusOK {
				t.Fatalf("stale ETag: %d, want 200", stale.Code)
			}
			if fresh := get(adminH, row.target, etag); fresh.Code != http.StatusNotModified {
				t.Fatalf("fresh ETag: %d, want 304", fresh.Code)
			}
			row.mutate(t)
			after := get(adminH, row.target, etag)
			if after.Code != http.StatusOK {
				t.Fatalf("post-mutation stale ETag: %d, want 200", after.Code)
			}
			if netag := after.Header().Get("ETag"); netag == etag {
				t.Fatalf("ETag did not move on mutation: %q", netag)
			}
		})
	}
}
