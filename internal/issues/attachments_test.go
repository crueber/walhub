package issues

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// --- §12 attachment fixtures ----------------------------------------------------

// Minimal magic-prefixed bodies (the server sniffs magic only, so short
// bodies are faithful).
var (
	pngBody  = append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("fakepng")...)
	jpgBody  = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("fakejpg")...)
	gifBody  = []byte("GIF89afakegif")
	webpBody = []byte("RIFF\x00\x00\x00\x00WEBPfake")
	svgBody  = []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	txtBody  = []byte("just some text, not an image")
)

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// doUpload issues a raw-byte POST …/api/attachments with optional query,
// headers, and principal.
func doUpload(h *Handler, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeRecord(t *testing.T, w *httptest.ResponseRecorder) *AttachmentRecord {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %q", w.Code, w.Body.String())
	}
	var rec AttachmentRecord
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatalf("record decode: %v", err)
	}
	return &rec
}

// --- upload: happy paths ----------------------------------------------------------

func TestUploadAttachment(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	s.SpoolDir = t.TempDir()
	h := testHandler(s, janeP)

	t.Run("png with name and sha", func(t *testing.T) {
		w := doUpload(h, "/acme/repo/api/attachments?name=shot.png", pngBody,
			map[string]string{"X-Walgit-Attachment-Sha256": shaOf(pngBody)})
		rec := decodeRecord(t, w)
		if rec.Name != "shot.png" || rec.Size != int64(len(pngBody)) ||
			rec.SHA256 != shaOf(pngBody) || rec.ContentType != "image/png" {
			t.Fatalf("record = %+v", rec)
		}
		wantURL := "/acme/repo/attachments/" + shaOf(pngBody) + "/shot.png"
		if rec.URL != wantURL {
			t.Fatalf("url = %q, want %q", rec.URL, wantURL)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content-type = %q", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("cache-control = %q", cc)
		}
	})

	t.Run("sha header optional (S4)", func(t *testing.T) {
		w := doUpload(h, "/acme/repo/api/attachments?name=nosha.jpg", jpgBody, nil)
		rec := decodeRecord(t, w)
		if rec.SHA256 != shaOf(jpgBody) || rec.ContentType != "image/jpeg" {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("missing name defaults by type", func(t *testing.T) {
		for _, c := range []struct {
			body []byte
			name string
			ct   string
		}{
			{gifBody, "image.gif", "image/gif"},
			{webpBody, "image.webp", "image/webp"},
		} {
			w := doUpload(h, "/acme/repo/api/attachments", c.body, nil)
			rec := decodeRecord(t, w)
			if rec.Name != c.name || rec.ContentType != c.ct {
				t.Fatalf("record = %+v, want name %q type %q", rec, c.name, c.ct)
			}
		}
	})

	t.Run("browser lane", func(t *testing.T) {
		w := doUpload(h, "/acme/repo/api-browser/attachments?name=lane.png", pngBody, nil)
		decodeRecord(t, w)
	})

	t.Run("dedup is idempotent 201", func(t *testing.T) {
		a := doUpload(h, "/acme/repo/api/attachments?name=dup.png", pngBody, nil)
		b := doUpload(h, "/acme/repo/api/attachments?name=dup.png", pngBody, nil)
		ra, rb := decodeRecord(t, a), decodeRecord(t, b)
		if *ra != *rb {
			t.Fatalf("dedup records differ: %+v vs %+v", ra, rb)
		}
	})

	t.Run("same bytes under another name stores independently", func(t *testing.T) {
		w := doUpload(h, "/acme/repo/api/attachments?name=other.png", pngBody, nil)
		rec := decodeRecord(t, w)
		if rec.Name != "other.png" || rec.SHA256 != shaOf(pngBody) {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("no Content-Length required (B1)", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/acme/repo/api/attachments?name=chunked.png", bytes.NewReader(pngBody))
		r.ContentLength = -1 // chunked-style: unknown length must still upload
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		decodeRecord(t, w)
	})

	t.Run(" Gif87a accepted", func(t *testing.T) {
		w := doUpload(h, "/acme/repo/api/attachments?name=a.gif", []byte("GIF87a0123456789"), nil)
		if rec := decodeRecord(t, w); rec.ContentType != "image/gif" {
			t.Fatalf("record = %+v", rec)
		}
	})
}

// --- upload: rejections (table-driven) ----------------------------------------------

func TestUploadAttachmentRejections(t *testing.T) {
	newSetup := func() (*Service, *Handler) {
		roles := newFakeRoles()
		s := testService(roles)
		s.SpoolDir = t.TempDir()
		return s, testHandler(s, janeP)
	}
	cases := []struct {
		name       string
		target     string
		body       []byte
		headers    map[string]string
		wantStatus int
		wantFrag   string
	}{
		{"sha mismatch", "/acme/repo/api/attachments?name=a.png", pngBody,
			map[string]string{"X-Walgit-Attachment-Sha256": strings.Repeat("0", 64)}, 400, "mismatch"},
		{"sha malformed", "/acme/repo/api/attachments?name=a.png", pngBody,
			map[string]string{"X-Walgit-Attachment-Sha256": "zzz"}, 400, "hex"},
		{"svg rejected", "/acme/repo/api/attachments?name=x.svg", svgBody, nil, 415, "only PNG"},
		{"text rejected", "/acme/repo/api/attachments?name=x.txt", txtBody, nil, 415, "only PNG"},
		{"empty rejected", "/acme/repo/api/attachments?name=x.png", []byte{}, nil, 415, "only PNG"},
		{"truncated magic", "/acme/repo/api/attachments?name=x.png", []byte{0x89, 0x50}, nil, 415, "only PNG"},
		{"dotdot name", "/acme/repo/api/attachments?name=..%2F..%2Fx.png", pngBody, nil, 400, "attachment name"},
		{"leading dot", "/acme/repo/api/attachments?name=.hidden.png", pngBody, nil, 400, "'.'"},
		{"long name", "/acme/repo/api/attachments?name=" + strings.Repeat("n", 201) + ".png", pngBody, nil, 400, "exceeds"},
		{"control char", "/acme/repo/api/attachments?name=a%01b.png", pngBody, nil, 400, "control"},
		{"blank name defaults", "/acme/repo/api/attachments?name=%20", pngBody, nil, 201, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, h := newSetup()
			w := doUpload(h, c.target, c.body, c.headers)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %q", w.Code, c.wantStatus, w.Body.String())
			}
			if c.wantFrag != "" && !strings.Contains(w.Body.String(), c.wantFrag) {
				t.Fatalf("body %q lacks %q", w.Body.String(), c.wantFrag)
			}
			if c.wantStatus != 201 {
				if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
					t.Fatalf("error content-type = %q, want text/plain", ct)
				}
			}
		})
	}

	t.Run("over cap → 413", func(t *testing.T) {
		roles := newFakeRoles()
		s := testService(roles)
		s.SpoolDir = t.TempDir()
		s.MaxImageBytes = 4
		h := testHandler(s, janeP)
		w := doUpload(h, "/acme/repo/api/attachments?name=big.png", pngBody, nil)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %q", w.Code, w.Body.String())
		}
	})

	t.Run("anonymous → 401", func(t *testing.T) {
		roles := newFakeRoles()
		s := testService(roles)
		s.SpoolDir = t.TempDir()
		h := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return auth.Anonymous(), nil
		}}
		w := doUpload(h, "/acme/repo/api/attachments?name=a.png", pngBody, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %q", w.Code, w.Body.String())
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing WWW-Authenticate on 401")
		}
		// Anonymous byte-GET on a public repo is allowed (read gate,
		// anonymous iff the repo is public); on a private repo it is 401.
		rec := mustUpload(t, testHandler(s, janeP), "/acme/repo/api/attachments?name=pub.png", pngBody)
		if w2 := getRepo(h, rec, nil, ""); w2.Code != http.StatusOK {
			t.Fatalf("anon public GET = %d", w2.Code)
		}
	})

	t.Run("private repo gates", func(t *testing.T) {
		roles := newFakeRoles()
		roles.private["acme/repo"] = true
		roles.grant("acme", "repo", "jane@example.com", "read")
		s := testService(roles)
		s.SpoolDir = t.TempDir()
		rec := mustUpload(t, testHandler(s, janeP), "/acme/repo/api/attachments?name=priv.png", pngBody)

		stranger := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return bobP, nil
		}}
		if w := doUpload(stranger, "/acme/repo/api/attachments?name=s.png", pngBody, nil); w.Code != http.StatusForbidden {
			t.Fatalf("stranger upload = %d, want 403", w.Code)
		}
		if w := getRepo(stranger, rec, nil, ""); w.Code != http.StatusForbidden {
			t.Fatalf("stranger GET = %d, want 403", w.Code)
		}
		anon := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return auth.Anonymous(), nil
		}}
		if w := doUpload(anon, "/acme/repo/api/attachments?name=s.png", pngBody, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("anon upload = %d, want 401", w.Code)
		}
		if w2 := getRepo(anon, rec, nil, ""); w2.Code != http.StatusUnauthorized {
			t.Fatalf("anon GET = %d, want 401", w2.Code)
		}
	})
}

// --- byte GET --------------------------------------------------------------------------

func mustUpload(t *testing.T, h *Handler, target string, body []byte) *AttachmentRecord {
	t.Helper()
	w := doUpload(h, target, body, nil)
	return decodeRecord(t, w)
}

// getRepo issues a byte-GET through HandleRepo exactly as the server's
// repoDispatch calls it (decoded sub segments, GET|HEAD).
func getRepo(h *Handler, rec *AttachmentRecord, headers map[string]string, method string) *httptest.ResponseRecorder {
	if method == "" {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, rec.URL, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	if !h.HandleRepo(w, r, git.RepoId{Owner: "acme", Name: "repo"}, []string{"attachments", rec.SHA256, rec.Name}) {
		panic("HandleRepo(byte route) not claimed")
	}
	return w
}

func TestServeAttachment(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	s.SpoolDir = t.TempDir()
	h := testHandler(s, janeP)
	rec := mustUpload(t, h, "/acme/repo/api/attachments?name=shot.png", pngBody)

	// As repoDispatch calls it: sub segments arrive percent-decoded,
	// and the byte shape is exactly ["attachments", sha, name].
	get := func(sha, name string, headers map[string]string, method string) *httptest.ResponseRecorder {
		if method == "" {
			method = http.MethodGet
		}
		r := httptest.NewRequest(method, "/acme/repo/attachments/"+sha+"/"+name, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		if !h.HandleRepo(w, r, git.RepoId{Owner: "acme", Name: "repo"}, []string{"attachments", sha, name}) {
			t.Fatalf("HandleRepo(attachments/%s/%s) not claimed", sha, name)
		}
		return w
	}
	sha, name := rec.SHA256, rec.Name

	t.Run("200 with static contract headers", func(t *testing.T) {
		w := get(sha, name, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %q", w.Code, w.Body.String())
		}
		hdr := w.Header()
		if got := hdr.Get("Content-Type"); got != "image/png" {
			t.Fatalf("content-type = %q", got)
		}
		if got := hdr.Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
			t.Fatalf("cache-control = %q", got)
		}
		if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("nosniff = %q", got)
		}
		if got := hdr.Get("Accept-Ranges"); got != "bytes" {
			t.Fatalf("accept-ranges = %q", got)
		}
		if hdr.Get("ETag") == "" {
			t.Fatal("missing ETag")
		}
		if !bytes.Equal(w.Body.Bytes(), pngBody) {
			t.Fatal("body mismatch")
		}
	})

	t.Run("HEAD has headers, no body", func(t *testing.T) {
		w := get(sha, name, nil, http.MethodHead)
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatalf("head = %d len %d", w.Code, w.Body.Len())
		}
		if w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("head content-type = %q", w.Header().Get("Content-Type"))
		}
	})

	t.Run("304 on match (*, strong, weak)", func(t *testing.T) {
		w := get(sha, name, nil, "")
		etag := w.Header().Get("ETag")
		for _, inm := range []string{etag, "*", "W/" + etag} {
			w2 := get(sha, name, map[string]string{"If-None-Match": inm}, "")
			if w2.Code != http.StatusNotModified {
				t.Fatalf("inm %q = %d", inm, w2.Code)
			}
		}
	})

	t.Run("206 range + suffix range", func(t *testing.T) {
		w := get(sha, name, map[string]string{"Range": "bytes=0-3"}, "")
		if w.Code != http.StatusPartialContent {
			t.Fatalf("range = %d: %q", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), pngBody[:4]) {
			t.Fatalf("range body = %x", w.Body.Bytes())
		}
		if cr := w.Header().Get("Content-Range"); cr != "bytes 0-3/"+itoa(len(pngBody)) {
			t.Fatalf("content-range = %q", cr)
		}
		ws := get(sha, name, map[string]string{"Range": "bytes=-4"}, "")
		if ws.Code != http.StatusPartialContent || !bytes.Equal(ws.Body.Bytes(), pngBody[len(pngBody)-4:]) {
			t.Fatalf("suffix = %d %x", ws.Code, ws.Body.Bytes())
		}
	})

	t.Run("206 offset range (sniff side path)", func(t *testing.T) {
		w := get(sha, name, map[string]string{"Range": "bytes=4-7"}, "")
		if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), pngBody[4:8]) {
			t.Fatalf("offset = %d %x", w.Code, w.Body.Bytes())
		}
		if w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("range content-type = %q", w.Header().Get("Content-Type"))
		}
	})

	t.Run("416 with content-range", func(t *testing.T) {
		w := get(sha, name, map[string]string{"Range": "bytes=9999-10000"}, "")
		if w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d", w.Code)
		}
		if cr := w.Header().Get("Content-Range"); cr != "bytes */"+itoa(len(pngBody)) {
			t.Fatalf("content-range = %q", cr)
		}
	})

	t.Run("If-Range mismatch → full 200", func(t *testing.T) {
		w := get(sha, name, map[string]string{"Range": "bytes=0-3", "If-Range": `"stale"`}, "")
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), pngBody) {
			t.Fatalf("if-range = %d len %d", w.Code, w.Body.Len())
		}
	})

	t.Run("If-Range match → 206", func(t *testing.T) {
		w0 := get(sha, name, nil, "")
		etag := w0.Header().Get("ETag")
		w := get(sha, name, map[string]string{"Range": "bytes=0-3", "If-Range": etag}, "")
		if w.Code != http.StatusPartialContent {
			t.Fatalf("if-range match = %d", w.Code)
		}
	})

	t.Run("unknown sha / bad shape / bad name → 404", func(t *testing.T) {
		for _, sub := range [][]string{
			{"attachments", strings.Repeat("0", 64), "ghost.png"},
			{"attachments", "zzz", "shot.png"},
			{"attachments", shaOf(pngBody), ".."},
		} {
			r := httptest.NewRequest(http.MethodGet, "/acme/repo/attachments/x", nil)
			w := httptest.NewRecorder()
			if !h.HandleRepo(w, r, git.RepoId{Owner: "acme", Name: "repo"}, sub) {
				t.Fatalf("HandleRepo(%v) not claimed", sub)
			}
			if w.Code != http.StatusNotFound {
				t.Fatalf("HandleRepo(%v) = %d, want 404", sub, w.Code)
			}
		}
		// Uppercase sha normalizes to the same object (200, not 404).
		r := httptest.NewRequest(http.MethodGet, "/acme/repo/attachments/x", nil)
		w := httptest.NewRecorder()
		if !h.HandleRepo(w, r, git.RepoId{Owner: "acme", Name: "repo"},
			[]string{"attachments", strings.ToUpper(sha), name}) {
			t.Fatal("uppercase-sha route not claimed")
		}
		if w.Code != http.StatusOK {
			t.Fatalf("uppercase sha = %d, want 200", w.Code)
		}
		// Wrong arity / wrong family fall through to the core mux (false).
		for _, sub := range [][]string{
			{"attachments", shaOf(pngBody)},
			{"attachments", shaOf(pngBody), "a", "b"},
			{"other", strings.Repeat("a", 64), "x.png"},
		} {
			r := httptest.NewRequest(http.MethodGet, "/acme/repo/x", nil)
			if h.HandleRepo(httptest.NewRecorder(), r, git.RepoId{Owner: "acme", Name: "repo"}, sub) {
				t.Fatalf("HandleRepo(%v) = true, want false", sub)
			}
		}
	})

	t.Run("corrupt stored bytes → 503", func(t *testing.T) {
		mustPut(t, s, AttachmentKey("acme", "repo", strings.Repeat("1", 64), "evil.png"), txtBody)
		w := get(strings.Repeat("1", 64), "evil.png", nil, "")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("corrupt = %d", w.Code)
		}
	})
}

// --- routing / methods / auth ---------------------------------------------------------------

func TestAttachmentRouting(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	s.SpoolDir = t.TempDir()
	h := testHandler(s, janeP)

	// Non-attachment paths still fall through (the byte family is NOT on
	// the Handle chain — it rides ChainRepo/HandleRepo, so even the byte
	// shape reports false here and the core mux dispatches to HandleRepo).
	for _, target := range []string{
		"/acme/repo/api/bogus",
		"/acme/repo/attachments",
		"/acme/repo/attachments/" + strings.Repeat("a", 64) + "/x.png",
		"/acme/repo/other/" + strings.Repeat("a", 64) + "/x.png",
	} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if h.Handle(httptest.NewRecorder(), r) {
			t.Errorf("Handle(%q) = true, want false", target)
		}
	}
	// Method mismatches on the lane route → 405 with Allow.
	for _, c := range []struct{ method, target string }{
		{"GET", "/acme/repo/api/attachments"},
		{"PUT", "/acme/repo/api/attachments"},
	} {
		r := httptest.NewRequest(c.method, c.target, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") == "" {
			t.Errorf("%s %s = %d (Allow=%q), want 405", c.method, c.target, w.Code, w.Header().Get("Allow"))
		}
	}
	// Method mismatches on the byte route → 405 with Allow (via HandleRepo).
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		r := httptest.NewRequest(method, "/acme/repo/attachments/x", nil)
		w := httptest.NewRecorder()
		id := git.RepoId{Owner: "acme", Name: "repo"}
		if !h.HandleRepo(w, r, id, []string{"attachments", strings.Repeat("b", 64), "x.png"}) {
			t.Errorf("HandleRepo(%s) not claimed", method)
		}
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") == "" {
			t.Errorf("HandleRepo(%s) = %d (Allow=%q), want 405", method, w.Code, w.Header().Get("Allow"))
		}
	}
	// Unknown issue-shaped path under api/attachments falls through.
	r := httptest.NewRequest(http.MethodPost, "/acme/repo/api/attachments/extra", nil)
	if h.Handle(httptest.NewRecorder(), r) {
		t.Error("Handle(api/attachments/extra) = true, want false")
	}
	// Bad repo id falls through on both lanes.
	for _, target := range []string{
		"/Bad%20Owner/repo/attachments/" + strings.Repeat("c", 64) + "/x.png",
		"/Bad%20Owner/repo/api/attachments",
	} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if h.Handle(httptest.NewRecorder(), r) {
			t.Errorf("Handle(%q) = true, want false", target)
		}
	}
}

func TestAttachmentAuth(t *testing.T) {
	roles := newFakeRoles()
	roles.private["acme/repo"] = true
	s := testService(roles)
	s.SpoolDir = t.TempDir()
	// Seed one object as a reader.
	roles.grant("acme", "repo", "jane@example.com", "read")
	h := testHandler(s, janeP)
	rec := mustUpload(t, h, "/acme/repo/api/attachments?name=priv.png", pngBody)

	// Identity-down reads fail 503 through the service gate.
	roles.unavail = true
	if w := getRepo(h, rec, nil, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavail GET = %d, want 503", w.Code)
	}
	roles.unavail = false

	// Auth-chain errors map through writeErr (403 here).
	forbidden := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	}}
	if w2 := getRepo(forbidden, rec, nil, ""); w2.Code != http.StatusForbidden {
		t.Fatalf("auth-fail GET = %d, want 403", w2.Code)
	}
}

// --- unit tables ---------------------------------------------------------------------

func TestSniffImageType(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		ct   string
		ok   bool
	}{
		{"png", pngBody, "image/png", true},
		{"jpeg", jpgBody, "image/jpeg", true},
		{"gif89", gifBody, "image/gif", true},
		{"gif87", []byte("GIF87a12345"), "image/gif", true},
		{"webp", webpBody, "image/webp", true},
		{"svg", svgBody, "", false},
		{"xml decl", []byte(`<?xml version="1.0"?><svg/>`), "", false},
		{"text", txtBody, "", false},
		{"empty", nil, "", false},
		{"truncated png", []byte{0x89, 0x50, 0x4E}, "", false},
		{"riff not webp", []byte("RIFF\x00\x00\x00\x00AVI "), "", false},
		{"gif88", []byte("GIF88a12345"), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ct, ok := sniffImageType(c.head)
			if ct != c.ct || ok != c.ok {
				t.Fatalf("sniff = %q,%v want %q,%v", ct, ok, c.ct, c.ok)
			}
		})
	}
	for ct, ext := range map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
	} {
		if extForType(ct) != ext {
			t.Fatalf("ext(%q) = %q", ct, extForType(ct))
		}
	}
	if extForType("bogus") != ".bin" {
		t.Fatal("ext fallback")
	}
}

func TestSanitizeAttachmentName(t *testing.T) {
	for _, good := range []string{"a.png", "shot 2.JPG", "caf\u00e9.webp", strings.Repeat("n", 200)} {
		if _, err := sanitizeAttachmentName(good); err != nil {
			t.Errorf("name %q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{"", "   ", ".", "..", ".hidden", "a/b", `a\b`, strings.Repeat("n", 201), "a\x01b", "a\x7fb"} {
		if _, err := sanitizeAttachmentName(bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	// Whitespace trims.
	if n, err := sanitizeAttachmentName("  x.png  "); err != nil || n != "x.png" {
		t.Fatalf("trim = %q,%v", n, err)
	}
}

func TestAttachmentShape(t *testing.T) {
	sha := shaOf(pngBody)
	if s, n, ok := attachmentShape(sha, "a.png"); !ok || s != sha || n != "a.png" {
		t.Fatalf("shape = %q,%q,%v", s, n, ok)
	}
	if _, _, ok := attachmentShape(strings.ToUpper(sha), "a.png"); !ok {
		t.Fatal("uppercase sha rejected (must normalize)")
	}
	for _, bad := range [][2]string{
		{"zzz", "a.png"}, {strings.Repeat("0", 63), "a.png"}, {"", "a.png"},
		{sha, ".."}, {sha, ""},
	} {
		if _, _, ok := attachmentShape(bad[0], bad[1]); ok {
			t.Errorf("shape %q/%q accepted", bad[0], bad[1])
		}
	}
}

func TestAttachmentStatusCodes(t *testing.T) {
	if statusFor(ErrTooLarge) != http.StatusRequestEntityTooLarge {
		t.Fatal("ErrTooLarge ≠ 413")
	}
	if statusFor(ErrUnsupportedMedia) != http.StatusUnsupportedMediaType {
		t.Fatal("ErrUnsupportedMedia ≠ 415")
	}
}

func TestAttachmentConfig(t *testing.T) {
	s := New(nil, nil)
	if s.maxImageBytes() != DefaultMaxImageBytes {
		t.Fatalf("default cap = %d, want %d", s.maxImageBytes(), DefaultMaxImageBytes)
	}
	s.MaxImageBytes = 123
	if s.maxImageBytes() != 123 {
		t.Fatal("override ignored")
	}
	if AttachmentKey("o", "r", "s", "n") != "repos/o/r/attachments/s/n" {
		t.Fatal("key shape")
	}
	if AttachmentsPrefix("o", "r") != "repos/o/r/attachments/" {
		t.Fatal("prefix shape")
	}
}

func TestUploadUnreadableBody(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	s.SpoolDir = t.TempDir()
	if _, err := s.UploadAttachment(reqCtx(), "acme", "repo", janeP, "a.png", errReader{}, ""); err == nil {
		t.Fatal("unreadable body accepted")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

var (
	_ = bytes.MinRead
	_ = io.Discard
)

func TestParseAttachmentRange(t *testing.T) {
	cases := []struct {
		name   string
		spec   string
		size   int64
		wantS  int64
		wantE  int64
		wantOK bool
	}{
		{"full span", "bytes=0-9", 10, 0, 9, true},
		{"open end", "bytes=5-", 10, 5, 9, true},
		{"clamped end", "bytes=5-99", 10, 5, 9, true},
		{"suffix", "bytes=-4", 10, 6, 9, true},
		{"suffix over size", "bytes=-99", 10, 0, 9, true},
		{"case prefix", "Bytes=0-1", 10, 0, 1, true},
		{"spaces", "bytes= 2 - 4 ", 10, 2, 4, true},
		{"no prefix", "0-9", 10, 0, 0, false},
		{"wrong unit", "items=0-9", 10, 0, 0, false},
		{"multi", "bytes=0-1,3-4", 10, 0, 0, false},
		{"no dash", "bytes=99", 10, 0, 0, false},
		{"empty", "bytes=-", 10, 0, 0, false},
		{"suffix zero", "bytes=-0", 10, 0, 0, false},
		{"suffix empty size", "bytes=-4", 0, 0, 0, false},
		{"start past end", "bytes=9-10000", 10, 9, 9, true},
		{"start at size", "bytes=10-12", 10, 0, 0, false},
		{"end before start", "bytes=5-2", 10, 0, 0, false},
		{"bad ints", "bytes=a-b", 10, 0, 0, false},
		{"negative start", "bytes=-5-2", 10, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, e, ok := parseAttachmentRange(c.spec, c.size)
			if s != c.wantS || e != c.wantE || ok != c.wantOK {
				t.Fatalf("parse(%q,%d) = %d,%d,%v want %d,%d,%v",
					c.spec, c.size, s, e, ok, c.wantS, c.wantE, c.wantOK)
			}
		})
	}
}

func TestNormalizeSHA256(t *testing.T) {
	sha := shaOf(pngBody)
	if got, err := normalizeSHA256(""); err != nil || got != "" {
		t.Fatalf("empty = %q,%v", got, err)
	}
	if got, err := normalizeSHA256(strings.ToUpper(sha)); err != nil || got != sha {
		t.Fatalf("upper = %q,%v", got, err)
	}
	for _, bad := range []string{"short", strings.Repeat("0", 63), strings.Repeat("z", 64), strings.Repeat("0", 65)} {
		if _, err := normalizeSHA256(bad); err == nil {
			t.Errorf("sha %q accepted", bad)
		}
	}
}

func TestUploadAuthChainError(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	s.SpoolDir = t.TempDir()
	h := &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}}
	w := doUpload(h, "/acme/repo/api/attachments?name=a.png", pngBody, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestServeAttachmentStoreErrors(t *testing.T) {
	roles := newFakeRoles()
	inner := testService(roles)
	inner.SpoolDir = t.TempDir()
	h0 := testHandler(inner, janeP)
	rec := mustUpload(t, h0, "/acme/repo/api/attachments?name=e.png", pngBody)

	t.Run("get failure → 503", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: inner.Store, failGet: func(key string) error {
			return errStoreDown(key)
		}}
		s := New(fl, roles)
		s.SpoolDir = t.TempDir()
		h := testHandler(s, janeP)
		if w := getRepo(h, rec, nil, ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
	})

	t.Run("range re-get failure → 503", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: inner.Store}
		s := New(fl, roles)
		s.SpoolDir = t.TempDir()
		h := testHandler(s, janeP)
		calls := 0
		fl.failGet = func(key string) error {
			calls++
			if calls > 1 {
				return errStoreDown(key)
			}
			return nil
		}
		if w := getRepo(h, rec, map[string]string{"Range": "bytes=0-2"}, ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
	})
}

func errStoreDown(key string) error {
	return &storeErrorDown{key: key}
}

type storeErrorDown struct{ key string }

func (e *storeErrorDown) Error() string { return "store down: " + e.key }
