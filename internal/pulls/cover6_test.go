package pulls

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestCover6StartAuth(t *testing.T) {
	e := newTestEnv()
	e.roles.Public = false
	e.roles.Roles["jane@example.com"] = "write"
	openBasic(t, e, "o", "r")
	// StartMerge: anonymous + private stranger.
	if _, err := e.svc.StartMerge(ctx(), "o", "r", 1, auth.Anonymous(), MergeInput{Strategy: StrategyMerge}, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon: %v", err)
	}
	if _, err := e.svc.StartMerge(ctx(), "o", "r", 1, auth.Principal{Name: "ghost@x"}, MergeInput{Strategy: StrategyMerge}, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
	// UpdateBranch: anonymous + private stranger.
	if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, auth.Anonymous(), ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ub anon: %v", err)
	}
	if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, auth.Principal{Name: "ghost@x"}, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ub stranger: %v", err)
	}
	// StartFork: private stranger fails requireRead.
	if _, _, err := e.svc.StartFork(ctx(), "o", "r", auth.Principal{Name: "ghost@x"}, ForkInput{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("fork stranger: %v", err)
	}
}

func TestCover6RunMergeDirect(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		return e
	}
	rec := &TaskRecord{Progress: []string{}}
	t.Run("loadThread error", func(t *testing.T) {
		e := mk(t)
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
	})
	t.Run("missing thread", func(t *testing.T) {
		e := mk(t)
		_ = e.store.Delete(ctx(), ThreadKey("o", "r", 1), "")
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); !errors.Is(err, ErrNotFound) {
			t.Fatalf("thread: %v", err)
		}
	})
	t.Run("loadPR error", func(t *testing.T) {
		e := mk(t)
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("missing sidecar", func(t *testing.T) {
		e := mk(t)
		_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); !errors.Is(err, ErrNotFound) {
			t.Fatalf("pr: %v", err)
		}
	})
	t.Run("dirs outages", func(t *testing.T) {
		e := mk(t)
		e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("base dirs: %v", err)
		}
		e.svc.Dirs = &FakeDirs{}
	})
	t.Run("cross-fork head dirs down", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		e.svc.Dirs = &failForkDirs{ok: map[string]bool{"o/r": true}}
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("head dirs: %v", err)
		}
		e.svc.Dirs = &FakeDirs{}
	})
	t.Run("trial dirties under the task", func(t *testing.T) {
		e := mk(t)
		// Stamp trial clean, task trial dirty: the §5 step-1 race the
		// re-verify exists for (base moved between stamp and plan).
		e.git.TrialQueue = []TrialOutcome{
			{Tree: "44444444444444444444444444444444444444444444"},
			{Conflicts: []string{"z.go"}, Err: errDirty},
		}
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("task trial: %v", err)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatal("task-trial dirty must not publish")
		}
	})
	t.Run("rebase merge-base fails under the task", func(t *testing.T) {
		e := mk(t)
		e.git.MergeBaseQueue = []MergeBaseOutcome{{SHA: hexSHA(7)}, {Err: errors.New("no base")}}
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyRebase}, "", rec); err == nil {
			t.Fatal("rebase mb must fail")
		}
	})
	t.Run("replan unresolvable base", func(t *testing.T) {
		e := mk(t)
		e.refs.UpdateConflictOnce = true
		e.git.TrialQueue = []TrialOutcome{
			{Tree: "44444444444444444444444444444444444444444444"},
			{Tree: "44444444444444444444444444444444444444444444"},
		}
		// Delete the base on its 3rd resolve (step-1 stamp, stamp
		// recompute, then the replan resolve): deterministic mid-task
		// race via the resolve hook.
		mains := 0
		e.git.OnResolve = func(dir, ref string) {
			if ref == "refs/heads/main" {
				mains++
				if mains >= 3 {
					// Direct map delete: the hook runs inside
					// ResolveRef with the fake's mutex already held
					// (delGitRef would self-deadlock here).
					delete(e.git.Refs, "dir:o/r\x00refs/heads/main")
				}
			}
		}
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "unresolvable") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("replan dirty recompute", func(t *testing.T) {
		e := mk(t)
		e.refs.UpdateConflictOnce = true
		e.git.TrialQueue = []TrialOutcome{
			{Tree: "44444444444444444444444444444444444444444444"},
			{Tree: "44444444444444444444444444444444444444444444"},
			{Conflicts: []string{"m.go"}, Err: errDirty},
		}
		// Move the base on its 3rd resolve (after stamp + stamp
		// recompute): the replan diverges and the recompute trial is
		// dirty — "no longer clean".
		mains := 0
		e.git.OnResolve = func(dir, ref string) {
			if ref == "refs/heads/main" {
				mains++
				if mains >= 3 {
					e.git.Refs["dir:o/r\x00refs/heads/main"] = hexSHA(6)
				}
			}
		}
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "no longer clean") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("merged event outage narrated, publish stands", func(t *testing.T) {
		e := mk(t)
		// The merged event is seq 1 (opened was seq 0, no other events).
		e.failPutErr(EventKey("o", "r", 1, 1), errors.New("disk down"))
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategySquash}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		warned := false
		for _, p := range done.Progress {
			if strings.Contains(p, "merged event failed") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("no warning: %v", done.Progress)
		}
		pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		if pr == nil || !pr.Merged {
			t.Fatalf("publish stands: %+v", pr)
		}
		e.clearFails()
	})
	t.Run("rebase keeps commit texts", func(t *testing.T) {
		e := mk(t)
		e.git.LogRows = []CommitEntry{{SHA: hexSHA(2), Subject: "feat", Author: "j@x", At: "2026-09-04T12:00:00Z"}}
		e.closer.Closed = []int{}
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyRebase}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		found := false
		for _, tx := range e.closer.Texts {
			if strings.Contains(tx, "feat") {
				found = true
			}
		}
		if !found {
			t.Fatalf("texts = %q", e.closer.Texts)
		}
	})
}

func TestCover6UpdateBranchDirect(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(5), "refs/heads/topic": hexSHA(2)})
		e.refs.Refs["o/r"] = map[string]string{"refs/heads/topic": hexSHA(2)}
		return e
	}
	rec := &TaskRecord{Progress: []string{}}
	t.Run("loadThread error", func(t *testing.T) {
		e := mk(t)
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), rec); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("cross-fork head dirs down", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		e.svc.Dirs = &failForkDirs{ok: map[string]bool{"o/r": true}}
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), rec); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("head dirs: %v", err)
		}
		e.svc.Dirs = &FakeDirs{}
	})
	t.Run("base resolve down", func(t *testing.T) {
		e := mk(t)
		e.delGitRef("o/r", "refs/heads/main")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), rec); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("base: %v", err)
		}
	})
}

func TestCover6DeleteHeadDirect(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		return e
	}
	t.Run("loadThread error", func(t *testing.T) {
		e := mk(t)
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
	})
	t.Run("loadPR error", func(t *testing.T) {
		e := mk(t)
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
}

func TestCover6OpenDirect(t *testing.T) {
	t.Run("private stranger read fails", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Public = false
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", auth.Principal{Name: "ghost@x"}, OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("stranger: %v", err)
		}
	})
	t.Run("bad head ref", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "topic"}, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("head ref: %v", err)
		}
	})
	t.Run("cross-fork head dirs down", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.svc.Dirs = &failForkDirs{ok: map[string]bool{"o/r": true}}
		// Base resolves through the fake; head repo lookup fails.
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, ""); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("head dirs: %v", err)
		}
	})
}

func TestCover6SinkAndCommits(t *testing.T) {
	t.Run("non-pr cards skipped", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		ix, ver, _ := e.svc.loadIndex(ctx(), "o", "r")
		ix.Open = append(ix.Open, Card{Num: 77, Kind: "issue", Title: "ghost", State: "open", UpdatedAt: "2026-09-04T12:00:00Z"})
		raw, _ := json.Marshal(ix)
		if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), raw, store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
			t.Fatalf("ghost write: %v", err)
		}
		ghost, _, err := e.svc.loadIndex(ctx(), "o", "r")
		if err != nil || len(ghost.Open) != 2 {
			t.Fatalf("ghost write: %+v %v", ghost, err)
		}
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(8), "refs/heads/topic": hexSHA(2)})
		e.git.MergeBaseSHA = hexSHA(7)
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/main", Old: hexSHA(1), New: hexSHA(8)})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if c, _, _ := e.svc.loadMergeable(ctx(), "o", "r", 1); c != nil && c.BaseSHA == hexSHA(8) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("sink never recomputed")
	})
	t.Run("cross-fork unreachable diff uses fork dir", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		patch, err := e.svc.Diff(ctx(), "o", "r", 1, writer())
		if err != nil || patch == "" {
			t.Fatalf("fork diff: %q %v", patch, err)
		}
		// The fork dir was probed (not just the base dir).
		found := false
		for _, c := range e.git.CallLog() {
			if c == "diff" {
				found = true
			}
		}
		if !found {
			t.Fatal("no diff call")
		}
	})
	t.Run("commits truncation sets more", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		e.git.LogRows = []CommitEntry{
			{SHA: hexSHA(11), Subject: "a"}, {SHA: hexSHA(12), Subject: "b"}, {SHA: hexSHA(13), Subject: "c"},
		}
		rows, more, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), 0, 2)
		if err != nil || len(rows) != 2 || !more {
			t.Fatalf("trunc: %+v %v %v", rows, more, err)
		}
	})
	t.Run("diff pr load error", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); err == nil {
			t.Fatal("pr err must surface")
		}
		if _, _, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), 0, 10); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
}

func TestCover6HTTPLeftovers(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		seedOpened(t, e)
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
		return e
	}
	call := func(e *testEnv, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		return w
	}
	t.Run("commits extra segment", func(t *testing.T) {
		e := mk(t)
		req := httptest.NewRequest("GET", "/o/r/api/pulls/1/commits/extra", nil)
		w := httptest.NewRecorder()
		if e.h.Handle(w, req) {
			t.Fatalf("extra claimed: %d", w.Code)
		}
		w2 := httptest.NewRecorder()
		e.h.ServeHTTP(w2, req)
		if w2.Code != 404 {
			t.Fatalf("extra = %d", w2.Code)
		}
	})
	t.Run("oversize body refused", func(t *testing.T) {
		e := mk(t)
		if w := call(e, "POST", "/o/r/api/pulls", strings.Repeat("x", (1<<20)+1)); w.Code != 400 {
			t.Fatalf("oversize = %d", w.Code)
		}
	})
	t.Run("valid sort and after", func(t *testing.T) {
		e := mk(t)
		if w := call(e, "GET", "/o/r/api/pulls?sort=created", ""); w.Code != 200 {
			t.Fatalf("sort = %d (%s)", w.Code, w.Body.String())
		}
		if w := call(e, "GET", "/o/r/api/pulls?after=1&n=1", ""); w.Code != 200 {
			t.Fatalf("after = %d (%s)", w.Code, w.Body.String())
		}
		if w := call(e, "GET", "/o/r/api/pulls?n=0", ""); w.Code != 400 {
			t.Fatalf("n=0 = %d", w.Code)
		}
	})
	t.Run("valid skip", func(t *testing.T) {
		e := mk(t)
		e.git.LogRows = []CommitEntry{{SHA: hexSHA(2), Subject: "s"}}
		if w := call(e, "GET", "/o/r/api/pulls/1/commits?skip=1", ""); w.Code != 200 {
			t.Fatalf("skip = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("runFork corrupt index fails closed", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		_, _ = store.PutBytes(ctx(), e.store, ForksKey("o", "r"), []byte("{oops"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", "f", "c", writer(), rec); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt forks: %v", err)
		}
	})
}

func TestCover6RequireRoleAnon(t *testing.T) {
	e := newTestEnv()
	if err := e.svc.requireRole(ctx(), "o", "r", auth.Anonymous(), "write"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon role: %v", err)
	}
}
