package checks

import (
	"strings"
	"testing"
)

// min caps a length for failure messages.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Table-driven validation: contexts, states, shas, URLs, descriptions,
// token shapes, and the combined precedence — the pure core every handler
// and gate builds on.

func TestValidContext(t *testing.T) {
	good := []string{"ci/build", "lint", "woodpecker/test", "a", "A0._-/"[:5], strings.Repeat("x", 100)}
	for _, c := range good {
		if err := ValidContext(c); err != nil {
			t.Fatalf("good %q rejected: %v", c, err)
		}
	}
	bad := []string{"", strings.Repeat("x", 101), "/lead", "trail/", "ctx.json", "has space", "semi;colon", "back\\slash", "unicode-✓"}
	for _, c := range bad {
		if err := ValidContext(c); err == nil {
			t.Fatalf("bad %q accepted", c)
		}
	}
}

func TestNormalizeSHA(t *testing.T) {
	full40 := strings.Repeat("a", 40)
	full64 := strings.Repeat("b", 64)
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{full40, full40, true},
		{full64, full64, true},
		{strings.ToUpper(full40), full40, true},
		{"  " + full40 + "  ", full40, true},
		{"abc", "", false},
		{strings.Repeat("a", 39), "", false},
		{strings.Repeat("g", 40), "", false},
		{"", "", false},
		{"index", "", false},
	} {
		got, err := normalizeSHA(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("sha %q: got %q, %v", tc.in, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("sha %q accepted", tc.in)
		}
	}
}

func TestCombinedPrecedence(t *testing.T) {
	for _, tc := range []struct {
		states []string
		want   string
	}{
		{nil, StatePending},
		{[]string{}, StatePending},
		{[]string{StateSuccess}, StateSuccess},
		{[]string{StateSuccess, StateSuccess}, StateSuccess},
		{[]string{StateSuccess, StatePending}, StatePending},
		{[]string{StatePending}, StatePending},
		{[]string{StateSuccess, StateFailure}, StateFailure},
		{[]string{StatePending, StateFailure}, StateFailure},
		{[]string{StateFailure, StateError}, StateError},
		{[]string{StateSuccess, StatePending, StateFailure, StateError}, StateError},
	} {
		if got := combinedState(tc.states); got != tc.want {
			t.Fatalf("states %v: got %q want %q", tc.states, got, tc.want)
		}
	}
}

func TestTokenShapes(t *testing.T) {
	// Claim is the cheap prefix test.
	if !ClaimToken("wct_abcd1234.secret") || ClaimToken("wgt_abcd") || ClaimToken("") {
		t.Fatal("claim")
	}
	// Well-formed parses.
	id, secret, err := ParseCIToken("wct_abcd1234." + strings.Repeat("s", 64))
	if err != nil || id != "abcd1234" || secret != strings.Repeat("s", 64) {
		t.Fatalf("parse: %q %q %v", id, secret, err)
	}
	// Malformed is 401-class, never a fall-through.
	for _, bad := range []string{"wgt_abcd", "wct_", "wct_short.secret", "wct_ABCD1234.secret", "wct_abcd1234", "wct_abcd1234."} {
		_, _, err := ParseCIToken(bad)
		if err == nil {
			t.Fatalf("bad %q accepted", bad)
		}
		if statusFor(err) != 401 {
			t.Fatalf("bad %q: status %d, want 401", bad, statusFor(err))
		}
	}
	// "wct_abcd1234.secret.extra.more" parses with secret
	// "secret.extra.more" (Cut on the first dot). That is fine: the hash
	// comparison decides, and a dotted secret can never match a minted
	// hex secret.
	if _, secret, err := ParseCIToken("wct_abcd1234.secret.extra.more"); err != nil || secret != "secret.extra.more" {
		t.Fatalf("dotted: %q %v", secret, err)
	}
}

func TestTokenHashRoundTrip(t *testing.T) {
	secret := strings.Repeat("9f", 32)
	h := hashSecret(secret)
	if !verifySecret(secret, h) {
		t.Fatal("self-verify failed")
	}
	if verifySecret("other", h) || verifySecret(secret, "deadbeef") {
		t.Fatal("forgery accepted")
	}
}

func TestCIPrincipalNames(t *testing.T) {
	if CIPrincipalName("abcd1234") != "ci:abcd1234" {
		t.Fatal("name")
	}
	if id, ok := IsCIPrincipal("ci:abcd1234"); !ok || id != "abcd1234" {
		t.Fatal("detect")
	}
	for _, not := range []string{"jane@example.com", "ci:", "ci:SHORT", "anonymous", "ci:abcd1234x", ""} {
		if _, ok := IsCIPrincipal(not); ok {
			t.Fatalf("non-ci %q detected", not)
		}
	}
}

func TestAssertPrefixDisjoint(t *testing.T) {
	AssertPrefixDisjoint("wgt_", "Bearer ", "Basic ") // must not panic
	for _, tc := range [][]string{{"wct_"}, {"wct_x"}, {"w"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("overlap %q did not panic", tc[0])
				}
			}()
			AssertPrefixDisjoint(tc...)
		}()
	}
}

func TestStatusFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrNotFound, 404}, {ErrInvalid, 400}, {ErrUnauthorized, 401},
		{ErrForbidden, 403}, {ErrConflict, 409}, {ErrUnprocessable, 422},
		{ErrUnavailable, 503}, {ErrInvalidState, 409}, {ErrCorrupt, 500},
	}
	for _, c := range cases {
		if got := statusFor(c.err); got != c.want {
			t.Fatalf("%v: got %d want %d", c.err, got, c.want)
		}
	}
}

func TestFieldValidators(t *testing.T) {
	if err := validTargetURL("https://ci.example/run/1"); err != nil {
		t.Fatalf("good url: %v", err)
	}
	if err := validTargetURL(""); err != nil {
		t.Fatalf("empty url: %v", err)
	}
	badURLs := []string{"ftp://x/y", "notaurl", "https://" + strings.Repeat("x", 2049)}
	for _, bad := range badURLs {
		if err := validTargetURL(bad); err == nil {
			t.Fatalf("bad url accepted: %q", bad[:min(32, len(bad))])
		}
	}
	if err := validDescription(strings.Repeat("x", 256)); err != nil {
		t.Fatalf("256 chars: %v", err)
	}
	if err := validDescription(strings.Repeat("x", 257)); err == nil {
		t.Fatal("257 chars accepted")
	}
	if err := validState("bogus"); err {
		t.Fatal("bogus state accepted")
	}
	for _, good := range []string{StatePending, StateSuccess, StateFailure, StateError} {
		if !validState(good) {
			t.Fatalf("good state %q rejected", good)
		}
	}
}
