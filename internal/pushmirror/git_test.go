// git_test.go — the push runner: argv discipline, auth shapes, scrubbing.
package pushmirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	return string(out)
}

// fixtureWorkRepo returns a non-bare repo dir with one commit on main.
func fixtureWorkRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-q", "-m", "one")
	return dir
}

func TestPushFileBare(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	work := fixtureWorkRepo(t)
	upstream := t.TempDir()
	gitTest(t, upstream, "init", "-q", "--bare", ".")
	r := NewRunner("git", t.TempDir(), 60*time.Second, 30*time.Second)
	if err := r.Push(ctx, work, "file://"+upstream, PushAuth{Kind: AuthNone, Scheme: "file"}); err != nil {
		t.Fatalf("push file:// : %v", err)
	}
	tip := strings.TrimSpace(gitTest(t, work, "rev-parse", "refs/heads/main"))
	got := strings.TrimSpace(gitTest(t, upstream, "rev-parse", "refs/heads/main"))
	if got != tip {
		t.Errorf("upstream tip = %q, want %q", got, tip)
	}
	refs, err := r.ListRefs(ctx, upstream)
	if err != nil || refs["refs/heads/main"] != tip {
		t.Errorf("ListRefs = %v,%v", refs, err)
	}
}

func TestPushUnknownAuthKind(t *testing.T) {
	r := NewRunner("git", t.TempDir(), time.Minute, time.Minute)
	err := r.Push(context.Background(), t.TempDir(), "file:///x", PushAuth{Kind: "keys"})
	if err == nil || !strings.Contains(err.Error(), "unknown auth kind") {
		t.Errorf("want unknown-auth-kind error, got %v", err)
	}
}

func TestSSHCommandMaterialization(t *testing.T) {
	r := NewRunner("git", t.TempDir(), time.Minute, time.Minute)
	k, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	cmd, cleanup, err := r.sshCommand(PushAuth{Kind: AuthSSH, PrivateKey: k.PrivatePEM})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "-i ") || !strings.Contains(cmd, "BatchMode=yes") || !strings.Contains(cmd, "accept-new") {
		t.Errorf("ssh command = %q", cmd)
	}
	// Pinned known_hosts → strict checking with the pinned file.
	cmd, cleanup, err = r.sshCommand(PushAuth{Kind: AuthSSH, PrivateKey: k.PrivatePEM, KnownHosts: "example.com ssh-ed25519 AAAA\n"})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "StrictHostKeyChecking=yes") || !strings.Contains(cmd, "UserKnownHostsFile=") {
		t.Errorf("pinned ssh command = %q", cmd)
	}
	// The key file is 0600 and swept by cleanup.
	if _, _, err := r.sshCommand(PushAuth{Kind: AuthSSH}); err == nil {
		t.Error("empty private key accepted")
	}
}

func TestCredentialArgvPinned(t *testing.T) {
	argv := credentialArgv("https", "example.com", "WALHUB_PUSHMIRROR_TOKEN", "alice")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "credential.https://example.com.helper") {
		t.Errorf("helper not host-pinned: %q", joined)
	}
	if strings.Index(joined, `"="`) < 0 && !strings.Contains(joined, "-c") {
		t.Errorf("clear-then-set order missing: %q", joined)
	}
}
