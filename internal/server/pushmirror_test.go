// pushmirror_test.go — Forgejo #623 on-push fan-out: a landed client
// push notifies the hook fire-and-forget on the pushPipeline both
// transports share; refusals/failures never notify; nil hook never
// fires. Server-side publishes never enter pushPipeline (the sync.go:434
// bypass) — pinned structurally by the absence of any hook call outside
// pushPipeline.
package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/wal"
)

func onPushServer(t *testing.T, eng *fakeEngine, hook func(id git.RepoId)) *Server {
	t.Helper()
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = eng
		o.OnPush = hook
	})
	eng.exists = true
	eng.placement = Placement{Serve: true}
	return s
}

func TestOnPushFiresAfterLandedPush(t *testing.T) {
	eng := &fakeEngine{}
	var mu sync.Mutex
	var got []string
	s := onPushServer(t, eng, func(id git.RepoId) {
		mu.Lock()
		got = append(got, id.String())
		mu.Unlock()
	})
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	oid := "1111111111111111111111111111111111111111"
	// Delete-only (no tips → no connectivity walk on the fake repo):
	// the fake engine publishes ok → landed → hook fires.
	preq := &git.PushRequest{
		Commands: []git.PushCommand{{Old: oid, New: zero, Ref: "refs/heads/main"}},
		Caps:     []string{"report-status", "side-band-64k"},
	}
	var out strings.Builder
	p := auth.Principal{Name: "ada", Write: true}
	// pushPipeline ingests via the layer: nil pack skips ingest, and the
	// fake engine publishes ok.
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq, nil, 1<<20, &out); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !strings.Contains(out.String(), "ok refs/heads/main") {
		t.Fatalf("report = %q", out.String())
	}
	// Fire-and-forget: poll for the hook (never block the test on it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("on-push hook never fired (got %d)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[0] != "o/r" {
		t.Errorf("hook repo = %v", got)
	}
}

func TestOnPushSilentOnRefusalAndFailure(t *testing.T) {
	eng := &fakeEngine{}
	fired := false
	s := onPushServer(t, eng, func(id git.RepoId) { fired = true })
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Name: "ada", Write: true}
	// All-refused push (per-ref errors, nothing landed): no fan-out.
	eng.pubPerRef = []wal.RefResult{}
	// Use the managed-ref refusal path: refs/pull/** never lands.
	preq := &git.PushRequest{
		Commands: []git.PushCommand{{Old: strings.Repeat("0", 40), New: "1111111111111111111111111111111111111111", Ref: "refs/pull/1/head"}},
		Caps:     []string{"report-status"},
	}
	var out strings.Builder
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq, nil, 1<<20, &out); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if fired {
		t.Error("refused push fanned out")
	}
	// Nil hook: a landed push fires nothing (and never panics).
	// Delete-only (no tips → no connectivity walk on the fake repo).
	eng.pubPerRef = nil
	s2, _ := newTestServer(t, func(o *Options) { o.Engine = eng })
	eng.exists = true
	eng.placement = Placement{Serve: true}
	preq2 := &git.PushRequest{
		Commands: []git.PushCommand{{Old: "1111111111111111111111111111111111111111", New: strings.Repeat("0", 40), Ref: "refs/heads/main"}},
		Caps:     []string{"report-status"},
	}
	var out2 strings.Builder
	if err := s2.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq2, nil, 1<<20, &out2); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !strings.Contains(out2.String(), "ok refs/heads/main") {
		t.Fatalf("report = %q", out2.String())
	}
}
