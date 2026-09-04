package pulls

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestCover8GitShapes(t *testing.T) {
	ctx := context.Background()
	t.Run("rev-parse garbage is unknown", func(t *testing.T) {
		bin := fakeGitScript(t, `echo "garbage"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.ResolveRef(ctx, t.TempDir(), "refs/heads/main"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("resolve: %v", err)
		}
	})
	t.Run("empty merge-tree is corrupt", func(t *testing.T) {
		bin := fakeGitScript(t, `exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, _, err := g.TrialMerge(ctx, t.TempDir(), "a", "b"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("trial: %v", err)
		}
	})
	t.Run("commit-tree garbage is corrupt", func(t *testing.T) {
		bin := fakeGitScript(t, `echo "garbage"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.CommitTree(ctx, t.TempDir(), "t", []string{"p"}, "m", "a", "a@x", "c", "c@x", time.Now()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("commit-tree: %v", err)
		}
	})
	t.Run("replay success returns the tip", func(t *testing.T) {
		sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		bin := fakeGitScript(t, "echo "+sha+"; exit 0")
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		// MergeBase succeeds (garbage is not valid here — use the sha for
		// both so validation passes).
		got, err := g.Replay(ctx, t.TempDir(), sha, sha, sha)
		if err != nil || got != sha {
			t.Fatalf("replay = %q %v", got, err)
		}
	})
	t.Run("log default n and malformed rows", func(t *testing.T) {
		// dash printf has no \xHH (octal only): \000 is the NUL the
		// --format=%H%x00… separator needs.
		bin := fakeGitScript(t, "printf 'BADLINE\\n0123456789abcdef0123456789abcdef01234567\\000s\\000a\\000t\\n'; exit 0")
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		rows, err := g.LogRange(ctx, t.TempDir(), "b", "h", 0, 0)
		if err != nil || len(rows) != 1 || rows[0].SHA == "" {
			t.Fatalf("log = %+v %v", rows, err)
		}
	})
	t.Run("merge-base garbage is unprocessable", func(t *testing.T) {
		bin := fakeGitScript(t, `echo "garbage"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		// Covered in cover5; re-asserted here for the restructured runner.
		if _, err := g.MergeBase(ctx, t.TempDir(), "a", "b"); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("mb: %v", err)
		}
	})
}

func TestCover8ServiceLeftovers(t *testing.T) {
	t.Run("appendEvent header outage", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.failPutErr(ThreadKey("o", "r", 1), errors.New("disk down"))
		if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "x"); err == nil {
			t.Fatal("header err must surface")
		}
		e.clearFails()
	})
	t.Run("loadIndex corrupt", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		_, ver, _ := e.svc.getJSON(ctx(), IndexKey("o", "r"))
		_, _ = store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{oops"), store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
		if _, _, err := e.svc.loadIndex(ctx(), "o", "r"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("index: %v", err)
		}
	})
	t.Run("updateIndex put outage drops silently", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1)
		e.failPutErr(IndexKey("o", "r"), errors.New("disk down"))
		e.svc.updateIndex(ctx(), "o", "r", prCardOf(th)) // best-effort: no error, no crash
		e.clearFails()
	})
	t.Run("open alloc outage", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		_, _ = store.PutBytes(ctx(), e.store, CounterKey("o", "r"), []byte(`{"next":0}`), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("alloc: %v", err)
		}
	})
	t.Run("open thread outage", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		// Generic (non-412) failure on the thread Create only: allocNum
		// uses the counter key, so it still succeeds.
		e.failPutErr(ThreadKey("o", "r", 1), errors.New("disk down"))
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); err == nil {
			t.Fatal("thread put must surface")
		}
		e.clearFails()
	})
	t.Run("open sidecar outage", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		e.failPutErr(PRKey("o", "r", 1), errors.New("disk down"))
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); err == nil {
			t.Fatal("sidecar put must surface")
		}
		e.clearFails()
	})
	t.Run("getPR load outages", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("commits requireRead fails", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Public = false
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		if _, _, err := e.svc.Commits(ctx(), "o", "r", 1, auth.Principal{Name: "ghost@x"}, 0, 10); !errors.Is(err, ErrForbidden) {
			t.Fatalf("commits read: %v", err)
		}
	})
	t.Run("updateBranch load outages", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), ""); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), ""); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("trial generic error under task", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		e.git.TrialQueue = []TrialOutcome{
			{Tree: "44444444444444444444444444444444444444444444"},
			{Err: errors.New("git down")},
		}
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "git down") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("replan clean retries", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		e.refs.UpdateConflictOnce = true
		e.git.TrialQueue = []TrialOutcome{
			{Tree: "44444444444444444444444444444444444444444444"},
			{Tree: "44444444444444444444444444444444444444444444"},
			{Tree: "44444444444444444444444444444444444444444444"},
		}
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
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "re-planned") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("timeline windows", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		for _, body := range []string{"one", "two", "three"} {
			if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), body); err != nil {
				t.Fatalf("comment: %v", err)
			}
		}
		view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
		if err != nil || len(view.Events) != 4 || view.EventsMore {
			t.Fatalf("timeline: %+v %v", view.Events, err)
		}
		if view.Events[0].Seq != 3 {
			t.Fatalf("newest-first: %+v", view.Events[0])
		}
		paged, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 3, 50)
		if err != nil || len(paged.Events) != 3 || paged.EventsMore {
			t.Fatalf("paged: %+v %v", paged.Events, err)
		}
		if paged.Events[0].Seq != 2 {
			t.Fatalf("window: %+v", paged.Events[0])
		}
		one, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 2)
		if err != nil || len(one.Events) != 2 || !one.EventsMore {
			t.Fatalf("n=2: %+v %v", one.Events, err)
		}
		huge, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 500)
		if err != nil || len(huge.Events) != 4 {
			t.Fatalf("clamp: %+v %v", huge.Events, err)
		}
	})
	t.Run("get windows and bad params", func(t *testing.T) {
		e := newTestEnv()
		seedOpened(t, e)
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
		call := func(path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			e.h.Handle(w, req)
			return w
		}
		if w := call("/o/r/api/pulls/1?after_seq=0&n=10"); w.Code != 200 {
			t.Fatalf("window = %d (%s)", w.Code, w.Body.String())
		}
		if w := call("/o/r/api/pulls/1?after_seq=-1"); w.Code != 400 {
			t.Fatalf("bad seq = %d", w.Code)
		}
		if w := call("/o/r/api/pulls/1?after_seq=zz"); w.Code != 400 {
			t.Fatalf("bad seq2 = %d", w.Code)
		}
		if w := call("/o/r/api/pulls/1?n=0"); w.Code != 400 {
			t.Fatalf("bad n = %d", w.Code)
		}
		if w := call("/o/r/api/pulls/1?n=500"); w.Code != 200 {
			t.Fatalf("clamp = %d", w.Code)
		}
	})
	t.Run("loadPolicy outage", func(t *testing.T) {
		e := newTestEnv()
		e.failGet(PolicyKey("o", "r"), errors.New("down"))
		if _, err := e.svc.loadPolicy(ctx(), "o", "r"); err == nil {
			t.Fatal("policy err must surface")
		}
		e.clearFails()
	})
	t.Run("compute sidecar missing", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("sidecar: %v", err)
		}
	})
}
