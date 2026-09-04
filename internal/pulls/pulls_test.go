package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func ctx() context.Context { return context.Background() }

func writer() auth.Principal { return auth.Principal{Name: "jane@example.com"} }

func openBasic(t *testing.T, e *testEnv, owner, repo string) (*Thread, *PRDoc) {
	t.Helper()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs(owner+"/"+repo, map[string]string{
		"refs/heads/main":  hexSHA(1),
		"refs/heads/topic": hexSHA(2),
	})
	th, pr, err := e.svc.OpenPR(ctx(), owner, repo, writer(), OpenInput{
		Title: "Add feature", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic", Body: "fixes #3",
	}, "req-1")
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	return th, pr
}

func TestOpenPRSuccess(t *testing.T) {
	e := newTestEnv()
	th, pr := openBasic(t, e, "o", "r")
	if th.Num != 1 || th.Kind != "pr" || th.Title != "Add feature" || th.State != "open" {
		t.Fatalf("thread = %+v", th)
	}
	if pr.Num != 1 || pr.Kind != "pr" || pr.Base.SHA != hexSHA(1) || pr.Head.SHA != hexSHA(2) {
		t.Fatalf("pr = %+v", pr)
	}
	if !pr.HeadPublished {
		t.Fatal("head should be published")
	}
	if pr.Body != "fixes #3" {
		t.Fatalf("body = %q", pr.Body)
	}
	// refs/pull/1/head published through the WAL funnel with agent meta.
	found := false
	for _, c := range e.refs.Calls {
		if c.Op == "create" && c.Ref == "refs/pull/1/head" && c.New == hexSHA(2) && c.Meta["agent"] == "pulls" && c.Meta["principal"] == "jane@example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no pull-head publish: %+v", e.refs.Calls)
	}
	// thread.json carries NO PR fields (contract §2.1).
	raw, _, _ := e.svc.getJSON(ctx(), ThreadKey("o", "r", 1))
	for _, leak := range []string{`"base"`, `"head"`, `"merged"`, `"pr.json"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("thread.json leaks PR field %s: %s", leak, raw)
		}
	}
	// shared index carries the pr card.
	ix, _, _ := e.svc.loadIndex(ctx(), "o", "r")
	if len(ix.Open) != 1 || ix.Open[0].Kind != "pr" || ix.Open[0].Num != 1 {
		t.Fatalf("index = %+v", ix)
	}
	// stream + notify fan-out (P8).
	if st := e.streams(); len(st) == 0 || st[0].Action != "opened" || st[0].Name != "pull" {
		t.Fatalf("stream = %+v", st)
	}
}

func TestOpenPRTable(t *testing.T) {
	base, head := hexSHA(1), hexSHA(2)
	cases := []struct {
		name    string
		roles   map[string]string
		public  bool
		anon    bool
		setup   func(e *testEnv)
		input   OpenInput
		wantErr error
	}{
		{name: "anonymous rejected", anon: true, public: false, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, wantErr: ErrUnauthorized},
		{name: "read role cannot open", roles: map[string]string{"jane@example.com": "read"}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, wantErr: ErrForbidden},
		{name: "empty title", roles: map[string]string{"jane@example.com": "write"}, input: OpenInput{Title: " ", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, wantErr: ErrInvalid},
		{name: "bad base ref", roles: map[string]string{"jane@example.com": "write"}, input: OpenInput{Title: "x", BaseRef: "main", HeadRef: "refs/heads/topic"}, wantErr: ErrInvalid},
		{name: "unknown base", roles: map[string]string{"jane@example.com": "write"}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, wantErr: ErrUnprocessable},
		{name: "unknown head", roles: map[string]string{"jane@example.com": "write"}, setup: func(e *testEnv) {
			e.seedRefs("o/r", map[string]string{"refs/heads/main": base})
		}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/nope"}, wantErr: ErrUnprocessable},
		{name: "unreachable head", roles: map[string]string{"jane@example.com": "write"}, setup: func(e *testEnv) {
			e.seedRefs("o/r", map[string]string{"refs/heads/main": base, "refs/heads/topic": head})
			e.git.ReachableMap = map[string]bool{head: false}
		}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, wantErr: ErrUnprocessable},
		{name: "body too big", roles: map[string]string{"jane@example.com": "write"}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic", Body: strings.Repeat("b", MaxBodyBytes+1)}, wantErr: ErrInvalid},
		{name: "bad fork repo", roles: map[string]string{"jane@example.com": "write"}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/f", Fork: &ForkInfo{Repo: "nope"}}, wantErr: ErrInvalid},
		{name: "git unwired", roles: map[string]string{"jane@example.com": "write"}, setup: func(e *testEnv) {
			e.svc.Git, e.svc.Dirs = nil, nil
		}, input: OpenInput{Title: "x", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, wantErr: ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			e.roles.Roles = tc.roles
			e.roles.Public = tc.public
			if tc.setup != nil {
				tc.setup(e)
			}
			p := writer()
			if tc.anon {
				p = auth.Anonymous()
			}
			if _, _, err := e.svc.OpenPR(ctx(), "o", "r", p, tc.input, ""); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestOpenPRDuplicate409(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "again", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenPRCrossFork(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
	// Cross-fork head not reachable in base: no publish, fork-local record.
	e.git.ReachableMap = map[string]bool{hexSHA(9): false}
	_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{
		Title: "fork work", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"},
	}, "")
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Fork == nil || pr.Fork.Repo != "f/r" {
		t.Fatalf("fork = %+v", pr.Fork)
	}
	if pr.HeadPublished {
		t.Fatal("unreachable cross-fork head must not publish")
	}
	for _, c := range e.refs.Calls {
		if c.Ref == "refs/pull/1/head" {
			t.Fatalf("unexpected publish: %+v", c)
		}
	}
}

func TestOpenPRPublishFailureStill201(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	e.refs.CreateErr = errors.New("wal down")
	_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, "")
	if err != nil {
		t.Fatalf("open must 201 despite publish failure: %v", err)
	}
	if pr.HeadPublished {
		t.Fatal("publish failed: HeadPublished must be false")
	}
	// Recovery: GET re-publishes idempotently once the funnel heals.
	e.refs.CreateErr = nil
	view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if !view.HeadRefOk {
		t.Fatal("GET should have re-published refs/pull/1/head")
	}
}

func TestGetPRMergeableStates(t *testing.T) {
	setup := func(t *testing.T, state string, behind int) *testEnv {
		t.Helper()
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.seedRefs("o/r", map[string]string{
			"refs/heads/main":  hexSHA(1),
			"refs/heads/topic": hexSHA(2),
			"refs/pull/1/head": hexSHA(2),
		})
		e.git.MergeBaseSHA = hexSHA(7)
		switch state {
		case MergeableClean:
			e.git.Behind = behind
		case MergeableDirty:
			e.git.TrialErr = errDirty
			e.git.TrialConflicts = []string{"a.txt", "b.txt"}
		case MergeableUpToDate:
			// Head fully merged into base (head is an ancestor of
			// base): nothing to merge.
			e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(1)] = true
		}
		return e
	}
	// First fetch serves unknown (§4: stamp mismatch ⇒ unknown + enqueued
	// recompute); the background pass converges the cache; the second fetch
	// serves the stamp.
	fetchConverged := func(t *testing.T, e *testEnv) *PullView {
		t.Helper()
		first, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
		if err != nil {
			t.Fatalf("GetPR: %v", err)
		}
		if first.Mergeable.State != MergeableUnknown {
			t.Fatalf("first fetch must be unknown, got %+v", first.Mergeable)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
			if err != nil {
				t.Fatalf("GetPR: %v", err)
			}
			if view.Mergeable.State != MergeableUnknown {
				return view
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("mergeable never converged from unknown")
		return nil
	}
	t.Run("clean", func(t *testing.T) {
		e := setup(t, MergeableClean, 0)
		view := fetchConverged(t, e)
		if view.Mergeable.State != MergeableClean || view.Mergeable.MergeBase != hexSHA(7) {
			t.Fatalf("mergeable = %+v", view.Mergeable)
		}
		if !view.HeadRefOk || view.HeadLive != hexSHA(2) || view.BaseLive != hexSHA(1) {
			t.Fatalf("view = %+v", view)
		}
		// Second read serves the stamped cache (no new merge-tree run).
		calls := len(e.git.CallLog())
		view2, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
		if err != nil {
			t.Fatalf("GetPR: %v", err)
		}
		if view2.Mergeable.State != MergeableClean {
			t.Fatalf("cached = %+v", view2.Mergeable)
		}
		for _, c := range e.git.CallLog()[calls:] {
			if c == "merge-tree" {
				t.Fatal("stamped cache must avoid a second merge-tree")
			}
		}
	})
	t.Run("behind", func(t *testing.T) {
		e := setup(t, MergeableClean, 3)
		view := fetchConverged(t, e)
		if view.Mergeable.State != MergeableBehind {
			t.Fatalf("mergeable = %+v", view.Mergeable)
		}
	})
	t.Run("dirty with conflicts", func(t *testing.T) {
		e := setup(t, MergeableDirty, 0)
		view := fetchConverged(t, e)
		if view.Mergeable.State != MergeableDirty || len(view.Mergeable.Conflicts) != 2 {
			t.Fatalf("mergeable = %+v", view.Mergeable)
		}
	})
	t.Run("up_to_date", func(t *testing.T) {
		e := setup(t, MergeableUpToDate, 0)
		view := fetchConverged(t, e)
		if view.Mergeable.State != MergeableUpToDate {
			t.Fatalf("mergeable = %+v", view.Mergeable)
		}
	})
	t.Run("unknown PR and issue-kind isolation", func(t *testing.T) {
		e := setup(t, MergeableClean, 0)
		if _, err := e.svc.GetPR(ctx(), "o", "r", 999, writer(), 0, 0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
		// A kind:"issue" thread at the shared family is invisible to PR reads.
		raw, ver, _ := e.svc.getJSON(ctx(), ThreadKey("o", "r", 1))
		s := strings.Replace(string(raw), `"kind":"pr"`, `"kind":"issue"`, 1)
		if _, err := store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 1), []byte(s),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if _, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("issue-kind err = %v", err)
		}
	})
}

func TestGetPRHeadDrift(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Normal push to the head branch: sha refresh, no force event.
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(3),
		"refs/pull/1/head": hexSHA(2),
	})
	e.git.MergeBaseSHA = hexSHA(7)
	e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
	view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if view.PR.Head.SHA != hexSHA(3) {
		t.Fatalf("head sha = %s", view.PR.Head.SHA)
	}
	if view.PR.HeadForcePushedAt != nil {
		t.Fatal("fast-forward must not record force-push")
	}
	// Force-push: evidence event + stamp.
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(4),
		"refs/pull/1/head": hexSHA(3),
	})
	view, err = e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if view.PR.HeadForcePushedAt == nil {
		t.Fatal("force-push must stamp head_force_pushed_at")
	}
	found := false
	for _, s := range e.streams() {
		if s.Action == "head_force_pushed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no head_force_pushed stream: %+v", e.streams())
	}
}

func TestListPRs(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/a": hexSHA(2), "refs/heads/b": hexSHA(3)})
	for _, head := range []string{"refs/heads/a", "refs/heads/b"} {
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "pr " + head, BaseRef: "refs/heads/main", HeadRef: head}, "")
		if err != nil {
			t.Fatalf("OpenPR: %v", err)
		}
	}
	res, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{})
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(res.Pulls) != 2 || res.More {
		t.Fatalf("list = %+v", res)
	}
	if res.Pulls[0].HeadSHA != hexSHA(3) {
		t.Fatalf("newest first: %+v", res.Pulls)
	}
	byHead, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{Head: "refs/heads/a"})
	if err != nil || len(byHead.Pulls) != 1 {
		t.Fatalf("byHead = %+v %v", byHead, err)
	}
	byBaseNone, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{Base: "refs/heads/nope"})
	if err != nil || len(byBaseNone.Pulls) != 0 {
		t.Fatalf("byBaseNone = %+v %v", byBaseNone, err)
	}
	paged, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{N: 1})
	if err != nil || len(paged.Pulls) != 1 || !paged.More {
		t.Fatalf("paged = %+v %v", paged, err)
	}
	paged2, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{N: 1, After: paged.Pulls[0].Num})
	if err != nil || len(paged2.Pulls) != 1 || paged2.More {
		t.Fatalf("paged2 = %+v %v", paged2, err)
	}
	closed, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{State: "closed"})
	if err != nil || len(closed.Pulls) != 0 {
		t.Fatalf("closed = %+v %v", closed, err)
	}
	if _, err := e.svc.ListPRs(ctx(), "o", "r", auth.Anonymous(), ListFilter{}); e.roles.Public {
		if err != nil {
			t.Fatalf("public anon list: %v", err)
		}
	}
}

func TestUpdatePR(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Author edits title + body.
	th, pr, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("New title"), Body: strPtr("new body")})
	if err != nil {
		t.Fatalf("UpdatePR: %v", err)
	}
	if th.Title != "New title" || pr.Body != "new body" {
		t.Fatalf("th=%+v pr=%+v", th, pr)
	}
	// Stranger (read) cannot edit.
	e.roles.Roles["mallory@example.com"] = "read"
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "mallory@example.com"}, PRPatch{Title: strPtr("hijack")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger title edit err = %v", err)
	}
	// Triage may close others'.
	e.roles.Roles["tri@example.com"] = "triage"
	th, _, err = e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "tri@example.com"}, PRPatch{State: strPtr("closed")})
	if err != nil || th.State != "closed" {
		t.Fatalf("triage close: %+v %v", th, err)
	}
	// Reopen clears the close.
	th, _, err = e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{State: strPtr("open")})
	if err != nil || th.State != "open" {
		t.Fatalf("reopen: %+v %v", th, err)
	}
	// Bad state + unknown PR.
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{State: strPtr("merged")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad state err = %v", err)
	}
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 42, writer(), PRPatch{Title: strPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Anonymous(), PRPatch{Title: strPtr("x")}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon err = %v", err)
	}
}

func TestAddComment(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	ev, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "looks good")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if ev.Type != "commented" || ev.Seq != 1 {
		t.Fatalf("ev = %+v", ev)
	}
	if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty err = %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "o", "r", 9, writer(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "o", "r", 1, auth.Anonymous(), "x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon err = %v", err)
	}
}

func TestComputeMergeableClosed(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	e.roles.Roles["tri@example.com"] = "triage"
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "tri@example.com"}, PRPatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("close: %v", err)
	}
	m, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("ComputeMergeable: %v", err)
	}
	if m.State != MergeableUpToDate {
		t.Fatalf("m = %+v", m)
	}
	if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 77); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
	e.svc.Git = nil
	if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unwired err = %v", err)
	}
}

func TestMergeMessageTemplates(t *testing.T) {
	title, body := mergeMessage(StrategyMerge, 42, "refs/heads/topic", "subj", "b", "", "")
	if title != "Merge pull request #42 from topic" {
		t.Fatalf("title = %q", title)
	}
	if body != "subj\n\nb" {
		t.Fatalf("body = %q", body)
	}
	title, _ = mergeMessage(StrategySquash, 7, "refs/heads/f", "subj", "", "Custom", "msg")
	if title != "Custom" {
		t.Fatalf("override title = %q", title)
	}
	if fullMessage("t", "") != "t" || fullMessage("t", "b") != "t\n\nb" {
		t.Fatal("fullMessage")
	}
	if shortSHA(hexSHA(1)) != hexSHA(1)[:12] || shortSHA("abc") != "abc" {
		t.Fatal("shortSHA")
	}
	if strOrEmpty(nil) != "" || strOrEmpty(strPtr("x")) != "x" {
		t.Fatal("strOrEmpty")
	}
	if firstSubject(nil) != "" || firstSubject([]CommitEntry{{Subject: "s"}}) != "s" {
		t.Fatal("firstSubject")
	}
}

func TestProtectedRefGate(t *testing.T) {
	e := newTestEnv()
	// No policy.json ⇒ allow-all.
	if err := e.svc.checkProtectedRef(ctx(), "o", "r", "jane@example.com", "refs/heads/main", "update"); err != nil {
		t.Fatalf("allow-all: %v", err)
	}
	// Managed refs never pass the policy path (push pipeline owns them).
	if err := e.svc.checkProtectedRef(ctx(), "o", "r", "jane@example.com", "refs/pull/1/head", "create"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("managed err = %v", err)
	}
	// Protect rule denies.
	putPolicy(t, e, `{"version":1,"rules":[{"name":"no-direct-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}]}`)
	if err := e.svc.checkProtectedRef(ctx(), "o", "r", "jane@example.com", "refs/heads/main", "update"); err == nil || !strings.Contains(err.Error(), "no-direct-main") {
		t.Fatalf("deny err = %v", err)
	}
	// Unrelated ref still allowed.
	if err := e.svc.checkProtectedRef(ctx(), "o", "r", "jane@example.com", "refs/heads/topic", "update"); err != nil {
		t.Fatalf("topic: %v", err)
	}
	// Unparseable policy fails closed.
	putPolicyRaw(t, e, `{"version":1,"rules":[{"name":"x"]}`)
	if err := e.svc.checkProtectedRef(ctx(), "o", "r", "jane@example.com", "refs/heads/main", "update"); !errors.Is(err, ErrConflict) {
		t.Fatalf("corrupt err = %v", err)
	}
}

func putPolicy(t *testing.T, e *testEnv, doc string) {
	t.Helper()
	key := PolicyKey("o", "r")
	_, ver, _ := e.svc.getJSON(ctx(), key)
	opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
	if ver == "" {
		opts.Mode = store.PutCreate
	}
	if _, err := store.PutBytes(ctx(), e.store, key, []byte(doc), opts); err != nil {
		t.Fatalf("putPolicy: %v", err)
	}
}

func putPolicyRaw(t *testing.T, e *testEnv, doc string) {
	t.Helper()
	putPolicy(t, e, doc)
}
