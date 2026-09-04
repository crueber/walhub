package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeReviewGate scripts pulls' ReviewGate seam (the 04 merge-time half;
// satisfied in production by *review.Service — this package never imports
// it, keeping the seam one-directional).
type fakeReviewGate struct {
	err        error
	calls      int
	lastHead   string
	lastBase   string
	lastMerger string
	lastNum    int
}

func (f *fakeReviewGate) CheckRequiredReviews(_ context.Context, _, _ string, num int, headSHA, baseRef, merger string) error {
	f.calls++
	f.lastNum = num
	f.lastHead = headSHA
	f.lastBase = baseRef
	f.lastMerger = merger
	return f.err
}

// TestMergeConsultsReviewGate proves the 04 coordination: the merge task
// consults the review-provided gate with the LIVE head sha after the
// protected-ref check; a failed gate narrates the shortfall and publishes
// nothing (the merge logic is NOT forked — one call site).
func TestMergeConsultsReviewGate(t *testing.T) {
	t.Run("deny blocks the publish", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		gate := &fakeReviewGate{err: errors.New("conflict: required-reviews: need 2 approvals, have 1")}
		e.svc.Reviews = gate
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-g1")
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
		if !strings.Contains(done.Error, "required-reviews") {
			t.Fatalf("narration misses the gate: %q", done.Error)
		}
		if gate.calls != 1 {
			t.Fatalf("gate calls = %d", gate.calls)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatalf("base published despite the gate: %+v", e.refs.updatesFor("refs/heads/main"))
		}
		pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		if pr.Merged {
			t.Fatalf("pr marked merged despite the gate")
		}
	})
	t.Run("allow passes head/base/merger through", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		gate := &fakeReviewGate{}
		e.svc.Reviews = gate
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-g2")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		if gate.calls != 1 || gate.lastNum != 1 || gate.lastBase != "refs/heads/main" ||
			gate.lastMerger != "merger@example.com" || gate.lastHead != hexSHA(2) {
			t.Fatalf("gate inputs wrong: %+v", gate)
		}
	})
	t.Run("required-reviews rule does not deny the protect check", func(t *testing.T) {
		// Regression pin for the two-halves split: a required-reviews
		// rule matching the base must NOT fail checkProtectedRef (the
		// merge publish is server-side, not a receive-pack push) — its
		// verdict comes only from the gate above. Before EvaluateProtect,
		// this merge died with "rejected by rule 'pr-gate'".
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		putPolicy(t, e, `{"version":1,"rules":[{"name":"pr-gate","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2}}}]}`)
		e.svc.Reviews = &fakeReviewGate{}
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-g3")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
	})
}
