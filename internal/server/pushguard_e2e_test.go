package server

// pushguard_e2e_test.go — Forgejo #347 end-to-end proof with the real git
// binary on both transports: the repo-scoped push rule (and auto-create
// admission) over real pushes, plus the #345 read alignment on the fetch
// side. In-process server (memory store + real WAL engine + real identity
// service, token mode) driven by git over HTTP (httptest) and SSH
// (loopback). Push denials assert hard failure; the named message is
// asserted on ls-remote (HTTP discovery) and on push stderr (SSH), where
// git surfaces it — the POST body itself only needs to fail.

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// e2eGit runs git with an isolated HOME/config and a fixed identity.
func e2eGit(t *testing.T, dir string, extra []string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{
		"-c", "user.name=E2E", "-c", "user.email=e2e@example.com",
		"-c", "protocol.version=0",
	}, extra...)
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	sandbox := t.TempDir()
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+sandbox,
		"GIT_CONFIG_GLOBAL="+filepath.Join(sandbox, "gitconfig"),
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.com",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func e2eSeed(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if _, err := e2eGit(t, dir, nil, "init", "-q", "-b", "main", "."); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e2eGit(t, dir, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := e2eGit(t, dir, nil, "commit", "-q", "-m", msg); err != nil {
		t.Fatal(err)
	}
}

func e2eAuthHdr(token string) []string {
	return []string{"-c", "http.extraHeader=Authorization: Bearer " + token}
}

// pushGuardStack boots the shipped-shape stack for the e2e: real engine +
// real identity wired as BOTH the read gate (#345) and the push gate
// (#347), token mode with two host writers (alice, mallory — NEITHER a
// host admin, so every allow below comes from repo-scoped rights).
func pushGuardStack(t *testing.T, mutate func(*config.Config)) (*Server, *identity.Service) {
	t.Helper()
	cfg := walTestCfg(t)
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "alice", Token: "tok-alice", Write: true},
		{Principal: "mallory", Token: "tok-mallory", Write: true},
		{Principal: "observer", Token: "tok-observer"},
	}
	cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	cfg.Server.AutoCreateOnPush = true
	if mutate != nil {
		mutate(cfg)
	}
	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	ident := identity.New(st, cfg)
	eng := NewWalEngine(reg, cfg)
	srv := New(Options{
		Config: cfg, Store: st, Engine: eng,
		DataDir: t.TempDir(), Log: testLogger(t),
		ReadGate: ident, PushGate: ident,
	})
	return srv, ident
}

// markPrivate flips a repo to private visibility (the #345 read-alignment
// cell: the stranger's fetch must then fail while the owner still reads).
func markPrivate(t *testing.T, ident *identity.Service, owner, repo string) {
	t.Helper()
	if _, err := ident.PutAccess(context.Background(), owner, repo, "", identity.VisibilityPrivate, nil); err != nil {
		t.Fatal(err)
	}
}

// grantOwner adds a user:<owner> admin binding to a repo's access doc
// (Forgejo #374: on a private user-owned repo the owner reads through an
// explicit binding — what the Access tab saves — never through the
// host-write flag).
func grantOwner(t *testing.T, ident *identity.Service, owner, repo, user string) {
	t.Helper()
	ctx := context.Background()
	doc, ver, err := ident.GetAccess(ctx, owner, repo)
	if err != nil {
		t.Fatal(err)
	}
	bindings := append(doc.RoleBindings, identity.AccessBinding{
		Subject: "user:" + user, Role: identity.RoleAdmin,
	})
	if _, err := ident.PutAccess(ctx, owner, repo, ver, doc.Visibility, bindings); err != nil {
		t.Fatal(err)
	}
}

func TestPushGuardEndToEndHTTP(t *testing.T) {
	srv, _ := pushGuardStack(t, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// alice seeds her own namespace (self auto-create admits).
	seed := t.TempDir()
	e2eSeed(t, seed, "hello.txt", "hello\n", "first")
	if out, err := e2eGit(t, seed, e2eAuthHdr("tok-alice"), "push", ts.URL+"/alice/r.git", "main"); err != nil {
		t.Fatalf("self push: %v\n%s", err, out)
	}

	// mallory — a host writer with NO relationship to alice/r — must fail
	// the push even though the legacy flag gate would have let her in.
	mallory := t.TempDir()
	if out, err := e2eGit(t, mallory, nil, "clone", "-q", ts.URL+"/alice/r.git", "."); err != nil {
		t.Fatalf("public clone: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(mallory, "evil.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e2eGit(t, mallory, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := e2eGit(t, mallory, nil, "commit", "-q", "-m", "hijack"); err != nil {
		t.Fatal(err)
	}
	if out, err := e2eGit(t, mallory, e2eAuthHdr("tok-mallory"), "push", "origin", "main"); err == nil {
		t.Fatalf("foreign push must fail\n%s", out)
	}
	// The named denial rides the receive-pack discovery twin (unit-pinned
	// in TestPushGuardDiscoveryMatrix); over the wire here the push
	// itself must simply fail and leave the repo untouched.

	// mallory's own namespace auto-creates fine (self admission).
	mself := t.TempDir()
	e2eSeed(t, mself, "mine.txt", "mine\n", "mine")
	if out, err := e2eGit(t, mself, e2eAuthHdr("tok-mallory"), "push", ts.URL+"/mallory/r.git", "main"); err != nil {
		t.Fatalf("self-namespace push: %v\n%s", err, out)
	}

	// A squat under alice's namespace dies at admission.
	if out, err := e2eGit(t, mself, e2eAuthHdr("tok-mallory"), "push", ts.URL+"/alice/hijack.git", "main"); err == nil {
		t.Fatalf("foreign-namespace auto-create must fail\n%s", out)
	} else if !strings.Contains(out, "not permitted") {
		t.Fatalf("squat denial must cite the admission rule, got:\n%s", out)
	}

	// The repo alice created still serves her content to the stranger
	// (public read alignment — the push refusal changed nothing).
	clone := t.TempDir()
	if out, err := e2eGit(t, clone, e2eAuthHdr("tok-mallory"), "clone", "-q", ts.URL+"/alice/r.git", "."); err != nil {
		t.Fatalf("public clone after refused push: %v\n%s", err, out)
	}
	if blob, err := os.ReadFile(filepath.Join(clone, "hello.txt")); err != nil || string(blob) != "hello\n" {
		t.Fatalf("cloned content = %q err=%v", blob, err)
	}
}

func TestPushGuardEndToEndSSH(t *testing.T) {
	srv, ident := pushGuardStack(t, func(c *config.Config) { c.Server.SSH.Listen = "127.0.0.1:0" })
	ctx := context.Background()
	dir := t.TempDir()
	aliceKey, alicePub := writeClientKey(t, dir, "alice_ed25519")
	malloryKey, malloryPub := writeClientKey(t, dir, "mallory_ed25519")
	observerKey, observerPub := writeClientKey(t, dir, "observer_ed25519")
	keys := srv.SSHKeyRegistry()
	if _, err := keys.Add(ctx, "alice", alicePub, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(ctx, "mallory", malloryPub, "mallory"); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(ctx, "observer", observerPub, "observer"); err != nil {
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
	sshEnv := func(keyPath string) []string {
		return []string{"-c", "core.sshCommand=ssh -i " + keyPath +
			" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes"}
	}

	// alice seeds her own namespace over SSH.
	seed := t.TempDir()
	e2eSeed(t, seed, "hello.txt", "over ssh\n", "first over ssh")
	if out, err := e2eGit(t, seed, sshEnv(aliceKey), "push", base+"/alice/sshr.git", "main"); err != nil {
		t.Fatalf("self ssh push: %v\n%s", err, out)
	}

	// mallory's push to alice's repo dies with the named refusal on stderr.
	mallory := t.TempDir()
	if out, err := e2eGit(t, mallory, sshEnv(malloryKey), "clone", "-q", base+"/alice/sshr.git", "."); err != nil {
		t.Fatalf("public ssh clone: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(mallory, "evil.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e2eGit(t, mallory, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := e2eGit(t, mallory, nil, "commit", "-q", "-m", "hijack"); err != nil {
		t.Fatal(err)
	}
	if out, err := e2eGit(t, mallory, sshEnv(malloryKey), "push", "origin", "main"); err == nil {
		t.Fatalf("foreign ssh push must fail\n%s", out)
	} else if !strings.Contains(out, `write access to "alice/sshr"`) {
		t.Fatalf("ssh denial must name the repo, got:\n%s", out)
	}

	// mallory's own namespace auto-creates over SSH; a squat does not.
	mself := t.TempDir()
	e2eSeed(t, mself, "mine.txt", "mine\n", "mine")
	if out, err := e2eGit(t, mself, sshEnv(malloryKey), "push", base+"/mallory/sshr.git", "main"); err != nil {
		t.Fatalf("ssh self-namespace push: %v\n%s", err, out)
	}
	if out, err := e2eGit(t, mself, sshEnv(malloryKey), "push", base+"/alice/squat.git", "main"); err == nil {
		t.Fatalf("ssh foreign-namespace auto-create must fail\n%s", out)
	} else if !strings.Contains(out, "not permitted") {
		t.Fatalf("ssh squat denial must cite the admission rule, got:\n%s", out)
	}

	// Fetch-side alignment in the same gate area: a private repo refuses
	// a stranger's clone while the owner still reads. Forgejo #374: the
	// owner reads through an explicit binding (private user-owned repos
	// admit owner + bindings + admin only — the host-write flag alone
	// grants nothing, so mallory is denied too); the observer stays the
	// flagless stranger cell.
	privSeed := t.TempDir()
	e2eSeed(t, privSeed, "secret.txt", "secret\n", "secret")
	if out, err := e2eGit(t, privSeed, sshEnv(aliceKey), "push", base+"/alice/priv.git", "main"); err != nil {
		t.Fatalf("private seed push: %v\n%s", err, out)
	}
	markPrivate(t, ident, "alice", "priv")
	grantOwner(t, ident, "alice", "priv", "alice")
	if out, err := e2eGit(t, t.TempDir(), sshEnv(observerKey), "clone", base+"/alice/priv.git", "."); err == nil {
		t.Fatalf("private ssh clone must fail\n%s", out)
	}
	if out, err := e2eGit(t, t.TempDir(), sshEnv(malloryKey), "clone", base+"/alice/priv.git", "."); err == nil {
		t.Fatalf("private ssh clone by host-write outsider must fail\n%s", out)
	}
	if out, err := e2eGit(t, t.TempDir(), sshEnv(observerKey), "clone", "-q", base+"/alice/sshr.git", "."); err != nil {
		t.Fatalf("public ssh clone by stranger: %v\n%s", err, out)
	}
	if out, err := e2eGit(t, t.TempDir(), sshEnv(aliceKey), "clone", "-q", base+"/alice/priv.git", "."); err != nil {
		t.Fatalf("owner ssh clone of private repo: %v\n%s", err, out)
	}
}
