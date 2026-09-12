package identity

// Org avatar tests (Forgejo #359): bucket-backed avatar upload/display,
// size cap, content-type sniffing, auth, and delete-org cleanup.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// Image fixtures: magic bytes only (the sniffer never looks past the
// head) plus a payload tail so overwrite tests can distinguish bodies.
var (
	testPNG  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 'p', 'n', 'g'}
	testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'p', 'g'}
	testGIF  = []byte{'G', 'I', 'F', '8', '9', 'a', 'g', 'i', 'f'}
	testWEBP = []byte{'R', 'I', 'F', 'F', 1, 2, 3, 4, 'W', 'E', 'B', 'P'}
	testSVG  = []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	testTXT  = []byte(`just some text, not an image`)
)

// doReqBytes issues one raw-byte request (avatar PUTs carry bytes, not
// JSON — doReq only sends strings).
func doReqBytes(h *Handler, method, target string, raw []byte, contentType string) *httptest.ResponseRecorder {
	var rd io.Reader
	if raw != nil {
		rd = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, target, rd)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOrgAvatarKey(t *testing.T) {
	if OrgAvatarKey("acme") != "orgs/acme/avatar" {
		t.Error("OrgAvatarKey broken")
	}
}

func TestPutOrgAvatarRoundTrip(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	// No avatar initially.
	if raw, ct, err := s.GetOrgAvatar(ctx, "acme"); err != nil || raw != nil || ct != "" {
		t.Fatalf("fresh org must have no avatar: %v %q %v", raw, ct, err)
	}
	o, err := s.PutOrgAvatar(ctx, "acme", testPNG)
	if err != nil {
		t.Fatalf("PutOrgAvatar: %v", err)
	}
	if o.AvatarContentType != "image/png" || o.AvatarUpdatedAt == "" {
		t.Errorf("pointer not set: %+v", o)
	}
	raw, ct, err := s.GetOrgAvatar(ctx, "acme")
	if err != nil || ct != "image/png" || !bytes.Equal(raw, testPNG) {
		t.Errorf("avatar round-trip broken: %q %v", ct, err)
	}
	// Overwrite changes the type.
	if _, err := s.PutOrgAvatar(ctx, "acme", testJPEG); err != nil {
		t.Fatal(err)
	}
	if raw, ct, err := s.GetOrgAvatar(ctx, "acme"); err != nil || ct != "image/jpeg" || !bytes.Equal(raw, testJPEG) {
		t.Errorf("avatar overwrite broken: %q %v", ct, err)
	}
	// Unknown org 404s; bad slug 400s.
	if _, err := s.PutOrgAvatar(ctx, "ghost", testPNG); !errors.Is(err, ErrNotFound) {
		t.Errorf("ghost avatar = %v", err)
	}
	if _, err := s.PutOrgAvatar(ctx, "BAD!", testPNG); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad slug avatar = %v", err)
	}
	if _, _, err := s.GetOrgAvatar(ctx, "BAD!"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad slug get = %v", err)
	}
	if _, _, err := s.GetOrgAvatar(ctx, "ghost"); err != nil {
		t.Errorf("ghost get must be (nil, \"\", nil): %v", err)
	}
	if _, err := s.DeleteOrgAvatar(ctx, "BAD!"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad slug delete = %v", err)
	}
	if _, err := s.DeleteOrgAvatar(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ghost delete = %v", err)
	}
}

func TestOrgAvatarContentTypes(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"png", testPNG, "image/png"},
		{"jpeg", testJPEG, "image/jpeg"},
		{"gif", testGIF, "image/gif"},
		{"webp", testWEBP, "image/webp"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, err := s.PutOrgAvatar(ctx, "acme", c.raw)
			if err != nil {
				t.Fatalf("PutOrgAvatar(%s): %v", c.name, err)
			}
			if o.AvatarContentType != c.want {
				t.Errorf("content type = %q, want %q", o.AvatarContentType, c.want)
			}
			raw, ct, err := s.GetOrgAvatar(ctx, "acme")
			if err != nil || ct != c.want || !bytes.Equal(raw, c.raw) {
				t.Errorf("get(%s) = %q, %v", c.name, ct, err)
			}
			// The bytes live on the bucket under the avatar key
			// (wipe-safe per law 4: no local disk involved).
			stored, _, err := store.GetBytes(ctx, s.Store, OrgAvatarKey("acme"), store.GetOptions{})
			if err != nil || !bytes.Equal(stored, c.raw) {
				t.Errorf("bucket bytes for %s: %v", c.name, err)
			}
		})
	}
	for _, c := range []struct {
		name string
		raw  []byte
		err  error
	}{
		{"svg rejected", testSVG, ErrUnsupportedMedia},
		{"text rejected", testTXT, ErrUnsupportedMedia},
		{"empty rejected", []byte{}, ErrUnsupportedMedia},
		{"truncated magic", []byte{0x89, 0x50}, ErrUnsupportedMedia},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.PutOrgAvatar(ctx, "acme", c.raw); !errors.Is(err, c.err) {
				t.Errorf("PutOrgAvatar(%s) = %v, want %v", c.name, err, c.err)
			}
		})
	}
	// Size cap: exactly at the cap uploads; one byte over 413s.
	atCap := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
		bytes.Repeat([]byte{0}, int(maxOrgAvatarBytes)-8)...)
	if _, err := s.PutOrgAvatar(ctx, "acme", atCap); err != nil {
		t.Errorf("at-cap upload must succeed: %v", err)
	}
	over := append(atCap, 0)
	if _, err := s.PutOrgAvatar(ctx, "acme", over); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over-cap upload = %v, want ErrTooLarge", err)
	}
	if got := statusFor(ErrTooLarge); got != http.StatusRequestEntityTooLarge {
		t.Errorf("ErrTooLarge maps to %d, want 413", got)
	}
	if got := statusFor(ErrUnsupportedMedia); got != http.StatusUnsupportedMediaType {
		t.Errorf("ErrUnsupportedMedia maps to %d, want 415", got)
	}
}

func TestOrgAvatarHTTP(t *testing.T) {
	s := testService()
	if _, err := s.CreateOrg(reqCtx(), "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	owner := testHandler(s, alice)
	member := testHandler(s, carol)
	if _, err := s.SetMember(reqCtx(), "acme", "carol@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	anonH := testHandler(s, anon)

	// No avatar yet: 404 on both lanes.
	for _, target := range []string{"/api/v1/orgs/acme/avatar", "/api-browser/v1/orgs/acme/avatar"} {
		if w := doReq(owner, "GET", target, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, w.Code)
		}
	}
	// Auth: member PUT → 403, anon PUT → 401, anon GET → 200 (public).
	if w := doReqBytes(member, "PUT", "/api/v1/orgs/acme/avatar", testPNG, "image/png"); w.Code != http.StatusForbidden {
		t.Errorf("member PUT = %d, want 403", w.Code)
	}
	if w := doReqBytes(anonH, "PUT", "/api/v1/orgs/acme/avatar", testPNG, "image/png"); w.Code != http.StatusUnauthorized {
		t.Errorf("anon PUT = %d, want 401", w.Code)
	}
	// Upload: the client Content-Type is ignored (sniff wins).
	w := doReqBytes(owner, "PUT", "/api/v1/orgs/acme/avatar", testPNG, "text/plain")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"avatar_content_type":"image/png"`) {
		t.Errorf("PUT must return the avatar pointer: %s", w.Body.String())
	}
	// Display: exact bytes + sniffed type + immutable cache headers.
	for _, target := range []string{"/api/v1/orgs/acme/avatar", "/api-browser/v1/orgs/acme/avatar"} {
		w := doReqBytes(anonH, "GET", target, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("GET %s Content-Type = %q, want image/png", target, ct)
		}
		if !bytes.Equal(w.Body.Bytes(), testPNG) {
			t.Errorf("GET %s bytes differ", target)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("GET %s Cache-Control = %q, want immutable", target, cc)
		}
	}
	// GET org advertises the avatar (the UI's render gate, no probe).
	w = doReq(owner, "GET", "/api/v1/orgs/acme", "")
	if !strings.Contains(w.Body.String(), `"avatar_content_type":"image/png"`) {
		t.Errorf("GET org must carry the avatar pointer: %s", w.Body.String())
	}
	// Bad uploads: SVG → 415, oversize → 413, ghost org (admin) → 404.
	if w := doReqBytes(owner, "PUT", "/api/v1/orgs/acme/avatar", testSVG, "image/svg+xml"); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("svg PUT = %d, want 415", w.Code)
	}
	big := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
		bytes.Repeat([]byte{0}, int(maxOrgAvatarBytes))...)
	if w := doReqBytes(owner, "PUT", "/api/v1/orgs/acme/avatar", big, "image/png"); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("big PUT = %d, want 413", w.Code)
	}
	if w := doReqBytes(testHandler(s, admin), "PUT", "/api/v1/orgs/ghost/avatar", testPNG, "image/png"); w.Code != http.StatusNotFound {
		t.Errorf("ghost PUT = %d, want 404", w.Code)
	}
	if w := doReqBytes(owner, "PUT", "/api/v1/orgs/BAD!/avatar", testPNG, "image/png"); w.Code != http.StatusNotFound {
		t.Errorf("bad slug PUT = %d, want 404", w.Code)
	}
	// Failed uploads leave the good avatar in place.
	if raw, ct, err := s.GetOrgAvatar(reqCtx(), "acme"); err != nil || ct != "image/png" || !bytes.Equal(raw, testPNG) {
		t.Errorf("failed uploads must not clobber: %q %v", ct, err)
	}
	// DELETE: member → 403; owner clears pointer + bytes; repeat idempotent.
	if w := doReq(member, "DELETE", "/api/v1/orgs/acme/avatar", ""); w.Code != http.StatusForbidden {
		t.Errorf("member DELETE = %d, want 403", w.Code)
	}
	w = doReq(owner, "DELETE", "/api/v1/orgs/acme/avatar", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "avatar_content_type") {
		t.Errorf("DELETE must clear the pointer: %s", w.Body.String())
	}
	if w := doReq(owner, "GET", "/api/v1/orgs/acme/avatar", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", w.Code)
	}
	if w := doReq(owner, "DELETE", "/api/v1/orgs/acme/avatar", ""); w.Code != http.StatusOK {
		t.Errorf("repeat DELETE = %d, want 200", w.Code)
	}
	// Wrong method + extra path segment.
	if w := doReq(owner, "POST", "/api/v1/orgs/acme/avatar", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST avatar = %d, want 405", w.Code)
	}
	if w := doReq(owner, "GET", "/api/v1/orgs/acme/avatar/extra", ""); w.Code != http.StatusNotFound {
		t.Errorf("avatar/extra = %d, want 404", w.Code)
	}
}

func TestDeleteOrgRemovesAvatar(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutOrgAvatar(ctx, "acme", testPNG); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrg(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := s.GetOrgAvatar(ctx, "acme"); err != nil || raw != nil {
		t.Errorf("avatar must die with the org: %v", err)
	}
	if _, _, err := store.GetBytes(ctx, s.Store, OrgAvatarKey("acme"), store.GetOptions{}); !store.IsNotFound(err) {
		t.Errorf("avatar object must be gone: %v", err)
	}
}

// TestOrgAvatarMissingObjectRendersUnset pins the prune-tolerance rule: a
// set pointer whose bytes were deleted out-of-band renders as "no avatar"
// (nil, nil), never an error — the org page must not 503 for a pruned bucket.
func TestOrgAvatarMissingObjectRendersUnset(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutOrgAvatar(ctx, "acme", testPNG); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Delete(ctx, OrgAvatarKey("acme"), ""); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, alice)
	if w := doReq(h, "GET", "/api/v1/orgs/acme/avatar", ""); w.Code != http.StatusNotFound {
		t.Errorf("pruned avatar GET = %d, want 404", w.Code)
	}
}
