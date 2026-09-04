package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- OpenPR leftovers ----------------------------------------------------------

func TestCoverOpenConflicts(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	// Pre-seed the thread key: the header Create 412s ⇒ 409 already exists.
	_, _ = store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 1), encodeThread(&Thread{Num: 1, Kind: "pr"}),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("thread taken: %v", err)
	}
	// Pre-seed the event instead: event Create fails ⇒ error (not silent).
	e2 := newTestEnv()
	e2.roles.Roles["jane@example.com"] = "write"
	e2.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	_, _ = store.PutBytes(ctx(), e2.store, EventKey("o", "r", 1, 0), encodeEvent(&Event{Seq: 0}),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if _, _, err := e2.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); err == nil {
		t.Fatal("event taken must error")
	}
	// Reachability backend error.
	e3 := newTestEnv()
	e3.roles.Roles["jane@example.com"] = "write"
	e3.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	e3.git.ReachableErr = errors.New("git down")
	if _, _, err := e3.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reachable err: %v", err)
	}
	// Dirs failure on head repo (cross-fork).
	e4 := newTestEnv()
	e4.roles.Roles["jane@example.com"] = "write"
	e4.svc.Dirs = &FakeDirs{UnknownErr: errors.New("no repo")}
	if _, _, err := e4.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dirs err: %v", err)
	}
	// findOpenPair index error surfaces.
	e5 := newTestEnv()
	e5.roles.Roles["jane@example.com"] = "write"
	e5.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	e5.failGet(IndexKey("o", "r"), errors.New("down"))
	if _, _, err := e5.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); err == nil {
		t.Fatal("index err must surface")
	}
}

// --- GetPR leftovers -------------------------------------------------------------

func TestCoverGetBranches(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Git unwired ⇒ unknown mergeable, no live shas.
	git := e.svc.Git
	e.svc.Git = nil
	view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil || view.Mergeable.State != MergeableUnknown {
		t.Fatalf("unwired: %+v %v", view, err)
	}
	e.svc.Git = git
	// Dirs failure ⇒ unknown.
	e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
	view, err = e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil || view.Mergeable.State != MergeableUnknown {
		t.Fatalf("dirs: %+v %v", view, err)
	}
	e.svc.Dirs = &FakeDirs{}
	// Deleted base branch ⇒ unknown with snapshot shas (the ref is really
	// gone: resolve fails and the §3 snapshot rule serves pr.Base.SHA).
	e.delGitRef("o/r", "refs/heads/main")
	view, err = e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil || view.Mergeable.State != MergeableUnknown || view.BaseLive != hexSHA(1) {
		t.Fatalf("deleted base: %+v %v", view, err)
	}
	if view.HeadRefOk {
		t.Fatal("pull-head drifted: HeadRefOk must be false")
	}
	// Deleted head branch ⇒ unknown with snapshot head sha.
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e.delGitRef("o/r", "refs/heads/topic")
	view, err = e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil || view.HeadLive != hexSHA(2) {
		t.Fatalf("deleted head: %+v %v", view, err)
	}
	// Missing sidecar ⇒ 404.
	_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
	if _, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sidecar: %v", err)
	}
	// headRefMatches with no publisher ⇒ false.
	if e.svc.headRefMatches(ctx(), &PRDoc{Num: 1}, hexSHA(2)) {
		t.Fatal("nil publisher must not match")
	}
	e.svc.Refs = nil
	if e.svc.headRefMatches(ctx(), &PRDoc{Num: 1}, hexSHA(2)) {
		t.Fatal("nil refs must not match")
	}
}

// --- Diff/Commits leftovers -------------------------------------------------------

func TestCoverDiffCommits(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	// Git unwired.
	git := e.svc.Git
	e.svc.Git = nil
	if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("diff unwired: %v", err)
	}
	if _, _, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), 0, 10); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("commits unwired: %v", err)
	}
	e.svc.Git = git
	// Unknown PR (no thread, no sidecar).
	if _, err := e.svc.Diff(ctx(), "o", "r", 77, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("diff unknown: %v", err)
	}
	if _, _, err := e.svc.Commits(ctx(), "o", "r", 77, writer(), 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commits unknown: %v", err)
	}
	// Git failures.
	e.git.DiffErr = errors.New("diff down")
	if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); e.git.DiffErr != nil && err == nil {
		t.Fatal("diff err must surface")
	}
	e.git.DiffErr = nil
	e.git.LogErr = errors.New("log down")
	if _, _, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), 0, 10); err == nil {
		t.Fatal("log err must surface")
	}
	e.git.LogErr = nil
	// skip<0 clamps; n>200 clamps; more flag.
	e.git.LogRows = []CommitEntry{{SHA: hexSHA(2), Subject: "s"}}
	rows, more, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), -5, 500)
	if err != nil || len(rows) != 1 || more {
		t.Fatalf("clamp: %+v %v %v", rows, more, err)
	}
	// Cross-fork reachable-in-base ⇒ base dir (no fork dir probe failure).
	e2 := newTestEnv()
	e2.roles.Roles["jane@example.com"] = "write"
	e2.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e2.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
	_, pr, err := e2.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
	if err != nil {
		t.Fatalf("fork open: %v", err)
	}
	_ = pr
	if _, err := e2.svc.Diff(ctx(), "o", "r", 1, writer()); err != nil {
		t.Fatalf("fork diff: %v", err)
	}
	// Cross-fork unreachable + fork dir down ⇒ unavailable.
	e2.git.ReachableMap = map[string]bool{hexSHA(9): false}
	e2.svc.Dirs = &failForkDirs{ok: map[string]bool{"o/r": true}}
	if _, err := e2.svc.Diff(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fork dirs: %v", err)
	}
	// Reachability error.
	e2.svc.Dirs = &FakeDirs{}
	e2.git.ReachableErr = errors.New("down")
	if _, err := e2.svc.Diff(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reachable: %v", err)
	}
	e2.git.ReachableErr = nil
}

type failForkDirs struct {
	ok map[string]bool
}

func (f *failForkDirs) Dir(_ context.Context, repo string) (string, error) {
	if f.ok[repo] {
		return "dir:" + repo, nil
	}
	return "", errors.New("no repo " + repo)
}

// --- UpdatePR leftovers --------------------------------------------------------------

func TestCoverUpdateNoop(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// All-nil patch: no events, index refreshed, same thread.
	th, pr, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{})
	if err != nil || th == nil || pr == nil {
		t.Fatalf("noop: %+v %+v %v", th, pr, err)
	}
	// Same-value title/state: no events.
	th2, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: &th.Title, State: &th.State})
	if err != nil || th2.Title != th.Title {
		t.Fatalf("same: %+v %v", th2, err)
	}
	// Merged PR cannot be closed.
	pr.Merged = true
	_, prVer, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	_ = e.svc.savePR(ctx(), "o", "r", pr, prVer)
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{State: strPtr("closed")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("merged close: %v", err)
	}
	// Missing sidecar.
	_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sidecar: %v", err)
	}
}

// --- runMerge leftovers ------------------------------------------------------------------

func TestCoverMergeErrors(t *testing.T) {
	setup := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		return e
	}
	run := func(e *testEnv, in MergeInput) *TaskRecord {
		t.Helper()
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), in, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		return waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	}
	t.Run("commit-tree error", func(t *testing.T) {
		e := setup(t)
		e.git.CommitErr = errors.New("no tree")
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("replay error", func(t *testing.T) {
		e := setup(t)
		e.git.ReplayErr = errors.New("no replay")
		if done := run(e, MergeInput{Strategy: StrategyRebase}); done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("merge-base error on rebase", func(t *testing.T) {
		e := setup(t)
		e.git.MergeBaseErr = errors.New("no base")
		if done := run(e, MergeInput{Strategy: StrategyRebase}); done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("closed not merged", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["tri@example.com"] = "triage"
		_, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "tri@example.com"}, PRPatch{State: strPtr("closed")})
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskError || !strings.Contains(done.Error, "closed") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("base unresolvable", func(t *testing.T) {
		e := setup(t)
		e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(2), "refs/pull/1/head": hexSHA(2)})
		e.delGitRef("o/r", "refs/heads/main")
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("head unresolvable", func(t *testing.T) {
		e := setup(t)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/pull/1/head": hexSHA(2)})
		e.delGitRef("o/r", "refs/heads/topic")
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("generic publish error", func(t *testing.T) {
		e := setup(t)
		e.refs.Refs["o/r"] = map[string]string{}
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("delete-head policy deny narrated", func(t *testing.T) {
		e := setup(t)
		putPolicy(t, e, `{"version":1,"rules":[{"name":"no-del","match":{"refs":["refs/heads/topic"]},"effect":{"protect":{"restricts":["delete"]}}}]}`)
		if done := run(e, MergeInput{Strategy: StrategySquash, DeleteHead: true}); done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		if _, ok := e.refs.Refs["o/r"]["refs/heads/topic"]; !ok {
			t.Fatal("denied delete must keep the head")
		}
	})
	t.Run("delete-head backend error narrated", func(t *testing.T) {
		e := setup(t)
		e.refs.DeleteErr = errors.New("wal down")
		if done := run(e, MergeInput{Strategy: StrategySquash, DeleteHead: true}); done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		e.refs.DeleteErr = nil
	})
	t.Run("up_to_date refused", func(t *testing.T) {
		e := setup(t)
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(1)] = true
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskError || !strings.Contains(done.Error, "up to date") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("closer nil is fine", func(t *testing.T) {
		e := setup(t)
		e.svc.Closer = nil
		if done := run(e, MergeInput{Strategy: StrategyMerge}); done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("missing thread fails before publish", func(t *testing.T) {
		e := setup(t)
		// Break the thread so the open-time read fails fast (404): the
		// merge never starts and nothing publishes.
		_ = e.store.Delete(ctx(), ThreadKey("o", "r", 1), "")
		_, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatal("missing thread must fail before publish")
		}
	})
}

// --- runUpdateBranch / DeleteHead / Fork leftovers --------------------------------------------

func TestCoverBranchHeadFork(t *testing.T) {
	t.Run("update-branch already current is a no-op success", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		// base ancestor of head ⇒ contained ⇒ return head sha.
		e.git.Ancestors[hexSHA(1)+"\x00"+hexSHA(2)] = true
		sha, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{Progress: []string{}})
		if err != nil || sha != hexSHA(2) {
			t.Fatalf("noop: %q %v", sha, err)
		}
	})
	t.Run("update-branch resolve failures", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); err == nil {
			t.Fatal("no seeds must fail")
		}
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.delGitRef("o/r", "refs/heads/topic")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("head: %v", err)
		}
	})
	t.Run("update-branch commit/publish failures", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(5), "refs/heads/topic": hexSHA(2)})
		e.refs.Refs["o/r"] = map[string]string{"refs/heads/topic": hexSHA(2)}
		e.git.CommitErr = errors.New("no commit")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); err == nil {
			t.Fatal("commit err must fail")
		}
		e.git.CommitErr = nil
		e.git.TrialErr = errors.New("git down")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); err == nil {
			t.Fatal("trial err must fail")
		}
		e.git.TrialErr = nil
		e.refs.Refs["o/r"] = map[string]string{}
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("publish: %v", err)
		}
	})
	t.Run("delete-head backend down", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done == nil || done.State != TaskOK {
			t.Fatalf("merge: %+v", done)
		}
		e.refs.Refs["o/r"]["refs/heads/topic"] = hexSHA(2)
		e.refs.DeleteErr = errors.New("wal down")
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); err == nil {
			t.Fatal("delete err must surface")
		}
		e.refs.DeleteErr = nil
		e.svc.Refs = nil
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unwired: %v", err)
		}
	})
	t.Run("delete-head unknown + head-policy deny", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["merger@example.com"] = "maintain"
		if err := e.svc.DeleteHead(ctx(), "o", "r", 9, maintainer()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown: %v", err)
		}
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		pr, prVer, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		pr.Merged = true
		_ = e.svc.savePR(ctx(), "o", "r", pr, prVer)
		putPolicy(t, e, `{"version":1,"rules":[{"name":"no-del","match":{"refs":["refs/heads/topic"]},"effect":{"protect":{"restricts":["delete"]}}}]}`)
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); !errors.Is(err, ErrConflict) {
			t.Fatalf("deny: %v", err)
		}
	})
	t.Run("fork join reuses id", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		// Occupy the task slot manually so the second StartFork joins.
		entry, joined := e.svc.tasks.begin("o/r-fork", TaskKindFork)
		if joined {
			t.Fatal("first begin must lead")
		}
		rec, child, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "r-fork"})
		if err != nil || child != "o/r-fork" {
			t.Fatalf("join: %v %q", err, child)
		}
		if rec.ID != entry.rec.snapshot().ID {
			t.Fatalf("join must reuse id: %+v", rec)
		}
		e.svc.tasks.end("o/r-fork", TaskKindFork)
	})
	t.Run("update-branch join reuses id", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		entry, joined := e.svc.tasks.begin("o/r", TaskKindUpdateBranch)
		if joined {
			t.Fatal("first begin must lead")
		}
		rec, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), "")
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if rec.ID != entry.rec.snapshot().ID {
			t.Fatalf("join must reuse id")
		}
		e.svc.tasks.end("o/r", TaskKindUpdateBranch)
	})
}

// --- sink leftovers ------------------------------------------------------------------

func TestCoverSinkErrors(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Index read failure ⇒ silent return (sink never fails the push path).
	e.failGet(IndexKey("o", "r"), errors.New("down"))
	e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/main"})
	e.clearFails()
	// Corrupt sidecar for the open card ⇒ skipped.
	_, _ = store.PutBytes(ctx(), e.store, PRKey("o", "r", 1), []byte("{oops"), store.PutOptions{Mode: store.PutUpdate, IfVersion: mustVersion(t, e, PRKey("o", "r", 1)), ContentType: "application/json"})
	e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/main"})
}

func mustVersion(t *testing.T, e *testEnv, key string) store.Version {
	t.Helper()
	_, ver, err := e.svc.getJSON(ctx(), key)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	return ver
}

// --- taskTable direct ---------------------------------------------------------------------

func TestCoverTaskTable(t *testing.T) {
	tbl := newTaskTable()
	if got := tbl.get("r", "k"); got != nil {
		t.Fatalf("empty get: %+v", got)
	}
	if got := (*TaskRecord)(nil).snapshot(); got != nil {
		t.Fatal("nil snapshot must be nil")
	}
	entry, joined := tbl.begin("r", "k")
	if joined {
		t.Fatal("first must lead")
	}
	if got := tbl.get("r", "k"); got == nil || got.ID != entry.rec.snapshot().ID {
		t.Fatal("running get")
	}
	tbl.end("r", "k")
	if got := tbl.get("r", "k"); got == nil || got.Finished == "" {
		t.Fatalf("recent get: %+v", got)
	}
	tbl.end("r", "k") // idempotent: no running entry
	// Eviction past the cap keeps the table bounded.
	for i := 0; i < 140; i++ {
		k := "r" + itoa(i)
		en, _ := tbl.begin(k, "k")
		en.rec.setState(TaskOK, "done", "", nil)
		tbl.end(k, "k")
	}
	tbl.mu.Lock()
	n := len(tbl.recent)
	tbl.mu.Unlock()
	if n > 128 {
		t.Fatalf("recent unbounded: %d", n)
	}
}
