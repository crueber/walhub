package social

import (
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestCover2StarGates(t *testing.T) {
	// Read-gated star (private repo stranger → 403).
	x := newHarness(t)
	x.svc.Roles = &stubRoles{checkErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}}
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); !isErr(err, ErrForbidden) {
		t.Fatalf("gated star: %v", err)
	}
	// Nil clock.
	s := &Service{Store: store.NewMemory()}
	if s.nowUTC().IsZero() {
		t.Fatal("nil clock")
	}
}

func TestCover2StarFloorAndCorruptCount(t *testing.T) {
	x := newHarness(t)
	// Record without a counter bump (crashed between Create and
	// increment): unstar floors at 0 instead of going negative.
	seedSocialKey(t, x, StarKey("jane", "o", "r"), `{"repo":"o/r","starred_at":"2026-09-04T12:00:00Z"}`)
	n, err := x.svc.Unstar(ctx(), jane(), "o", "r")
	if err != nil || n != 0 {
		t.Fatalf("floor: %d %v", n, err)
	}
	// Corrupt counters + live record: restar reports 0, no error (the
	// record is the truth; the count reheals on the next mutation).
	x2 := newHarness(t)
	seedSocialKey(t, x2, StarKey("jane", "o", "r"), `{"repo":"o/r","starred_at":"2026-09-04T12:00:00Z"}`)
	seedSocialKey(t, x2, SocialKey("o", "r"), `{oops`)
	n2, err := x2.svc.Star(ctx(), jane(), "o", "r")
	if err != nil || n2 != 0 {
		t.Fatalf("corrupt count: %d %v", n2, err)
	}
}

func TestCover2StarredSkips(t *testing.T) {
	x := newHarness(t)
	// Non-record key under the prefix (index-style object) is skipped.
	seedSocialKey(t, x, StarredPrefix("jane")+"index.json", `{"v":1}`)
	// Corrupt record is skipped.
	seedSocialKey(t, x, StarredPrefix("jane")+"o/z.json", `{oops`)
	seedSocialKey(t, x, StarredPrefix("jane")+"o/a.json", `{"repo":"o/a","starred_at":"2026-09-03T12:00:00Z"}`)
	entries, _, err := x.svc.Starred(ctx(), "jane", 10, "")
	if err != nil || len(entries) != 1 || entries[0].Repo != "o/a" {
		t.Fatalf("skips: %+v %v", entries, err)
	}
}

func TestCover2MyStarredAuthError(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}
	rec := do(t, x, "GET", "/api/v1/me/starred", nil, nil)
	if rec.Code != 503 {
		t.Fatalf("twin auth: %d", rec.Code)
	}
}
