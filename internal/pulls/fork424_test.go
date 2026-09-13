package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// fakeForkExec records ShareManifest calls (the §7 manifest step,
// issue #424).
type fakeForkExec struct {
	mu          sync.Mutex
	calls       []forkCall
	err         error
	rollbacks   []forkCall
	rollbackErr error
}

type forkCall struct {
	parent, child string
	opt           ForkOptions
}

func (f *fakeForkExec) ShareManifest(_ context.Context, parent, child string, opt ForkOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, forkCall{parent, child, opt})
	return f.err
}

func (f *fakeForkExec) last() (forkCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return forkCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// RollbackShare records rollback calls (issue #432); rollbackErr scripts a
// rollback shortfall.
func (f *fakeForkExec) RollbackShare(_ context.Context, parent, child string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rollbacks = append(f.rollbacks, forkCall{parent, child, ForkOptions{}})
	return f.rollbackErr
}

func (f *fakeForkExec) rollbackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rollbacks)
}

// fakeOwnerGate scripts the #346 target admission.
type fakeOwnerGate struct {
	err *auth.AuthError
}

func (f *fakeOwnerGate) CheckCreateOwner(_ context.Context, _ string, _ auth.Principal) *auth.AuthError {
	return f.err
}

// fakeAccessBoot records EnsureRepoAccess calls.
type fakeAccessBoot struct {
	mu    sync.Mutex
	calls [][4]string
	err   error
}

func (f *fakeAccessBoot) EnsureRepoAccess(_ context.Context, owner, repo, creator, visibility string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [4]string{owner, repo, creator, visibility})
	return f.err
}

func TestFork424InputValidation(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		return e
	}
	t.Run("bad visibility", func(t *testing.T) {
		e := mk(t)
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Visibility: "galaxy"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("visibility: %v", err)
		}
	})
	t.Run("bad branches", func(t *testing.T) {
		e := mk(t)
		for _, b := range []string{"refs/tags/v1", "refs/heads/", "refs/pull/1/head", "a b", "a..b"} {
			if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "c" + strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
					return r
				}
				return 'x'
			}, b), Branch: b}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("branch %q: %v", b, err)
			}
		}
	})
	t.Run("branch normalization", func(t *testing.T) {
		if b, err := normalizeForkBranch("dev"); err != nil || b != "refs/heads/dev" {
			t.Fatalf("short: %q %v", b, err)
		}
		if b, err := normalizeForkBranch("refs/heads/feat/x"); err != nil || b != "refs/heads/feat/x" {
			t.Fatalf("full: %q %v", b, err)
		}
		if b, err := normalizeForkBranch(""); err != nil || b != "" {
			t.Fatalf("empty: %q %v", b, err)
		}
	})
	t.Run("bad description", func(t *testing.T) {
		e := mk(t)
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Description: strings.Repeat("x", MaxForkDescription+1)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("long: %v", err)
		}
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Description: "bad\x00ctl"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("control: %v", err)
		}
	})
	t.Run("owner gate deny", func(t *testing.T) {
		e := mk(t)
		e.svc.OwnerGate = &fakeOwnerGate{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "foreign owner"}}
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{TargetOwner: "other"}); err == nil {
			t.Fatal("gate deny must surface")
		} else if aerr, ok := err.(*auth.AuthError); !ok || aerr.Kind != auth.ErrForbidden {
			t.Fatalf("gate err = %v", err)
		}
	})
	t.Run("precheck fork.json", func(t *testing.T) {
		e := mk(t)
		raw, _ := json.Marshal(&ForkDoc{Parent: "x/y", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("o", "taken"), raw); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "taken"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("taken: %v", err)
		}
	})
	t.Run("precheck manifest", func(t *testing.T) {
		e := mk(t)
		if err := e.svc.putCreate(ctx(), manifestKey("o", "real"), []byte("manifest")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "real"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("real repo: %v", err)
		}
	})
}

func TestFork424RunForkWiring(t *testing.T) {
	t.Run("executor spec + access + root", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		fx := &fakeForkExec{}
		e.svc.ForkExec = fx
		ab := &fakeAccessBoot{}
		e.svc.AccessBoot = ab
		// Parent is itself a fork of a/b (Root carried, not parent).
		proot, _ := json.Marshal(&ForkDoc{Parent: "a/b", Root: "a/b", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("o", "r"), proot); err != nil {
			t.Fatalf("seed parent: %v", err)
		}
		rec := &TaskRecord{Progress: []string{}}
		in := ForkInput{TargetOwner: "f", Name: "c", Visibility: "private", Branch: "refs/heads/dev", Description: "my fork"}
		if err := e.svc.runFork(ctx(), "o", "r", in, writer(), rec); err != nil {
			t.Fatalf("runFork: %v", err)
		}
		call, ok := fx.last()
		if !ok || call.parent != "o/r" || call.child != "f/c" {
			t.Fatalf("executor calls: %+v", fx.calls)
		}
		if call.opt.Branch != "refs/heads/dev" || call.opt.Description != "my fork" || call.opt.Creator != "jane@example.com" {
			t.Fatalf("executor opt: %+v", call.opt)
		}
		ab.mu.Lock()
		acc := append([][4]string(nil), ab.calls...)
		ab.mu.Unlock()
		if len(acc) != 1 || acc[0] != [4]string{"f", "c", "jane@example.com", "private"} {
			t.Fatalf("access calls: %v", acc)
		}
		raw, _, _ := e.svc.getJSON(ctx(), ForkKey("f", "c"))
		var doc ForkDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("fork.json: %v", err)
		}
		if doc.Parent != "o/r" || doc.Root != "a/b" {
			t.Fatalf("fork doc: %+v", doc)
		}
	})
	t.Run("legacy parent without root", func(t *testing.T) {
		e := newTestEnv()
		pleg, _ := json.Marshal(&ForkDoc{Parent: "a/b", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("o", "r"), pleg); err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("runFork: %v", err)
		}
		raw, _, _ := e.svc.getJSON(ctx(), ForkKey("f", "c"))
		var doc ForkDoc
		_ = json.Unmarshal(raw, &doc)
		if doc.Root != "a/b" {
			t.Fatalf("root: %+v", doc)
		}
	})
	t.Run("corrupt parent fork.json fails closed", func(t *testing.T) {
		e := newTestEnv()
		if err := e.svc.putCreate(ctx(), ForkKey("o", "r"), []byte("{bad")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt: %v", err)
		}
	})
	t.Run("share conflict adopts ours", func(t *testing.T) {
		e := newTestEnv()
		fx := &fakeForkExec{err: errForkConflict}
		e.svc.ForkExec = fx
		own, _ := json.Marshal(&ForkDoc{Parent: "o/r", Root: "o/r", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), own); err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("adopt: %v", err)
		}
	})
	t.Run("share conflict foreign is 409", func(t *testing.T) {
		e := newTestEnv()
		fx := &fakeForkExec{err: errForkConflict}
		e.svc.ForkExec = fx
		other, _ := json.Marshal(&ForkDoc{Parent: "x/y", Root: "x/y", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), other); err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign: %v", err)
		}
	})
	t.Run("share conflict no provenance is 409", func(t *testing.T) {
		e := newTestEnv()
		fx := &fakeForkExec{err: errForkConflict}
		e.svc.ForkExec = fx
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("bare: %v", err)
		}
	})
	t.Run("access failure fails loud", func(t *testing.T) {
		e := newTestEnv()
		e.svc.ForkExec = &fakeForkExec{}
		e.svc.AccessBoot = &fakeAccessBoot{err: errors.New("store down")}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err == nil {
			t.Fatal("access failure must fail the task")
		}
	})
}

var errForkConflict = fmt.Errorf("%w: child manifest already exists", ErrConflict)

func TestFork424List(t *testing.T) {
	seed := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		rec := &TaskRecord{Progress: []string{}}
		for _, n := range []string{"a1", "a2", "a3"} {
			if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: n}, writer(), rec); err != nil {
				t.Fatalf("seed %s: %v", n, err)
			}
		}
		return e
	}
	t.Run("empty + pages", func(t *testing.T) {
		e := newTestEnv()
		p, err := e.svc.ListForks(ctx(), "o", "r", writer(), "", 0)
		if err != nil || len(p.Forks) != 0 || p.More || p.Version != 0 {
			t.Fatalf("empty: %+v %v", p, err)
		}
		e2 := seed(t)
		p, err = e2.svc.ListForks(ctx(), "o", "r", writer(), "", 2)
		if err != nil || len(p.Forks) != 2 || !p.More {
			t.Fatalf("page1: %+v %v", p, err)
		}
		if p.Forks[0].Repo != "f/a1" || p.Forks[1].Repo != "f/a2" {
			t.Fatalf("order: %+v", p.Forks)
		}
		p2, err := e2.svc.ListForks(ctx(), "o", "r", writer(), "f/a2", 2)
		if err != nil || len(p2.Forks) != 1 || p2.More || p2.Forks[0].Repo != "f/a3" {
			t.Fatalf("page2: %+v %v", p2, err)
		}
		if p.Version == 0 {
			t.Fatalf("version must be stamped: %+v", p)
		}
	})
	t.Run("bad pagination", func(t *testing.T) {
		e := seed(t)
		if _, err := e.svc.ListForks(ctx(), "o", "r", writer(), "", MaxForkList+1); !errors.Is(err, ErrInvalid) {
			t.Fatalf("n: %v", err)
		}
		if _, err := e.svc.ListForks(ctx(), "o", "r", writer(), "bogus", 10); !errors.Is(err, ErrInvalid) {
			t.Fatalf("after: %v", err)
		}
	})
	t.Run("private stranger denied", func(t *testing.T) {
		e := seed(t)
		e.roles.Public = false
		if _, err := e.svc.ListForks(ctx(), "o", "r", auth.Principal{Name: "ghost@x"}, "", 10); !errors.Is(err, ErrForbidden) {
			t.Fatalf("stranger: %v", err)
		}
	})
}
