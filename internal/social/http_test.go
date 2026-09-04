package social

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// do fires one request through the handler with test-principal headers.
func do(t *testing.T, x *harness, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	x.handler.ServeHTTP(rec, req)
	return rec
}

func asUser(name string) map[string]string { return map[string]string{"X-Test-Principal": name} }

func TestSocialHTTPTable(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	seedRepo(t, x, "o.git", "r")
	rows := []struct {
		name   string
		method string
		path   string
		head   map[string]string
		status int
	}{
		{"anon star denied", "PUT", "/o/r/api/star", nil, 401},
		{"anon unstar denied", "DELETE", "/o/r/api/star", nil, 401},
		{"star", "PUT", "/o/r/api/star", asUser("jane"), 200},
		{"restar idempotent", "PUT", "/o/r/api/star", asUser("jane"), 200},
		{"social", "GET", "/o/r/api/social", asUser("jane"), 200},
		{"social anon", "GET", "/o/r/api/social", nil, 401},
		{"social browser lane", "GET", "/o/r/api-browser/social", asUser("jane"), 200},
		{"social dotgit", "GET", "/o.git/r.git/api/social", asUser("jane"), 200},
		{"star wrong method", "GET", "/o/r/api/star", asUser("jane"), 405},
		{"social wrong method", "POST", "/o/r/api/social", asUser("jane"), 405},
		{"unstar", "DELETE", "/o/r/api/star", asUser("jane"), 200},
		{"me starred anon", "GET", "/api/v1/me/starred", nil, 401},
		{"me starred", "GET", "/api/v1/me/starred?n=10", asUser("jane"), 200},
		{"me starred browser", "GET", "/api-browser/v1/me/starred", asUser("jane"), 200},
		{"user starred", "GET", "/api/v1/users/jane/starred", nil, 200},
		{"user starred browser", "GET", "/api-browser/v1/users/jane/starred", nil, 200},
		{"me starred wrong method", "POST", "/api/v1/me/starred", asUser("jane"), 405},
		{"user starred wrong method", "DELETE", "/api/v1/users/jane/starred", nil, 405},
		{"bad n", "GET", "/api/v1/me/starred?n=x", asUser("jane"), 400},
		{"watch untouched", "PUT", "/o/r/api/watch", asUser("jane"), 404},
		{"non social untouched", "GET", "/o/r/api/refs", asUser("jane"), 404},
		{"bad repo untouched", "GET", "/bad!o/r/api/social", asUser("jane"), 404},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := do(t, x, row.method, row.path, nil, row.head)
			if rec.Code != row.status {
				t.Fatalf("%s %s: got %d want %d (%q)", row.method, row.path, rec.Code, row.status, rec.Body.String())
			}
		})
	}

	// Star response shape + social viewer flags.
	x2 := newHarness(t)
	seedRepo(t, x2, "o", "r")
	rec := do(t, x2, "PUT", "/o/r/api/star", nil, asUser("jane"))
	var star map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &star); err != nil {
		t.Fatal(err)
	}
	if star["stars"] != float64(1) {
		t.Fatalf("star count: %v", star)
	}
	seedWatchRecord(t, x2, "jane", "o", "r")
	rec2 := do(t, x2, "GET", "/o/r/api/social", nil, asUser("jane"))
	var soc map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &soc); err != nil {
		t.Fatal(err)
	}
	if soc["stars"] != float64(1) || soc["watchers"] != float64(0) || soc["forks"] != float64(0) {
		t.Fatalf("counters: %v", soc)
	}
	viewer, _ := soc["viewer"].(map[string]any)
	if viewer["starred"] != true || viewer["watching"] != true {
		t.Fatalf("viewer: %v", viewer)
	}
	if cc := rec2.Header().Get("Cache-Control"); !strings.Contains(cc, "stale-while-revalidate") {
		t.Fatalf("class: %q", cc)
	}
	etag := rec2.Header().Get("ETag")
	rec3 := do(t, x2, "GET", "/o/r/api/social", nil,
		mergeHeaders(asUser("jane"), map[string]string{"If-None-Match": etag}))
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("304: %d", rec3.Code)
	}
	// Starred list entries.
	rec4 := do(t, x2, "GET", "/api/v1/me/starred", nil, asUser("jane"))
	var tray map[string]any
	if err := json.Unmarshal(rec4.Body.Bytes(), &tray); err != nil {
		t.Fatal(err)
	}
	items, _ := tray["starred"].([]any)
	if len(items) != 1 {
		t.Fatalf("starred: %v", tray)
	}
	first, _ := items[0].(map[string]any)
	if first["repo"] != "o/r" {
		t.Fatalf("entry: %v", first)
	}
}

func mergeHeaders(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestSocialPrincipalFallback(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = nil
	rec := do(t, x, "PUT", "/o/r/api/star", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("nil auth: %d", rec.Code)
	}
}

// TestSocialDeletedRepoTable pins the #63 HTTP surface for ghost repos
// (no manifest seeded): mutations fail closed, lists tolerate.
func TestSocialDeletedRepoTable(t *testing.T) {
	x := newHarness(t)
	// A stale userspace record with no repo behind it.
	seedSocialKey(t, x, StarredPrefix("jane")+"o/ghost.json", `{"repo":"o/ghost","starred_at":"2026-09-04T12:00:00Z"}`)
	rows := []struct {
		name   string
		method string
		path   string
		head   map[string]string
		status int
		body   string // "" = unchecked; otherwise must be contained
	}{
		{"star ghost 404", "PUT", "/o/ghost/api/star", asUser("jane"), 404, "not found"},
		{"social ghost 404", "GET", "/o/ghost/api/social", asUser("jane"), 404, "not found"},
		{"unstar ghost cleans record", "DELETE", "/o/ghost/api/star", asUser("jane"), 200, `"stars":0`},
		{"social anon still 401 first", "GET", "/o/ghost/api/social", nil, 401, ""},
		{"starred skips ghost", "GET", "/api/v1/me/starred", asUser("jane"), 200, `"starred":[]`},
		{"user starred skips ghost", "GET", "/api/v1/users/jane/starred", nil, 200, `"starred":[]`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := do(t, x, row.method, row.path, nil, row.head)
			if rec.Code != row.status {
				t.Fatalf("%s %s: got %d want %d (%q)", row.method, row.path, rec.Code, row.status, rec.Body.String())
			}
			if row.body != "" && !strings.Contains(rec.Body.String(), row.body) {
				t.Fatalf("%s %s: body %q lacks %q", row.method, row.path, rec.Body.String(), row.body)
			}
		})
	}
	// The ghost unstar above removed the record: a second unstar is a
	// quiet no-op, and no social.json was resurrected.
	rec := do(t, x, "DELETE", "/o/ghost/api/star", nil, asUser("jane"))
	if rec.Code != 200 {
		t.Fatalf("re-unstar: %d", rec.Code)
	}
	if raw, _, err := store.GetBytes(ctx(), x.svc.Store, SocialKey("o", "ghost"), store.GetOptions{}); err == nil && raw != nil {
		t.Fatalf("resurrected social.json: %s", raw)
	}
}

// TestStarredPagesTable walks the keyset pages through the HTTP twins
// (#65): key-ordered n=2 pages, ghost + corrupt skips mid-walk, the
// n clamp, []-not-null on the exhausted page, and the plain-text 400 on
// a malformed cursor. All stars share the harness timestamp, so the
// after cursors below are deterministic.
func TestStarredPagesTable(t *testing.T) {
	x := newHarness(t)
	for _, r := range []string{"a", "b", "d", "e"} {
		seedRepo(t, x, "o", r)
		if _, err := x.svc.Star(ctx(), jane(), "o", r); err != nil {
			t.Fatal(err)
		}
	}
	// Ghost (record without a manifest) sorts mid-walk and is skipped;
	// corrupt tail record is skipped without failing the page.
	seedSocialKey(t, x, StarredPrefix("jane")+"o/c.json", `{"repo":"o/c","starred_at":"2026-09-04T12:00:00Z"}`)
	seedSocialKey(t, x, StarredPrefix("jane")+"o/z.json", `{oops`)
	ts := "2026-09-04T12:00:00Z"
	rows := []struct {
		name       string
		path       string
		head       map[string]string
		status     int
		wantRepos  []string
		wantMore   bool
		wantBody   string // "" = unchecked; otherwise must be contained
		wantNonNil bool   // assert "starred":[] (never null) in the raw body
	}{
		{"page1 me", "/api/v1/me/starred?n=2", asUser("jane"), 200, []string{"o/a", "o/b"}, true, "", false},
		{"page1 users twin", "/api/v1/users/jane/starred?n=2", nil, 200, []string{"o/a", "o/b"}, true, "", false},
		{"page2 skips ghost", "/api/v1/me/starred?n=2&after=" + url.QueryEscape(ts+"|o/b"), asUser("jane"), 200, []string{"o/d", "o/e"}, false, "", false},
		{"page3 exhausted", "/api/v1/me/starred?n=2&after=" + url.QueryEscape(ts+"|o/e"), asUser("jane"), 200, nil, false, "", true},
		{"clamp", "/api/v1/me/starred?n=500", asUser("jane"), 200, []string{"o/a", "o/b", "o/d", "o/e"}, false, "", false},
		{"bad cursor", "/api/v1/me/starred?after=" + url.QueryEscape("bogus"), asUser("jane"), 400, nil, false, "malformed after cursor", false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := do(t, x, "GET", row.path, nil, row.head)
			if rec.Code != row.status {
				t.Fatalf("GET %s: got %d want %d (%q)", row.path, rec.Code, row.status, rec.Body.String())
			}
			if row.status != 200 {
				if row.wantBody != "" && !strings.Contains(rec.Body.String(), row.wantBody) {
					t.Fatalf("body %q lacks %q", rec.Body.String(), row.wantBody)
				}
				return
			}
			if row.wantNonNil && !strings.Contains(rec.Body.String(), `"starred":[]`) {
				t.Fatalf("nullable list: %s", rec.Body.String())
			}
			var body struct {
				Starred []struct {
					Repo      string `json:"repo"`
					StarredAt string `json:"starred_at"`
				} `json:"starred"`
				More bool `json:"more"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.More != row.wantMore {
				t.Fatalf("more: got %v want %v (%s)", body.More, row.wantMore, rec.Body.String())
			}
			if len(body.Starred) != len(row.wantRepos) {
				t.Fatalf("repos: got %+v want %+v", body.Starred, row.wantRepos)
			}
			for i, want := range row.wantRepos {
				if body.Starred[i].Repo != want {
					t.Fatalf("repos: got %+v want %+v", body.Starred, row.wantRepos)
				}
			}
		})
	}
}
