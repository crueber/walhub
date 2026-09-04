package releases

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// do fires one request through the lane handler with test-principal headers.
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

func asReader(name string) map[string]string { return map[string]string{"X-Test-Principal": name} }
func asWriter() map[string]string {
	return map[string]string{"X-Test-Principal": "jane", "X-Test-Write": "1"}
}

func putJSON(t *testing.T, x *harness, tag string, doc map[string]any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(doc)
	return do(t, x, "PUT", "/o/r/api/releases/"+url.PathEscape(tag), raw,
		mergeHeaders(headers, map[string]string{"Content-Type": "application/json"}))
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

func TestReleasesHTTPTable(t *testing.T) {
	x := newHarness(t)
	x.roles.grant("o", "r", "jane", "write")
	x.roles.grant("o", "r", "root", "maintain")
	x.git.tags["v1.0.0"] = strings.Repeat("a", 40)
	x.git.tags["feat/x"] = strings.Repeat("b", 40)

	adminH := map[string]string{"X-Test-Principal": "root", "X-Test-Admin": "1"}
	rows := []struct {
		name   string
		method string
		path   string
		body   string
		head   map[string]string
		status int
	}{
		{"anon list denied", "GET", "/o/r/api/releases", "", nil, 401},
		{"empty list", "GET", "/o/r/api/releases", "", asReader("bob"), 200},
		{"empty latest", "GET", "/o/r/api/releases/latest", "", asReader("bob"), 404},
		{"unknown release", "GET", "/o/r/api/releases/nope", "", asReader("bob"), 404},
		{"anon create denied", "PUT", "/o/r/api/releases/v1.0.0", `{}`, nil, 401},
		{"unknown tag", "PUT", "/o/r/api/releases/nope", `{}`, asWriter(), 404},
		{"bad JSON", "PUT", "/o/r/api/releases/v1.0.0", `{`, asWriter(), 400},
		{"unknown field", "PUT", "/o/r/api/releases/v1.0.0", `{"bogus":1}`, asWriter(), 400},
		{"create", "PUT", "/o/r/api/releases/v1.0.0", `{"name":"R1"}`, asWriter(), 201},
		{"get", "GET", "/o/r/api/releases/v1.0.0", "", asReader("bob"), 200},
		{"get encoded tag", "GET", "/o/r/api/releases/feat%2Fx", "", asReader("bob"), 404},
		{"create slash tag", "PUT", "/o/r/api/releases/feat%2Fx", `{}`, asWriter(), 201},
		{"get slash tag", "GET", "/o/r/api/releases/feat%2Fx", "", asReader("bob"), 200},
		{"browser lane get", "GET", "/o/r/api-browser/releases/v1.0.0", "", asReader("bob"), 200},
		{"dotgit lane get", "GET", "/o/r.git/api/releases/v1.0.0", "", asReader("bob"), 200},
		{"latest", "GET", "/o/r/api/releases/latest", "", asReader("bob"), 200},
		{"list page", "GET", "/o/r/api/releases?n=1", "", asReader("bob"), 200},
		{"bad n", "GET", "/o/r/api/releases?n=x", "", asReader("bob"), 400},
		{"autodraft needs tag", "GET", "/o/r/api/releases/autodraft", "", asReader("bob"), 400},
		{"autodraft", "GET", "/o/r/api/releases/autodraft?tag=v1.0.0", "", asReader("bob"), 200},
		{"asset upload no sha", "POST", "/o/r/api/releases/v1.0.0/assets/f", "x", asWriter(), 400},
		{"asset missing release", "DELETE", "/o/r/api/releases/nope/assets/f", "", asWriter(), 404},
		{"asset missing name", "DELETE", "/o/r/api/releases/v1.0.0/assets/nope", "", asWriter(), 404},
		{"release delete forbidden", "DELETE", "/o/r/api/releases/v1.0.0", "", asWriter(), 403},
		{"release delete", "DELETE", "/o/r/api/releases/feat%2Fx", "", adminH, 200},
		{"release delete gone", "DELETE", "/o/r/api/releases/feat%2Fx", "", adminH, 404},
		{"method not allowed", "POST", "/o/r/api/releases/v1.0.0", "", asWriter(), 405},
		{"asset method not allowed", "PATCH", "/o/r/api/releases/v1.0.0/assets/f", "", asWriter(), 405},
		{"unknown path", "GET", "/o/r/api/releases/v1.0.0/bogus", "", asReader("bob"), 404},
		{"non releases untouched", "GET", "/o/r/api/refs", "", asReader("bob"), 404},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := do(t, x, row.method, row.path, []byte(row.body), row.head)
			if rec.Code != row.status {
				t.Fatalf("%s %s: got %d want %d (%q)", row.method, row.path, rec.Code, row.status, rec.Body.String())
			}
		})
	}

	// Wire shape: browser_download_url + [] assets.
	rec := do(t, x, "GET", "/o/r/api/releases/v1.0.0", nil, asReader("bob"))
	var wire map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire["tag"] != "v1.0.0" || wire["tag_sha"] != strings.Repeat("a", 40) {
		t.Fatalf("wire: %v", wire)
	}
	if assets, ok := wire["assets"].([]any); !ok || len(assets) != 0 {
		t.Fatalf("assets not []: %v", wire["assets"])
	}
}

func TestReleasesHTTPCache(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	putJSON(t, x, "v1", map[string]any{"name": "R"}, asWriter())

	rec := do(t, x, "GET", "/o/r/api/releases/v1", nil, asReader("bob"))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "stale-while-revalidate") {
		t.Fatalf("class: %q", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	rec2 := do(t, x, "GET", "/o/r/api/releases/v1", nil,
		mergeHeaders(asReader("bob"), map[string]string{"If-None-Match": etag}))
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("304: %d", rec2.Code)
	}
	// If-Match update round-trips the ETag.
	rec3 := putJSON(t, x, "v1", map[string]any{"name": "R2"},
		mergeHeaders(asWriter(), map[string]string{"If-Match": etag}))
	if rec3.Code != 200 {
		t.Fatalf("if-match update: %d %q", rec3.Code, rec3.Body.String())
	}
	rec4 := putJSON(t, x, "v1", map[string]any{"name": "R3"},
		mergeHeaders(asWriter(), map[string]string{"If-Match": etag}))
	if rec4.Code != 409 {
		t.Fatalf("stale if-match: %d", rec4.Code)
	}
}

func TestAssetHTTPFlow(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	putJSON(t, x, "v1", map[string]any{}, asWriter())

	body := []byte("0123456789abcdef")
	sha := shaOf(body)
	// Upload.
	up := do(t, x, "POST", "/o/r/api/releases/v1/assets/tool",
		body, mergeHeaders(asWriter(), map[string]string{"X-Walgit-Asset-Sha256": sha}))
	if up.Code != 201 {
		t.Fatalf("upload: %d %q", up.Code, up.Body.String())
	}
	var ae map[string]any
	if err := json.Unmarshal(up.Body.Bytes(), &ae); err != nil {
		t.Fatal(err)
	}
	if ae["sha256"] != sha || ae["size"] != float64(len(body)) {
		t.Fatalf("entry: %v", ae)
	}
	wantURL := "/o/r/releases/v1/assets/tool"
	if ae["browser_download_url"] != wantURL {
		t.Fatalf("download url: %v", ae["browser_download_url"])
	}
	// Bytes via the repo-subpath route (HandleRepo, as repoDispatch calls it).
	id := git.RepoId{Owner: "o", Name: "r"}
	get := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
	get.Header.Set("X-Test-Principal", "bob")
	grec := httptest.NewRecorder()
	if !x.handler.HandleRepo(grec, get, id, []string{"releases", "v1", "assets", "tool"}) {
		t.Fatal("byte route not claimed")
	}
	res := grec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("bytes: %d", res.StatusCode)
	}
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, body) {
		t.Fatal("bytes differ")
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("static CC: %q", cc)
	}
	if res.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("no accept-ranges")
	}
	etag := res.Header.Get("ETag")
	// 304.
	get2 := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
	get2.Header.Set("X-Test-Principal", "bob")
	get2.Header.Set("If-None-Match", etag)
	grec2 := httptest.NewRecorder()
	x.handler.HandleRepo(grec2, get2, id, []string{"releases", "v1", "assets", "tool"})
	if grec2.Code != 304 {
		t.Fatalf("304: %d", grec2.Code)
	}
	// Range 206.
	get3 := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
	get3.Header.Set("X-Test-Principal", "bob")
	get3.Header.Set("Range", "bytes=0-3")
	grec3 := httptest.NewRecorder()
	x.handler.HandleRepo(grec3, get3, id, []string{"releases", "v1", "assets", "tool"})
	if grec3.Code != 206 || grec3.Body.String() != "0123" {
		t.Fatalf("206: %d %q", grec3.Code, grec3.Body.String())
	}
	if grec3.Header().Get("Content-Range") != "bytes 0-3/16" {
		t.Fatalf("content-range: %q", grec3.Header().Get("Content-Range"))
	}
	// Unsatisfiable → 416.
	get4 := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
	get4.Header.Set("X-Test-Principal", "bob")
	get4.Header.Set("Range", "bytes=99-100")
	grec4 := httptest.NewRecorder()
	x.handler.HandleRepo(grec4, get4, id, []string{"releases", "v1", "assets", "tool"})
	if grec4.Code != 416 {
		t.Fatalf("416: %d", grec4.Code)
	}
	// HEAD: headers, no body.
	head := httptest.NewRequest("HEAD", "/o/r/releases/v1/assets/tool", nil)
	head.Header.Set("X-Test-Principal", "bob")
	hrec := httptest.NewRecorder()
	x.handler.HandleRepo(hrec, head, id, []string{"releases", "v1", "assets", "tool"})
	if hrec.Code != 200 || hrec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d len=%d", hrec.Code, hrec.Body.Len())
	}
	// Wrong shape / method / auth.
	if x.handler.HandleRepo(httptest.NewRecorder(), get, id, []string{"releases", "v1"}) {
		t.Fatal("short sub claimed")
	}
	post := httptest.NewRequest("POST", "/o/r/releases/v1/assets/tool", nil)
	prec := httptest.NewRecorder()
	x.handler.HandleRepo(prec, post, id, []string{"releases", "v1", "assets", "tool"})
	if prec.Code != 405 {
		t.Fatalf("byte POST: %d", prec.Code)
	}
	anon := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
	arec := httptest.NewRecorder()
	x.handler.HandleRepo(arec, anon, id, []string{"releases", "v1", "assets", "tool"})
	if arec.Code != 401 {
		t.Fatalf("anon bytes: %d", arec.Code)
	}
	// Missing asset → 404.
	miss := httptest.NewRequest("GET", "/o/r/releases/v1/assets/nope", nil)
	miss.Header.Set("X-Test-Principal", "bob")
	mrec := httptest.NewRecorder()
	x.handler.HandleRepo(mrec, miss, id, []string{"releases", "v1", "assets", "nope"})
	if mrec.Code != 404 {
		t.Fatalf("missing asset: %d", mrec.Code)
	}
	// Delete via lane; bytes gone after.
	del := do(t, x, "DELETE", "/o/r/api/releases/v1/assets/tool", nil, asWriter())
	if del.Code != 200 {
		t.Fatalf("delete: %d %q", del.Code, del.Body.String())
	}
	gone := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
	gone.Header.Set("X-Test-Principal", "bob")
	gorec := httptest.NewRecorder()
	x.handler.HandleRepo(gorec, gone, id, []string{"releases", "v1", "assets", "tool"})
	if gorec.Code != 404 {
		t.Fatalf("post-delete bytes: %d", gorec.Code)
	}
}

func TestAssetUploadTooLargeHTTP(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	putJSON(t, x, "v1", map[string]any{}, asWriter())
	x.svc.MaxAssetBytes = 4
	rec := do(t, x, "POST", "/o/r/api/releases/v1/assets/big", []byte("12345"),
		mergeHeaders(asWriter(), map[string]string{"X-Walgit-Asset-Sha256": shaOf([]byte("12345"))}))
	if rec.Code != 413 {
		t.Fatalf("413: %d %q", rec.Code, rec.Body.String())
	}
}

func TestParseRangeTable(t *testing.T) {
	rows := []struct {
		spec  string
		size  int64
		start int64
		end   int64
		ok    bool
	}{
		{"bytes=0-3", 16, 0, 3, true},
		{"bytes=4-", 16, 4, 15, true},
		{"bytes=-4", 16, 12, 15, true},
		{"bytes=0-99", 16, 0, 15, true},
		{"bytes=16-20", 16, 0, 0, false},
		{"bytes=5-2", 16, 0, 0, false},
		{"bytes=0-1,3-4", 16, 0, 0, false},
		{"items=0-1", 16, 0, 0, false},
		{"bytes=-0", 16, 0, 0, false},
		{"bytes=abc", 16, 0, 0, false},
		{"", 16, 0, 0, false},
	}
	for _, row := range rows {
		s, e, ok := parseRange(row.spec, row.size)
		if ok != row.ok || (ok && (s != row.start || e != row.end)) {
			t.Fatalf("%q → %d,%d,%v want %d,%d,%v", row.spec, s, e, ok, row.start, row.end, row.ok)
		}
	}
}

func TestPrincipalFallbackAnonymous(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = nil
	rec := do(t, x, "GET", "/o/r/api/releases", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("nil auth: %d", rec.Code)
	}
	if _, aerr := x.handler.principal(httptest.NewRequest("GET", "/", nil)); aerr != nil {
		t.Fatal("nil auth principal should fall back")
	}
	var _ = auth.Anonymous
}
