package pulls

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

var errTestDown = errors.New("backend down")

// authErrHandler returns a Handler whose Auth always fails with kind.
func authErrHandler(e *testEnv, kind auth.AuthErrorKind, why string) *Handler {
	return &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: kind, Why: why}
	}}
}

func TestCoverHTTPAuthErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       auth.AuthErrorKind
		wantStatus int
		wantHdr    string
	}{
		{"invalid", auth.ErrInvalid, 401, "WWW-Authenticate"},
		{"unauthorized", auth.ErrUnauthorized, 401, "WWW-Authenticate"},
		{"forbidden", auth.ErrForbidden, 403, ""},
		{"unavailable", auth.ErrUnavailable, 503, "Retry-After"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			seedOpened(t, e)
			h := authErrHandler(e, tc.kind, "nope")
			for _, target := range []struct {
				method, path string
			}{
				{"GET", "/o/r/api/pulls"},
				{"POST", "/o/r/api/pulls"},
				{"GET", "/o/r/api/pulls/1"},
				{"GET", "/o/r/api/pulls/1/diff"},
				{"GET", "/o/r/api/pulls/1/commits"},
				{"PUT", "/o/r/api/pulls/1"},
				{"POST", "/o/r/api/pulls/1/comments"},
				{"POST", "/o/r/api/pulls/1/merge"},
				{"GET", "/o/r/api/pulls/1/merge/task"},
				{"POST", "/o/r/api/pulls/1/update-branch"},
				{"DELETE", "/o/r/api/pulls/1/head"},
				{"POST", "/api/v1/repos/o/r/forks"},
			} {
				req := httptest.NewRequest(target.method, target.path, strings.NewReader(`{}`))
				w := httptest.NewRecorder()
				h.Handle(w, req)
				if w.Code != tc.wantStatus {
					t.Fatalf("%s %s = %d, want %d (%s)", target.method, target.path, w.Code, tc.wantStatus, w.Body.String())
				}
				if tc.wantHdr != "" && w.Header().Get(tc.wantHdr) == "" {
					t.Fatalf("%s %s missing %s", target.method, target.path, tc.wantHdr)
				}
			}
		})
	}
}

func TestCoverHTTPNilAuth(t *testing.T) {
	e := newTestEnv()
	seedOpened(t, e)
	e.h.Auth = nil // production always injects; nil falls back to anonymous
	req := httptest.NewRequest("GET", "/o/r/api/pulls", nil)
	w := httptest.NewRecorder()
	e.h.Handle(w, req)
	if w.Code != 200 {
		t.Fatalf("nil auth on public = %d (%s)", w.Code, w.Body.String())
	}
}

func TestCoverHTTPMethodShapeMatrix(t *testing.T) {
	e := newTestEnv()
	seedOpened(t, e)
	cases := []struct {
		method, path string
		want         int
	}{
		{"PUT", "/o/r/api/pulls/1/diff", 405},
		{"POST", "/o/r/api/pulls/1/commits", 405},
		{"GET", "/o/r/api/pulls/1/comments", 405},
		{"GET", "/o/r/api/pulls/1/merge", 405},
		{"POST", "/o/r/api/pulls/1/merge/task", 405},
		{"GET", "/o/r/api/pulls/1/update-branch", 405},
		{"POST", "/o/r/api/pulls/1/head", 405},
		{"GET", "/o/r/api/pulls/1/merge/extra", 404},
		{"GET", "/o/r/api/pulls/1/diff/extra", 404},
		{"GET", "/o/r/api/pulls/1/comments/extra", 404},
		{"POST", "/o/r/api/pulls/1/update-branch/extra", 404},
		{"DELETE", "/o/r/api/pulls/1/head/extra", 404},
		{"GET", "/o/r/api/pulls/1/nope", 404},
		{"GET", "/o/r/api/pulls/99999999", 404},    // past the 06x range
		{"GET", "/o/r/api/pulls/1/reactions", 404}, // unknown subroute
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
			w := httptest.NewRecorder()
			if !e.h.Handle(w, req) {
				// Fell through to the core mux: only valid for unknown
				// subroutes; ServeHTTP would 404 them.
				w2 := httptest.NewRecorder()
				e.h.ServeHTTP(w2, req)
				if w2.Code != 404 {
					t.Fatalf("fallthrough = %d", w2.Code)
				}
				return
			}
			if w.Code != tc.want {
				t.Fatalf("= %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestCoverHTTPServiceErrors(t *testing.T) {
	e := newTestEnv()
	seedOpened(t, e)
	e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		return w
	}
	// Unknown PR on every subroute.
	for _, target := range []struct{ method, path, body string }{
		{"GET", "/o/r/api/pulls/99", ""},
		{"GET", "/o/r/api/pulls/99/diff", ""},
		{"GET", "/o/r/api/pulls/99/commits", ""},
		{"PUT", "/o/r/api/pulls/99", `{"title":"x"}`},
		{"POST", "/o/r/api/pulls/99/comments", `{"body":"x"}`},
		{"POST", "/o/r/api/pulls/99/merge", `{"strategy":"merge"}`},
		{"POST", "/o/r/api/pulls/99/update-branch", `{}`},
		{"DELETE", "/o/r/api/pulls/99/head", ""},
	} {
		if w := call(target.method, target.path, target.body); w.Code != 404 && w.Code != 403 {
			t.Fatalf("%s %s = %d (%s)", target.method, target.path, w.Code, w.Body.String())
		}
	}
	// Diff backend error.
	e.git.DiffErr = errTestDown
	if w := call("GET", "/o/r/api/pulls/1/diff", ""); w.Code != 500 && w.Code != 503 {
		t.Fatalf("diff down = %d", w.Code)
	}
	e.git.DiffErr = nil
	// Commits backend error.
	e.git.LogErr = errTestDown
	if w := call("GET", "/o/r/api/pulls/1/commits", ""); w.Code == 200 {
		t.Fatal("log down must fail")
	}
	e.git.LogErr = nil
	// Empty body ⇒ expected an object.
	if w := call("POST", "/o/r/api/pulls", ""); w.Code != 400 {
		t.Fatalf("empty body = %d", w.Code)
	}
	// Fork service error (bad name).
	if w := call("POST", "/api/v1/repos/o/r/forks", `{"name":".."}`); w.Code != 400 {
		t.Fatalf("bad fork = %d", w.Code)
	}
	// Top-level bad repo id falls through (an undecodable owner segment is
	// fail-closed downstream: it never matches a repo).
	req := httptest.NewRequest("POST", "/api/v1/repos/o%20x/r/forks", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	if e.h.Handle(w, req) {
		t.Fatalf("bad top id claimed: %d", w.Code)
	}
	// Top-level wrong shape falls through.
	req = httptest.NewRequest("POST", "/api/v1/repos/o/r/clones", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	if e.h.Handle(w, req) {
		t.Fatal("non-fork top claimed")
	}
	// Empty path never claims.
	req = httptest.NewRequest("GET", "/", nil)
	w = httptest.NewRecorder()
	if e.h.Handle(w, req) {
		t.Fatal("root claimed")
	}
}

func TestCoverHTTPWriters(t *testing.T) {
	// writeJSON marshal failure.
	w := httptest.NewRecorder()
	writeJSON(w, 200, func() {})
	if w.Code != 500 {
		t.Fatalf("marshal fail = %d", w.Code)
	}
	// writeCached marshal failure.
	req := httptest.NewRequest("GET", "/x", nil)
	w = httptest.NewRecorder()
	writeCached(w, req, ccSWR, "e", 200, func() {})
	if w.Code != 500 {
		t.Fatalf("cached marshal fail = %d", w.Code)
	}
	// matchETag variants.
	if !matchETag("*", "abc") || !matchETag(`W/"abc", "def"`, "abc") || !matchETag(`"abc"`, "abc") {
		t.Fatal("etag match")
	}
	if matchETag(`"zzz"`, "abc") || matchETag("", "abc") {
		t.Fatal("etag mismatch")
	}
	// correlationID variants.
	r := httptest.NewRequest("GET", "/x", nil)
	if correlationID(r) != "" {
		t.Fatal("no header")
	}
	r.Header.Set("X-Request-ID", "a")
	if correlationID(r) != "a" {
		t.Fatal("x-request-id")
	}
	r2 := httptest.NewRequest("GET", "/x", nil)
	r2.Header.Set("X-Walgit-Request-ID", "b")
	if correlationID(r2) != "b" {
		t.Fatal("walgit id")
	}
	// writePlain 503 carries Retry-After.
	w = httptest.NewRecorder()
	writePlain(w, 503, "down")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 retry-after")
	}
	// writeErr maps service errors to statuses.
	for err, want := range map[error]int{ErrNotFound: 404, ErrUnavailable: 503} {
		w = httptest.NewRecorder()
		writeErr(w, err)
		if w.Code != want {
			t.Fatalf("writeErr %v = %d", err, w.Code)
		}
	}
	// decodeSegment survives undecodable segments verbatim (fail closed
	// downstream: it won't match a num shape).
	if got := decodeSegment("%zz"); got != "%zz" {
		t.Fatalf("decodeSegment = %q", got)
	}
	if got := decodeSegment("a%20b"); got != "a b" {
		t.Fatalf("decodeSegment = %q", got)
	}
}
