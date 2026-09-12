// users_test.go — Forgejo #370: the username registry.
//
// Usernames are derived at first OIDC login (email local part,
// collision-uniquified: crueber, crueber2, …), stored on
// users/<username>/user.json ↔ email, immutable thereafter, and stable
// across sessions (the email alias fast path). principalFromEmail
// resolves email → username; the email never becomes a principal name.
package identity

import (
	"context"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// TestResolveUsernameFirstLogin pins derivation + persistence: the base
// lands on user.json, the alias lands, and the principal name has no @.
func TestResolveUsernameFirstLogin(t *testing.T) {
	s := testService()
	ctx := context.Background()
	u, err := s.ResolveUsername(ctx, "Crueber@Gmail.COM")
	if err != nil {
		t.Fatal(err)
	}
	if u != "crueber" {
		t.Fatalf("username = %q, want %q", u, "crueber")
	}
	raw, _, err := store.GetBytes(ctx, s.Store, UserKey("crueber"), store.GetOptions{})
	if err != nil || !strings.Contains(string(raw), "crueber@gmail.com") {
		t.Fatalf("user.json must bind the email: %q %v", raw, err)
	}
	araw, _, err := store.GetBytes(ctx, s.Store, EmailAliasKey("crueber@gmail.com"), store.GetOptions{})
	if err != nil || !strings.Contains(string(araw), "crueber") {
		t.Fatalf("alias must map email → username: %q %v", araw, err)
	}
}

// TestResolveUsernameStable pins immutability: repeat logins (and a
// differently-cased repeat) return the same username without new keys.
func TestResolveUsernameStable(t *testing.T) {
	s := testService()
	ctx := context.Background()
	first, err := s.ResolveUsername(ctx, "crueber@gmail.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"crueber@gmail.com", "CRUEBER@gmail.com"} {
		u, err := s.ResolveUsername(ctx, email)
		if err != nil || u != first {
			t.Fatalf("repeat ResolveUsername(%q) = %q, %v; want %q", email, u, err, first)
		}
	}
	if _, _, err := store.GetBytes(ctx, s.Store, UserKey("crueber2"), store.GetOptions{}); err == nil {
		t.Fatal("stable login must not mint suffixed names")
	}
}

// TestResolveUsernameCollision pins uniquification: a foreign email
// holding the base pushes the newcomer to base2 (the issue's example:
// crueber, crueber2), and both bindings stay put afterwards.
func TestResolveUsernameCollision(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.ResolveUsername(ctx, "crueber@first.test"); err != nil {
		t.Fatal(err)
	}
	u, err := s.ResolveUsername(ctx, "crueber@second.test")
	if err != nil {
		t.Fatal(err)
	}
	if u != "crueber2" {
		t.Fatalf("collision username = %q, want crueber2", u)
	}
	// Both directions resolve stably afterwards.
	if v, _ := s.ResolveUsername(ctx, "crueber@first.test"); v != "crueber" {
		t.Fatalf("first user moved to %q", v)
	}
	if v, _ := s.ResolveUsername(ctx, "crueber@second.test"); v != "crueber2" {
		t.Fatalf("second user moved to %q", v)
	}
	em, err := s.EmailForUsername(ctx, "crueber2")
	if err != nil || em != "crueber@second.test" {
		t.Fatalf("EmailForUsername(cr ueber2) = %q, %v", em, err)
	}
	if em, err := s.EmailForUsername(ctx, "ghost"); err != nil || em != "" {
		t.Fatalf("EmailForUsername(ghost) = %q, %v; want empty", em, err)
	}
}

// TestResolveUsernameAdoptedAlias pins the alias-repair path: a user.json
// written without its alias (crash between the two Creates) still
// resolves to the same username, and the alias is repaired.
func TestResolveUsernameAdoptedAlias(t *testing.T) {
	s := testService()
	ctx := context.Background()
	u := &UserDoc{Version: 1, Username: "solo", Email: "solo@example.com", CreatedAt: "2026-09-12T00:00:00Z"}
	if _, err := store.PutBytes(ctx, s.Store, UserKey("solo"), encodeUserDoc(u),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveUsername(ctx, "solo@example.com")
	if err != nil || got != "solo" {
		t.Fatalf("adopt = %q, %v; want solo", got, err)
	}
	if _, _, err := store.GetBytes(ctx, s.Store, EmailAliasKey("solo@example.com"), store.GetOptions{}); err != nil {
		t.Fatalf("alias must be repaired: %v", err)
	}
}

// TestMatchPrincipal pins the migration alias: stored email spellings
// match username principals carrying that email, and vice versa — with
// no store round trip (pure comparison, push-path safe).
func TestMatchPrincipal(t *testing.T) {
	p := auth.Principal{Name: "crueber", Email: "crueber@gmail.com"}
	for _, stored := range []string{"crueber", "Crueber@Gmail.COM", "crueber@gmail.com"} {
		if !matchPrincipal(stored, p) {
			t.Errorf("matchPrincipal(%q) = false, want true", stored)
		}
	}
	for _, stored := range []string{"crueber2", "other@gmail.com", "", "crueber@gmail.com.evil.test"} {
		if matchPrincipal(stored, p) {
			t.Errorf("matchPrincipal(%q) = true, want false", stored)
		}
	}
	// A principal without a carried email matches by name only.
	bare := auth.Principal{Name: "crueber"}
	if matchPrincipal("crueber@gmail.com", bare) {
		t.Error("email spelling must not match an email-less principal")
	}
}

// TestSynthesizeOwnerUserVsOrg pins the grant boundary: legacy email
// owners and registry-backed usernames bind; orgs, synthetic names,
// and unclaimed usernames never do (a user:<orgslug> binding would be
// a latent grant to whoever later claims that username).
func TestSynthesizeOwnerUserVsOrg(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.ResolveUsername(ctx, "crueber@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		owner string
		bind  bool
	}{
		{"crueber@gmail.com", true}, // legacy email owner keeps its grant
		{"crueber", true},           // registry-backed username binds
		{"acme", false},             // org: org-owner resolution covers it
		{"anon", false},             // synthetic: never bound
		{"anonymous", false},        // synthetic: never bound
		{"unclaimed", false},        // no registry entry: foreign namespace
		{"bad!!principal", false},   // not a principal at all
	}
	for _, tc := range cases {
		doc := s.SynthesizeOwner(ctx, tc.owner)
		if got := len(doc.RoleBindings) > 0; got != tc.bind {
			t.Errorf("SynthesizeOwner(%q) bind = %v, want %v (%+v)", tc.owner, got, tc.bind, doc.RoleBindings)
		}
	}
	// The no-store fallback binds legacy emails only (fail closed).
	doc := SynthesizeDefault("crueber")
	if len(doc.RoleBindings) != 0 {
		t.Errorf("pure fallback must not bind usernames: %+v", doc.RoleBindings)
	}
	doc = SynthesizeDefault("crueber@gmail.com")
	if len(doc.RoleBindings) != 1 {
		t.Errorf("pure fallback must keep the legacy email grant: %+v", doc.RoleBindings)
	}
}

// TestUsernameKeysAreAdditive pins law 5: the new keys live alongside
// the old ones, and no existing key shape changed.
func TestUsernameKeysAreAdditive(t *testing.T) {
	if UserKey("Crueber") != "users/crueber/user.json" {
		t.Errorf("UserKey = %q", UserKey("Crueber"))
	}
	if EmailAliasKey("Crueber@Gmail.COM") != "users/by-email/crueber%40gmail.com/ref.json" {
		t.Errorf("EmailAliasKey = %q", EmailAliasKey("Crueber@Gmail.COM"))
	}
	if ProfileKey("crueber@gmail.com") != "users/crueber%40gmail.com/profile.json" {
		t.Errorf("legacy profile key moved: %q", ProfileKey("crueber@gmail.com"))
	}
	if ProfileKey("crueber") != "users/crueber/profile.json" {
		t.Errorf("username profile key = %q", ProfileKey("crueber"))
	}
}

// TestResolveUsernameConcurrentFirstLogin pins the CAS race: two
// sessions racing first-login for ONE email must converge on ONE
// username (the PutCreate loser re-reads the winner in the same pass
// and adopts it — never claims the next suffix). Repeated to force the
// interleave; a split is exactly one username per user lost.
func TestResolveUsernameConcurrentFirstLogin(t *testing.T) {
	for i := 0; i < 50; i++ {
		st := store.NewMemory()
		s1 := New(st, config.Defaults())
		s2 := New(st, config.Defaults())
		s1.Now = testClock
		s2.Now = testClock
		start := make(chan struct{})
		var w sync.WaitGroup
		got := make([]string, 2)
		errs := make([]error, 2)
		w.Add(2)
		go func() {
			defer w.Done()
			<-start
			got[0], errs[0] = s1.ResolveUsername(context.Background(), "race@example.com")
		}()
		go func() {
			defer w.Done()
			<-start
			got[1], errs[1] = s2.ResolveUsername(context.Background(), "race@example.com")
		}()
		close(start)
		w.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("iter %d errs: %v %v", i, errs[0], errs[1])
		}
		if got[0] != got[1] {
			t.Fatalf("iter %d split identity: %q vs %q", i, got[0], got[1])
		}
	}
}
