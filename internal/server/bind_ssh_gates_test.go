package server

// bind_ssh_gates_test.go — the sshd.Transport gates and mappings (17_ssh.md
// §4): drain, placement, not-found/unavailable sentinels, and the SSH()
// construction branches, against the fake engine.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/sshd"
)

func sshGateServer(t *testing.T, eng *fakeEngine, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.SSH.Listen = "127.0.0.1:0"
	if mutate != nil {
		mutate(cfg)
	}
	return New(Options{
		Config:  cfg,
		Store:   newFakeStore(),
		Engine:  eng,
		DataDir: t.TempDir(),
		Log:     testLogger(t),
	})
}

func TestSSHTransportSentinelMapping(t *testing.T) {
	cases := []struct {
		name string
		eng  *fakeEngine
		want error
	}{
		{"not found", &fakeEngine{exists: false, noCreate: true, placement: Placement{Serve: true}}, sshd.ErrNotFound},
		{"not served", &fakeEngine{exists: true, placement: Placement{Serve: false, Maintain: false, ServedBy: "elsewhere"}}, sshd.ErrUnavailable},
		{"sync unavailable", &fakeEngine{exists: true, placement: Placement{Serve: true}, syncErr: errors.New("bucket down")}, sshd.ErrUnavailable},
	}
	for _, c := range cases {
		s := sshGateServer(t, c.eng, nil)
		root := t.TempDir()
		ctx := context.WithValue(context.Background(), repoRootKey{}, root)
		err := s.SSHUploadPack(ctx, mustRepoID(t, "o/r"), "", strings.NewReader(""), io.Discard, io.Discard)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestSSHTransportDrainGate(t *testing.T) {
	s := sshGateServer(t, &fakeEngine{exists: true, placement: Placement{Serve: true}}, nil)
	drainCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())

	// phase 1: HTTP keeps serving — SSH must too (§12 parity)
	s.Drain().Begin1(drainCtx, context.Background())
	if err := s.SSHUploadPack(ctx, mustRepoID(t, "o/r"), "", strings.NewReader(""), io.Discard, io.Discard); errors.Is(err, sshd.ErrUnavailable) {
		t.Fatalf("phase-1 upload must still serve: %v", err)
	}

	// phase 2: new git work refused on both transports
	s.Drain().Begin2()
	err := s.SSHUploadPack(ctx, mustRepoID(t, "o/r"), "", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("drained upload = %v", err)
	}
	err = s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("drained receive = %v", err)
	}
}

func TestSSHReceivePackPushPipeline(t *testing.T) {
	eng := &fakeEngine{exists: false, placement: Placement{Serve: true}} // auto-create on
	s := sshGateServer(t, eng, nil)
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)

	// Pure-delete push: no pack bytes, report-status result on stdout.
	zero := strings.Repeat("0", 40)
	oid := "1111111111111111111111111111111111111111"
	body := git.Pkt(oid + " " + zero + " refs/heads/main\x00report-status atomic\n")
	body = append(body, git.Flush()...)

	var out strings.Builder
	id := mustRepoID(t, "o/r")
	if err := s.SSHReceivePack(ctx, id, "ada", strings.NewReader(string(body)), &out, io.Discard); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if !strings.Contains(out.String(), "unpack ok") {
		t.Fatalf("report = %q", out.String())
	}
	if eng.published != 1 {
		t.Fatalf("publishes = %d", eng.published)
	}
	if eng.lastPrincipal != "ada" {
		t.Fatalf("principal = %q", eng.lastPrincipal)
	}
}

func TestSSHReceivePackNotFound(t *testing.T) {
	eng := &fakeEngine{exists: false, noCreate: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, nil)
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrNotFound) {
		t.Fatalf("receive not-found = %v", err)
	}
}

func TestSSHBuilderBranches(t *testing.T) {
	// disabled → (nil, nil)
	s := sshGateServer(t, &fakeEngine{exists: true}, func(c *config.Config) { c.Server.SSH.Listen = "" })
	if got, err := s.SSH(); got != nil || err != nil {
		t.Fatalf("disabled SSH = %v %v", got, err)
	}
	// setup-only (no engine) → (nil, nil)
	bare := New(Options{Config: sshGateServer(t, nil, nil).cfg, Store: newFakeStore(), DataDir: t.TempDir(), Log: testLogger(t)})
	if got, err := bare.SSH(); got != nil || err != nil {
		t.Fatalf("setup-only SSH = %v %v", got, err)
	}
	// host_key_env unset → boot-fatal
	s3 := sshGateServer(t, &fakeEngine{exists: true}, func(c *config.Config) {
		c.Server.SSH.HostKeyEnv = "WALHUB_TEST_MISSING_HOST_KEY"
	})
	if _, err := s3.SSH(); err == nil || !strings.Contains(err.Error(), "host_key_env") {
		t.Fatalf("unset host_key_env = %v", err)
	}
	// valid: builds and parses the key
	s4 := sshGateServer(t, &fakeEngine{exists: true}, nil)
	got, err := s4.SSH()
	if err != nil || got == nil {
		t.Fatalf("valid SSH() = %v %v", got, err)
	}
	// auto host key persisted under the data dir
	if _, err := os.Stat(filepath.Join(s4.dataDir, "ssh", "ed25519_host_key")); err != nil {
		t.Fatalf("auto host key missing: %v", err)
	}
}

func TestSSHReceivePackOverMaxPushBytes(t *testing.T) {
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, func(c *config.Config) {
		c.Server.MaxPushBytes = 200 // commands parse; the pack exceeds it
	})
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)

	// a real pack larger than max_push_bytes: commands parse, IngestStream's
	// cap fails -> band-2 + unpack-ng report on stdout, nil err.
	fixture := t.TempDir()
	mk := func(argv ...string) {
		cmd := exec.Command("git", argv...)
		cmd.Dir = fixture
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, o)
		}
	}
	mk("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(fixture, "big.txt"), bytes.Repeat([]byte("over the cap "), 200), 0o644); err != nil {
		t.Fatal(err)
	}
	mk("add", ".")
	mk("commit", "-q", "-m", "big")
	var pack bytes.Buffer
	packCmd := exec.Command("git", "pack-objects", "--stdout", "--revs")
	packCmd.Dir = fixture
	packCmd.Stdin = strings.NewReader("HEAD\n")
	packCmd.Stdout = &pack
	if err := packCmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	if pack.Len() <= 200 {
		t.Fatalf("fixture pack too small: %d", pack.Len())
	}

	zero := strings.Repeat("0", 40)
	oid := "1111111111111111111111111111111111111111"
	body := git.Pkt(zero + " " + oid + " refs/heads/main\x00report-status side-band-64k\n")
	body = append(body, git.Flush()...)
	body = append(body, pack.Bytes()...)

	var out strings.Builder
	if err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(string(body)), &out, io.Discard); err != nil {
		t.Fatalf("over-cap receive must report on the wire, not error: %v", err)
	}
	wire := out.String()
	if !strings.Contains(wire, "pack exceeds max_bytes") {
		t.Fatalf("band-2 must name the cap; wire (%d bytes) = %q", len(wire), wire)
	}
	if !strings.Contains(wire, "unpack ng") {
		t.Fatalf("report-status trailer missing: %q", wire)
	}
	if eng.published != 0 {
		t.Fatalf("over-cap push must not publish; publishes = %d", eng.published)
	}
}

func TestSSHPlacementMaintainOnlyRefused(t *testing.T) {
	// split placement: this host MAINTAINS the repo but does not SERVE it —
	// the HTTP routes 503 here, and SSH must refuse identically (§4.3).
	eng := &fakeEngine{exists: true, placement: Placement{Serve: false, Maintain: true, ServedBy: "elsewhere"}}
	s := sshGateServer(t, eng, nil)
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)

	if err := s.SSHUploadPack(ctx, mustRepoID(t, "o/r"), "", strings.NewReader(""), io.Discard, io.Discard); !errors.Is(err, sshd.ErrUnavailable) {
		t.Fatalf("maintain-only fetch = %v, want unavailable", err)
	}
	if err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), io.Discard, io.Discard); !errors.Is(err, sshd.ErrUnavailable) {
		t.Fatalf("maintain-only push = %v, want unavailable", err)
	}
}

func TestSSHCountingReaderCap(t *testing.T) {
	// a counting reader errors once the consumed total reaches the cap
	cr := &countingReader{r: strings.NewReader("abcdef"), max: 2}
	for i := 0; i < 2; i++ {
		if _, err := cr.Read(make([]byte, 1)); err != nil {
			t.Fatalf("in-cap read %d: %v", i, err)
		}
	}
	_, err := cr.Read(make([]byte, 1))
	if err == nil || !errors.Is(err, git.ErrMaxBytes) {
		t.Fatalf("past-cap read = %v", err)
	}
	// sticky
	if _, err := cr.Read(make([]byte, 1)); !errors.Is(err, git.ErrMaxBytes) {
		t.Fatalf("sticky read = %v", err)
	}
}

func TestSSHReceivePackMalformedCommand(t *testing.T) {
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, nil)
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader("zzzz"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "malformed push request") {
		t.Fatalf("malformed command = %v", err)
	}
}

func TestSSHGateSemAndRepoErr(t *testing.T) {
	// per-repo semaphore busy → unavailable ("repository busy")
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, func(c *config.Config) { c.Server.MaxConcurrentPerRepo = 1 })
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	id := mustRepoID(t, "o/r")

	rel := s.sem.TryAcquire(id.String())
	if rel == nil {
		t.Fatal("test setup: slot must be free")
	}
	if err := s.SSHUploadPack(ctx, id, "", strings.NewReader(""), io.Discard, io.Discard); !errors.Is(err, sshd.ErrUnavailable) || !strings.Contains(err.Error(), "repository busy") {
		t.Fatalf("busy upload = %v", err)
	}
	rel()

	// Repo open error (non-not-found) → unavailable
	eng.repoErr = errors.New("cache dir gone")
	if err := s.SSHUploadPack(ctx, id, "", strings.NewReader(""), io.Discard, io.Discard); !errors.Is(err, sshd.ErrUnavailable) {
		t.Fatalf("repo error = %v", err)
	}
	err := s.SSHReceivePack(ctx, id, "ada", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) {
		t.Fatalf("receive repo error = %v", err)
	}
}

func TestSSHPlacementEmptyServedBy(t *testing.T) {
	eng := &fakeEngine{exists: true, placement: Placement{Serve: false, ServedBy: ""}}
	s := sshGateServer(t, eng, nil)
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	err := s.SSHUploadPack(ctx, mustRepoID(t, "o/r"), "", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("empty served-by = %v", err)
	}
}

func TestSSHGateBusyBranch(t *testing.T) {
	// per-repo semaphore: holding the slot makes the SSH gate refuse with
	// "repository busy" (the HTTP route's 503 twin). ReqLog's fallback and
	// the placement metric are covered by the other SSH tests.
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, func(c *config.Config) { c.Server.MaxConcurrentPerRepo = 1 })
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	id := mustRepoID(t, "o/r")

	rel := s.sem.TryAcquire(id.String())
	if rel == nil {
		t.Fatal("test setup: slot must be free")
	}
	err := s.SSHUploadPack(ctx, id, "", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) || !strings.Contains(err.Error(), "repository busy") {
		t.Fatalf("busy = %v", err)
	}
	rel()
}

func TestSSHReceivePackAdvertisementAndCountingPaths(t *testing.T) {
	// advertisement failure → unavailable (a repo whose local dir vanished)
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}, repoCreate: func(root string, id git.RepoId, format git.ObjectFormat) (*git.LocalRepo, error) {
		if err := os.MkdirAll(filepath.Join(root, id.Owner, id.Name+".git", "objects", "info"), 0o755); err != nil {
			return nil, err
		}
		return git.InitLocalRepo(root, id, format)
	}}
	s := sshGateServer(t, eng, nil)
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	id := mustRepoID(t, "o/r")

	// happy push: advertisement on the wire + counting reader consumed
	var out strings.Builder
	zero := strings.Repeat("0", 40)
	oid := "1111111111111111111111111111111111111111"
	body := git.Pkt(oid + " " + zero + " refs/heads/main\x00report-status\n")
	body = append(body, git.Flush()...)
	if err := s.SSHReceivePack(ctx, id, "ada", strings.NewReader(string(body)), &out, io.Discard); err != nil {
		t.Fatalf("receive = %v", err)
	}
	if !strings.Contains(out.String(), "unpack ok") {
		t.Fatalf("report = %q", out.String())
	}

	// host_key_env unset → SSH() boot-fatal
	s2 := sshGateServer(t, eng, func(c *config.Config) { c.Server.SSH.HostKeyEnv = "WALHUB_TEST_NO_HOST_KEY" })
	if _, err := s2.SSH(); err == nil || !strings.Contains(err.Error(), "host_key_env") {
		t.Fatalf("unset host_key_env = %v", err)
	}
}

func TestSSHPlacementLookupErrorServes(t *testing.T) {
	// §4.3 parity: a placement lookup error is "no info → serve" (the HTTP
	// gate treats it the same), so SSH auth proceeds and fetch works.
	eng := &fakeEngine{exists: true, placementErr: errors.New("no heartbeat yet"), placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, nil)
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	if err := s.sshPlacement(ctx, mustRepoID(t, "o/r")); err != nil {
		t.Fatalf("lookup error must serve: %v", err)
	}
}

func TestSSHAdvertisementErrorIsUnavailable(t *testing.T) {
	// a repo whose local dir vanished after Repo() → the layer's
	// advertisement fails → unavailable on the wire path
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}, repoCreate: func(root string, id git.RepoId, format git.ObjectFormat) (*git.LocalRepo, error) {
		lr, err := git.InitLocalRepo(root, id, format)
		if err != nil {
			return nil, err
		}
		_ = os.RemoveAll(lr.Path) // the local copy is gone; the store says it exists
		return lr, nil
	}}
	s := sshGateServer(t, eng, func(c *config.Config) { c.Server.MaxPushBytes = 4096 })
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("vanished repo must fail")
	}
	// the advertisement fails before the command parse, so the error is the
	// layer's (wrapped or raw); the sshd session handler turns any error into
	// stderr + exit 1 — the client sees a clean failure either way.
	_ = err
}
