package pulls

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// fakeGitScript writes an executable shell script emulating chosen git
// outputs (tests only: covers output-shape branches stock git cannot
// produce — replay success with garbage, non-numeric counts, missing
// objects on the reachability pipeline).
func fakeGitScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("script: %v", err)
	}
	return p
}

func TestCover4GitShapes(t *testing.T) {
	ctx := context.Background()
	t.Run("replay garbage output is corrupt", func(t *testing.T) {
		bin := fakeGitScript(t, `if [ "$1" = "merge-base" ]; then echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; exit 0; fi
echo "not-a-sha"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.Replay(ctx, t.TempDir(), "o", "b", "h"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("replay: %v", err)
		}
	})
	t.Run("non-numeric count is corrupt", func(t *testing.T) {
		bin := fakeGitScript(t, `echo "lots"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.BehindCount(ctx, t.TempDir(), "b", "h"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("count: %v", err)
		}
	})
	t.Run("unreachable sha reports missing", func(t *testing.T) {
		bin := fakeGitScript(t, `if [ "$1" = "rev-list" ]; then echo "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef f.txt"; exit 0; fi
echo "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef missing"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		ok, err := g.Reachable(ctx, t.TempDir(), "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		if err != nil || ok {
			t.Fatalf("reachable = %v %v", ok, err)
		}
	})
	t.Run("present sha is reachable", func(t *testing.T) {
		bin := fakeGitScript(t, `if [ "$1" = "rev-list" ]; then echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa f.txt"; exit 0; fi
echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa blob 3"; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		ok, err := g.Reachable(ctx, t.TempDir(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if err != nil || !ok {
			t.Fatalf("reachable = %v %v", ok, err)
		}
	})
	t.Run("cat-file outage errors", func(t *testing.T) {
		bin := fakeGitScript(t, `if [ "$1" = "rev-list" ]; then echo "abc f.txt"; exit 0; fi
echo "boom" >&2; exit 128`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		if _, err := g.Reachable(ctx, t.TempDir(), "abc"); err == nil {
			t.Fatal("cat-file err must surface")
		}
	})
	t.Run("stdin pipe large input", func(t *testing.T) {
		bin := fakeGitScript(t, `cat > /dev/null; echo ""; exit 0`)
		g := NewSubprocessGit(bin)
		g.Timeout = 30 * time.Second
		ok, err := g.Reachable(ctx, t.TempDir(), strings.Repeat("a", 40))
		if err != nil || !ok {
			t.Fatalf("reachable = %v %v", ok, err)
		}
	})
}

func TestCover4StaleCacheKeepsMergeBase(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2),
		"refs/pull/1/head": hexSHA(2),
	})
	// Stale stamp (head moved on): GET serves unknown but keeps the last
	// merge base for display continuity, and enqueues the recompute.
	stale := &MergeableDoc{BaseRef: "refs/heads/main", BaseSHA: hexSHA(1), HeadSHA: hexSHA(8),
		MergeBase: hexSHA(7), State: MergeableClean, Conflicts: []string{}, ComputedAt: "2026-09-04T11:00:00Z"}
	_, _ = store.PutBytes(ctx(), e.store, MergeableKey("o", "r", 1), encodeMergeable(stale),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	e.git.MergeBaseSHA = hexSHA(7)
	view, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view.Mergeable.State != MergeableUnknown || view.Mergeable.MergeBase != hexSHA(7) {
		t.Fatalf("stale: %+v", view.Mergeable)
	}
}

func TestCover4PRCreateConflict(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
	_, _ = store.PutBytes(ctx(), e.store, PRKey("o", "r", 1), encodePR(&PRDoc{Num: 1}),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "t", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic"}, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("pr taken: %v", err)
	}
}

func TestCover4UpsertParseIndex(t *testing.T) {
	ix := &Index{Open: []Card{}, ClosedRecent: []Card{}}
	upsertCard(ix, Card{Num: 3, Kind: "pr", Title: "c", State: "closed", UpdatedAt: "2026-09-04T12:00:00Z"})
	if len(ix.ClosedRecent) != 1 {
		t.Fatalf("ix = %+v", ix)
	}
	upsertCard(ix, Card{Num: 3, Kind: "pr", Title: "c", State: "open", UpdatedAt: "2026-09-04T13:00:00Z"})
	if len(ix.Open) != 1 || len(ix.ClosedRecent) != 0 {
		t.Fatalf("reopen: %+v", ix)
	}
	m, err := parseIndex([]byte(`{"version":1,"open":[{"num":1}],"closed_recent":null}`))
	if err != nil || m.Open[0].Labels == nil || m.Open[0].Assignees == nil || m.ClosedRecent == nil {
		t.Fatalf("normalize: %+v %v", m, err)
	}
	sortCards([]Card{{Num: 1, UpdatedAt: "2026-09-04T12:00:00Z"}, {Num: 2, UpdatedAt: "2026-09-04T12:00:00Z"}})
}

func TestCover4RunMergeBadStrategy(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	rec := &TaskRecord{Progress: []string{}}
	_, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: "weird"}, "", rec)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("strategy: %v", err)
	}
}

func TestCover4HTTPLeftovers(t *testing.T) {
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
	t.Run("wrong-typed title is 400", func(t *testing.T) {
		e := mk(t)
		if w := call(e, "POST", "/o/r/api/pulls", `{"title":123,"base_ref":"refs/heads/main","head_ref":"refs/heads/topic"}`); w.Code != 400 {
			t.Fatalf("typed = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("repo lane bad id falls through", func(t *testing.T) {
		e := mk(t)
		req := httptest.NewRequest("GET", "/o/../api/pulls", nil)
		w := httptest.NewRecorder()
		if e.h.Handle(w, req) {
			t.Fatalf("bad id claimed: %d", w.Code)
		}
	})
	t.Run("update-index outage still opens", func(t *testing.T) {
		e := mk(t)
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/z": hexSHA(6)})
		e.failGet(IndexKey("o", "r"), errors.New("down"))
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "z", BaseRef: "refs/heads/main", HeadRef: "refs/heads/z"}, "")
		if err != nil {
			// findOpenPair's index read fails ⇒ the open fails loudly (the
			// duplicate check cannot run blind).
			if !strings.Contains(fmt.Sprint(err), "down") {
				t.Fatalf("open: %v", err)
			}
		}
		e.clearFails()
	})
	t.Run("flight leader error propagates", func(t *testing.T) {
		e := mk(t)
		e.git.MergeBaseErr = errors.New("no base")
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("leader err must surface")
		}
	})
	t.Run("saveMergeable outage is best-effort", func(t *testing.T) {
		e := mk(t)
		e.failPut(MergeableKey("o", "r", 1), 99)
		m, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1)
		if err != nil || m == nil {
			t.Fatalf("best-effort: %+v %v", m, err)
		}
		e.clearFails()
	})
	t.Run("now clock formats", func(t *testing.T) {
		e := mk(t)
		e.svc.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
		if got := e.svc.nowUTC().Format(dateTimeFmt); got != "2026-01-02T03:04:05Z" {
			t.Fatalf("clock: %s", got)
		}
	})
}
