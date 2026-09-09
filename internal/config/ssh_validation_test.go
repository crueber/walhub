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

func TestCheckSSHExternalPortRange(t *testing.T) {
	for _, port := range []int{-1, 65536, 100000} {
		c := Defaults()
		c.Server.SSH.Listen = "0.0.0.0:2222"
		c.Server.SSH.ExternalPort = port
		_, errs := Validate(c)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "external_port") {
			t.Fatalf("external_port=%d = %v, want one external_port error", port, errs)
		}
	}
	for _, port := range []int{0, 1, 22, 2222, 12222, 65535} {
		c := Defaults()
		c.Server.SSH.Listen = "0.0.0.0:2222"
		c.Server.SSH.ExternalPort = port
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("external_port=%d = %v, want clean", port, errs)
		}
	}
}

// TestAdvertisedSSHPort (issue #215): external_port wins when set, else the
// listen port; 0 when there is nothing to advertise.
func TestAdvertisedSSHPort(t *testing.T) {
	cases := []struct {
		name     string
		listen   string
		external int
		want     int
	}{
		{"disabled", "", 0, 0},
		{"listen only", "0.0.0.0:2222", 0, 2222},
		{"external wins", "0.0.0.0:2222", 12222, 12222},
		{"external without listen", "", 12222, 12222},
		{"unparseable listen", "2222", 0, 0},
		{"out of range external", "0.0.0.0:2222", 70000, 0},
		{"ipv6 listen", "[::1]:2222", 0, 2222},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ServerSSH{Listen: tc.listen, ExternalPort: tc.external}
			if got := s.AdvertisedSSHPort(); got != tc.want {
				t.Fatalf("AdvertisedSSHPort(%q, %d) = %d, want %d",
					tc.listen, tc.external, got, tc.want)
			}
		})
	}
}
