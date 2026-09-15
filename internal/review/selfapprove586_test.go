package review

// Forgejo #586: the per-repo allow_self_approval knob (default ON) +
// the merge-gate counting rule (Decision 1b: author self-approvals NEVER
// count toward min_approvals, even where submitting them is allowed).
//
// Table-driven in the service_test.go/gate_test.go style (subtests named
// by case), reusing their fixtures (testSvc/seedPR/submitAs/seedPolicy).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withPolicy wires a stub SettingsResolver: allow=true/false answers the
// knob; allowUnwired leaves the seam nil (the fresh-repo default path).
func withPolicy(svc *Service, allow *bool) {
	if allow == nil {
		svc.Settings = nil
		return
	}
	a := *allow
	svc.Settings = func(_ context.Context, _, _ string) (ReviewSettings, error) {
		return ReviewSettings{AllowSelfApproval: a}, nil
	}
}

func boolPtr(b bool) *bool { return &b }

func TestSubmitReviewSelfApproval(t *testing.T) {
	ctx := context.Background()
	allow, deny := true, false
	for _, tc := range []struct {
		name  string
		allow *bool // nil = seam unwired (fresh-repo default)
		who   string
		state string
		want  int // 0 = success, else HTTP status
	}{
		// Default ON (nil seam): the author may approve, request
		// changes, or comment on their own PR — GitHub-like.
		{"default allows author APPROVED", nil, "alice", StateApproved, 0},
		{"default allows author CHANGES_REQUESTED", nil, "alice", StateChangesRequested, 0},
		{"default allows author COMMENTED", nil, "alice", StateCommented, 0},
		{"default allows non-author APPROVED", nil, "bob", StateApproved, 0},
		// Explicit ON: same as the default, verbatim.
		{"on allows author APPROVED", &allow, "alice", StateApproved, 0},
		{"on allows author CHANGES_REQUESTED", &allow, "alice", StateChangesRequested, 0},
		{"on allows author COMMENTED", &allow, "alice", StateCommented, 0},
		{"on allows non-author APPROVED", &allow, "bob", StateApproved, 0},
		// Explicit OFF: the historical 422 stands for the author on
		// verdict states (Decision 2: the one toggle couples APPROVED
		// and CHANGES_REQUESTED); COMMENTED stays open; non-authors
		// are unaffected.
		{"off denies author APPROVED with 422", &deny, "alice", StateApproved, 422},
		{"off denies author CHANGES_REQUESTED with 422", &deny, "alice", StateChangesRequested, 422},
		{"off allows author COMMENTED", &deny, "alice", StateCommented, 0},
		{"off allows non-author APPROVED", &deny, "bob", StateApproved, 0},
		{"off allows non-author CHANGES_REQUESTED", &deny, "carol", StateChangesRequested, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := testSvc()
			seedPR(t, svc)
			withPolicy(svc, tc.allow)
			_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR,
				testPrincipal(tc.who), submitAs(svc, tc.who, tc.state, testHead))
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("submit = %v, want success", err)
				}
				return
			}
			if err == nil || statusFor(err) != tc.want {
				t.Fatalf("submit = %v, want status %d", err, tc.want)
			}
		})
	}
}

func TestSubmitReviewSelfApprovalDeniedText(t *testing.T) {
	// The 422 text is verbatim (plain-text wire contract, 07 §2).
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	withPolicy(svc, boolPtr(false))
	_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR,
		testPrincipal("alice"), submitAs(svc, "alice", StateApproved, testHead))
	if err == nil || !strings.Contains(err.Error(), "author cannot approve their own pull request") {
		t.Fatalf("err = %v, want the verbatim 422", err)
	}
}

func TestSubmitReviewPolicyErrorFailsClosed(t *testing.T) {
	// A resolver that cannot answer fails the submit closed (503), never
	// silently allowed or denied.
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	svc.Settings = func(_ context.Context, _, _ string) (ReviewSettings, error) {
		return ReviewSettings{}, errors.New("manifest down")
	}
	_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR,
		testPrincipal("alice"), submitAs(svc, "alice", StateApproved, testHead))
	if err == nil || statusFor(err) != 503 {
		t.Fatalf("err = %v, want 503", err)
	}
	// Non-authors are unaffected by the policy read failing… except the
	// check only runs for the author, so a non-author submit still
	// succeeds (the seam is consulted only on the author path).
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR,
		testPrincipal("bob"), submitAs(svc, "bob", StateApproved, testHead)); err != nil {
		t.Fatalf("non-author submit with broken resolver = %v, want success", err)
	}
}

func TestDefaultReviewSettings(t *testing.T) {
	if got := DefaultReviewSettings(); !got.AllowSelfApproval {
		t.Fatalf("default = %+v, want self-approval allowed", got)
	}
	svc, _ := testSvc()
	svc.Settings = nil
	pol, err := svc.reviewPolicy(context.Background(), testOwner, testRepo)
	if err != nil || !pol.AllowSelfApproval {
		t.Fatalf("nil seam = %+v %v, want allowed default", pol, err)
	}
}

func TestGateNeverCountsSelfApproval(t *testing.T) {
	// Decision 1b: author self-approvals never count toward
	// min_approvals — even fresh, even where submitting them is allowed
	// (submit-time toggle governs recording; the gate governs protection).
	ctx := context.Background()
	oneApproval := `{"version":1,"rules":[
		{"name":"pr-gate","match":{"refs":["refs/heads/main"]},
		 "effect":{"required-reviews":{"min_approvals":1}}}]}
	`
	submit := func(t *testing.T, svc *Service, who, state, sha string) {
		t.Helper()
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal(who),
			SubmitInput{State: state, CommitSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("fresh self-approval alone does not satisfy min_approvals 1", func(t *testing.T) {
		svc, _ := testSvc() // nil seam: submitting it is allowed
		seedPR(t, svc)
		seedPolicy(t, svc, oneApproval)
		submit(t, svc, "alice", StateApproved, testHead)
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob")
		if err == nil || !strings.Contains(err.Error(), "need 1 approvals, have 0") {
			t.Fatalf("err = %v, want the 0-count shortfall", err)
		}
		v, verr := svc.EvaluateGate(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob")
		if verr == nil || v == nil || v.Approvals != 0 || v.Needed != 1 {
			t.Fatalf("verdict = %+v %v, want 0/1", v, verr)
		}
	})
	t.Run("self-approval plus one real approval satisfies min_approvals 1 with count 1", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, oneApproval)
		submit(t, svc, "alice", StateApproved, testHead)
		submit(t, svc, "bob", StateApproved, testHead)
		v, err := svc.EvaluateGate(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob")
		if err != nil {
			t.Fatalf("gate = %v, want pass", err)
		}
		if v.Approvals != 1 {
			t.Fatalf("approvals = %d, want 1 (self-approval excluded)", v.Approvals)
		}
	})
	t.Run("author CHANGES_REQUESTED still blocks like any reviewer", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, oneApproval)
		submit(t, svc, "bob", StateApproved, testHead)
		submit(t, svc, "alice", StateChangesRequested, testHead)
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob")
		if err == nil || !strings.Contains(err.Error(), "changes requested by alice") {
			t.Fatalf("err = %v, want the author block", err)
		}
	})
	t.Run("case-insensitive author match excludes the vote", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, oneApproval)
		submit(t, svc, "Alice", StateApproved, testHead)
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob")
		if err == nil || !strings.Contains(err.Error(), "have 0") {
			t.Fatalf("err = %v, want the 0-count shortfall", err)
		}
	})
}

func TestGateStaleSelfApproval(t *testing.T) {
	// Decision 3: dismiss_stale needs no special handling — a stale
	// self-approval already fails the freshness check like any other
	// stale approval (and, excluded as an author vote, it never counts
	// fresh either — both halves pinned here).
	ctx := context.Background()
	stalePolicy := `{"version":1,"rules":[
		{"name":"pr-gate","match":{"refs":["refs/heads/main"]},
		 "effect":{"required-reviews":{"min_approvals":1,"dismiss_stale":true}}}]}
	`
	submit := func(t *testing.T, svc *Service, who, sha string) {
		t.Helper()
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal(who),
			SubmitInput{State: StateApproved, CommitSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("stale self-approval does not satisfy the gate", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, stalePolicy)
		submit(t, svc, "alice", testHead)
		moveHead(t, svc, testHead2)
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead2, "refs/heads/main", "bob")
		if err == nil || !strings.Contains(err.Error(), "need 1 approvals, have 0") {
			t.Fatalf("err = %v, want the 0-count shortfall", err)
		}
	})
	t.Run("stale non-author approval does not satisfy the gate (control)", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, stalePolicy)
		submit(t, svc, "bob", testHead)
		moveHead(t, svc, testHead2)
		if err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead2, "refs/heads/main", "bob"); err == nil {
			t.Fatal("stale approval counted")
		}
	})
	t.Run("author re-approving the new head still does not count", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedPolicy(t, svc, stalePolicy)
		submit(t, svc, "alice", testHead)
		moveHead(t, svc, testHead2)
		// The author cannot re-approve the moved head through the
		// normal pin (their old review pins testHead); submitting a
		// fresh self-approval on the new head is allowed but excluded.
		submit(t, svc, "alice", testHead2)
		err := svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead2, "refs/heads/main", "bob")
		if err == nil || !strings.Contains(err.Error(), "have 0") {
			t.Fatalf("err = %v, want the 0-count shortfall", err)
		}
	})
}

func TestGateSelfApprovalTable(t *testing.T) {
	// End-to-end through the seam: submits recorded under each policy,
	// then one gate evaluation per row (min_approvals 2, no staleness).
	ctx := context.Background()
	policy := `{"version":1,"rules":[
		{"name":"pr-gate","match":{"refs":["refs/heads/main"]},
		 "effect":{"required-reviews":{"min_approvals":2}}}]}
	`
	for _, tc := range []struct {
		name      string
		allow     *bool
		submits   [][2]string // {who, state}
		wantPass  bool
		wantCount int
	}{
		{
			name:      "two non-author approvals pass",
			allow:     boolPtr(false),
			submits:   [][2]string{{"bob", StateApproved}, {"carol", StateApproved}},
			wantPass:  true,
			wantCount: 2,
		},
		{
			name:      "self plus one real approval still short",
			allow:     nil, // default ON so the self-approval records
			submits:   [][2]string{{"alice", StateApproved}, {"bob", StateApproved}},
			wantPass:  false,
			wantCount: 1,
		},
		{
			name:      "self plus two real approvals pass with count 2",
			allow:     nil,
			submits:   [][2]string{{"alice", StateApproved}, {"bob", StateApproved}, {"carol", StateApproved}},
			wantPass:  true,
			wantCount: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := testSvc()
			seedPR(t, svc)
			seedPolicy(t, svc, policy)
			withPolicy(svc, tc.allow)
			for _, s := range tc.submits {
				if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal(s[0]),
					SubmitInput{State: s[1], CommitSHA: testHead}); err != nil {
					t.Fatal(err)
				}
			}
			v, err := svc.EvaluateGate(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "dave")
			if tc.wantPass && err != nil {
				t.Fatalf("gate = %v, want pass", err)
			}
			if !tc.wantPass && err == nil {
				t.Fatal("gate passed, want shortfall")
			}
			if v == nil || v.Approvals != tc.wantCount {
				t.Fatalf("verdict = %+v, want count %d", v, tc.wantCount)
			}
		})
	}
}
