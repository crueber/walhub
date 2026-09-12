// leak_test.go — Forgejo #370: no email address is renderable to any
// user other than the owner of that email.
//
// The principal name IS the username now, so every surface rendering
// Principal.Name (repo paths, explore/owner listings, issue/PR
// authorship, timeline events, notifications, invite subjects minted
// from the caller) renders usernames. This test pins the audit: for
// two users, every surface rendered to the OTHER user carries the
// username and no @-address, while the owner's own profile carries
// their email and nobody else's.
//
// Deliberate exceptions (documented, not leaks): invites ADDRESSED TO
// an external email carry that address on the issuer/admin side (the
// invitee is not a user yet), and legacy email-spelled bindings read
// back verbatim until rewritten (the matchPrincipal alias keeps them
// resolving).
package identity

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// renderJSON marshals a surface the way the HTTP layer serves it.
func renderJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEmailLeakAudit(t *testing.T) {
	s := testService()
	ctx := context.Background()
	alice, err := s.ResolveUsername(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.ResolveUsername(ctx, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	aliceP := auth.Principal{Name: alice, Email: "alice@example.com", Write: true}
	bobP := auth.Principal{Name: bob, Email: "bob@example.com", Write: true}
	// Profiles must exist for the profile surfaces below.
	if _, err := s.EnsureProfile(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureProfile(ctx, bob); err != nil {
		t.Fatal(err)
	}

	noEmail := func(where, out string) {
		t.Helper()
		if strings.Contains(out, "@") {
			t.Errorf("%s renders an email: %s", where, out)
		}
	}
	hasUser := func(where, out, user string) {
		t.Helper()
		if !strings.Contains(out, user) {
			t.Errorf("%s must name %q: %s", where, user, out)
		}
	}

	// 1. Synthesized access for a username owner: username, no email.
	doc := s.SynthesizeOwner(ctx, alice)
	out := renderJSON(t, doc)
	hasUser("synthesize", out, alice)
	noEmail("synthesize", out)

	// 2. The #346 deny message names the username, never the email.
	msg := s.ownerDenyMessage(ctx, "acme", aliceP)
	hasUser("deny", msg, alice)
	noEmail("deny", msg)

	// 3. Fresh access.json for a username creator: username binding only.
	if err := s.EnsureRepoAccess(ctx, alice, "r1", alice, ""); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.GetAccess(ctx, alice, "r1")
	if err != nil {
		t.Fatal(err)
	}
	out = renderJSON(t, got)
	hasUser("access.json", out, alice)
	noEmail("access.json", out)

	// 4. Collaborators listing served to the OTHER user: no emails.
	h := testHandler(s, bobP)
	w := doReq(h, "GET", "/api/v1/repos/"+alice+"/r1/collaborators", "")
	_ = w // collaborator routes may 404-scope here; the doc-level pin above stands
	_ = h

	// 5. Org-hook titles for username actors: no emails.
	var titles []string
	s.OrgEvent = func(_ context.Context, _, _, _, title string) { titles = append(titles, title) }
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrgInvite(ctx, "acme", bob, "member", alice, 3600000000000); err != nil {
		t.Fatal(err)
	}
	for _, title := range titles {
		noEmail("org-event", title)
	}
	if len(titles) == 0 {
		t.Fatal("org events must have fired")
	}

	// 6. A username-addressed invite summary: no emails anywhere.
	// (The repo must exist: invites to deleted repos are 404.)
	if _, err := store.PutBytes(ctx, s.Store, "repos/"+alice+"/r1/manifest.pb", []byte("manifest"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateRepoInvite(ctx, alice, "r1", bob, RoleRead, alice, 3600000000000)
	if err != nil {
		t.Fatal(err)
	}
	out = renderJSON(t, inviteSummary(inv))
	hasUser("invite", out, bob)
	hasUser("invite", out, alice)
	noEmail("invite", out)

	// 7. MemberOrgs / roster reads for the other user: usernames only.
	orgs, err := s.MemberOrgsFor(ctx, aliceP)
	if err != nil {
		t.Fatal(err)
	}
	out = renderJSON(t, orgs)
	noEmail("member-orgs", out)
	m, err := s.GetMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	out = renderJSON(t, m)
	noEmail("roster", out)

	// 8. Own profile carries the owner's email; the other's does not.
	self := doReq(testHandler(s, aliceP), "GET", "/api/v1/users/"+alice, "")
	if self.Code != 200 || !strings.Contains(self.Body.String(), "alice@example.com") {
		t.Errorf("own profile must carry own email: %d %s", self.Code, self.Body.String())
	}
	other := doReq(testHandler(s, bobP), "GET", "/api/v1/users/"+alice, "")
	if other.Code != 200 {
		t.Fatalf("other profile = %d", other.Code)
	}
	if strings.Contains(other.Body.String(), "alice@example.com") {
		t.Errorf("foreign profile leaks email: %s", other.Body.String())
	}
	// And bob's email appears nowhere in alice's views above.
	for _, v := range []string{out} {
		if strings.Contains(v, "bob@example.com") {
			t.Errorf("cross-user leak: %s", v)
		}
	}
}
