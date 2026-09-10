// tags_wal_test.go — Forgejo #253 end-to-end through the real WAL: the
// shipped tags composition (newTagsService: subprocess git, registry dirs,
// WAL publish funnel) creates a lightweight tag at a real commit, the tag
// is visible after sync, survives a registry restart (fresh cache dir —
// disk is a cache, the bucket is the repository), resolves via the tags
// ref stream shape, and recreating it 409s. Skipped when git is absent.
package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/tags"
	"git.packden.us/crueber/walhub/internal/wal"
)

func testTagsConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	return cfg
}

// seedCommit mints a real commit object in the bare repo at gitDir
// (commit-tree with pinned identity; runners have no global git config)
// and returns its sha.
func seedCommit(t *testing.T, gitDir string) string {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_DIR="+gitDir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
			"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
			"GIT_TERMINAL_PROMPT=0",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	tree := run("mktree", "--missing")
	cmd := exec.Command("git", "commit-tree", tree, "-m", "seed")
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+gitDir,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git commit-tree: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func liveTagSHA(t *testing.T, reg *wal.Registry, repo, ref string) (string, bool) {
	t.Helper()
	ctx := context.Background()
	h, err := reg.Open(ctx, repo)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if g, serr := h.Sync(ctx, wal.LevelServe); serr != nil {
		t.Fatalf("sync: %v", serr)
	} else {
		g.Release()
	}
	snap, serr := reg.GitLayer().Snapshot(h.Repo())
	if serr != nil {
		t.Fatalf("snapshot: %v", serr)
	}
	e, ok := snap.Get(ref)
	if !ok {
		return "", false
	}
	return string(e.Oid), true
}

func TestTagsEndToEndRealWAL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary absent")
	}
	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, testTagsConfig(t))
	defer reg.Close()

	h, err := reg.Create(ctx, "o/r", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sha := seedCommit(t, h.Repo().Path)
	if len(sha) != 40 {
		t.Fatalf("seed sha = %q", sha)
	}

	svc, hdl := newTagsService(st, nil, reg, "git")
	svc.Roles = nil // untyped nil: exercise the principal-flags fallback
	_ = hdl
	principal := auth.Principal{Name: "jane", Write: true}

	tag, err := svc.CreateTag(ctx, "o", "r", principal, tags.CreateInput{Name: "v1", SHA: sha})
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if tag.Ref != "refs/tags/v1" || tag.SHA != sha {
		t.Fatalf("tag = %+v", tag)
	}

	// Visible after sync (the tags ref stream reads this snapshot).
	if live, ok := liveTagSHA(t, reg, "o/r", "refs/tags/v1"); !ok || live != sha {
		t.Fatalf("live = %q ok=%v, want %s", live, ok, sha)
	}

	// Recreating the tag 409s (CAS create, never a move) — checked on the
	// live handle: after a cold restart the seed object itself is gone
	// from the fresh cache (ref-only publishes carry no pack), so an
	// unknown-sha 404 would precede the conflict there.
	_, err = svc.CreateTag(ctx, "o", "r", principal, tags.CreateInput{Name: "v1", SHA: sha})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("recreate err = %v, want 409 already-exists", err)
	}

	// Restart survival: a fresh registry over the same bucket with a wiped
	// cache dir replays the log and converges on the tag.
	reg.Close()
	reg2 := wal.NewRegistry(ctx, st, testTagsConfig(t))
	defer reg2.Close()
	if live, ok := liveTagSHA(t, reg2, "o/r", "refs/tags/v1"); !ok || live != sha {
		t.Fatalf("replayed = %q ok=%v, want %s", live, ok, sha)
	}
}
