package server

// bind_ssh_test.go — real git over SSH (17_ssh.md §6): the in-process server
// with a real WAL engine answers `git clone` and `git push` spoken by the git
// CLI through ssh://, in both directions. Keys come from the store-backed
// registry: registered directly (unit flow) or through the HTTP API
// (user story flow).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
	gossh "golang.org/x/crypto/ssh"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
// transport on 127.0.0.1:0, registers the client key for principal "ada"
// (write granted via auth "none"), and returns the ssh:// base URL.
func sshTestEnv(t *testing.T) (base, keyPath string) {
	t.Helper()
	keyPath, pubLine := writeClientKey(t, t.TempDir(), "client_ed25519")

	cfg := walTestCfg(t)
	cfg.Server.AutoCreateOnPush = true
	cfg.Server.SSH.Listen = "127.0.0.1:0"

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
	// the user registers the key through the API surface (17_ssh.md §3)
	if _, err := srv.SSHKeyRegistry().Add(ctx, "ada", pubLine, "e2e"); err != nil {
		t.Fatal(err)
	}
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
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes",
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
	// oidc mode with a write_domains split: observer is admitted (read) but
	// has no write — their key clones, their push is refused. Rights come
	// from PrincipalForName at SSH-auth time, not from the key.
	dir := t.TempDir()
	rwPath, rwLine := writeClientKey(t, dir, "rw_ed25519")
	roPath, roLine := writeClientKey(t, dir, "ro_ed25519")

	cfg := walTestCfg(t)
	cfg.Server.AutoCreateOnPush = true
	cfg.Server.SSH.Listen = "127.0.0.1:0"
	cfg.Server.Auth.Mode = "oidc"
	cfg.Server.Auth.AllowedDomains = []string{"example.com", "writer.example.com"}
	cfg.Server.Auth.WriteDomains = []string{"writer.example.com"}

	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	srv := New(Options{Config: cfg, Store: st, Engine: NewWalEngine(reg, cfg), DataDir: t.TempDir(), Log: testLogger(t)})
	keys := srv.SSHKeyRegistry()
	if _, err := keys.Add(ctx, "ada@writer.example.com", rwLine, "rw"); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(ctx, "observer@example.com", roLine, "ro"); err != nil {
		t.Fatal(err)
	}
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

	// the write principal seeds the repo by pushing (auto-create on push)
	seed := t.TempDir()
	gitSSH(t, rwPath, seed, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitSSH(t, rwPath, seed, "add", "-A")
	gitSSH(t, rwPath, seed, "commit", "-q", "-m", "rw push")
	gitSSH(t, rwPath, seed, "push", "-q", base+"/acme/ro.git", "main")

	// the read-only key clones fine but its push dies with the refusal
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

func TestSSHKeysViaAPIThenClone(t *testing.T) {
	// the full user story (17_ssh.md §3): the key is added through the HTTP
	// API, used for a clone and a push, and its removal revokes the access.
	keyPath, pubLine := writeClientKey(t, t.TempDir(), "client_ed25519")

	cfg := walTestCfg(t)
	cfg.Server.AutoCreateOnPush = true
	cfg.Server.SSH.Listen = "127.0.0.1:0"

	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)

	apiEnv := api.NewEnv(st, nil, cfg, NewWalEngine(reg, cfg), "test", "test-host")
	apiProvider := NewAPIProvider(apiEnv)
	srv := New(Options{Config: cfg, Store: st, Engine: NewWalEngine(reg, cfg), DataDir: t.TempDir(), Log: testLogger(t), API: apiProvider})
	apiEnv.SSHKeys = srv.SSHKeyRegistry() // read at request time
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)

	sshSrv, err := srv.SSH()
	if err != nil {
		t.Fatal(err)
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
	sshBase := "ssh://git@" + addr

	// the key does not work before it is registered
	seed := t.TempDir()
	gitSSHExpectFail(t, keyPath, seed, "ls-remote", sshBase+"/acme/api.git")

	// add the key through the API
	res, err := http.Post(api.URL+"/api/v1/ssh-keys", "application/json",
		strings.NewReader(fmt.Sprintf(`{"key": %q, "title": "laptop"}`, pubLine)))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("add key = %d %s", res.StatusCode, b)
	}
	var added struct {
		Principal   string `json:"principal"`
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(res.Body).Decode(&added); err != nil {
		t.Fatal(err)
	}
	if added.Principal != "anon" || added.ID == "" || !strings.HasPrefix(added.Fingerprint, "SHA256:") {
		t.Fatalf("added record = %+v", added)
	}

	// list shows exactly one key
	lres, lerr := http.Get(api.URL + "/api/v1/ssh-keys")
	if lerr != nil {
		t.Fatal(lerr)
	}
	var list []map[string]any
	json.NewDecoder(lres.Body).Decode(&list)
	if lres.StatusCode != 200 || len(list) != 1 || list[0]["id"] != added.ID {
		t.Fatalf("list = %d %v", lres.StatusCode, list)
	}

	// duplicate registration -> 409
	dup, derr := http.Post(api.URL+"/api/v1/ssh-keys", "application/json",
		strings.NewReader(fmt.Sprintf(`{"key": %q}`, pubLine)))
	if derr != nil {
		t.Fatal(derr)
	}
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate = %d, want 409", dup.StatusCode)
	}

	// the key now pushes (in, auto-creating the repo) and clones (out)
	seedDir := t.TempDir()
	gitSSH(t, keyPath, seedDir, "init", "-q", "-b", "main", ".")
	if werr := os.WriteFile(filepath.Join(seedDir, "f.txt"), []byte("via api key\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	gitSSH(t, keyPath, seedDir, "add", "-A")
	gitSSH(t, keyPath, seedDir, "commit", "-q", "-m", "push with an api-added key")
	gitSSH(t, keyPath, seedDir, "push", "-q", sshBase+"/acme/api.git", "main")

	cloneDir := t.TempDir()
	gitSSH(t, keyPath, cloneDir, "clone", "-q", sshBase+"/acme/api.git", ".")

	// removing the key revokes it: the next ls-remote fails
	delReq, delErr := http.NewRequest(http.MethodDelete, api.URL+"/api/v1/ssh-keys/"+added.ID, nil)
	if delErr != nil {
		t.Fatal(delErr)
	}
	delRes, delErr := http.DefaultClient.Do(delReq)
	if delErr != nil || delRes.StatusCode != http.StatusOK {
		t.Fatalf("delete = %v %d", delErr, delRes.StatusCode)
	}
	delRes.Body.Close()
	gitSSHExpectFail(t, keyPath, cloneDir, "ls-remote", sshBase+"/acme/api.git")
}

// gitSSHExpectFail asserts a git command fails (access revoked, absent repo).
func gitSSHExpectFail(t *testing.T, keyPath, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o BatchMode=yes",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git %s must fail\n%s", strings.Join(args, " "), out)
	}
}
