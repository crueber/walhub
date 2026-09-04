// http_perms_test.go — Feature 08 §§3.6/5: the permission-gating
// surface. Table-driven httptest over the three endpoints: role
// resolution per P6 (incl. the anonymous null/read contract), effective
// collaborators with sources, assignables dedup + display fallback,
// method gating, and the read-gate 401s.
package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func seedPermsRepo(t *testing.T, s *Service) {
	t.Helper()
	ctx := reqCtx()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "dev", "Dev", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "dev", "dev@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMember(ctx, "acme", "boss@example.com", OrgOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureProfile(ctx, "dev@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAccess(ctx, "acme", "repo", "", VisibilityPrivate, []AccessBinding{
		{Subject: "user:alice@example.com", Role: RoleMaintain},
		{Subject: "team:acme/dev", Role: RoleWrite},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAccess(ctx, "acme", "pub", "", VisibilityPublic, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsEndpoint(t *testing.T) {
	s := testService()
	seedPermsRepo(t, s)
	rows := []struct {
		name string
		repo string
		as   string
		role string
		want int
	}{
		{"direct maintain", "repo", "alice@example.com", "maintain", http.StatusOK},
		{"team write", "repo", "dev@example.com", "write", http.StatusOK},
		{"org owner admin", "repo", "boss@example.com", "admin", http.StatusOK},
		{"stranger private denied", "repo", "stranger@example.com", "", http.StatusForbidden},
		{"anon private denied", "repo", "", "", http.StatusUnauthorized},
		{"anon public reads", "pub", "", "read", http.StatusOK},
		{"stranger public null role", "pub", "stranger@example.com", "", http.StatusOK},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			var p = anon
			if tc.as != "" {
				p = auth.Principal{Name: tc.as}
			}
			h := testHandler(s, p)
			w := doReq(h, "GET", "/acme/"+tc.repo+"/api/permissions", "")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			var got struct {
				Role *string `json:"role"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tc.role == "" {
				if got.Role != nil {
					t.Fatalf("role = %q, want null", *got.Role)
				}
				return
			}
			if got.Role == nil || *got.Role != tc.role {
				t.Fatalf("role = %+v, want %q", got.Role, tc.role)
			}
		})
	}
}

func TestPermissionsMethodGate(t *testing.T) {
	s := testService()
	seedPermsRepo(t, s)
	h := testHandler(s, admin)
	for _, path := range []string{"permissions", "collaborators", "assignables"} {
		if w := doReq(h, "POST", "/acme/repo/api/"+path, ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, w.Code)
		}
		if w := doReq(h, "GET", "/acme/repo/api/"+path+"/extra", ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s/extra = %d, want 404", path, w.Code)
		}
	}
}

func TestCollaboratorsEndpoint(t *testing.T) {
	s := testService()
	seedPermsRepo(t, s)
	h := testHandler(s, admin)
	w := doReq(h, "GET", "/acme/repo/api/collaborators", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Collaborators []collaborator `json:"collaborators"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byPrincipal := map[string]collaborator{}
	for _, c := range got.Collaborators {
		byPrincipal[c.Principal] = c
	}
	if c, ok := byPrincipal["alice@example.com"]; !ok || c.Role != "maintain" || c.Source != "direct" {
		t.Errorf("alice = %+v", c)
	}
	if c, ok := byPrincipal["dev@example.com"]; !ok || c.Role != "write" || c.Source != "team:acme/dev" {
		t.Errorf("dev = %+v", c)
	}
	if c, ok := byPrincipal["boss@example.com"]; !ok || c.Role != "admin" || c.Source != "org-owner" {
		t.Errorf("boss = %+v", c)
	}
	// Anon on private → 401 with WWW-Authenticate (fail closed).
	ha := testHandler(s, anon)
	w = doReq(ha, "GET", "/acme/repo/api/collaborators", "")
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("anon = %d, want 401+WWW-Authenticate", w.Code)
	}
}

func TestAssignablesEndpoint(t *testing.T) {
	s := testService()
	seedPermsRepo(t, s)
	h := testHandler(s, admin)
	w := doReq(h, "GET", "/acme/repo/api/assignables", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Assignables []assignable `json:"assignables"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Assignables) == 0 {
		t.Fatal("empty assignables")
	}
	seen := map[string]bool{}
	prev := ""
	for _, a := range got.Assignables {
		if seen[a.Principal] {
			t.Fatalf("duplicate %q", a.Principal)
		}
		seen[a.Principal] = true
		if a.Principal < prev {
			t.Fatalf("unsorted: %q after %q", a.Principal, prev)
		}
		prev = a.Principal
		if a.Display == "" {
			t.Fatalf("empty display for %q", a.Principal)
		}
	}
	for _, want := range []string{"alice@example.com", "dev@example.com", "boss@example.com", "owner@example.com"} {
		if !seen[want] {
			t.Errorf("missing %q in %v", want, got.Assignables)
		}
	}
}

func TestAccessPutPublishesFrame(t *testing.T) {
	s := testService()
	var published []string
	s.Stream = func(_ context.Context, repo string) { published = append(published, repo) }
	if _, err := s.PutAccess(reqCtx(), "acme", "repo", "", VisibilityPrivate, []AccessBinding{
		{Subject: "user:alice@example.com", Role: RoleRead},
	}); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0] != "acme/repo" {
		t.Fatalf("published = %v", published)
	}
}
