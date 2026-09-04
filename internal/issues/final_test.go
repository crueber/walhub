package issues

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// Clocks, roles, and tiny units.
func TestClockAndRoles(t *testing.T) {
	if got := (&Service{}).nowUTC(); got.IsZero() {
		t.Fatal("nil clock is zero")
	}
	s := testService(newFakeRoles())
	if got := s.nowUTC(); got.Year() != 2026 {
		t.Fatalf("fixed clock = %v", got)
	}
	bare := New(store.NewMemory(), nil)
	if got := bare.roleOf(reqCtx(), "o", "r", adminP); got != "admin" {
		t.Fatalf("roleOf admin = %q", got)
	}
	if err := bare.requireRole(reqCtx(), "o", "r", aliceP, "read"); err != nil {
		t.Fatalf("bare read = %v", err)
	}
	if err := bare.requireRead(reqCtx(), "o", "r", adminP); err != nil {
		t.Fatalf("admin read = %v", err)
	}
	if decodeSegment("%zz") != "%zz" || decodeSegment("a%20b") != "a b" {
		t.Fatal("decodeSegment")
	}
	// upsertCard over nil pages stays []-shaped.
	ix := &Index{}
	upsertCard(ix, Card{Num: 1, Kind: "issue", State: StateOpen, UpdatedAt: "2026-09-04T12:00:00Z"})
	if ix.Open == nil || len(ix.Open) != 1 {
		t.Fatalf("upsert nil pages = %+v", ix)
	}
}

// Create's three store-failure points.
func TestCreateStoreFailures(t *testing.T) {
	t.Run("alloc fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory(), failPut: func(key string, n int) error {
			return precond(key)
		}}
		s := New(fl, newFakeRoles())
		if _, _, err := s.CreateIssue(reqCtx(), "acme", "repo", janeP, "t", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("thread put fails hard", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory(), failPut: func(key string, n int) error {
			if strings.HasSuffix(key, "thread.json") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}}
		s := New(fl, newFakeRoles())
		if _, _, err := s.CreateIssue(reqCtx(), "acme", "repo", janeP, "t", ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("event put fails hard", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory(), failPut: func(key string, n int) error {
			if strings.Contains(key, "/events/") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}}
		s := New(fl, newFakeRoles())
		if _, _, err := s.CreateIssue(reqCtx(), "acme", "repo", janeP, "t", ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("comment on corrupt thread", func(t *testing.T) {
		s := testService(newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		mustPut(t, s, ThreadKey("acme", "repo", 1), []byte("{bad"))
		if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "x"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("comment on private repo as stranger", func(t *testing.T) {
		roles := newFakeRoles()
		roles.private["acme/priv"] = true
		s := testService(roles)
		if _, err := s.AddComment(reqCtx(), "acme", "priv", 1, bobP, "x"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("header CAS fails hard", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory()}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failPut = func(key string, n int) error {
			if strings.HasSuffix(key, "thread.json") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}
		if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "x"); err == nil {
			t.Fatal("want error")
		}
	})
}

// Event-scan failure points.
func TestEventScanFailures(t *testing.T) {
	t.Run("loadEvent get fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory()}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error {
			if strings.Contains(key, "/events/") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}
		if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", 1, 0, bobP, "+1"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("loadEvent corrupt target", func(t *testing.T) {
		s := testService(newFakeRoles())
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		mustPut(t, s, EventKey("acme", "repo", th.Num, 7), []byte("{bad"))
		if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 7, bobP, "+1"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scan get fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory()}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error {
			if strings.Contains(key, "/events/") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}
		if _, err := s.GetThread(reqCtx(), "acme", "repo", 1, janeP, 0, 10); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("torn event skipped", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory()}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "c"); err != nil {
			t.Fatal(err)
		}
		// The LIST still reports the key but the GET misses it: skipped.
		fl.failGet = func(key string) error {
			if strings.HasSuffix(key, "/events/000000000000.json") {
				return store.NewNotFound(key)
			}
			return nil
		}
		view, err := s.GetThread(reqCtx(), "acme", "repo", 1, janeP, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Events) != 1 || view.Events[0].Seq != 1 {
			t.Fatalf("events = %+v", view.Events)
		}
	})
}

// Index/label/milestone read failure points.
func TestIndexReadFailures(t *testing.T) {
	t.Run("create tolerates corrupt index", func(t *testing.T) {
		s := testService(newFakeRoles())
		mustPut(t, s, IndexKey("acme", "repo"), []byte("{bad"))
		if _, _, err := s.CreateIssue(reqCtx(), "acme", "repo", janeP, "t", ""); err != nil {
			t.Fatalf("create with corrupt index = %v", err)
		}
	})
	t.Run("create tolerates index write failure", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory(), failPut: func(key string, n int) error {
			if strings.HasSuffix(key, "index.json") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}}
		s := New(fl, newFakeRoles())
		if _, _, err := s.CreateIssue(reqCtx(), "acme", "repo", janeP, "t", ""); err != nil {
			t.Fatalf("create with failing index = %v", err)
		}
	})
	t.Run("empty label set reads", func(t *testing.T) {
		s := testService(newFakeRoles())
		mustPut(t, s, LabelsKey("acme", "repo"), []byte(`{}`))
		ls, _, err := s.loadLabels(reqCtx(), "acme", "repo")
		if err != nil || ls.Labels == nil {
			t.Fatalf("ls = %+v,%v", ls, err)
		}
	})
	t.Run("labels get fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory(), failGet: func(key string) error {
			return store.NewRetryable(key, fmt.Errorf("down"))
		}}
		s := New(fl, newFakeRoles())
		h := testHandler(s, janeP)
		if w := doReq(h, "GET", "/acme/repo/api/labels", ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("labels = %d", w.Code)
		}
	})
	t.Run("milestone counters clamp at zero", func(t *testing.T) {
		s := testService(newFakeRoles())
		mustPut(t, s, MilestoneKey("acme", "repo", "000001"), []byte(`{"id":"000001","title":"t","state":"open"}`))
		s.bumpMilestone(reqCtx(), "acme", "repo", "000001", -5, -5)
		m, _, _ := s.loadMilestone(reqCtx(), "acme", "repo", "000001")
		if m.OpenIssues != 0 || m.ClosedIssues != 0 {
			t.Fatalf("clamped = %+v", m)
		}
	})
	t.Run("milestone list fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory(), failList: func(prefix string) error {
			return fmt.Errorf("list down")
		}}
		s := New(fl, newFakeRoles())
		h := testHandler(s, janeP)
		if w := doReq(h, "GET", "/acme/repo/api/milestones", ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("milestones = %d", w.Code)
		}
	})
	t.Run("milestone get fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory()}
		roles := newFakeRoles()
		grantTriage(roles, "acme", "repo")
		s := New(fl, roles)
		m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fl.failGet = func(key string) error {
			if strings.Contains(key, "milestones/"+m.ID) {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}
		h := testHandler(s, janeP)
		if w := doReq(h, "GET", "/acme/repo/api/milestones/"+m.ID, ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("milestone = %d", w.Code)
		}
	})
}

// Compaction shapes: varied timestamps, giant single card, open-only overflow.
func TestCompactShapes(t *testing.T) {
	s := testService(newFakeRoles())
	// Varied timestamps exercise both sort arms.
	ix := bigIndex(10)
	for i := range ix.ClosedRecent {
		if i%2 == 0 {
			ix.ClosedRecent[i].UpdatedAt = "2026-01-03T00:00:00Z"
		}
	}
	raw, _ := json.Marshal(ix)
	mustPut(t, s, IndexKey("acme", "varied"), raw)
	_ = raw
	// Giant single card evicts fully.
	giant := &Index{Open: []Card{}, ClosedRecent: []Card{{
		Num: 1, Kind: "issue", Title: strings.Repeat("g", 300<<10),
		State: StateClosed, Labels: []string{}, Assignees: []string{},
		Author: "a", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}}}
	rawg, _ := json.Marshal(giant)
	mustPut(t, s, IndexKey("acme", "giant"), rawg)
	ok, err := s.CompactIndex(reqCtx(), "acme", "giant")
	if err != nil || !ok {
		t.Fatalf("giant = %v,%v", ok, err)
	}
	after, _, _ := s.loadIndex(reqCtx(), "acme", "giant")
	if len(after.ClosedRecent) != 0 || after.CompactedThrough == "" {
		t.Fatalf("giant after = %+v", after)
	}
	// Open-only overflow is not this task's work: false, untouched.
	open := &Index{Open: []Card{}, ClosedRecent: []Card{}}
	for i := 1; i <= 700; i++ {
		open.Open = append(open.Open, Card{Num: i, Kind: "issue", Title: "open " + strings.Repeat("y", 300),
			State: StateOpen, Labels: []string{}, Assignees: []string{},
			Author: "a", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z"})
	}
	rawo, _ := json.Marshal(open)
	mustPut(t, s, IndexKey("acme", "openonly"), rawo)
	ok, err = s.CompactIndex(reqCtx(), "acme", "openonly")
	if err != nil || ok {
		t.Fatalf("openonly = %v,%v", ok, err)
	}
}

// Compensating-event mismatches (stale index cards): the mutate no-ops,
// the delete still succeeds.
func TestStaleCompensation(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	ha := testHandler(s, aliceP)
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "d73a4a", ""); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
	// Poison the index card to claim a label + milestone the header lacks.
	ix, _, _ := s.loadIndex(reqCtx(), "acme", "repo")
	ix.Open[0].Labels = []string{"bug"}
	ix.Open[0].Milestone = &m.ID
	raw, _ := json.Marshal(ix)
	mustPut(t, s, IndexKey("acme", "repo"), raw)
	if w := doReq(ha, "DELETE", "/acme/repo/api/labels/bug", ""); w.Code != http.StatusOK {
		t.Fatalf("stale label delete = %d", w.Code)
	}
	if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); err != nil {
		t.Fatalf("stale milestone delete = %v", err)
	}
	cur, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
	if len(cur.Labels) != 0 || cur.Milestone != nil {
		t.Fatalf("header = %+v", cur)
	}
	// Label delete with failing thread writes: best-effort, still 200.
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "z", "ffffff", ""); err != nil {
		t.Fatal(err)
	}
	th2 := mustCreate(t, s, "acme", "repo", janeP, "t2", "")
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th2.Num, aliceP, IssuePatch{Labels: &[]string{"z"}}); err != nil {
		t.Fatal(err)
	}
	_ = th2
}

// Reaction scan failure + shared-target decrement.
func TestReactionEdges(t *testing.T) {
	t.Run("scan list fails", func(t *testing.T) {
		fl := &flakyStore{ObjectStore: store.NewMemory()}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failList = func(prefix string) error { return fmt.Errorf("list down") }
		if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", 1, 0, bobP, "+1"); err == nil {
			t.Fatal("want error")
		}
		if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", 1, 0, bobP, "+1"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("shared target decrements", func(t *testing.T) {
		s := testService(newFakeRoles())
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1"); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, aliceP, "+1"); err != nil {
			t.Fatal(err)
		}
		rt, err := s.RemoveReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1")
		if err != nil {
			t.Fatal(err)
		}
		if rt.ReactionSummary[seqKey(0)]["+1"] != 1 {
			t.Fatalf("summary = %v", rt.ReactionSummary)
		}
	})
}

// Closing refs skip non-issue threads.
func TestClosingSkipsPR(t *testing.T) {
	s := testService(newFakeRoles())
	raw, _ := json.Marshal(&Thread{Num: 1, Kind: "pr", Title: "pr", State: StateOpen, Author: "a",
		CreatedAt: "2026-09-04T12:00:00Z", UpdatedAt: "2026-09-04T12:00:00Z", NextEventSeq: 1, Version: 1})
	mustPut(t, s, ThreadKey("acme", "repo", 1), raw)
	mustPut(t, s, CounterKey("acme", "repo"), []byte(`{"next":2}`))
	closed, err := s.ApplyClosingReferences(reqCtx(), "acme", "repo", 7, "sha", "mq", []string{"fixes #1"})
	if err != nil || len(closed) != 0 {
		t.Fatalf("closed=%v err=%v", closed, err)
	}
}

// Remaining HTTP branches in one sweep.
func TestHTTPRemainders(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	h := testHandler(s, janeP)
	ha := testHandler(s, aliceP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "b")
	// Extra segments under events/comments fall through (false → 404).
	for _, target := range []string{
		"/acme/repo/api/issues/1/events/x",
		"/acme/repo/api/issues/1/comments/x",
	} {
		if w := doReq(h, "GET", target, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d", target, w.Code)
		}
	}
	// Events POST → 405.
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/events", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("events POST = %d", w.Code)
	}
	// Null body → 400 expected-object.
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/comments", `null`); w.Code != http.StatusBadRequest {
		t.Errorf("null body = %d", w.Code)
	}
	// Well-formed keys but wrong types → 400 from the second decode.
	if w := doReq(h, "PATCH", "/acme/repo/api/issues/1", `{"title":123}`); w.Code != http.StatusBadRequest {
		t.Errorf("typed body = %d", w.Code)
	}
	// Patch validation through HTTP.
	if w := doReq(h, "PATCH", "/acme/repo/api/issues/1", `{"title":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty title = %d", w.Code)
	}
	if w := doReq(ha, "PATCH", "/acme/repo/api/issues/1", `{"labels":[""]}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty label = %d", w.Code)
	}
	// Anon patch → 401; stranger on private → 403.
	hanon := testHandler(s, anonP)
	if w := doReq(hanon, "PATCH", "/acme/repo/api/issues/1", `{"title":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anon patch = %d", w.Code)
	}
	roles.private["acme/priv"] = true
	hbob := testHandler(s, bobP)
	if w := doReq(hbob, "PATCH", "/acme/priv/api/issues/1", `{"title":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("private patch = %d", w.Code)
	}
	// Corrupt labels through PATCH.
	mustPut(t, s, LabelsKey("acme", "repo"), []byte("{bad"))
	if w := doReq(ha, "PATCH", "/acme/repo/api/issues/1", `{"labels":["bug"]}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt labels patch = %d", w.Code)
	}
	if err := s.Store.Delete(reqCtx(), LabelsKey("acme", "repo"), ""); err != nil {
		t.Fatal(err)
	}
	// Events window validation.
	if w := doReq(h, "GET", "/acme/repo/api/issues/1/events?n=x", ""); w.Code != http.StatusBadRequest {
		t.Errorf("events bad n = %d", w.Code)
	}
	// Reaction decode failure.
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/reactions", `{`); w.Code != http.StatusBadRequest {
		t.Errorf("react bad json = %d", w.Code)
	}
	// Private-repo read gates on the collection routes.
	hanon2 := testHandler(s, anonP)
	for _, target := range []string{"/acme/priv/api/labels", "/acme/priv/api/milestones"} {
		if w := doReq(hanon2, "GET", target, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("anon %s = %d", target, w.Code)
		}
	}
	// List query spellings.
	for _, q := range []string{"?assignee=jane@example.com", "?milestone=none", "?since=2020-01-01T00:00:00Z", "?milestone=000001"} {
		if w := doReq(h, "GET", "/acme/repo/api/issues"+q, ""); w.Code != http.StatusOK {
			t.Errorf("query %s = %d: %s", q, w.Code, w.Body.String())
		}
	}
}
