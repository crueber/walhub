package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// gaps5_test.go closes the remaining edge arms: policy store-error paths,
// SSE client-disconnect write arms, cache bucket arms, walView snapshot /
// local-view / log error arms, and pure helpers (routes, render, settings).

// --- helpers ---------------------------------------------------------------------------

// failWriter fails every Write: a client that vanished mid-stream.
type failWriter struct{ hdr http.Header }

func newFailWriter() *failWriter                { return &failWriter{hdr: http.Header{}} }
func (w *failWriter) Header() http.Header       { return w.hdr }
func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (w *failWriter) WriteHeader(int)           {}
func (w *failWriter) Flush()                    {}

// noFlushWriter implements http.ResponseWriter without http.Flusher.
type noFlushWriter struct{ code int }

func (w *noFlushWriter) Header() http.Header         { return http.Header{} }
func (w *noFlushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *noFlushWriter) WriteHeader(c int)           { w.code = c }

// flakyStore decorates an ObjectStore with injectable failures.
type flakyStore struct {
	store.ObjectStore
	getErr    error           // Get always fails with this
	getResult store.GetResult // when set, Get returns this result instead
	putErr    error
	delErr    error
}

func (s *flakyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.getResult != nil {
		return s.getResult, nil
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *flakyStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil {
		return store.ObjectMeta{}, s.putErr
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func (s *flakyStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if s.delErr != nil {
		return s.delErr
	}
	return s.ObjectStore.Delete(ctx, key, ifVersion)
}

// errLogBoom is the injected ReadLog failure; errBoom comes from gaps_test.go.
var errLogBoom = errors.New("log boom")

// adminP is a full-access principal for direct requests.
func adminP() *auth.Principal { return &auth.Principal{Name: "jane", Write: true, Admin: true} }

// --- policy store-error paths (§10) ----------------------------------------------------

func TestPolicyStoreFailures(t *testing.T) {
	id := git.RepoId{Owner: "demo", Name: "walgit"}

	// A stored-but-unparseable doc fails closed on GET (503, not allow-all).
	f := newFixture(t)
	if _, err := f.env.Store.Put(context.Background(), policyKey(id),
		store.PutBody{Bytes: []byte("}{ not json")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if w := f.req("GET", "/demo/walgit/api/policy"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET corrupt policy = %d (%s)", w.Code, w.Body.String())
	}

	// A generic store read failure: 503 on GET and on PUT's loadPolicy arm.
	f2 := newFixture(t)
	f2.env.Store = &flakyStore{ObjectStore: f2.env.Store, getErr: store.NewRetryable(policyKey(id), errors.New("boom"))}
	if w := f2.req("GET", "/demo/walgit/api/policy"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET with failing store = %d", w.Code)
	}
	if w := f2.req("PUT", "/demo/walgit/api/policy", strings.NewReader(allowAllPolicy)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT with failing loadPolicy = %d (%s)", w.Code, w.Body.String())
	}

	// CAS loss on Put → 409; generic Put failure → 503.
	for _, tc := range []struct {
		name   string
		putErr error
		want   int
	}{
		{"precondition", store.NewPrecondition(policyKey(id), "v"), http.StatusConflict},
		{"generic", store.NewRetryable(policyKey(id), errors.New("boom")), http.StatusServiceUnavailable},
	} {
		f3 := newFixture(t)
		f3.env.Store = &flakyStore{ObjectStore: f3.env.Store, putErr: tc.putErr}
		w := f3.req("PUT", "/demo/walgit/api/policy", strings.NewReader(allowAllPolicy))
		if w.Code != tc.want {
			t.Fatalf("PUT %s = %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body.String())
		}
	}

	// Delete failures other than not-found → 503.
	f4 := newFixture(t)
	f4.env.Store = &flakyStore{ObjectStore: f4.env.Store, delErr: store.NewRetryable(policyKey(id), errors.New("boom"))}
	if w := f4.req("DELETE", "/demo/walgit/api/policy"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("DELETE with failing store = %d", w.Code)
	}
	// Deleting an absent policy is fine (NotFound tolerated → 204).
	f5 := newFixture(t)
	if w := f5.req("DELETE", "/demo/walgit/api/policy"); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE absent policy = %d", w.Code)
	}
}

// TestPolicyValidateAllowAll: no stored policy and an empty body → ok=true
// with zero rules (the loadPolicy allow-all path behind validatePolicy).
func TestPolicyValidateAllowAll(t *testing.T) {
	f := newFixture(t)
	w := f.req("POST", "/demo/walgit/api/policy/validate", strings.NewReader(""))
	if w.Code != 200 {
		t.Fatalf("validate = %d (%s)", w.Code, w.Body.String())
	}
	var res policyValidateBody
	decodeJSON(t, w, &res)
	if !res.OK || res.Rules != 0 || res.Groups != 0 || res.Protect == nil {
		t.Fatalf("allow-all validate = %+v", res)
	}
	// protectSummaries: nil doc → empty; a non-protect rule → skipped.
	if got := protectSummaries(nil); got == nil || len(got) != 0 {
		t.Fatalf("protectSummaries(nil) = %v", got)
	}
	doc := &policy.Document{Rules: []*policy.Rule{{Name: "hist", Effect: &policy.HistoryEffect{}}}}
	if got := protectSummaries(doc); len(got) != 0 {
		t.Fatalf("protectSummaries(non-protect) = %+v", got)
	}
}

// TestPolicyDryRunLast: ?last=N narrows the push window (§10).
func TestPolicyDryRunLast(t *testing.T) {
	f := newFixture(t)
	f.view.pushes[git.RepoId{Owner: "demo", Name: "walgit"}.String()] = []PushRecord{}
	w := f.req("POST", "/demo/walgit/api/policy/dry-run?last=5", strings.NewReader(""))
	if w.Code != 200 {
		t.Fatalf("dry-run = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"results":[]`) {
		t.Fatalf("dry-run body = %s", w.Body.String())
	}
}

// --- SSE client-disconnect write arms (§9.3) --------------------------------------------

func TestSSEWriteFailureArms(t *testing.T) {
	// A writer whose Write always fails simulates the client gone mid-stream.
	s, ok := NewSSE(newFailWriter(), httptest.NewRequest("GET", "/x", nil))
	if !ok {
		t.Fatal("failing writer must still construct (Flush is implemented)")
	}
	defer s.Close()
	if s.Event("notice", "{}") {
		t.Fatal("Event must fail on a broken client")
	}
	if s.comment(": keepalive") {
		t.Fatal("comment must fail on a broken writer")
	}
	if s.Event("result", "{}") {
		t.Fatal("terminal Event must fail on a broken writer")
	}

	// mustJSON falls back to {"error":"encode"} on unmarshalable values.
	if got := mustJSON(make(chan int)); got != `{"error":"encode"}` {
		t.Fatalf("mustJSON(chan) = %s", got)
	}

	// A canceled request context makes comment refuse without writing.
	rw := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/x", nil)
	ctx, cancel := context.WithCancel(r2.Context())
	defer cancel()
	s2, ok := NewSSE(rw, r2.WithContext(ctx))
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer s2.Close()
	cancel()
	if s2.comment(": keepalive") {
		t.Fatal("comment after request cancel must refuse")
	}

	// pump on an ended envelope: the replay arm returns early; a live packet
	// on the same dead envelope is also refused.
	s3, _ := NewSSE(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	s3.Event("result", "{}")
	rec := TaskRecord{ID: "t", Kind: "fsck", Hostname: "h"}
	s3.pump(TaskStream{Record: rec, Replay: []Progress{{Kind: "notice", Text: "late"}}})
	live := make(chan Progress, 1)
	live <- Progress{Kind: "notice", Text: "later"}
	close(live)
	s3.pump(TaskStream{Record: rec, Updates: live})
}

// --- cache arms (§5) ---------------------------------------------------------------------

func TestRefCacheStaleRevision(t *testing.T) {
	c := newRefCache(4)
	c.l.Put(refKey("demo/walgit", "main"), 1, &revStamped{revision: 1, sha: "aaa"})
	if sha, ok := c.Get("demo/walgit", "main", 1); !ok || sha != "aaa" {
		t.Fatalf("fresh hit = (%q, %v)", sha, ok)
	}
	// A newer manifest revision lazily invalidates the entry.
	if _, ok := c.Get("demo/walgit", "main", 2); ok {
		t.Fatal("stale revision must miss")
	}
}

func TestBucketArms(t *testing.T) {
	ctx := context.Background()

	// Store error → miss.
	f := newFixture(t)
	f.env.Store = &flakyStore{ObjectStore: f.env.Store, getErr: store.NewRetryable("k", errors.New("boom"))}
	if b, ok := f.env.bucketGet(ctx, "k", 1); ok || b != nil {
		t.Fatal("bucketGet must fail closed on store error")
	}
	// Non-Object GetResult → miss.
	f.env.Store = &flakyStore{ObjectStore: f.env.Store, getResult: store.NotModified{Version: "1"}}
	if _, ok := f.env.bucketGet(ctx, "k", 1); ok {
		t.Fatal("non-object result must be a miss")
	}

	f2 := newFixture(t)
	// Corrupt mirror body → miss.
	if _, err := f2.env.Store.Put(ctx, bucketKey("k"), store.PutBody{Bytes: []byte("not json")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.env.bucketGet(ctx, "k", 1); ok {
		t.Fatal("corrupt envelope must miss")
	}
	// Stale revision → miss; matching revision → hit.
	body := []byte(`{"tree":true}`)
	env2, _ := json.Marshal(bucketEnvelope{Revision: 2, Body: json.RawMessage(body)})
	if _, err := f2.env.Store.Put(ctx, bucketKey("k"), store.PutBody{Bytes: env2}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.env.bucketGet(ctx, "k", 1); ok {
		t.Fatal("stale generation must miss")
	}
	if b, ok := f2.env.bucketGet(ctx, "k", 2); !ok || !strings.Contains(string(b), "tree") {
		t.Fatalf("matching envelope = (%q, %v)", b, ok)
	}

	// bucketPut: nil store is a no-op; an unmarshalable body returns early.
	var nilStoreEnv Env
	nilStoreEnv.bucketPut("k", 1, []byte("{}"))
	f2.env.bucketPut("k", 1, []byte{0xFF}) // json.RawMessage marshal error
	f2.env.bucketPut("k", 1, []byte(`{"ok":true}`))

	// renderImmutable serves the bucket mirror without rendering.
	rendered := false
	got, err := f2.env.renderImmutable(ctx, "k", 2, "abc", func() ([]byte, error) {
		rendered = true
		return nil, errors.New("must not render")
	})
	if err != nil || rendered {
		t.Fatalf("bucket hit rendered anyway: %q %v", got, err)
	}
	// A render error propagates.
	if _, err := f2.env.renderImmutable(ctx, "k2", 1, "e", func() ([]byte, error) { return nil, errBoom }); !errors.Is(err, errBoom) {
		t.Fatalf("render error = %v", err)
	}
}

// --- discovery gate failures (§8) --------------------------------------------------------

func TestDiscoveryOwnerGateFails(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = false
	for _, path := range []string{"/api/v1/owners", "/api/v1/owners/demo/repos"} {
		if w := f.do("GET", path, nil, nil, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s anonymous = %d, want 401", path, w.Code)
		}
	}
}

// --- ops / tasks error arms (§12) ---------------------------------------------------------

func TestOpsTaskErrorArms(t *testing.T) {
	f := newFixture(t)
	f.tasks.listErr = errors.New("list boom")
	// opsList tolerates a List error (recent becomes empty) → 200.
	if w := f.req("GET", "/demo/walgit/api/ops"); w.Code != 200 {
		t.Fatalf("ops list error = %d (%s)", w.Code, w.Body.String())
	}
	// tasksList surfaces it.
	if w := f.req("GET", "/demo/walgit/api/tasks"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("tasks list error = %d", w.Code)
	}

	// Unknown op → 404; Begin failure → 503.
	if w := f.req("POST", "/demo/walgit/api/ops/nope"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown op = %d", w.Code)
	}
	f.tasks.beginErr = errors.New("begin boom")
	if w := f.req("POST", "/demo/walgit/api/ops/fsck"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("op begin error = %d (%s)", w.Code, w.Body.String())
	}

	// taskGet: attach error (SSE dialect) and record lookup error (JSON).
	f.tasks.attachErr = errors.New("attach boom")
	w := f.do("GET", "/demo/walgit/api/tasks/t1", nil, map[string]string{"Accept": "text/event-stream"}, adminP())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("task attach error = %d (%s)", w.Code, w.Body.String())
	}
	f.tasks.attachErr = nil
	f.tasks.getErr = errors.New("get boom")
	if w := f.req("GET", "/demo/walgit/api/tasks/t"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("task get error = %d (%s)", w.Code, w.Body.String())
	}
}

// --- refs arms ------------------------------------------------------------------------------

func TestOpenWithoutRepoView(t *testing.T) {
	f := newFixture(t)
	f.env.Repo = nil // pending surface
	if w := f.req("GET", "/demo/walgit/api/policy"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("open with nil Repo = %d, want 503", w.Code)
	}
	if w := f.req("DELETE", "/demo/walgit/api/settings"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("settingsDelete with nil Repo = %d", w.Code)
	}
}

func TestRefsListTagsAndStreamArms(t *testing.T) {
	f := newFixture(t)
	if w := f.req("GET", "/demo/walgit/api/refs/tags"); w.Code != 200 {
		t.Fatalf("refs/tags = %d (%s)", w.Code, w.Body.String())
	}
	if w := f.req("GET", "/demo/walgit/api/refs/branches?n=0"); w.Code != 400 {
		t.Fatalf("refs n=0 = %d", w.Code)
	}
	h := &handlers{env: f.env}
	// A writer without Flusher → 406.
	nfw := &noFlushWriter{}
	h.streamRefs(nfw, []Ref{{Name: "refs/heads/main", SHA: fakeSHA}}, false)
	if nfw.code != http.StatusNotAcceptable {
		t.Fatalf("streamRefs non-flusher = %d", nfw.code)
	}
	// A client gone before the first packet ends the stream quietly.
	h.streamRefs(newFailWriter(), []Ref{{Name: "refs/heads/main", SHA: fakeSHA}}, false)
}

// --- repo lifecycle arms (§9.1) ---------------------------------------------------------

func TestRepoLifecycleArms(t *testing.T) {
	f := newFixture(t)
	if w := f.req("PUT", "/demo/newrepo?object_format=bogus"); w.Code != 400 {
		t.Fatalf("bad object_format = %d (%s)", w.Code, w.Body.String())
	}
	if w := f.req("PUT", "/demo/walgit"); w.Code != 201 {
		t.Fatalf("first PUT = %d (%s)", w.Code, w.Body.String())
	}
	if w := f.req("PUT", "/demo/walgit"); w.Code != http.StatusConflict {
		t.Fatalf("duplicate PUT = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	f.env.Repos = nil
	if w := f.req("PUT", "/demo/other"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT without registry = %d", w.Code)
	}
	if w := f.req("DELETE", "/demo/walgit"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("DELETE with nil registry = %d", w.Code)
	}
}

// --- misc route/query arms ---------------------------------------------------------------

func TestCommitsAndPathQueries(t *testing.T) {
	f := newFixture(t)
	if w := f.req("GET", "/demo/walgit/api/commits?n=abc"); w.Code != 400 {
		t.Fatalf("commits n=abc = %d (%s)", w.Code, w.Body.String())
	}
	if w := f.req("GET", "/demo/walgit/api/commits?skip=-1"); w.Code != 400 {
		t.Fatalf("commits skip=-1 = %d", w.Code)
	}
	// rawSegments splits on "/" and keeps each decoded segment.
	segs := rawSegments(httptest.NewRequest("GET", "/demo/walgit/api/tree/main", nil))
	if len(segs) != 5 || segs[4] != "main" {
		t.Fatalf("rawSegments = %v", segs)
	}
	// A {...} wildcard is only valid in final position.
	if got := matchRoute("{a...}/x", []string{"p", "x"}); got != nil {
		t.Fatalf("non-final wildcard must not match: %v", got)
	}
	if LaneOf(httptest.NewRequest("GET", "/x", nil)) != LaneAPI {
		t.Fatal("default lane must be LaneAPI")
	}
}

// --- walView over a (mostly real) serving copy ------------------------------------------

func TestWalViewSnapshotFailures(t *testing.T) {
	dir := t.TempDir()
	id := git.RepoId{Owner: "demo", Name: "walgit"}

	// packed-refs as a directory makes the snapshot unreadable.
	bad := dir
	if err := os.Mkdir(filepath.Join(bad, "packed-refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newEngineFixture(t, &fakeEngine{obj: wal.ObjectAccess{Local: &git.LocalRepo{Root: dir, ID: id, Path: bad}}, rev: 3})
	v := f.env.Repo.(*walView)
	ctx := context.Background()
	if _, err := v.Resolve(ctx, id, "main"); err == nil {
		t.Fatal("Resolve must fail when the snapshot is unreadable")
	}
	if _, _, err := v.Head(ctx, id); err == nil {
		t.Fatal("Head must fail when the snapshot fails")
	}
	if _, _, err := v.RefList(ctx, id, "heads", RefQuery{}); err == nil {
		t.Fatal("RefList must fail when the snapshot fails")
	}
	if _, err := v.Summary(ctx, id); err == nil {
		t.Fatal("Summary must fail when the snapshot fails")
	}

	// An unborn HEAD (no HEAD file) is a 404 on "" and false on Head.
	empty := t.TempDir()
	f2 := newEngineFixture(t, &fakeEngine{obj: wal.ObjectAccess{Local: &git.LocalRepo{Root: empty, ID: id, Path: empty}}, rev: 1})
	v2 := f2.env.Repo.(*walView)
	if _, err := v2.Resolve(ctx, id, ""); err == nil || !strings.Contains(err.Error(), "unborn") {
		t.Fatalf("unborn HEAD resolve = %v", err)
	}
	if _, ok, _ := v2.Head(ctx, id); ok {
		t.Fatal("unborn HEAD must resolve to false")
	}
	if s, err := v2.Summary(ctx, id); err != nil || s.Branches != 0 {
		t.Fatalf("empty repo summary = %+v %v", s, err)
	}
}

func TestWalViewLocalAndLogFailures(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}

	// Object access failures hit every local-serving recipe.
	f := newEngineFixture(t, &fakeEngine{objErr: errBoom})
	v := f.env.Repo.(*walView)
	if _, err := v.Tree(ctx, id, "main", ""); err == nil {
		t.Fatal("Tree must fail when object access is unavailable")
	}
	if _, err := v.Blob(ctx, id, "main", "hello.txt", false); err == nil {
		t.Fatal("Blob must fail when object access fails")
	}
	if _, err := v.Commits(ctx, id, "main", "", 0, 10); err == nil {
		t.Fatal("Commits must fail when object access fails")
	}
	if _, err := v.Commit(ctx, id, fakeSHA); err == nil {
		t.Fatal("Commit must fail when object access fails")
	}
	fSync := newEngineFixture(t, &fakeEngine{syncErr: errBoom})
	if err := fSync.env.Repo.(*walView).Sync(ctx, id, SyncRefs); !errors.Is(err, errBoom) {
		t.Fatalf("Sync error = %v", err)
	}

	// A log read failure propagates through PushHistory and logEntries.
	f2 := newEngineFixture(t, &fakeEngine{man: testManifest(), logErr: errLogBoom})
	v2 := f2.env.Repo.(*walView)
	if _, err := v2.PushHistory(ctx, id, 5); !errors.Is(err, errLogBoom) {
		t.Fatalf("PushHistory log error = %v", err)
	}
	if _, _, err := v2.logEntries(ctx, id, proto.EntryKindSettings); !errors.Is(err, errLogBoom) {
		t.Fatalf("logEntries log error = %v", err)
	}

}

// --- settings arms (§11) -------------------------------------------------------------------

func TestSettingsArms(t *testing.T) {
	// Body over the 16 KiB budget → 413.
	f := newFixture(t)
	if w := f.req("PUT", "/demo/walgit/api/settings", strings.NewReader(strings.Repeat("x", 17<<10))); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized settings = %d", w.Code)
	}

	// Engine manifest error → 503 on the effective/describe renders.
	f2 := newEngineFixture(t, &fakeEngine{manErr: errBoom})
	if w := f2.do("GET", "/demo/walgit/api/settings/effective"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings/effective manifest error = %d", w.Code)
	}
	if w := f2.do("GET", "/demo/walgit/api/settings/describe"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings/describe manifest error = %d", w.Code)
	}

	// A stored TOML that fails to parse → 503 from the effective-config merge.
	bad := testManifest()
	bad.Settings.Toml = "not [valid"
	f3 := newEngineFixture(t, &fakeEngine{man: bad})
	if w := f3.do("GET", "/demo/walgit/api/settings/effective"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings/effective bad TOML = %d (%s)", w.Code, w.Body.String())
	}
	if w := f3.do("GET", "/demo/walgit/api/settings/describe"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings/describe bad TOML = %d", w.Code)
	}

	// settings/validate: an invalid body reports ok=false, and a body that
	// parses but fails host validation does too.
	f4 := newFixture(t)
	w := f4.req("POST", "/demo/walgit/api/settings/validate", strings.NewReader("not [valid"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("invalid TOML validate = %d (%s)", w.Code, w.Body.String())
	}
	w = f4.req("POST", "/demo/walgit/api/settings/validate",
		strings.NewReader("[[bundles.strategy]]\nname = \"x\"\nkind = \"bogus\"\n"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("invalid strategy validate = %d (%s)", w.Code, w.Body.String())
	}

	// Helpers: hostname list misses, nil diffs, scalar strings.
	if listMatches([]string{"host-a"}, "host-b") {
		t.Fatal("listMatches must miss an unlisted host")
	}
	if !listMatches([]string{"host-a"}, "host-a") || !listMatches([]string{"*"}, "host-b") {
		t.Fatal("listMatches must hit exact host or wildcard")
	}
	if got := diffFields(nil, nil); len(got) != 0 {
		t.Fatalf("diffFields(nil,nil) = %v", got)
	}
	if got := scalarValue(reflect.ValueOf("ssd"), reflect.TypeOf("")); got != "ssd" {
		t.Fatalf("scalarValue(string) = %v", got)
	}
}
