package review

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestOpenThread(t *testing.T) {
	ctx := context.Background()
	t.Run("happy path allocates tid 1 with first comment", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "look here")
		if err != nil {
			t.Fatal(err)
		}
		if th.TID != "00000001" || th.CommentCount != 1 || th.NextEventSeq != 2 || th.Resolved {
			t.Fatalf("%+v", th)
		}
		view, err := svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Comments) != 1 || view.Comments[0].Body != "look here" {
			t.Fatalf("%+v", view)
		}
	})
	t.Run("second open allocates tid 2 (shared counter with submits)", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"),
			SubmitInput{State: StateCommented, CommitSHA: testHead, Threads: []NewThread{{Anchor: testAnchor(), Body: "a"}}}); err != nil {
			t.Fatal(err)
		}
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "b")
		if err != nil {
			t.Fatal(err)
		}
		if th.TID != "00000002" {
			t.Fatalf("tid %s", th.TID)
		}
	})
	t.Run("rejections", func(t *testing.T) {
		bad := testAnchor()
		bad.Side = "NOPE"
		for _, tc := range []struct {
			name   string
			who    auth.Principal
			anchor Anchor
			body   string
			seed   bool
			want   int
		}{
			{"anonymous", auth.Anonymous(), testAnchor(), "x", true, 401},
			{"bad anchor", testPrincipal("carol"), bad, "x", true, 400},
			{"empty body", testPrincipal("carol"), testAnchor(), " ", true, 400},
			{"unknown PR", testPrincipal("carol"), testAnchor(), "x", false, 404},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc, _ := testSvc()
				if tc.seed {
					seedPR(t, svc)
				}
				_, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, tc.who, tc.anchor, tc.body)
				if err == nil || statusFor(err) != tc.want {
					t.Fatalf("err=%v want %d", err, tc.want)
				}
			})
		}
	})
}

func TestThreadComments(t *testing.T) {
	ctx := context.Background()
	t.Run("comment appends via two-step, newest-first windows", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		c, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("bob"), "two")
		if err != nil {
			t.Fatal(err)
		}
		if c.Seq != 2 || c.By != "bob" {
			t.Fatalf("%+v", c)
		}
		view, err := svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Comments) != 1 || !view.More || view.Comments[0].Seq != 2 {
			t.Fatalf("%+v", view)
		}
		view, err = svc.GetThread(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), 2, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Comments) != 1 || view.Comments[0].Seq != 1 || view.More {
			t.Fatalf("%+v", view)
		}
	})
	t.Run("rejections", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			tid  string
			who  auth.Principal
			body string
			want int
		}{
			{"bad tid", "zzz", testPrincipal("bob"), "x", 404},
			{"unknown tid", "00000009", testPrincipal("bob"), "x", 404},
			{"anonymous", "00000001", auth.Anonymous(), "x", 401},
			{"empty", "00000001", testPrincipal("bob"), " ", 400},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc, _ := testSvc()
				seedPR(t, svc)
				if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one"); err != nil {
					t.Fatal(err)
				}
				_, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, tc.tid, tc.who, tc.body)
				if err == nil || statusFor(err) != tc.want {
					t.Fatalf("err=%v want %d", err, tc.want)
				}
			})
		}
	})
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Service, string) {
		t.Helper()
		svc, _ := testSvc()
		seedPR(t, svc)
		th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), testAnchor(), "one")
		if err != nil {
			t.Fatal(err)
		}
		return svc, th.TID
	}
	t.Run("opener resolves and unresolves", func(t *testing.T) {
		svc, tid := setup(t)
		th, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("carol"))
		if err != nil {
			t.Fatal(err)
		}
		if !th.Resolved || th.ResolvedBy != "carol" || th.ResolvedAt == "" {
			t.Fatalf("%+v", th)
		}
		th, err = svc.UnresolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("carol"))
		if err != nil {
			t.Fatal(err)
		}
		if th.Resolved || th.ResolvedBy != "" || th.ResolvedAt != "" {
			t.Fatalf("%+v", th)
		}
	})
	t.Run("participant resolves; stranger 403; triage resolves", func(t *testing.T) {
		svc, tid := setup(t)
		if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, tid, testPrincipal("bob"), "ack"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("bob")); err != nil {
			t.Fatalf("participant: %v", err)
		}
		if _, err := svc.UnresolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("bob")); err != nil {
			t.Fatal(err)
		}
		// carol has read only... carol is read; use a fresh stranger: zed has no binding → read? FakeRoles default read.
		if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("zed")); statusFor(err) != 403 {
			t.Fatalf("stranger err=%v", err)
		}
		if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("dave")); err != nil {
			t.Fatalf("triage: %v", err)
		}
	})
	t.Run("unknown tid 404", func(t *testing.T) {
		svc, _ := setup(t)
		if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, "00000009", testPrincipal("dave")); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("list filter resolved", func(t *testing.T) {
		svc, tid := setup(t)
		if _, err := svc.ResolveThread(ctx, testOwner, testRepo, testPR, tid, testPrincipal("dave")); err != nil {
			t.Fatal(err)
		}
		yes := true
		res, err := svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), &yes, "", 50)
		if err != nil || len(res.Threads) != 1 || res.More {
			t.Fatalf("%+v %v", res, err)
		}
		no := false
		res, err = svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), &no, "", 50)
		if err != nil || len(res.Threads) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		res, err = svc.ListThreads(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), nil, "", 1)
		if err != nil || len(res.Threads) != 1 || res.More {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

func TestReviewRequests(t *testing.T) {
	ctx := context.Background()
	t.Run("add dedups, remove filters, empty doc reads []", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		got, err := svc.GetRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"))
		if err != nil || len(got.Reviewers) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
		reqs, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob", "carol", "bob"})
		if err != nil {
			t.Fatal(err)
		}
		if len(reqs.Reviewers) != 2 || reqs.Reviewers[0].By != "alice" {
			t.Fatalf("%+v", reqs)
		}
		// Re-request is a no-op (same content, still converges).
		reqs, err = svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"})
		if err != nil || len(reqs.Reviewers) != 2 {
			t.Fatalf("%+v %v", reqs, err)
		}
		reqs, err = svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"})
		if err != nil || len(reqs.Reviewers) != 1 || reqs.Reviewers[0].Principal != "carol" {
			t.Fatalf("%+v %v", reqs, err)
		}
	})
	t.Run("self-removal allowed for the requested", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"carol"}); err != nil {
			t.Fatal(err)
		}
		reqs, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), []string{"carol"})
		if err != nil || len(reqs.Reviewers) != 0 {
			t.Fatalf("%+v %v", reqs, err)
		}
	})
	t.Run("auth: non-author non-triage cannot add/remove others", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), []string{"bob"}); statusFor(err) != 403 {
			t.Fatalf("add err=%v", err)
		}
		if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("dave"), []string{"bob"}); err != nil {
			t.Fatalf("triage add: %v", err)
		}
		if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), []string{"bob"}); statusFor(err) != 403 {
			t.Fatalf("remove err=%v", err)
		}
		if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{}); statusFor(err) != 400 {
			t.Fatalf("empty err=%v", err)
		}
		if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{" "}); statusFor(err) != 400 {
			t.Fatalf("blank err=%v", err)
		}
	})
}

type fakeExpander struct{ members map[string][]string }

func (f *fakeExpander) ExpandGroups(_ context.Context, members []string) ([]string, []string) {
	var out []string
	for _, m := range members {
		out = append(out, f.members[m]...)
	}
	return out, nil
}

type fakeAuthors struct{ authors []string }

func (f *fakeAuthors) HeadAuthors(_ context.Context, _, _ string, _, _ int) ([]string, error) {
	return f.authors, nil
}

func TestSuggest(t *testing.T) {
	ctx := context.Background()
	seedAccess := func(t *testing.T, svc *Service) {
		t.Helper()
		put(t, svc, AccessKey(testOwner, testRepo), []byte(`{"version":1,"visibility":"private","role_bindings":[
			{"subject":"user:bob","role":"maintain"},
			{"subject":"user:carol","role":"read"},
			{"subject":"user:zed","role":"read"},
			{"subject":"team:o/devs","role":"triage"},
			{"subject":"user:low","role":"none"}]}`), store.PutCreate, "")
	}
	t.Run("merges bindings, teams, authors in order with q filter", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		seedAccess(t, svc)
		svc.Expander = &fakeExpander{members: map[string][]string{"team:o/devs": {"dave", "bob"}}}
		svc.Authors = &fakeAuthors{authors: []string{"mallory", "bob"}}
		out, err := svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"bob", "carol", "zed", "dave", "mallory"}
		if len(out) != len(want) {
			t.Fatalf("got %v want %v", out, want)
		}
		for i := range want {
			if out[i] != want[i] {
				t.Fatalf("got %v want %v", out, want)
			}
		}
		out, err = svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "b")
		if err != nil || len(out) != 1 || out[0] != "bob" {
			t.Fatalf("q=b: %v %v", out, err)
		}
		out, err = svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "zzz")
		if err != nil || len(out) != 0 {
			t.Fatalf("q=zzz: %v %v", out, err)
		}
	})
	t.Run("nil seams degrade to bindings; missing access degrades to []", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		out, err := svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "")
		if err != nil || len(out) != 0 {
			t.Fatalf("%v %v", out, err)
		}
		seedAccess(t, svc)
		out, err = svc.Suggest(ctx, testOwner, testRepo, testPR, testPrincipal("carol"), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 3 || out[0] != "bob" {
			t.Fatalf("%v", out)
		}
	})
	t.Run("anonymous denied; unknown PR 404", func(t *testing.T) {
		svc, _ := testSvc()
		seedPR(t, svc)
		if _, err := svc.Suggest(ctx, testOwner, testRepo, testPR, auth.Anonymous(), ""); statusFor(err) != 401 {
			t.Fatalf("err=%v", err)
		}
		if _, err := svc.Suggest(ctx, testOwner, testRepo, 999, testPrincipal("carol"), ""); statusFor(err) != 404 {
			t.Fatalf("err=%v", err)
		}
	})
}
