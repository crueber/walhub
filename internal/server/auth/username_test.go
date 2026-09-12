// username_test.go — Forgejo #370: email → username derivation rules.
//
// The principal name is a username, never a raw email (leak axis, fail
// closed). Derivation is pure: the same verified email always derives
// the same base; the store-backed registry (internal/identity)
// uniquifies collisions (base, base2, …) and owns immutability.
package auth

import "testing"

func TestDeriveUsername(t *testing.T) {
	cases := []struct{ email, want string }{
		{"crueber@gmail.com", "crueber"},
		{"CrueBer@Gmail.COM", "crueber"},         // lowercased
		{"first.last@example.com", "first.last"}, // dots survive (ID charset)
		{"a+b@example.com", "a-b"},               // plus folds to dash
		{"o'brien@example.com", "o-brien"},       // quote folds
		{"user_name-x.9@example.com", "user_name-x.9"},
		{"anonymous@example.com", "anonymous-user"}, // reserved, never bare
		{"anon@example.com", "anon-user"},
		{"@example.com", "user"},     // empty local part
		{"...@example.com", "user"},  // only dots
		{"a..b@example.com", "a..b"}, // interior dots kept (valid ID part)
		{"no-at-sign", "no-at-sign"}, // no @: whole string is local
	}
	for _, tc := range cases {
		if got := DeriveUsername(tc.email); got != tc.want {
			t.Errorf("DeriveUsername(%q) = %q, want %q", tc.email, got, tc.want)
		}
	}
	// Local part splits at the LAST @.
	if got := DeriveUsername("a@b@c.example.com"); got != "a-b" {
		t.Errorf("DeriveUsername multi-@ = %q, want %q", got, "a-b")
	}
	// Long local parts truncate to the cap.
	long := ""
	for range 80 {
		long += "a"
	}
	if got := DeriveUsername(long + "@example.com"); len(got) > maxUsernameLen {
		t.Errorf("DeriveUsername too long: %q", got)
	}
	// Every derivation is a valid username with no @ (the leak pin).
	for _, tc := range cases {
		got := DeriveUsername(tc.email)
		if !ValidUsername(got) {
			t.Errorf("DeriveUsername(%q) = %q is not a valid username", tc.email, got)
		}
		for i := 0; i < len(got); i++ {
			if got[i] == '@' {
				t.Errorf("DeriveUsername(%q) = %q contains @", tc.email, got)
			}
		}
	}
}

func TestValidUsername(t *testing.T) {
	valid := []string{"crueber", "a", "a.b_c-d", "x9", "user2"}
	for _, s := range valid {
		if !ValidUsername(s) {
			t.Errorf("ValidUsername(%q) = false, want true", s)
		}
	}
	invalid := []string{"", ".lead", "..", "UPPER", "has space", "has@at", "slash/x", "bang!", "üser"}
	for _, s := range invalid {
		if ValidUsername(s) {
			t.Errorf("ValidUsername(%q) = true, want false", s)
		}
	}
	long := ""
	for range maxUsernameLen + 1 {
		long += "a"
	}
	if ValidUsername(long) {
		t.Errorf("ValidUsername(%d chars) = true, want false", len(long))
	}
}

func TestWithCollisionSuffix(t *testing.T) {
	if got := WithCollisionSuffix("crueber", 0); got != "crueber" {
		t.Errorf("i=0 = %q", got)
	}
	if got := WithCollisionSuffix("crueber", 1); got != "crueber2" {
		t.Errorf("i=1 = %q", got)
	}
	if got := WithCollisionSuffix("crueber", 8); got != "crueber9" {
		t.Errorf("i=8 = %q", got)
	}
	// Long bases truncate so the candidate fits the cap and stays valid.
	long := ""
	for range maxUsernameLen {
		long += "a"
	}
	got := WithCollisionSuffix(long, 1)
	if len(got) > maxUsernameLen || !ValidUsername(got) {
		t.Errorf("suffixed long base invalid: %q", got)
	}
}

func TestSynthetic(t *testing.T) {
	for _, s := range []string{"anonymous", "anon", "Anon", " ANONYMOUS "} {
		if !Synthetic(s) {
			t.Errorf("Synthetic(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"crueber", "anon-user", "anonymous-user", ""} {
		if Synthetic(s) {
			t.Errorf("Synthetic(%q) = true, want false", s)
		}
	}
}

func TestPrincipalShapes(t *testing.T) {
	a := Anonymous()
	if !a.Anonymous || a.Name != "anonymous" || a.Write || a.Admin {
		t.Errorf("Anonymous() = %+v", a)
	}
	n := None()
	if n.Anonymous || n.Name != "anon" || !n.Write || !n.Admin {
		t.Errorf("None() = %+v", n)
	}
	if got := (&AuthError{Why: "nope"}).Error(); got != "nope" {
		t.Errorf("Error() = %q", got)
	}
	// The Chain Require helpers are contract stubs (the server owner
	// implements them): they panic by design.
	for name, fn := range map[string]func(){
		"RequireRead":  func() { (&Chain{}).RequireRead(a) },
		"RequireWrite": func() { (&Chain{}).RequireWrite(a) },
		"RequireAdmin": func() { (&Chain{}).RequireAdmin(a) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic (contract stub)", name)
				}
			}()
			fn()
		}()
	}
}
