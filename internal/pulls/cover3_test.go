package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestCover3IsCASConflict(t *testing.T) {
	if !isCASConflict(errors.New("CAS conflict: x moved")) {
		t.Fatal("cas")
	}
	if !isCASConflict(errors.New("precondition failed")) {
		t.Fatal("precondition")
	}
	if !isCASConflict(errors.New("changed concurrently")) {
		t.Fatal("concurrent")
	}
	if isCASConflict(nil) || isCASConflict(errors.New("wal down")) {
		t.Fatal("negative")
	}
}

func TestCover3RoleRank(t *testing.T) {
	if roleRank("bogus") != 0 || roleRank("") != 0 {
		t.Fatal("unknown role ranks zero")
	}
}

func TestCover3CheckRequiredChecksDirect(t *testing.T) {
	e := newTestEnv()
	putPolicy(t, e, `{"version":1,"rules":[]}`)
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("b", 40), "refs/heads/main", "m"); err != nil {
		t.Fatalf("empty: %v", err)
	}
	_ = e.store.Delete(ctx(), PolicyKey("o", "r"), "")
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("b", 40), "refs/heads/main", "m"); err != nil {
		t.Fatalf("absent: %v", err)
	}
}

func TestCover3ValidateRepoPart(t *testing.T) {
	for _, bad := range []string{"", "..", ".hidden", strings.Repeat("a", 101), "has space", "semi;colon"} {
		if validateRepoPart(bad) == nil {
			t.Fatalf("bad %q accepted", bad)
		}
	}
	if validateRepoPart("good-name_1.x") != nil {
		t.Fatal("good rejected")
	}
}

func TestCover3FlightJoinCancel(t *testing.T) {
	g := newFlightGroup()
	release := make(chan struct{})
	go func() {
		_, _ = g.Do(context.Background(), "k", func() (any, error) {
			<-release
			return 1, nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Do(cctx, "k", func() (any, error) { return 2, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("join cancel: %v", err)
	}
	close(release)
}

func TestCover3ComputeBranches(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{
			"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2),
			"refs/pull/1/head": hexSHA(2),
		})
		return e
	}
	t.Run("dirs down", func(t *testing.T) {
		e := mk(t)
		e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("dirs: %v", err)
		}
	})
	t.Run("base gone", func(t *testing.T) {
		e := mk(t)
		e.delGitRef("o/r", "refs/heads/main")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("base: %v", err)
		}
	})
	t.Run("head gone serves unknown", func(t *testing.T) {
		e := mk(t)
		e.delGitRef("o/r", "refs/heads/topic")
		m, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1)
		if err != nil || m.State != MergeableUnknown {
			t.Fatalf("head: %+v %v", m, err)
		}
	})
	t.Run("ancestry outage", func(t *testing.T) {
		e := mk(t)
		e.git.IsAncestorErr = errors.New("git down")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("ancestry err must surface")
		}
	})
	t.Run("merge-base outage", func(t *testing.T) {
		e := mk(t)
		e.git.MergeBaseErr = errors.New("no base")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("merge-base err must surface")
		}
	})
	t.Run("behind outage", func(t *testing.T) {
		e := mk(t)
		e.git.BehindErr = errors.New("no count")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("behind err must surface")
		}
	})
	t.Run("trial outage", func(t *testing.T) {
		e := mk(t)
		e.git.TrialErr = errors.New("git down")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("trial err must surface")
		}
	})
	t.Run("mergeable load error", func(t *testing.T) {
		e := mk(t)
		e.failGet(MergeableKey("o", "r", 1), errors.New("down"))
		// GetPR falls back to unknown + enqueue on cache-read failure.
		view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_ = view
		e.clearFails()
	})
	t.Run("loadPolicy error", func(t *testing.T) {
		e := mk(t)
		e.failGet(PolicyKey("o", "r"), errors.New("down"))
		if err := e.svc.checkProtectedRef(ctx(), "o", "r", "m", "refs/heads/main", "update"); err == nil {
			t.Fatal("policy err must surface")
		}
		e.clearFails()
	})
	t.Run("loadPR/Thread store errors", func(t *testing.T) {
		e := mk(t)
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		e.failGet(IndexKey("o", "r"), errors.New("down"))
		if _, _, err := e.svc.loadThread(ctx(), "o", "r", 1); err == nil {
			t.Fatal("thread")
		}
		if _, _, err := e.svc.loadPR(ctx(), "o", "r", 1); err == nil {
			t.Fatal("pr")
		}
		if _, _, err := e.svc.loadIndex(ctx(), "o", "r"); err == nil {
			t.Fatal("index")
		}
		e.clearFails()
	})
}

func TestCover3EnsurePullHead(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	// Nil publisher ⇒ stored flag.
	e.svc.Refs = nil
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) != pr.HeadPublished {
		t.Fatal("nil refs")
	}
	e.svc.Refs = e.refs
	// Dirs failure ⇒ false.
	e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("dirs err must be false")
	}
	e.svc.Dirs = &FakeDirs{}
	// Missing ref + unreachable ⇒ false (no publish).
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	e.git.ReachableMap = map[string]bool{hexSHA(2): false}
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("unreachable must be false")
	}
	// Reachability outage ⇒ false.
	e.git.ReachableErr = errors.New("down")
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("reachable err must be false")
	}
	e.git.ReachableErr = nil
	e.git.ReachableMap = nil
	// Publish outage on the create path ⇒ false (pull ref absent from git).
	e.refs.CreateErr = errors.New("wal down")
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("create err must be false")
	}
	e.refs.CreateErr = nil
	// Drifted server ref is CAS-forwarded to the live head (never forced).
	e.seedRefs("o/r", map[string]string{"refs/pull/1/head": hexSHA(9)})
	e.refs.Refs["o/r"] = map[string]string{"refs/pull/1/head": hexSHA(9)}
	if !e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("drift must advance")
	}
	if got := e.refs.Refs["o/r"]["refs/pull/1/head"]; got != hexSHA(2) {
		t.Fatalf("pullhead = %s", got)
	}
	calls := e.refs.updatesFor("refs/pull/1/head")
	if len(calls) == 0 || calls[len(calls)-1].Old != hexSHA(9) {
		t.Fatalf("forward must carry old: %+v", calls)
	}
	// Drifted but unreachable ⇒ false (no advance).
	e.refs.Refs["o/r"] = map[string]string{"refs/pull/1/head": hexSHA(9)}
	e.git.ReachableMap = map[string]bool{hexSHA(2): false}
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("unreachable drift must be false")
	}
	e.git.ReachableMap = nil
	// Drifted but publish-conflicted ⇒ false.
	e.refs.UpdateConflictOnce = true
	if e.svc.ensurePullHead(ctx(), pr, hexSHA(2)) {
		t.Fatal("conflicted forward must be false")
	}
}

func TestCover3HeadRefMatchesBranches(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
	if e.svc.headRefMatches(ctx(), pr, hexSHA(2)) {
		t.Fatal("dirs err must be false")
	}
	e.svc.Dirs = &FakeDirs{}
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	if e.svc.headRefMatches(ctx(), pr, hexSHA(2)) {
		t.Fatal("missing pull ref must be false")
	}
	e.seedRefs("o/r", map[string]string{"refs/pull/1/head": hexSHA(9)})
	if e.svc.headRefMatches(ctx(), pr, hexSHA(2)) {
		t.Fatal("drifted pull ref must be false")
	}
	e.seedRefs("o/r", map[string]string{"refs/pull/1/head": hexSHA(2)})
	if !e.svc.headRefMatches(ctx(), pr, hexSHA(2)) {
		t.Fatal("matching pull ref must be true")
	}
	// Git nil ⇒ false.
	g := e.svc.Git
	e.svc.Git = nil
	if e.svc.headRefMatches(ctx(), pr, hexSHA(2)) {
		t.Fatal("nil git must be false")
	}
	e.svc.Git = g
}

func TestCover3DiffDirBranches(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
	if _, err := e.svc.diffDir(ctx(), pr); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("base dirs: %v", err)
	}
	e.svc.Dirs = &FakeDirs{}
}

func TestCover3FindOpenPairGhost(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Ghost cards in the index (sidecar absent, or non-pr kind) are
	// skipped, not fatal — both `continue` branches.
	ix, ver, _ := e.svc.loadIndex(ctx(), "o", "r")
	ix.Open = append(ix.Open,
		Card{Num: 77, Kind: "pr", Title: "ghost", State: "open"},
		Card{Num: 78, Kind: "issue", Title: "other", State: "open"})
	raw, _ := json.Marshal(ix)
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), raw, store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatalf("ghost write: %v", err)
	}
	if n, err := e.svc.findOpenPair(ctx(), "o", "r", "refs/heads/main", "refs/heads/topic", "o/r"); err != nil || n != 1 {
		t.Fatalf("pair = %d %v", n, err)
	}
	if n, err := e.svc.findOpenPair(ctx(), "o", "r", "refs/heads/main", "refs/heads/other", "o/r"); err != nil || n != 0 {
		t.Fatalf("pair = %d %v", n, err)
	}
}

func TestCover3DiffThreadFallback(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Sidecar gone but thread present (kind pr): Diff still 404s (sidecar
	// is the read authority for refs).
	_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
	if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("diff: %v", err)
	}
	// Thread rewritten to issue-kind: 404 via the thread check.
	raw, ver, _ := e.svc.getJSON(ctx(), ThreadKey("o", "r", 1))
	s := strings.Replace(string(raw), `"kind":"pr"`, `"kind":"issue"`, 1)
	_, _ = store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 1), []byte(s), store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
	if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("diff kind: %v", err)
	}
	if _, _, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commits kind: %v", err)
	}
}

func TestCover3MergeGenericPublishErr(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	e.refs.UpdateErr = errors.New("wal down")
	rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = rec
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskError || !strings.Contains(done.Error, "wal down") {
		t.Fatalf("task = %+v", done)
	}
}

func TestCover3MergeDriftedHead(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	// Head moved (fast-forward) between open and merge: the task refreshes
	// the snapshot, recomputes, and merges the live head.
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(3),
		"refs/pull/1/head": hexSHA(2),
	})
	e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
	rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = rec
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskOK {
		t.Fatalf("task = %+v", done)
	}
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	if pr.Head.SHA != hexSHA(3) {
		t.Fatalf("head = %s", pr.Head.SHA)
	}
}

func TestCover3UpdateBranchBranches(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(5), "refs/heads/topic": hexSHA(2)})
		e.refs.Refs["o/r"] = map[string]string{"refs/heads/topic": hexSHA(2)}
		return e
	}
	t.Run("expected sha match proceeds", func(t *testing.T) {
		e := mk(t)
		rec, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), hexSHA(2))
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		_ = rec
	})
	t.Run("unknown num", func(t *testing.T) {
		e := mk(t)
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 99, writer(), ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown: %v", err)
		}
	})
	t.Run("missing sidecar", func(t *testing.T) {
		e := mk(t)
		_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("sidecar: %v", err)
		}
	})
	t.Run("closed", func(t *testing.T) {
		e := mk(t)
		e.roles.Roles["tri@example.com"] = "triage"
		_, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "tri@example.com"}, PRPatch{State: strPtr("closed")})
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("closed: %v", err)
		}
	})
	t.Run("unwired", func(t *testing.T) {
		e := mk(t)
		e.svc.Git, e.svc.Dirs, e.svc.Refs = nil, nil, nil
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), ""); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unwired: %v", err)
		}
	})
	t.Run("ancestry outage", func(t *testing.T) {
		e := mk(t)
		e.git.IsAncestorErr = errors.New("down")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); err == nil {
			t.Fatal("ancestry err must surface")
		}
	})
	t.Run("dirs down", func(t *testing.T) {
		e := mk(t)
		e.svc.Dirs = &FakeDirs{UnknownErr: errors.New("down")}
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("dirs: %v", err)
		}
	})
	t.Run("missing pr", func(t *testing.T) {
		e := mk(t)
		_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("pr: %v", err)
		}
	})
}

func TestCover3DeleteHeadBranches(t *testing.T) {
	t.Run("private stranger forbidden", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Public = false
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.roles.Roles["s@example.com"] = "read"
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, auth.Principal{Name: "s@example.com"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("stranger: %v", err)
		}
	})
	t.Run("missing sidecar", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["merger@example.com"] = "maintain"
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, maintainer()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("sidecar: %v", err)
		}
	})
	t.Run("anon", func(t *testing.T) {
		e := newTestEnv()
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("anon: %v", err)
		}
	})
}

func TestCover3ForkBranches(t *testing.T) {
	t.Run("explicit names", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		rec, child, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{TargetOwner: "F", Name: "Custom"})
		if err != nil || child != "f/Custom" {
			t.Fatalf("child = %q %v", child, err)
		}
		_ = rec
	})
	t.Run("bad name", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "bad name"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name: %v", err)
		}
	})
	t.Run("runFork store errors", func(t *testing.T) {
		e := newTestEnv()
		rec := &TaskRecord{Progress: []string{}}
		e.failPutErr(ForkKey("f", "c"), errors.New("bad put"))
		if err := e.svc.runFork(ctx(), "o", "r", "f", "c", writer(), rec); err == nil {
			t.Fatal("put err must surface")
		}
		e.clearFails()
		e.clearFails()
		e.failPut(ForksKey("o", "r"), 99)
		if err := e.svc.runFork(ctx(), "o", "r", "f", "c2", writer(), rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("cas exhaust: %v", err)
		}
		e.clearFails()
	})
	t.Run("runFork already listed is idempotent", func(t *testing.T) {
		e := newTestEnv()
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", "f", "c", writer(), rec); err != nil {
			t.Fatalf("first: %v", err)
		}
		// Drop fork.json but keep the index row: re-run converges without
		// duplicating the row.
		_ = e.store.Delete(ctx(), ForkKey("f", "c"), "")
		if err := e.svc.runFork(ctx(), "o", "r", "f", "c", writer(), rec); err != nil {
			t.Fatalf("second: %v", err)
		}
		raw, _, _ := e.svc.getJSON(ctx(), ForksKey("o", "r"))
		if strings.Count(string(raw), "f/c") != 1 {
			t.Fatalf("dup row: %s", raw)
		}
	})
}

func TestCover3HTTPMore(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		seedOpened(t, e)
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
		return e
	}
	call := func(e *testEnv, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		return w
	}
	t.Run("null body", func(t *testing.T) {
		e := mk(t)
		if w := call(e, "POST", "/o/r/api/pulls", "null"); w.Code != 400 {
			t.Fatalf("null = %d", w.Code)
		}
	})
	t.Run("merge unwired 503", func(t *testing.T) {
		e := mk(t)
		e.roles.Roles["merger@example.com"] = "maintain"
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return maintainer(), nil }
		e.svc.Git = nil
		w := call(e, "POST", "/o/r/api/pulls/1/merge", `{"strategy":"merge"}`)
		if w.Code != 503 {
			t.Fatalf("unwired = %d (%s)", w.Code, w.Body.String())
		}
		if w.Header().Get("Retry-After") == "" {
			t.Fatal("503 retry-after")
		}
	})
	t.Run("update-branch closed 409", func(t *testing.T) {
		e := mk(t)
		if w := call(e, "PUT", "/o/r/api/pulls/1", `{"state":"closed"}`); w.Code != 200 {
			t.Fatalf("close = %d", w.Code)
		}
		if w := call(e, "POST", "/o/r/api/pulls/1/update-branch", `{}`); w.Code != 409 {
			t.Fatalf("ub closed = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("delete-head unmerged 409", func(t *testing.T) {
		e := mk(t)
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return maintainer(), nil }
		if w := call(e, "DELETE", "/o/r/api/pulls/1/head", ""); w.Code != 409 {
			t.Fatalf("head = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("open with fork shape", func(t *testing.T) {
		e := mk(t)
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		w := call(e, "POST", "/o/r/api/pulls", `{"title":"F","base_ref":"refs/heads/main","head_ref":"refs/heads/feat","fork":{"repo":"f/r"}}`)
		if w.Code != 201 || !strings.Contains(w.Body.String(), `"fork"`) {
			t.Fatalf("fork open = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("etag star matches", func(t *testing.T) {
		e := mk(t)
		req := httptest.NewRequest("GET", "/o/r/api/pulls/1", nil)
		req.Header.Set("If-None-Match", "*")
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		if w.Code != 304 {
			t.Fatalf("star = %d", w.Code)
		}
	})
	t.Run("anon private diff 401", func(t *testing.T) {
		e := mk(t)
		e.roles.Public = false
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return auth.Anonymous(), nil }
		req := httptest.NewRequest("GET", "/o/r/api/pulls/1/diff", nil)
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("anon diff = %d", w.Code)
		}
	})
	t.Run("stranger private list 403", func(t *testing.T) {
		e := mk(t)
		e.roles.Public = false
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return auth.Principal{Name: "stranger@x"}, nil
		}
		req := httptest.NewRequest("GET", "/o/r/api/pulls", nil)
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		if w.Code != 403 {
			t.Fatalf("stranger = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("merge-task private anon 401", func(t *testing.T) {
		e := mk(t)
		e.roles.Public = false
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return auth.Anonymous(), nil }
		req := httptest.NewRequest("GET", "/o/r/api/pulls/1/merge/task", nil)
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		if w.Code != 401 {
			t.Fatalf("task anon = %d", w.Code)
		}
	})
	t.Run("root never claims", func(t *testing.T) {
		e := mk(t)
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		if e.h.Handle(w, req) {
			t.Fatal("root claimed")
		}
	})
}
