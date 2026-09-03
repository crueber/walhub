package config

// ssh_validation_test.go — checkSSH branches (17_ssh.md §3): the transport is
// disabled by default; every key line must parse, carry a principal, and
// resolve from exactly one of key/key_env; fingerprints are unique.

import (
	"strings"
	"testing"
)

const validTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII/VO93dFREgk2CscLYyCKH4ZjDpD8XYGB/X8ReU2QWx ada@laptop"

func sshCfg(listen string, keys []SshKey) *Config {
	c := Defaults()
	c.Server.SSH.Listen = listen
	c.Server.SSH.Keys = keys
	return c
}

func sshErrs(t *testing.T, c *Config) []string {
	t.Helper()
	_, errs := Validate(c)
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		if strings.HasPrefix(e.Error(), "server.ssh") || strings.Contains(e.Error(), "server.ssh") {
			out = append(out, e.Error())
		}
	}
	return out
}

func TestCheckSSHDisabledByDefault(t *testing.T) {
	if errs := sshErrs(t, Defaults()); len(errs) != 0 {
		t.Fatalf("defaults must not trip ssh validation: %v", errs)
	}
}

func TestCheckSSHListenFormat(t *testing.T) {
	if errs := sshErrs(t, sshCfg("2222", nil)); len(errs) != 1 || !strings.Contains(errs[0], "host:port") {
		t.Fatalf("port-only listen = %v", errs)
	}
	if errs := sshErrs(t, sshCfg("0.0.0.0:2222", nil)); len(errs) != 0 {
		t.Fatalf("valid listen = %v", errs)
	}
}

func TestCheckSSHKeyRules(t *testing.T) {
	cases := []struct {
		name string
		key  SshKey
		want string
	}{
		{"no principal", SshKey{Key: validTestKey}, "principal is required"},
		{"both key and env", SshKey{Principal: "a", Key: validTestKey, KeyEnv: "K"}, "exactly one"},
		{"neither key nor env", SshKey{Principal: "a"}, "exactly one"},
		{"bad key line", SshKey{Principal: "a", Key: "not-a-key"}, "authorized_keys"},
		{"env unset", SshKey{Principal: "a", KeyEnv: "WALHUB_TEST_UNSET_KEY_ENV"}, "is not set"},
	}
	for _, c := range cases {
		if errs := sshErrs(t, sshCfg("127.0.0.1:2222", []SshKey{c.key})); len(errs) != 1 || !strings.Contains(errs[0], c.want) {
			t.Fatalf("%s: errs = %v, want %q", c.name, errs, c.want)
		}
	}
}

func TestCheckSSHValidAndDuplicate(t *testing.T) {
	k1 := SshKey{Principal: "ada", Key: validTestKey, Write: true}
	if errs := sshErrs(t, sshCfg("127.0.0.1:2222", []SshKey{k1})); len(errs) != 0 {
		t.Fatalf("valid key rejected: %v", errs)
	}
	dup := SshKey{Principal: "bob", Key: validTestKey}
	if errs := sshErrs(t, sshCfg("127.0.0.1:2222", []SshKey{k1, dup})); len(errs) != 1 || !strings.Contains(errs[0], "duplicate key") {
		t.Fatalf("duplicate fingerprint = %v", errs)
	}
	// key_env resolution: set the env, the same key validates.
	t.Setenv("WALHUB_TEST_KEY_ENV", validTestKey)
	k2 := SshKey{Principal: "ada2", KeyEnv: "WALHUB_TEST_KEY_ENV", Write: true}
	if errs := sshErrs(t, sshCfg("127.0.0.1:2222", []SshKey{k2})); len(errs) != 0 {
		t.Fatalf("key_env resolution = %v", errs)
	}
}
