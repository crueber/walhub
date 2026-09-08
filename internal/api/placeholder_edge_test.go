package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- PlaceholderHints lifecycle (api-level cover) ----------------------------

func TestPlaceholderHintsAddConsume(t *testing.T) {
	var nilHints *PlaceholderHints
	nilHints.Add(git.RepoId{Owner: "a", Name: "b"}) // nil-safe, no panic
	if nilHints.Consume(git.RepoId{Owner: "a", Name: "b"}) {
		t.Fatal("nil hints must never consume")
	}
	h := &PlaceholderHints{}
	id := git.RepoId{Owner: "a", Name: "b"}
	if h.Consume(id) {
		t.Fatal("absent hint must not consume")
	}
	h.Add(id)
	h.Add(id)
	if !h.Consume(id) || h.Consume(id) {
		t.Fatal("consume must fire exactly once")
	}
}

// --- CreateHandler.ServeHTTP ---------------------------------------------------

func TestCreateHandlerServeHTTP(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	ch := &CreateHandler{Env: f.env}
	// Non-create path → 404.
	w := httptest.NewRecorder()
	ch.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/owners", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-create = %d", w.Code)
	}
	// Create path via ServeHTTP → gated (no principal, token mode) → 403
	// (anonymous with AnonymousRead keeps read, but write is refused).
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"a","name":"b"}`))
	ch.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("anonymous create = %d (%s)", w.Code, w.Body.String())
	}
}

// --- post edges ------------------------------------------------------------------

func TestCreateHandlerNilEnv(t *testing.T) {
	ch := &CreateHandler{Env: nil}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{}`))
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
	ch.Handle(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil env = %d", w.Code)
	}
}

func TestCreateHandlerUnreadableBody(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	ch := &CreateHandler{Env: f.env}
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(``))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
	w := httptest.NewRecorder()
	ch.Handle(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d", w.Code)
	}
}

func TestCreateHandlerNoRepos(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	f.env.Repos = nil
	ch := &CreateHandler{Env: f.env}
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"a","name":"b"}`))
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
	w := httptest.NewRecorder()
	ch.Handle(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry = %d", w.Code)
	}
}

func TestCreateHandlerPlaceholderFalse(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	ch := &CreateHandler{Env: f.env}
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"acme","name":"plain","placeholder":false}`))
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
	w := httptest.NewRecorder()
	ch.Handle(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("placeholder:false = %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "placeholder") {
		t.Fatalf("flag-less POST must not carry placeholder: %s", w.Body.String())
	}
	raw, _, _ := store.GetBytes(context.Background(), f.env.Store, "repos/acme/plain/"+PlaceholderKeySuffix, store.GetOptions{})
	if raw != nil {
		t.Fatal("flag-less POST must not write a sidecar")
	}
}

func TestCreateHandlerAccessBootError(t *testing.T) {
	f, _, b := placeholderFixture(t)
	b.err = errors.New("access down")
	ch := &CreateHandler{Env: f.env}
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"acme","name":"bootfail"}`))
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
	w := httptest.NewRecorder()
	ch.Handle(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("access failure = %d (%s)", w.Code, w.Body.String())
	}
}

func TestCreateHandlerReadGateDeny(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	ch := &CreateHandler{Env: f.env}
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"a","name":"b"}`))
	// Read-only principal: require_write refuses with 403.
	r = r.WithContext(WithPrincipal(r.Context(), auth.Principal{Name: "reader"}))
	w := httptest.NewRecorder()
	ch.Handle(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-only = %d", w.Code)
	}
}

// --- repoPut edges ------------------------------------------------------------------

func TestPutPlaceholderNoStore(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	f.env.Store = nil // marker skipped; manifest create still wins
	w := f.do("PUT", "/acme/nostore?placeholder=true", nil, nil, writerPrincipal("a@b.c"))
	if w.Code != http.StatusCreated {
		t.Fatalf("no-store PUT = %d (%s)", w.Code, w.Body.String())
	}
}

func TestPutPlaceholderNoRepos(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	f.env.Repos = nil
	w := f.do("PUT", "/acme/noreg?placeholder=true", nil, nil, writerPrincipal("a@b.c"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry = %d", w.Code)
	}
}

func TestPutPlaceholderSidecarError(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	f.env.Store = &failPutStore{ObjectStore: f.env.Store}
	w := f.do("PUT", "/acme/scerr?placeholder=true", nil, nil, writerPrincipal("a@b.c"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sidecar failure = %d (%s)", w.Code, w.Body.String())
	}
}

// failPutStore fails placeholder sidecar Creates (non-412) while letting the
// manifest Create through.
type failPutStore struct {
	store.ObjectStore
}

func (f *failPutStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if strings.HasSuffix(key, PlaceholderKeySuffix) {
		return store.ObjectMeta{}, errors.New("sidecar down")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// --- stillUnborn edges ---------------------------------------------------------------

func TestStillUnbornEdges(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	h := &handlers{env: f.env}
	id := git.RepoId{Owner: "acme", Name: "x"}
	// Unknown to the view (Summary errors) → fail-closed false.
	if h.stillUnborn(context.Background(), id) {
		t.Fatal("view error must fail closed (false)")
	}
	// Nil view → sidecar decides (true).
	f.env.Repo = nil
	if !h.stillUnborn(context.Background(), id) {
		t.Fatal("nil view must return true")
	}
}

// --- probe edges -----------------------------------------------------------------------

func TestProbePlaceholderEdges(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "acme", Name: "x"}
	if _, ok := probePlaceholder(ctx, nil, id); ok {
		t.Fatal("nil store must not probe")
	}
	st := store.NewMemory()
	if _, err := st.Put(ctx, "repos/acme/x/"+PlaceholderKeySuffix, store.PutBody{Bytes: []byte("{corrupt")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := probePlaceholder(ctx, st, id); ok {
		t.Fatal("corrupt sidecar must not project")
	}
}

// --- splitCreatePath (per-segment decoding, never unescape joined) -------------------
// The PathUnescape-error branch is defensive dead code through net/url
// (EscapedPath never yields an undecodable escape — same shape as
// repoimport.splitPath): the landed behavior is one decoded segment per
// raw segment.
func TestSplitCreatePathDecodesPerSegment(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/repos", nil)
	segs := splitCreatePath(r)
	if len(segs) != 3 || segs[0] != "api" || segs[1] != "v1" || segs[2] != "repos" {
		t.Fatalf("segs = %v", segs)
	}
}
