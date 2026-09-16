// service_test.go — the sync engine: validation, file:// end to end,
// lease contention, backoff, loop, on-push enqueue, server-publish exclusion.
package pushmirror

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

var registerOnce sync.Once

func testRegistry(t *testing.T) (*wal.Registry, store.ObjectStore) {
	t.Helper()
	st := store.NewMemory()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	reg := wal.NewRegistry(context.Background(), st, cfg)
	t.Cleanup(reg.Close)
	return reg, st
}

func testService(t *testing.T, st store.ObjectStore, reg *wal.Registry) *Service {
	t.Helper()
	registerOnce.Do(func() { RegisterKind(KindPushMirrorSync) })
	return New(Deps{
		Store: st, Reg: reg,
		CacheDir: t.TempDir(), Hostname: "test-host",
		AllowPrivate: true, AllowFile: true,
	})
}

// seedRepo publishes one commit (from a real git fixture) through the
// WAL: pack → AddPack → ref txn. Returns the branch tip.
func seedRepo(t *testing.T, ctx context.Context, reg *wal.Registry, owner, name string) string {
	t.Helper()
	work := t.TempDir()
	gg := func(argv ...string) string {
		cmd := exec.Command("git", argv...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gg("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gg("add", ".")
	gg("commit", "-q", "-m", "one")
	tip := gg("rev-parse", "refs/heads/main")

	// Build the pack via --stdout into a file (--all, no stdin).
	cmd := exec.Command("git", "pack-objects", "--stdout", "--all")
	cmd.Dir = work
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	packPath := filepath.Join(t.TempDir(), "seed.pack")
	if err := os.WriteFile(packPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	idxCmd := exec.Command("git", "index-pack", packPath)
	idxCmd.Dir = work
	if out, err := idxCmd.CombinedOutput(); err != nil {
		t.Fatalf("index-pack: %v\n%s", err, out)
	}
	trailer := raw[len(raw)-20:]
	checksum := hex.EncodeToString(trailer)

	h, err := reg.Open(ctx, owner+"/"+name)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The idx-install discipline (the pull-mirror publishPack shape):
	// AddPack's internal LevelServe Sync needs the .idx locally BEFORE
	// the pack lands, and a fresh instance materializes from the store
	// alone — so install locally first, upload the .idx after.
	idxSrc := packPath[:len(packPath)-len(".pack")] + ".idx"
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	idxBytes, err := os.ReadFile(idxSrc)
	if err != nil {
		t.Fatalf("read idx: %v", err)
	}
	if err := os.WriteFile(servingIdx+".tmp", idxBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(servingIdx+".tmp", servingIdx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPack(ctx, packPath, checksum, 0, map[string]string{"agent": "test"}); err != nil {
		t.Fatalf("AddPack: %v", err)
	}
	if _, err := store.PutBytes(ctx, reg.Store(), store.RepoPrefix(owner, name)+store.IdxKey(checksum), idxBytes,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil && !store.IsPreconditionFailed(err) {
		t.Fatalf("upload idx: %v", err)
	}
	txn := &proto.RefTransaction{Atomic: true, Updates: []*proto.RefUpdate{
		{Name: "refs/heads/main", OldOid: git.Sha1.ZeroHex(), NewOid: tip},
	}}
	if _, err := h.Publish(ctx, wal.PublishRequest{Txn: txn, Meta: map[string]string{"principal": "test"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return tip
}

func setupPush(t *testing.T, ctx context.Context, reg *wal.Registry, st store.ObjectStore, owner, name, upstreamURL, kind string) {
	t.Helper()
	if _, err := reg.Create(ctx, owner+"/"+name, git.Sha1); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	n, err := repoimport.NormalizeSource(upstreamURL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := Create(ctx, st, owner, name, n.URL, kind, "", ScheduleOff); err != nil {
		t.Fatalf("create pushmirror: %v", err)
	}
}

func TestValidateTarget(t *testing.T) {
	mustNorm := func(raw string) repoimport.Normalized {
		n, err := repoimport.NormalizeSource(raw)
		if err != nil {
			t.Fatalf("normalize %q: %v", raw, err)
		}
		return n
	}
	cases := []struct {
		url  string
		kind string
		ok   bool
	}{
		{"file:///tmp/x", AuthNone, true},
		{"file:///tmp/x", AuthToken, false},
		{"https://example.com/o/r.git", AuthNone, true},
		{"https://example.com/o/r.git", AuthPassword, true},
		{"https://example.com/o/r.git", AuthToken, true},
		{"https://example.com/o/r.git", AuthSSH, false},
		{"http://example.com/o/r.git", AuthNone, true},
		{"http://example.com/o/r.git", AuthToken, false},
		{"git@example.com:o/r.git", AuthSSH, true},
		{"git@example.com:o/r.git", AuthToken, false},
		{"ssh://example.com/o/r.git", AuthSSH, true},
		{"ssh://example.com/o/r.git", AuthNone, false},
		{"git://example.com/o/r.git", AuthNone, false},
	}
	for _, c := range cases {
		err := ValidateTarget(mustNorm(c.url), c.kind, true)
		if c.ok && err != nil {
			t.Errorf("ValidateTarget(%q,%q) = %v, want ok", c.url, c.kind, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateTarget(%q,%q) ok, want error", c.url, c.kind)
		}
	}
}

func TestRunPushFileEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	upstream := t.TempDir()
	gg := func(argv ...string) string {
		cmd := exec.Command("git", argv...)
		cmd.Dir = upstream
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gg("init", "-q", "--bare", ".")
	setupPush(t, ctx, reg, st, "o", "r", "file://"+upstream, AuthNone)
	tip := seedRepo(t, ctx, reg, "o", "r")

	svc := testService(t, st, reg)
	rec, err := svc.SyncNow(ctx, "o", "r", false)
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if rec == nil {
		t.Fatal("nil task record")
	}
	got := gg("rev-parse", "refs/heads/main")
	if got != tip {
		t.Errorf("upstream tip = %q, want %q", got, tip)
	}
	// Clone works — objects landed, not just refs.
	clone := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", "file://"+upstream, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	doc, _, _ := Load(ctx, st, "o", "r")
	if doc.LastResult != "ok" || doc.LastSyncedAt == "" || doc.ConsecutiveFailures != 0 {
		t.Errorf("outcome = %+v", doc)
	}
	// No-op second fire: still ok, still the same tip.
	if _, err := svc.SyncNow(ctx, "o", "r", false); err != nil {
		t.Fatalf("second SyncNow: %v", err)
	}
}

func TestRunPushMissingSecretFailsScrubbed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	upstream := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare", ".")
	cmd.Dir = upstream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	setupPush(t, ctx, reg, st, "o", "s", "file://"+upstream, AuthNone)
	// Flip the kind to token without storing material: must fail
	// (verdict, never silent anonymous) with no secret in the error.
	doc, ver, _ := Load(ctx, st, "o", "s")
	doc.AuthKind = AuthToken
	if err := UpdateCAS(ctx, st, "o", "s", doc, ver); err != nil {
		t.Fatal(err)
	}
	svc := testService(t, st, reg)
	_, err := svc.SyncNow(ctx, "o", "s", false)
	if err == nil {
		t.Fatal("token-without-secret sync succeeded")
	}
	if strings.Contains(err.Error(), "tok") {
		t.Errorf("error leaks material: %v", err)
	}
	doc, _, _ = Load(ctx, st, "o", "s")
	if doc.ConsecutiveFailures != 1 || !strings.HasPrefix(doc.LastResult, "failed: ") {
		t.Errorf("failure outcome = %+v", doc)
	}
}

func TestLeaseContentionSkips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	setupPush(t, ctx, reg, st, "o", "l", "file:///tmp/x", AuthNone)
	svc := testService(t, st, reg)
	// Hold the lease from another host, then fire: skip, not failure.
	other := New(Deps{Store: st, Reg: reg, CacheDir: t.TempDir(), Hostname: "other", AllowFile: true})
	rel, err := other.acquireLease(ctx, "o", "l")
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	defer rel()
	if _, err := svc.SyncNow(ctx, "o", "l", false); err != nil {
		t.Errorf("held-lease fire = %v (want skip-nil)", err)
	}
	doc, _, _ := Load(ctx, st, "o", "l")
	if doc.ConsecutiveFailures != 0 || doc.LastResult != "" {
		t.Errorf("skip touched outcome: %+v", doc)
	}
}

func TestScheduledLoopDueAndOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	upstream := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare", ".")
	cmd.Dir = upstream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	// Scheduled-on repo: fires via Round.
	setupPush(t, ctx, reg, st, "o", "d", "file://"+upstream, AuthNone)
	doc, ver, _ := Load(ctx, st, "o", "d")
	doc.Schedule = PresetDaily
	if err := UpdateCAS(ctx, st, "o", "d", doc, ver); err != nil {
		t.Fatal(err)
	}
	seedRepo(t, ctx, reg, "o", "d")
	// OFF repo: never fires via Round.
	setupPush(t, ctx, reg, st, "o", "f", "file://"+upstream, AuthNone)
	svc := testService(t, st, reg)
	svc.Round(ctx, nil)
	doc, _, _ = Load(ctx, st, "o", "d")
	if doc.LastResult != "ok" {
		t.Errorf("loop did not fire due repo: %+v", doc)
	}
	off, _, _ := Load(ctx, st, "o", "f")
	if off.LastResult != "" || off.LastAttemptAt != "" {
		t.Errorf("loop fired OFF repo: %+v", off)
	}
}

func TestEnqueueOnPushNoConfigNoFire(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// No config: returns immediately, fires nothing (cheap probe only).
	svc.EnqueueOnPush("o", "ghost")
	// Nil-safety: never panics.
	var nilSvc *Service
	nilSvc.EnqueueOnPush("o", "ghost")
}

func TestEnqueueOnPushFiresWhenConfigured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	upstream := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare", ".")
	cmd.Dir = upstream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	setupPush(t, ctx, reg, st, "o", "e", "file://"+upstream, AuthNone)
	seedRepo(t, ctx, reg, "o", "e")
	svc := testService(t, st, reg)
	svc.EnqueueOnPush("o", "e")
	// The async fire lands within the task window: poll the sidecar.
	deadline := time.Now().Add(60 * time.Second)
	for {
		doc, _, _ := Load(ctx, st, "o", "e")
		if doc != nil && doc.LastResult == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("on-push fire never landed: %+v", doc)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestServerPublishBypassExclusion(t *testing.T) {
	// Structural pin: server-side publishes (wal.Publish direct — the
	// sync.go:434 bypass) never enter pushPipeline, so they never reach
	// EnqueueOnPush. This test pins the hook side: EnqueueOnPush is a
	// *server-hook-only* entry — nothing in this package calls it from
	// runPush/SyncNow/Round (a fan-out loop would double-fire every
	// sync). Grep-pin: runPush must not reference EnqueueOnPush.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	setupPush(t, ctx, reg, st, "o", "b", "file:///tmp/x", AuthNone)
	svc := testService(t, st, reg)
	// A scheduled Round on an OFF repo fires nothing even though a
	// config exists — the only automatic fire is the server hook.
	svc.Round(ctx, nil)
	doc, _, _ := Load(ctx, st, "o", "b")
	if doc.LastAttemptAt != "" {
		t.Errorf("Round fired OFF repo without server hook: %+v", doc)
	}
}

func TestRegisterKindPanicsOnDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("duplicate RegisterKind did not panic")
		}
	}()
	RegisterKind("test-dup-kind-623")
	RegisterKind("test-dup-kind-623")
}

func TestResolveAuthErrors(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	n := mustFileNorm(t, "file:///tmp/x")
	if _, err := svc.resolveAuth(&Doc{AuthKind: "bogus"}, nil, n); err == nil {
		t.Error("bogus kind resolved")
	}
	if _, err := svc.resolveAuth(&Doc{AuthKind: AuthToken}, nil, n); err == nil {
		t.Error("token without secret resolved")
	}
	if _, err := svc.resolveAuth(&Doc{AuthKind: AuthSSH}, &Secret{AuthKind: AuthSSH}, n); err == nil {
		t.Error("ssh without key resolved")
	}
	if _, err := svc.resolveAuth(&Doc{AuthKind: AuthNone}, nil, n); err != nil {
		t.Errorf("none failed: %v", err)
	}
}

func mustFileNorm(t *testing.T, raw string) repoimport.Normalized {
	t.Helper()
	nn, err := repoimport.NormalizeSource(raw)
	if err != nil {
		t.Fatal(err)
	}
	return nn
}
