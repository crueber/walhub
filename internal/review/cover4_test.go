package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file mops up the remaining store-error arms: every load/scan/parse
// failure behind each method, the Create-collision paths on the submit
// flow, pagination clamps, and the admin/Write fast paths on the role
// gates.

func TestFlakyLoadArms(t *testing.T) {
	ctx := context.Background()
	diskDown := errors.New("disk down")
	t.Run("sidecar/requests/review-key get failures", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failGet(PRKey(testOwner, testRepo, testPR), diskDown)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err == nil {
			t.Fatal("sidecar get err")
		}
		e.clearFails()
		e.failGet(ReviewRequestsKey(testOwner, testRepo, testPR), diskDown)
		if _, err := e.svc.GetRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol")); err == nil {
			t.Fatal("requests get err")
		}
		e.clearFails()
	})
	t.Run("scan get/parse failures", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		// Corrupt the stored review: every scan fails closed.
		put(t, e.svc, ReviewKey(testOwner, testRepo, testPR, 1), []byte("{"), store.PutUpdate, revVersion(t, e.svc, 1))
		if _, err := e.svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 10); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt scan: %v", err)
		}
		if _, _, err := e.svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("erin"), "x"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt dismiss scan: %v", err)
		}
		if _, err := e.svc.GetReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("carol")); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt get: %v", err)
		}
	})
	t.Run("scan get failure", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		e.failGet(ReviewKey(testOwner, testRepo, testPR, 1), diskDown)
		if _, err := e.svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 10); err == nil {
			t.Fatal("scan get err")
		}
		e.clearFails()
	})
	t.Run("thread header/comment scan failures", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		th, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		tkey := ReviewThreadKey(testOwner, testRepo, testPR, th.TID)
		put(t, e.svc, tkey, []byte("{"), store.PutUpdate, threadVersion(t, e.svc, tkey))
		if _, err := e.svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 10); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt thread: %v", err)
		}
		if _, err := e.svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("bob"), "x"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt comment base: %v", err)
		}
		if _, err := e.svc.ResolveThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("dave")); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt resolve base: %v", err)
		}
	})
	t.Run("thread comment scan failures", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		th, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		ckey := ReviewThreadEventKey(testOwner, testRepo, testPR, th.TID, 1)
		put(t, e.svc, ckey, []byte("{"), store.PutUpdate, threadVersion(t, e.svc, ckey))
		if _, err := e.svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 10); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt comment: %v", err)
		}
	})
	t.Run("reserveTID parse/kind/put arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		hdr := ThreadKey(testOwner, testRepo, testPR)
		raw := get(t, e.svc, hdr)
		h, _ := parsePRHeader(raw)
		h.Kind = "issue"
		_, meta, _ := store.GetBytes(ctx, e.svc.Store, hdr, store.GetOptions{})
		put(t, e.svc, hdr, encodePRHeader(h), store.PutUpdate, meta.Version)
		if _, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("kind: %v", err)
		}
	})
	t.Run("reserveTID corrupt + put failure", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		hdr := ThreadKey(testOwner, testRepo, testPR)
		put(t, e.svc, hdr, []byte("{"), store.PutUpdate, mustVersionFlaky(t, e.svc, hdr))
		if _, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "x"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt: %v", err)
		}
	})
	t.Run("submit attached-thread create arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		// Squat the tid the submit will allocate: header Create 412s.
		th := &ThreadHeader{TID: "00000001", Num: testPR, Kind: "review_thread", Version: 1}
		put(t, e.svc, ReviewThreadKey(testOwner, testRepo, testPR, "00000001"), encodeThreadHeader(th), store.PutCreate, "")
		in := SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{{Anchor: testAnchor(), Body: "x"}}}
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); !errors.Is(err, ErrConflict) {
			t.Fatalf("header collision: %v", err)
		}
	})
	t.Run("submit first-comment create arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		// Header lands (fresh tid), first comment Create 412s.
		c := &ThreadComment{Kind: KindReviewThreadComment, Seq: 1, At: "x", By: "mallory", Body: "squat"}
		put(t, e.svc, ReviewThreadEventKey(testOwner, testRepo, testPR, "00000001", 1), encodeThreadComment(c), store.PutCreate, "")
		in := SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{{Anchor: testAnchor(), Body: "x"}}}
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); !errors.Is(err, ErrConflict) {
			t.Fatalf("comment collision: %v", err)
		}
		e.clearFails()
		e2 := newFlakyEnv()
		seedPR(t, e2.svc)
		e2.failPutErr(ReviewThreadEventKey(testOwner, testRepo, testPR, "00000001", 1), diskDown)
		if _, _, _, err := e2.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), in); err == nil {
			t.Fatal("comment put err")
		}
		e2.clearFails()
	})
	t.Run("submit with corrupt requests fails in removeRequester", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		put(t, e.svc, ReviewRequestsKey(testOwner, testRepo, testPR), []byte("{"), store.PutCreate, "")
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("refresh put failure + exhaustion still return the value", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPutErr(ThreadKey(testOwner, testRepo, testPR), diskDown)
		if _, err := e.svc.refreshSummary(ctx, testOwner, testRepo, testPR); err == nil {
			t.Fatal("put err")
		}
		e.clearFails()
		e.failPut(ThreadKey(testOwner, testRepo, testPR), 99)
		sum, err := e.svc.refreshSummary(ctx, testOwner, testRepo, testPR)
		if err != nil || sum.Decision != DecisionReviewRequired {
			t.Fatalf("%+v %v", sum, err)
		}
		e.clearFails()
	})
	t.Run("openThread create backend failures", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPutErr(ReviewThreadKey(testOwner, testRepo, testPR, "00000001"), diskDown)
		if _, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "x"); err == nil {
			t.Fatal("header put err")
		}
		e.clearFails()
	})
	t.Run("resolve put backend failure", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		th, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		e.failPutErr(ReviewThreadKey(testOwner, testRepo, testPR, th.TID), diskDown)
		if _, err := e.svc.ResolveThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("dave")); err == nil {
			t.Fatal("put err")
		}
		e.clearFails()
	})
	t.Run("gate on missing PR", func(t *testing.T) {
		e := newFlakyEnv()
		seedPolicy(t, e.svc, gatePolicy)
		if err := e.svc.CheckRequiredReviews(ctx, testOwner, testRepo, testPR, testHead, "refs/heads/main", "bob"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("pagination clamps", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		res, err := e.svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 10000)
		if err != nil || len(res.Reviews) != 1 {
			t.Fatalf("%+v %v", res, err)
		}
		th, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		lr, err := e.svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "", 10000)
		if err != nil || len(lr.Threads) != 1 {
			t.Fatalf("%+v %v", lr, err)
		}
		view, err := e.svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 10000)
		if err != nil || len(view.Comments) != 1 {
			t.Fatalf("%+v %v", view, err)
		}
		if _, err := e.svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), many(51)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("51 reviewers: %v", err)
		}
	})
	t.Run("role fast paths", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if err := e.svc.requireRole(ctx, testOwner, testRepo, auth.Principal{Name: "root", Admin: true}, "admin"); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.requireRead(ctx, testOwner, testRepo, auth.Principal{Name: "w", Write: true}); err != nil {
			t.Fatal(err)
		}
		if got := e.svc.roleOf(ctx, testOwner, testRepo, auth.Principal{Name: "root", Admin: true}); got != "admin" {
			t.Fatalf("got %q", got)
		}
		if _, err := e.svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"  "}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("blank add: %v", err)
		}
	})
	t.Run("suggest skips low roles and empty teams", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		put(t, e.svc, AccessKey(testOwner, testRepo), []byte(`{"version":1,"role_bindings":[
			{"subject":"user:bob","role":"maintain"},{"subject":"team:o/devs","role":"triage"}]}`), store.PutCreate, "")
		out, err := e.svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0] != "bob" {
			t.Fatalf("%v (nil expander skips teams)", out)
		}
	})
}

func many(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "u" + strings.Repeat("x", i+1)
	}
	return out
}

func revVersion(t *testing.T, svc *Service, seq int) store.Version {
	t.Helper()
	_, meta, err := store.GetBytes(context.Background(), svc.Store, ReviewKey(testOwner, testRepo, testPR, seq), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return meta.Version
}

func threadVersion(t *testing.T, svc *Service, key string) store.Version {
	t.Helper()
	_, meta, err := store.GetBytes(context.Background(), svc.Store, key, store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return meta.Version
}

func mustVersionFlaky(t *testing.T, svc *Service, key string) store.Version {
	t.Helper()
	_, meta, err := store.GetBytes(context.Background(), svc.Store, key, store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return meta.Version
}
