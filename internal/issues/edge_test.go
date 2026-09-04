package issues

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// jsonMarshal renders one value (test helper; panics on failure... no:
// reports through mustPut-style errors via the caller).
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// Every route maps an authenticator failure to its status (401/403/503),
// covering each handler's principal-error branch in one sweep.
func TestPrincipalErrorSweep(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	mustCreate(t, s, "acme", "repo", janeP, "t", "b")
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct{ method, target, body string }{
		{"GET", "/acme/repo/api/issues", ""},
		{"POST", "/acme/repo/api/issues", `{"title":"x"}`},
		{"GET", "/acme/repo/api/issues/1", ""},
		{"GET", "/acme/repo/api/issues/1/events", ""},
		{"PATCH", "/acme/repo/api/issues/1", `{"title":"x"}`},
		{"POST", "/acme/repo/api/issues/1/comments", `{"body":"x"}`},
		{"POST", "/acme/repo/api/issues/1/reactions", `{"target_event_seq":0,"content":"+1"}`},
		{"DELETE", "/acme/repo/api/issues/1/reactions/0/%2B1", ""},
		{"GET", "/acme/repo/api/labels", ""},
		{"POST", "/acme/repo/api/labels", `{"name":"x","color":"ffffff"}`},
		{"PATCH", "/acme/repo/api/labels/x", `{"color":"ffffff"}`},
		{"DELETE", "/acme/repo/api/labels/x", ""},
		{"GET", "/acme/repo/api/milestones", ""},
		{"POST", "/acme/repo/api/milestones", `{"title":"x"}`},
		{"GET", "/acme/repo/api/milestones/" + m.ID, ""},
		{"PATCH", "/acme/repo/api/milestones/" + m.ID, `{"title":"y"}`},
		{"DELETE", "/acme/repo/api/milestones/" + m.ID, ""},
	}
	for _, c := range []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrUnauthorized, http.StatusUnauthorized},
		{auth.ErrForbidden, http.StatusForbidden},
		{auth.ErrUnavailable, http.StatusServiceUnavailable},
	} {
		h := authFailHandler(s, c.kind)
		for _, rt := range routes {
			if w := doReq(h, rt.method, rt.target, rt.body); w.Code != c.want {
				t.Errorf("kind %d %s %s = %d, want %d: %s", c.kind, rt.method, rt.target, w.Code, c.want, w.Body.String())
			}
		}
	}
}

// Mutations against a store that fails reads/writes surface errors
// instead of partial state.
func TestMutationStoreFailures(t *testing.T) {
	newSvc := func() (*Service, *flakyStore) {
		inner := store.NewMemory()
		fl := &flakyStore{ObjectStore: inner}
		roles := newFakeRoles()
		grantTriage(roles, "acme", "repo")
		return New(fl, roles), fl
	}
	t.Run("comment get fails", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error { return store.NewRetryable(key, fmt.Errorf("down")) }
		if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "x"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("patch get fails", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error { return store.NewRetryable(key, fmt.Errorf("down")) }
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 1, aliceP, IssuePatch{Title: strPtr("x")}); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("react get fails", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error { return store.NewRetryable(key, fmt.Errorf("down")) }
		if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", 1, 0, bobP, "+1"); err == nil {
			t.Fatal("want error")
		}
		if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", 1, 0, bobP, "+1"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("thread CAS always loses", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failPut = func(key string, n int) error {
			if strings.HasSuffix(key, "thread.json") {
				return precond(key)
			}
			return nil
		}
		if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "x"); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("event write fails hard", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failPut = func(key string, n int) error {
			if strings.Contains(key, "/events/") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}
		if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, bobP, "x"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("alloc get fails", func(t *testing.T) {
		s, fl := newSvc()
		fl.failGet = func(key string) error { return store.NewRetryable(key, fmt.Errorf("down")) }
		if _, err := s.allocNum(reqCtx(), "acme", "repo"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("create races an existing header", func(t *testing.T) {
		s, _ := newSvc()
		mustPut(t, s, CounterKey("acme", "repo"), []byte(`{"next":1}`))
		raw, _ := jsonMarshal(&Thread{Num: 1, Kind: "issue"})
		mustPut(t, s, ThreadKey("acme", "repo", 1), raw)
		if _, _, err := s.CreateIssue(reqCtx(), "acme", "repo", janeP, "t", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("milestone put fails", func(t *testing.T) {
		s, fl := newSvc()
		fl.failPut = func(key string, n int) error {
			if strings.Contains(key, "milestones/") {
				return store.NewRetryable(key, fmt.Errorf("down"))
			}
			return nil
		}
		if _, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("milestone id clash", func(t *testing.T) {
		s, _ := newSvc()
		mustPut(t, s, MilestoneCounterKey("acme", "repo"), []byte(`{"next":1}`))
		mustPut(t, s, MilestoneKey("acme", "repo", "000001"), []byte(`{}`))
		if _, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("milestone update always loses", func(t *testing.T) {
		s, fl := newSvc()
		m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fl.failPut = func(key string, n int) error {
			if strings.Contains(key, "milestones/") && !strings.Contains(key, "index.json") {
				return precond(key)
			}
			return nil
		}
		if _, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, strPtr("v2"), nil, nil, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("thread read fails mid-list", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failList = func(prefix string) error { return fmt.Errorf("list down") }
		if _, err := s.GetThread(reqCtx(), "acme", "repo", 1, janeP, 0, 10); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("index write skipped silently when reads fail", func(t *testing.T) {
		s, fl := newSvc()
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error { return store.NewRetryable(key, fmt.Errorf("down")) }
		s.updateIndex(reqCtx(), "acme", "repo", cardOf(th)) // must not panic
	})
	t.Run("closing refs tolerate store failure", func(t *testing.T) {
		s, fl := newSvc()
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		fl.failGet = func(key string) error { return store.NewRetryable(key, fmt.Errorf("down")) }
		closed, err := s.ApplyClosingReferences(reqCtx(), "acme", "repo", 9, "sha", "mq", []string{"fixes #1"})
		if err != nil || len(closed) != 0 {
			t.Fatalf("closed=%v err=%v", closed, err)
		}
	})
	t.Run("label delete tolerates corrupt index", func(t *testing.T) {
		s, _ := newSvc()
		if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "d73a4a", ""); err != nil {
			t.Fatal(err)
		}
		mustPut(t, s, IndexKey("acme", "repo"), []byte("{bad"))
		if n, err := s.DeleteLabel(reqCtx(), "acme", "repo", aliceP, "bug"); err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
	t.Run("milestone delete tolerates list failure", func(t *testing.T) {
		s, fl := newSvc()
		m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fl.failList = func(prefix string) error { return fmt.Errorf("list down") }
		if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

// Remaining validation branches.
func TestValidationEdges(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	// Assignee ceiling and principal shape.
	many := make([]string, 0, MaxAssignees+1)
	for i := 0; i <= MaxAssignees; i++ {
		many = append(many, fmt.Sprintf("u%d@example.com", i))
	}
	if _, err := s.checkAssignees(reqCtx(), "acme", "repo", many); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ceiling = %v", err)
	}
	if _, err := s.checkAssignees(reqCtx(), "acme", "repo", []string{"not-an-email"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shape = %v", err)
	}
	// Writer flag resolves without bindings on a bare service.
	bare := New(store.NewMemory(), nil)
	if got := bare.roleOf(reqCtx(), "o", "r", writerP); got != "write" {
		t.Fatalf("writer = %q", got)
	}
	if got := bare.roleOf(reqCtx(), "o", "r", anonP); got != "" {
		t.Fatalf("anon = %q", got)
	}
	// roleRank across the ladder.
	for _, c := range []struct {
		role string
		want int
	}{
		{"read", 1}, {"triage", 2}, {"write", 3}, {"maintain", 4}, {"admin", 5},
		{"READ", 1}, {"Maintain", 4}, {"", 0}, {"root", 0},
	} {
		if roleRank(c.role) != c.want {
			t.Errorf("roleRank(%q) = %d, want %d", c.role, roleRank(c.role), c.want)
		}
	}
	// sortCards tiebreaks by num desc; uniqSorted empties stay [].
	sortCards(nil)
	pool := []Card{{Num: 1, UpdatedAt: "2026-09-04T12:00:00Z"}, {Num: 2, UpdatedAt: "2026-09-04T12:00:00Z"}, {Num: 3, UpdatedAt: "2026-09-03T12:00:00Z"}}
	sortCards(pool)
	if pool[0].Num != 2 || pool[1].Num != 1 || pool[2].Num != 3 {
		t.Fatalf("sorted = %+v", pool)
	}
	if got := uniqSorted(nil); got == nil || len(got) != 0 {
		t.Fatalf("uniqSorted(nil) = %#v", got)
	}
	// Event windows: older-on-demand with more, and the n cap.
	th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
	for _, b := range []string{"c1", "c2", "c3"} {
		if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, b); err != nil {
			t.Fatal(err)
		}
	}
	view, err := s.GetThread(reqCtx(), "acme", "repo", th.Num, janeP, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Events) != 2 || !view.EventsMore || view.Events[0].Seq != 3 {
		t.Fatalf("window = %+v more=%v", view.Events, view.EventsMore)
	}
	view, err = s.GetThread(reqCtx(), "acme", "repo", th.Num, janeP, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Events) != 4 || view.EventsMore {
		t.Fatalf("capped = %d more=%v", len(view.Events), view.EventsMore)
	}
	// Re-close with a different reason records a new event.
	nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{State: strPtr("closed")})
	if err != nil {
		t.Fatal(err)
	}
	_ = nt
	nt2, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{State: strPtr("closed"), StateReason: strPtr("not_planned")})
	if err != nil {
		t.Fatal(err)
	}
	if nt2.NextEventSeq != nt.NextEventSeq+1 || nt2.StateReason == nil || *nt2.StateReason != ReasonNotPlanned {
		t.Fatalf("reason change = %+v", nt2)
	}
	// Anon label create is 401 (not 403): attribution first.
	ha := testHandler(s, anonP)
	if w := doReq(ha, "POST", "/acme/repo/api/labels", `{"name":"x","color":"ffffff"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anon label = %d", w.Code)
	}
	if w := doReq(ha, "POST", "/acme/repo/api/milestones", `{"title":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anon milestone = %d", w.Code)
	}
}
