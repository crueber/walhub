package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeChecksGate scripts pulls' ChecksGate seam (the 05 merge-time half;
// satisfied in production by *checks.Service — this package never imports
// it, keeping the seam one-directional).
type fakeChecksGate struct {
	err        error
	calls      int
	lastHead   string
	lastBase   string
	lastMerger string
}

func (f *fakeChecksGate) CheckRequiredChecks(_ context.Context, _, _, headSHA, baseRef, merger string) error {
	f.calls++
	f.lastHead = headSHA
	f.lastBase = baseRef
	f.lastMerger = merger
	return f.err
}

// TestMergeConsultsChecksGate proves the 05 coordination: the merge task
// consults the checks-provided gate with the LIVE head sha after the
// protected-ref check; a failed gate narrates the shortfall verbatim and
// publishes nothing (the merge logic is NOT forked — one call site).
func TestMergeConsultsChecksGate(t *testing.T) {
	t.Run("deny blocks the publish with the verbatim message", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		gate := &fakeChecksGate{err: errors.New("merge refused: required checks not green for " + hexSHA(2) + ": ci/build (failure), lint (missing)")}
		e.svc.Checks = gate
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-c1")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		if rec.State != TaskRunning {
			t.Fatalf("must return running: %+v", rec)
		}
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
		if !strings.Contains(done.Error, "merge refused: required checks not green for "+hexSHA(2)) {
			t.Fatalf("narration misses the verbatim gate message: %q", done.Error)
		}
		if gate.calls != 1 {
			t.Fatalf("gate calls = %d", gate.calls)
		}
		if gate.lastHead != hexSHA(2) {
			t.Fatalf("gate must see the live head %s, got %s", hexSHA(2), gate.lastHead)
		}
		if gate.lastBase != "refs/heads/main" || gate.lastMerger != "merger@example.com" {
			t.Fatalf("gate args: base=%q merger=%q", gate.lastBase, gate.lastMerger)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatalf("base published despite the gate: %+v", e.refs.updatesFor("refs/heads/main"))
		}
		pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		if pr.Merged {
			t.Fatalf("pr marked merged despite the gate")
		}
	})
	t.Run("allow lands the merge", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		gate := &fakeChecksGate{}
		e.svc.Checks = gate
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-c2")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		if gate.calls != 1 {
			t.Fatalf("gate calls = %d", gate.calls)
		}
	})
	t.Run("carried gate with no backend fails closed", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		putPolicy(t, e, `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","delete","force-push"],"require_checks":["ci/build"]}}}]}`)
		// No backend wired (nil Checks): the merge refuses rather than
		// silently allowing.
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-c3")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
		if !strings.Contains(done.Error, "main-needs-ci") || !strings.Contains(done.Error, "no checks backend") {
			t.Fatalf("narration must name the rule and the missing backend: %q", done.Error)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatalf("base published despite the gate: %+v", e.refs.updatesFor("refs/heads/main"))
		}
	})
}
