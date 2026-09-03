package server

// bind_ssh_gates_test.go — the sshd.Transport gates and mappings (17_ssh.md
// §4): drain, placement, not-found/unavailable sentinels, and the SSH()
// construction branches, against the fake engine.

import (
	"context"
	"errors"
	"io"
	"os"
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
	cfg.Server.SSH.Keys = []config.SshKey{{
		Principal: "ada",
		Key:       "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII/VO93dFREgk2CscLYyCKH4ZjDpD8XYGB/X8ReU2QWx ada@laptop",
		Write:     true,
	}}
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
	s.Drain().Begin1(drainCtx, context.Background())

	err := s.SSHUploadPack(context.Background(), mustRepoID(t, "o/r"), "", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("drained upload = %v", err)
	}
	err = s.SSHReceivePack(context.Background(), mustRepoID(t, "o/r"), "ada", strings.NewReader(""), io.Discard, io.Discard)
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
	// key_env unset → boot-fatal
	s2 := sshGateServer(t, &fakeEngine{exists: true}, func(c *config.Config) {
		c.Server.SSH.Keys = []config.SshKey{{Principal: "ada", KeyEnv: "WALHUB_TEST_MISSING_KEY"}}
	})
	if _, err := s2.SSH(); err == nil || !strings.Contains(err.Error(), "no key material") {
		t.Fatalf("unset key_env = %v", err)
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
