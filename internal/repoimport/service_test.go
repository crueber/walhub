// service_test.go — Begin gates, B2/B3, end-to-end file:// import, scrub,
// gates, outcomes (real git + memory store, no network).
package repoimport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// --- helpers -------------------------------------------------------------------------

func awaitDone(t *testing.T, svc *Service, id string, timeout time.Duration) *Outcome {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		st, ok := svc.streams[id]
		svc.mu.Unlock()
		if ok {
			_, outcome, done := st.snapshot()
			if done {
				return outcome
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("import %s not finished within %v", id, timeout)
	return nil
}

func fileParams(owner, name, fileURL string) Params {
	n, err := NormalizeSource(fileURL)
	if err != nil {
		panic(err)
	}
	return Params{Owner: owner, Name: name, Source: n, Refs: []string{}, importer: "tester"}
}

func doPost(t *testing.T, h *Handler, path, body string, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func doGet(t *testing.T, h *Handler, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func testHandler(svc *Service, p auth.Principal) *Handler {
	return &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return p, nil
	}}
}

// --- Begin gates -----------------------------------------------------------------------

func TestBeginGates(t *testing.T) {
	newSvc := func(t *testing.T, roles RoleService) (*Service, *Handler) {
		svc, _ := testService(t, nil, roles)
		return svc, testHandler(svc, adminPrincipal())
	}
	mkParams := func() (Params, string) {
		p, tok, err := ParseRequest([]byte(`{"source_url":"file:///srv/r.git","owner":"acme","name":"w"}`), testConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	t.Run("anonymous 401", func(t *testing.T) {
		svc, _ := newSvc(t, &FakeRoles{})
		h := testHandler(svc, auth.Anonymous())
		w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/r.git","owner":"acme","name":"w"}`, "")
		if w.Code != 401 {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if h := w.Header().Get("WWW-Authenticate"); h != `Bearer realm="walgit"` {
			t.Fatalf("WWW-Authenticate = %q", h)
		}
	})
	t.Run("forbidden 403", func(t *testing.T) {
		svc, _ := newSvc(t, &FakeRoles{Roles: map[string]string{"pleb": "read"}})
		h := testHandler(svc, auth.Principal{Name: "pleb"})
		w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/r.git","owner":"acme","name":"w"}`, "")
		if w.Code != 403 {
			t.Fatalf("status = %d, want 403 (body %q)", w.Code, w.Body.String())
		}
	})
	t.Run("b3 foreign manifest 409", func(t *testing.T) {
		svc, h := newSvc(t, &FakeRoles{})
		ctx := context.Background()
		if _, err := svc.reg.Create(ctx, "acme/taken", 0); err != nil {
			t.Fatal(err)
		}
		w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/r.git","owner":"acme","name":"taken"}`, "")
		if w.Code != 409 {
			t.Fatalf("status = %d, want 409 (body %q)", w.Code, w.Body.String())
		}
	})
	t.Run("b3 no-op 200", func(t *testing.T) {
		svc, h := newSvc(t, &FakeRoles{})
		ctx := context.Background()
		if _, err := svc.reg.Create(ctx, "acme/done", 0); err != nil {
			t.Fatal(err)
		}
		doc := &ImportDoc{Version: 1, SourceURL: "file:///srv/r.git", SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{"refs/heads/main": strings.Repeat("a", 40)}, Importer: "tester", Format: "sha1", ImportedAt: nowRFC3339()}
		if err := writeImportDoc(ctx, svc.store, "acme", "done", doc); err != nil {
			t.Fatal(err)
		}
		w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/r.git","owner":"acme","name":"done"}`, "")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
		}
		var out struct {
			Repo   string            `json:"repo"`
			Import map[string]string `json:"import"`
		}
		_ = out
	})
	t.Run("b3 same target different source 409", func(t *testing.T) {
		svc, h := newSvc(t, &FakeRoles{})
		ctx := context.Background()
		if _, err := svc.reg.Create(ctx, "acme/other", 0); err != nil {
			t.Fatal(err)
		}
		doc := &ImportDoc{Version: 1, SourceURL: "file:///srv/else.git", SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339()}
		if err := writeImportDoc(ctx, svc.store, "acme", "other", doc); err != nil {
			t.Fatal(err)
		}
		w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/r.git","owner":"acme","name":"other"}`, "")
		if w.Code != 409 {
			t.Fatalf("status = %d, want 409 (body %q)", w.Code, w.Body.String())
		}
	})
	t.Run("nil roles host flags", func(t *testing.T) {
		svc, _ := testService(t, nil, nil)
		p, _ := mkParams()
		if _, _, err := svc.Begin(context.Background(), auth.Principal{Name: "x"}, p, ""); err == nil {
			t.Fatalf("expected 403 without host flags")
		} else if se, ok := err.(*StatusError); !ok || se.Status != 403 {
			t.Fatalf("err = %v, want 403", err)
		}
	})
}

// --- B2 params-aware join ------------------------------------------------------------------

func TestBeginB2Join(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	ctx := context.Background()
	mk := func(src string) Params {
		q, _, err := ParseRequest([]byte(fmt.Sprintf(`{"source_url":%q,"owner":"acme","name":"w"}`, src)), svc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	// Seed a running entry directly (deterministic: no git involved).
	st := newStream()
	st.target = "acme/w"
	done := make(chan struct{})
	want := mk("file:///srv/r.git").scrubbedMap()
	svc.mu.Lock()
	svc.running["acme/w"] = &running{id: "i-test", params: want, done: done}
	svc.streams["i-test"] = st
	svc.mu.Unlock()

	// Same params → join the same id.
	res, _, err := svc.Begin(ctx, adminPrincipal(), mk("file:///srv/r.git"), "")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if !res.Joined || res.TaskID != "i-test" {
		t.Fatalf("join = %+v, want Joined i-test", res)
	}
	// Different source → 409 naming the running import.
	_, _, err = svc.Begin(ctx, adminPrincipal(), mk("file:///srv/other.git"), "")
	if se, ok := err.(*StatusError); !ok || se.Status != 409 || !strings.Contains(se.Message, "i-test") {
		t.Fatalf("mismatch err = %v, want 409 naming i-test", err)
	}
	// Different options → 409.
	other := mk("file:///srv/r.git")
	other.IncludePullHeads = true
	_, _, err = svc.Begin(ctx, adminPrincipal(), other, "")
	if se, ok := err.(*StatusError); !ok || se.Status != 409 {
		t.Fatalf("options-mismatch err = %v, want 409", err)
	}
	close(done)
	svc.mu.Lock()
	delete(svc.running, "acme/w")
	delete(svc.streams, "i-test")
	svc.mu.Unlock()
}

// TestBeginConcurrentSingleFlight hammers one target: exactly one leader,
// every same-params starter joins it (stress with -count=100).
func TestBeginConcurrentSingleFlight(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	ctx := context.Background()
	const n = 16
	type res struct {
		id     string
		joined bool
		err    error
	}
	out := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q, _, err := ParseRequest([]byte(`{"source_url":"file:///srv/missing.git","owner":"acme","name":"race"}`), svc.cfg)
			if err != nil {
				out[i] = res{err: err}
				return
			}
			r, _, err := svc.Begin(ctx, adminPrincipal(), q, "")
			if err != nil {
				out[i] = res{err: err}
				return
			}
			out[i] = res{id: r.TaskID, joined: r.Joined}
		}(i)
	}
	wg.Wait()
	ids := map[string]int{}
	leaders := 0
	for _, r := range out {
		if r.err != nil {
			t.Fatalf("begin error: %v", r.err)
		}
		ids[r.id]++
		if !r.joined {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want exactly 1 (%v)", leaders, out)
	}
	if len(ids) != 1 {
		t.Fatalf("task ids = %v, want exactly one shared id", ids)
	}
	for id := range ids {
		o := awaitDone(t, svc, id, 60*time.Second)
		if o.Err == nil {
			t.Fatalf("missing-file import should fail, got %+v", o)
		}
	}
}

// --- POST validation matrix (table-driven httptest) ------------------------------------------

func TestPostValidationMatrix(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	cfg := svc.cfg
	_ = cfg
	for _, tc := range []struct {
		name string
		body string
		want int
		part string
	}{
		{name: "bad json", body: `{`, want: 400},
		{name: "unknown field", body: `{"source_url":"file:///x","owner":"a","name":"b","nope":1}`, want: 400, part: "unknown field"},
		{name: "missing owner", body: `{"source_url":"file:///x","name":"b"}`, want: 400},
		{name: "bad target chars", body: `{"source_url":"file:///x","owner":"a c","name":"b"}`, want: 400},
		{name: "bad url", body: `{"source_url":"notaurl","owner":"a","name":"b"}`, want: 400},
		{name: "embedded creds", body: `{"source_url":"https://u:p@github.com/a/b.git","owner":"a","name":"b"}`, want: 400, part: "must not embed credentials"},
		{name: "ssh refused", body: `{"source_url":"ssh://example.com/a/b.git","owner":"a","name":"b"}`, want: 400, part: "not supported"},
		{name: "scp refused", body: `{"source_url":"git@github.com:a/b.git","owner":"a","name":"b"}`, want: 400, part: "not supported"},
		{name: "token over http", body: `{"source_url":"http://example.com/a/b.git","owner":"a","name":"b","token":"t","dangerous":true}`, want: 400, part: "never send credentials"},
		{name: "bad format", body: `{"source_url":"file:///x","owner":"a","name":"b","format":"md5"}`, want: 400},
		{name: "bad refs entry", body: `{"source_url":"file:///x","owner":"a","name":"b","refs":["main"]}`, want: 400},
		{name: "file denied", body: `{"source_url":"file:///x","owner":"a","name":"b"}`, want: 400, part: "allow_file_urls"},
		{name: "generic needs dangerous", body: `{"source_url":"https://git.example.com/a/b.git","owner":"a","name":"b"}`, want: 400, part: "dangerous confirm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "file denied" {
				cfg2 := testConfig(t)
				cfg2.Import.AllowFileURLs = false
				svc2, _ := testService(t, cfg2, &FakeRoles{})
				h2 := testHandler(svc2, adminPrincipal())
				w := doPost(t, h2, "/api/v1/repos/imports", tc.body, "")
				if w.Code != tc.want {
					t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
				}
				return
			}
			w := doPost(t, h, "/api/v1/repos/imports", tc.body, "")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
			if tc.part != "" && !strings.Contains(w.Body.String(), tc.part) {
				t.Fatalf("body %q lacks %q", w.Body.String(), tc.part)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("error content-type = %q, want text/plain", ct)
			}
		})
	}
}

// --- routing -------------------------------------------------------------------------------------

func TestHandlerRouting(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)) {
		t.Fatalf("non-import path must report false")
	}
	w := doPost(t, h, "/api/v1/repos/imports", `{}`, "")
	if w.Code != 400 {
		t.Fatalf("empty object must 400, got %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/imports", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != 405 {
		t.Fatalf("PUT imports = %d, want 405", w2.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/imports/x", nil)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req)
	if w3.Code != 405 {
		t.Fatalf("POST imports/{id} = %d, want 405", w3.Code)
	}
	w4 := doGet(t, h, "/api/v1/repos/imports/does-not-exist", "")
	if w4.Code != 404 {
		t.Fatalf("unknown id = %d, want 404", w4.Code)
	}
	// Browser twin routes identically.
	w5 := doGet(t, h, "/api-browser/v1/repos/imports/does-not-exist", "")
	if w5.Code != 404 {
		t.Fatalf("browser twin unknown id = %d, want 404", w5.Code)
	}
	// Fallthrough 404.
	req = httptest.NewRequest(http.MethodGet, "/nope", nil)
	w6 := httptest.NewRecorder()
	h.ServeHTTP(w6, req)
	if w6.Code != 404 {
		t.Fatalf("fallthrough = %d, want 404", w6.Code)
	}
}
