package pulls

import (
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// seedMergeable primes a clean mergeable stamp for the merge task.
func seedMergeable(t *testing.T, e *testEnv, base, head string) {
	t.Helper()
	e.git.MergeBaseSHA = hexSHA(7)
	e.git.Behind = 0
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main":  base,
		"refs/heads/topic": head,
		"refs/pull/1/head": head,
	})
	e.refs.Refs["o/r"] = map[string]string{"refs/heads/main": base, "refs/heads/topic": head}
}

func maintainer() auth.Principal { return auth.Principal{Name: "merger@example.com"} }

func TestMergeStrategies(t *testing.T) {
	for _, strategy := range []string{StrategyMerge, StrategySquash, StrategyRebase} {
		t.Run(strategy, func(t *testing.T) {
			e := newTestEnv()
			e.roles.Roles["jane@example.com"] = "write"
			e.roles.Roles["merger@example.com"] = "maintain"
			openBasic(t, e, "o", "r")
			seedMergeable(t, e, hexSHA(1), hexSHA(2))
			e.closer.Closed = []int{3}
			rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: strategy, DeleteHead: true}, "corr-1")
			if err != nil {
				t.Fatalf("StartMerge: %v", err)
			}
			if rec.State != TaskRunning {
				t.Fatalf("must return running: %+v", rec)
			}
			done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
			if done == nil || done.State != TaskOK {
				t.Fatalf("task = %+v", done)
			}
			sha, _ := done.Result["sha"].(string)
			if len(sha) != 40 {
				t.Fatalf("sha = %q", sha)
			}
			// Never force: the update carried old = base sha at plan time.
			updates := e.refs.updatesFor("refs/heads/main")
			if len(updates) == 0 || updates[0].Old != hexSHA(1) || updates[0].New != sha {
				t.Fatalf("updates = %+v", updates)
			}
			if updates[0].Meta["agent"] != "pulls" || updates[0].Meta["principal"] != "merger@example.com" {
				t.Fatalf("meta = %+v", updates[0].Meta)
			}
			// pr.json merge outcome.
			pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
			if !pr.Merged || pr.MergeCommitSHA == nil || *pr.MergeCommitSHA != sha || *pr.MergeStrategy != strategy || pr.MergedBy == nil {
				t.Fatalf("pr = %+v", pr)
			}
			// Thread closed with a merged event.
			th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1)
			if th.State != StateClosed {
				t.Fatalf("thread = %+v", th)
			}
			// Closing seam called with PR body + commit messages.
			if e.closer.Calls != 1 || e.closer.PRNum != 1 || e.closer.SHA != sha || e.closer.Actor != "merger@example.com" {
				t.Fatalf("closer = %+v", e.closer)
			}
			found := false
			for _, tx := range e.closer.Texts {
				if strings.Contains(tx, "fixes #3") {
					found = true
				}
			}
			if !found {
				t.Fatalf("closer texts miss PR body: %q", e.closer.Texts)
			}
			// Shared index moved to closed.
			ix, _, _ := e.svc.loadIndex(ctx(), "o", "r")
			if len(ix.Open) != 0 || len(ix.ClosedRecent) != 1 {
				t.Fatalf("index = %+v", ix)
			}
			// Head deleted (same-repo, requested, policy allow-all).
			if _, ok := e.refs.Refs["o/r"]["refs/heads/topic"]; ok {
				t.Fatal("head branch should be deleted")
			}
			// Merged stream event.
			foundStream := false
			for _, s := range e.streams() {
				if s.Action == "merged" && s.HeadSHA == sha {
					foundStream = true
				}
			}
			if !foundStream {
				t.Fatalf("no merged stream: %+v", e.streams())
			}
		})
	}
}

func TestMergeGates(t *testing.T) {
	setup := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		return e
	}
	t.Run("write cannot merge", func(t *testing.T) {
		e := setup(t)
		_, err := e.svc.StartMerge(ctx(), "o", "r", 1, writer(), MergeInput{Strategy: StrategyMerge}, "")
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad strategy", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		_, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: "octopus"}, "")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown PR", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		_, err := e.svc.StartMerge(ctx(), "o", "r", 99, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dirty refused", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		e.git.TrialErr = errDirty
		e.git.TrialConflicts = []string{"x.go"}
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "conflict") {
			t.Fatalf("task = %+v", done)
		}
		_ = rec
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatal("dirty merge must not publish")
		}
	})
	t.Run("protect denies with rule name", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		putPolicy(t, e, `{"version":1,"rules":[{"name":"no-direct-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}]}`)
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "no-direct-main") {
			t.Fatalf("task = %+v", done)
		}
		_ = rec
	})
	t.Run("required-checks gate fails closed only when carried", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		// No gate carried ⇒ merges (protect rules evaluated, checks pending).
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategySquash}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("plain protect must not block: %+v", done)
		}
		_ = rec
	})
	t.Run("publish CAS conflict re-plans loudly", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		e.refs.UpdateConflictOnce = true
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
		// The retry path attempted exactly one conflicted publish + no force.
		for _, u := range e.refs.updatesFor("refs/heads/main") {
			if u.Old == "" {
				t.Fatalf("force publish: %+v", u)
			}
		}
		_ = rec
	})
	t.Run("second start joins", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		e.roles.Roles["admin2@example.com"] = "maintain"
		first, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		second, err := e.svc.StartMerge(ctx(), "o", "r", 1, auth.Principal{Name: "admin2@example.com"}, MergeInput{Strategy: StrategySquash}, "")
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if first.ID != second.ID {
			t.Fatalf("join must reuse id: %q vs %q", first.ID, second.ID)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("already merged", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		_ = rec
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done == nil || done.State != TaskOK {
			t.Fatalf("first task = %+v", done)
		}
		rec2, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("second start: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "already merged") {
			t.Fatalf("task = %+v rec2=%+v", done, rec2)
		}
	})
	t.Run("git unwired", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		e.svc.Git, e.svc.Dirs, e.svc.Refs = nil, nil, nil
		_, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUpdateBranch(t *testing.T) {
	setup := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(5), "refs/heads/topic": hexSHA(2)})
		e.refs.Refs["o/r"] = map[string]string{"refs/heads/topic": hexSHA(2)}
		return e
	}
	t.Run("success publishes head update", func(t *testing.T) {
		e := setup(t)
		rec, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), "")
		if err != nil {
			t.Fatalf("UpdateBranch: %v", err)
		}
		if rec.State != TaskRunning {
			t.Fatalf("rec = %+v", rec)
		}
		deadline := time.Now().Add(5 * time.Second)
		var done *TaskRecord
		for time.Now().Before(deadline) {
			if r := e.svc.tasks.get("o/r", TaskKindUpdateBranch); r != nil && r.State != TaskRunning {
				done = r
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		sha, _ := done.Result["sha"].(string)
		if e.refs.Refs["o/r"]["refs/heads/topic"] != sha {
			t.Fatalf("head not updated: %+v", e.refs.Refs["o/r"])
		}
	})
	t.Run("sha mismatch 409", func(t *testing.T) {
		e := setup(t)
		_, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), hexSHA(9))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dirty 409", func(t *testing.T) {
		e := setup(t)
		e.git.TrialErr = errDirty
		rec, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = rec
		deadline := time.Now().Add(5 * time.Second)
		var done *TaskRecord
		for time.Now().Before(deadline) {
			if r := e.svc.tasks.get("o/r", TaskKindUpdateBranch); r != nil && r.State != TaskRunning {
				done = r
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "conflict") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("read cannot update", func(t *testing.T) {
		e := setup(t)
		e.roles.Roles["ro@example.com"] = "read"
		_, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, auth.Principal{Name: "ro@example.com"}, "")
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDeleteHead(t *testing.T) {
	t.Run("unmerged refused", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("merged deletes", func(t *testing.T) {
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
			t.Fatalf("merge = %+v", done)
		}
		// Re-create the head (merge deleted it) to exercise the endpoint.
		e.refs.Refs["o/r"]["refs/heads/topic"] = hexSHA(2)
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); err != nil {
			t.Fatalf("DeleteHead: %v", err)
		}
		if _, ok := e.refs.Refs["o/r"]["refs/heads/topic"]; ok {
			t.Fatal("head should be gone")
		}
	})
	t.Run("fork head never deleted by base", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		pr.Merged = true
		_, prVer, _ := e.svc.loadPR(ctx(), "o", "r", pr.Num)
		_ = e.svc.savePR(ctx(), "o", "r", pr, prVer)
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("write cannot delete", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFork(t *testing.T) {
	t.Run("success records provenance + index", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		rec, child, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{})
		if err != nil {
			t.Fatalf("StartFork: %v", err)
		}
		if child != "o/r-fork" {
			t.Fatalf("child = %q", child)
		}
		if rec.State != TaskRunning {
			t.Fatalf("rec = %+v", rec)
		}
		deadline := time.Now().Add(5 * time.Second)
		var done *TaskRecord
		for time.Now().Before(deadline) {
			if r := e.svc.tasks.get(child, TaskKindFork); r != nil && r.State != TaskRunning {
				done = r
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		raw, _, _ := e.svc.getJSON(ctx(), ForkKey("o", "r-fork"))
		if raw == nil || !strings.Contains(string(raw), `"parent":"o/r"`) {
			t.Fatalf("fork.json = %s", raw)
		}
		fraw, _, _ := e.svc.getJSON(ctx(), ForksKey("o", "r"))
		if fraw == nil || !strings.Contains(string(fraw), "o/r-fork") {
			t.Fatalf("forks.json = %s", fraw)
		}
	})
	t.Run("name taken 409", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		rec, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "r-fork"})
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		_ = rec
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if r := e.svc.tasks.get("o/r-fork", TaskKindFork); r != nil && r.State != TaskRunning {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		rec2, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "r-fork"})
		if err != nil {
			t.Fatalf("second start: %v", err)
		}
		_ = rec2
		deadline = time.Now().Add(5 * time.Second)
		var done *TaskRecord
		for time.Now().Before(deadline) {
			if r := e.svc.tasks.get("o/r-fork", TaskKindFork); r != nil && r.State != TaskRunning {
				done = r
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "already exists") {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("bad names + gates", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{TargetOwner: ".."}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("owner err = %v", err)
		}
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", auth.Anonymous(), ForkInput{}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("anon err = %v", err)
		}
		e.roles.Roles["ro@example.com"] = "read"
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", auth.Principal{Name: "ro@example.com"}, ForkInput{}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("read err = %v", err)
		}
	})
}

func TestSink(t *testing.T) {
	t.Run("base push recomputes dirty PRs", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(8), "refs/heads/topic": hexSHA(2)})
		e.git.MergeBaseSHA = hexSHA(7)
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/main", Old: hexSHA(1), New: hexSHA(8)})
		deadline := time.Now().Add(5 * time.Second)
		var m *MergeableDoc
		for time.Now().Before(deadline) {
			if c, _, _ := e.svc.loadMergeable(ctx(), "o", "r", 1); c != nil && c.BaseSHA == hexSHA(8) {
				m = c
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if m == nil {
			t.Fatal("sink never recomputed the stamp")
		}
	})
	t.Run("head push refreshes sha", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/topic", Old: hexSHA(2), New: hexSHA(3)})
		deadline := time.Now().Add(5 * time.Second)
		var sha string
		for time.Now().Before(deadline) {
			if pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1); pr != nil && pr.Head.SHA == hexSHA(3) {
				sha = pr.Head.SHA
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if sha == "" {
			t.Fatal("head sha never refreshed")
		}
	})
	t.Run("unrelated ref ignored", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/other", Old: hexSHA(1), New: hexSHA(2)})
		time.Sleep(50 * time.Millisecond)
		if c, _, _ := e.svc.loadMergeable(ctx(), "o", "r", 1); c != nil {
			t.Fatalf("no recompute expected: %+v", c)
		}
	})
	t.Run("force-push records evidence", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{Repo: "o/r", RefName: "refs/heads/topic", Old: hexSHA(2), New: hexSHA(4)})
		deadline := time.Now().Add(5 * time.Second)
		var stamped bool
		for time.Now().Before(deadline) {
			if pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1); pr != nil && pr.HeadForcePushedAt != nil {
				stamped = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !stamped {
			t.Fatal("force-push evidence missing")
		}
	})
}
