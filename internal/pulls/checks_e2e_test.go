package pulls

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/checks"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// fakeCommits resolves any well-formed sha (the e2e pins the head sha
// the merge task actually resolves, so echoing is exact).
type fakeCommits struct{}

func (fakeCommits) ResolveCommit(_ context.Context, _, sha string) (string, error) {
	return sha, nil
}

// TestMergeChecksGateEndToEnd proves the 05 acceptance flow against the
// REAL checks service (test-only import — production code stays
// seam-separated): post failure → merge blocked with the verbatim
// message → post success → merge lands. The merge logic is NOT forked:
// the only coordination is the ChecksGate call at the step-4 site.
func TestMergeChecksGateEndToEnd(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	putPolicy(t, e, `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","delete","force-push"],"require_checks":["ci/build"]}}}]}`)

	checksSvc := checks.New(e.store, e.roles)
	checksSvc.Commits = fakeCommits{}
	e.svc.Checks = checksSvc

	reporter := auth.Principal{Name: "jane@example.com", Write: true}
	head := hexSHA(2)

	// Post failure → merge blocked with the verbatim refusal.
	if _, err := checksSvc.ReportStatus(ctx(), "o", "r", head, reporter, "", checks.ReportInput{Context: "ci/build", State: "failure"}); err != nil {
		t.Fatalf("report failure: %v", err)
	}
	rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-e2e-1")
	if err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	_ = rec
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskError {
		t.Fatalf("blocked task = %+v", done)
	}
	want := "merge refused: required checks not green for " + head + ": ci/build (failure)"
	if !strings.Contains(done.Error, want) {
		t.Fatalf("refusal %q misses verbatim %q", done.Error, want)
	}
	if len(e.refs.updatesFor("refs/heads/main")) != 0 {
		t.Fatalf("base published despite red checks")
	}

	// Post success → merge lands.
	if _, err := checksSvc.ReportStatus(ctx(), "o", "r", head, reporter, "", checks.ReportInput{Context: "ci/build", State: "success"}); err != nil {
		t.Fatalf("report success: %v", err)
	}
	rec, err = e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "corr-e2e-2")
	if err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	_ = rec
	done = waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskOK {
		t.Fatalf("green task = %+v", done)
	}
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	if pr == nil || !pr.Merged {
		t.Fatal("pr not marked merged")
	}
}
