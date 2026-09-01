package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// mustJSON decodes a 200 JSON body.
func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

// fixture wires the full Mount stack over fakes.
type fixture struct {
	t     *testing.T
	view  *fakeView
	reg   *fakeRegistry
	tasks *fakeTasks
	env   *Env
	mux   *http.ServeMux
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:     t,
		view:  newFakeView(),
		reg:   &fakeRegistry{repos: map[string][]string{"demo": {"hello", "walgit"}}, created: map[string]bool{}},
		tasks: &fakeTasks{records: map[string]TaskRecord{}, streams: map[string]TaskStream{}},
	}
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.AnonymousRead = true
	f.env = &Env{
		Store:    store.NewMemory(),
		Repos:    f.reg,
		Repo:     f.view,
		Tasks:    f.tasks,
		Cfg:      cfg,
		Version:  "test",
		Hostname: "host-a",
	}
	f.env.Ready()
	f.mux = Mount(f.env).(*http.ServeMux)
	return f
}

func (f *fixture) do(method, path string, body io.Reader, header map[string]string, principal *auth.Principal) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, body)
	if principal != nil {
		r = r.WithContext(WithPrincipal(r.Context(), *principal))
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// req is the common form: bearer-authorized full-access principal; the body
// is optional (nil for reads).
func (f *fixture) req(method, path string, body ...io.Reader) *httptest.ResponseRecorder {
	var b io.Reader
	if len(body) > 0 {
		b = body[0]
	}
	p := auth.Principal{Name: "jane", Write: true, Admin: true}
	return f.do(method, path, b, nil, &p)
}

// readPrincipal is a read-only principal (no write, no admin).
func readP() *auth.Principal { p := auth.Principal{Name: "reader"}; return &p }

// --- discovery (§8) -------------------------------------------------------------------

func TestDiscoveryShape(t *testing.T) {
	f := newFixture(t)
	w := f.do("GET", "/api/v1", nil, nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccNoCache {
		t.Fatalf("cache-control = %q", cc)
	}
	var doc struct {
		Version     int    `json:"version"`
		Base        string `json:"base"`
		BrowserBase string `json:"browser_base"`
		SDK         string `json:"sdk"`
		Auth        struct {
			Bearer       bool   `json:"bearer"`
			Setup        string `json:"setup"`
			Browser      string `json:"browser"`
			Authenticate string `json:"authenticate"`
		} `json:"auth"`
		Endpoints []string `json:"endpoints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.Base != "/api/v1" || doc.BrowserBase != "/api/v1" || doc.SDK != "/repos.js" {
		t.Fatalf("discovery header fields: %+v", doc)
	}
	if !doc.Auth.Bearer || doc.Auth.Setup != "/services/setup.json" || doc.Auth.Browser != "/api-browser/v1" || doc.Auth.Authenticate != "/api/v1/authenticate" {
		t.Fatalf("auth block: %+v", doc.Auth)
	}
	want := []string{
		"/api/v1/me",
		"/api/v1/owners",
		"/api/v1/owners/{owner}/repos",
		"/{owner}/{repo}/api",
		"/{owner}/{repo}/api/refs",
		"/{owner}/{repo}/api/refs/{branches|tags}",
		"/{owner}/{repo}/api/resolve/{ref}",
		"/{owner}/{repo}/api/tree/{rev}",
		"/{owner}/{repo}/api/blob/{rev}/{path}",
		"/{owner}/{repo}/api/commits",
		"/{owner}/{repo}/api/commit/{sha}",
		"/{owner}/{repo}/api/policy",
		"/{owner}/{repo}/api/settings",
		"/{owner}/{repo}/api/overview",
		"/{owner}/{repo}/api/ops",
		"/{owner}/{repo}/api/tasks",
	}
	if len(doc.Endpoints) != len(want) {
		t.Fatalf("endpoints = %v, want %v", doc.Endpoints, want)
	}
	for i := range want {
		if doc.Endpoints[i] != want[i] {
			t.Fatalf("endpoints[%d] = %q, want %q (no phantom routes)", i, doc.Endpoints[i], want[i])
		}
	}
	// browser twin answers identically
	if w2 := f.do("GET", "/api-browser/v1", nil, nil, nil); w2.Code != 200 {
		t.Fatalf("browser twin status = %d", w2.Code)
	}
}

// --- me, owners, instance ---------------------------------------------------------------

func TestMe(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = false // 401 when anonymous read is off
	w := f.do("GET", "/api/v1/me", nil, nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", w.Code)
	}
	if h := w.Header().Get("WWW-Authenticate"); h != `Bearer realm="walgit"` {
		t.Fatalf("www-authenticate = %q", h)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("error body must be plain text, got %q", ct)
	}
	p := auth.Principal{Name: "jane", Write: true}
	w = f.do("GET", "/api/v1/me", nil, nil, &p)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
		t.Fatalf("me cache-control = %q", cc)
	}
	var body struct {
		Principal string `json:"principal"`
		Write     bool   `json:"write"`
		Anonymous bool   `json:"anonymous"`
	}
	decodeJSON(t, w, &body)
	if body.Principal != "jane" || !body.Write || body.Anonymous {
		t.Fatalf("me = %+v", body)
	}
	// anonymous read allowed
	f.env.Cfg.Server.Auth.AnonymousRead = true
	w = f.do("GET", "/api/v1/me", nil, nil, nil)
	var anon struct {
		Anonymous bool `json:"anonymous"`
	}
	decodeJSON(t, w, &anon)
	if !anon.Anonymous {
		t.Fatal("anonymous expected")
	}
}

func TestOwnersAndRepos(t *testing.T) {
	f := newFixture(t)
	w := f.do("GET", "/api/v1/owners", nil, nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("owners cache class = %q", cc)
	}
	var names []string
	decodeJSON(t, w, &names)
	if len(names) != 1 || names[0] != "demo" {
		t.Fatalf("owners = %v", names)
	}
	// services twin
	if w := f.do("GET", "/services/api/owners", nil, nil, nil); w.Code != 200 {
		t.Fatalf("services twin status = %d", w.Code)
	}
	// repos
	w = f.do("GET", "/api/v1/owners/demo/repos", nil, nil, nil)
	var repos []string
	decodeJSON(t, w, &repos)
	if len(repos) != 2 || repos[0] != "hello" || repos[1] != "walgit" {
		t.Fatalf("repos = %v", repos)
	}
	// unknown owner → 200 [] (never 404)
	w = f.do("GET", "/api/v1/owners/unknown/repos", nil, nil, nil)
	if w.Code != 200 {
		t.Fatalf("unknown owner status = %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("unknown owner repos = %q, want []", got)
	}
}

func TestInstanceShape(t *testing.T) {
	f := newFixture(t)
	w := f.do("GET", "/services/api/instance", nil, nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
		t.Fatalf("instance cache = %q", cc)
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	for _, k := range []string{"kind", "name", "revision", "instance", "version", "roles", "disk", "cpus", "memory_bytes"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("instance missing %q: %v", k, body)
		}
	}
}

// --- summary + refs ----------------------------------------------------------------------

func seedSummary(f *fixture) {
	f.view.summaries["demo/walgit"] = SummaryData{Head: new(Ref), Branches: 2, Tags: 1}
	*f.view.summaries["demo/walgit"].Head = Ref{Name: "refs/heads/main", SHA: fakeSHA}
	f.view.heads["demo/walgit"] = Ref{Name: "refs/heads/main", SHA: fakeSHA}
	f.view.resolves["demo/walgit/"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Kind: "branch", Revision: 7}
	f.view.resolves["demo/walgit/main"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Kind: "branch", Revision: 7}
	f.view.resolves["demo/walgit/v1"] = Resolution{Ref: "refs/tags/v1", SHA: fakeSHA2, Kind: "tag", Revision: 7}
	f.view.resolves["demo/walgit/"+fakeSHA] = Resolution{Ref: "", SHA: fakeSHA, Kind: "commit", Revision: 7}
}

func TestRepoSummary(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("summary cache = %q", cc)
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("etag = %q", etag)
	}
	var body struct {
		Owner    string `json:"owner"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Head     *struct {
			Name string `json:"name"`
			SHA  string `json:"sha"`
		} `json:"head"`
		Branches int    `json:"branches"`
		Tags     int    `json:"tags"`
		CloneURL string `json:"clone_url"`
		HTMLURL  string `json:"html_url"`
		APIURL   string `json:"api_url"`
	}
	decodeJSON(t, w, &body)
	if body.Owner != "demo" || body.Name != "walgit" || body.FullName != "demo/walgit" {
		t.Fatalf("identity fields: %+v", body)
	}
	if body.Head == nil || body.Head.SHA != fakeSHA || body.Head.Name != "refs/heads/main" {
		t.Fatalf("head: %+v", body.Head)
	}
	if !strings.HasSuffix(body.CloneURL, "/demo/walgit.git") {
		t.Fatalf("clone_url = %q", body.CloneURL)
	}
	if !strings.HasSuffix(body.APIURL, "/demo/walgit/api") {
		t.Fatalf("api_url = %q", body.APIURL)
	}
	// 304 path
	w = f.req("GET", "/demo/walgit/api")
	w2 := f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": `"` + fakeSHA + `"`}, readP())
	if w2.Code != http.StatusNotModified {
		t.Fatalf("if-none-match status = %d", w2.Code)
	}
	_ = w
	// head null → JSON null, no ETag
	f.view.summaries["demo/empty"] = SummaryData{}
	w = f.req("GET", "/demo/empty/api")
	if w.Code != 200 {
		t.Fatalf("empty repo status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"head":null`) {
		t.Fatalf("head must serialize as null: %s", w.Body.String())
	}
}

func TestRefsHeadAndList(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	w := f.req("GET", "/demo/walgit/api/refs")
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("refs cache = %q", cc)
	}
	var head struct {
		Head *struct{ Name, SHA string } `json:"head"`
	}
	decodeJSON(t, w, &head)
	if head.Head == nil || head.Head.Name != "refs/heads/main" || head.Head.SHA != fakeSHA {
		t.Fatalf("head = %+v", head.Head)
	}
	// head null (unborn HEAD)
	delete(f.view.heads, "demo/walgit")
	w = f.req("GET", "/demo/walgit/api/refs")
	if !strings.Contains(w.Body.String(), `"head":null`) {
		t.Fatalf("refs head null: %s", w.Body.String())
	}

	// list page
	f.view.lists["demo/walgit/heads"] = []Ref{
		{Name: "refs/heads/feature/a", SHA: fakeSHA2},
		{Name: "refs/heads/main", SHA: fakeSHA},
		{Name: "refs/heads/release/1", SHA: fakeTag},
		{Name: "refs/heads/release/2", SHA: fakeSHA2},
	}
	w = f.req("GET", "/demo/walgit/api/refs/branches?prefix=release/&n=1")
	var page struct {
		Refs []struct{ Name, SHA string } `json:"refs"`
		More bool                         `json:"more"`
	}
	decodeJSON(t, w, &page)
	if len(page.Refs) != 1 || page.Refs[0].Name != "refs/heads/release/1" || !page.More {
		t.Fatalf("page = %+v", page)
	}
	// q substring + default n=100
	w = f.req("GET", "/demo/walgit/api/refs/branches?q=FEATURE")
	decodeJSON(t, w, &page)
	if len(page.Refs) != 1 || page.Refs[0].Name != "refs/heads/feature/a" || page.More {
		t.Fatalf("q page = %+v", page)
	}
	// invalid n
	if w := f.req("GET", "/demo/walgit/api/refs/branches?n=0"); w.Code != http.StatusBadRequest {
		t.Fatalf("n=0 status = %d", w.Code)
	}
	// unknown namespace → 404
	if w := f.req("GET", "/demo/walgit/api/refs/other"); w.Code != 404 {
		t.Fatalf("unknown ns status = %d", w.Code)
	}
}

func TestRefsListSSEDialect(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.lists["demo/walgit/heads"] = []Ref{
		{Name: "refs/heads/main", SHA: fakeSHA},
		{Name: "refs/heads/other", SHA: fakeSHA2},
	}
	w := f.req("GET", "/demo/walgit/api/refs/branches", nil)
	w = f.do("GET", "/demo/walgit/api/refs/branches", nil,
		map[string]string{"Accept": "text/event-stream"}, readP())
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	want := "event: ref\ndata: {\"name\":\"refs/heads/main\",\"sha\":\"" + fakeSHA + "\"}\n\n" +
		"event: ref\ndata: {\"name\":\"refs/heads/other\",\"sha\":\"" + fakeSHA2 + "\"}\n\n" +
		"event: done\ndata: {\"more\":false}\n\n"
	if body != want {
		t.Fatalf("dialect bytes:\n got %q\nwant %q", body, want)
	}
	if strings.Contains(body, ": walgit") || strings.Contains(body, "keepalive") {
		t.Fatal("the §7 dialect must not carry the envelope opener or keepalives")
	}
	if w.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("X-Accel-Buffering: no required")
	}
}

// --- resolve / tree / blob ------------------------------------------------------------------

func seedTree(f *fixture) {
	seedSummary(f)
	f.view.resolves["demo/walgit/main/src"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Path: "src", Kind: "branch", Revision: 7}
	f.view.resolves["demo/walgit/main"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Kind: "branch", Revision: 7}
	f.view.resolves["demo/walgit/"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Kind: "branch", Revision: 7}
	f.view.trees["demo/walgit|"+fakeSHA+"|"] = TreeResult{
		Entries: []TreeEntry{
			{Name: "zeta.txt", Type: "blob", Mode: "100644", Size: 3, SHA: fakeSHA2},
			{Name: "src", Type: "tree", Mode: "040000", Size: -1, SHA: fakeTag},
		},
	}
	commit := Commit{SHA: fakeTag, Parents: []string{}, Subject: "s", Trailers: []Trailer{}}
	f.view.trees["demo/walgit|"+fakeSHA+"|src"] = TreeResult{
		Entries: []TreeEntry{{Name: "a.txt", Type: "blob", Mode: "100644", Size: 5, SHA: fakeSHA2}},
		Commit:  &commit,
		Readme:  &Readme{Name: "README.md", Contents: "# hello"},
	}
	f.view.blobs["demo/walgit|"+fakeSHA+"|src/a.txt"] = BlobResult{Size: 5, Contents: []byte("hello")}
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|src/a.txt"] = []byte("hello")
}

func TestResolve(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.resolves["demo/walgit/"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Path: "", Kind: "branch", Revision: 7}
	f.view.resolves["demo/walgit/v1/src"] = Resolution{Ref: "refs/tags/v1", SHA: fakeTag, Path: "src", Kind: "tag", Revision: 7}
	f.view.resolves["demo/walgit/main/src"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Path: "src", Kind: "branch", Revision: 7}
	w := f.req("GET", "/demo/walgit/api/resolve/main/src")
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("resolve cache = %q", cc)
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("etag = %q", etag)
	}
	var body struct {
		Ref, SHA, Path, Kind string
	}
	decodeJSON(t, w, &body)
	if body.Ref != "refs/heads/main" || body.SHA != fakeSHA || body.Path != "src" || body.Kind != "branch" {
		t.Fatalf("resolve = %+v", body)
	}
	// default branch (no rest)
	w = f.req("GET", "/demo/walgit/api/resolve")
	decodeJSON(t, w, &body)
	if body.Ref != "refs/heads/main" || body.Path != "" {
		t.Fatalf("default resolve: %+v", body)
	}
	// 404 unknown revision, plain text
	w = f.req("GET", "/demo/walgit/api/resolve/nope")
	if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("resolve 404: %d %q", w.Code, w.Body.String())
	}
}

func TestTreeCacheClasses(t *testing.T) {
	f := newFixture(t)
	seedTree(f)
	// name-addressed: SWR + ETag
	w := f.req("GET", "/demo/walgit/api/tree/main/src")
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("named tree cache = %q", cc)
	}
	var body struct {
		Ref, SHA, Path string
		Entries        []struct {
			Name, Type, Mode string
			Size             int64
			SHA              string
		}
		Commit *struct{ SHA string } `json:"commit"`
		Readme *struct{ Name, Contents string }
	}
	decodeJSON(t, w, &body)
	if body.SHA != fakeSHA || body.Path != "src" || body.Ref != "refs/heads/main" {
		t.Fatalf("tree identity: %+v", body)
	}
	if len(body.Entries) != 1 || body.Entries[0].Name != "a.txt" {
		t.Fatalf("entries: %+v", body.Entries)
	}
	if body.Commit == nil || body.Commit.SHA != fakeTag {
		t.Fatalf("commit present when path non-empty: %+v", body.Commit)
	}
	if body.Readme == nil || body.Readme.Name != "README.md" {
		t.Fatalf("readme: %+v", body.Readme)
	}
	// sha-addressed: immutable
	f.view.trees["demo/walgit|"+fakeSHA+"|src"] = f.view.trees["demo/walgit|"+fakeSHA+"|src"]
	w = f.req("GET", "/demo/walgit/api/tree/"+fakeSHA+"/src")
	if cc := w.Header().Get("Cache-Control"); cc != ccImmutable {
		t.Fatalf("sha tree cache = %q", cc)
	}
	// 304 on If-None-Match (sha-addressed)
	w2 := f.do("GET", "/demo/walgit/api/tree/"+fakeSHA+"/src", nil,
		map[string]string{"If-None-Match": `"` + fakeSHA + `"`}, readP())
	if w2.Code != http.StatusNotModified {
		t.Fatalf("sha 304 status = %d", w2.Code)
	}
	// root path: no commit field
	w = f.req("GET", "/demo/walgit/api/tree/main")
	var rootBody struct {
		Commit *struct{ SHA string } `json:"commit"`
	}
	decodeJSON(t, w, &rootBody)
	if rootBody.Commit != nil {
		t.Fatal("commit must be absent at path=\"\"")
	}
	// 404 not a tree
	if w := f.req("GET", "/demo/walgit/api/tree/main/nope"); w.Code != 404 {
		t.Fatalf("missing tree status = %d", w.Code)
	}
}

func TestBlobShapes(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.blobs["demo/walgit|"+fakeSHA+"|src/a.txt"] = BlobResult{Size: 5, Contents: []byte("hello")}
	f.view.blobs["demo/walgit|"+fakeSHA+"|src/b.bin"] = BlobResult{Size: 3, Binary: true, Contents: []byte{0, 1, 2}}
	big := strings.Repeat("x", 2<<20+1)
	f.view.blobs["demo/walgit|"+fakeSHA+"|src/c.big"] = BlobResult{Size: int64(len(big)), TooLarge: true}

	decodeBlob := func(rec *httptest.ResponseRecorder) (out struct {
		Ref, SHA, Path, Name string
		Size                 int64
		Contents             string `json:"contents,omitempty"`
		Binary               bool   `json:"binary,omitempty"`
		TooLarge             bool   `json:"too_large,omitempty"`
	}) {
		decodeJSON(t, rec, &out)
		return out
	}
	w := f.req("GET", "/demo/walgit/api/blob/main/src/a.txt")
	body := decodeBlob(w)
	if body.Name != "a.txt" || body.Size != 5 || body.Contents != "hello" || body.Binary || body.TooLarge {
		t.Fatalf("blob = %+v", body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("named blob cache = %q", cc)
	}
	body = decodeBlob(f.req("GET", "/demo/walgit/api/blob/main/src/b.bin"))
	if !body.Binary || body.Contents != "" {
		t.Fatalf("binary blob: %+v", body)
	}
	body = decodeBlob(f.req("GET", "/demo/walgit/api/blob/main/src/c.big"))
	if !body.TooLarge || body.Contents != "" {
		t.Fatalf("too_large blob: %+v", body)
	}
	// raw: full bytes, text/plain
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|src/c.big"] = []byte(big)
	w = f.req("GET", "/demo/walgit/api/blob/main/src/c.big?raw")
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("raw content-type = %q", ct)
	}
	if w.Body.String() != big {
		t.Fatalf("raw must emit the uncapped blob bytes (len=%d)", len(w.Body.Bytes()))
	}
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|src/a.txt"] = []byte("hello")
	// raw for a sha-addressed blob stays immutable
	w = f.req("GET", "/demo/walgit/api/blob/"+fakeSHA+"/src/a.txt?raw")
	if cc := w.Header().Get("Cache-Control"); cc != ccImmutable {
		t.Fatalf("sha raw blob cache = %q", cc)
	}
	if w.Body.String() != "hello" {
		t.Fatalf("sha raw body = %q", w.Body.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- commits / commit detail -----------------------------------------------------------

func TestCommitsShape(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	c1 := Commit{SHA: fakeTag, Parents: []string{}, Subject: "s1", Trailers: []Trailer{}}
	c2 := Commit{SHA: fakeSHA2, Parents: []string{fakeTag}, Subject: "s2", Trailers: []Trailer{}}
	f.view.commitPg["demo/walgit|cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6e7||0|1"] = CommitPage{Commits: []Commit{c1}, More: true}
	f.view.commitPg["demo/walgit|cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6e7||0|35"] = CommitPage{Commits: []Commit{c1, c2}}
	f.view.commitPg["demo/walgit|cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6e7||2|35"] = CommitPage{Commits: []Commit{c2}}

	w := f.req("GET", "/demo/walgit/api/commits?ref=main&n=1")
	var body struct {
		Ref, SHA string
		Commits  []struct {
			SHA      string
			Parents  []string
			Trailers []struct{ K, V string }
		}
		More bool `json:"more"`
	}
	decodeJSON(t, w, &body)
	if body.Ref != "refs/heads/main" || body.SHA != fakeSHA || !body.More || len(body.Commits) != 1 {
		t.Fatalf("commits = %+v", body)
	}
	if body.Commits[0].Parents == nil {
		t.Fatal("parents must be [] not null")
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("named commits cache = %q", cc)
	}
	// skip
	w = f.req("GET", "/demo/walgit/api/commits?ref=main&skip=2")
	decodeJSON(t, w, &body)
	if len(body.Commits) != 1 || body.Commits[0].SHA != fakeSHA2 {
		t.Fatalf("skip: %+v", body.Commits)
	}
	// full-sha ref → immutable
	f.view.commitPg["demo/walgit|"+fakeSHA+"||0|35"] = CommitPage{Commits: []Commit{c1}}
	w = f.req("GET", "/demo/walgit/api/commits?ref="+fakeSHA)
	if cc := w.Header().Get("Cache-Control"); cc != ccImmutable {
		t.Fatalf("sha commits cache = %q", cc)
	}
	// invalid n
	if w := f.req("GET", "/demo/walgit/api/commits?n=abc"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid n status = %d", w.Code)
	}
}

func TestCommitDetailShape(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	full := Commit{SHA: fakeSHA, Parents: []string{fakeSHA2}, Subject: "sub", Trailers: []Trailer{}}
	f.view.commits["demo/walgit|"+fakeSHA] = CommitDetail{
		Commit: full,
		Stats: []Stat{
			{Path: "a.txt", Additions: 3, Deletions: 1},
			{Path: "bin.dat", Additions: -1, Deletions: -1},
		},
		Patch: "diff --git a/a.txt b/a.txt\n",
	}
	// short sha resolves to the full-sha render key (§9.8)
	f.view.resolves["demo/walgit/cb38da1"] = Resolution{Ref: "", SHA: fakeSHA, Kind: "commit", Revision: 7}

	w := f.req("GET", "/demo/walgit/api/commit/"+fakeSHA)
	if cc := w.Header().Get("Cache-Control"); cc != ccImmutable {
		t.Fatalf("full sha commit cache = %q", cc)
	}
	var body struct {
		Commit struct {
			SHA     string
			Parents []string
		}
		Stats []struct {
			Path                 string
			Additions, Deletions int64
		}
		Patch string `json:"patch"`
	}
	decodeJSON(t, w, &body)
	if body.Commit.SHA != fakeSHA || len(body.Stats) != 2 || body.Stats[1].Additions != -1 {
		t.Fatalf("detail = %+v", body)
	}
	if body.Patch == "" {
		t.Fatal("patch must pass through verbatim")
	}
	// short sha: SWR + ETag full sha; same render key reused
	w = f.req("GET", "/demo/walgit/api/commit/cb38da1")
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("short sha cache = %q", cc)
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("short sha etag = %q", etag)
	}
	if len(body.Stats) != 2 {
		t.Fatalf("short sha must render the same detail: %+v", body)
	}
}

// --- policy ------------------------------------------------------------------------------

func TestPolicyEndpoints(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	// GET missing → allow-all
	w := f.req("GET", "/demo/walgit/api/policy")
	if got := strings.TrimSpace(w.Body.String()); got != allowAllPolicy {
		t.Fatalf("missing policy = %q", got)
	}
	// PUT invalid → 400 plain text, nothing stored
	w = f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(`{"version":1,"rules":[{"name":"x","effect":{"bogus":{}}}]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy status = %d body=%s", w.Code, w.Body.String())
	}
	// PUT requires admin
	w = f.do("PUT", "/demo/walgit/api/policy", strings.NewReader(`{}`), nil, readP())
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT status = %d", w.Code)
	}
	// PUT valid (CAS write to the store)
	good := `{"version":1,"groups":[],"rules":[{"name":"protect-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"]}}}]}` + "\n"
	w = f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(good))
	if w.Code != 200 {
		t.Fatalf("valid PUT status = %d body=%s", w.Code, w.Body.String())
	}
	// GET now returns the stored doc
	w = f.req("GET", "/demo/walgit/api/policy")
	if strings.TrimSpace(w.Body.String()) != strings.TrimSpace(good) {
		t.Fatalf("stored policy = %q", w.Body.String())
	}
	// validate (stored)
	w = f.req("POST", "/demo/walgit/api/policy/validate", strings.NewReader(""))
	var v struct {
		OK      bool     `json:"ok"`
		Errors  []string `json:"errors"`
		Rules   int      `json:"rules"`
		Groups  int      `json:"groups"`
		Protect []struct {
			Rule   string   `json:"rule"`
			Refs   []string `json:"refs"`
			Ops    []string `json:"ops"`
			Bypass []string `json:"bypass"`
		} `json:"protect"`
	}
	decodeJSON(t, w, &v)
	if !v.OK || v.Rules != 1 || len(v.Protect) != 1 || v.Protect[0].Rule != "protect-main" {
		t.Fatalf("validate = %+v", v)
	}
	// DELETE → back to allow-all
	w = f.req("DELETE", "/demo/walgit/api/policy")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", w.Code)
	}
	w = f.req("GET", "/demo/walgit/api/policy")
	if strings.TrimSpace(w.Body.String()) != allowAllPolicy {
		t.Fatalf("after delete = %q", w.Body.String())
	}
}

func TestPolicyDryRun(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	policyDoc := `{"version":1,"rules":[{"name":"no-force-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"]}}}]}` + "\n"
	f.view.pushes["demo/walgit"] = []PushRecord{
		{Seq: 12, At: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC), Principal: "jane", Atomic: true,
			Refs: []PushRef{{Name: "refs/heads/main", Old: fakeSHA2, New: fakeSHA, Force: false}}},
		{Seq: 13, At: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), Principal: "bob", Atomic: false,
			Refs: []PushRef{{Name: "refs/heads/main", Old: fakeSHA, New: fakeSHA2, Force: true}}},
	}
	w := f.do("POST", "/demo/walgit/api/policy/dry-run?last=10", strings.NewReader(policyDoc), nil, readP())
	var out struct {
		Pushes, Allowed, Denied int
		Results                 []struct {
			Seq       uint64 `json:"seq"`
			Principal string `json:"principal"`
			Refs      []struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				Reason string `json:"reason"`
				Force  bool   `json:"force"`
			} `json:"refs"`
		} `json:"results"`
	}
	decodeJSON(t, w, &out)
	if out.Pushes != 2 || out.Allowed != 1 || out.Denied != 1 {
		t.Fatalf("dry-run totals: %+v", out)
	}
	last := out.Results[1].Refs[0]
	if last.OK || last.Reason != "rejected by rule 'no-force-main'" || !last.Force {
		t.Fatalf("denied ref = %+v", last)
	}
	if !out.Results[0].Refs[0].OK || out.Results[0].Refs[0].Reason != "" {
		t.Fatalf("allowed ref = %+v", out.Results[0].Refs[0])
	}
}

// --- settings ----------------------------------------------------------------------------

func TestSettingsLifecycle(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	// revision 0 = none
	w := f.req("GET", "/demo/walgit/api/settings")
	var doc struct {
		Revision  uint64 `json:"revision"`
		Author    string `json:"author"`
		UpdatedAt string `json:"updated_at"`
		Message   string `json:"message"`
		TOML      string `json:"toml"`
	}
	decodeJSON(t, w, &doc)
	if doc.Revision != 0 || doc.UpdatedAt != "" {
		t.Fatalf("empty settings = %+v", doc)
	}
	// PUT: too large → 413
	big := strings.Repeat("x", 16<<10+1)
	if w := f.req("PUT", "/demo/walgit/api/settings?message=no", strings.NewReader(big)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d", w.Code)
	}
	// PUT: host-only section refused → 400
	if w := f.req("PUT", "/demo/walgit/api/settings", strings.NewReader("[server]\nlisten=\"1.2.3.4:9\"\n")); w.Code != http.StatusBadRequest {
		t.Fatalf("host-only section status = %d body=%s", w.Code, w.Body.String())
	}
	// PUT valid
	valid := "[bundles]\nmain_only = false\n"
	w = f.req("PUT", "/demo/walgit/api/settings?message=set", strings.NewReader(valid))
	var rev struct {
		Revision uint64 `json:"revision"`
	}
	if w.Code != 200 {
		t.Fatalf("valid PUT status = %d body=%s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &rev)
	if rev.Revision != 4 {
		t.Fatalf("revision = %d", rev.Revision)
	}
	// GET now shows the published doc
	w = f.req("GET", "/demo/walgit/api/settings")
	decodeJSON(t, w, &doc)
	if doc.Revision != 3 || doc.Author != "jane" || doc.TOML != valid {
		t.Fatalf("published settings: %+v", doc)
	}
	// DELETE publishes empty
	w = f.req("DELETE", "/demo/walgit/api/settings")
	if w.Code != 200 {
		t.Fatalf("DELETE status = %d", w.Code)
	}
	decodeJSON(t, w, &rev)
	if rev.Revision != 4 {
		t.Fatalf("delete revision = %d", rev.Revision)
	}
}

func TestSettingsEffectiveAndDescribe(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.published["demo/walgit"] = []byte("[bundles]\nmain_only = false\n\n[[bundles.strategy]]\nname = \"weekly\"\nkind = \"full\"\nschedule = \"0 0 23 * * Sun\"\n")
	// effective TOML
	w := f.req("GET", "/demo/walgit/api/settings/effective")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/toml" {
		t.Fatalf("content-type = %q", ct)
	}
	tomlBody := w.Body.String()
	if !strings.Contains(tomlBody, "[bundles]") || !strings.Contains(tomlBody, "main_only = false") {
		t.Fatalf("effective toml = %q", tomlBody)
	}
	if strings.Contains(tomlBody, "token_env") {
		t.Fatalf("token_env must never appear: %q", tomlBody)
	}
	// describe shape
	f.view.overviews["demo/walgit"] = OverviewData{
		Bundles: []BundleInfo{{SHA: fakeSHA, Strategy: "weekly", Kind: "full", Tips: []string{}}},
	}
	f.view.headSeq["demo/walgit"] = 118
	w = f.req("GET", "/demo/walgit/api/settings/describe")
	var d struct {
		Settings   map[string]any `json:"settings"`
		Sections   []string       `json:"sections"`
		Strategies []struct {
			Name, Kind, Schedule, ScheduleHuman, Next string
		} `json:"strategies"`
		Bundles     []map[string]any `json:"bundles"`
		Maintenance struct {
			Checkpoints bool `json:"checkpoints"`
			ThisHost    struct {
				Name      string   `json:"name"`
				Serves    bool     `json:"serves"`
				Maintains bool     `json:"maintains"`
				Roles     []string `json:"roles"`
			} `json:"this_host"`
		} `json:"maintenance"`
		Upstream struct {
			TokenEnv bool     `json:"token_env"`
			Follow   []string `json:"follow"`
		} `json:"upstream"`
		Fields []struct {
			Key       string `json:"key"`
			Value     any    `json:"value"`
			HostValue any    `json:"host_value"`
			Source    string `json:"source"`
		} `json:"fields"`
		HeadSeq uint64 `json:"head_seq"`
	}
	decodeJSON(t, w, &d)
	if got := d.Sections; len(got) != 4 {
		t.Fatalf("sections = %v", got)
	}
	if len(d.Strategies) == 0 || d.Strategies[0].Name != "weekly" || d.Strategies[0].Next == "" {
		t.Fatalf("strategies = %+v", d.Strategies)
	}
	if d.Maintenance.ThisHost.Name != "host-a" || !d.Maintenance.ThisHost.Serves || !d.Maintenance.ThisHost.Maintains {
		t.Fatalf("this_host = %+v", d.Maintenance.ThisHost)
	}
	if d.Upstream.TokenEnv {
		t.Fatal("token_env must be bool presence")
	}
	keys := map[string]string{}
	for _, f := range d.Fields {
		keys[f.Key] = f.Source
	}
	if keys["bundles.main_only"] != "setting" {
		t.Fatalf("bundles.main_only override missing from fields: %+v", d.Fields)
	}
	if keys["bundles.min_commits"] != "setting" {
		t.Fatalf("bundles.min_commits override missing from fields: %+v", d.Fields)
	}
	if len(d.Fields) != len(keys) {
		t.Fatalf("duplicate keys in fields: %+v", d.Fields)
	}
	if d.HeadSeq != 118 {
		t.Fatalf("head_seq = %d", d.HeadSeq)
	}
	// history
	f.view.history["demo/walgit"] = SettingsHistory{MinSeq: 3, Entries: []SettingsEntry{{Seq: 5, Revision: 2, Author: "jane", At: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}}}
	w = f.req("GET", "/demo/walgit/api/settings/history")
	var hist struct {
		MinSeq  uint64 `json:"min_seq"`
		Entries []struct {
			Seq, Revision uint64
		} `json:"entries"`
	}
	decodeJSON(t, w, &hist)
	if hist.MinSeq != 3 || len(hist.Entries) != 1 || hist.Entries[0].Seq != 5 {
		t.Fatalf("history = %+v", hist)
	}
}

// --- overview / ops / tasks ----------------------------------------------------------------

func TestOverviewNoStore(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.overviews["demo/walgit"] = OverviewData{
		Health:   Health{Status: "ok", Issues: []string{}, Suggestions: []Suggestion{}},
		Manifest: ManifestInfo{Version: 118, NextSeq: 42, MinSeq: 3, Segments: []string{}},
		Bundles:  []BundleInfo{},
	}
	w := f.req("GET", "/demo/walgit/api/overview")
	if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
		t.Fatalf("overview cache = %q", cc)
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	for _, k := range []string{"repo", "clone_url", "hostname", "health", "manifest", "local", "packs", "bundles", "bundle_plan", "compactions", "node"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("overview missing %q", k)
		}
	}
	if body["repo"] != "demo/walgit" {
		t.Fatalf("repo = %v", body["repo"])
	}
}

func TestOpsListAndStart(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	w := f.req("GET", "/demo/walgit/api/ops")
	if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
		t.Fatalf("ops cache = %q", cc)
	}
	var body struct {
		Available []struct {
			Op     string `json:"op"`
			Params []struct {
				Name   string   `json:"name"`
				Values []string `json:"values,omitempty"`
			} `json:"params"`
		} `json:"available"`
		Recent           []TaskRecord        `json:"recent"`
		BundleStrategies []map[string]string `json:"bundle_strategies"`
	}
	decodeJSON(t, w, &body)
	ops := map[string]bool{}
	for _, a := range body.Available {
		ops[a.Op] = true
	}
	for _, want := range []string{"fsck", "repair", "follow", "rev-index", "compact", "bundle", "checkpoint", "sync", "rematerialize"} {
		if !ops[want] {
			t.Fatalf("op %q missing: %v", want, ops)
		}
	}
	if len(body.BundleStrategies) == 0 || body.BundleStrategies[0]["name"] != "weekly" {
		t.Fatalf("bundle_strategies = %+v", body.BundleStrategies)
	}
}

func TestOpStartSSEAttach(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	ok := true
	rec := TaskRecord{ID: "t-1", Kind: "compact", Repo: "demo/walgit", Hostname: "host-a", Summary: "compacting", LogTail: []string{}}
	done := make(chan TaskDone, 1)
	updates := make(chan Progress, 8)
	f.tasks.streams["op:compact"] = TaskStream{
		Record:  rec,
		Replay:  []Progress{{Kind: "notice", Text: "starting"}},
		Updates: updates,
		Done:    done,
	}
	go func() {
		updates <- Progress{Kind: "progress", Label: "packs", Done: 1, Total: new(uint64(2)), Unit: "packs"}
		updates <- Progress{Kind: "notice", Text: "done"}
		close(updates)
		done <- TaskDone{Record: rec, Value: "ok", Err: nil}
		done <- TaskDone{Record: rec, Value: "ok", Err: nil} // extra must be ignored
	}()
	w := f.req("POST", "/demo/walgit/api/ops/compact")
	body := w.Body.String()
	if !strings.HasPrefix(body, ": walgit\n\n") {
		t.Fatalf("missing opener: %q", body[:min(30, len(body))])
	}
	want := ": walgit\n\n" +
		"event: task\ndata: {\"id\":\"t-1\",\"kind\":\"compact\",\"repo\":\"demo/walgit\",\"hostname\":\"host-a\",\"started\":\"\",\"elapsed_ms\":0,\"summary\":\"compacting\",\"log_tail\":[]}\n\n" +
		"event: notice\ndata: {\"text\":\"starting\"}\n\n" +
		"event: progress\ndata: {\"label\":\"packs\",\"done\":1,\"total\":2,\"unit\":\"packs\"}\n\n" +
		"event: notice\ndata: {\"text\":\"done\"}\n\n" +
		"event: result\ndata: {\"task\":{\"id\":\"t-1\",\"kind\":\"compact\",\"repo\":\"demo/walgit\",\"hostname\":\"host-a\",\"started\":\"\",\"elapsed_ms\":0,\"summary\":\"compacting\",\"log_tail\":[]},\"value\":\"ok\"}\n\n"
	if body != want {
		t.Fatalf("SSE bytes:\n got %q\nwant %q", body, want)
	}
	_ = ok
	// unknown op → 404
	if w := f.req("POST", "/demo/walgit/api/ops/bogus"); w.Code != 404 {
		t.Fatalf("unknown op status = %d", w.Code)
	}
	// invalid param
	if w := f.req("POST", "/demo/walgit/api/ops/compact?force=2"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid param status = %d", w.Code)
	}
	// missing required param
	if w := f.req("POST", "/demo/walgit/api/ops/bundle"); w.Code != http.StatusBadRequest {
		t.Fatalf("missing param status = %d", w.Code)
	}
}

func TestTasksEndpoints(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	ok := false
	f.tasks.recent = []TaskRecord{{ID: "t-9", Kind: "fsck", Repo: "demo/walgit", Hostname: "host-a", Summary: "clean", OK: new(ok), LogTail: []string{}}}
	f.tasks.records["t-9"] = f.tasks.recent[0]

	w := f.req("GET", "/demo/walgit/api/tasks")
	var list struct {
		Hostname string       `json:"hostname"`
		Running  []TaskRecord `json:"running"`
		Recent   []TaskRecord `json:"recent"`
	}
	decodeJSON(t, w, &list)
	if list.Hostname != "host-a" || len(list.Recent) != 1 || list.Running == nil {
		t.Fatalf("tasks = %+v", list)
	}
	if w := f.req("GET", "/demo/walgit/api/tasks/t-9"); w.Code != 200 {
		t.Fatalf("task get status = %d", w.Code)
	}
	if w := f.req("GET", "/demo/walgit/api/tasks/nope"); w.Code != 404 {
		t.Fatalf("unknown task status = %d", w.Code)
	}
	// attach dialect: one task packet, replay, terminal
	done := make(chan TaskDone, 1)
	updates := make(chan Progress, 2)
	f.tasks.streams["t-9"] = TaskStream{Record: f.tasks.records["t-9"], Updates: updates, Done: done}
	go func() {
		updates <- Progress{Kind: "notice", Text: "hi"}
		close(updates)
		done <- TaskDone{Record: f.tasks.records["t-9"], Value: "v"}
	}()
	w = f.do("GET", "/demo/walgit/api/tasks/t-9", nil, map[string]string{"Accept": "text/event-stream"}, readP())
	body := w.Body.String()
	if !strings.Contains(body, "event: task\ndata: {\"id\":\"t-9\"") {
		t.Fatalf("attach task packet: %q", body)
	}
	if !strings.Contains(body, "event: result\ndata: {\"task\":{\"id\":\"t-9\"") || !strings.Contains(body, "\"value\":\"v\"") {
		t.Fatalf("attach terminal: %q", body)
	}
}

// --- routing (§3.1/§3.2: lanes, .git, per-segment decoding, 404/405) ----------------------

func TestRoutingConventions(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)

	// .git suffix accepted and stripped everywhere
	w := f.req("GET", "/demo/walgit.git/api")
	if w.Code != 200 {
		t.Fatalf("with .git status = %d body=%s", w.Code, w.Body.String())
	}
	// api-browser lane answers identically
	if w := f.req("GET", "/demo/walgit/api-browser"); w.Code != 200 {
		t.Fatalf("browser lane status = %d", w.Code)
	}
	// lane root without trailing slash
	if w := f.req("GET", "/demo/walgit/api"); w.Code != 200 {
		t.Fatalf("lane root status = %d", w.Code)
	}
	// unknown repo → 404 plain text
	w = f.req("GET", "/missing/walgit/api")
	if w.Code != 404 {
		t.Fatalf("unknown repo status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("404 content-type = %q", ct)
	}
	// bad repo id → 404
	if w := f.req("GET", "/.bad/repo/api"); w.Code != 404 {
		t.Fatalf("bad repo id status = %d", w.Code)
	}
	// method mismatch → 405 with Allow
	w = f.req("POST", "/demo/walgit/api/refs")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("405 status = %d", w.Code)
	}
	if w.Header().Get("Allow") != "GET" {
		t.Fatalf("allow = %q", w.Header().Get("Allow"))
	}
	// non-repo junk → 404
	if w := f.req("GET", "/api/v1/definitely-not-a-route"); w.Code != 404 {
		t.Fatalf("junk status = %d", w.Code)
	}
	// per-segment decoding: one segment "feat/ure x" (the encoded slash
	// must NOT split the segment into a path separator)
	f.view.trees["demo/walgit|"+fakeSHA+"|feat/ure x"] = TreeResult{Entries: []TreeEntry{}}
	w = f.req("GET", "/demo/walgit/api/tree/main/feat%2Fure%20x")
	if w.Code != 200 {
		t.Fatalf("per-segment decode status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Path string `json:"path"`
	}
	decodeJSON(t, w, &body)
	if body.Path != "feat/ure x" {
		t.Fatalf("decoded path = %q", body.Path)
	}
	// lane routing: repo root PUT/DELETE
	w = f.req("PUT", "/demo/walgit/api")
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	if w := f.req("PUT", "/demo/walgit/api"); w.Code != http.StatusConflict {
		t.Fatalf("recreate status = %d", w.Code)
	}
	if w := f.do("PUT", "/demo/walgit/api", nil, nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("non-write create status = %d", w.Code)
	}
	w = f.req("DELETE", "/demo/walgit/api")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}
}

func TestAuthGates(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	// write route with a read-only principal → 403
	if w := f.do("POST", "/demo/walgit/api/ops/sync", nil, nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("require_write status = %d", w.Code)
	}
	// admin route with a write-only principal → 403
	p := auth.Principal{Name: "writer", Write: true}
	if w := f.do("PUT", "/demo/walgit/api/settings", strings.NewReader("[bundles]\n"), nil, &p); w.Code != http.StatusForbidden {
		t.Fatalf("require_admin status = %d", w.Code)
	}
	// reads self-auth for anonymous read
	if w := f.do("GET", "/demo/walgit/api", nil, nil, nil); w.Code != 200 {
		t.Fatalf("anonymous read status = %d", w.Code)
	}
}

// --- SSE envelope lifecycle ---------------------------------------------------------------

func TestSSEEnvelopeLifecycle(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	rec := TaskRecord{ID: "x", Kind: "sync", Repo: "demo/walgit", LogTail: []string{}}
	done := make(chan TaskDone, 1)
	updates := make(chan Progress, 2)
	f.tasks.streams["op:sync"] = TaskStream{Record: rec, Updates: updates, Done: done}
	go func() {
		close(updates)
		done <- TaskDone{Record: rec, Err: &TaskErr{Status: 503, Message: "interrupted: instance shut down; will be retried by the next pass"}}
	}()
	w := f.req("POST", "/demo/walgit/api/ops/sync")
	body := w.Body.String()
	want := ": walgit\n\n" +
		"event: task\ndata: {\"id\":\"x\",\"kind\":\"sync\",\"repo\":\"demo/walgit\",\"hostname\":\"\",\"started\":\"\",\"elapsed_ms\":0,\"summary\":\"\",\"log_tail\":[]}\n\n" +
		"event: error\ndata: {\"status\":503,\"message\":\"interrupted: instance shut down; will be retried by the next pass\"}\n\n"
	if body != want {
		t.Fatalf("error envelope:\n got %q\nwant %q", body, want)
	}
	if h := w.Header().Get("Content-Type"); h != "text/event-stream; charset=utf-8" {
		t.Fatalf("sse content-type = %q", h)
	}
	if h := w.Header().Get("Cache-Control"); h != "no-store" {
		t.Fatalf("sse cache = %q", h)
	}
	if h := w.Header().Get("X-Accel-Buffering"); h != "no" {
		t.Fatalf("sse accel = %q", h)
	}
}

func TestSSETerminalOnce(t *testing.T) {
	s, ok := NewSSE(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !ok {
		t.Fatal("recorder must flush")
	}
	if !s.Event("notice", `{"text":"a"}`) {
		t.Fatal("first packet should write")
	}
	if !s.Event("result", `{"task":{}}`) {
		t.Fatal("terminal should write")
	}
	if s.Event("error", `{"status":500,"message":"late"}`) {
		t.Fatal("terminal-once violated")
	}
	if s.Event("notice", `{"text":"late"}`) {
		t.Fatal("packets after terminal must be refused")
	}
	s.Close()
}

func TestSSEContextCancel(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s, ok := NewSSE(w, r)
	if !ok {
		t.Fatal("flusher expected")
	}
	cancel()
	if s.Event("notice", `{"text":"x"}`) {
		t.Fatal("packets must stop on client cancellation")
	}
}
