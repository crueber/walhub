package issues

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRouting(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	h := testHandler(s, janeP)
	// Non-issues paths report false on both lanes.
	for _, target := range []string{
		"/api/v1/owners",
		"/acme/repo/api/tree/main",
		"/acme/repo/api-browser/unknown",
		"/acme/repo/api/issues/1/unknown",
		"/acme/repo/api/issues/1/reactions/2",
		"/acme/repo/api/labels/x/y",
		"/acme/repo/api/milestones/000001/extra",
		"/Bad%20Owner/repo/api/issues",
	} {
		r := httptest.NewRequest("GET", target, nil)
		if h.Handle(httptest.NewRecorder(), r) {
			t.Errorf("Handle(%q) = true, want false", target)
		}
	}
	// ServeHTTP 404s otherwise.
	if w := doReq(h, "GET", "/api/v1/owners", ""); w.Code != http.StatusNotFound {
		t.Errorf("fallback = %d", w.Code)
	}
	// Unknown issue num shape → 404 unknown issue.
	if w := doReq(h, "GET", "/acme/repo/api/issues/0", ""); w.Code != http.StatusNotFound {
		t.Errorf("num 0 = %d", w.Code)
	}
	if w := doReq(h, "GET", "/acme/repo/api/issues/abc", ""); w.Code != http.StatusNotFound {
		t.Errorf("num abc = %d", w.Code)
	}
	// Unknown milestone id shape falls through to the core mux (false).
	if w := doReq(h, "GET", "/acme/repo/api/milestones/zzz", ""); w.Code != http.StatusNotFound {
		t.Errorf("bad milestone = %d", w.Code)
	}
	// Method mismatches → 405 with Allow.
	for _, c := range []struct{ method, target string }{
		{"DELETE", "/acme/repo/api/issues"},
		{"POST", "/acme/repo/api/issues/1"},
		{"GET", "/acme/repo/api/issues/1/comments"},
		{"GET", "/acme/repo/api/issues/1/reactions"},
		{"POST", "/acme/repo/api/issues/1/reactions/0/+1"},
		{"PUT", "/acme/repo/api/labels"},
		{"GET", "/acme/repo/api/labels/bug"},
		{"PUT", "/acme/repo/api/milestones"},
		{"POST", "/acme/repo/api/milestones/000001"},
	} {
		if w := doReq(h, c.method, c.target, ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") == "" {
			t.Errorf("%s %s = %d (Allow=%q), want 405", c.method, c.target, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestBothLanes(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mustCreate(t, s, "acme", "repo", janeP, "lane", "")
	for _, lane := range []string{"api", "api-browser"} {
		h := testHandler(s, janeP)
		if w := doReq(h, "GET", "/acme/repo/"+lane+"/issues", ""); w.Code != http.StatusOK {
			t.Errorf("lane %s list = %d: %s", lane, w.Code, w.Body.String())
		}
		if w := doReq(h, "GET", "/acme/repo/"+lane+"/issues/1", ""); w.Code != http.StatusOK {
			t.Errorf("lane %s get = %d", lane, w.Code)
		}
		if w := doReq(h, "GET", "/acme/repo/"+lane+"/labels", ""); w.Code != http.StatusOK {
			t.Errorf("lane %s labels = %d", lane, w.Code)
		}
		if w := doReq(h, "GET", "/acme/repo/"+lane+"/milestones", ""); w.Code != http.StatusOK {
			t.Errorf("lane %s milestones = %d", lane, w.Code)
		}
	}
}

func TestListIssuesHTTP(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	h := testHandler(s, janeP)
	for _, title := range []string{"one", "two", "three"} {
		mustCreate(t, s, "acme", "repo", janeP, title, "")
	}
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "d73a4a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 1, aliceP, IssuePatch{Labels: &[]string{"bug"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 2, aliceP, IssuePatch{State: strPtr("closed")}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		query     string
		wantCode  int
		wantCount int
		wantMore  bool
	}{
		{"all", "", 200, 3, false},
		{"open", "?state=open", 200, 2, false},
		{"closed", "?state=closed", 200, 1, false},
		{"label", "?labels=bug", 200, 1, false},
		{"label miss", "?labels=nope", 200, 0, false},
		{"page", "?n=2", 200, 2, true},
		{"cursor", "?n=2&after=2", 200, 1, false},
		{"cursor more", "?n=1&after=3", 200, 1, true},
		{"bad state", "?state=weird", 400, 0, false},
		{"bad n", "?n=x", 400, 0, false},
		{"n over max", "?n=101", 400, 0, false},
		{"bad after", "?after=x", 400, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doReq(h, "GET", "/acme/repo/api/issues"+c.query, "")
			if w.Code != c.wantCode {
				t.Fatalf("code = %d, want %d: %s", w.Code, c.wantCode, w.Body.String())
			}
			if c.wantCode != 200 {
				if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
					t.Fatalf("error content-type = %q, want text/plain", ct)
				}
				return
			}
			var res ListResult
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			if len(res.Issues) != c.wantCount || res.More != c.wantMore {
				t.Fatalf("got %d issues more=%v, want %d/%v", len(res.Issues), res.More, c.wantCount, c.wantMore)
			}
			if res.Issues == nil {
				t.Fatal("issues is null, want []")
			}
			if cc := w.Header().Get("Cache-Control"); cc != ccNoStore {
				t.Fatalf("cache = %q, want no-store", cc)
			}
		})
	}
	// Anon on a public repo may list.
	ha := testHandler(s, anonP)
	if w := doReq(ha, "GET", "/acme/repo/api/issues", ""); w.Code != http.StatusOK {
		t.Fatalf("anon list = %d", w.Code)
	}
	// Anon on a private repo gets a real 401 with Bearer challenge.
	roles.private["acme/priv"] = true
	if w := doReq(ha, "GET", "/acme/priv/api/issues", ""); w.Code != http.StatusUnauthorized ||
		w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anon private = %d challenge=%q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

func TestCreateIssueHTTP(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	h := testHandler(s, janeP)
	w := doReq(h, "POST", "/acme/repo/api/issues", `{"title":"hello","body":"world #1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Thread *Thread  `json:"thread"`
		Events []*Event `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Thread.Num != 1 || len(res.Events) != 1 || res.Events[0].Type != EventOpened {
		t.Fatalf("create body = %s", w.Body.String())
	}
	for _, c := range []struct{ name, body string }{
		{"unknown key", `{"title":"x","priority":"high"}`},
		{"bad json", `{`},
		{"missing title", `{}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if w := doReq(h, "POST", "/acme/repo/api/issues", c.body); w.Code != http.StatusBadRequest {
				t.Errorf("code = %d: %s", w.Code, w.Body.String())
			}
		})
	}
	// Anon create → 401.
	ha := testHandler(s, anonP)
	if w := doReq(ha, "POST", "/acme/repo/api/issues", `{"title":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anon create = %d", w.Code)
	}
}

func TestGetIssueHTTP(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	h := testHandler(s, janeP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "b")
	if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "c1"); err != nil {
		t.Fatal(err)
	}
	w := doReq(h, "GET", "/acme/repo/api/issues/1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	var view ThreadView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Events) != 2 || view.EventsMore {
		t.Fatalf("events = %d more=%v", len(view.Events), view.EventsMore)
	}
	// Newest-last order.
	if view.Events[0].Seq < view.Events[1].Seq {
		t.Fatalf("not newest-last: %+v", view.Events)
	}
	// If-None-Match → 304.
	r := httptest.NewRequest("GET", "/acme/repo/api/issues/1", nil)
	r.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r)
	if w2.Code != http.StatusNotModified {
		t.Fatalf("inm = %d", w2.Code)
	}
	// Unknown issue → 404 plain text.
	if w := doReq(h, "GET", "/acme/repo/api/issues/9", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost = %d", w.Code)
	}
	// Bad window params → 400.
	if w := doReq(h, "GET", "/acme/repo/api/issues/1?n=x", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad n = %d", w.Code)
	}
	if w := doReq(h, "GET", "/acme/repo/api/issues/1?after_seq=-1", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad after_seq = %d", w.Code)
	}
	// Events sub-route.
	w = doReq(h, "GET", "/acme/repo/api/issues/1/events?after_seq=2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("events = %d", w.Code)
	}
	var evres struct {
		Events []*Event `json:"events"`
		More   bool     `json:"more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &evres); err != nil {
		t.Fatal(err)
	}
	if len(evres.Events) != 2 || evres.More { // seqs 0,1 < 2
		t.Fatalf("window = %+v", evres)
	}
	w = doReq(h, "GET", "/acme/repo/api/issues/1/events?after_seq=1", "")
	if err := json.Unmarshal(w.Body.Bytes(), &evres); err != nil {
		t.Fatal(err)
	}
	if len(evres.Events) != 1 || evres.Events[0].Seq != 0 {
		t.Fatalf("window = %+v", evres)
	}
}

func TestPatchIssueHTTP(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	h := testHandler(s, janeP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "d73a4a", ""); err != nil {
		t.Fatal(err)
	}
	// Alice (triage) patches every field group at once.
	ha := testHandler(s, aliceP)
	w := doReq(ha, "PATCH", "/acme/repo/api/issues/1", `{"title":"t2","labels":["bug"],"state":"closed","state_reason":"completed"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Thread *Thread `json:"thread"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Thread.Title != "t2" || res.Thread.State != StateClosed || len(res.Thread.Labels) != 1 {
		t.Fatalf("patched = %+v", res.Thread)
	}
	// Unknown key → 400.
	if w := doReq(h, "PATCH", "/acme/repo/api/issues/1", `{"priority":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown key = %d", w.Code)
	}
	// Forbidden for a stranger on someone else's thread.
	hs := testHandler(s, bobP)
	if w := doReq(hs, "PATCH", "/acme/repo/api/issues/1", `{"title":"hijack"}`); w.Code != http.StatusForbidden {
		t.Errorf("stranger = %d", w.Code)
	}
	// Ghost → 404.
	if w := doReq(h, "PATCH", "/acme/repo/api/issues/9", `{"title":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("ghost = %d", w.Code)
	}
}

func TestCommentsHTTP(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	h := testHandler(s, bobP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	w := doReq(h, "POST", "/acme/repo/api/issues/1/comments", `{"body":"looks good"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("comment = %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Event *Event `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Event.Type != EventCommented || res.Event.Seq != 1 {
		t.Fatalf("event = %+v", res.Event)
	}
	for _, c := range []struct{ name, body string }{
		{"empty", `{"body":""}`},
		{"unknown key", `{"body":"x","format":"md"}`},
		{"bad json", `{"body":`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if w := doReq(h, "POST", "/acme/repo/api/issues/1/comments", c.body); w.Code != http.StatusBadRequest {
				t.Errorf("code = %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if w := doReq(h, "POST", "/acme/repo/api/issues/9/comments", `{"body":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("ghost = %d", w.Code)
	}
}

func TestReactionsHTTP(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	h := testHandler(s, bobP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "b")
	w := doReq(h, "POST", "/acme/repo/api/issues/1/reactions", `{"target_event_seq":0,"content":"+1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("react = %d: %s", w.Code, w.Body.String())
	}
	// Duplicate → 200 with the summary.
	w = doReq(h, "POST", "/acme/repo/api/issues/1/reactions", `{"target_event_seq":0,"content":"+1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("dup = %d: %s", w.Code, w.Body.String())
	}
	var dup struct {
		Summary map[string]map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dup); err != nil {
		t.Fatal(err)
	}
	if dup.Summary[seqKey(0)]["+1"] != 1 {
		t.Fatalf("dup summary = %v", dup.Summary)
	}
	// Missing target seq → 400; bad content → 400.
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/reactions", `{"content":"+1"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing seq = %d", w.Code)
	}
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/reactions", `{"target_event_seq":0,"content":"nope"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad content = %d", w.Code)
	}
	// Delete own → 204; again → 404; bad seq → 400.
	if w := doReq(h, "DELETE", "/acme/repo/api/issues/1/reactions/0/%2B1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("unreact = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(h, "DELETE", "/acme/repo/api/issues/1/reactions/0/%2B1", ""); w.Code != http.StatusNotFound {
		t.Errorf("double unreact = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/acme/repo/api/issues/1/reactions/x/%2B1", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad seq = %d", w.Code)
	}
}

func TestLabelsHTTP(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	ha := testHandler(s, aliceP)
	w := doReq(ha, "POST", "/acme/repo/api/labels", `{"name":"bug","color":"d73a4a","description":"broken"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create label = %d: %s", w.Code, w.Body.String())
	}
	// Duplicate → 409.
	if w := doReq(ha, "POST", "/acme/repo/api/labels", `{"name":"BUG","color":"ffffff"}`); w.Code != http.StatusConflict {
		t.Errorf("dup = %d", w.Code)
	}
	// Update.
	w = doReq(ha, "PATCH", "/acme/repo/api/labels/bug", `{"color":"000000"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	// Bob (read) may list but not write.
	hb := testHandler(s, bobP)
	if w := doReq(hb, "GET", "/acme/repo/api/labels", ""); w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	if w := doReq(hb, "POST", "/acme/repo/api/labels", `{"name":"x","color":"ffffff"}`); w.Code != http.StatusForbidden {
		t.Errorf("bob create = %d", w.Code)
	}
	// Delete reports threads_affected with 200 (a 204 cannot carry it).
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 1, aliceP, IssuePatch{Labels: &[]string{"bug"}}); err != nil {
		t.Fatal(err)
	}
	w = doReq(ha, "DELETE", "/acme/repo/api/labels/bug", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	var del struct {
		ThreadsAffected int `json:"threads_affected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &del); err != nil {
		t.Fatal(err)
	}
	if del.ThreadsAffected != 1 {
		t.Fatalf("affected = %d", del.ThreadsAffected)
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/labels/bug", ""); w.Code != http.StatusNotFound {
		t.Errorf("double delete = %d", w.Code)
	}
}

func TestMilestonesHTTP(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	ha := testHandler(s, aliceP)
	w := doReq(ha, "POST", "/acme/repo/api/milestones", `{"title":"v1","due_on":"2026-10-01T00:00:00Z"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Milestone *Milestone `json:"milestone"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Milestone.ID
	// Get + list with state filter.
	if w := doReq(ha, "GET", "/acme/repo/api/milestones/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	if w := doReq(ha, "GET", "/acme/repo/api/milestones?state=closed", ""); w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	} else {
		var list struct {
			Milestones []*Milestone `json:"milestones"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Milestones) != 0 {
			t.Fatalf("closed filter = %+v", list.Milestones)
		}
	}
	if w := doReq(ha, "GET", "/acme/repo/api/milestones?state=bogus", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad state = %d", w.Code)
	}
	// Patch + close, then delete (no open issues) → 204.
	if w := doReq(ha, "PATCH", "/acme/repo/api/milestones/"+id, `{"state":"closed"}`); w.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/milestones/"+id, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(ha, "GET", "/acme/repo/api/milestones/"+id, ""); w.Code != http.StatusNotFound {
		t.Errorf("get deleted = %d", w.Code)
	}
	// 409 while open issues reference it.
	w = doReq(ha, "POST", "/acme/repo/api/milestones", `{"title":"v2"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id2 := created.Milestone.ID
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	pid := &id2
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 1, aliceP, IssuePatch{Milestone: &pid}); err != nil {
		t.Fatal(err)
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/milestones/"+id2, ""); w.Code != http.StatusConflict {
		t.Errorf("blocked delete = %d: %s", w.Code, w.Body.String())
	}
	// Bob cannot manage milestones.
	hb := testHandler(s, bobP)
	if w := doReq(hb, "POST", "/acme/repo/api/milestones", `{"title":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("bob create = %d", w.Code)
	}
}
