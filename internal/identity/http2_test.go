package identity

import (
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestMembersTeamsEndpoints(t *testing.T) {
	s := testService()
	h := testHandler(s, alice)
	mustOrg(t, s)
	hc := testHandler(s, carol)

	// Members collection.
	w := doReq(h, "GET", "/api/v1/orgs/acme/members", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "alice@example.com") {
		t.Fatalf("GET members = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/members", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST members = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/ghost/members", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET ghost members = %d", w.Code)
	}
	// One member.
	w = doReq(h, "GET", "/api/v1/orgs/acme/members/alice%40example.com", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET member = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/acme/members/ghost%40x.c", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET non-member = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/acme/members/nope", ""); w.Code != http.StatusBadRequest {
		t.Errorf("GET bad principal = %d", w.Code)
	}
	// PUT as non-owner → 403.
	if _, err := s.SetMember(reqCtx(), "acme", "carol@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	if w := doReq(hc, "PUT", "/api/v1/orgs/acme/members/dave%40example.com", `{"role":"member"}`); w.Code != http.StatusForbidden {
		t.Errorf("PUT member non-owner = %d", w.Code)
	}
	w = doReq(h, "PUT", "/api/v1/orgs/acme/members/dave%40example.com", `{"role":"member"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT member = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(h, "PUT", "/api/v1/orgs/acme/members/dave%40example.com", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("PUT member bad json = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/api/v1/orgs/acme/members/dave%40example.com", `{"role":"root"}`); w.Code != http.StatusBadRequest {
		t.Errorf("PUT member bad role = %d", w.Code)
	}
	w = doReq(h, "DELETE", "/api/v1/orgs/acme/members/dave%40example.com", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE member = %d", w.Code)
	}
	// Last owner removal → 409.
	if w := doReq(h, "DELETE", "/api/v1/orgs/acme/members/alice%40example.com", ""); w.Code != http.StatusConflict {
		t.Errorf("DELETE last owner = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/members/x%40y.z", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST member = %d", w.Code)
	}

	// Teams collection + create.
	w = doReq(h, "POST", "/api/v1/orgs/acme/teams", `{"slug":"platform","name":"Platform"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST team = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(hc, "POST", "/api/v1/orgs/acme/teams", `{"slug":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("POST team non-owner = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/teams", `{"slug":"platform"}`); w.Code != http.StatusConflict {
		t.Errorf("dup team = %d", w.Code)
	}
	w = doReq(h, "GET", "/api/v1/orgs/acme/teams", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "platform") {
		t.Fatalf("GET teams = %d: %s", w.Code, w.Body.String())
	}
	w = doReq(h, "GET", "/api-browser/v1/orgs/acme/teams?n=10", "")
	if w.Code != http.StatusOK {
		t.Errorf("browser lane teams = %d", w.Code)
	}
	// Team CRUD.
	w = doReq(h, "GET", "/api/v1/orgs/acme/teams/platform", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET team = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/acme/teams/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET ghost team = %d", w.Code)
	}
	if w := doReq(h, "GET", "/api/v1/orgs/acme/teams/BAD!", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET bad slug = %d", w.Code)
	}
	w = doReq(h, "PUT", "/api/v1/orgs/acme/teams/platform", `{"name":"Plat"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT team = %d", w.Code)
	}
	// Team membership.
	w = doReq(h, "PUT", "/api/v1/orgs/acme/teams/platform/members/bob%40example.com", "")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT team member = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(h, "PUT", "/api/v1/orgs/acme/teams/platform/members/nope", ""); w.Code != http.StatusBadRequest {
		t.Errorf("PUT team member bad = %d", w.Code)
	}
	if w := doReq(hc, "PUT", "/api/v1/orgs/acme/teams/platform/members/x%40y.z", ""); w.Code != http.StatusForbidden {
		t.Errorf("PUT team member non-owner = %d", w.Code)
	}
	w = doReq(h, "DELETE", "/api/v1/orgs/acme/teams/platform/members/bob%40example.com", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE team member = %d", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/teams/platform/members/bob%40example.com", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST team member = %d", w.Code)
	}
	// Team delete strips bindings (repo lane tested below).
	w = doReq(h, "DELETE", "/api/v1/orgs/acme/teams/platform", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE team = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(h, "POST", "/api/v1/orgs/acme/teams", ""); w.Code != http.StatusBadRequest {
		t.Errorf("POST teams empty = %d", w.Code)
	}
	if w := doReq(h, "PATCH", "/api/v1/orgs/acme/teams/platform", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH team = %d", w.Code)
	}
}

func TestAccessEndpoints(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	ha := testHandler(s, admin)
	hb := testHandler(s, bob)
	hc := testHandler(s, carol)
	hanon := testHandler(s, anon)

	// GET on missing access.json synthesizes (admin flag bypasses triage).
	w := doReq(ha, "GET", "/acme/repo/api/access", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET access = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"visibility":"public"`) {
		t.Errorf("synthesis must be public: %s", w.Body.String())
	}
	// Browser lane twin.
	if w := doReq(ha, "GET", "/acme/repo/api-browser/access", ""); w.Code != http.StatusOK {
		t.Errorf("browser lane access = %d", w.Code)
	}
	// PUT as host admin.
	w = doReq(ha, "PUT", "/acme/repo/api/access",
		`{"version":0,"visibility":"private","role_bindings":[{"subject":"team:acme/platform","role":"write"},{"subject":"user:carol@example.com","role":"triage"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":1`) {
		t.Fatalf("PUT access = %d: %s", w.Code, w.Body.String())
	}
	// Bob (team write) fails the triage read? No — write ≥ triage, passes.
	if w := doReq(hb, "GET", "/acme/repo/api/access", ""); w.Code != http.StatusOK {
		t.Errorf("bob GET access = %d", w.Code)
	}
	// Carol (triage) reads.
	if w := doReq(hc, "GET", "/acme/repo/api/access", ""); w.Code != http.StatusOK {
		t.Errorf("carol GET access = %d", w.Code)
	}
	// Carol cannot PUT (needs admin).
	if w := doReq(hc, "PUT", "/acme/repo/api/access", `{"version":1,"visibility":"private","role_bindings":[]}`); w.Code != http.StatusForbidden {
		t.Errorf("carol PUT = %d", w.Code)
	}
	// Stale version → 409 with reload hint.
	if w := doReq(ha, "PUT", "/acme/repo/api/access", `{"version":0,"visibility":"private","role_bindings":[]}`); w.Code != http.StatusConflict {
		t.Errorf("stale PUT = %d", w.Code)
	}
	// Invalid body → 400.
	if w := doReq(ha, "PUT", "/acme/repo/api/access", `{"version":1,"visibility":"x","role_bindings":[]}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad visibility PUT = %d", w.Code)
	}
	if w := doReq(ha, "PUT", "/acme/repo/api/access", `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("bad json PUT = %d", w.Code)
	}
	// Stranger (no role, private repo) → 403; anon → 401 + Bearer.
	hs := testHandler(s, stranger)
	if w := doReq(hs, "GET", "/acme/repo/api/access", ""); w.Code != http.StatusForbidden {
		t.Errorf("stranger GET = %d", w.Code)
	}
	w = doReq(hanon, "GET", "/acme/repo/api/access", "")
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("anon GET private = %d (need 401+Bearer)", w.Code)
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/access", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE access = %d", w.Code)
	}
}

func TestInviteEndpoints(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	seedRepo(t, s, "acme", "repo")
	ha := testHandler(s, alice) // org owner
	hb := testHandler(s, bob)   // member, team write on repos only
	hanon := testHandler(s, anon)

	// Org invite create (owner).
	w := doReq(ha, "POST", "/api/v1/orgs/acme/invitations", `{"email":"pat@example.com","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST org invite = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "accept_url") {
		t.Errorf("invite must return accept_url: %s", w.Body.String())
	}
	// Non-owner cannot invite.
	if w := doReq(hb, "POST", "/api/v1/orgs/acme/invitations", `{"email":"x@y.z","role":"member"}`); w.Code != http.StatusForbidden {
		t.Errorf("member invite = %d", w.Code)
	}
	// Org invite list (owner).
	w = doReq(ha, "GET", "/api/v1/orgs/acme/invitations", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pat@example.com") {
		t.Fatalf("GET org invites = %d: %s", w.Code, w.Body.String())
	}
	// Mine (pat).
	hp := testHandler(s, patP())
	w = doReq(hp, "GET", "/api/v1/invitations", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "acme") {
		t.Fatalf("GET mine = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(hanon, "GET", "/api/v1/invitations", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon mine = %d", w.Code)
	}
	if w := doReq(hp, "POST", "/api/v1/invitations", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST mine = %d", w.Code)
	}
	// Accept.
	var id string
	{
		entries, err := s.MyInvites(reqCtx(), "pat@example.com")
		if err != nil || len(entries) != 1 {
			t.Fatalf("inbox: %v %+v", err, entries)
		}
		id = entries[0].ID
	}
	w = doReq(hp, "POST", "/api/v1/invitations/"+id+"/accept", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"bound":"org"`) {
		t.Fatalf("accept = %d: %s", w.Code, w.Body.String())
	}
	// Re-accept → 409.
	if w := doReq(hp, "POST", "/api/v1/invitations/"+id+"/accept", ""); w.Code != http.StatusConflict {
		t.Errorf("re-accept = %d", w.Code)
	}

	// Repo invites (admin lane): alice is org owner → repo admin.
	w = doReq(ha, "POST", "/acme/repo/api/invitations", `{"subject":"dave@example.com","role":"write"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST repo invite = %d: %s", w.Code, w.Body.String())
	}
	w = doReq(ha, "GET", "/acme/repo/api/invitations", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "dave@example.com") {
		t.Fatalf("GET repo invites = %d: %s", w.Code, w.Body.String())
	}
	// Bob (no repo admin: team platform exists but no binding) → 403.
	if w := doReq(hb, "GET", "/acme/repo/api/invitations", ""); w.Code != http.StatusForbidden {
		t.Errorf("bob repo invites = %d", w.Code)
	}
	// Dave accepts via browser lane.
	hd := testHandler(s, dave)
	var rid string
	{
		entries, err := s.MyInvites(reqCtx(), "dave@example.com")
		if err != nil || len(entries) != 1 {
			t.Fatalf("dave inbox: %v", err)
		}
		rid = entries[0].ID
	}
	w = doReq(hd, "POST", "/api-browser/v1/invitations/"+rid+"/accept", "")
	if w.Code != http.StatusOK {
		t.Fatalf("browser accept = %d: %s", w.Code, w.Body.String())
	}
	// Admin deletes a pending invite.
	w = doReq(ha, "POST", "/acme/repo/api/invitations", `{"subject":"erin@example.com","role":"read"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST repo invite 2 = %d", w.Code)
	}
	var eid string
	{
		entries, err := s.MyInvites(reqCtx(), "erin@example.com")
		if err != nil || len(entries) != 1 {
			t.Fatalf("erin inbox: %v", err)
		}
		eid = entries[0].ID
	}
	w = doReq(ha, "DELETE", "/acme/repo/api/invitations/"+eid, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE repo invite = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/invitations/"+eid, ""); w.Code != http.StatusNotFound {
		t.Errorf("re-delete = %d", w.Code)
	}
	// Invitee declines via top-level DELETE.
	w = doReq(ha, "POST", "/acme/repo/api/invitations", `{"subject":"fin@example.com","role":"read"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST repo invite 3 = %d", w.Code)
	}
	var fid string
	{
		entries, _ := s.MyInvites(reqCtx(), "fin@example.com")
		fid = entries[0].ID
	}
	hf := testHandler(s, finP())
	if w := doReq(hf, "DELETE", "/api/v1/invitations/"+fid, ""); w.Code != http.StatusNoContent {
		t.Fatalf("decline = %d: %s", w.Code, w.Body.String())
	}
	// Bad role on repo invite → 400.
	if w := doReq(ha, "POST", "/acme/repo/api/invitations", `{"subject":"x@y.z","role":"super"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad role invite = %d", w.Code)
	}
	if w := doReq(ha, "GET", "/acme/repo/api/invitations/extra/path", ""); w.Code != http.StatusNotFound {
		t.Errorf("deep invite path must not route: %d", w.Code)
	}
}

func TestInvitePreviewToken(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	ha := testHandler(s, alice)
	w := doReq(ha, "POST", "/api/v1/orgs/acme/invitations", `{"email":"preview@example.com","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("invite = %d", w.Code)
	}
	entries, err := s.MyInvites(reqCtx(), "preview@example.com")
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	// Load the raw invite to get the token.
	raw := mustInviteRaw(t, s, "acme", entries[0].ID)
	inv, err := parseInvite(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Authenticated preview with the token works (signed-link preview; the
	// token is the bearer secret for forwarded links). Located via the
	// subject's inbox; a non-subject caller cannot locate the invite.
	hsub := testHandler(s, authPrincipal("preview@example.com"))
	w = doReq(hsub, "GET", "/api/v1/invitations/"+inv.ID+"?token="+inv.Token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("token preview = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), inv.Token) {
		t.Error("preview must redact the token")
	}
	// Subject without token also previews (subject-match suffices).
	if w := doReq(hsub, "GET", "/api/v1/invitations/"+inv.ID, ""); w.Code != http.StatusOK {
		t.Errorf("subject preview = %d", w.Code)
	}
	// Anonymous preview requires login first.
	hanon := testHandler(s, anon)
	if w := doReq(hanon, "GET", "/api/v1/invitations/"+inv.ID+"?token="+inv.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon preview = %d", w.Code)
	}
	// Wrong token as anonymous still requires login first.
	if w := doReq(hanon, "GET", "/api/v1/invitations/"+inv.ID+"?token=wrong", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon bad-token preview = %d", w.Code)
	}
	// GET on /accept path with GET → 405.
	if w := doReq(hanon, "GET", "/api/v1/invitations/"+inv.ID+"/accept", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET accept = %d", w.Code)
	}
	// No token, wrong subject → 403/409.
	ho := testHandler(s, stranger)
	if w := doReq(ho, "GET", "/api/v1/invitations/"+inv.ID, ""); w.Code != http.StatusForbidden && w.Code != http.StatusConflict {
		t.Errorf("wrong-subject preview = %d", w.Code)
	}
}

func mustOrg(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.CreateOrg(reqCtx(), "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
}

func mustInviteRaw(t *testing.T, s *Service, org, id string) []byte {
	t.Helper()
	raw, _, err := store.GetBytes(reqCtx(), s.Store, OrgInviteKey(org, id), store.GetOptions{})
	if err != nil {
		t.Fatalf("GetBytes invite: %v", err)
	}
	return raw
}
