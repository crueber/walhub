package api

// gaps_test.go covers the handler branches the main suite leaves open: repo
// lifecycle, registry error paths, policy CRUD/eval branches, ops error arms,
// SSE envelope internals, and the small helpers.

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

var errBoom = errors.New("boom")

// --- repo lifecycle (PUT/DELETE lane root) ------------------------------------------

func TestRepoLifecycle(t *testing.T) {
	f := newFixture(t)
	p := auth.Principal{Name: "jane", Write: true, Admin: true}

	// PUT creates → 201 + identity body
	w := f.do("PUT", "/demo/walgit/api", nil, nil, &p)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]string
	decodeJSON(t, w, &created)
	if created["full_name"] != "demo/walgit" || created["owner"] != "demo" || created["name"] != "walgit" {
		t.Fatalf("created = %v", created)
	}
	// PUT again → 409
	if w = f.do("PUT", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusConflict {
		t.Fatalf("recreate = %d, want 409", w.Code)
	}
	// bad object_format → 400
	if w = f.do("PUT", "/demo/walgit/api?object_format=sha3", nil, nil, &p); w.Code != http.StatusBadRequest {
		t.Fatalf("bad format = %d, want 400", w.Code)
	}
	// write-less principal → 403
	if w = f.do("PUT", "/demo/walgit/api", nil, nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("readonly create = %d, want 403", w.Code)
	}
	// DELETE → 204, then DELETE again → 204 (idempotent at this layer)
	if w = f.do("DELETE", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	if w = f.do("DELETE", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusNoContent {
		t.Fatalf("re-delete = %d, want 204", w.Code)
	}
	// registry failure → 503
	f.reg.fail = errBoom
	if w = f.do("PUT", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusCreated {
		t.Fatalf("create with failed Owners = %d, want 201 (Create ignores fail)", w.Code)
	}
	if w = f.do("DELETE", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed delete = %d, want 503", w.Code)
	}
	f.reg.fail = nil
	// no registry wired → 503 on both lifecycle verbs
	f.env.Repos = nil
	if w = f.do("PUT", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry create = %d", w.Code)
	}
	if w = f.do("DELETE", "/demo/walgit/api", nil, nil, &p); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry delete = %d", w.Code)
	}
}

// --- registry error paths on discovery ------------------------------------------------

func TestDiscoveryRegistryErrors(t *testing.T) {
	f := newFixture(t)
	f.reg.fail = errBoom
	if w := f.do("GET", "/api/v1/owners", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("owners = %d, want 503", w.Code)
	}
	if w := f.do("GET", "/api/v1/owners/demo/repos", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ownerRepos = %d, want 503", w.Code)
	}
	f.reg.fail = nil
	f.env.Repos = nil
	if w := f.do("GET", "/api/v1/owners", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil registry owners = %d, want 503", w.Code)
	}
}

// --- summary / overview / object-render error arms -------------------------------------

func TestViewErrorArms404(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/demo/walgit/api", // no summary seeded
		"/demo/walgit/api/overview",
		"/demo/walgit/api/resolve/main",
		"/demo/walgit/api/tree/main",
		"/demo/walgit/api/commits",
		"/demo/walgit/api/commit/abc",
	} {
		if w := f.req("GET", path); w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 (body %s)", path, w.Code, w.Body.String())
		}
	}
	// unknown namespace → 404
	if w := f.req("GET", "/demo/walgit/api/refs/notes"); w.Code != http.StatusNotFound {
		t.Fatalf("bad ns = %d", w.Code)
	}
	// invalid n → 400
	if w := f.req("GET", "/demo/walgit/api/refs/branches?n=abc"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad n = %d", w.Code)
	}
	// n above the cap is clamped, not rejected
	if w := f.req("GET", "/demo/walgit/api/refs/branches?n=5000"); w.Code != http.StatusOK {
		t.Fatalf("big n = %d", w.Code)
	}
}

func TestSummaryWithoutHead(t *testing.T) {
	f := newFixture(t)
	f.view.summaries["demo/walgit"] = SummaryData{Branches: 1}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if etag := w.Header().Get("ETag"); etag != "" {
		t.Fatalf("unborn head must omit etag, got %q", etag)
	}
}

// --- blob: raw download + missing path --------------------------------------------------

func TestBlobRawAndGuards(t *testing.T) {
	f := newFixture(t)
	f.view.resolves["demo/walgit/"+fakeSHA] = Resolution{Ref: "", SHA: fakeSHA, Kind: "commit", Revision: 7}
	f.view.blobRaw["demo/walgit|"+fakeSHA+"|hi.txt"] = []byte("raw bytes")
	f.view.blobs["demo/walgit|"+fakeSHA+"|hi.txt"] = BlobResult{Contents: []byte("hello"), Size: 5}

	// JSON render via the sha-addressed cache class
	w := f.req("GET", "/demo/walgit/api/blob/"+fakeSHA+"/hi.txt")
	if w.Code != 200 {
		t.Fatalf("blob = %d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccImmutable {
		t.Fatalf("sha blob cache = %q", cc)
	}
	var body blobBody
	decodeJSON(t, w, &body)
	if body.Name != "hi.txt" || body.Contents != "hello" {
		t.Fatalf("blob body = %+v", body)
	}
	// ?raw download
	w = f.req("GET", "/demo/walgit/api/blob/"+fakeSHA+"/hi.txt?raw")
	if w.Code != 200 || w.Body.String() != "raw bytes" {
		t.Fatalf("raw = %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("raw content-type = %q", ct)
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("raw etag = %q", etag)
	}
	// ?raw on a missing blob → 404
	if w = f.req("GET", "/demo/walgit/api/blob/main/absent.txt?raw"); w.Code != http.StatusNotFound {
		t.Fatalf("raw missing = %d", w.Code)
	}
	// missing path (route-level empty path) → 404 guard
	h := &handlers{env: f.env}
	r := httptest.NewRequest("GET", "/blob/main/", nil)
	r.SetPathValue("rev", "main")
	r.SetPathValue("path", "")
	w2 := httptest.NewRecorder()
	h.blob(w2, r)
	if w2.Code != http.StatusNotFound || w2.Body.String() != "blob requires a path" {
		t.Fatalf("empty path = %d %q", w2.Code, w2.Body.String())
	}
}

// --- policy CRUD + eval branches ---------------------------------------------------------

func putPolicyRaw(t *testing.T, f *fixture, body string) {
	t.Helper()
	_, err := f.env.Store.Put(t.Context(), policyKey(git.RepoId{Owner: "demo", Name: "walgit"}),
		store.PutBody{Bytes: []byte(body)}, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPolicyCorruptDoc503(t *testing.T) {
	f := newFixture(t)
	putPolicyRaw(t, f, `{"version":1,"rules":[{"name":"x","effect":{"protect":{"restricts":["bogus"]}}}]}`)
	if w := f.req("GET", "/demo/walgit/api/policy"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt GET = %d, want 503 (fail closed)", w.Code)
	}
	// validate with empty body falls back to the stored (corrupt) policy
	w := f.req("POST", "/demo/walgit/api/policy/validate", strings.NewReader(""))
	var v struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	decodeJSON(t, w, &v)
	if v.OK || len(v.Errors) == 0 {
		t.Fatalf("validate on corrupt = %+v", v)
	}
	// dry-run on a corrupt candidate → 400
	if w = f.req("POST", "/demo/walgit/api/policy/dry-run", strings.NewReader(`{"version":0}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("dry-run bad policy = %d", w.Code)
	}
}

func TestPolicyPutCreateUpdateDelete(t *testing.T) {
	f := newFixture(t)
	doc := `{"version":1,"groups":[],"rules":[]}`
	// create
	w := f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(doc))
	if w.Code != 200 || w.Body.String() != doc {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}
	// update (version present → PutUpdate)
	w = f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(doc))
	if w.Code != 200 {
		t.Fatalf("update = %d body=%s", w.Code, w.Body.String())
	}
	// invalid body → 400, nothing written
	if w = f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(`{"version":1,"rules":[{"effect":{}}]}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT = %d body=%s", w.Code, w.Body.String())
	}
	// read-only principal → 403
	if w = f.do("PUT", "/demo/walgit/api/policy", strings.NewReader(doc), nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("readonly PUT = %d", w.Code)
	}
	// delete → 204; deleting a missing policy is still 204
	if w = f.req("DELETE", "/demo/walgit/api/policy"); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", w.Code)
	}
	if w = f.req("DELETE", "/demo/walgit/api/policy"); w.Code != http.StatusNoContent {
		t.Fatalf("re-delete = %d", w.Code)
	}
}

func TestPolicyValidateEmptyBodyUsesStored(t *testing.T) {
	f := newFixture(t)
	putPolicyRaw(t, f, `{"version":1,"rules":[{"name":"protect-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"]}}}]}`)
	w := f.req("POST", "/demo/walgit/api/policy/validate", strings.NewReader(""))
	var v struct {
		OK      bool `json:"ok"`
		Rules   int  `json:"rules"`
		Protect []struct {
			Rule string `json:"rule"`
		} `json:"protect"`
	}
	decodeJSON(t, w, &v)
	if !v.OK || v.Rules != 1 || len(v.Protect) != 1 || v.Protect[0].Rule != "protect-main" {
		t.Fatalf("validate stored = %+v", v)
	}
}

func TestEvalPushRefTable(t *testing.T) {
	doc, err := policy.Parse([]byte(`{"version":1,"groups":[],"rules":[{"name":"protect-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["force-push"]}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	cases := []struct {
		ref  PushRef
		want bool
		op   string
	}{
		{PushRef{Name: "refs/heads/main", Old: zero, New: fakeSHA}, true, "create"},
		{PushRef{Name: "refs/heads/main", Old: fakeSHA, New: zero}, true, "delete"},
		{PushRef{Name: "refs/heads/main", Old: fakeSHA, New: fakeSHA2}, true, "update"},
		{PushRef{Name: "refs/heads/main", Old: fakeSHA, New: fakeSHA2, Force: true}, false, "force-push"},
		{PushRef{Name: "refs/heads/dev", Old: fakeSHA, New: fakeSHA2, Force: true}, true, "update"},
	}
	for _, c := range cases {
		ok, reason := evalPushRef(doc, "jane", c.ref)
		if ok != c.want {
			t.Fatalf("%+v → %v (%s)", c.ref, ok, reason)
		}
	}
	if !isZeroHex("") || !isZeroHex(zero) || isZeroHex(fakeSHA) {
		t.Fatal("isZeroHex broken")
	}
	// empty policy = allow-all
	ok, _ := evalPushRef(nil, "jane", PushRef{Name: "refs/heads/main", Old: fakeSHA, New: fakeSHA2, Force: true})
	if !ok {
		t.Fatal("nil policy must allow")
	}
}

// --- ops + tasks branches ----------------------------------------------------------------

// opsOnly overrides the frozen op table (fakeTasks.Ops() always returns nil).
type opsTasks struct {
	fakeTasks
	ops []OpSpec
}

func (f *opsTasks) Ops() []OpSpec { return f.ops }

func streamWithOutcome(rec TaskRecord, done TaskDone) TaskStream {
	upd := make(chan Progress)
	close(upd)
	dch := make(chan TaskDone, 1)
	dch <- done
	return TaskStream{Record: rec, Updates: upd, Done: dch}
}

func TestOpsListAndStartBranches(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.tasks.listErr = errBoom // ops list still renders; recent falls back to []
	w := f.req("GET", "/demo/walgit/api/ops")
	if w.Code != 200 {
		t.Fatalf("ops = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Available []OpSpec  `json:"available"`
		Recent    []TaskRec `json:"recent"`
	}
	decodeJSON(t, w, &body)
	_ = body

	// custom op table flows through
	ot := &opsTasks{fakeTasks: *f.tasks, ops: []OpSpec{{Op: "custom", Params: []OpParam{{Name: "p"}}}}}
	f.env.Tasks = ot
	f.tasks.records = map[string]TaskRecord{}
	w = f.req("GET", "/demo/walgit/api/ops")
	var body2 struct {
		Available []OpSpec `json:"available"`
	}
	decodeJSON(t, w, &body2)
	if len(body2.Available) != 1 || body2.Available[0].Op != "custom" {
		t.Fatalf("custom ops = %+v", body2.Available)
	}

	// unknown op → 404
	if w = f.req("POST", "/demo/walgit/api/ops/nope"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown op = %d", w.Code)
	}
	// bare param (required) missing → 400
	if w = f.req("POST", "/demo/walgit/api/ops/bundle"); w.Code != http.StatusBadRequest {
		t.Fatalf("missing param = %d body=%s", w.Code, w.Body.String())
	}
	// disallowed value → 400
	if w = f.req("POST", "/demo/walgit/api/ops/compact?force=2"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid param = %d", w.Code)
	}
	// valid op with a completed stream → SSE with result
	rec := TaskRecord{ID: "t1", Kind: "fsck", Repo: "demo/walgit", Hostname: "host-a"}
	f.tasks.streams["op:fsck"] = streamWithOutcome(rec, TaskDone{Record: rec, Value: "ok"})
	w = f.do("POST", "/demo/walgit/api/ops/fsck?connectivity=1", nil, map[string]string{"Accept": "text/event-stream"}, &auth.Principal{Name: "jane", Write: true})
	if w.Code != 200 {
		t.Fatalf("fsck start = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `event: result`) || !strings.Contains(w.Body.String(), `"value":"ok"`) {
		t.Fatalf("fsck stream = %s", w.Body.String())
	}
}

type TaskRec = TaskRecord

func TestTasksBranches(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)

	// list error → 503
	f.tasks.listErr = errBoom
	if w := f.req("GET", "/demo/walgit/api/tasks"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("list err = %d", w.Code)
	}
	f.tasks.listErr = nil

	// unknown task → 404
	if w := f.req("GET", "/demo/walgit/api/tasks/ghost"); w.Code != http.StatusNotFound {
		t.Fatalf("ghost = %d", w.Code)
	}
	// known record → 200
	ok := true
	rec := taskRecordFrom("t1", "fsck", "demo/walgit", "host-a", 1700000000000, &ok, "done", nil, map[string]string{"k": "v"})
	f.tasks.records["t1"] = rec
	w := f.req("GET", "/demo/walgit/api/tasks/t1")
	if w.Code != 200 {
		t.Fatalf("get = %d", w.Code)
	}
	var got TaskRecord
	decodeJSON(t, w, &got)
	if got.ID != "t1" || got.LogTail == nil || len(got.LogTail) != 0 {
		t.Fatalf("record = %+v (log tail must serialize as [])", got)
	}
	// SSE attach: known → envelope with task + result; unknown → 404
	f.tasks.streams["t1"] = streamWithOutcome(rec, TaskDone{Record: rec, Value: "v"})
	hdr := map[string]string{"Accept": "text/event-stream"}
	p := &auth.Principal{Name: "jane"}
	w = f.do("GET", "/demo/walgit/api/tasks/t1", nil, hdr, p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `event: task`) || !strings.Contains(w.Body.String(), `event: result`) {
		t.Fatalf("attach = %d %s", w.Code, w.Body.String())
	}
	if w = f.do("GET", "/demo/walgit/api/tasks/ghost", nil, hdr, p); w.Code != http.StatusNotFound {
		t.Fatalf("ghost attach = %d", w.Code)
	}
}

// --- SSE envelope internals ---------------------------------------------------------------

func TestSSECommentAndPacketJSON(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	s, ok := NewSSE(w, r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer s.Close()
	if !strings.Contains(w.Body.String(), ": walgit") {
		t.Fatalf("opener = %q", w.Body.String())
	}
	if !s.comment(": keepalive") {
		t.Fatal("comment failed")
	}
	if !strings.Contains(w.Body.String(), ": keepalive") {
		t.Fatalf("comment body = %q", w.Body.String())
	}
	// terminal-once still applies to comments
	s.Event("result", "{}")
	if s.comment(": again") {
		t.Fatal("comment after terminal must refuse")
	}

	if got := packetJSON(Progress{Kind: "notice", Text: "hi"}); got != `{"text":"hi"}` {
		t.Fatalf("notice = %s", got)
	}
	if got := packetJSON(Progress{Kind: "task"}); got != "{}" {
		t.Fatalf("nil task = %s", got)
	}
	total := uint64(4)
	if got := packetJSON(Progress{Kind: "progress", Label: "lb", Done: 1, Total: &total, Unit: "objs"}); !strings.Contains(got, `"label":"lb"`) || !strings.Contains(got, `"total":4`) {
		t.Fatalf("progress = %s", got)
	}
}

func TestSSEPumpFlows(t *testing.T) {
	newPump := func(st TaskStream) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/x", nil)
		s, _ := NewSSE(w, r)
		s.pump(st)
		s.Close()
		return w
	}
	// replay + live + result
	upd := make(chan Progress, 2)
	upd <- Progress{Kind: "notice", Text: "a"}
	upd <- Progress{Kind: "notice", Text: "b"}
	close(upd)
	rec := TaskRecord{ID: "t", Kind: "gc", Repo: "demo/walgit", Hostname: "h"}
	dch := make(chan TaskDone, 1)
	dch <- TaskDone{Record: rec, Value: "v"}
	w := newPump(TaskStream{Record: rec, Replay: []Progress{{Kind: "notice", Text: "r"}}, Updates: upd, Done: dch})
	for _, want := range []string{`event: task`, `event: notice`, `event: result`, `"value":"v"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("pump missing %s: %s", want, w.Body.String())
		}
	}
	// other-host task → terminal 409 error, no live phase
	w = newPump(TaskStream{Record: rec, Other: &TaskRecord{ID: "o", Kind: "gc", Hostname: "host-b"}})
	if !strings.Contains(w.Body.String(), `"status":409`) || !strings.Contains(w.Body.String(), "host-b") {
		t.Fatalf("other pump = %s", w.Body.String())
	}
	// error outcome
	upd2 := make(chan Progress)
	close(upd2)
	dch2 := make(chan TaskDone, 1)
	dch2 <- TaskDone{Err: &TaskErr{Status: 500, Message: "kaboom"}}
	w = newPump(TaskStream{Record: rec, Updates: upd2, Done: dch2})
	if !strings.Contains(w.Body.String(), `"status":500`) || !strings.Contains(w.Body.String(), "kaboom") {
		t.Fatalf("err pump = %s", w.Body.String())
	}
	// closed done channel (publisher vanished) → clean return
	upd3 := make(chan Progress)
	close(upd3)
	dch3 := make(chan TaskDone)
	close(dch3)
	w = newPump(TaskStream{Record: rec, Updates: upd3, Done: dch3})
	if !strings.Contains(w.Body.String(), `event: task`) {
		t.Fatalf("closed pump = %s", w.Body.String())
	}
	s, _ := NewSSE(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	s.Event("result", "{}")
	if s.Event("notice", "{}") {
		t.Fatal("post-terminal Event must fail")
	}
	_ = w
}

// w4w builds a fresh recorder for the SSE-reuse check above.
func w4w() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func TestNewIDShape(t *testing.T) {
	id := newID()
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("newID = %q", id)
	}
	if id2 := newID(); id2 == id {
		t.Fatal("ids must be unique")
	}
}

// --- env helpers ---------------------------------------------------------------------------

func TestPrincipalOfModes(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest("GET", "/x", nil)
	// no principal + token mode → anonymous (read-only, never write)
	p := f.env.PrincipalOf(r)
	if !p.Anonymous || p.Write || p.Admin {
		t.Fatalf("anon default = %+v", p)
	}
	// mode none → full access
	f.env.Cfg.Server.Auth.Mode = "none"
	p = f.env.PrincipalOf(r)
	if !p.Write || !p.Admin {
		t.Fatalf("mode none = %+v", p)
	}
	// injected principal wins
	r = r.WithContext(WithPrincipal(r.Context(), auth.Principal{Name: "jane", Write: true}))
	if p := f.env.PrincipalOf(r); p.Name != "jane" {
		t.Fatalf("injected = %+v", p)
	}
}

func TestGateArms(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = false
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	if f.env.gate(w, r, AuthRead) {
		t.Fatal("anonymous read must be refused")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
	// authenticated but insufficient
	f.env.Cfg.Server.Auth.AnonymousRead = true
	for _, tc := range []struct {
		level AuthLevel
		p     auth.Principal
		code  int
	}{
		{AuthWrite, auth.Principal{Name: "j"}, http.StatusForbidden},
		{AuthAdmin, auth.Principal{Name: "j", Write: true}, http.StatusForbidden},
	} {
		w2 := httptest.NewRecorder()
		r2 := r.WithContext(WithPrincipal(r.Context(), tc.p))
		if f.env.gate(w2, r2, tc.level) {
			t.Fatalf("level %d must refuse %+v", tc.level, tc.p)
		}
		if w2.Code != tc.code {
			t.Fatalf("level %d status = %d, want %d", tc.level, w2.Code, tc.code)
		}
	}
	// AuthOpen always passes
	if !f.env.gate(httptest.NewRecorder(), r, AuthOpen) {
		t.Fatal("AuthOpen must always pass")
	}
}

func TestWriteJSONEncodeError(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, map[string]any{"bad": func() {}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("encode error = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("encode error body must be plain text, got %q", ct)
	}
}

func TestMapViewErrArms(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{ErrNotFound, http.StatusNotFound},
		{errWrapNotFound(), http.StatusNotFound},
		{ErrExists, http.StatusConflict},
		{errBoom, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		mapViewErr(w, c.err)
		if w.Code != c.code {
			t.Fatalf("%v → %d, want %d", c.err, w.Code, c.code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("errors must be plain text, got %q", ct)
		}
	}
}

func errWrapNotFound() error {
	return errWrap(ErrNotFound)
}

func errWrap(err error) error { return &wrapErr{err} }

type wrapErr struct{ e error }

func (w *wrapErr) Error() string { return "wrapped: " + w.e.Error() }
func (w *wrapErr) Unwrap() error { return w.e }

func TestRawSegmentsAndIsUTF8(t *testing.T) {
	r := httptest.NewRequest("GET", "/a%20b/c%2Fd", nil)
	got := rawSegments(r)
	if len(got) != 2 || got[0] != "a b" || got[1] != "c/d" {
		t.Fatalf("segments = %v", got)
	}
	// invalid escape falls back to the raw segment (built by hand —
	// NewRequest rejects unparseable targets)
	r.URL = &url.URL{Path: "/a%zz/b"}
	r.URL.RawPath = "/a%zz/b"
	if segs := rawSegments(r); segs[0] != "a%zz" {
		t.Fatalf("bad escape = %v", segs)
	}
	if !isUTF8([]byte("héllo")) || isUTF8([]byte{0xff, 0xfe}) || isUTF8([]byte("a\x00b")) {
		t.Fatal("isUTF8 broken")
	}
}

// --- small pure helpers -------------------------------------------------------------------

func TestSmallHelpers(t *testing.T) {
	if kindOf("refs/heads/main") != "branch" || kindOf("refs/tags/v1") != "tag" || kindOf("refs/notes/x") != "commit" {
		t.Fatal("kindOf broken")
	}
	if baseName("a/b/c.txt") != "c.txt" || baseName("plain") != "plain" || baseName("a/b/") != "" {
		t.Fatal("baseName broken")
	}
	if filepathExt("README.md") != ".md" || filepathExt("README") != "" {
		t.Fatal("filepathExt broken")
	}
	if !isBinary([]byte("ab\x00cd")) || isBinary([]byte(strings.Repeat("a", 9000))) {
		t.Fatal("isBinary broken")
	}
	if !revIsFullSHA(fakeSHA) || revIsFullSHA("short") {
		t.Fatal("revIsFullSHA broken")
	}
	if paramName("{name}") != "name" || paramName("{rest...}") != "rest" || paramName("literal") != "literal" {
		t.Fatal("paramName broken")
	}
	if !contains([]string{"a", "b"}, "b") || contains([]string{"a"}, "b") {
		t.Fatal("contains broken")
	}
	// LaneOf reflects the injected lane
	h := &handlers{env: newFixture(t).env}
	r := httptest.NewRequest("GET", "/demo/walgit/api", nil)
	ri := inject(r, LaneAPI, git.RepoId{Owner: "demo", Name: "walgit"}, map[string]string{})
	if LaneOf(ri) != LaneAPI {
		t.Fatalf("LaneOf = %v", LaneOf(ri))
	}
	_ = h
}

// --- settings describe/validate extra arms -------------------------------------------------

func TestSettingsValidateArms(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	// invalid TOML section → ok:false with a reason, still 200
	w := f.req("POST", "/demo/walgit/api/settings/validate", strings.NewReader("[server]\nlisten=\"x\"\n"))
	var bad struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	decodeJSON(t, w, &bad)
	if bad.OK || len(bad.Errors) == 0 {
		t.Fatalf("invalid validate = %+v", bad)
	}
	// valid body → ok:true with the merged shape
	w = f.req("POST", "/demo/walgit/api/settings/validate", strings.NewReader("[compaction]\ntrigger_packs = 9\n"))
	var good struct {
		OK         bool `json:"ok"`
		Compaction struct {
			TriggerPacks int `json:"trigger_packs"`
		} `json:"compaction"`
	}
	decodeJSON(t, w, &good)
	if !good.OK || good.Compaction.TriggerPacks == 0 {
		t.Fatalf("valid validate = %+v", good)
	}
	// placement excludes flip this_host.serves/maintains
	f.env.Cfg.Server.PublicURL = ""
	f.env.Cfg.Placement.ServeExclude = []string{"host-a"}
	w = f.req("GET", "/demo/walgit/api/settings/describe")
	var d struct {
		Maintenance struct {
			ThisHost struct {
				Serves    bool `json:"serves"`
				Maintains bool `json:"maintains"`
			} `json:"this_host"`
		} `json:"maintenance"`
		Upstream struct {
			TokenEnv bool     `json:"token_env"`
			Follow   []string `json:"follow"`
		} `json:"upstream"`
	}
	decodeJSON(t, w, &d)
	if d.Maintenance.ThisHost.Serves {
		t.Fatalf("excluded host must not serve: %+v", d.Maintenance)
	}
	if d.Upstream.Follow == nil {
		t.Fatal("follow must serialize as []")
	}
	// broken published TOML → effective/describe answer 503 (merge fails)
	f.view.published["demo/walgit"] = []byte("[compaction]\ntrigger_packs = \"not-a-number\"\n")
	if w = f.req("GET", "/demo/walgit/api/settings/effective"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken effective = %d body=%s", w.Code, w.Body.String())
	}
	if w = f.req("GET", "/demo/walgit/api/settings/describe"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken describe = %d", w.Code)
	}
}

// --- misc handler arms -----------------------------------------------------------------

func TestCommitsAndCommitQueryArms(t *testing.T) {
	f := newFixture(t)
	f.view.resolves["demo/walgit/main"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Kind: "branch", Revision: 7}
	f.view.commitPg["demo/walgit|"+fakeSHA+"||0|2"] = CommitPage{Commits: []Commit{{SHA: fakeSHA}}, More: true}
	w := f.req("GET", "/demo/walgit/api/commits?ref=main&n=2")
	if w.Code != 200 {
		t.Fatalf("commits = %d body=%s", w.Code, w.Body.String())
	}
	var page struct {
		Commits []map[string]any `json:"commits"`
		More    bool             `json:"more"`
	}
	decodeJSON(t, w, &page)
	if !page.More || len(page.Commits) != 1 {
		t.Fatalf("page = %+v", page)
	}
	// skip param
	f.view.commitPg["demo/walgit|"+fakeSHA+"||5|2"] = CommitPage{Commits: []Commit{}}
	if w = f.req("GET", "/demo/walgit/api/commits?ref=main&skip=5&n=2"); w.Code != 200 {
		t.Fatalf("skip commits = %d", w.Code)
	}
	// path filter
	f.view.commitPg["demo/walgit|"+fakeSHA+"|docs|0|2"] = CommitPage{Commits: []Commit{}, More: false}
	if w = f.req("GET", "/demo/walgit/api/commits?ref=main&path=docs&n=2"); w.Code != 200 {
		t.Fatalf("path commits = %d", w.Code)
	}
	// commit detail with stats + patch
	f.view.commits["demo/walgit|"+fakeSHA] = CommitDetail{
		Commit: Commit{SHA: fakeSHA, Trailers: []Trailer{}},
		Stats:  []Stat{{Path: "a", Additions: 1, Deletions: -1}},
		Patch:  "diff",
	}
	f.view.resolves["demo/walgit/"+fakeSHA] = Resolution{SHA: fakeSHA, Kind: "commit", Revision: 7}
	w = f.req("GET", "/demo/walgit/api/commit/"+fakeSHA)
	if w.Code != 200 {
		t.Fatalf("commit = %d", w.Code)
	}
	var detail struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Stats []map[string]any `json:"stats"`
	}
	decodeJSON(t, w, &detail)
	if detail.Commit.SHA != fakeSHA || len(detail.Stats) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestBaseURLVariants(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.PublicURL = "https://git.example.com/"
	if got := f.env.baseURL(httptest.NewRequest("GET", "/x", nil)); got != "https://git.example.com" {
		t.Fatalf("public url = %q", got)
	}
	f.env.Cfg.Server.PublicURL = ""
	r := httptest.NewRequest("GET", "http://host.example/x", nil)
	if got := f.env.baseURL(r); got != "http://host.example" {
		t.Fatalf("host url = %q", got)
	}
	if got := f.env.baseURL(nil); got != "http://" {
		t.Fatalf("nil request = %q", got)
	}
	// TLS request → https scheme
	r2 := httptest.NewRequest("GET", "http://host.example/x", nil)
	r2.TLS = &tls.ConnectionState{}
	if got := f.env.baseURL(r2); !strings.HasPrefix(got, "https://") {
		t.Fatalf("tls = %q", got)
	}
}
