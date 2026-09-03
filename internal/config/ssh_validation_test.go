package config

// ssh_validation_test.go — checkSSH (17_ssh.md §3): the transport is
// disabled by default; the listener shape is validated; user keys are not
// config (they live in the object store).

import (
	"strings"
	"testing"
)

func TestCheckSSHDisabledByDefault(t *testing.T) {
	if _, errs := Validate(Defaults()); len(errs) != 0 {
		t.Fatalf("defaults must not trip ssh validation: %v", errs)
	}
}

func TestCheckSSHListenFormat(t *testing.T) {
	c := Defaults()
	c.Server.SSH.Listen = "2222" // no host
	_, errs := Validate(c)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "host:port") {
		t.Fatalf("port-only listen = %v", errs)
	}
	c.Server.SSH.Listen = "0.0.0.0:2222"
	if _, errs = Validate(c); len(errs) != 0 {
		t.Fatalf("valid listen = %v", errs)
	}
	// host key path set without listen still validates (the transport stays
	// disabled; the host key is simply unused)
	c.Server.SSH.Listen = ""
	c.Server.SSH.HostKey = "/tmp/unused"
	if _, errs = Validate(c); len(errs) != 0 {
		t.Fatalf("host key without listen = %v", errs)
	}
}
