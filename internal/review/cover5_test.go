package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"

	"git.packden.us/crueber/walhub/internal/store"
)

// This file covers the last arms: participant-scan resolution (fred
// comments then resolves; gina reads but never participates), skipped
// reads via store.Delete, dismissal-sequence edge cases, request-mutation
// exhaustion, suggest fault/overflow, and the HTTP tails.

func TestParticipantScanResolve(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
	if err != nil {
		t.Fatal(err)
	}
	// fred reads and comments (participant, not triage): the scan admits.
	if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("fred"), "ack"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("fred")); err != nil {
		t.Fatalf("participant: %v", err)
	}
	if _, err := svc.UnresolveThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("fred")); err != nil {
		t.Fatal(err)
	}
	// gina reads but never participates: the scan runs and denies.
	if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("gina")); statusFor(err) != 403 {
		t.Fatalf("stranger err=%v", err)
	}
	// Public repo, anonymous resolve: requireRole's anonymous arm → 401.
	svc2, roles := testSvc()
	roles.Public = true
	seedPR(t, svc2)
	th2, err := svc2.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.ResolveThread(ctx, testOwner, testRepo, testPR, th2.TID, auth.Anonymous()); statusFor(err) != 401 {
		t.Fatalf("anon err=%v", err)
	}
	// Bad tid shape at the service level.
	if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, "zzz", testPrincipal("dave")); statusFor(err) != 404 {
		t.Fatalf("tid err=%v", err)
	}
}

func TestSkippedReads(t *testing.T) {
	ctx := context.Background()
	del := func(svc *Service, key string) {
		_, meta, err := store.GetBytes(ctx, svc.Store, key, store.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		if err := svc.Store.Delete(ctx, key, meta.Version); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
	}
	t.Run("deleted review event skips the seq", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		for _, who := range []string{"bob", "carol"} {
			if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal(who),
				SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
				t.Fatal(err)
			}
		}
		del(svc, ReviewKey(testOwner, testRepo, testPR, 1))
		res, err := svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 10)
		if err != nil || len(res.Reviews) != 1 || res.Reviews[0].Seq != 2 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("deleted thread header skips", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		del(svc, ReviewThreadKey(testOwner, testRepo, testPR, th.TID))
		res, err := svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "", 10)
		if err != nil || len(res.Threads) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		if _, err := svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 10); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("deleted comment normalizes to []", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		del(svc, ReviewThreadEventKey(testOwner, testRepo, testPR, th.TID, 1))
		view, err := svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 10)
		if err != nil || len(view.Comments) != 0 {
			t.Fatalf("%+v %v", view, err)
		}
	})
}

func TestDismissalEdges(t *testing.T) {
	ctx := context.Background()
	t.Run("NextReviewSeq defaults to 1 without prior reviews", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		// Hand-seed review seq 5 (bypassing the allocator, which stays 0):
		// the dismissal reserves seq 1 via the nextSeq<1 default.
		ev := &ReviewEvent{Kind: KindReview, Seq: 5, At: "2026-09-04T12:00:00Z", By: "bob", State: StateApproved, CommitSHA: testHead}
		put(t, svc, ReviewKey(testOwner, testRepo, testPR, 5), encodeReview(ev), store.PutCreate, "")
		dev, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 5, testPrincipal("erin"), "stale")
		if err != nil || dev.Seq != 1 {
			t.Fatalf("%+v %v", dev, err)
		}
	})
	t.Run("dismissing history while newer stands is recorded but inert", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		for _, st := range []string{StateApproved, StateCommented} {
			if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
				SubmitInput{State: st, CommitSHA: testHead}); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("erin"), "history"); err != nil {
			t.Fatal(err)
		}
		sum, err := svc.refreshSummary(ctx, testOwner, testRepo, testPR)
		if err != nil || sum.Latest["bob"].State != StateCommented {
			t.Fatalf("%+v %v", sum, err)
		}
	})
	t.Run("dismissal create arms", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		if _, _, _, err := e.svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, CommitSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		e.failPutErr(ReviewKey(testOwner, testRepo, testPR, 2), errors.New("bad create"))
		if _, _, err := e.svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("erin"), "x"); err == nil {
			t.Fatal("dismissal put err")
		}
		e.clearFails()
		// The failed reservation above bumped the allocator, so the next
		// dismissal reserves seq 3: fault that key for the 412 arm.
		e.failPut(ReviewKey(testOwner, testRepo, testPR, 3), 1)
		if _, _, err := e.svc.DismissReview(ctx, testOwner, testRepo, testPR, 1, testPrincipal("erin"), "x"); !errors.Is(err, ErrConflict) {
			t.Fatalf("dismissal 412: %v", err)
		}
		e.clearFails()
	})
}

func TestRequestMutationEdges(t *testing.T) {
	ctx := context.Background()
	t.Run("exhaustion normalizes to conflict", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failPut(ReviewRequestsKey(testOwner, testRepo, testPR), 99)
		if _, err := e.svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("err=%v", err)
		}
		e.clearFails()
	})
	t.Run("remove with corrupt index fails closed", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		put(t, e.svc, ReviewRequestsKey(testOwner, testRepo, testPR), []byte("{"), store.PutCreate, "")
		if _, err := e.svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("remove empty list rejected", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("long bodies rejected", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		big := strings.Repeat("x", MaxBodyBytes+1)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateApproved, Body: big, CommitSHA: testHead}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("big review: %v", err)
		}
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{{Anchor: testAnchor(), Body: big}}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("big thread: %v", err)
		}
		if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), big); !errors.Is(err, ErrInvalid) {
			t.Fatalf("big open: %v", err)
		}
		if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, "00000001", testPrincipal("carol"), big); err == nil {
			t.Fatal("big comment on missing thread should 404 first")
		}
	})
	t.Run("default page sizes", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		res, err := svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 0)
		if err != nil || len(res.Reviews) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		lr, err := svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "", 0)
		if err != nil || len(lr.Threads) != 0 {
			t.Fatalf("%+v %v", lr, err)
		}
	})
	t.Run("after-cursor + truncation windows", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		a2 := testAnchor()
		a2.Path = "b.go"
		if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), a2, "two"); err != nil {
			t.Fatal(err)
		}
		res, err := svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "00000001", 10)
		if err != nil || len(res.Threads) != 1 || res.Threads[0].TID != "00000002" {
			t.Fatalf("%+v %v", res, err)
		}
		res, err = svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "", 1)
		if err != nil || len(res.Threads) != 1 || !res.More {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("suggest fault + overflow", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		e.failGet(AccessKey(testOwner, testRepo), errors.New("disk down"))
		if _, err := e.svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), ""); err == nil {
			t.Fatal("access get err")
		}
		e.clearFails()
		var bindings []string
		for i := 0; i < 25; i++ {
			bindings = append(bindings, `{"subject":"user:u`+strings.Repeat("x", i+1)+`","role":"read"}`)
		}
		put(t, e.svc, AccessKey(testOwner, testRepo), []byte(`{"version":1,"role_bindings":[`+strings.Join(bindings, ",")+`]}`), store.PutCreate, "")
		out, err := e.svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "")
		if err != nil || len(out) != 20 {
			t.Fatalf("%d %v", len(out), err)
		}
	})
	t.Run("comment create backend failure", func(t *testing.T) {
		e := newFlakyEnv()
		seedPR(t, e.svc)
		th, err := e.svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		e.failPutErr(ReviewThreadEventKey(testOwner, testRepo, testPR, th.TID, 2), errors.New("bad create"))
		if _, err := e.svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("bob"), "x"); err == nil {
			t.Fatal("put err")
		}
		e.clearFails()
	})
	t.Run("refresh on missing header 404s", func(t *testing.T) {
		svc, _ := testSvc()
		if _, err := svc.GetRequests(ctx, testOwner, testRepo, 999, testPrincipal("carol")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err=%v", err)
		}
		if _, err := svc.GetReview(ctx, testOwner, testRepo, testPR, 1, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestHTTPTails(t *testing.T) {
	h, _ := testHandler(t)
	base := "/o/r/api/pulls/7"
	// /pulls with no num is 03's surface — we decline (false → 404 here).
	if w := doReq(h, "GET", "/o/r/api/pulls", "carol", ""); w.Code != 404 {
		t.Fatalf("code %d", w.Code)
	}
	// Explicit page sizes ride queryInt's valid arm.
	if w := doReq(h, "GET", base+"/reviews?n=1", "carol", ""); w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	// Decode failures on every body-taking endpoint.
	for name, tc := range map[string]struct {
		method, path, body string
		want               int
	}{
		"dismiss bad json":   {"POST", base + "/reviews/1/dismiss", "{", 400},
		"open bad json":      {"POST", base + "/threads", "{", 400},
		"comment bad json":   {"POST", base + "/threads/00000001/comments", "{", 400},
		"add-req bad json":   {"POST", base + "/review-requests", "{", 400},
		"rm-req bad json":    {"DELETE", base + "/review-requests", "{", 400},
		"submit empty":       {"POST", base + "/reviews", "", 400},
		"thread wrong shape": {"POST", base + "/threads", `{"anchor":1}`, 400},
	} {
		t.Run(name, func(t *testing.T) {
			if w := doReq(h, tc.method, tc.path, "bob", tc.body); w.Code != tc.want {
				t.Fatalf("= %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
	// Submit with attached threads over HTTP.
	anchor := `{"path":"src/main.go","side":"NEW","new_start":120,"new_lines":3,` +
		`"commit_sha":"` + testHead + `",` +
		`"context_sha":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`
	w := doReq(h, "POST", base+"/reviews", "bob",
		`{"state":"COMMENTED","commit_sha":"`+testHead+`","threads":[{"anchor":`+anchor+`,"body":"nit"}]}`)
	if w.Code != 201 {
		t.Fatalf("submit+threads: %d %s", w.Code, w.Body.String())
	}
}
