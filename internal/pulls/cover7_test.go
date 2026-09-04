package pulls

import (
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestCover7UpdateMatrix(t *testing.T) {
	mk := func(t *testing.T, private bool) *testEnv {
		t.Helper()
		e := newTestEnv()
		if private {
			e.roles.Public = false
		}
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		return e
	}
	t.Run("private stranger read fails", func(t *testing.T) {
		e := mk(t, true)
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "ghost@x"}, PRPatch{Title: strPtr("x")}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("stranger: %v", err)
		}
		if _, err := e.svc.AddComment(ctx(), "o", "r", 1, auth.Principal{Name: "ghost@x"}, "x"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("comment stranger: %v", err)
		}
	})
	t.Run("load failures", func(t *testing.T) {
		e := mk(t, false)
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("x")}); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
		e.clearFails()
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("x")}); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("issue-kind invisible", func(t *testing.T) {
		e := mk(t, false)
		raw, ver, _ := e.svc.getJSON(ctx(), ThreadKey("o", "r", 1))
		s := strings.Replace(string(raw), `"kind":"pr"`, `"kind":"issue"`, 1)
		_, _ = store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 1), []byte(s), store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("x")}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("kind: %v", err)
		}
	})
	t.Run("missing sidecar", func(t *testing.T) {
		e := mk(t, false)
		_ = e.store.Delete(ctx(), PRKey("o", "r", 1), "")
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("x")}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("sidecar: %v", err)
		}
	})
	t.Run("field gates", func(t *testing.T) {
		e := mk(t, false)
		e.roles.Roles["mallory@example.com"] = "read"
		mallory := auth.Principal{Name: "mallory@example.com"}
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, mallory, PRPatch{State: strPtr("closed")}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("state gate: %v", err)
		}
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, mallory, PRPatch{Body: strPtr("b")}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("body gate: %v", err)
		}
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr(" ")}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("title empty: %v", err)
		}
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Body: strPtr(strings.Repeat("b", MaxBodyBytes+1))}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("body big: %v", err)
		}
	})
	t.Run("event outage fails the edit", func(t *testing.T) {
		e := mk(t, false)
		e.failPut(ThreadKey("o", "r", 1), 99)
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Title: strPtr("zzz")}); !errors.Is(err, ErrConflict) {
			t.Fatalf("event cas: %v", err)
		}
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{State: strPtr("closed")}); !errors.Is(err, ErrConflict) {
			t.Fatalf("state event cas: %v", err)
		}
		e.clearFails()
	})
	t.Run("body save outage fails the edit", func(t *testing.T) {
		e := mk(t, false)
		e.failPut(PRKey("o", "r", 1), 99)
		if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Body: strPtr("zzz")}); !errors.Is(err, ErrConflict) {
			t.Fatalf("body cas: %v", err)
		}
		e.clearFails()
	})
}

func TestCover7ComputeDirect(t *testing.T) {
	t.Run("load errors", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		openBasic(t, e, "o", "r")
		e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("thread err must surface")
		}
		e.clearFails()
		e.failGet(PRKey("o", "r", 1), errors.New("down"))
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); err == nil {
			t.Fatal("pr err must surface")
		}
		e.clearFails()
	})
	t.Run("cross-fork head dirs down", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
		e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
		_, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{Title: "f", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"}}, "")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		e.svc.Dirs = &failForkDirs{ok: map[string]bool{"o/r": true}}
		if _, err := e.svc.ComputeMergeable(ctx(), "o", "r", 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("head dirs: %v", err)
		}
		e.svc.Dirs = &FakeDirs{}
	})
	t.Run("index empty object normalizes", func(t *testing.T) {
		m, err := parseIndex([]byte(`{}`))
		if err != nil || m.Open == nil || m.ClosedRecent == nil {
			t.Fatalf("empty: %+v %v", m, err)
		}
	})
}
