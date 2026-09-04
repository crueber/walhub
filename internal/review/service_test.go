package review

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func submitAs(svc *Service, who, state, sha string) SubmitInput {
	return SubmitInput{State: state, CommitSHA: sha}
}

func TestSubmitReview(t *testing.T) {
	ctx := context.Background()
	t.Run("approve happy path reserves seq 1 and retires request", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob", "carol"}); err != nil {
			t.Fatal(err)
		}
		ev, threads, sum, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateApproved, testHead))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != 1 || ev.State != StateApproved || ev.CommitSHA != testHead {
			t.Fatalf("%+v", ev)
		}
		if len(threads) != 0 {
			t.Fatalf("threads: %+v", threads)
		}
		if sum.Decision != DecisionApproved || sum.Approvals != 1 {
			t.Fatalf("%+v", sum)
		}
		// bob's request retired; carol remains.
		if len(sum.Requested) != 1 || sum.Requested[0] != "carol" {
			t.Fatalf("requested: %+v", sum.Requested)
		}
		// Header counters advanced.
		h, _, _ := svc.loadPRHeader(ctx, testOwner, testRepo, testPR)
		if h.NextReviewSeq != 2 {
			t.Fatalf("next_review_seq %d", h.NextReviewSeq)
		}
	})

	t.Run("second submit reserves seq 2", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateApproved, testHead)); err != nil {
			t.Fatal(err)
		}
		ev, _, sum, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), submitAs(svc, "carol", StateCommented, testHead))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != 2 {
			t.Fatalf("seq %d", ev.Seq)
		}
		if sum.Decision != DecisionApproved || len(sum.Latest) != 2 {
			t.Fatalf("%+v", sum)
		}
	})

	t.Run("with attached threads opens them atomically", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		a2 := testAnchor()
		a2.Path = "src/other.go"
		ev, threads, sum, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{
				{Anchor: testAnchor(), Body: "nit: rename"},
				{Anchor: a2, Body: "second"},
			}})
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != 1 || len(threads) != 2 {
			t.Fatalf("%+v %+v", ev, threads)
		}
		if threads[0].TID != "00000001" || threads[1].TID != "00000002" {
			t.Fatalf("%+v", threads)
		}
		if sum.ThreadsTotal != 2 || sum.ThreadsUnresolved != 2 {
			t.Fatalf("%+v", sum)
		}
		h, _, _ := svc.loadPRHeader(ctx, testOwner, testRepo, testPR)
		if h.NextThreadNum != 3 {
			t.Fatalf("next_thread_num %d", h.NextThreadNum)
		}
	})

	t.Run("table: rejections", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			who   auth.Principal
			in    SubmitInput
			check func(error) bool
		}{
			{"anonymous 401", auth.Anonymous(), submitAs(nil, "x", StateApproved, testHead),
				func(e error) bool { return statusFor(e) == 401 }},
			{"bad state 400", testPrincipal("bob"), submitAs(nil, "bob", "APPROVE", testHead),
				func(e error) bool { return statusFor(e) == 400 }},
			{"bad sha 400", testPrincipal("bob"), submitAs(nil, "bob", StateApproved, "xyz"),
				func(e error) bool { return statusFor(e) == 400 }},
			{"stale sha 409", testPrincipal("bob"), submitAs(nil, "bob", StateApproved, testHead2),
				func(e error) bool { return statusFor(e) == 409 }},
			{"author approve 422", testPrincipal("alice"), submitAs(nil, "alice", StateApproved, testHead),
				func(e error) bool { return statusFor(e) == 422 }},
			{"author request-changes 422", testPrincipal("alice"), submitAs(nil, "alice", StateChangesRequested, testHead),
				func(e error) bool { return statusFor(e) == 422 }},
			{"unknown PR 404", testPrincipal("bob"), submitAs(nil, "bob", StateApproved, testHead),
				func(e error) bool { return statusFor(e) == 404 }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc, _ := testSvc()
				if !strings.Contains(tc.name, "unknown PR") {
					seedPR(t, svc)
				}
				_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, tc.who, tc.in)
				if err == nil || !tc.check(err) {
					t.Fatalf("err=%v", err)
				}
			})
		}
	})

	t.Run("author COMMENTED allowed", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), submitAs(svc, "alice", StateCommented, testHead)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("409 text is verbatim", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateApproved, testHead2))
		if err == nil || !strings.Contains(err.Error(), "reviewed commit is not the pull request head") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("bad thread bodies rejected before reservation", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		bad := testAnchor()
		bad.Side = "LEFT"
		_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{{Anchor: bad, Body: "x"}}})
		if statusFor(err) != 400 {
			t.Fatalf("err=%v", err)
		}
		_, _, _, err = svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{{Anchor: testAnchor(), Body: "  "}}})
		if statusFor(err) != 400 {
			t.Fatalf("err=%v", err)
		}
		h, _, _ := svc.loadPRHeader(ctx, testOwner, testRepo, testPR)
		if h.NextReviewSeq != 0 || h.NextThreadNum != 0 {
			t.Fatalf("reservation leaked: %+v", h)
		}
	})
}

func TestDismissReview(t *testing.T) {
	ctx := context.Background()
	seedAndApprove := func(t *testing.T, svc *Service) {
		t.Helper()
		seedPR(t, svc)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateApproved, testHead)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("maintain dismisses, summary demotes", func(t *testing.T) {
		svc, _ := testSvc()
		seedAndApprove(t, svc)
		ev, sum, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("bob"), "stale")
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != KindReviewDismissed || ev.Seq != 2 || ev.Dismisses == nil || *ev.Dismisses != 1 {
			t.Fatalf("%+v", ev)
		}
		if sum.Decision != DecisionReviewRequired || sum.Latest["bob"].State != StateDismissed {
			t.Fatalf("%+v", sum)
		}
	})
	t.Run("double dismiss 409", func(t *testing.T) {
		svc, _ := testSvc()
		seedAndApprove(t, svc)
		if _, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("bob"), "stale"); err != nil {
			t.Fatal(err)
		}
		_, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("bob"), "again")
		if statusFor(err) != 409 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("table: rejections", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			who    auth.Principal
			seq    int
			reason string
			want   int
		}{
			{"non-maintain 403", testPrincipal("carol"), 1, "x", 403},
			{"anonymous 401", auth.Anonymous(), 1, "x", 401},
			{"unknown review 404", testPrincipal("bob"), 99, "x", 404},
			{"empty reason 400", testPrincipal("bob"), 1, "  ", 400},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc, _ := testSvc()
				seedAndApprove(t, svc)
				_, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, tc.seq, tc.who, tc.reason)
				if err == nil || statusFor(err) != tc.want {
					t.Fatalf("err=%v want %d", err, tc.want)
				}
			})
		}
	})
	t.Run("dismissing a dismissal 409", func(t *testing.T) {
		svc, _ := testSvc()
		seedAndApprove(t, svc)
		if _, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("bob"), "stale"); err != nil {
			t.Fatal(err)
		}
		_, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 2, testPrincipal("erin"), "nope")
		if statusFor(err) != 409 {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestListGetReviews(t *testing.T) {
	ctx := context.Background()
	t.Run("list newest-first with after/n windows", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		for _, who := range []string{"bob", "carol", "dave"} {
			if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal(who), submitAs(svc, who, StateApproved, testHead)); err != nil {
				t.Fatal(err)
			}
		}
		res, err := svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Reviews) != 3 || res.More || res.Reviews[0].Seq != 3 {
			t.Fatalf("%+v", res)
		}
		res, err = svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 3, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Reviews) != 2 || res.Reviews[0].Seq != 2 {
			t.Fatalf("%+v", res)
		}
		res, err = svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Reviews) != 2 || !res.More {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("get + 404s", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateApproved, testHead)); err != nil {
			t.Fatal(err)
		}
		ev, err := svc.GetReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("carol"))
		if err != nil || ev.By != "bob" {
			t.Fatalf("%+v %v", ev, err)
		}
		if _, err := svc.GetReview(ctx, testOwner, testRepo, testPR, 42, testPrincipal("carol")); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
		if _, err := svc.GetReview(ctx, testOwner, testRepo, 999, 1, testPrincipal("carol")); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
		if _, err := svc.ListReviews(ctx, testOwner, testRepo, testPR, auth.Anonymous(), 0, 0); statusFor(err) != 401 {
			t.Fatalf("err=%v", err)
		}
	})
}
