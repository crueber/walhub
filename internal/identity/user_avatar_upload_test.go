package identity

// User avatar upload tests (Forgejo #601): PNG/JPEG/GIF uploads are
// center-cropped server-side to a square and PNG re-encoded (pure
// stdlib — law 1, no image dependency); 2 MiB cap (413); SVG and WebP
// rejected (415 — WebP is the deliberate divergence from the #359 org
// twin, which stdlib cannot decode); AvatarUpdatedAt bumps on every
// upload (else ?v=/ETag clients serve stale); POST regenerate replaces
// an upload; DELETE opts out for both kinds.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"
)

// encodeUploadPNG builds a real W×H PNG with column markers (column c
// carries shade c — the center-crop proof reads pixels back).
func encodeUploadPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 20), G: uint8(y * 20), B: 0x80, A: 0xFF})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func encodeUploadJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 0x10, G: 0x80, B: 0x40, A: 0xFF})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func encodeUploadGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var out bytes.Buffer
	if err := gif.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// decodeUploadSize decodes served avatar bytes back to dimensions.
func decodeUploadSize(t *testing.T, raw []byte) (int, int) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("served avatar does not decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("served avatar format = %q, want png (normalized re-encode)", format)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

func TestPutUserAvatarUploadRoundTrip(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	// No avatar initially.
	if raw, prof, err := s.GetUserAvatar(ctx, "dave"); err != nil || raw != nil || prof != nil {
		t.Fatalf("fresh user must have no avatar: %v %+v %v", raw, prof, err)
	}
	for _, c := range []struct {
		name string
		raw  []byte
		w, h int
		want int // cropped square side
	}{
		{"wide png crops to height", encodeUploadPNG(t, 8, 4), 8, 4, 4},
		{"tall jpeg crops to width", encodeUploadJPEG(t, 6, 10), 6, 10, 6},
		{"square gif passes through", encodeUploadGIF(t, 5, 5), 5, 5, 5},
		{"wide gif crops to height", encodeUploadGIF(t, 9, 3), 9, 3, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			prof, err := s.PutUserAvatarBytes(ctx, "dave", c.raw)
			if err != nil {
				t.Fatalf("PutUserAvatarBytes: %v", err)
			}
			if prof.AvatarContentType != uploadedUserAvatarContentType {
				t.Errorf("pointer type = %q, want %q", prof.AvatarContentType, uploadedUserAvatarContentType)
			}
			if prof.AvatarUpdatedAt == "" {
				t.Error("upload must bump AvatarUpdatedAt (else ?v=/ETag clients serve stale)")
			}
			if prof.AvatarDisabled {
				t.Error("install must clear the opt-out")
			}
			raw, got, err := s.GetUserAvatar(ctx, "dave")
			if err != nil || got.AvatarContentType != uploadedUserAvatarContentType {
				t.Fatalf("round-trip broken: %+v %v", got, err)
			}
			if w, h := decodeUploadSize(t, raw); w != c.want || h != c.want {
				t.Errorf("served size = %dx%d, want %dx%d (center-cropped square)", w, h, c.want, c.want)
			}
		})
	}
}

func TestCropSquarePNGCenters(t *testing.T) {
	// 9×5, middle column (x=4) white on black: the crop takes the
	// centered 5 columns (x=2..6), so served pixel (2,2) is white while
	// the corners stay black. A top-left crop would put white at (4,2).
	img := image.NewRGBA(image.Rect(0, 0, 9, 5))
	for y := 0; y < 5; y++ {
		for x := 0; x < 9; x++ {
			img.Set(x, y, color.RGBA{A: 0xFF})
		}
	}
	for y := 0; y < 5; y++ {
		img.Set(4, y, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
	}
	var src bytes.Buffer
	if err := png.Encode(&src, img); err != nil {
		t.Fatal(err)
	}
	out, err := cropSquarePNG(src.Bytes())
	if err != nil {
		t.Fatalf("crop: %v", err)
	}
	got, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != 5 || b.Dy() != 5 {
		t.Fatalf("cropped size = %dx%d, want 5x5", b.Dx(), b.Dy())
	}
	r, g, b, _ := got.At(2, 2).RGBA()
	if r != 0xFFFF || g != 0xFFFF || b != 0xFFFF {
		t.Error("center pixel is not the source middle column (crop is not centered)")
	}
	r, g, b, _ = got.At(0, 0).RGBA()
	if r != 0 || g != 0 || b != 0 {
		t.Error("corner pixel changed (crop moved content)")
	}
	// Tall twin: 5×9 with middle row (y=4) white.
	img = image.NewRGBA(image.Rect(0, 0, 5, 9))
	for y := 0; y < 9; y++ {
		for x := 0; x < 5; x++ {
			img.Set(x, y, color.RGBA{A: 0xFF})
		}
	}
	for x := 0; x < 5; x++ {
		img.Set(x, 4, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
	}
	src.Reset()
	if err := png.Encode(&src, img); err != nil {
		t.Fatal(err)
	}
	out, err = cropSquarePNG(src.Bytes())
	if err != nil {
		t.Fatalf("crop tall: %v", err)
	}
	got, _, err = image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != 5 || b.Dy() != 5 {
		t.Fatalf("tall cropped size = %dx%d, want 5x5", b.Dx(), b.Dy())
	}
	r, g, b, _ = got.At(2, 2).RGBA()
	if r != 0xFFFF || g != 0xFFFF || b != 0xFFFF {
		t.Error("tall center pixel is not the source middle row (crop is not centered)")
	}
}

func TestPutUserAvatarUploadRejects(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	webp := []byte{'R', 'I', 'F', 'F', 1, 2, 3, 4, 'W', 'E', 'B', 'P'}
	for _, c := range []struct {
		name string
		raw  []byte
		err  error
	}{
		{"svg rejected (same-origin script risk)", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), ErrUnsupportedMedia},
		{"webp rejected (stdlib cannot crop it)", webp, ErrUnsupportedMedia},
		{"text rejected", []byte(`just some text, not an image`), ErrUnsupportedMedia},
		{"empty rejected", []byte{}, ErrUnsupportedMedia},
		{"truncated magic rejected", []byte{0x89, 0x50}, ErrUnsupportedMedia},
		{"png magic but corrupt body rejected", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 'n', 'o', 't', ' ', 'p', 'n', 'g'}, ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.PutUserAvatarBytes(ctx, "dave", c.raw); !errors.Is(err, c.err) {
				t.Errorf("PutUserAvatarBytes = %v, want %v", err, c.err)
			}
		})
	}
	// WebP names itself in the error (the caller learns the divergence
	// instead of guessing).
	if _, err := s.PutUserAvatarBytes(ctx, "dave", webp); err == nil || !strings.Contains(err.Error(), "WebP") {
		t.Errorf("webp error must name WebP: %v", err)
	}
	// Size cap: exactly at the cap uploads; one byte over 413s.
	atCap := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
		bytes.Repeat([]byte{0}, int(maxUserAvatarUploadBytes)-8)...)
	// Zero-filled tail is not a decodable PNG — pad a real image instead.
	big := encodeUploadPNG(t, 64, 64)
	big = append(big, bytes.Repeat([]byte{0}, int(maxUserAvatarUploadBytes)-len(big))...)
	if _, err := s.PutUserAvatarBytes(ctx, "dave", big); err != nil {
		t.Errorf("at-cap upload must succeed: %v", err)
	}
	_ = atCap
	over := append(big, 0)
	if _, err := s.PutUserAvatarBytes(ctx, "dave", over); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over-cap upload = %v, want ErrTooLarge", err)
	}
	// Unknown principal 404s (no synthesis); bad spelling 400s.
	if _, err := s.PutUserAvatarBytes(ctx, "ghost", encodeUploadPNG(t, 4, 4)); !errors.Is(err, ErrNotFound) {
		t.Errorf("ghost upload = %v, want ErrNotFound", err)
	}
	if _, err := s.PutUserAvatarBytes(ctx, "BAD!", encodeUploadPNG(t, 4, 4)); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad spelling upload = %v, want ErrInvalid", err)
	}
	if _, _, err := s.GetUserAvatar(ctx, "BAD!"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad spelling get = %v, want ErrInvalid", err)
	}
}

func TestUserAvatarUploadHTTP(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	self := testHandler(s, authPrincipal("dave"))
	other := testHandler(s, authPrincipal("mallory"))
	god := testHandler(s, admin)
	anonH := testHandler(s, anon)

	// Auth: foreign PUT → 403, anon PUT → 401.
	if w := doReqBytes(other, "PUT", "/api/v1/users/dave/avatar", encodeUploadPNG(t, 4, 4), "image/png"); w.Code != http.StatusForbidden {
		t.Errorf("foreign PUT = %d, want 403", w.Code)
	}
	if w := doReqBytes(anonH, "PUT", "/api/v1/users/dave/avatar", encodeUploadPNG(t, 4, 4), "image/png"); w.Code != http.StatusUnauthorized {
		t.Errorf("anon PUT = %d, want 401", w.Code)
	}
	// Upload: the client Content-Type is ignored (sniff wins).
	w := doReqBytes(self, "PUT", "/api/v1/users/dave/avatar", encodeUploadPNG(t, 8, 4), "text/plain")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"avatar_content_type":"image/png"`) {
		t.Errorf("PUT must return the raster pointer: %s", w.Body.String())
	}
	// Display: cropped square PNG + normalized type + immutable cache.
	w = doReqBytes(anonH, "GET", "/api/v1/users/dave/avatar", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("GET Content-Type = %q, want image/png", ct)
	}
	if ww, hh := decodeUploadSize(t, w.Body.Bytes()); ww != 4 || hh != 4 {
		t.Errorf("GET size = %dx%d, want 4x4 (8x4 cropped)", ww, hh)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("GET Cache-Control = %q, want immutable", cc)
	}
	// Bad uploads: SVG → 415, WebP → 415 (named), oversize → 413,
	// ghost (admin) → 404.
	if w := doReqBytes(self, "PUT", "/api/v1/users/dave/avatar", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml"); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("svg PUT = %d, want 415", w.Code)
	}
	webp := []byte{'R', 'I', 'F', 'F', 1, 2, 3, 4, 'W', 'E', 'B', 'P'}
	if w := doReqBytes(self, "PUT", "/api/v1/users/dave/avatar", webp, "image/webp"); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("webp PUT = %d, want 415", w.Code)
	} else if !strings.Contains(w.Body.String(), "WebP") {
		t.Errorf("webp 415 must name WebP: %s", w.Body.String())
	}
	big := append(encodeUploadPNG(t, 64, 64), bytes.Repeat([]byte{0}, int(maxUserAvatarUploadBytes))...)
	if w := doReqBytes(self, "PUT", "/api/v1/users/dave/avatar", big, "image/png"); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("big PUT = %d, want 413", w.Code)
	}
	if w := doReqBytes(god, "PUT", "/api/v1/users/ghost/avatar", encodeUploadPNG(t, 4, 4), "image/png"); w.Code != http.StatusNotFound {
		t.Errorf("ghost PUT = %d, want 404", w.Code)
	}
	// Failed uploads leave the good avatar in place.
	if raw, got, err := s.GetUserAvatar(ctx, "dave"); err != nil || got.AvatarContentType != "image/png" {
		t.Errorf("failed uploads must not clobber: %+v %v", got, err)
	} else if ww, hh := decodeUploadSize(t, raw); ww != 4 || hh != 4 {
		t.Errorf("failed uploads changed the bytes: %dx%d", ww, hh)
	}
}

func TestUserAvatarUploadBumpsCacheBust(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	// Advancing clock: installs a minute apart so the RFC3339-second
	// pointer provably changes (testService's fixed clock would tie).
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tick := 0
	s.Now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Minute) }
	self := testHandler(s, authPrincipal("dave"))

	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.putUserAvatar(ctx, "dave", svg); err != nil {
		t.Fatal(err)
	}
	before := s.UserAvatarURL(ctx, "dave")
	w1 := doReq(self, "GET", "/api/v1/users/dave/avatar", "")
	etag1 := w1.Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("missing ETag on generated avatar")
	}
	if _, err := s.PutUserAvatarBytes(ctx, "dave", encodeUploadPNG(t, 8, 4)); err != nil {
		t.Fatal(err)
	}
	after := s.UserAvatarURL(ctx, "dave")
	if after == before {
		t.Errorf("upload must change the ?v= cache-buster: %q unchanged", after)
	}
	w2 := doReq(self, "GET", "/api/v1/users/dave/avatar", "")
	etag2 := w2.Header().Get("ETag")
	if etag2 == "" || etag2 == etag1 {
		t.Errorf("upload must change the ETag: %q vs %q", etag2, etag1)
	}
	if w2.Body.String() == svg {
		t.Error("upload must replace the generated bytes")
	}
}

func TestRegenerateReplacesUpload(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.ResolveUsername(ctx, "dave@example.com"); err != nil {
		t.Fatal(err)
	}
	// Uploads need an existing profile (no synthesis — the 404 rule);
	// the login-time generated path creates it, an explicit upload does
	// not.
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUserAvatarBytes(ctx, "dave", encodeUploadPNG(t, 8, 4)); err != nil {
		t.Fatal(err)
	}
	if raw, got, _ := s.GetUserAvatar(ctx, "dave"); got.AvatarContentType != "image/png" || raw == nil {
		t.Fatalf("upload did not install: %+v", got)
	}
	// POST regenerate replaces the upload with the deterministic render
	// (the #601 decision: regenerate stays visible beside the upload
	// control and is the documented opt-back-in after Remove).
	re, err := s.RegenerateUserAvatar(ctx, "dave")
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if re.AvatarDisabled || re.AvatarContentType != userAvatarContentType {
		t.Fatalf("regenerate must reinstall the generated pointer: %+v", re)
	}
	svg, err := GenerateUserAvatarSVG("dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _, err := s.GetUserAvatar(ctx, "dave"); err != nil || string(raw) != svg {
		t.Fatal("regeneration must reproduce the identical generated avatar over the upload")
	}
	// HTTP: self POST over an upload → 200 generated pointer.
	self := testHandler(s, authPrincipal("dave"))
	if _, err := s.PutUserAvatarBytes(ctx, "dave", encodeUploadPNG(t, 6, 6)); err != nil {
		t.Fatal(err)
	}
	w := doReq(self, "POST", "/api/v1/users/dave/avatar", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST over upload = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"avatar_content_type":"image/svg+xml"`) {
		t.Errorf("POST must restore the generated pointer: %s", w.Body.String())
	}
}

func TestDeleteOptsOutUpload(t *testing.T) {
	s := testService()
	ctx := reqCtx()
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUserAvatarBytes(ctx, "dave", encodeUploadPNG(t, 8, 4)); err != nil {
		t.Fatal(err)
	}
	// DELETE clears the upload exactly like a generated avatar: pointer
	// clears, flag sets, bytes go away.
	del, err := s.DeleteUserAvatar(ctx, "dave")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !del.AvatarDisabled || del.AvatarContentType != "" || del.AvatarUpdatedAt != "" {
		t.Fatalf("delete must clear pointer + set opt-out: %+v", del)
	}
	if raw, _, _ := s.GetUserAvatar(ctx, "dave"); raw != nil {
		t.Fatal("upload bytes survived delete")
	}
	self := testHandler(s, authPrincipal("dave"))
	if w := doReq(self, "GET", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", w.Code)
	}
	// Repeat DELETE is idempotent 200 (still opted out).
	if w := doReq(self, "DELETE", "/api/v1/users/dave/avatar", ""); w.Code != http.StatusOK {
		t.Errorf("repeat DELETE = %d, want 200", w.Code)
	}
	// Re-upload after delete opts back in (install clears the flag —
	// upload is an explicit having-an-avatar, like regenerate).
	up, err := s.PutUserAvatarBytes(ctx, "dave", encodeUploadPNG(t, 4, 4))
	if err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if up.AvatarDisabled || up.AvatarContentType != uploadedUserAvatarContentType {
		t.Fatalf("re-upload must clear opt-out + set pointer: %+v", up)
	}
}

// fakeUploadDims rewrites the IHDR dimensions of a real small PNG and
// fixes the chunk CRC, so DecodeConfig reports w×h while the body stays
// tiny: the dimension gate must reject from the header alone, before
// any pixel buffer is allocated (the decompression-bomb guard — a
// solid-color 8000×8000 PNG is ~424 KiB on the wire but 244 MiB
// decoded, doubled by the crop copy).
func fakeUploadDims(t *testing.T, w, h int) []byte {
	t.Helper()
	raw := append([]byte(nil), encodeUploadPNG(t, 4, 4)...)
	// PNG layout: 8-byte signature, 4-byte length, 4-byte "IHDR",
	// then width (16..20) and height (20..24).
	binary.BigEndian.PutUint32(raw[16:20], uint32(w))
	binary.BigEndian.PutUint32(raw[20:24], uint32(h))
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || cfg.Width != w || cfg.Height != h {
		t.Fatalf("fake dims unreadable: %+v %v", cfg, err)
	}
	return raw
}

func TestCropSquarePNGRejectsDimensions(t *testing.T) {
	for _, c := range []struct {
		name string
		w, h int
	}{
		{"side over cap", 5000, 100},
		{"tall side over cap", 100, 5000},
		{"pixels over cap, sides within", 4000, 4200}, // 16.8M > 16M
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := fakeUploadDims(t, c.w, c.h)
			if _, err := cropSquarePNG(raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("cropSquarePNG = %v, want ErrInvalid", err)
			} else if !strings.Contains(err.Error(), "dimensions") {
				t.Errorf("dimension error must say dimensions: %v", err)
			}
		})
	}
	// Service + HTTP surface: dimension rejects are 400 (bad image),
	// never 500 — and must not clobber the installed avatar.
	s := testService()
	ctx := reqCtx()
	if _, err := s.EnsureProfile(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUserAvatarBytes(ctx, "dave", encodeUploadPNG(t, 4, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUserAvatarBytes(ctx, "dave", fakeUploadDims(t, 5000, 5000)); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversize upload = %v, want ErrInvalid", err)
	}
	self := testHandler(s, authPrincipal("dave"))
	if w := doReqBytes(self, "PUT", "/api/v1/users/dave/avatar", fakeUploadDims(t, 5000, 5000), "image/png"); w.Code != http.StatusBadRequest {
		t.Errorf("oversize PUT = %d, want 400", w.Code)
	}
	if raw, got, err := s.GetUserAvatar(ctx, "dave"); err != nil || got.AvatarContentType != uploadedUserAvatarContentType {
		t.Errorf("rejected dimensions must not clobber: %+v %v", got, err)
	} else if ww, hh := decodeUploadSize(t, raw); ww != 4 || hh != 4 {
		t.Errorf("rejected dimensions changed the bytes: %dx%d", ww, hh)
	}
	// A realistic photo-sized image passes the gate and crops square.
	out, err := cropSquarePNG(encodeUploadJPEG(t, 1600, 1200))
	if err != nil {
		t.Fatalf("photo-sized crop: %v", err)
	}
	if img, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	} else if b := img.Bounds(); b.Dx() != 1200 || b.Dy() != 1200 {
		t.Errorf("photo crop = %dx%d, want 1200x1200", b.Dx(), b.Dy())
	}
}
