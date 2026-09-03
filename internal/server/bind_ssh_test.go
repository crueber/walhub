package server

// bind_ssh_test.go — real git over SSH (17_ssh.md §6): the in-process server
// with a real WAL engine answers `git clone` and `git push` spoken by the git
// CLI through ssh://, in both directions, with a generated client key.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
	gossh "golang.org/x/crypto/ssh"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// writeClientKey writes an OpenSSH private key for GIT_SSH_COMMAND and
// returns the matching authorized_keys line.
func writeClientKey(t *testing.T, dir, name string) (keyPath, pubLine string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(priv, name)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, name)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath, strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
}

// sshTestEnv boots a real server (memory store + WAL engine) with the SSH
// transport on 127.0.0.1:0 and returns the ssh:// base URL plus a client key
// authorized for principal "ada" (write granted).
func sshTestEnv(t *testing.T) (base, keyPath string) {
	t.Helper()
	keyPath, pubLine := writeClientKey(t, t.TempDir(), "client_ed25519")

	cfg := walTestCfg(t)
	cfg.Server.AutoCreateOnPush = true
	cfg.Server.SSH.Listen = "127.0.0.1:0"
	cfg.Server.SSH.Keys = []config.SshKey{{Principal: "ada", Key: pubLine, Write: true}}

	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)

	srv := New(Options{
		Config:  cfg,
		Store:   st,
		Engine:  NewWalEngine(reg, cfg),
		DataDir: t.TempDir(),
		Log:     testLogger(t),
	})
	sshSrv, err := srv.SSH()
	if err != nil {
		t.Fatal(err)
	}
	if sshSrv == nil {
		t.Fatal("SSH() must build with listen configured")
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = sshSrv.ListenAndServe(runCtx) }()

	var addr string
	for i := 0; i < 100; i++ {
		if a := sshSrv.Addr(); a != nil {
			if c, derr := net.Dial("tcp", a.String()); derr == nil {
				c.Close()
				addr = a.String()
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("ssh listener did not come up")
	}
	return "ssh://git@" + addr, keyPath
}

// gitSSH runs one git command against the SSH remote with the client key.
func gitSSH(t *testing.T, keyPath, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -vv -o IdentitiesOnly=yes -E "+keyPath+".sshlog",
		"GIT_AUTHOR_NAME=ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=ada", "GIT_COMMITTER_EMAIL=ada@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestSSHTransportCloneAndPush(t *testing.T) {
	base, keyPath := sshTestEnv(t)

	// --- push (in direction): seed a repo over ssh://receive-pack ---------
	src := t.TempDir()
	gitSSH(t, keyPath, src, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("over ssh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitSSH(t, keyPath, src, "add", "-A")
	gitSSH(t, keyPath, src, "commit", "-q", "-m", "first over ssh")
	gitSSH(t, keyPath, src, "push", "-q", base+"/acme/e2e.git", "main")

	// --- clone (out direction): fetch it back over ssh://upload-pack ------
	clone := t.TempDir()
	gitSSH(t, keyPath, clone, "clone", "-q", base+"/acme/e2e.git", ".")
	blob, err := os.ReadFile(filepath.Join(clone, "hello.txt"))
	if err != nil || string(blob) != "over ssh\n" {
		t.Fatalf("cloned content = %q err=%v", blob, err)
	}

	// --- push again from the clone (round trip) ---------------------------
	if err := os.WriteFile(filepath.Join(clone, "second.txt"), []byte("round trip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitSSH(t, keyPath, clone, "add", "-A")
	gitSSH(t, keyPath, clone, "commit", "-q", "-m", "second over ssh")
	gitSSH(t, keyPath, clone, "push", "-q", "origin", "main")

	logOut := gitSSH(t, keyPath, clone, "log", "--oneline")
	if !strings.Contains(logOut, "first over ssh") || !strings.Contains(logOut, "second over ssh") {
		t.Fatalf("clone log missing commits:\n%s", logOut)
	}
}

func TestSSHReadOnlyKeyCannotPush(t *testing.T) {
	dir := t.TempDir()
	rwPath, rwLine := writeClientKey(t, dir, "rw_ed25519")
	roPath, roLine := writeClientKey(t, dir, "ro_ed25519")

	cfg := walTestCfg(t)
	cfg.Server.AutoCreateOnPush = true
	cfg.Server.SSH.Listen = "127.0.0.1:0"
	cfg.Server.SSH.Keys = []config.SshKey{
		{Principal: "ada", Key: rwLine, Write: true},
		{Principal: "observer", Key: roLine},
	}

	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	srv := New(Options{Config: cfg, Store: st, Engine: NewWalEngine(reg, cfg), DataDir: t.TempDir(), Log: testLogger(t)})
	sshSrv, err := srv.SSH()
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = sshSrv.ListenAndServe(runCtx) }()
	var base string
	for i := 0; i < 100; i++ {
		if a := sshSrv.Addr(); a != nil {
			if c, derr := net.Dial("tcp", a.String()); derr == nil {
				c.Close()
				base = "ssh://git@" + a.String()
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if base == "" {
		t.Fatal("ssh listener did not come up")
	}

	// The write key seeds the repo by pushing (auto-create on push).
	seed := t.TempDir()
	gitSSH(t, rwPath, seed, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitSSH(t, rwPath, seed, "add", "-A")
	gitSSH(t, rwPath, seed, "commit", "-q", "-m", "rw push")
	gitSSH(t, rwPath, seed, "push", "-q", base+"/acme/ro.git", "main")

	// The read-only key clones fine but its push dies with the refusal.
	ro := t.TempDir()
	gitSSH(t, roPath, ro, "clone", "-q", base+"/acme/ro.git", ".")
	if err := os.WriteFile(filepath.Join(ro, "g.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitSSH(t, roPath, ro, "add", "-A")
	gitSSH(t, roPath, ro, "commit", "-q", "-m", "ro attempt")

	push := exec.Command("git", "push", "origin", "main")
	push.Dir = ro
	push.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -i "+roPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes",
		"GIT_AUTHOR_NAME=o", "GIT_AUTHOR_EMAIL=o@e", "GIT_COMMITTER_NAME=o", "GIT_COMMITTER_EMAIL=o@e",
	)
	out, err := push.CombinedOutput()
	if err == nil {
		t.Fatalf("read-only push must fail; output:\n%s", out)
	}
	if !strings.Contains(string(out), "write access required") {
		t.Fatalf("read-only push error must name the refusal; got:\n%s", out)
	}
}
