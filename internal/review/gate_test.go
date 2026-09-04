package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func seedPolicy(t *testing.T, svc *Service, doc string) {
	t.Helper()
	put(t, svc, "repos/"+testOwner+"/"+testRepo+"/policy.json", []byte(doc), store.PutCreate, "")
}

const gatePolicy = `{"version":1,"rules":[
	{"name":"pr-gate","match":{"refs":["refs/heads/main"]},
	 "effect":{"required-reviews":{"min_approvals":2,"dismiss_stale":true,"bypass":["svc:merge-queue"]}}}]}
`

func TestGate(t *testing.T) {
	ctx := context.Background()
	approve := func(t *testing.T, svc *Service, who, sha string) {
		t.Helper()
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal(who),
			SubmitInput{State: StateApproved, CommitSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("no policy passes", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no matching rule passes", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, `{"version":1,"rules":[
			{"name":"pr-gate","match":{"refs":["refs/heads/other"]},
			 "effect":{"required-reviews":{"min_approvals":2}}}]}
		`)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("shortfall narrates need/have", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, gatePolicy)
		approve(t, svc, "bob", testHead)
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob")
		if err == nil || statusFor(err) != 409 {
			t.Fatalf("err=%v", err)
		}
		if got := err.Error(); got != "conflict: required-reviews: need 2 approvals, have 1" && !contains(got, "need 2 approvals, have 1") {
			t.Fatalf("err=%q", got)
		}
	})
	t.Run("two fresh approvals pass; stale fails with dismiss_stale", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, gatePolicy)
		approve(t, svc, "bob", testHead)
		approve(t, svc, "carol", testHead)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "dave"); err != nil {
			t.Fatalf("two fresh: %v", err)
		}
		// The head moves: carol's approval goes stale (commit_sha pin).
		moveHead(t, svc, testHead2)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead2, "refs/heads/main", "dave"); err == nil {
			t.Fatalf("stale approval counted")
		}
	})
	t.Run("changes requested blocks even when approvals suffice", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, gatePolicy)
		approve(t, svc, "bob", testHead)
		approve(t, svc, "dave", testHead)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("carol"),
			SubmitInput{State: StateChangesRequested, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "dave")
		if err == nil || !contains(err.Error(), "changes requested by carol") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("bypass skips the gate; non-bypass does not", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, gatePolicy)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "svc:merge-queue"); err != nil {
			t.Fatalf("bypass: %v", err)
		}
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); err == nil {
			t.Fatalf("non-bypass passed with zero approvals")
		}
	})
	t.Run("dismissed approval does not count", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, `{"version":1,"rules":[
			{"name":"pr-gate","match":{"refs":["refs/heads/main"]},
			 "effect":{"required-reviews":{"min_approvals":1}}}]}
		`)
		approve(t, svc, "bob", testHead)
		if _, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("erin"), "wrong reviewer"); err != nil {
			t.Fatal(err)
		}
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "dave"); err == nil {
			t.Fatalf("dismissed approval counted")
		}
	})
	t.Run("most-restrictive combination across overlapping rules", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, `{"version":1,"rules":[
			{"name":"a","match":{"refs":["refs/heads/main"]},
			 "effect":{"required-reviews":{"min_approvals":1}}},
			{"name":"b","match":{"refs":["refs/heads/*"]},
			 "effect":{"required-reviews":{"min_approvals":2,"bypass":["svc:merge-queue"]}}}]}
		`)
		approve(t, svc, "bob", testHead)
		// One approval satisfies a but not b (need 2); the bot bypasses b
		// but NOT a (a has no bypass) — most restrictive: still gated.
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "svc:merge-queue"); err == nil {
			t.Fatalf("bot passed rule a without approvals")
		}
		approve(t, svc, "dave", testHead)
		// Two approvals satisfy both rules' counts, and the bot bypasses
		// b — rule a has no bypass but its count is met, so the gate
		// passes for everyone now.
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "svc:merge-queue"); err != nil {
			t.Fatalf("bot with 2 approvals: %v", err)
		}
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "erin"); err != nil {
			t.Fatalf("two approvals should satisfy max(1,2): %v", err)
		}
	})
	t.Run("unparseable policy fails closed", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, `{"version":1,"rules":[{"name":"x","effect":{"bogus":{}}}]}`)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); statusFor(err) != 409 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("bad head sha rejected", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, "xyz", "refs/heads/main", "bob"); statusFor(err) != 400 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("gate never trusts the cached summary", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, gatePolicy)
		approve(t, svc, "bob", testHead)
		approve(t, svc, "dave", testHead)
		// Poison the cache: the gate must still pass by scan.
		h, _, _ := svc.loadPRHeader(ctx, testOwner, testRepo, testPR)
		h.ReviewSummary = Rollup(nil, nil, 0, 0)
		put(t, svc, ThreadKey(testOwner, testRepo, testPR), encodePRHeader(h), store.PutUpdate, mustVersion(t, svc))
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); err != nil {
			t.Fatalf("gate trusted poisoned cache: %v", err)
		}
	})
	t.Run("deadline fails closed", func(t *testing.T) {
		svc, _ := testSvc()
		svc.GateTimeout = time.Nanosecond
		seedPR(t, svc)
		seedPolicy(t, svc, gatePolicy)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := svc.CheckRequiredReviews(cctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); err == nil {
			t.Fatalf("canceled context passed")
		}
	})
}

func mustVersion(t *testing.T, svc *Service) store.Version {
	t.Helper()
	_, meta, err := store.GetBytes(context.Background(), svc.Store, ThreadKey(testOwner, testRepo, testPR), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return meta.Version
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// moveHead rewrites pr.json's head SHA (simulating a push past approvals —
// the commit_sha pin makes pre-move approvals stale).
func moveHead(t *testing.T, svc *Service, sha string) {
	t.Helper()
	raw := get(t, svc, PRKey(testOwner, testRepo, testPR))
	side, err := parseSidecar(raw)
	if err != nil {
		t.Fatal(err)
	}
	side.Head.SHA = sha
	_, meta, err := store.GetBytes(context.Background(), svc.Store, PRKey(testOwner, testRepo, testPR), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(side)
	put(t, svc, PRKey(testOwner, testRepo, testPR), out, store.PutUpdate, meta.Version)
}
