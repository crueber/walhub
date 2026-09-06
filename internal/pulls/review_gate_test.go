package pulls

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	// entered, when non-nil, is closed on the first gate call so the test
	// can prove the worker reached the gate; release, when non-nil,
	// blocks the gate until the test closes it. Wiring both makes the
	// StartMerge → running snapshot deterministic: the worker cannot run
	// to terminal before the snapshot (issue #180).
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func (f *fakeReviewGate) CheckRequiredReviews(ctx context.Context, _, _ string, num int, headSHA, baseRef, merger string) error {
	f.calls++
	f.lastNum = num
	f.lastHead = headSHA
	f.lastBase = baseRef
	f.lastMerger = merger
	if f.entered != nil {
		f.enterOnce.Do(func() { close(f.entered) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
		}
	}
	return f.err
}

// awaitGate waits for the worker to reach the gate (issue #180: without
// this, releasing before the worker blocks races the terminal wait).
func awaitGate(t *testing.T, entered chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the gate")
	}
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
		entered := make(chan struct{})
		release := make(chan struct{})
		gate := &fakeReviewGate{err: errors.New("conflict: required-reviews: need 2 approvals, have 1"), entered: entered, release: release}
		e.svc.Reviews = gate
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-g1")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		if rec.State != TaskRunning {
			t.Fatalf("must return running: %+v", rec)
		}
		// The worker blocks in the gate: the running snapshot above is
		// deterministic, not a scheduling win (issue #180).
		awaitGate(t, entered)
		close(release)
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
