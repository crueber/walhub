package pulls

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// doReq issues one request against the Handler (both lanes via lane arg).
func doReq(t *testing.T, h *Handler, method, lane, path, body string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return p, nil }
	w := httptest.NewRecorder()
	h.Handle(w, req)
	_ = lane
	return w
}

func lanePath(lane, suffix string) string {
	if lane == "browser" {
		return "/o/r/api-browser" + suffix
	}
	return "/o/r/api" + suffix
}

func seedOpened(t *testing.T, e *testEnv) {
	t.Helper()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	e.roles.Public = true
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "T", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, "")
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2),
		"refs/pull/1/head": hexSHA(2),
	})
	e.git.MergeBaseSHA = hexSHA(7)
}

func TestHTTPOpenTable(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		body       string
		principal  auth.Principal
		wantStatus int
		wantSub    string
	}{
		{name: "created", method: "POST", body: `{"title":"T","base_ref":"refs/heads/main","head_ref":"refs/heads/topic"}`, principal: writer(), wantStatus: 201, wantSub: `"kind":"pr"`},
		{name: "created with body+fork-shape", method: "POST", body: `{"title":"T","base_ref":"refs/heads/main","head_ref":"refs/heads/topic","body":"hi"}`, principal: writer(), wantStatus: 201},
		{name: "unknown field", method: "POST", body: `{"title":"T","base_ref":"refs/heads/main","head_ref":"refs/heads/topic","zzz":1}`, principal: writer(), wantStatus: 400, wantSub: "unknown field"},
		{name: "bad json", method: "POST", body: `{`, principal: writer(), wantStatus: 400},
		{name: "missing title", method: "POST", body: `{"base_ref":"refs/heads/main","head_ref":"refs/heads/topic"}`, principal: writer(), wantStatus: 400},
		{name: "anonymous", method: "POST", body: `{"title":"T","base_ref":"refs/heads/main","head_ref":"refs/heads/topic"}`, principal: auth.Anonymous(), wantStatus: 401, wantSub: ""},
		{name: "unreachable", method: "POST", body: `{"title":"T","base_ref":"refs/heads/main","head_ref":"refs/heads/topic"}`, principal: writer(), wantStatus: 422},
		{name: "method not allowed", method: "DELETE", body: ``, principal: writer(), wantStatus: 405},
	}
	for _, tc := range cases {
		for _, lane := range []string{"api", "browser"} {
			t.Run(tc.name+"/"+lane, func(t *testing.T) {
				e := newTestEnv()
				e.roles.Roles["jane@example.com"] = "write"
				e.roles.Public = false
				if tc.name != "unreachable" {
					e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
				} else {
					e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
					e.git.ReachableMap = map[string]bool{hexSHA(2): false}
				}
				w := doReq(t, e.h, tc.method, lane, lanePath(lane, "/pulls"), tc.body, tc.principal)
				if w.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (%s)", w.Code, tc.wantStatus, w.Body.String())
				}
				if tc.wantSub != "" && !strings.Contains(w.Body.String(), tc.wantSub) {
					t.Fatalf("body = %s", w.Body.String())
				}
				if tc.wantStatus == 401 && w.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("401 must carry WWW-Authenticate: Bearer")
				}
			})
		}
	}
}

func TestHTTPGetListPut(t *testing.T) {
	for _, lane := range []string{"api", "browser"} {
		t.Run("lane="+lane, func(t *testing.T) {
			e := newTestEnv()
			seedOpened(t, e)
			// GET one.
			w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1"), "", writer())
			if w.Code != 200 {
				t.Fatalf("get = %d (%s)", w.Code, w.Body.String())
			}
			var view map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, k := range []string{"thread", "pr", "mergeable"} {
				if _, ok := view[k]; !ok {
					t.Fatalf("view lacks %s: %v", k, view)
				}
			}
			etag := w.Header().Get("ETag")
			if etag == "" {
				t.Fatal("GET pull must carry ETag <head sha>")
			}
			// 304 on If-None-Match.
			req := httptest.NewRequest("GET", lanePath(lane, "/pulls/1"), nil)
			req.Header.Set("If-None-Match", etag)
			e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
			w2 := httptest.NewRecorder()
			e.h.Handle(w2, req)
			if w2.Code != 304 {
				t.Fatalf("304 = %d", w2.Code)
			}
			// GET unknown + bad num.
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/99"), "", writer()); w.Code != 404 {
				t.Fatalf("unknown = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/0"), "", writer()); w.Code != 404 {
				t.Fatalf("badnum = %d", w.Code)
			}
			// List + filters + bad params.
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls?state=open&n=10"), "", writer()); w.Code != 200 {
				t.Fatalf("list = %d (%s)", w.Code, w.Body.String())
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls?state=bogus"), "", writer()); w.Code != 400 {
				t.Fatalf("bad state = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls?n=101"), "", writer()); w.Code != 400 {
				t.Fatalf("big n = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls?sort=bogus"), "", writer()); w.Code != 400 {
				t.Fatalf("bad sort = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls?after=zzz"), "", writer()); w.Code != 400 {
				t.Fatalf("bad after = %d", w.Code)
			}
			// PUT title + state + bad key + close.
			w = doReq(t, e.h, "PUT", lane, lanePath(lane, "/pulls/1"), `{"title":"Renamed"}`, writer())
			if w.Code != 200 || !strings.Contains(w.Body.String(), "Renamed") {
				t.Fatalf("put = %d (%s)", w.Code, w.Body.String())
			}
			if w := doReq(t, e.h, "PUT", lane, lanePath(lane, "/pulls/1"), `{"frobnicate":1}`, writer()); w.Code != 400 {
				t.Fatalf("unknown key = %d", w.Code)
			}
			if w := doReq(t, e.h, "PUT", lane, lanePath(lane, "/pulls/1"), `{"state":"bogus"}`, writer()); w.Code != 400 {
				t.Fatalf("bad state = %d", w.Code)
			}
			if w := doReq(t, e.h, "PUT", lane, lanePath(lane, "/pulls/1"), `{"state":"closed"}`, writer()); w.Code != 200 {
				t.Fatalf("close = %d (%s)", w.Code, w.Body.String())
			}
			// PUT method gates.
			if w := doReq(t, e.h, "PATCH", lane, lanePath(lane, "/pulls/1"), `{}`, writer()); w.Code != 405 {
				t.Fatalf("patch = %d", w.Code)
			}
		})
	}
}

func TestHTTPDiffCommitsComments(t *testing.T) {
	for _, lane := range []string{"api", "browser"} {
		t.Run("lane="+lane, func(t *testing.T) {
			e := newTestEnv()
			seedOpened(t, e)
			e.git.DiffText = "diff --git a/x b/x\n"
			e.git.LogRows = []CommitEntry{{SHA: hexSHA(2), Subject: "work", Author: "jane@example.com", At: "2026-09-04T12:00:00Z"}}
			w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1/diff"), "", writer())
			if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
				t.Fatalf("diff = %d (%s) ct=%s", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
			}
			w = doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1/commits?n=10"), "", writer())
			if w.Code != 200 || !strings.Contains(w.Body.String(), hexSHA(2)) {
				t.Fatalf("commits = %d (%s)", w.Code, w.Body.String())
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1/commits?n=0"), "", writer()); w.Code != 400 {
				t.Fatalf("bad n = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1/commits?skip=-1"), "", writer()); w.Code != 400 {
				t.Fatalf("bad skip = %d", w.Code)
			}
			w = doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls/1/comments"), `{"body":"nice"}`, writer())
			if w.Code != 201 {
				t.Fatalf("comment = %d (%s)", w.Code, w.Body.String())
			}
			if w := doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls/1/comments"), `{"body":""}`, writer()); w.Code != 400 {
				t.Fatalf("empty comment = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1/diff"), "", auth.Anonymous()); w.Code != 200 {
				t.Fatalf("public anon diff = %d", w.Code)
			}
		})
	}
}

func TestHTTPMergeFlow(t *testing.T) {
	for _, lane := range []string{"api", "browser"} {
		t.Run("lane="+lane, func(t *testing.T) {
			e := newTestEnv()
			seedOpened(t, e)
			e.refs.Refs["o/r"] = map[string]string{"refs/heads/main": hexSHA(1)}
			// Write role cannot merge.
			w := doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls/1/merge"), `{"strategy":"merge"}`, writer())
			if w.Code != 403 {
				t.Fatalf("write merge = %d (%s)", w.Code, w.Body.String())
			}
			// Bad strategy + unknown field.
			w = doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls/1/merge"), `{"strategy":"octopus"}`, maintainer())
			if w.Code != 400 {
				t.Fatalf("bad strategy = %d (%s)", w.Code, w.Body.String())
			}
			w = doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls/1/merge"), `{"strategy":"merge","zzz":1}`, maintainer())
			if w.Code != 400 {
				t.Fatalf("unknown field = %d", w.Code)
			}
			// Start merge → 202 + task.
			w = doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls/1/merge"), `{"strategy":"squash","delete_head":true}`, maintainer())
			if w.Code != 202 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Fatalf("merge = %d (%s) cc=%s", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
			}
			// Attach poll.
			deadline := time.Now().Add(5 * time.Second)
			var taskBody string
			for time.Now().Before(deadline) {
				w2 := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/1/merge/task"), "", maintainer())
				if w2.Code == 200 && strings.Contains(w2.Body.String(), `"state":"ok"`) {
					taskBody = w2.Body.String()
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if taskBody == "" {
				t.Fatal("merge task never reached ok")
			}
			// Task for another num 404s.
			if w := doReq(t, e.h, "GET", lane, lanePath(lane, "/pulls/2/merge/task"), "", maintainer()); w.Code != 404 {
				t.Fatalf("other task = %d", w.Code)
			}
			// Update-branch + delete-head.
			e2 := newTestEnv()
			seedOpened(t, e2)
			e2.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(5), "refs/heads/topic": hexSHA(2)})
			e2.refs.Refs["o/r"] = map[string]string{"refs/heads/topic": hexSHA(2)}
			w = doReq(t, e2.h, "POST", lane, lanePath(lane, "/pulls/1/update-branch"), `{}`, writer())
			if w.Code != 202 {
				t.Fatalf("update-branch = %d (%s)", w.Code, w.Body.String())
			}
			if w := doReq(t, e2.h, "POST", lane, lanePath(lane, "/pulls/1/update-branch"), `{"zzz":1}`, writer()); w.Code != 400 {
				t.Fatalf("ub unknown = %d", w.Code)
			}
			if w := doReq(t, e.h, "DELETE", lane, lanePath(lane, "/pulls/1/head"), "", maintainer()); w.Code != 204 {
				t.Fatalf("delete-head = %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestHTTPFork(t *testing.T) {
	for _, lane := range []string{"api", "browser"} {
		t.Run("lane="+lane, func(t *testing.T) {
			e := newTestEnv()
			e.roles.Public = true
			e.roles.Roles["jane@example.com"] = "write"
			top := func(suffix string) string {
				if lane == "browser" {
					return "/api-browser/v1" + suffix
				}
				return "/api/v1" + suffix
			}
			w := doReq(t, e.h, "POST", lane, top("/repos/o/r/forks"), `{"name":"r-fork"}`, writer())
			if w.Code != 202 || !strings.Contains(w.Body.String(), "o/r-fork") {
				t.Fatalf("fork = %d (%s)", w.Code, w.Body.String())
			}
			if w := doReq(t, e.h, "POST", lane, top("/repos/o/r/forks"), `{"name":"x","zzz":1}`, writer()); w.Code != 400 {
				t.Fatalf("unknown = %d", w.Code)
			}
			if w := doReq(t, e.h, "GET", lane, top("/repos/o/r/forks"), ``, writer()); w.Code != 405 {
				t.Fatalf("get forks = %d", w.Code)
			}
			// Non-pulls paths fall through (false → core mux owns them).
			req := httptest.NewRequest("GET", "/o/r/api/issues", nil)
			e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
			w2 := httptest.NewRecorder()
			if e.h.Handle(w2, req) {
				t.Fatal("issues path must not claim pulls handling")
			}
			req = httptest.NewRequest("GET", "/api/v1/orgs", nil)
			w2 = httptest.NewRecorder()
			if e.h.Handle(w2, req) {
				t.Fatal("top-level non-fork path must fall through")
			}
			// ServeHTTP 404s non-pulls paths.
			req = httptest.NewRequest("GET", "/nope", nil)
			w2 = httptest.NewRecorder()
			e.h.ServeHTTP(w2, req)
			if w2.Code != 404 {
				t.Fatalf("servehttp = %d", w2.Code)
			}
		})
	}
}

func TestHTTPAuthMatrix(t *testing.T) {
	e := newTestEnv()
	seedOpened(t, e)
	e.roles.Public = false
	// Private repo: anonymous reads get a real 401 with Bearer challenge.
	w := doReq(t, e.h, "GET", "api", "/o/r/api/pulls", "", auth.Anonymous())
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anon list = %d (%s)", w.Code, w.Body.String())
	}
	w = doReq(t, e.h, "GET", "api", "/o/r/api/pulls/1", "", auth.Anonymous())
	if w.Code != 401 {
		t.Fatalf("anon get = %d", w.Code)
	}
}
