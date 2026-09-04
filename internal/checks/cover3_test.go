package checks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Third edge file: handler routing branches, writer/decoder edges, and
// the small uncovered service/codec seams.

func TestCoverHandleBranches(t *testing.T) {
	e, h := testHandler()
	sha := hexSHA(100)
	e.knowSHA(sha)
	authed := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return admin(), nil
	}}
	// Unknown owner/repo shape ⇒ not a checks route (core mux answers).
	req := httptest.NewRequest("GET", "/o!/api/checks", nil)
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatal("bad owner claimed")
	}
	// Short path ⇒ not a checks route.
	req = httptest.NewRequest("GET", "/o", nil)
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatal("short path claimed")
	}
	// Tokens with an extra segment ⇒ 404.
	rec := doRequest(authed, "GET", "/o/r/api/checks/tokens/a/b", "", "")
	if rec.Code != 404 {
		t.Fatalf("tokens extra: %d", rec.Code)
	}
	// Statuses with an extra segment ⇒ 404.
	rec = doRequest(authed, "GET", "/o/r/api/checks/statuses/"+sha+"/x", "", "")
	if rec.Code != 404 {
		t.Fatalf("statuses extra: %d", rec.Code)
	}
	// Undecodable segment fails closed (verbatim, never a match): the
	// route is claimed and answers 400.
	req = &http.Request{Method: "GET", URL: &url.URL{Path: "/o/r/api/checks/x", RawPath: "/o/r/api/checks/%zz"}, Header: http.Header{}}
	rec2 := httptest.NewRecorder()
	if !h.Handle(rec2, req) {
		t.Fatal("undecodable route unclaimed")
	}
	if rec2.Code != 400 {
		t.Fatalf("undecodable: %d", rec2.Code)
	}
	if decodeSegment("%zz") != "%zz" {
		t.Fatal("decode verbatim")
	}
}

func TestCoverWriterAndDecoder(t *testing.T) {
	// writeJSON encode failure ⇒ 500.
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, func() {})
	if rec.Code != 500 {
		t.Fatalf("encode: %d", rec.Code)
	}
	// decodeStrict: over-limit body ⇒ 400.
	e, h := testHandler()
	_ = h
	req := httptest.NewRequest("POST", "/o/r/api/checks/statuses/"+hexSHA(101), strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return admin(), nil
	}}
	authed.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("over-limit: %d", rec.Code)
	}
	// decodeStrict: null body ⇒ 400.
	rec = doRequest(authed, "POST", "/o/r/api/checks/statuses/"+hexSHA(101), `null`, "")
	if rec.Code != 400 {
		t.Fatalf("null body: %d", rec.Code)
	}
	// decodeStrict: valid JSON, wrong shape (array) ⇒ 400.
	rec = doRequest(authed, "POST", "/o/r/api/checks/statuses/"+hexSHA(101), `[1,2]`, "")
	if rec.Code != 400 {
		t.Fatalf("array body: %d", rec.Code)
	}
}

func TestCoverHandlerErrors(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(102)
	e.knowSHA(sha)
	failAuth := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "nope"}
	}}
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"statuses", "GET", "/o/r/api/checks/statuses/" + sha, ""},
		{"combined", "GET", "/o/r/api/checks/" + sha, ""},
		{"index", "GET", "/o/r/api/checks", ""},
		{"tokens", "GET", "/o/r/api/checks/tokens", ""},
		{"create", "POST", "/o/r/api/checks/tokens", `{"name":"x"}`},
		{"revoke", "DELETE", "/o/r/api/checks/tokens/abcd1234", ""},
		{"report", "POST", "/o/r/api/checks/statuses/" + sha, `{"context":"ci","state":"success"}`},
	} {
		rec := doRequest(failAuth, tc.method, tc.path, tc.body, "")
		if rec.Code != 401 {
			t.Fatalf("%s: %d", tc.name, rec.Code)
		}
	}
	// Service errors map through (corrupt index ⇒ 500 on list).
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	okAuth := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return admin(), nil
	}}
	rec := doRequest(okAuth, "GET", "/o/r/api/checks", "", "")
	if rec.Code != 500 {
		t.Fatalf("corrupt index: %d", rec.Code)
	}
	// Corrupt token ⇒ 500 on token list.
	if _, err := store.PutBytes(ctx(), e.store, TokenKey("o", "r", "deadbeef"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec = doRequest(okAuth, "GET", "/o/r/api/checks/tokens", "", "")
	if rec.Code != 500 {
		t.Fatalf("corrupt token: %d", rec.Code)
	}
	// Invalid token create ⇒ 400.
	rec = doRequest(okAuth, "POST", "/o/r/api/checks/tokens", `{"name":""}`, "")
	if rec.Code != 400 {
		t.Fatalf("empty name: %d", rec.Code)
	}
}

func TestCoverSmallSeams(t *testing.T) {
	e := newTestEnv()
	// nowUTC without a clock.
	bare := New(e.store, nil)
	if bare.nowUTC().IsZero() {
		t.Fatal("zero clock")
	}
	// casUpdate attempts ≤ 0 defaults to 5.
	if _, err := bare.casUpdate(ctx(), "repos/o/r/probe.json", 0, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); err != nil {
		t.Fatalf("cas default: %v", err)
	}
	// casUpdate applies f errors and non-412 failures verbatim.
	boom := errors.New("boom")
	if _, err := bare.casUpdate(ctx(), "repos/o/r/probe.json", 2, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return nil, false, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("f err: %v", err)
	}
	bare.Store = &errStore{inner: e.store, putErr: errors.New("down")}
	if _, err := bare.casUpdate(ctx(), "repos/o/r/probe2.json", 2, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); err == nil {
		t.Fatal("put failure accepted")
	}
	bare.Store = e.store
	// emit/stream with preset envelope fields.
	var got NotifyEvent
	bare.Notify = func(_ context.Context, ev NotifyEvent) { got = ev }
	bare.emit(ctx(), NotifyEvent{Repo: "o/r", At: "2026-09-04T12:00:00Z"})
	if got.At != "2026-09-04T12:00:00Z" || got.Class != NotifyClass {
		t.Fatalf("emit = %+v", got)
	}
	var gotS StreamEvent
	bare.Stream = func(_ context.Context, ev StreamEvent) { gotS = ev }
	bare.stream(ctx(), StreamEvent{Repo: "o/r"})
	if gotS.Name != StreamName {
		t.Fatalf("stream = %+v", gotS)
	}
	// combineView with nil statuses (zero-context fold, direct).
	view := combineView(hexSHA(103), nil)
	if view.State != StatePending || view.Statuses == nil {
		t.Fatalf("nil fold = %+v", view)
	}
	// roleRank bogus; requireRole on the nil-Roles default ladder.
	if roleRank("bogus") != 0 {
		t.Fatal("bogus rank")
	}
	if err := bare.requireRole(ctx(), "o", "r", reader(), "read"); err != nil {
		t.Fatalf("default read: %v", err)
	}
	// AssertPrefixDisjoint skips empty prefixes.
	AssertPrefixDisjoint("")
	// loadToken store failure; ListChecks store failure.
	bare.Store = &errStore{inner: e.store, getErr: errors.New("down")}
	if _, _, err := bare.loadToken(ctx(), "o", "r", "abcd1234"); err == nil {
		t.Fatal("token read failure accepted")
	}
	if _, err := bare.ListChecks(ctx(), "o", "r", admin(), "", 10); err == nil {
		t.Fatal("index read failure accepted")
	}
	bare.Store = e.store
}

func TestCoverCompactExhaustion(t *testing.T) {
	e := newTestEnv()
	// Oversized index + permanent contention ⇒ silent drop (false, nil).
	ix := &IndexDoc{SHAs: []IndexSHA{}}
	for i := 0; i < IndexHotWindow+10; i++ {
		sha := strings.Repeat("d", 38) + sprintf2(i)
		ix.SHAs = append(ix.SHAs, IndexSHA{SHA: sha, State: StateSuccess,
			Contexts:  []IndexContext{{Name: "ci", State: StateSuccess, UpdatedAt: "2026-09-04T12:00:00Z"}},
			UpdatedAt: "2026-09-04T12:00:00Z"})
	}
	ix.SHAs[0].Contexts[0].Name = strings.Repeat("q", IndexSizeLimit)
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), encodeIndex(ix),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e.svc.Store = &errStore{inner: e.store, put412: true}
	if compacted, err := e.svc.CompactIndex(ctx(), "o", "r"); err != nil || compacted {
		t.Fatalf("exhaustion: %v %v", compacted, err)
	}
	e.svc.Store = e.store
}

func TestCoverNotifySkips(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(61)
	other := hexSHA(62)
	e.knowSHA(sha)
	e.knowSHA(other)
	// Card open but thread closed ⇒ skip; card open but thread wrong
	// kind ⇒ skip; corrupt pr.json ⇒ skip.
	seedPR(t, e, 21, "open", false, sha)
	overwrite(t, e, "repos/o/r/issues/000015/thread.json", `{"num":21,"kind":"pr","title":"t","state":"closed","author":"a","created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z","labels":[],"assignees":[],"participants":[],"next_event_seq":1,"comment_count":0,"version":1}`)
	seedPR(t, e, 22, "open", false, sha)
	overwrite(t, e, "repos/o/r/issues/000016/thread.json", `{"num":22,"kind":"issue","title":"t","state":"open","author":"a","created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z","labels":[],"assignees":[],"participants":[],"next_event_seq":1,"comment_count":0,"version":1}`)
	seedPR(t, e, 23, "open", false, other)
	overwrite(t, e, "repos/o/r/pulls/000017/pr.json", `{oops`)
	var emitted []NotifyEvent
	e.svc.Notify = func(_ context.Context, ev NotifyEvent) { emitted = append(emitted, ev) }
	e.mustReport(t, sha, "ci", StateFailure)
	if len(emitted) != 0 {
		t.Fatalf("skips emit = %+v", emitted)
	}
}

// overwrite replaces a seeded object (tests only).
func overwrite(t *testing.T, e *testEnv, key, body string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(body),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); err != nil {
		t.Fatalf("overwrite %s: %v", key, err)
	}
}
