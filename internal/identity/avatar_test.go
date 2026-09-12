package identity

// User avatar tests (Forgejo #376): deterministic DiceBear generation,
// seed-absence/sanitization, per-principal single-flight background
// install, serving headers, self-or-admin auth, and the delete
// opt-out (login must not regenerate until an explicit regenerate).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// pollProfile waits up to 5 s for fn to report true (background
// generation is a goroutine — the test must not sleep a fixed span).
func pollProfile(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for background avatar generation")
}

func TestUserAvatarKey(t *testing.T) {
	if UserAvatarKey("Dave") != "users/dave/avatar.svg" {
		t.Errorf("UserAvatarKey = %q", UserAvatarKey("Dave"))
	}
	if got := UserAvatarURL("dave", "2026-09-12T00:00:00Z"); got != "/api/v1/users/dave/avatar?v=2026-09-12T00:00:00Z" {
		t.Errorf("UserAvatarURL = %q", got)
	}
	if got := UserAvatarURL("dave", ""); got != "/api/v1/users/dave/avatar" {
		t.Errorf("UserAvatarURL without v = %q", got)
	}
}

func TestGenerateDeterminism(t *testing.T) {
	a, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatalf("generate again: %v", err)
	}
	if a != b {
		t.Fatal("same seed must render byte-identical SVG (determinism contract)")
	}
	c, err := GenerateUserAvatarSVG("erin@example.com")
	if err != nil {
		t.Fatalf("generate other: %v", err)
	}
	if a == c {
		t.Fatal("distinct seeds must not render identical avatars")
	}
	// The Service default path (nil Generate stub) renders through the
	// same library entry point.
	if _, err := testService().generate("dave@example.com"); err != nil {
		t.Fatalf("service default generate: %v", err)
	}
}

func TestGenerateSeedAbsent(t *testing.T) {
	// Hostile seeds: markup-breaking email spellings must never appear
	// in the output (the seed feeds the PRNG only — verified, not
	// assumed) and the document must carry no script element.
	for _, seed := range []string{
		`dave@example.com`,
		`"></svg><script>alert(1)</script><svg x="`,
		`a<b@example.com`,
		`x&y@example.com`,
	} {
		svg, err := GenerateUserAvatarSVG(seed)
		if err != nil {
			t.Fatalf("generate(%q): %v", seed, err)
		}
		if strings.Contains(svg, seed) {
			t.Errorf("seed %q leaked into generated SVG", seed)
		}
		if strings.Contains(strings.ToLower(svg), "<script") {
			t.Errorf("seed %q produced a script element", seed)
		}
		if !strings.HasPrefix(strings.TrimSpace(svg), "<svg") {
			t.Errorf("seed %q did not produce an SVG document", seed)
		}
	}
}

func TestCheckAvatarSVG(t *testing.T) {
	ok, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAvatarSVG(ok, "dave@example.com"); err != nil {
		t.Errorf("valid SVG rejected: %v", err)
	}
	for name, svg := range map[string]string{
		"not svg":   `<html></html>`,
		"script":    `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"SCRIPT":    `<svg xmlns="http://www.w3.org/2000/svg"><SCRIPT>alert(1)</SCRIPT></svg>`,
		"seed leak": `<svg><!-- dave@example.com --></svg>`,
		"empty":     ``,
	} {
		if err := checkAvatarSVG(svg, "dave@example.com"); err == nil {
			t.Errorf("%s: accepted, want rejection", name)
		}
	}
	if err := checkAvatarSVG(strings.Repeat("x", int(maxUserAvatarBytes)+1), ""); err == nil {
		t.Error("oversize accepted, want rejection")
	}
	// putUserAvatar enforces the gate too (fail closed on the write path).
	s := testService()
	if _, err := s.putUserAvatar(reqCtx(), "dave", `<html></html>`); err == nil {
		t.Error("putUserAvatar accepted non-SVG, want rejection")
	}
}

func TestPutGetUserAvatarRoundTrip(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// No avatar initially (nil profile, nil bytes — not an error).
	if raw, prof, err := s.GetUserAvatar(ctx, "dave"); err != nil || raw != nil || prof != nil {
		t.Fatalf("fresh user must have no avatar: %v %+v %v", raw, prof, err)
	}
	prof, err := s.putUserAvatar(ctx, "dave", svg)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if prof.AvatarContentType != userAvatarContentType || prof.AvatarUpdatedAt == "" {
		t.Fatalf("pointer not set: %+v", prof)
	}
	if prof.AvatarDisabled {
		t.Fatalf("install must clear opt-out: %+v", prof)
	}
	raw, got, err := s.GetUserAvatar(ctx, "dave")
	if err != nil || string(raw) != svg || got.AvatarContentType != userAvatarContentType {
		t.Fatalf("round-trip broken: %v %+v %v", string(raw) != svg, got, err)
	}
	// Bytes live on the bucket (law 4 — wipe-safe, not memory-only).
	if _, _, err := store.GetBytes(ctx, s.Store, UserAvatarKey("dave"), store.GetOptions{}); err != nil {
		t.Fatalf("avatar bytes not on bucket: %v", err)
	}
	// Profile GET carries the pointer (single round trip render gate).
	stored, err := s.GetProfile(ctx, "dave")
	if err != nil || stored.AvatarContentType == "" || stored.AvatarUpdatedAt == "" {
		t.Fatalf("profile pointer missing: %+v %v", stored, err)
	}
}

func TestEnsureAvatarSingleFlight(t *testing.T) {
	s := testService()
	var calls atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	s.Generate = func(seed string) (string, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return GenerateUserAvatarSVG(seed)
	}
	// Five concurrent logins for one principal: the dedup set is
	// acquired synchronously, so after all five callers return exactly
	// one generation is in flight.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.EnsureAvatarAsync("dave", "dave@example.com") }()
	}
	wg.Wait()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no generation started")
	}
	// A sixth login while the first is still rendering must not start
	// a second generation.
	s.EnsureAvatarAsync("dave", "dave@example.com")
	select {
	case <-entered:
		t.Fatal("second concurrent login started a second generation")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	pollProfile(t, func() bool {
		got, err := s.GetProfile(reqCtx(), "dave")
		return err == nil && got != nil && got.AvatarContentType != ""
	})
	if n := calls.Load(); n != 1 {
		t.Fatalf("generations = %d, want exactly 1", n)
	}
	raw, _, err := s.GetUserAvatar(reqCtx(), "dave")
	if err != nil || raw == nil {
		t.Fatalf("installed avatar missing: %v", err)
	}
}

func TestEnsureAvatarAsyncSkips(t *testing.T) {
	s := testService()
	var called atomic.Bool
	s.Generate = func(seed string) (string, error) { called.Store(true); return "x", nil }
	// Empty identity never enqueues.
	s.EnsureAvatarAsync("", "")
	s.EnsureAvatarAsync("dave", "")
	// A user who already holds an avatar is left alone.
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.putUserAvatar(reqCtx(), "dave", svg); err != nil {
		t.Fatal(err)
	}
	s.EnsureAvatarAsync("dave", "dave@example.com")
	time.Sleep(100 * time.Millisecond)
	if called.Load() {
		t.Error("generation ran for a user who already has an avatar")
	}
	// An opted-out user is left alone (the deletion opt-out contract).
	if _, err := s.DeleteUserAvatar(reqCtx(), "dave"); err != nil {
		t.Fatal(err)
	}
	s.EnsureAvatarAsync("dave", "dave@example.com")
	time.Sleep(200 * time.Millisecond)
	if called.Load() {
		t.Error("generation ran for an opted-out user (login must not regenerate)")
	}
	if raw, _, _ := s.GetUserAvatar(reqCtx(), "dave"); raw != nil {
		t.Error("opted-out user gained an avatar")
	}
}

func TestEnsureAvatarAsyncFailureRetries(t *testing.T) {
	s := testService()
	// Mode-switching stub (one assignment before any spawn — no field
	// swap races): fail the first attempt, succeed the retry.
	var failFirst atomic.Bool
	failFirst.Store(true)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	s.Generate = func(seed string) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		if failFirst.Load() {
			return "", errBoom
		}
		return GenerateUserAvatarSVG(seed)
	}
	s.EnsureAvatarAsync("dave", "dave@example.com")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first attempt never started")
	}
	close(release)
	// Wait for the failed attempt to release the dedup entry (same
	// package — observe the set directly, no fixed sleeps).
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.avatarMu.Lock()
		_, busy := s.avatarBusy["dave"]
		s.avatarMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dedup entry never released after failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The next login retries and succeeds.
	failFirst.Store(false)
	s.EnsureAvatarAsync("dave", "dave@example.com")
	pollProfile(t, func() bool {
		got, err := s.GetProfile(reqCtx(), "dave")
		return err == nil && got != nil && got.AvatarContentType != ""
	})
}

func TestDeleteRegenerateOptOut(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	// Bind dave → dave@example.com (the #370 registry ResolveUsername
	// would create on first login).
	if _, err := s.ResolveUsername(ctx, "dave@example.com"); err != nil {
		t.Fatal(err)
	}
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.putUserAvatar(ctx, "dave", svg); err != nil {
		t.Fatal(err)
	}
	// DELETE opts out: pointer clears, flag sets, bytes go away.
	del, err := s.DeleteUserAvatar(ctx, "dave")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !del.AvatarDisabled || del.AvatarContentType != "" || del.AvatarUpdatedAt != "" {
		t.Fatalf("delete must clear pointer + set opt-out: %+v", del)
	}
	if raw, _, _ := s.GetUserAvatar(ctx, "dave"); raw != nil {
		t.Fatal("bytes survived delete")
	}
	// Determinism note: regenerating reproduces the identical image
	// (same email → same avatar).
	re, err := s.RegenerateUserAvatar(ctx, "dave")
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if re.AvatarDisabled || re.AvatarContentType == "" {
		t.Fatalf("regenerate must clear opt-out + set pointer: %+v", re)
	}
	raw, _, err := s.GetUserAvatar(ctx, "dave")
	if err != nil || string(raw) != svg {
		t.Fatal("regeneration must reproduce the identical avatar")
	}
	// DELETE on a profile without an avatar still records the opt-out
	// (deleting "nothing" means "no avatar").
	if _, err := s.DeleteUserAvatar(ctx, "erin"); err == nil {
		t.Fatal("delete for unknown principal must 404 (no synthesis)")
	}
	if _, err := s.EnsureProfile(ctx, "erin"); err != nil {
		t.Fatal(err)
	}
	if del, err := s.DeleteUserAvatar(ctx, "erin"); err != nil || !del.AvatarDisabled {
		t.Fatalf("delete-without-avatar must record opt-out: %+v %v", del, err)
	}
	// Regeneration without a mappable email fails closed (seed=email
	// contract — never invent a seed).
	if _, err := s.RegenerateUserAvatar(ctx, "erin"); err == nil {
		t.Fatal("regenerate without verified email must fail")
	}
}

func TestUserAvatarServeHeaders(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.putUserAvatar(ctx, "dave", svg); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, authPrincipal("dave"))
	w := doReq(h, "GET", "/api/v1/users/dave/avatar", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET avatar = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("content-type = %q, want image/svg+xml", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400, immutable" {
		t.Errorf("cache-control = %q", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if w.Body.String() != svg {
		t.Error("body is not the installed SVG")
	}
	// ETag revalidation → 304.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/users/dave/avatar", nil)
	r.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", rec.Code)
	}
	// Missing avatar → 404 (never a synthesized placeholder).
	if w := doReq(h, "GET", "/api/v1/users/ghost/avatar", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET ghost avatar = %d, want 404", w.Code)
	}
	// Anonymous GET follows the profile's anonymous-read rule.
	if w := doReq(testHandler(s, anon), "GET", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusOK {
		t.Errorf("anon GET avatar = %d, want 200 (anonymous read open in defaults)", w.Code)
	}
}

func TestUserAvatarAuth(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.putUserAvatar(ctx, "dave", svg); err != nil {
		t.Fatal(err)
	}
	self := testHandler(s, authPrincipal("dave"))
	other := testHandler(s, authPrincipal("mallory"))
	god := testHandler(s, admin)
	// DELETE by a foreign principal → 403; anonymous → 401.
	if w := doReq(other, "DELETE", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusForbidden {
		t.Errorf("foreign DELETE = %d, want 403", w.Code)
	}
	if w := doReq(testHandler(s, anon), "DELETE", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon DELETE = %d, want 401", w.Code)
	}
	// Self DELETE opts out (200 + flag).
	w := doReq(self, "DELETE", "/api/v1/users/dave/avatar", "")
	if w.Code != http.StatusOK {
		t.Fatalf("self DELETE = %d: %s", w.Code, w.Body.String())
	}
	if raw, _, _ := s.GetUserAvatar(ctx, "dave"); raw != nil {
		t.Fatal("avatar survived self DELETE")
	}
	// Admin regenerates for the user (POST): needs the registry
	// binding for the seed.
	if _, err := s.ResolveUsername(ctx, "dave@example.com"); err != nil {
		t.Fatal(err)
	}
	if w := doReq(god, "POST", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusOK {
		t.Fatalf("admin POST = %d: %s", w.Code, w.Body.String())
	}
	if raw, _, _ := s.GetUserAvatar(ctx, "dave"); raw == nil {
		t.Fatal("admin regenerate installed nothing")
	}
	// POST by a foreign principal → 403.
	if w := doReq(other, "POST", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusForbidden {
		t.Errorf("foreign POST = %d, want 403", w.Code)
	}
	// Unknown principal DELETE → 404 (no synthesis); bogus method → 405.
	if w := doReq(god, "DELETE", "/api/v1/users/ghost/avatar", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost DELETE = %d, want 404", w.Code)
	}
	if w := doReq(self, "PUT", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT avatar = %d, want 405", w.Code)
	}
}

func TestUserAvatarURLHook(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	// No avatar → "" (the navbar renders the username fallback).
	if got := s.UserAvatarURL(ctx, "dave"); got != "" {
		t.Errorf("no-avatar URL = %q, want empty", got)
	}
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	prof, err := s.putUserAvatar(ctx, "dave", svg)
	if err != nil {
		t.Fatal(err)
	}
	want := "/api/v1/users/dave/avatar?v=" + prof.AvatarUpdatedAt
	if got := s.UserAvatarURL(ctx, "dave"); got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

func TestUserAvatarErrorBranches(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	// Invalid principals fail closed on every entry point.
	for _, fn := range []func() error{
		func() error { _, _, err := s.GetUserAvatar(ctx, "not a principal!!"); return err },
		func() error { _, err := s.putUserAvatar(ctx, "", "x"); return err },
		func() error { _, err := s.DeleteUserAvatar(ctx, ""); return err },
	} {
		if err := fn(); err == nil {
			t.Error("invalid principal accepted, want rejection")
		}
	}
	// Regenerate surfaces generation failures (explicit user action —
	// the error reaches the caller, unlike the fire-and-forget path).
	if _, err := s.ResolveUsername(ctx, "dave@example.com"); err != nil {
		t.Fatal(err)
	}
	s.Generate = func(seed string) (string, error) { return "", errBoom }
	if _, err := s.RegenerateUserAvatar(ctx, "dave"); err == nil {
		t.Error("regenerate with failing renderer must fail")
	}
	s.Generate = nil
	// DELETE twice: the second is a no-write idempotent repeat (still
	// opted out, no version bump needed).
	if _, err := s.EnsureProfile(ctx, "erin"); err != nil {
		t.Fatal(err)
	}
	first, err := s.DeleteUserAvatar(ctx, "erin")
	if err != nil || !first.AvatarDisabled {
		t.Fatalf("first delete: %+v %v", first, err)
	}
	second, err := s.DeleteUserAvatar(ctx, "erin")
	if err != nil || !second.AvatarDisabled {
		t.Fatalf("repeat delete: %+v %v", second, err)
	}
	// Handler error surfaces: authenticator failure, invalid
	// principal spelling, and POST without a mappable email.
	badAuth := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad cred"}
	}}
	if w := doReq(badAuth, "GET", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("auth-failure GET = %d, want 401", w.Code)
	}
	h := testHandler(s, admin)
	if w := doReq(h, "GET", "/api/v1/users/%21%21/avatar", ""); w.Code != http.StatusBadRequest {
		t.Errorf("invalid-principal GET = %d, want 400", w.Code)
	}
	if w := doReq(h, "POST", "/api/v1/users/erin/avatar", ""); w.Code != http.StatusNotFound {
		t.Errorf("email-less POST = %d, want 404", w.Code)
	}
}
