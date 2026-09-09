// bind_wal_activity_test.go — Forgejo #247 push-path derivation (R1 B3):
// WalEngine.Publish derives the HEAD-tip hint from the serving copy and the
// WAL records it on the sidecar; derivation failure NEVER fails the push.
//
// The seed flow mirrors TestWalEngineNewLocalPackFindsIngestedPack (real
// commit in a scratch repo, objects fetched into the serving copy), then
// publishes through the real funnel and asserts the durable sidecar bytes.
package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

func TestWalEnginePublishDerivesActivity(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/act")
	h, err := reg.Create(ctx, id.String(), git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	repo := h.Repo()

	scratch := t.TempDir()
	_, oid := gitCommitPack(t, scratch)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + repo.Path}
	fetch := exec.Command("git", "fetch", "-q", scratch, "main:refs/heads/main")
	fetch.Dir = repo.Path
	fetch.Env = env
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("seed fetch: %v: %s", err, out)
	}
	del := exec.Command("git", "update-ref", "-d", "refs/heads/main")
	del.Dir = repo.Path
	del.Env = env
	if out, err := del.CombinedOutput(); err != nil {
		t.Fatalf("delete seed ref: %v: %s", err, out)
	}

	req := &git.PushRequest{Commands: []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: oid, Ref: "refs/heads/main"}}}
	res, err := e.Publish(ctx, id, req, "alice", wal.ObjectAccess{Local: repo})
	if err != nil || res.Seq == 0 {
		t.Fatalf("publish: res=%+v err=%v", res, err)
	}
	body, _, err := store.GetBytes(ctx, st, "repos/o/act/"+store.StatsKeySuffix, store.GetOptions{})
	if err != nil {
		t.Fatalf("sidecar GET: %v", err)
	}
	s, ok, err := sizecatalog.DecodeStats(body)
	if err != nil || !ok {
		t.Fatalf("sidecar decode: %v %v", ok, err)
	}
	if s.LastCommitSHA == nil || *s.LastCommitSHA != oid {
		t.Fatalf("derived tip = %+v, want %s", s.LastCommitSHA, oid)
	}
	if s.LastCommitTime == nil || *s.LastCommitTime == "" {
		t.Fatalf("derived time missing: %+v", s)
	}
	if s.LastPushAt == nil {
		t.Fatalf("push clock missing: %+v", s)
	}
	// The derived time matches git's own answer for the tip.
	log := exec.Command("git", "log", "-1", "--format=%cI", oid)
	log.Dir = repo.Path
	log.Env = env
	out, err := log.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	want := strings.TrimSpace(string(out))
	if got := *s.LastCommitTime; got != want[:len("2006-01-02T15:04:05")] && !strings.HasPrefix(want, got[:len("2006-01-02")]) {
		t.Fatalf("derived time = %q, git says %q", got, want)
	}
}

func TestWalEnginePublishWithoutLocalDegradesToNulls(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/nonlocal")
	if _, err := reg.Create(ctx, id.String(), git.Sha1); err != nil {
		t.Fatal(err)
	}
	// No serving copy (empty access): derivation is impossible, but the
	// push still commits (R1 B3 — never fail the push).
	req := &git.PushRequest{Commands: []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: strings.Repeat("b", 40), Ref: "refs/heads/main"}}}
	res, err := e.Publish(ctx, id, req, "alice", wal.ObjectAccess{})
	if err != nil || res.Seq == 0 {
		t.Fatalf("push without local must commit: res=%+v err=%v", res, err)
	}
	body, _, err := store.GetBytes(ctx, st, "repos/o/nonlocal/"+store.StatsKeySuffix, store.GetOptions{})
	if err != nil {
		t.Fatalf("sidecar GET: %v", err)
	}
	s, ok, err := sizecatalog.DecodeStats(body)
	if err != nil || !ok {
		t.Fatalf("sidecar decode: %v %v", ok, err)
	}
	if s.LastCommitSHA != nil || s.LastCommitTime != nil {
		t.Fatalf("impossible derivation must record nulls: %+v", s)
	}
	if s.LastPushAt == nil {
		t.Fatalf("push clock stamps even without derivation: %+v", s)
	}
}

func TestDerivePushActivityTagOnlySkips(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	h, err := reg.Create(ctx, "o/tag", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	repo := h.Repo()
	// A tag-only push (HEAD untouched) derives nothing — even with a local
	// copy present. Direct unit call (no publish): pure derivation rule.
	cmds := []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: strings.Repeat("c", 40), Ref: "refs/tags/v1"}}
	if act := derivePushActivity(ctx, h, wal.ObjectAccess{Local: repo}, cmds); act != nil {
		t.Fatalf("tag-only push must not derive: %+v", act)
	}
	// A delete of HEAD derives nothing either.
	del := []git.PushCommand{{
		Old: strings.Repeat("c", 40), New: strings.Repeat("0", 40), Ref: "refs/heads/main"}}
	if act := derivePushActivity(ctx, h, wal.ObjectAccess{Local: repo}, del); act != nil {
		t.Fatalf("HEAD delete must not derive: %+v", act)
	}
}

func TestDerivePushActivityMissingObjectsDegrades(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	h, err := reg.Create(ctx, "o/missobj", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	repo := h.Repo()
	// The tip is a valid oid but its objects were never ingested: git
	// cannot read it → nil hint (never fail the push).
	cmds := []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: strings.Repeat("d", 40), Ref: "refs/heads/main"}}
	if act := derivePushActivity(ctx, h, wal.ObjectAccess{Local: repo}, cmds); act != nil {
		t.Fatalf("missing objects must not derive: %+v", act)
	}
}

func TestDerivePushActivityDetachedHeadSkips(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	h, err := reg.Create(ctx, "o/detached", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	repo := h.Repo()
	// Detached HEAD (no symbolic target): no default branch to attribute.
	if err := os.WriteFile(repo.Path+"/HEAD", []byte(strings.Repeat("e", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds := []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: strings.Repeat("e", 40), Ref: "refs/heads/main"}}
	if act := derivePushActivity(ctx, h, wal.ObjectAccess{Local: repo}, cmds); act != nil {
		t.Fatalf("detached HEAD must not derive: %+v", act)
	}
}
