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

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestCover5IndexRetry(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1)
	// getJSON error ⇒ silent drop (best-effort index, never a 500).
	e.failGet(IndexKey("o", "r"), errors.New("down"))
	e.svc.updateIndex(ctx(), "o", "r", prCardOf(th))
	// Corrupt index ⇒ silent drop.
	e.clearFails()
	_, _ = store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{oops"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	_ = e.store.Delete(ctx(), IndexKey("o", "r"), "")
	_, _ = store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{oops"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	e.svc.updateIndex(ctx(), "o", "r", prCardOf(th))
	_ = e.store.Delete(ctx(), IndexKey("o", "r"), "")
	// Single 412 ⇒ retry converges.
	e.failPut(IndexKey("o", "r"), 1)
	e.svc.updateIndex(ctx(), "o", "r", prCardOf(th))
	e.clearFails()
	ix, _, _ := e.svc.loadIndex(ctx(), "o", "r")
	if len(ix.Open) != 1 {
		t.Fatalf("retry: %+v", ix)
	}
	// Persistent 412 ⇒ bounded drop (10 attempts, then proceed).
	e.failPut(IndexKey("o", "r"), 99)
	e.svc.updateIndex(ctx(), "o", "r", prCardOf(th))
	e.clearFails()
	// Nil-index upsert (drop-to-empty normalization).
	upsertCard(&Index{}, Card{Num: 9, Kind: "pr", Title: "x", State: "open", UpdatedAt: "2026-09-04T12:00:00Z"})
}

func TestCover5AppendEventCreate(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Reserved-seq 412 (pre-seeded next event) ⇒ retry reserves a new seq;
	// the gap is harmless (P3).
	_, _ = store.PutBytes(ctx(), e.store, EventKey("o", "r", 1, 1), encodeEvent(&Event{Seq: 1}),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	ev, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "after gap")
	if err != nil || ev.Seq != 2 {
		t.Fatalf("gap retry: %+v %v", ev, err)
	}
	// getJSON outage.
	e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
	if _, _, err := e.svc.appendEvent(ctx(), "o", "r", 1, func(t *Thread, seq int) (*Event, error) {
		return &Event{}, nil
	}); err == nil {
		t.Fatal("get err must surface")
	}
	e.clearFails()
	// Non-412 event-write outage ⇒ error (the reserved seq is skipped, P3).
	e.failPutErr(EventKey("o", "r", 1, 3), errors.New("disk down"))
	if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "lost"); err == nil {
		t.Fatal("put err must surface")
	}
	e.clearFails()
	// Comment on an issue-kind thread ⇒ 404 (03 owns pr timelines).
	raw, ver, _ := e.svc.getJSON(ctx(), ThreadKey("o", "r", 1))
	s := strings.Replace(string(raw), `"kind":"pr"`, `"kind":"issue"`, 1)
	_, _ = store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 1), []byte(s), store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
	if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kind: %v", err)
	}
	// Oversize body.
	if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), strings.Repeat("b", MaxBodyBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("big: %v", err)
	}
}

func TestCover5SavePRBranches(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Create path (ver "") on a missing key.
	pr := &PRDoc{Num: 9, Kind: "pr"}
	if err := e.svc.savePR(ctx(), "o", "r", pr, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Generic outage surfaces.
	e.failPutErr(PRKey("o", "r", 9), errors.New("bad put"))
	if err := e.svc.savePR(ctx(), "o", "r", &PRDoc{Num: 9}, "v"); err == nil {
		t.Fatal("put err must surface")
	}
	e.clearFails()
	// Stale version on a missing key ⇒ conflict (reload finds nothing).
	if err := e.svc.savePR(ctx(), "o", "r", &PRDoc{Num: 77}, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
}

func TestCover5ListBranches(t *testing.T) {
	t.Run("clamp + empty", func(t *testing.T) {
		e := newTestEnv()
		res, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{N: 500})
		if err != nil || res.Pulls == nil || res.More {
			t.Fatalf("empty: %+v %v", res, err)
		}
	})
	t.Run("index outage", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.failGet(IndexKey("o", "r"), errors.New("down"))
		if _, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{}); err == nil {
			t.Fatal("index err must surface")
		}
		e.clearFails()
	})
	t.Run("ghost + merged skips", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		ix, ver, _ := e.svc.loadIndex(ctx(), "o", "r")
		ix.Open = append(ix.Open,
			Card{Num: 77, Kind: "pr", Title: "ghost", State: "open"},
			Card{Num: 78, Kind: "issue", Title: "other", State: "open"})
		raw, _ := json.Marshal(ix)
		if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), raw, store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
			t.Fatalf("ghost write: %v", err)
		}
		back, _, _ := e.svc.loadIndex(ctx(), "o", "r")
		if len(back.Open) != 3 {
			t.Fatalf("ghosts missing: %+v", back)
		}
		res, err := e.svc.ListPRs(ctx(), "o", "r", writer(), ListFilter{})
		if err != nil || len(res.Pulls) != 1 {
			t.Fatalf("ghost: %+v %v", res, err)
		}
		pr, prVer, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		pr.Merged = true
		_ = e.svc.savePR(ctx(), "o", "r", pr, prVer)
		if n, err := e.svc.findOpenPair(ctx(), "o", "r", "refs/heads/main", "refs/heads/topic", "o/r"); err != nil || n != 0 {
			t.Fatalf("merged pair = %d %v", n, err)
		}
	})
}

func TestCover5GetHeadDirErr(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
	_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Fork dir down ⇒ unknown (degrades, never a 500 for the reader).
	e.svc.Dirs = &failForkDirs{ok: map[string]bool{"o/r": true}}
	view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil || view.Mergeable.State != MergeableUnknown {
		t.Fatalf("fork down: %+v %v", view, err)
	}
	e.svc.Dirs = &FakeDirs{}
}

func TestCover5OpenVariants(t *testing.T) {
	t.Run("no publisher skips publish", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		e.svc.Refs = nil
		_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, "")
		if err != nil || pr.HeadPublished {
			t.Fatalf("unpublished: %+v %v", pr, err)
		}
	})
	t.Run("reachable cross-fork publishes in base", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if err != nil || !pr.HeadPublished || pr.Fork == nil {
			t.Fatalf("fork publish: %+v %v", pr, err)
		}
		found := false
		for _, c := range e.refs.Calls {
			if c.Op == "create" && c.Repo == "o/r" && c.Ref == "refs/pull/1/head" && c.New == hexSHA(9) {
				found = true
			}
		}
		if !found {
			t.Fatalf("calls = %+v", e.refs.Calls)
		}
	})
}

func TestCover5MergeMore(t *testing.T) {
	t.Run("loadThread outage fails fast", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["merger@example.com"] = "maintain"
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, ""); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
	})
	t.Run("update-branch publish outage", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.git.MergeBaseSHA = hexSHA(7)
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(5), "refs/heads/topic": hexSHA(2)})
		e.refs.Refs["o/r"] = map[string]string{"refs/heads/topic": hexSHA(2)}
		e.refs.UpdateErr = errors.New("wal down")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("publish: %v", err)
		}
	})
	t.Run("delete-head requireRead fails", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Public = false
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		// No binding at all ⇒ require_read 403 (before any role check).
		if err := e.svc.DeleteHead(ctx(), "o", "r", 1, auth.Principal{Name: "ghost@x"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("read: %v", err)
		}
	})
	t.Run("compute drift refreshes inside task", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.seedRefs("o/r", map[string]string{
			"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(3),
			"refs/pull/1/head": hexSHA(2),
		})
		e.git.MergeBaseSHA = hexSHA(7)
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
		m, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1)
		if err != nil || m.HeadSHA != hexSHA(3) {
			t.Fatalf("drift: %+v %v", m, err)
		}
	})
	t.Run("refreshHead empty is a no-op", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1)
		e.svc.refreshHead(ctx(), "o", "r", pr, th, "", writer())
		e.svc.refreshHead(ctx(), "o", "r", pr, th, pr.Head.SHA, writer())
	})
	t.Run("error join shares the outcome", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.seedRefs("o/r", map[string]string{
			"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2),
			"refs/pull/1/head": hexSHA(2),
		})
		e.git.MergeBaseErr = errors.New("no base")
		const N = 4
		done := make(chan error, N)
		for i := 0; i < N; i++ {
			go func() {
				_, err := e.svc.ComputeMergeable(context.Background(), "o", "r", 1)
				done <- err
			}()
		}
		for i := 0; i < N; i++ {
			if err := <-done; err == nil {
				t.Fatal("joiners must share the leader error")
			}
		}
	})
}

func TestCover5Misc(t *testing.T) {
	t.Run("roleRank ladder", func(t *testing.T) {
		for role, want := range map[identity.Role]int{
			identity.RoleRead: 1, identity.RoleTriage: 2, identity.RoleWrite: 3,
			identity.RoleMaintain: 4, identity.RoleAdmin: 5,
		} {
			if got := roleRank(string(role)); got != want {
				t.Fatalf("rank(%s) = %d", role, got)
			}
		}
	})
	t.Run("mergeMessage rebase empty subject", func(t *testing.T) {
		title, _ := mergeMessage(StrategyRebase, 9, "refs/heads/f", "", "body", "", "")
		if title != "Merge pull request #9 from f" {
			t.Fatalf("title = %q", title)
		}
		title, _ = mergeMessage("bogus", 9, "refs/heads/f", "s", "", "", "")
		if title != "Merge pull request #9 from f" {
			t.Fatalf("default = %q", title)
		}
	})
	t.Run("commits clamps", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
		e.git.LogRows = []CommitEntry{{SHA: hexSHA(2), Subject: "s"}}
		rows, _, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), -3, 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("clamp: %+v %v", rows, err)
		}
		if _, _, err := e.svc.Commits(ctx(), "o", "r", 1, writer(), 0, 500); err != nil {
			t.Fatalf("clamp500: %v", err)
		}
	})
	t.Run("diff load error", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.Diff(ctx(), "o", "r", 1, writer()); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("merge-base garbage is unprocessable", func(t *testing.T) {
		bin := fakeGitScript(t, "echo garbage; exit 0")
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.MergeBase(context.Background(), t.TempDir(), "a", "b"); !errors.Is(err, ErrUnprocessable) {
			t.Fatalf("mb: %v", err)
		}
	})
	t.Run("replay merge-base outage", func(t *testing.T) {
		bin := fakeGitScript(t, "exit 1")
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.Replay(context.Background(), t.TempDir(), "o", "b", "h"); err == nil {
			t.Fatal("replay mb err must surface")
		}
	})
	t.Run("comments unknown field", func(t *testing.T) {
		e := newTestEnv()
		seedOpened(t, e)
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
		req := httptest.NewRequest("POST", "/o/r/api/pulls/1/comments", strings.NewReader(`{"body":"x","zzz":1}`))
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		if w.Code != 400 {
			t.Fatalf("unknown = %d", w.Code)
		}
	})
	t.Run("commits clamp over http", func(t *testing.T) {
		e := newTestEnv()
		seedOpened(t, e)
		e.git.LogRows = []CommitEntry{{SHA: hexSHA(2), Subject: "s"}}
		e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }
		req := httptest.NewRequest("GET", "/o/r/api/pulls/1/commits?n=500", nil)
		w := httptest.NewRecorder()
		e.h.Handle(w, req)
		if w.Code != 200 {
			t.Fatalf("clamp = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("head dirs nil is false", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		e.svc.Dirs = nil
		if e.svc.headRefMatches(ctx(), pr, hexSHA(2)) {
			t.Fatal("nil dirs must be false")
		}
	})
}
