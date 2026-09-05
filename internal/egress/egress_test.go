// egress_test.go — shared webhook egress screen: refuse-all redirects,
// non-public IP refusal (incl. benchmark + TEST-NETs + mapped-v6),
// loopback still dialable.
package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestRefuseRedirect(t *testing.T) {
	if err := RefuseRedirect(nil, nil); !errors.Is(err, ErrRedirect) {
		t.Fatalf("RefuseRedirect = %v, want ErrRedirect", err)
	}
	if !strings.Contains(ErrRedirect.Error(), "redirects are not followed") {
		t.Fatalf("ErrRedirect = %q, must name the refusal", ErrRedirect.Error())
	}
}

func TestBlockedIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// Loopback is allowed (dev/test rule).
		{name: "loopback v4", ip: "127.0.0.1", blocked: false},
		{name: "loopback v4 full /8", ip: "127.0.0.2", blocked: false},
		{name: "loopback v6", ip: "::1", blocked: false},

		// RFC1918.
		{name: "rfc1918 10/8", ip: "10.0.0.1", blocked: true},
		{name: "rfc1918 172.16/12 low", ip: "172.16.0.1", blocked: true},
		{name: "rfc1918 172.16/12 high", ip: "172.31.255.255", blocked: true},
		{name: "rfc1918 192.168/16", ip: "192.168.1.1", blocked: true},
		{name: "public next to 172.16/12", ip: "172.15.0.1", blocked: false},
		{name: "public next to 172.16/12 high", ip: "172.32.0.1", blocked: false},

		// Link-local + cloud metadata.
		{name: "metadata service", ip: "169.254.169.254", blocked: true},
		{name: "link-local v4", ip: "169.254.10.20", blocked: true},
		{name: "link-local v6", ip: "fe80::1", blocked: true},

		// CGNAT, benchmark, TEST-NETs, special v4.
		{name: "cgnat low", ip: "100.64.0.1", blocked: true},
		{name: "cgnat high", ip: "100.127.255.255", blocked: true},
		{name: "past cgnat", ip: "100.128.0.1", blocked: false},
		{name: "benchmark 198.18/15 low", ip: "198.18.0.1", blocked: true},
		{name: "benchmark 198.18/15 high", ip: "198.19.255.255", blocked: true},
		{name: "past benchmark", ip: "198.20.0.1", blocked: false},
		{name: "test-net-1", ip: "192.0.2.1", blocked: true},
		{name: "test-net-2", ip: "198.51.100.1", blocked: true},
		{name: "test-net-3", ip: "203.0.113.1", blocked: true},
		{name: "this-network 0/8", ip: "0.1.2.3", blocked: true},
		{name: "ietf assignments", ip: "192.0.0.170", blocked: true},
		{name: "multicast v4", ip: "224.0.0.1", blocked: true},
		{name: "reserved 240/4", ip: "240.0.0.1", blocked: true},

		// IPv6 specials.
		{name: "unspecified v6", ip: "::", blocked: true},
		{name: "ula", ip: "fc00::1", blocked: true},
		{name: "ula fd", ip: "fd00::1", blocked: true},
		{name: "multicast v6", ip: "ff02::1", blocked: true},
		{name: "documentation v6", ip: "2001:db8::1", blocked: true},

		// IPv4-mapped IPv6 must be judged as IPv4.
		{name: "mapped private", ip: "::ffff:10.0.0.1", blocked: true},
		{name: "mapped metadata", ip: "::ffff:169.254.169.254", blocked: true},
		{name: "mapped benchmark", ip: "::ffff:198.18.0.1", blocked: true},
		{name: "mapped test-net-1", ip: "::ffff:192.0.2.1", blocked: true},
		{name: "mapped test-net-2", ip: "::ffff:198.51.100.1", blocked: true},
		{name: "mapped test-net-3", ip: "::ffff:203.0.113.1", blocked: true},
		{name: "mapped loopback", ip: "::ffff:127.0.0.1", blocked: false},
		{name: "mapped public", ip: "::ffff:8.8.8.8", blocked: false},

		// Plain public.
		{name: "public v4", ip: "8.8.8.8", blocked: false},
		{name: "public v4 cloudflare", ip: "1.1.1.1", blocked: false},
		{name: "public v6", ip: "2606:4700:4700::1111", blocked: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.ip)
			if got := BlockedIP(addr); got != tc.blocked {
				t.Errorf("BlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

func TestDialContextLoopbackDials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback dial must succeed, got: %v", err)
	}
	conn.Close()
}

func TestDialContextPrivateLiteralRefused(t *testing.T) {
	for _, addr := range []string{"10.0.0.1:80", "169.254.169.254:80", "198.18.0.1:443", "192.0.2.1:80", "[fd00::1]:80", "[::ffff:10.0.0.1]:80"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := DialContext(ctx, "tcp", addr)
		cancel()
		if err == nil {
			t.Errorf("dial %s must be refused", addr)
		} else if !strings.Contains(err.Error(), "refused") {
			t.Errorf("dial %s error should name the refusal, got: %v", addr, err)
		}
	}
}

func TestDialContextBadAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialContext(ctx, "tcp", "not-an-address"); err == nil {
		t.Fatal("bad dial address must fail")
	} else if !strings.Contains(err.Error(), "bad dial address") {
		t.Fatalf("error should name the bad address, got: %v", err)
	}
}

func TestDialContextUnresolvableFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := DialContext(ctx, "tcp", "nonexistent.invalid:80"); err == nil {
		t.Fatal("unresolvable host must fail closed")
	}
}

func TestDialContextNoReachableAllowed(t *testing.T) {
	// Loopback is allowed but nothing listens here: every dial fails
	// without the host being blocked → "no reachable allowed address".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialContext(ctx, "tcp", addr); err == nil {
		t.Fatal("closed loopback port must fail")
	} else if !strings.Contains(err.Error(), "no reachable allowed") {
		t.Fatalf("error should name the unreachable screen, got: %v", err)
	}
}

func TestDialContextCanceledContext(t *testing.T) {
	// A canceled context surfaces the context error instead of the
	// unreachable screen.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialContext(ctx, "tcp", addr); err == nil {
		t.Fatal("canceled context must fail")
	}
}

func TestTransport(t *testing.T) {
	plain := Transport(false)
	if plain.DialContext == nil {
		t.Fatal("Transport must pin DialContext")
	}
	if plain.TLSClientConfig != nil && plain.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("secure Transport must verify TLS")
	}
	insecure := Transport(true)
	if insecure.DialContext == nil {
		t.Fatal("insecure Transport must pin DialContext")
	}
	if insecure.TLSClientConfig == nil || !insecure.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecure Transport must skip TLS verify")
	}
}

// stubRoundTripper stands in for http.DefaultTransport to cover the
// non-standard-transport fallback in Transport.
type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("stub")
}

func TestTransportFallback(t *testing.T) {
	prev := http.DefaultTransport
	http.DefaultTransport = stubRoundTripper{}
	defer func() { http.DefaultTransport = prev }()
	tr := Transport(true)
	if tr.DialContext == nil {
		t.Fatal("fallback Transport must pin DialContext")
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("fallback insecure Transport must skip TLS verify")
	}
}
