// http_test.go — the Seam 1 surface: routing, authz, CRUD, Sync-now,
// and the create-from-URL twin.
package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

func testHandler(t *testing.T, svc *Service, reg *wal.Registry, p auth.Principal) *Handler {
	t.Helper()
	return &Handler{
		Svc:  svc,
		Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) { return p, nil },
		CreateRepo: func(ctx context.Context, owner, name string) error {
			_, err := reg.Create(ctx, owner+"/"+name, git.Sha1)
			return err
		},
		AuthMode: func() string { return "none" },
		Now:      time.Now,
	}
}

func doHandle(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	if !h.Handle(rec, req) {
		rec.Code = 404
	}
	return rec
}

var (
	adminP = auth.Principal{Name: "root", Write: true, Admin: true}
	writeP = auth.Principal{Name: "dev", Write: true}
	anonP  = auth.Anonymous()
)

func TestRouting(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	// Non-mirror paths are not ours.
	for _, path := range []string{"/acme/m/api/issues", "/api/v1/repos", "/acme/m/api/mirrorx", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if h.Handle(httptest.NewRecorder(), req) {
			t.Fatalf("handled %q", path)
		}
	}
	// Bad repo id is not ours.
	req := httptest.NewRequest(http.MethodGet, "/..%2f/api/mirror", nil)
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatal("handled bad repo id")
	}
	// Method-not-allowed on known shapes.
	if rec := doHandle(h, http.MethodPost, "/acme/m/api/mirror", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST mirror = %d", rec.Code)
	}
	if rec := doHandle(h, http.MethodGet, "/api/v1/repos/mirrors", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET twin = %d", rec.Code)
	}
	// ServeHTTP 404s otherwise.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("servehttp = %d", rec.Code)
	}
	_ = ctx
}

func TestGetMirror(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, anonP) // GET is open (no secrets in the sidecar)
	if rec := doHandle(h, http.MethodGet, "/acme/m/api/mirror", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("absent = %d", rec.Code)
	}
	ctx := context.Background()
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	rec := doHandle(h, http.MethodGet, "/acme/m/api-browser/mirror", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var v View
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.UpstreamURL != "https://example.com/a.git" || v.Schedule != PresetDaily || !v.Due {
		t.Fatalf("view = %+v", v)
	}
	// Nil service → 503.
	h.Svc = nil
	if rec := doHandle(h, http.MethodGet, "/acme/m/api/mirror", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc = %d", rec.Code)
	}
}

func TestPutAuthAndValidation(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// Anonymous → 401; writer → 403.
	if rec := doHandle(testHandler(t, svc, reg, anonP), http.MethodPut, "/acme/m/api/mirror", `{"schedule":"daily"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon = %d", rec.Code)
	}
	if rec := doHandle(testHandler(t, svc, reg, writeP), http.MethodPut, "/acme/m/api/mirror", `{"schedule":"daily"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("writer = %d", rec.Code)
	}
	h := testHandler(t, svc, reg, adminP)
	if rec := doHandle(h, http.MethodPut, "/acme/m/api/mirror", `{"schedule":"never"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad preset = %d", rec.Code)
	}
	if rec := doHandle(h, http.MethodPut, "/acme/m/api/mirror", `{"bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := doHandle(h, http.MethodPut, "/acme/m/api/mirror", `{"schedule":"daily"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing upstream = %d: %s", rec.Code, rec.Body.String())
	}
	// Nil service → 503 (after auth).
	hn := testHandler(t, nil, reg, adminP)
	if rec := doHandle(hn, http.MethodPut, "/acme/m/api/mirror", `{"schedule":"daily"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc = %d", rec.Code)
	}
}

func TestPutCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	up := initUpstream(t)
	// Create via PUT (repo must exist first — PUT is config on a repo).
	if _, err := reg.Create(ctx, "acme/m", git.Sha1); err != nil {
		t.Fatal(err)
	}
	rec := doHandle(h, http.MethodPut, "/acme/m/api/mirror", `{"upstream_url":`+quote(mustNormalize(t, "file://"+up))+`,"schedule":"hourly"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("put create = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	task, _ := out["task"].(map[string]any)
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatalf("no task id: %s", rec.Body.String())
	}
	// Wait for the background first sync.
	waitAsync(t, svc, id, 30*time.Second)
	doc, _, _ := Load(ctx, st, "acme", "m")
	if doc == nil || doc.Schedule != PresetHourly {
		t.Fatalf("doc = %+v", doc)
	}
	// Schedule update → 200 (no new sync spawned).
	rec = doHandle(h, http.MethodPut, "/acme/m/api/mirror", `{"schedule":"daily"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put update = %d: %s", rec.Code, rec.Body.String())
	}
	// Upstream change → 409.
	rec = doHandle(h, http.MethodPut, "/acme/m/api/mirror", `{"upstream_url":"https://example.com/other.git","schedule":"daily"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("upstream change = %d", rec.Code)
	}
	// Duplicate PUT-create on the same sidecar path is an update, not a conflict —
	// but a raced sidecar (created between Load and Create) → 409 already-a-mirror.
	// Covered via Create directly: covered in mirror_test.
}

func TestPutCreateUnbornRepo404(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	up := initUpstream(t)
	n := mustNormalize(t, "file://"+up)
	// PUT-create on a repo that was never created → 404 (PUT is config
	// on a repo; only the create twin creates repos). No sidecar may
	// be left behind to 403 the name's future pushes.
	rec := doHandle(h, http.MethodPut, "/acme/ghost/api/mirror", `{"upstream_url":`+quote(n)+`,"schedule":"daily"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unborn put = %d: %s", rec.Code, rec.Body.String())
	}
	if doc, _, _ := Load(ctx, st, "acme", "ghost"); doc != nil {
		t.Fatalf("orphan sidecar: %+v", doc)
	}
}

func TestDeleteMirror(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if rec := doHandle(testHandler(t, svc, reg, anonP), http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon = %d", rec.Code)
	}
	h := testHandler(t, svc, reg, adminP)
	if rec := doHandle(h, http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("absent = %d", rec.Code)
	}
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	if rec := doHandle(h, http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if IsMirror(ctx, st, "acme", "m") {
		t.Fatal("still a mirror")
	}
}

func TestSyncNowHTTP(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if rec := doHandle(testHandler(t, svc, reg, anonP), http.MethodPost, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon = %d", rec.Code)
	}
	if rec := doHandle(testHandler(t, svc, reg, writeP), http.MethodPost, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("writer = %d", rec.Code)
	}
	h := testHandler(t, svc, reg, adminP)
	// Non-mirror → 404.
	if rec := doHandle(h, http.MethodPost, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-mirror = %d", rec.Code)
	}
	if rec := doHandle(h, http.MethodPost, "/acme/m/api/mirror/sync", `{"bogus":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	up := initUpstream(t)
	n := mustNormalize(t, "file://"+up)
	if _, err := reg.Create(ctx, "acme/m", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, st, "acme", "m", n, PresetDaily); err != nil {
		t.Fatal(err)
	}
	rec := doHandle(h, http.MethodPost, "/acme/m/api/mirror/sync", `{"force":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sync = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	id := out["task"].(map[string]any)["id"].(string)
	waitAsync(t, svc, id, 30*time.Second)
	// Status by id (done) + list shape.
	rec = doHandle(h, http.MethodGet, "/acme/m/api/mirror/sync?id="+id, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doHandle(h, http.MethodGet, "/acme/m/api/mirror/sync", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "recent") {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doHandle(h, http.MethodGet, "/acme/m/api/mirror/sync?id=nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", rec.Code)
	}
	hn := testHandler(t, nil, reg, adminP)
	if rec := doHandle(hn, http.MethodPost, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc post = %d", rec.Code)
	}
	if rec := doHandle(hn, http.MethodGet, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc get = %d", rec.Code)
	}
}

func TestCreateFromURL(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	anon := testHandler(t, svc, reg, anonP)
	body := `{"source_url":"https://example.com/a.git","owner":"acme","name":"n1"}`
	if rec := doHandle(anon, http.MethodPost, "/api/v1/repos/mirrors", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon = %d", rec.Code)
	}
	writer := testHandler(t, svc, reg, writeP)
	if rec := doHandle(writer, http.MethodPost, "/api/v1/repos/mirrors", `{"owner":"acme"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing = %d", rec.Code)
	}
	if rec := doHandle(writer, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":"https://example.com/a.git","owner":"..","name":"n"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad target = %d", rec.Code)
	}
	if rec := doHandle(writer, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":"https://example.com/a.git","owner":"acme","name":"n","schedule":"never"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad preset = %d", rec.Code)
	}
	if rec := doHandle(writer, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":"::bad::","owner":"acme","name":"n"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url = %d", rec.Code)
	}
	// Off-allowlist host + dangerous without authority (mode none) → 403.
	danger := `{"source_url":"https://example.com/a.git","owner":"acme","name":"n","dangerous":true}`
	if rec := doHandle(testHandler(t, svc, reg, adminP), http.MethodPost, "/api/v1/repos/mirrors", danger); rec.Code != http.StatusForbidden {
		t.Fatalf("dangerous = %d: %s", rec.Code, rec.Body.String())
	}
	// file:// twin on the api-browser lane: full flow.
	up := initUpstream(t)
	fileBody := `{"source_url":` + quote("file://"+up) + `,"owner":"acme","name":"m1"}`
	rec := doHandle(writer, http.MethodPost, "/api-browser/v1/repos/mirrors", fileBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	waitAsync(t, svc, out["task"].(map[string]any)["id"].(string), 30*time.Second)
	ctx := context.Background()
	doc, _, _ := Load(ctx, st, "acme", "m1")
	if doc == nil || doc.LastResult != "ok" {
		t.Fatalf("doc = %+v", doc)
	}
	// Duplicate → 409 (repo exists).
	if rec := doHandle(writer, http.MethodPost, "/api/v1/repos/mirrors", fileBody); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d: %s", rec.Code, rec.Body.String())
	}
	// Nil service → 503.
	hn := testHandler(t, nil, reg, writeP)
	hn.CreateRepo = nil
	if rec := doHandle(hn, http.MethodPost, "/api/v1/repos/mirrors", fileBody); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc = %d", rec.Code)
	}
}

func TestWritersAndShapes(t *testing.T) {
	// taskJSON nil + progress-carrying record.
	if got := taskJSON(nil); len(got) != 0 {
		t.Fatalf("nil task = %v", got)
	}
	ok := true
	rec := &wal.TaskRecord{ID: "x", Kind: KindMirrorSync, LogTail: nil, OK: &ok,
		Progress: &wal.Progress{Label: "ingest", Done: 1, Unit: "packs"},
		Params:   map[string]string{"a": "b"}}
	got := taskJSON(rec)
	if got["id"] != "x" || got["ok"] != true {
		t.Fatalf("task = %v", got)
	}
	rec2 := &wal.TaskRecord{ID: "y", Finished: "t"}
	if got := taskJSON(rec2); got["finished"] != "t" {
		t.Fatalf("task2 = %v", got)
	}
	// writePlain statuses.
	for _, tc := range []struct {
		status int
		header string
	}{
		{http.StatusUnauthorized, "WWW-Authenticate"},
		{http.StatusServiceUnavailable, "Retry-After"},
	} {
		rec := httptest.NewRecorder()
		writePlain(rec, tc.status, "m")
		if rec.Header().Get(tc.header) == "" {
			t.Fatalf("%d missing %s", tc.status, tc.header)
		}
	}
	// writeStatusErr: StatusError vs generic; writeAuthErr kinds.
	rw := httptest.NewRecorder()
	writeStatusErr(rw, &repoimport.StatusError{Status: 400, Message: "bad"})
	if rw.Code != 400 {
		t.Fatalf("status err = %d", rw.Code)
	}
	rw = httptest.NewRecorder()
	writeStatusErr(rw, fmt.Errorf("boom"))
	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("generic = %d", rw.Code)
	}
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrForbidden, http.StatusForbidden},
		{auth.ErrUnavailable, http.StatusServiceUnavailable},
		{auth.ErrInvalid, http.StatusUnauthorized},
	} {
		rw := httptest.NewRecorder()
		writeAuthErr(rw, &auth.AuthError{Kind: tc.kind, Why: "w"})
		if rw.Code != tc.want {
			t.Fatalf("kind %d = %d", tc.kind, rw.Code)
		}
	}
	// decodeStrict: empty ok, bad JSON, unknown field.
	var v struct {
		A string `json:"a"`
	}
	if err := decodeStrict(nil, &v); err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict([]byte("{oops"), &v); err == nil {
		t.Fatal("bad json ok")
	}
	if err := decodeStrict([]byte(`{"zzz":1}`), &v); err == nil {
		t.Fatal("unknown field ok")
	}
	// isRepoExists shapes.
	if isRepoExists(fmt.Errorf("nope")) || !isRepoExists(wal.ErrExists("x")) {
		t.Fatal("isRepoExists broken")
	}
	if !isExists(store.NewPrecondition("k", "v")) {
		t.Fatal("isExists broken")
	}
	// dangerousAllowed matrix.
	h := &Handler{AuthMode: func() string { return "token" }}
	if !h.dangerousAllowed(adminP) || h.dangerousAllowed(writeP) || h.dangerousAllowed(anonP) {
		t.Fatal("token-mode matrix broken")
	}
	h.AuthMode = func() string { return "none" }
	if h.dangerousAllowed(adminP) {
		t.Fatal("none-mode admin allowed")
	}
	h.AuthMode = nil
	if h.dangerousAllowed(adminP) {
		t.Fatal("nil-mode admin allowed")
	}
	// checkSSRF nil-service is permissive (no gate configured).
	hn := &Handler{}
	n2 := mustNormalized(t, "https://example.com/a.git")
	if err := hn.checkSSRF(n2, false); err != nil {
		t.Fatalf("nil check: %v", err)
	}
}

func mustNormalized(t *testing.T, raw string) repoimport.Normalized {
	t.Helper()
	n, err := repoimport.NormalizeSource(raw)
	if err != nil {
		t.Fatalf("normalize %q: %v", raw, err)
	}
	return n
}

// --- test helpers ------------------------------------------------------------

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustNormalize(t *testing.T, raw string) string {
	t.Helper()
	n, err := repoimport.NormalizeSource(raw)
	if err != nil {
		t.Fatalf("normalize %q: %v", raw, err)
	}
	return n.URL
}

func waitAsync(t *testing.T, svc *Service, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		a, ok := svc.SyncStatus(id)
		if !ok {
			t.Fatalf("async %q unknown", id)
		}
		select {
		case <-a.done:
			if a.err != nil {
				t.Fatalf("async %q: %v", id, a.err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("async %q never finished", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
