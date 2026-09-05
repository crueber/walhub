// drain_test.go — issue #74 regression: the import leader runs on a
// drain-scoped ctx (13 §8). Drain cancels an in-flight import promptly
// with a narrated 503 terminal, refuses new Begins, and refuses the
// post-drain manifest commit so no manifest can land after drain begins.
package repoimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// hangingGitBinary installs a git stand-in that hangs forever on clone
// (exec-replaced sleep, so CommandContext SIGKILL reaps it — no orphan
// grandchildren) and delegates every other argv to the real git.
func hangingGitBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hang-git")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"clone\" ]; then exec sleep 300; fi\n" +
		"done\n" +
		"exec git \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitRunning polls until the service holds the target's running entry
// (installed synchronously by Begin) or the timeout fires.
func waitRunning(t *testing.T, svc *Service, target string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		_, ok := svc.running[target]
		svc.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no running import for %s within %v", target, timeout)
}

// TestDrainCancelsHangingImport starts an import against a hanging clone,
// drains (table first, then service — the serve.go phase-1 order), and
// asserts a prompt 503 terminal with no manifest committed.
func TestDrainCancelsHangingImport(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, &FakeRoles{})
	svc.git.Binary = hangingGitBinary(t)

	res, _, err := svc.Begin(context.Background(), adminPrincipal(), fileParams("acme", "drain", "file:///hang.git"), "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if res == nil || res.TaskID == "" {
		t.Fatalf("Begin = %+v, want 202 task", res)
	}
	waitRunning(t, svc, "acme/drain", 10*time.Second)
	// Let the leader reach the hanging clone so this exercises a
	// mid-clone cancel, not a pre-start refusal.
	time.Sleep(2 * time.Second)

	start := time.Now()
	svc.reg.Tasks().Drain()
	svc.Drain()
	o := awaitDone(t, svc, res.TaskID, 30*time.Second)
	elapsed := time.Since(start)
	if elapsed >= 30*time.Second {
		t.Fatalf("drain took %v; want a prompt terminal (clone hangs 300s)", elapsed)
	}
	if o == nil || o.Err == nil {
		t.Fatalf("outcome = %+v, want terminal error", o)
	}
	if o.Err.Status != 503 || !strings.Contains(o.Err.Message, "draining") {
		t.Fatalf("outcome error = %+v, want narrated 503 drain interrupt", o.Err)
	}

	// No post-drain commit: neither the manifest nor the provenance
	// sidecar may exist.
	if meta, err := st.Head(context.Background(), store.RepoPrefix("acme", "drain")+store.Manifest); err != nil || meta != nil {
		t.Fatalf("manifest Head = %v, %v; want absent", meta, err)
	}
	if doc, _, err := readImportDoc(context.Background(), st, "acme", "drain"); err != nil || doc != nil {
		t.Fatalf("import.json = %+v, %v; want absent", doc, err)
	}
	// The running entry is released exactly once.
	svc.mu.Lock()
	_, still := svc.running["acme/drain"]
	svc.mu.Unlock()
	if still {
		t.Fatalf("running entry for acme/drain still held after drain")
	}
}

// TestBeginAfterDrain503 refuses new imports once drain has begun.
func TestBeginAfterDrain503(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	svc.Drain()
	svc.Drain() // idempotent: second call is a no-op
	if !svc.Draining() {
		t.Fatalf("Draining = false after Drain")
	}
	_, _, err := svc.Begin(context.Background(), adminPrincipal(), fileParams("acme", "late", "file:///x.git"), "")
	se, ok := err.(*StatusError)
	if !ok || se.Status != 503 {
		t.Fatalf("Begin after drain = %v, want 503 StatusError", err)
	}
	svc.mu.Lock()
	_, running := svc.running["acme/late"]
	_, streamed := svc.streams["acme/late"]
	svc.mu.Unlock()
	if running || streamed {
		t.Fatalf("drained Begin installed state (running=%v streamed=%v)", running, streamed)
	}
}

// TestDrainRefusesCommitPoint drains the service, then runs the body
// headless against a fast real fixture: the clone succeeds, but the
// manifest CAS must refuse before landing.
func TestDrainRefusesCommitPoint(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, &FakeRoles{})
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)
	svc.Drain()
	_, err := svc.RunHeadless(context.Background(), fileParams("acme", "guarded", srcURL), "", "op")
	se, ok := err.(*StatusError)
	if !ok || se.Status != 503 {
		t.Fatalf("RunHeadless on drained service = %v, want 503 StatusError", err)
	}
	if meta, merr := st.Head(context.Background(), store.RepoPrefix("acme", "guarded")+store.Manifest); merr != nil || meta != nil {
		t.Fatalf("manifest Head = %v, %v; want absent (commit refused)", meta, merr)
	}
	if doc, _, derr := readImportDoc(context.Background(), st, "acme", "guarded"); derr != nil || doc != nil {
		t.Fatalf("import.json = %+v, %v; want absent (commit refused)", doc, derr)
	}
}
