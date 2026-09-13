package pulls

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Unit tests for the §7 fork→base object bridge (issue #456) with scripted
// git: cross-fork heads missing from the base serving copy open fork-local
// (no 503), diff from the bridged base copy, compute mergeability, and fail
// loud when the bridge itself is down. End-to-end proof with the real git
// binary lives in bridge_e2e_test.go.

// seedForkPR wires a base repo (main=hexSHA(1)) and a fork repo
// (feat=hexSHA(9)) through the fake git dirs.
func seedForkPR(e *testEnv) {
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
}

func openForkPR(t *testing.T, e *testEnv) *PRDoc {
	t.Helper()
	_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return pr
}

func fetchCalls(g *FakeGit) []string {
	var out []string
	for _, c := range g.CallLog() {
		if strings.HasPrefix(c, "fetch:") {
			out = append(out, c)
		}
	}
	return out
}

func TestBridgeOpenForkUnique(t *testing.T) {
	t.Run("missing head bridges and opens fork-local", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		// Fork-unique: the head is NOT in the base object set.
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		pr := openForkPR(t, e)
		if pr.Fork == nil || pr.Fork.Repo != "f/r" {
			t.Fatalf("fork provenance: %+v", pr.Fork)
		}
		if pr.HeadPublished {
			t.Fatal("bridged-only head must stay fork-local (no base-side pull-head ref)")
		}
		if got := fetchCalls(e.git); len(got) != 1 {
			t.Fatalf("bridge fetch calls = %v", got)
		}
		for _, c := range e.refs.Calls {
			if c.Ref == PullHeadRef(1) {
				t.Fatalf("no pull-head publish for bridged-only head: %+v", c)
			}
		}
	})
	t.Run("base-contained head publishes pull-head", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		// Head already in the base object set: no fetch, base-side publish.
		e.git.ReachableMap = map[string]bool{hexSHA(9): true}
		pr := openForkPR(t, e)
		if !pr.HeadPublished {
			t.Fatal("pre-reachable head must publish refs/pull/<num>/head")
		}
		if got := fetchCalls(e.git); len(got) != 0 {
			t.Fatalf("no fetch when already reachable: %v", got)
		}
	})
	t.Run("bridge outage is 503", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		e.git.FetchErr = errors.New("bridge down")
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("same-repo unreachable is 422 with no fetch", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/feat": hexSHA(2)})
		e.git.ReachableMap = map[string]bool{hexSHA(2): false}
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat"}, "")
		if !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("err = %v", err)
		}
		if got := fetchCalls(e.git); len(got) != 0 {
			t.Fatalf("same-repo must never bridge: %v", got)
		}
	})
}

func TestBridgeDiffDir(t *testing.T) {
	t.Run("bridged head diffs from the base dir", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		pr := openForkPR(t, e)
		dir, err := e.svc.diffDir(ctx(), pr)
		if err != nil {
			t.Fatalf("diffDir: %v", err)
		}
		if dir != "dir:o/r" {
			t.Fatalf("dir = %q (want base after bridge)", dir)
		}
		if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); err != nil {
			t.Fatalf("diff: %v", err)
		}
	})
	t.Run("unbridged head falls back to the fork dir", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		e.git.FetchNoOp = true // fetch changes nothing: still missing
		pr := openForkPR(t, e)
		dir, err := e.svc.diffDir(ctx(), pr)
		if err != nil {
			t.Fatalf("diffDir: %v", err)
		}
		if dir != "dir:f/r" {
			t.Fatalf("dir = %q (want fork fallback)", dir)
		}
	})
	t.Run("bridge failure is 503", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		// Open while the head is base-contained, then break the bridge
		// for the diff: the head leaves the base object set AND the
		// fetch is down — fail closed, never a guessed fork-local diff.
		e.git.ReachableMap = map[string]bool{hexSHA(9): true}
		openForkPR(t, e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		e.git.FetchErr = errors.New("bridge down")
		if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestBridgeMergeable(t *testing.T) {
	t.Run("computes clean after bridging", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		pr := openForkPR(t, e)
		e.git.TrialTree = hexSHA(7)
		m, err := e.svc.ComputeMergeable(ctx(), "o", "r", pr.Num)
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if m.State != MergeableClean {
			t.Fatalf("state = %q", m.State)
		}
		if got := fetchCalls(e.git); len(got) == 0 {
			t.Fatal("compute must bridge the fork head")
		}
	})
	t.Run("bridge outage fails loud", func(t *testing.T) {
		e := newTestEnv()
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): true}
		pr := openForkPR(t, e)
		e.git.FetchErr = errors.New("bridge down")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", pr.Num); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestBridgeMergeTask(t *testing.T) {
	t.Run("merge bridges then publishes with pack", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): false}
		pr := openForkPR(t, e)
		// Base main ref for the merge CAS.
		if e.refs.Refs["o/r"] == nil {
			e.refs.Refs["o/r"] = map[string]string{}
		}
		e.refs.Refs["o/r"]["refs/heads/main"] = hexSHA(1)
		e.git.TrialTree = hexSHA(7)
		rec, err := e.svc.StartMerge(ctx(), "o", "r", pr.Num, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		_ = rec
		if done == nil || done.State != TaskOK {
			t.Fatalf("merge = %+v", done)
		}
		if got := fetchCalls(e.git); len(got) == 0 {
			t.Fatal("merge must bridge the fork head into the base copy")
		}
		foundPack := false
		for _, c := range e.refs.Calls {
			if c.Op == "update-pack" && c.Ref == "refs/heads/main" && c.Pack != "" {
				foundPack = true
			}
		}
		if !foundPack {
			t.Fatalf("merge must publish base move WITH pack: %+v", e.refs.Calls)
		}
	})
	t.Run("merge fails loud on bridge outage", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		seedForkPR(e)
		e.git.ReachableMap = map[string]bool{hexSHA(9): true}
		pr := openForkPR(t, e)
		if e.refs.Refs["o/r"] == nil {
			e.refs.Refs["o/r"] = map[string]string{}
		}
		e.refs.Refs["o/r"]["refs/heads/main"] = hexSHA(1)
		e.git.FetchErr = errors.New("bridge down")
		if _, err := e.svc.StartMerge(ctx(), "o", "r", pr.Num, maintainer(), MergeInput{Strategy: StrategyMerge}, ""); err != nil {
			t.Fatalf("start: %v", err)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError {
			t.Fatalf("merge = %+v", done)
		}
		if !strings.Contains(done.Error, "bridge") {
			t.Fatalf("error must name the bridge: %q", done.Error)
		}
	})
}
