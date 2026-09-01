// edge_test.go — second-pass coverage for the failure/edge bodies: metrics
// defaults, sweep error backstops, cursor read/CAS error paths, notify body
// limits, and the webhook sink's request-build error.
package events

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func otherErr(key string) error {
	return &store.StoreError{Kind: store.ErrKindOther, Key: key, Err: errors.New("boom")}
}

// failGet fails every Get with a non-notfound error.
type failGet struct{ store.ObjectStore }

func (s failGet) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, otherErr(key)
}

// failPut fails every Put with a non-precondition error.
type failPut struct{ store.ObjectStore }

func (s failPut) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, otherErr(key)
}

// wrongResult answers Get with a NotModified instead of an Object.
type wrongResult struct{ store.ObjectStore }

func (s wrongResult) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return store.NotModified{Version: "v1"}, nil
}

// ---- metrics defaults ---------------------------------------------------------

func TestNoopMetricsDiscards(t *testing.T) {
	NoopMetrics{}.Inc("x", "a")
	NoopMetrics{}.Add("x", 3, "a")
	NoopMetrics{}.Set("x", 7, "a")
}

func TestNew_DefaultsMetricsAndLogger(t *testing.T) {
	br := New(Deps{})
	if got := br.Wake("acme/repo"); got != StatusQueued {
		t.Fatalf("wake with default deps = %q, want queued", got)
	}
}

// ---- sweep backstops ----------------------------------------------------------

func TestSweep_ListErrorWarnsAndReturns(t *testing.T) {
	br, _, _ := newTestBridge(t, errSource{err: errors.New("listing down")})
	br.sweep(context.Background()) // must warn, not panic
}

func TestSweep_CatchUpErrorContinues(t *testing.T) {
	repo := &fakeRepo{head: 1, minSeq: 1, syncErr: errors.New("sync down")}
	src := newFakeSource(repo)
	br, met, _ := newTestBridge(t, src)
	br.sweep(context.Background())
	if met.count(MetricSweepFound) != 0 {
		t.Fatal("failed catch-up must not count sweep events")
	}
}

func TestSweep_PublishesAndCountsSha256(t *testing.T) {
	repo := &fakeRepo{head: 1, minSeq: 1, sha256: true, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, strings.Repeat("b", 40))),
	}}
	src := newFakeSource(repo)
	br, met, _ := newTestBridge(t, src, newFakeSink("webhook"))
	br.sweep(context.Background())
	if n := met.count(MetricSweepFound); n != 1 {
		t.Fatalf("sweep found = %d, want 1", n)
	}
	// formatOf → Sha256 → the zero OID on the wire is 64 hex.
	snk := br.sinks[0].(*fakeSink)
	if got := len(snk.lastBatch()[0].Old); got != 64 {
		t.Fatalf("sha256 zero oid len = %d, want 64", got)
	}
}

// errSource fails listing (Repos) or every Handle.
type errSource struct {
	err error
}

func (s errSource) Repos(ctx context.Context) ([]string, error)               { return nil, s.err }
func (s errSource) Handle(ctx context.Context, repo string) (RepoView, error) { return nil, s.err }

// ---- catchUp error paths -------------------------------------------------------

func TestCatchUp_BadRepoIdErrors(t *testing.T) {
	br, _, _ := newTestBridge(t, newFakeSource())
	if _, err := br.catchUp(context.Background(), "no-slash"); err == nil {
		t.Fatal("bad repo id must error")
	}
}

func TestCatchUp_CursorReadError(t *testing.T) {
	br, _, _ := newTestBridge(t, newFakeSource(&fakeRepo{head: 1, minSeq: 1}))
	br.st = failGet{store.NewMemory()}
	if _, err := br.catchUp(context.Background(), "owner/r0"); err == nil {
		t.Fatal("cursor read failure must error")
	}
}

func TestCatchUp_CursorPutError(t *testing.T) {
	repo := &fakeRepo{head: 1, minSeq: 1, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, "aaaa")),
	}}
	br, _, _ := newTestBridge(t, newFakeSource(repo), newFakeSink("webhook"))
	br.st = failPut{store.NewMemory()}
	if n, err := br.catchUp(context.Background(), "owner/r0"); err == nil || n != 0 {
		t.Fatalf("cursor CAS failure: n=%d err=%v, want error", n, err)
	}
}

// ---- cursor helpers ------------------------------------------------------------

func TestLoadCursor_Errors(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	key, err := cursorKey("acme/repo")
	if err != nil {
		t.Fatal(err)
	}

	// A Get that returns a non-Object result is rejected.
	if _, _, _, err := loadCursor(ctx, wrongResult{st}, key); err == nil {
		t.Fatal("non-object Get result must error")
	}

	// Corrupt cursor JSON is a decode error, not a silent reset.
	if _, err := st.Put(ctx, key, store.PutBody{Bytes: []byte("{oops")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadCursor(ctx, st, key); err == nil {
		t.Fatal("corrupt cursor must error")
	}
}

func TestCursorKey_BadRepo(t *testing.T) {
	if _, err := cursorKey("bad"); err == nil {
		t.Fatal("bad repo id must error")
	}
}

// ---- notify parser and handler edges -------------------------------------------

func TestParseNotify_GlueDecodeErrorYieldsNil(t *testing.T) {
	// Root is valid JSON, but the glue decode (key must be a string) fails.
	if got := parseNotify([]byte(`{"key": 123}`)); got != nil {
		t.Fatalf("glue decode error = %+v, want nil", got)
	}
}

func TestKeyTrigger_EmptyKey(t *testing.T) {
	if got := keyTrigger(""); got != nil {
		t.Fatalf("empty key = %+v, want nil", got)
	}
}

// nilBodyRequest has a nil Body (server-side abort cases).
func nilBodyRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/_events/notify", nil)
	r.Body = nil
	return r
}

type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("conn reset") }

type okWriteCloser struct{}

func (okWriteCloser) Read(p []byte) (int, error) { return 0, errors.New("boom") }
func (okWriteCloser) Close() error               { return nil }

func TestHandleNotify_BodyEdges(t *testing.T) {
	br, _, _ := notifyRepoFor(t, store.NewMemory())

	// Nil body → 400.
	w := httptest.NewRecorder()
	br.HandleNotify(w, nilBodyRequest())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("nil body → %d, want 400", w.Code)
	}

	// Read error → 400.
	req := httptest.NewRequest(http.MethodPost, "/_events/notify", errReader{})
	w = httptest.NewRecorder()
	br.HandleNotify(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("read error → %d, want 400", w.Code)
	}

	// Oversized body → 400 (limit is 1 MiB).
	big := `{"key":"` + strings.Repeat("a", 1<<20) + `"}`
	w = httptest.NewRecorder()
	br.HandleNotify(w, httptest.NewRequest(http.MethodPost, "/_events/notify", strings.NewReader(big)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body → %d, want 400", w.Code)
	}
}

// failAfterHeader fails every Write after the status has been sent — the
// handler must log and return rather than panic.
type failAfterHeader struct {
	http.ResponseWriter
}

func (failAfterHeader) Write([]byte) (int, error) { return 0, errors.New("client went away") }

func TestHandleNotify_EncodeErrorIsLogged(t *testing.T) {
	br, _, _ := notifyRepoFor(t, store.NewMemory())
	req := httptest.NewRequest(http.MethodPost, "/_events/notify", strings.NewReader(`{"repo":"acme/repo"}`))
	w := failAfterHeader{httptest.NewRecorder()}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("encode error must not panic: %v", r)
		}
	}()
	br.HandleNotify(w, req)
}

// ---- webhook sink edges ---------------------------------------------------------

func TestWebhookSink_NameAndRequestBuildError(t *testing.T) {
	s := &WebhookSink{URL: "http://example.invalid/hook"}
	if s.Name() != "webhook" {
		t.Fatalf("name = %q", s.Name())
	}
	// A control character in the URL fails request construction.
	bad := &WebhookSink{URL: "http://example.invalid/\x7f"}
	if err := bad.Deliver(context.Background(), "acme/repo", []RefEvent{{Repo: "acme/repo"}}); err == nil {
		t.Fatal("bad URL must error")
	}
}
