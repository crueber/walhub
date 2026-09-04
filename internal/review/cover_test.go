package review

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file covers the defensive branches: nil seams, corrupt objects,
// malformed wire input, CAS collisions, and concurrent writers. It exists
// to hold the ≥95% statement gate with intent, not padding — every case
// names the branch it pins.

// errRoles scripts CheckRead failures (the forbidden/unavailable/default
// arms of requireRead).
type errRoles struct {
	FakeRoles
	checkErr *auth.AuthError
}

func (f *errRoles) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return f.checkErr
}

func TestNilSeams(t *testing.T) {
	ctx := context.Background()
	t.Run("nil roles falls back to principal flags", func(t *testing.T) {
		svc := New(store.NewMemory(), nil)
		seedPR(t, svc)
		if err := svc.requireRole(ctx, testOwner, testRepo, auth.Principal{Name: "x", Admin: true}, "admin"); err != nil {
			t.Fatal(err)
		}
		if err := svc.requireRole(ctx, testOwner, testRepo, auth.Principal{Name: "x", Write: true}, "write"); err != nil {
			t.Fatal(err)
		}
		if got := svc.roleOf(ctx, testOwner, testRepo, auth.Anonymous()); got != "" {
			t.Fatalf("anon role %q", got)
		}
		if got := svc.roleOf(ctx, testOwner, testRepo, testPrincipal("x")); got != "read" {
			t.Fatalf("default role %q", got)
		}
		if err := svc.requireRead(ctx, testOwner, testRepo, auth.Anonymous()); statusFor(err) != 401 {
			t.Fatalf("err=%v", err)
		}
		if err := svc.requireRead(ctx, testOwner, testRepo, testPrincipal("x")); err != nil {
			t.Fatal(err)
		}
		// Nil clock falls back to wall time.
		svc.Now = nil
		if svc.nowUTC().IsZero() {
			t.Fatalf("zero clock")
		}
		if svc.gateTimeout() == 0 {
			t.Fatalf("zero gate timeout")
		}
		svc.GateTimeout = 1
		if svc.gateTimeout() != 1 {
			t.Fatalf("override ignored")
		}
	})
	t.Run("checkread error arms", func(t *testing.T) {
		for _, tc := range []struct {
			kind *auth.AuthError
			want int
		}{
			{&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, 403},
			{&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, 500},
			{&auth.AuthError{Kind: auth.ErrUnauthorized, Why: "who"}, 401},
		} {
			svc := New(store.NewMemory(), &errRoles{checkErr: tc.kind})
			if err := svc.requireRead(ctx, testOwner, testRepo, testPrincipal("x")); statusFor(err) != tc.want {
				t.Fatalf("%v: err=%v want %d", tc.kind, err, tc.want)
			}
		}
	})
	t.Run("nil authenticator falls back to anonymous", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		h := &Handler{Svc: svc}
		w := doReq(h, "GET", "/o/r/api/pulls/7/reviews", "", "")
		if w.Code != 401 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("handler without match answers 404", func(t *testing.T) {
		svc, _ := testSvc()
		h := &Handler{Svc: svc}
		w := doReq(h, "GET", "/o/r/api/pulls", "", "")
		if w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestMalformedWire(t *testing.T) {
	h, _ := testHandler(t)
	base := "/o/r/api/pulls/7"
	for _, tc := range []struct {
		name         string
		method, path string
		token, body  string
		want         int
	}{
		{"unreadable body", "POST", base + "/reviews", "bob", "\x00unterminated",
			// valid JSON NUL-prefixed is a parse error, still 400
			400},
		{"invalid JSON", "POST", base + "/reviews", "bob", "{", 400},
		{"non-object", "POST", base + "/reviews", "bob", "[1]", 400},
		{"wrong shape", "POST", base + "/reviews", "bob", `{"threads":"x"}`, 400},
		{"oversize body", "POST", base + "/reviews", "bob", `{"state":"` + strings.Repeat("A", 2<<20) + `"}`, 400},
		{"huge seq", "GET", "/o/r/api/pulls/7/reviews/99999999999", "carol", "", 404},
		{"empty seq", "GET", "/o/r/api/pulls/99999999/reviews", "carol", "", 404},
		{"over-range num", "GET", "/o/r/api/pulls/16777216/reviews", "carol", "", 404},
		{"bad lane tail", "GET", "/o/r/api/pulls/7/reviews/1/extra/deep", "bob", "", 404},
		{"bad thread tail", "GET", "/o/r/api/pulls/7/threads/00000001/extra/deep", "carol", "", 404},
		{"unknown thread op", "POST", "/o/r/api/pulls/7/threads/00000001/fold", "bob", "", 404},
		{"bad n ignored", "GET", base + "/reviews?n=bogus", "carol", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doReq(h, tc.method, tc.path, tc.token, tc.body)
			if w.Code != tc.want {
				t.Fatalf("= %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
	// Undecodable path segments survive verbatim and fail closed
	// downstream (direct unit pin — the net/http server rejects such
	// request-targets before the handler ever runs).
	if got := decodeSegment("%ZZ"); got != "%ZZ" {
		t.Fatalf("got %q", got)
	}
	if got := decodeSegment("pulls"); got != "pulls" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteErrArms(t *testing.T) {
	h, _ := testHandler(t)
	_ = h
	for _, tc := range []struct {
		err  error
		want int
		head string
	}{
		{&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, 403, ""},
		{&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, 503, "15"},
		{&auth.AuthError{Kind: auth.ErrUnauthorized, Why: "who"}, 401, ""},
		{ErrUnavailable, 503, "15"},
	} {
		w := httptest.NewRecorder()
		writeErr(w, tc.err)
		if w.Code != tc.want {
			t.Fatalf("%v: code %d want %d", tc.err, w.Code, tc.want)
		}
		if tc.head != "" && w.Header().Get("Retry-After") != tc.head {
			t.Fatalf("retry %q", w.Header().Get("Retry-After"))
		}
	}
}

func TestCorruptObjects(t *testing.T) {
	ctx := context.Background()
	t.Run("corrupt header/sidecar fail closed", func(t *testing.T) {
		svc, _ := testSvc()
		put(t, svc, ThreadKey(testOwner, testRepo, testPR), []byte("{"), store.PutCreate, "")
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			submitAs(svc, "bob", StateApproved, testHead)); statusFor(err) != 500 {
			t.Fatalf("err=%v", err)
		}
		svc2, _ := testSvc()
		seedPR(t, svc2)
		put(t, svc2, PRKey(testOwner, testRepo, testPR), []byte("{"), store.PutUpdate, prVersion(t, svc2))
		if _, _, _, err := svc2.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			submitAs(svc2, "bob", StateApproved, testHead)); statusFor(err) != 500 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("issue-kind thread is not a PR", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		raw := get(t, svc, ThreadKey(testOwner, testRepo, testPR))
		h, _ := parsePRHeader(raw)
		h.Kind = "issue"
		_, meta, _ := store.GetBytes(ctx, svc.Store, ThreadKey(testOwner, testRepo, testPR), store.GetOptions{})
		put(t, svc, ThreadKey(testOwner, testRepo, testPR), encodePRHeader(h), store.PutUpdate, meta.Version)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			submitAs(svc, "bob", StateApproved, testHead)); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missing sidecar is 404", func(t *testing.T) {
		svc, _ := testSvc()
		now := "2026-09-04T12:00:00Z"
		h := &PRHeader{Num: testPR, Kind: "pr", Title: "t", State: "open", Author: "alice",
			CreatedAt: now, UpdatedAt: now, Version: 1}
		put(t, svc, ThreadKey(testOwner, testRepo, testPR), encodePRHeader(h), store.PutCreate, "")
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			submitAs(svc, "bob", StateApproved, testHead)); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("corrupt access.json fails suggest closed", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		put(t, svc, AccessKey(testOwner, testRepo), []byte("{"), store.PutCreate, "")
		if _, err := svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), ""); statusFor(err) != 500 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("corrupt requests fail closed", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		put(t, svc, ReviewRequestsKey(testOwner, testRepo, testPR), []byte("{"), store.PutCreate, "")
		if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); statusFor(err) != 500 {
			t.Fatalf("err=%v", err)
		}
	})
}

func prVersion(t *testing.T, svc *Service) store.Version {
	t.Helper()
	_, meta, err := store.GetBytes(context.Background(), svc.Store, PRKey(testOwner, testRepo, testPR), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return meta.Version
}

func TestCreateCollisions(t *testing.T) {
	ctx := context.Background()
	t.Run("review seq taken → 409", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		ev := &ReviewEvent{Kind: KindReview, Seq: 1, At: "2026-09-04T12:00:00Z", By: "mallory", State: StateApproved, CommitSHA: testHead}
		put(t, svc, ReviewKey(testOwner, testRepo, testPR, 1), encodeReview(ev), store.PutCreate, "")
		_, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			submitAs(svc, "bob", StateApproved, testHead))
		if statusFor(err) != 409 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("thread tid taken → 409", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		th := &ThreadHeader{TID: "00000001", Num: testPR, Kind: "review_thread", Version: 1}
		put(t, svc, ReviewThreadKey(testOwner, testRepo, testPR, "00000001"), encodeThreadHeader(th), store.PutCreate, "")
		_, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "x")
		if statusFor(err) != 409 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("comment seq taken retries to the next seq", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		// Squat the next seq: the two-step must retry and land seq 3.
		squat := &ThreadComment{Kind: KindReviewThreadComment, Seq: 2, At: "x", By: "mallory", Body: "squat"}
		put(t, svc, ReviewThreadEventKey(testOwner, testRepo, testPR, th.TID, 2), encodeThreadComment(squat), store.PutCreate, "")
		c, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("bob"), "two")
		if err != nil {
			t.Fatal(err)
		}
		if c.Seq != 3 {
			t.Fatalf("seq %d", c.Seq)
		}
	})
}

func TestConcurrentSubmits(t *testing.T) {
	ctx := context.Background()
	svc, roles := testSvc()
	roles.Public = true // rev00..rev15 carry no bindings; the race is on the CAS, not the gate
	seedPR(t, svc)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			who := testPrincipal(fmt.Sprintf("rev%02d", i))
			// Bounded client retry: the reservation CAS is last-writer-
			// wins with loser-retry, so a burst of N submitters needs
			// client retries to converge (production contention is
			// human-rate; the loop bound is per-attempt, not per-burst).
			var err error
			for attempt := 0; attempt < 10; attempt++ {
				_, _, _, err = svc.SubmitReview(ctx, testOwner, testRepo, testPR, who,
					submitAs(svc, who.Name, StateApproved, testHead))
				if err == nil {
					return
				}
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	seqs := map[int]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	res, err := svc.ListReviews(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range res.Reviews {
		if seqs[ev.Seq] {
			t.Fatalf("dup seq %d", ev.Seq)
		}
		seqs[ev.Seq] = true
	}
	if len(seqs) != n {
		t.Fatalf("got %d reviews, want %d", len(seqs), n)
	}
	sum, err := svc.refreshSummary(ctx, testOwner, testRepo, testPR)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Approvals != n || sum.Decision != DecisionApproved {
		t.Fatalf("%+v", sum)
	}
}

func TestRoleLadder(t *testing.T) {
	if roleRank("maintain") <= roleRank("write") || roleRank("bogus") != 0 {
		t.Fatalf("ladder broken")
	}
	if roleRank("ADMIN") != roleRank("admin") {
		t.Fatalf("case-insensitive ladder broken")
	}
	_ = identity.RoleRead // the ladder speaks identity roles
}

func TestBodyLimits(t *testing.T) {
	if err := validateBody(strings.Repeat("x", MaxBodyBytes+1)); statusFor(err) != 400 {
		t.Fatalf("err=%v", err)
	}
	if err := validateContextSHA(strings.Repeat("a", 63)); err == nil {
		t.Fatalf("short hash accepted")
	}
}

func TestRequireAuthn(t *testing.T) {
	if err := requireAuthenticated(auth.Anonymous()); statusFor(err) != 401 {
		t.Fatalf("err=%v", err)
	}
}

func TestMixedPrincipalCase(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	// "BOB" normalizes to the stored request entry and retires it.
	if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"BOB"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
		submitAs(svc, "bob", StateApproved, testHead)); err != nil {
		t.Fatal(err)
	}
	reqs, err := svc.GetRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"))
	if err != nil || len(reqs.Reviewers) != 0 {
		t.Fatalf("%+v %v", reqs, err)
	}
}
