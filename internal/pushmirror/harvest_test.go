// harvest_test.go — Forgejo #625 end to end, headless.
//
// file:// cannot exercise SSH, and this environment has no upstream
// sshd to push to — so the transfer runs behind a stub git binary: it
// delegates ref enumeration to the real git and simulates the SSH
// layer by appending a fixture learned host line to the per-fire
// known_hosts file (parsed out of GIT_SSH_COMMAND, exactly where the
// real ssh client would record accept-new trust). That exercises the
// real sshCommand materialization, the Push harvest read, the secret
// merge, the view, and the repin — learn→persist→surface→repin — with
// fixture known_hosts lines and zero network.
package pushmirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// stubGitWithLearned installs a stub git binary on svc that answers
// for-each-ref via the real git and, on push, appends learnedLine to
// the per-fire known_hosts file (the accept-new simulation) while
// recording every GIT_SSH_COMMAND to capturePath (the repin proof).
// Values bake into the script — Push scrubs the child env to
// PATH/GIT_TERMINAL_PROMPT only, so nothing can ride os.Environ.
func stubGitWithLearned(t *testing.T, svc *Service, learnedLine, capturePath string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"for-each-ref\" ]; then exec git \"$@\"; fi\n" +
		"kh=$(printf '%s\\n' \"$GIT_SSH_COMMAND\" | sed -n 's/.*UserKnownHostsFile=\\([^ ][^ ]*\\).*/\\1/p')\n" +
		"if [ -n \"$kh\" ]; then\n" +
		"  printf '%s\\n' " + shellQuote(learnedLine) + " >> \"$kh\"\n" +
		"fi\n" +
		"printf '%s\\n' \"$GIT_SSH_COMMAND\" >> " + shellQuote(capturePath) + "\n" +
		"exit 0\n"
	fake := filepath.Join(t.TempDir(), "git-stub")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.git.Binary = fake
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestHarvestLearnPersistSurfaceRepin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)

	// Deploy key in the secret sidecar; accept-new (no known_hosts).
	deploy, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	setupPush(t, ctx, reg, st, "o", "r", "ssh://example.com/o/r.git", AuthSSH)
	if err := SaveSecret(ctx, st, "o", "r", &Secret{AuthKind: AuthSSH, SSHPrivateKey: deploy.PrivatePEM}); err != nil {
		t.Fatal(err)
	}
	seedRepo(t, ctx, reg, "o", "r")

	// The "upstream host key": a line the stub learns on first contact.
	hostKey, err := GenerateKeypair("upstream-host")
	if err != nil {
		t.Fatal(err)
	}
	hp := strings.Split(hostKey.PublicKey, " ")
	learnedLine := "example.com " + hp[0] + " " + hp[1]
	capture := filepath.Join(t.TempDir(), "ssh-commands.log")
	stubGitWithLearned(t, svc, learnedLine, capture)

	// First sync: accept-new learns.
	rec, err := svc.SyncNow(ctx, "o", "r", false)
	if err != nil {
		t.Fatalf("first SyncNow: %v", err)
	}
	// Persist: the learned line merged into the secret sidecar + stamp.
	sec, _, err := LoadSecret(ctx, st, "o", "r")
	if err != nil || sec == nil {
		t.Fatal(err)
	}
	if !strings.Contains(sec.SSHKnownHosts, learnedLine) {
		t.Errorf("secret known_hosts = %q, want learned line", sec.SSHKnownHosts)
	}
	if sec.SSHKnownHostsAcceptedAt == "" {
		t.Error("first-accepted-at not stamped")
	}
	// Surface: narration names the fingerprint (never key material),
	// and the view carries fingerprint + accepted-at.
	tail := strings.Join(rec.LogTail, "\n")
	if !strings.Contains(tail, "learned host key "+hostKey.Fingerprint) {
		t.Errorf("log tail misses the learn narration: %q", tail)
	}
	if strings.Contains(tail, hp[1]) {
		t.Error("narration leaks key material")
	}
	doc, _, _ := Load(ctx, st, "o", "r")
	if doc.LastResult != "ok" {
		t.Errorf("outcome = %+v", doc)
	}
	view := ViewOf(doc, sec, time.Now())
	if view.HostKeyFingerprint != hostKey.Fingerprint || view.HostKeyAcceptedAt != sec.SSHKnownHostsAcceptedAt {
		t.Errorf("view = %+v", view)
	}
	// First fire ran accept-new (nothing pinned yet).
	shot, _ := os.ReadFile(capture)
	if !strings.Contains(string(shot), "StrictHostKeyChecking=accept-new") {
		t.Errorf("first fire must accept-new: %q", shot)
	}

	// Second sync: repin — the stored trust now drives strict checking,
	// and the re-learned line dedupes (no duplicate trust).
	rec2, err := svc.SyncNow(ctx, "o", "r", false)
	if err != nil {
		t.Fatalf("second SyncNow: %v", err)
	}
	_ = rec2
	shot, _ = os.ReadFile(capture)
	lines := strings.Split(strings.TrimSpace(string(shot)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "StrictHostKeyChecking=yes") || !strings.Contains(last, "UserKnownHostsFile=") {
		t.Errorf("second fire must repin strict: %q", last)
	}
	sec2, _, _ := LoadSecret(ctx, st, "o", "r")
	if n := strings.Count(sec2.SSHKnownHosts, hp[1]); n != 1 {
		t.Errorf("learned key occurs %d times, want 1 (dedupe): %q", n, sec2.SSHKnownHosts)
	}
	if sec2.SSHKnownHostsAcceptedAt != sec.SSHKnownHostsAcceptedAt {
		t.Error("first-accepted-at must survive later syncs")
	}
}

func TestHarvestConflictKeepsOperatorPinEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)

	deploy, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	setupPush(t, ctx, reg, st, "o", "p", "ssh://example.com/o/p.git", AuthSSH)
	// Operator-pinned trust for the host (keygen/PUT-provided).
	pinned, err := GenerateKeypair("operator-pin")
	if err != nil {
		t.Fatal(err)
	}
	pp := strings.Split(pinned.PublicKey, " ")
	pinnedLine := "example.com " + pp[0] + " " + pp[1]
	if err := SaveSecret(ctx, st, "o", "p", &Secret{AuthKind: AuthSSH, SSHPrivateKey: deploy.PrivatePEM, SSHKnownHosts: pinnedLine + "\n"}); err != nil {
		t.Fatal(err)
	}
	seedRepo(t, ctx, reg, "o", "p")

	// The stub "learns" a DIFFERENT key for the same host+keytype
	// (rotation seen in the wild / MITM-shaped): the pin must win.
	rogue, err := GenerateKeypair("rogue-host")
	if err != nil {
		t.Fatal(err)
	}
	rp := strings.Split(rogue.PublicKey, " ")
	rogueLine := "example.com " + rp[0] + " " + rp[1]
	stubGitWithLearned(t, svc, rogueLine, filepath.Join(t.TempDir(), "ssh.log"))

	if _, err := svc.SyncNow(ctx, "o", "p", false); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	sec, _, _ := LoadSecret(ctx, st, "o", "p")
	if strings.Contains(sec.SSHKnownHosts, rp[1]) {
		t.Error("rogue learned key replaced the operator pin")
	}
	if !strings.Contains(sec.SSHKnownHosts, pp[1]) {
		t.Errorf("operator pin lost: %q", sec.SSHKnownHosts)
	}
	if sec.SSHKnownHostsAcceptedAt != "" {
		t.Error("conflict-only harvest must not stamp first-accepted-at")
	}
	doc, _, _ := Load(ctx, st, "o", "p")
	if doc.LastResult != "ok" {
		t.Errorf("conflict must not fail the sync: %+v", doc)
	}
	view := ViewOf(doc, sec, time.Now())
	if view.HostKeyFingerprint != pinned.Fingerprint {
		t.Errorf("view fingerprint = %q, want pinned %q", view.HostKeyFingerprint, pinned.Fingerprint)
	}
}

// failSecretPutStore fails writes to one key (the harvest-failure
// simulation: the bucket refuses the trust write mid-sync).
type failSecretPutStore struct {
	store.ObjectStore
	secretKey string
}

func (f failSecretPutStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == f.secretKey {
		return store.ObjectMeta{}, fmt.Errorf("boom: secret store down")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestHarvestFailureDoesNotFailSync(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reg, st := testRegistry(t)

	deploy, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	setupPush(t, ctx, reg, st, "o", "h", "ssh://example.com/o/h.git", AuthSSH)
	if err := SaveSecret(ctx, st, "o", "h", &Secret{AuthKind: AuthSSH, SSHPrivateKey: deploy.PrivatePEM}); err != nil {
		t.Fatal(err)
	}
	seedRepo(t, ctx, reg, "o", "h")

	// Service over the failing wrapper (setup used the healthy store).
	svc := testService(t, failSecretPutStore{ObjectStore: st, secretKey: store.PushMirrorSecretKey("o", "h")}, reg)
	hostKey, _ := GenerateKeypair("upstream-host")
	hp := strings.Split(hostKey.PublicKey, " ")
	learnedLine := "example.com " + hp[0] + " " + hp[1]
	stubGitWithLearned(t, svc, learnedLine, filepath.Join(t.TempDir(), "ssh.log"))

	rec, err := svc.SyncNow(ctx, "o", "h", false)
	if err != nil {
		t.Fatalf("harvest miss must not fail the sync: %v", err)
	}
	doc, _, _ := Load(ctx, st, "o", "h")
	if doc.LastResult != "ok" || doc.ConsecutiveFailures != 0 {
		t.Errorf("outcome must stay ok: %+v", doc)
	}
	tail := strings.Join(rec.LogTail, "\n")
	if !strings.Contains(tail, "host-key trust was not recorded") || !strings.Contains(tail, "will retry next sync") {
		t.Errorf("miss must narrate + promise retry: %q", tail)
	}
	if strings.Contains(tail, hp[1]) {
		t.Error("miss narration leaks key material")
	}
	// Trust unwritten: the next accept-new fire re-learns and retries.
	sec, _, _ := LoadSecret(ctx, st, "o", "h")
	if strings.Contains(sec.SSHKnownHosts, hp[1]) {
		t.Error("failed harvest must not persist partial trust")
	}
}
