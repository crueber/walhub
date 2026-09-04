package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- flaky/broken store wrappers -------------------------------------------------

// flakyStore injects governed failures into Put/Get/List/Delete.
type flakyStore struct {
	store.ObjectStore
	failPut    func(key string, n int) error
	failGet    func(key string) error
	failList   func(prefix string) error
	failDelete func(key string) error
	puts       map[string]int
}

func (f *flakyStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.puts == nil {
		f.puts = map[string]int{}
	}
	f.puts[key]++
	if f.failPut != nil {
		if err := f.failPut(key, f.puts[key]); err != nil {
			return store.ObjectMeta{}, err
		}
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *flakyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.failGet != nil {
		if err := f.failGet(key); err != nil {
			return nil, err
		}
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *flakyStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.failList != nil {
		if err := f.failList(prefix); err != nil {
			return err
		}
	}
	return f.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (f *flakyStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if f.failDelete != nil {
		if err := f.failDelete(key); err != nil {
			return err
		}
	}
	return f.ObjectStore.Delete(ctx, key, ifVersion)
}

func precond(key string) error { return store.NewPrecondition(key, "v9") }

// --- CAS-loop edge paths ------------------------------------------------------------

func TestAllocNumConflictExhausts(t *testing.T) {
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner, failPut: func(key string, n int) error {
		if strings.HasSuffix(key, "next_num") {
			return precond(key)
		}
		return nil
	}}
	s := New(fl, newFakeRoles())
	if _, err := s.allocNum(reqCtx(), "acme", "repo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestAllocNumCorrupt(t *testing.T) {
	s := testService(newFakeRoles())
	mustPut(t, s, CounterKey("acme", "repo"), []byte("{oops"))
	if _, err := s.allocNum(reqCtx(), "acme", "repo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
	mustPut(t, s, MilestoneCounterKey("acme", "repo"), []byte(`{"next":0}`))
	if _, err := s.allocMilestoneID(reqCtx(), "acme", "repo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("milestone err = %v", err)
	}
}

func TestAppendEventCreateConflictRetries(t *testing.T) {
	t.Run("create 412 reserves a gap and retries", func(t *testing.T) {
		inner := store.NewMemory()
		fl := &flakyStore{ObjectStore: inner}
		s := New(fl, newFakeRoles())
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failPut = func(key string, n int) error {
			if strings.HasSuffix(key, "/events/000000000001.json") {
				return precond(key)
			}
			return nil
		}
		ev, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "gap me")
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != 2 {
			t.Fatalf("seq = %d, want 2 (seq 1 skipped by the lost Create)", ev.Seq)
		}
	})
	t.Run("header CAS loss retries", func(t *testing.T) {
		inner := store.NewMemory()
		fl := &flakyStore{ObjectStore: inner}
		s := New(fl, newFakeRoles())
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failPut = func(key string, n int) error {
			if strings.HasSuffix(key, "thread.json") && n <= 3 {
				return precond(key)
			}
			return nil
		}
		ev, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "retry me")
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != 1 {
			t.Fatalf("seq = %d", ev.Seq)
		}
	})
}

func TestSaveMilestoneRetry(t *testing.T) {
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner}
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := New(fl, roles)
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fl.failPut = func(key string, n int) error {
		if strings.Contains(key, "milestones/") && !strings.Contains(key, "index.json") && n == 1 {
			return precond(key)
		}
		return nil
	}
	// First attempt 412s, the loop re-reads and retries to success.
	fl.puts = map[string]int{}
	um, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, strPtr("v2"), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if um.Title != "v2" {
		t.Fatalf("title = %q", um.Title)
	}
}

func TestUpdateIndexDropsAfterBound(t *testing.T) {
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner, failPut: func(key string, n int) error {
		if strings.HasSuffix(key, "index.json") {
			return precond(key)
		}
		return nil
	}}
	s := New(fl, newFakeRoles())
	th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
	// Must not error: 10 attempts then proceed without the index update.
	fl.puts = map[string]int{} // setup retries don't count
	s.updateIndex(reqCtx(), "acme", "repo", cardOf(th))
	if fl.puts[IndexKey("acme", "repo")] != 10 {
		t.Fatalf("puts = %d, want 10", fl.puts[IndexKey("acme", "repo")])
	}
}

func TestDeleteMilestoneDeleteConflict(t *testing.T) {
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner, failDelete: func(key string) error {
		return precond(key)
	}}
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := New(fl, roles)
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokenStoreSurfaces503(t *testing.T) {
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner, failGet: func(key string) error {
		return store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	s := New(fl, newFakeRoles())
	h := testHandler(s, janeP)
	if w := doReq(h, "GET", "/acme/repo/api/issues", ""); w.Code != http.StatusServiceUnavailable ||
		w.Header().Get("Retry-After") == "" {
		t.Fatalf("list = %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

// --- compaction ------------------------------------------------------------------------

func bigIndex(n int) *Index {
	ix := &Index{Open: []Card{}, ClosedRecent: []Card{}}
	for i := 1; i <= n; i++ {
		ix.ClosedRecent = append(ix.ClosedRecent, Card{
			Num: i, Kind: "issue", Title: "closed " + strings.Repeat("x", 300),
			State: StateClosed, Labels: []string{}, Assignees: []string{},
			Author: "jane@example.com", CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		})
	}
	return ix
}

func TestCompactIndex(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	// Absent → false, nil.
	if ok, err := s.CompactIndex(reqCtx(), "acme", "repo"); ok || err != nil {
		t.Fatalf("absent = %v,%v", ok, err)
	}
	// Small → false, nil.
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	if ok, err := s.CompactIndex(reqCtx(), "acme", "repo"); ok || err != nil {
		t.Fatalf("small = %v,%v", ok, err)
	}
	// Oversize → true; watermark advances; page halves; evicted still listable.
	raw, _ := json.Marshal(bigIndex(700))
	mustPut(t, s, IndexKey("acme", "big"), raw)
	ok, err := s.CompactIndex(reqCtx(), "acme", "big")
	if err != nil || !ok {
		t.Fatalf("compact = %v,%v", ok, err)
	}
	ix, _, _ := s.loadIndex(reqCtx(), "acme", "big")
	raw2, _ := json.Marshal(ix)
	if len(raw2) > IndexSizeLimit/2+4096 {
		t.Fatalf("compacted size = %d", len(raw2))
	}
	if ix.CompactedThrough == "" {
		t.Fatal("watermark did not advance")
	}
	// A retained closed card still lists; an evicted one is gone from the
	// index but the LIST fallback cannot serve it either (fabricated index
	// has no headers) — the watermark records the boundary.
	if len(ix.ClosedRecent) == 0 || len(ix.ClosedRecent) >= 700 {
		t.Fatalf("closed_recent = %d", len(ix.ClosedRecent))
	}
	// Corrupt → error.
	mustPut(t, s, IndexKey("acme", "corrupt"), []byte("{bad"))
	if _, err := s.CompactIndex(reqCtx(), "acme", "corrupt"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt err = %v", err)
	}
	// Inline trigger: an over-limit index compacts on the next mutation.
	raw3, _ := json.Marshal(bigIndex(700))
	mustPut(t, s, IndexKey("acme", "auto"), raw3)
	mustCreate(t, s, "acme", "auto", janeP, "fresh", "")
	ix3, _, _ := s.loadIndex(reqCtx(), "acme", "auto")
	if ix3.CompactedThrough == "" {
		t.Fatal("mutation did not trigger compaction")
	}
}

// --- LIST fallback ------------------------------------------------------------------------

func TestListFallbackWithoutIndex(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	for _, title := range []string{"a", "b", "c"} {
		mustCreate(t, s, "acme", "repo", janeP, title, "")
	}
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 2, aliceP, IssuePatch{State: strPtr("closed")}); err != nil {
		t.Fatal(err)
	}
	// Drop the index: reads fall back to LIST and stay complete.
	if err := s.Store.Delete(reqCtx(), IndexKey("acme", "repo"), ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{N: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 3 {
		t.Fatalf("fallback issues = %d", len(res.Issues))
	}
	closed, err := s.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{N: 50, State: StateClosed})
	if err != nil {
		t.Fatal(err)
	}
	if len(closed.Issues) != 1 || closed.Issues[0].Num != 2 {
		t.Fatalf("fallback closed = %+v", closed.Issues)
	}
	// A LIST failure degrades to the index window instead of erroring.
	fl := &flakyStore{ObjectStore: s.Store, failList: func(prefix string) error {
		return fmt.Errorf("list down")
	}}
	s2 := New(fl, roles)
	res2, err := s2.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{N: 50})
	if err != nil {
		t.Fatal(err)
	}
	_ = res2
}

func TestStaleCardReplacedByHeader(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mustCreate(t, s, "acme", "repo", janeP, "original", "")
	// Poison the index card; then simulate a lost update (counter ahead of
	// the cards) so the read falls through to LIST — the merged read must
	// show the header truth, not the stale card.
	ix, _, _ := s.loadIndex(reqCtx(), "acme", "repo")
	ix.Open[0].Title = "stale"
	raw, _ := json.Marshal(ix)
	mustPut(t, s, IndexKey("acme", "repo"), raw)
	mustPut(t, s, CounterKey("acme", "repo"), []byte(`{"next":3}`))
	res, err := s.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{N: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 1 || res.Issues[0].Title != "original" {
		t.Fatalf("merged = %+v", res.Issues)
	}
}

// --- filters -------------------------------------------------------------------------------

func TestListFilters(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	roles.grant("acme", "repo", "carol@example.com", "write")
	s := testService(roles)
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "d73a4a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "ui", "ffffff", ""); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	mustCreate(t, s, "acme", "repo", janeP, "c", "")
	mid := m.ID
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", a.Num, aliceP, IssuePatch{Labels: &[]string{"bug", "ui"}, Assignees: &[]string{"carol@example.com"}, Milestone: ptrTo(&mid)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", b.Num, aliceP, IssuePatch{Labels: &[]string{"bug"}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		f    ListFilter
		want []int
	}{
		{"label AND", ListFilter{Labels: []string{"bug", "ui"}}, []int{a.Num}},
		{"label case", ListFilter{Labels: []string{"BUG"}}, []int{b.Num, a.Num}},
		{"assignee", ListFilter{Assignee: "carol@example.com"}, []int{a.Num}},
		{"assignee none", ListFilter{Assignee: "*none"}, []int{3, 2}},
		{"milestone", ListFilter{Milestone: mid}, []int{a.Num}},
		{"milestone none", ListFilter{Milestone: "none"}, []int{3, 2}},
		{"since future", ListFilter{Since: "2027-01-01T00:00:00Z"}, []int{}},
		{"since past", ListFilter{Since: "2020-01-01T00:00:00Z"}, []int{3, 2, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.f.N = 50
			res, err := s.ListIssues(reqCtx(), "acme", "repo", janeP, c.f)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Issues) != len(c.want) {
				t.Fatalf("got %+v, want nums %v", res.Issues, c.want)
			}
			for i, n := range c.want {
				if res.Issues[i].Num != n {
					t.Fatalf("got %+v, want nums %v", res.Issues, c.want)
				}
			}
		})
	}
}

// --- pr-kind threads are not issues ------------------------------------------------------------

func TestPRKindRejected(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	// A 03-style pr thread shares the family but is invisible here.
	raw, _ := json.Marshal(&Thread{Num: 1, Kind: "pr", Title: "pr", State: StateOpen, Author: "jane@example.com",
		CreatedAt: "2026-09-04T12:00:00Z", UpdatedAt: "2026-09-04T12:00:00Z", NextEventSeq: 1, Version: 1})
	mustPut(t, s, ThreadKey("acme", "repo", 1), raw)
	mustPut(t, s, CounterKey("acme", "repo"), []byte(`{"next":2}`))
	h := testHandler(s, janeP)
	if w := doReq(h, "GET", "/acme/repo/api/issues/1", ""); w.Code != http.StatusNotFound {
		t.Errorf("get pr = %d", w.Code)
	}
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 1, aliceP, IssuePatch{Title: strPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patch pr = %v", err)
	}
	if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, aliceP, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("comment pr = %v", err)
	}
	if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", 1, 0, aliceP, "+1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("react pr = %v", err)
	}
	if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", 1, 0, aliceP, "+1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unreact pr = %v", err)
	}
	// ...and the LIST scan skips it (only the real issue lists).
	mustCreate(t, s, "acme", "repo", janeP, "real", "")
	if err := s.Store.Delete(reqCtx(), IndexKey("acme", "repo"), ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{N: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 1 || res.Issues[0].Num != 2 {
		t.Fatalf("scan = %+v", res.Issues)
	}
}

// --- idempotence / no-op paths ----------------------------------------------------------------------

func TestCloseTwiceIsNoop(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
	nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{State: strPtr("closed"), StateReason: strPtr("completed")})
	if err != nil {
		t.Fatal(err)
	}
	seq := nt.NextEventSeq
	nt2, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{State: strPtr("closed"), StateReason: strPtr("completed")})
	if err != nil {
		t.Fatal(err)
	}
	if nt2.NextEventSeq != seq {
		t.Fatalf("re-close consumed seq %d → %d", seq, nt2.NextEventSeq)
	}
}

func TestSameLengthLabelSwap(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	for _, l := range [][2]string{{"a", "111111"}, {"b", "222222"}, {"c", "333333"}} {
		if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, l[0], l[1], ""); err != nil {
			t.Fatal(err)
		}
	}
	th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Labels: &[]string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Labels: &[]string{"a", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(nt.Labels) != 2 || nt.Labels[0] != "a" || nt.Labels[1] != "c" {
		t.Fatalf("labels = %v", nt.Labels)
	}
	events, _ := s.scanEvents(reqCtx(), "acme", "repo", th.Num)
	last := events[len(events)-1]
	if last.Type != EventLabelsChanged || len(last.Added) != 1 || len(last.Removed) != 1 {
		t.Fatalf("delta = %+v", last)
	}
}

// --- corrupt objects ----------------------------------------------------------------------------

func TestCorruptObjects(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	h := testHandler(s, janeP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	// Corrupt thread → GET 503 with Retry-After.
	mustPut(t, s, ThreadKey("acme", "repo", 1), []byte("{bad"))
	if w := doReq(h, "GET", "/acme/repo/api/issues/1", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt thread = %d", w.Code)
	}
	// Corrupt index → list 503.
	mustPut(t, s, IndexKey("acme", "repo"), []byte("{bad"))
	if w := doReq(h, "GET", "/acme/repo/api/issues", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt index = %d", w.Code)
	}
	if err := s.Store.Delete(reqCtx(), IndexKey("acme", "repo"), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Delete(reqCtx(), ThreadKey("acme", "repo", 1), ""); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "acme", "repo", janeP, "t2", "")
	// Corrupt labels → GET labels 503; create-label 503.
	mustPut(t, s, LabelsKey("acme", "repo"), []byte("{bad"))
	if w := doReq(h, "GET", "/acme/repo/api/labels", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt labels = %d", w.Code)
	}
	ha := testHandler(s, aliceP)
	if w := doReq(ha, "POST", "/acme/repo/api/labels", `{"name":"x","color":"ffffff"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("create on corrupt = %d", w.Code)
	}
	if _, err := s.UpdateLabel(reqCtx(), "acme", "repo", aliceP, "x", strPtr("ffffff"), nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("update on corrupt = %v", err)
	}
	if _, err := s.DeleteLabel(reqCtx(), "acme", "repo", aliceP, "x"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("delete on corrupt = %v", err)
	}
	if err := s.Store.Delete(reqCtx(), LabelsKey("acme", "repo"), ""); err != nil {
		t.Fatal(err)
	}
	// Corrupt milestone → 503s.
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, MilestoneKey("acme", "repo", m.ID), []byte("{bad"))
	if w := doReq(h, "GET", "/acme/repo/api/milestones", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt milestone list = %d", w.Code)
	}
	if w := doReq(h, "GET", "/acme/repo/api/milestones/"+m.ID, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt milestone get = %d", w.Code)
	}
	if _, _, err := s.loadMilestone(reqCtx(), "acme", "repo", m.ID); !errors.Is(err, ErrCorrupt) {
		t.Errorf("load corrupt = %v", err)
	}
	// Corrupt event → thread read 503.
	mustPut(t, s, EventKey("acme", "repo", 2, 0), []byte("{bad"))
	if w := doReq(h, "GET", "/acme/repo/api/issues/2", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("corrupt event = %d", w.Code)
	}
}

// --- auth mapping -------------------------------------------------------------------------------

func authFailHandler(s *Service, kind auth.AuthErrorKind) *Handler {
	return &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: kind, Why: "nope"}
	}}
}

func TestAuthErrorMapping(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	for _, c := range []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrForbidden, http.StatusForbidden},
		{auth.ErrUnauthorized, http.StatusUnauthorized},
		{auth.ErrUnavailable, http.StatusServiceUnavailable},
	} {
		h := authFailHandler(s, c.kind)
		if w := doReq(h, "GET", "/acme/repo/api/issues", ""); w.Code != c.want {
			t.Errorf("kind %d = %d, want %d", c.kind, w.Code, c.want)
		}
	}
	// Identity down → read fails 503 through the service gate.
	roles.unavail = true
	h := testHandler(s, bobP)
	if w := doReq(h, "GET", "/acme/repo/api/issues", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unavail = %d", w.Code)
	}
	roles.unavail = false
	// Nil authenticator falls back to anonymous (public repo lists fine).
	h2 := &Handler{Svc: s}
	if w := doReq(h2, "GET", "/acme/repo/api/issues", ""); w.Code != http.StatusOK {
		t.Errorf("nil auth = %d", w.Code)
	}
}

// --- nil-role / nil-clock service ------------------------------------------------------------------

func TestBareService(t *testing.T) {
	s := New(store.NewMemory(), nil) // no roles, no clock, no emitter
	th, _, err := s.CreateIssue(reqCtx(), "acme", "repo", aliceP, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	if th.Author != "alice@example.com" {
		t.Fatalf("author = %q", th.Author)
	}
	// Admin flag short-circuits the nil-role resolution.
	if err := s.requireRole(reqCtx(), "acme", "repo", adminP, "admin"); err != nil {
		t.Fatalf("admin = %v", err)
	}
	if err := s.requireRole(reqCtx(), "acme", "repo", anonP, "read"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon = %v", err)
	}
	if err := s.requireRead(reqCtx(), "acme", "repo", anonP); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon read = %v", err)
	}
	// Stream hook fires.
	var streamed []StreamEvent
	s.Stream = func(ctx context.Context, ev StreamEvent) { streamed = append(streamed, ev) }
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, aliceP, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 || streamed[0].Name != "issue_event" {
		t.Fatalf("streamed = %+v", streamed)
	}
}

// --- unit coverage for small helpers ------------------------------------------------------------------

func TestHelpers(t *testing.T) {
	if roleRank("admin") != 5 || roleRank("bogus") != 0 || roleRank("TRIAGE") != 2 {
		t.Fatal("roleRank")
	}
	keys := map[string]string{
		"counter":   CounterKey("o", "r"),
		"thread":    ThreadKey("o", "r", 7),
		"threadPre": ThreadPrefix("o", "r", 7),
		"event":     EventKey("o", "r", 7, 9),
		"eventsPre": EventsPrefix("o", "r", 7),
		"prefix":    IssuesPrefix("o", "r"),
		"index":     IndexKey("o", "r"),
		"labels":    LabelsKey("o", "r"),
		"ms":        MilestoneKey("o", "r", "000001"),
		"msPre":     MilestonePrefix("o", "r"),
		"msIdx":     MilestoneCounterKey("o", "r"),
	}
	for name, k := range keys {
		if k == "" || strings.Contains(k, " ") {
			t.Errorf("key %s = %q", name, k)
		}
	}
	if ThreadKey("o", "r", 7) != "repos/o/r/issues/000007/thread.json" {
		t.Fatalf("thread key = %q", ThreadKey("o", "r", 7))
	}
	if EventKey("o", "r", 7, 9) != "repos/o/r/issues/000007/events/000000000009.json" {
		t.Fatalf("event key = %q", EventKey("o", "r", 7, 9))
	}
	if seqKey(3) != "000003" {
		t.Fatalf("seqKey = %q", seqKey(3))
	}
	if n, err := parseNum("12"); n != 12 || err != nil {
		t.Fatalf("parseNum = %d,%v", n, err)
	}
	if _, err := parseNum("99999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parseNum overflow = %v", err)
	}
	if matchETag("*", "v1") != true || matchETag(`W/"v1", "v2"`, "v1") != true || matchETag(`"v9"`, "v1") != false {
		t.Fatal("matchETag")
	}
	if milestonePercent(0, 0) != 0 || milestonePercent(1, 1) != 50 {
		t.Fatal("milestonePercent")
	}
	if !equalStr([]string{"a"}, []string{"a"}) || equalStr([]string{"a"}, []string{"a", "b"}) || equalStr([]string{"a"}, []string{"b"}) {
		t.Fatal("equalStr")
	}
	if !equalPtr(nil, nil) || !equalPtr(strPtr("a"), strPtr("a")) || equalPtr(nil, strPtr("a")) || equalPtr(strPtr("a"), nil) || equalPtr(strPtr("a"), strPtr("b")) {
		t.Fatal("equalPtr")
	}
	// upsertCard carries both kinds (one index, one numbering space);
	// issue reads filter kind:"issue" (filterCards).
	ix := &Index{Open: []Card{}, ClosedRecent: []Card{}}
	upsertCard(ix, Card{Num: 5, Kind: "pr", State: StateOpen})
	if len(ix.Open) != 1 {
		t.Fatalf("pr card dropped: %+v", ix)
	}
	if got := filterCards(append([]Card{}, ix.Open...), ListFilter{}); len(got) != 0 {
		t.Fatalf("pr card listed as issue: %+v", got)
	}
	upsertCard(ix, Card{Num: 5, Kind: "issue", State: StateOpen, UpdatedAt: "2026-09-04T12:00:00Z"})
	upsertCard(ix, Card{Num: 5, Kind: "issue", State: StateClosed, UpdatedAt: "2026-09-04T13:00:00Z"})
	if len(ix.Open) != 0 || len(ix.ClosedRecent) != 1 {
		t.Fatalf("upsert = %+v", ix)
	}
	// reactionState folds add/remove.
	add, remove := "add", "remove"
	folded := reactionState([]*Event{
		{Type: EventReactionChanged, Actor: "a", TargetEventSeq: intPtr(0), Content: strPtr("+1"), Op: &add},
		{Type: EventReactionChanged, Actor: "a", TargetEventSeq: intPtr(0), Content: strPtr("+1"), Op: &remove},
		{Type: EventCommented},
	})
	if folded["a\x000\x00+1"] {
		t.Fatal("fold kept a removed reaction")
	}
	// statusFor covers every sentinel.
	for _, c := range []struct {
		err  error
		want int
	}{
		{ErrInvalid, 400}, {ErrNotFound, 404}, {ErrForbidden, 403},
		{ErrUnauthorized, 401}, {ErrConflict, 409}, {ErrCorrupt, 503},
		{fmt.Errorf("other"), 503},
	} {
		if statusFor(c.err) != c.want {
			t.Errorf("statusFor(%v) = %d, want %d", c.err, statusFor(c.err), c.want)
		}
	}
	// encode/decode round-trips normalize nil slices (never null).
	th := &Thread{Num: 1}
	if string(encodeThread(th)) == "" {
		t.Fatal("encodeThread empty")
	}
	pt, err := parseThread([]byte(`{"num":1}`))
	if err != nil || pt.Labels == nil || pt.Assignees == nil || pt.Participants == nil || pt.ReactionSummary == nil {
		t.Fatalf("parseThread = %+v,%v", pt, err)
	}
	if _, err := parseThread([]byte("{bad")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("parseThread bad = %v", err)
	}
	if _, err := parseEvent([]byte("{bad")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("parseEvent bad = %v", err)
	}
	px, err := parseIndex([]byte(`{}`))
	if err != nil || px.Open == nil || px.ClosedRecent == nil {
		t.Fatalf("parseIndex = %+v,%v", px, err)
	}
	// writeJSON encode failure → 500.
	w := httptest.NewRecorder()
	writeJSON(w, 200, map[string]any{"f": func() {}})
	if w.Code != 500 {
		t.Fatalf("writeJSON bad = %d", w.Code)
	}
	w2 := httptest.NewRecorder()
	writeCached(w2, httptest.NewRequest("GET", "/", nil), ccSWR, "etag1", 200, map[string]any{"f": func() {}})
	if w2.Code != 500 {
		t.Fatalf("writeCached bad = %d", w2.Code)
	}
}

func intPtr(n int) *int { return &n }

// --- remaining HTTP branches ----------------------------------------------------------------------------

func TestHTTPEdges(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	h := testHandler(s, janeP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "b")
	// .git-suffixed lane resolves the same repo.
	if w := doReq(h, "GET", "/acme/repo.git/api/issues/1", ""); w.Code != http.StatusOK {
		t.Errorf("git lane = %d", w.Code)
	}
	// Unknown route family falls through (false).
	if w := doReq(h, "GET", "/acme/repo/api/bogus", ""); w.Code != http.StatusNotFound {
		t.Errorf("bogus = %d", w.Code)
	}
	if w := doReq(h, "GET", "/x", ""); w.Code != http.StatusNotFound {
		t.Errorf("short = %d", w.Code)
	}
	// Bad path encoding survives (per-segment fallback) → 404 unknown issue.
	badReq := &http.Request{Method: "GET", URL: &url.URL{Path: "/acme/repo/api/issues/%zz", RawPath: "/acme/repo/api/issues/%zz"}}
	badW := httptest.NewRecorder()
	h.ServeHTTP(badW, badReq)
	if badW.Code != http.StatusNotFound {
		t.Errorf("bad encoding = %d", badW.Code)
	}
	// Overflow num → 404.
	if w := doReq(h, "GET", "/acme/repo/api/issues/99999999", ""); w.Code != http.StatusNotFound {
		t.Errorf("overflow = %d", w.Code)
	}
	// ETag "*" and weak prefixes match → 304.
	w := doReq(h, "GET", "/acme/repo/api/issues/1", "")
	_ = w
	for _, inm := range []string{"*", `W/"v1"`} {
		r := httptest.NewRequest("GET", "/acme/repo/api/issues/1", nil)
		r.Header.Set("If-None-Match", inm)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, r)
		if w2.Code != http.StatusNotModified {
			t.Errorf("inm %q = %d", inm, w2.Code)
		}
	}
	// Events n clamps at 200.
	if w := doReq(h, "GET", "/acme/repo/api/issues/1/events?n=500", ""); w.Code != http.StatusOK {
		t.Errorf("clamp = %d", w.Code)
	}
	// Ghost events → 404 through the events route.
	if w := doReq(h, "GET", "/acme/repo/api/issues/9/events", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost events = %d", w.Code)
	}
	// Ghost reactions → 404; anon comment/react → 401.
	if w := doReq(h, "POST", "/acme/repo/api/issues/9/reactions", `{"target_event_seq":0,"content":"+1"}`); w.Code != http.StatusNotFound {
		t.Errorf("ghost react = %d", w.Code)
	}
	if w := doReq(h, "DELETE", "/acme/repo/api/issues/9/reactions/0/%2B1", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost unreact = %d", w.Code)
	}
	ha := testHandler(s, anonP)
	if w := doReq(ha, "POST", "/acme/repo/api/issues/1/comments", `{"body":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anon comment = %d", w.Code)
	}
	if w := doReq(ha, "POST", "/acme/repo/api/issues/1/reactions", `{"target_event_seq":0,"content":"+1"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anon react = %d", w.Code)
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/issues/1/reactions/0/%2B1", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon unreact = %d", w.Code)
	}
	// Oversized body → 400; non-object JSON → 400.
	big := `{"body":"` + strings.Repeat("x", 2<<20) + `"}`
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/comments", big); w.Code != http.StatusBadRequest {
		t.Errorf("oversize = %d", w.Code)
	}
	if w := doReq(h, "POST", "/acme/repo/api/issues/1/comments", `[1,2]`); w.Code != http.StatusBadRequest {
		t.Errorf("array body = %d", w.Code)
	}
	// Label/milestone error variants.
	hb := testHandler(s, bobP)
	if w := doReq(hb, "PATCH", "/acme/repo/api/labels/bug", `{"color":"ffffff"}`); w.Code != http.StatusForbidden {
		t.Errorf("bob label patch = %d", w.Code)
	}
	ht := testHandler(s, aliceP)
	if w := doReq(ht, "POST", "/acme/repo/api/labels", `{"name":"toolong`+strings.Repeat("x", 60)+`","color":"ffffff"}`); w.Code != http.StatusBadRequest {
		t.Errorf("long label = %d", w.Code)
	}
	if w := doReq(ht, "POST", "/acme/repo/api/labels", `{"name":"x","color":"ffffff","description":"`+strings.Repeat("y", 201)+`"}`); w.Code != http.StatusBadRequest {
		t.Errorf("long desc = %d", w.Code)
	}
	if w := doReq(ht, "POST", "/acme/repo/api/labels", `{"name":"x","color":"ffffff","bogus":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("label unknown key = %d", w.Code)
	}
	if w := doReq(ht, "PATCH", "/acme/repo/api/labels/ghost", `{"color":"ffffff"}`); w.Code != http.StatusNotFound {
		t.Errorf("ghost label = %d", w.Code)
	}
	if w := doReq(ht, "DELETE", "/acme/repo/api/labels/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost label delete = %d", w.Code)
	}
	if w := doReq(hb, "DELETE", "/acme/repo/api/labels/ghost", ""); w.Code != http.StatusForbidden {
		t.Errorf("bob label delete = %d", w.Code)
	}
	if w := doReq(hb, "POST", "/acme/repo/api/milestones", `{"title":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("bob milestone = %d", w.Code)
	}
	if w := doReq(ht, "POST", "/acme/repo/api/milestones", `{"title":"x","due_on":"soon"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad due = %d", w.Code)
	}
	if w := doReq(ht, "POST", "/acme/repo/api/milestones", `{"title":"","x":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("ms unknown key = %d", w.Code)
	}
	if w := doReq(ht, "PATCH", "/acme/repo/api/milestones/0000ff", `{"title":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("ghost milestone = %d", w.Code)
	}
	if w := doReq(hb, "DELETE", "/acme/repo/api/milestones/000001", ""); w.Code != http.StatusForbidden {
		t.Errorf("bob ms delete = %d", w.Code)
	}
	if w := doReq(ht, "DELETE", "/acme/repo/api/milestones/0000ff", ""); w.Code != http.StatusNotFound {
		t.Errorf("ghost ms delete = %d", w.Code)
	}
	// Milestone patch validation through HTTP.
	mw := doReq(ht, "POST", "/acme/repo/api/milestones", `{"title":"v9"}`)
	var mc struct {
		Milestone *Milestone `json:"milestone"`
	}
	if err := json.Unmarshal(mw.Body.Bytes(), &mc); err != nil {
		t.Fatal(err)
	}
	id := mc.Milestone.ID
	if w := doReq(ht, "PATCH", "/acme/repo/api/milestones/"+id, `{"due_on":"never"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad due patch = %d", w.Code)
	}
	if w := doReq(ht, "PATCH", "/acme/repo/api/milestones/"+id, `{"state":"shipped"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad state patch = %d", w.Code)
	}
	if w := doReq(ht, "PATCH", "/acme/repo/api/milestones/"+id, `{"title":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty title patch = %d", w.Code)
	}
	if w := doReq(ht, "PATCH", "/acme/repo/api/milestones/"+id, `{"due_on":""}`); w.Code != http.StatusOK {
		t.Errorf("clear due = %d: %s", w.Code, w.Body.String())
	}
	// Direct saveMilestone Create path + bump edge paths.
	m2 := &Milestone{ID: "0000aa", Title: "direct", State: StateOpen, CreatedBy: "a", CreatedAt: "x"}
	if err := s.saveMilestone(reqCtx(), "acme", "repo", m2, ""); err != nil {
		t.Fatalf("direct create = %v", err)
	}
	s.bumpMilestone(reqCtx(), "acme", "repo", "0000ff", 1, 0) // absent: no-op
	mustPut(t, s, MilestoneKey("acme", "repo", "0000ab"), []byte("{bad"))
	s.bumpMilestone(reqCtx(), "acme", "repo", "0000ab", 1, 0) // corrupt: no-op
	s.bumpMilestone(reqCtx(), "acme", "repo", "", 1, 0)       // empty: no-op
	// Direct casUpdate attempts<=0 defaults; validation abort passes through.
	if _, err := s.casUpdate(reqCtx(), CounterKey("acme", "zero"), 0, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return nil, false, nil
	}); err != nil {
		t.Fatalf("no-write cas = %v", err)
	}
}

func mustPut(t *testing.T, s *Service, key string, body []byte) {
	t.Helper()
	if _, err := store.PutBytes(reqCtx(), s.Store, key, body, store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}
