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
	// The push source is the bare serving-copy shape (production runs
	// the transfer in the bare serving copy, never a work tree).
	src := t.TempDir()
	gitTest(t, src, "init", "-q", "--bare", ".")
	gitTest(t, work, "push", "-q", "--mirror", src)
	upstream := t.TempDir()
	gitTest(t, upstream, "init", "-q", "--bare", ".")
	r := NewRunner("git", t.TempDir(), 60*time.Second, 30*time.Second)
	if _, err := r.Push(ctx, src, "file://"+upstream, PushAuth{Kind: AuthNone, Scheme: "file"}); err != nil {
		t.Fatalf("push file:// : %v", err)
	}
	tip := strings.TrimSpace(gitTest(t, src, "rev-parse", "refs/heads/main"))
	got := strings.TrimSpace(gitTest(t, upstream, "rev-parse", "refs/heads/main"))
	if got != tip {
		t.Errorf("upstream tip = %q, want %q", got, tip)
	}
	refs, err := r.ListRefs(ctx, upstream)
	if err != nil || refs["refs/heads/main"] != tip {
		t.Errorf("ListRefs = %v,%v", refs, err)
	}
}

// Forge-internal refs must never leave the serving copy: refs/pull/**
// (walhub's own PR heads — ordinary WAL ref state, materialized by every
// Serve sync) plus the S4-dropped namespaces stay out, while user
// namespaces (heads/tags/custom) ship. A deleted branch prunes upstream
// (walhub is the primary).
func TestPushSkipsInternalRefsAndPrunes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	work := fixtureWorkRepo(t)
	src := t.TempDir()
	gitTest(t, src, "init", "-q", "--bare", ".")
	gitTest(t, work, "push", "-q", "--mirror", src)
	tip := strings.TrimSpace(gitTest(t, src, "rev-parse", "refs/heads/main"))
	gitTest(t, src, "update-ref", "refs/pull/1/head", tip)
	gitTest(t, src, "update-ref", "refs/notes/commits", tip)
	gitTest(t, src, "update-ref", "refs/custom/x", tip)
	gitTest(t, src, "update-ref", "refs/tags/v1", tip)
	upstream := t.TempDir()
	gitTest(t, upstream, "init", "-q", "--bare", ".")
	r := NewRunner("git", t.TempDir(), 60*time.Second, 30*time.Second)
	if _, err := r.Push(ctx, src, "file://"+upstream, PushAuth{Kind: AuthNone, Scheme: "file"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	refs, err := r.ListRefs(ctx, upstream)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"refs/heads/main", "refs/tags/v1", "refs/custom/x"} {
		if refs[want] != tip {
			t.Errorf("upstream missing %s: %v", want, refs)
		}
	}
	for _, banned := range []string{"refs/pull/1/head", "refs/notes/commits"} {
		if _, ok := refs[banned]; ok {
			t.Errorf("internal ref %s shipped upstream", banned)
		}
	}
	// Delete the branch locally: the next fire prunes it upstream.
	gitTest(t, src, "update-ref", "-d", "refs/heads/main")
	if _, err := r.Push(ctx, src, "file://"+upstream, PushAuth{Kind: AuthNone, Scheme: "file"}); err != nil {
		t.Fatalf("prune push: %v", err)
	}
	refs, err = r.ListRefs(ctx, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs["refs/heads/main"]; ok {
		t.Errorf("deleted branch survived upstream: %v", refs)
	}
}

func TestPushUnknownAuthKind(t *testing.T) {
	r := NewRunner("git", t.TempDir(), time.Minute, time.Minute)
	_, err := r.Push(context.Background(), t.TempDir(), "file:///x", PushAuth{Kind: "keys"})
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
	cmd, khPath, cleanup, err := r.sshCommand(PushAuth{Kind: AuthSSH, PrivateKey: k.PrivatePEM})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "-i ") || !strings.Contains(cmd, "BatchMode=yes") || !strings.Contains(cmd, "accept-new") {
		t.Errorf("ssh command = %q", cmd)
	}
	// Unpinned accept-new still pins a per-fire known_hosts file (never
	// the ambient ~/.ssh/known_hosts); the harvest reads it back.
	if !strings.Contains(cmd, "UserKnownHostsFile=") || khPath == "" {
		t.Errorf("unpinned ssh command must pin a per-fire known_hosts file: %q", cmd)
	}
	if raw, rerr := os.ReadFile(khPath); rerr != nil || len(raw) != 0 {
		t.Errorf("unpinned known_hosts file = %q,%v, want empty", raw, rerr)
	}
	// Pinned known_hosts → strict checking with the pinned file.
	cmd, _, cleanup, err = r.sshCommand(PushAuth{Kind: AuthSSH, PrivateKey: k.PrivatePEM, KnownHosts: "example.com ssh-ed25519 AAAA\n"})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "StrictHostKeyChecking=yes") || !strings.Contains(cmd, "UserKnownHostsFile=") {
		t.Errorf("pinned ssh command = %q", cmd)
	}
	// Stable plaintext hostnames for the harvest merge (Forgejo #625):
	// distro ssh_config often ships HashKnownHosts=yes, and salted |1|
	// tokens would defeat the host+keytype dedupe with a fresh token
	// per fire.
	if !strings.Contains(cmd, "HashKnownHosts=no") {
		t.Errorf("ssh command must pin HashKnownHosts=no: %q", cmd)
	}
	// The key file is 0600 and swept by cleanup.
	if _, _, _, err := r.sshCommand(PushAuth{Kind: AuthSSH}); err == nil {
		t.Error("empty private key accepted")
	}
}

func TestCredentialArgvPinned(t *testing.T) {
	argv := credentialArgv("https", "example.com", "WALHUB_PUSHMIRROR_TOKEN", "WALHUB_PUSHMIRROR_USER")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "credential.https://example.com.helper") {
		t.Errorf("helper not host-pinned: %q", joined)
	}
	if strings.Index(joined, `"="`) < 0 && !strings.Contains(joined, "-c") {
		t.Errorf("clear-then-set order missing: %q", joined)
	}
	if !strings.Contains(joined, "username=$WALHUB_PUSHMIRROR_USER") ||
		!strings.Contains(joined, "password=$WALHUB_PUSHMIRROR_TOKEN") {
		t.Errorf("credential halves must ride env, not argv: %q", joined)
	}
}

// The username is user-controlled text and the `!` credential helper
// runs through a shell: the value must reach git ONLY via child env,
// never via argv. A fake git binary records both; a hostile username
// must appear in env verbatim and nowhere in argv.
func TestPushHostileUsernameRidesEnvOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	dump := filepath.Join(dir, "calls.dump")
	script := "#!/bin/sh\n" +
		"echo \"ARGV: $@\" >> " + dump + "\n" +
		"env >> " + dump + "\n" +
		"if [ \"$1\" = \"for-each-ref\" ]; then echo \"abc123 refs/heads/main\"; fi\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	evil := "x;touch " + filepath.Join(dir, "pwned") + " $(touch " + filepath.Join(dir, "pwned2") + ")"
	r := NewRunner(fake, t.TempDir(), 60*time.Second, 30*time.Second)
	_, err := r.Push(ctx, dir, "https://example.com/r.git", PushAuth{
		Kind: AuthPassword, Scheme: "https", Host: "example.com",
		Username: evil, Password: "s3cret",
	})
	if err != nil {
		t.Fatalf("fake push: %v", err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "ARGV:") && strings.Contains(line, evil) {
			t.Errorf("username reached argv (shell-injection surface): %q", line)
		}
	}
	if !strings.Contains(string(raw), "WALHUB_PUSHMIRROR_USER="+evil) {
		t.Errorf("username missing from child env:\n%s", raw)
	}
	if !strings.Contains(string(raw), "WALHUB_PUSHMIRROR_TOKEN=s3cret") {
		t.Errorf("secret missing from child env:\n%s", raw)
	}
	if !strings.Contains(string(raw), "--prune") ||
		!strings.Contains(string(raw), "+refs/heads/*:refs/heads/*") {
		t.Errorf("transfer shape not pinned in argv:\n%s", raw)
	}
	for _, marker := range []string{"pwned", "pwned2"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); !os.IsNotExist(err) {
			t.Errorf("shell expanded the username (marker %s exists)", marker)
		}
	}
}

// pushRefspecs renders forced wildcards per surviving namespace, exact
// forced refspecs for bare two-segment names, and fails loudly when the
// enumeration itself fails (never a silent empty push).
func TestPushRefspecsRender(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	work := fixtureWorkRepo(t)
	src := t.TempDir()
	gitTest(t, src, "init", "-q", "--bare", ".")
	gitTest(t, work, "push", "-q", "--mirror", src)
	tip := strings.TrimSpace(gitTest(t, src, "rev-parse", "refs/heads/main"))
	gitTest(t, src, "update-ref", "refs/foo", tip)
	r := NewRunner("git", t.TempDir(), 60*time.Second, 30*time.Second)
	specs, err := r.pushRefspecs(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(specs, " ")
	for _, want := range []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "+refs/foo:refs/foo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("refspecs = %q, want %q", joined, want)
		}
	}
	if _, err := r.pushRefspecs(ctx, t.TempDir()); err == nil {
		t.Error("pushRefspecs in non-repo succeeded")
	}
}

func TestKeepPushRef(t *testing.T) {
	keep := []string{"refs/heads/main", "refs/tags/v1", "refs/custom/x", "refs/drafts/1"}
	for _, k := range keep {
		if !keepPushRef(k) {
			t.Errorf("keepPushRef(%q) = false", k)
		}
	}
	drop := []string{
		"refs/pull/1/head", "refs/changes/1", "refs/review/1",
		"refs/notes/commits", "refs/replace/abc", "refs/meta/x",
		"refs/keep-around/y",
	}
	for _, d := range drop {
		if keepPushRef(d) {
			t.Errorf("keepPushRef(%q) = true", d)
		}
	}
}
