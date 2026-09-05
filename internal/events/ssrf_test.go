// ssrf_test.go — webhook SSRF hardening (09 §4.2): redirects never
// followed (signature/body stay on the validated host), non-public IPs
// refused at delivery time, loopback still deliverable (dev/test rule).
package events

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookBlockedIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// Loopback is allowed (operator config + dev/test rule).
		{name: "loopback v4", ip: "127.0.0.1", blocked: false},
		{name: "loopback v4 full /8", ip: "127.0.0.2", blocked: false},
		{name: "loopback v6", ip: "::1", blocked: false},
		{name: "localhost maps to loopback, judged by resolution not literal", ip: "127.0.0.1", blocked: false},

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
		{name: "mapped public", ip: "::ffff:8.8.8.8", blocked: false},

		// Plain public.
		{name: "public v4", ip: "8.8.8.8", blocked: false},
		{name: "public v4 cloudflare", ip: "1.1.1.1", blocked: false},
		{name: "public v6", ip: "2606:4700:4700::1111", blocked: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.ip)
			if got := webhookBlockedIP(addr); got != tc.blocked {
				t.Errorf("webhookBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

func TestWebhookSink_PrivateLiteralRefused(t *testing.T) {
	urls := []string{
		"http://10.0.0.1/hook",
		"http://172.16.5.4:8080/hook",
		"http://192.168.1.1/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/hook",
		"https://198.18.0.1/hook",
		"http://192.0.2.1/hook",
		"http://[fd00::1]/hook",
		"http://[fe80::1]/hook",
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			sink := &WebhookSink{URL: raw, Secret: "s3cr3t"}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			err := sink.Deliver(ctx, "o/r", twoEvents())
			if err == nil {
				t.Fatalf("Deliver to %q must fail (non-public literal)", raw)
			}
			if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "no reachable allowed") {
				t.Errorf("Deliver to %q error should name the refusal, got: %v", raw, err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("refusal of %q took %v; must fail without dialing", raw, elapsed)
			}
		})
	}
}

// TestWebhookSink_NoRedirectCrossHost: a validated hook that 302s elsewhere
// must fail delivery AND the redirect target must see nothing — no signature,
// no body.
func TestWebhookSink_NoRedirectCrossHost(t *testing.T) {
	var targetHits atomic.Int64
	var targetSig atomic.Pointer[string]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		s := r.Header.Get("X-Walgit-Signature")
		targetSig.Store(&s)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/hook", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	sink := &WebhookSink{URL: origin.URL, Secret: "s3cr3t"}
	if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err == nil {
		t.Fatal("cross-host redirect must fail the delivery")
	} else if !strings.Contains(err.Error(), "redirects are not followed") {
		t.Fatalf("error should name the redirect refusal, got: %v", err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target hit %d times; signature/body must never leave the validated host", n)
	}
}

// TestWebhookSink_RedirectToLinkLocalRefused: validated https→link-local
// hop is refused at the redirect layer (before any dial screening).
func TestWebhookSink_RedirectToLinkLocalRefused(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	sink := &WebhookSink{URL: origin.URL, Secret: "s3cr3t"}
	if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err == nil {
		t.Fatal("redirect to link-local must fail the delivery")
	}
}

// TestWebhookSink_SameHostRedirectRefused: the policy is refuse-all, not
// same-host-only (a same-host https→http downgrade would leak secret+body
// in plaintext).
func TestWebhookSink_SameHostRedirectRefused(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sink := &WebhookSink{URL: srv.URL + "/hook", Secret: "s3cr3t"}
	if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err == nil {
		t.Fatal("same-host redirect must fail the delivery (refuse-all policy)")
	}
}

// TestWebhookDialContext_LoopbackDials: the screen must not break the
// legitimate dev/test path — loopback listeners stay reachable.
func TestWebhookDialContext_LoopbackDials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := webhookDialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback dial must succeed, got: %v", err)
	}
	conn.Close()
}

// TestWebhookDialContext_PrivateLiteralRefused: the dial layer refuses
// before any SYN — fast, no network touched.
func TestWebhookDialContext_PrivateLiteralRefused(t *testing.T) {
	for _, addr := range []string{"10.0.0.1:80", "169.254.169.254:80", "[fd00::1]:80"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := webhookDialContext(ctx, "tcp", addr)
		cancel()
		if err == nil {
			t.Errorf("dial %s must be refused", addr)
		} else if !strings.Contains(err.Error(), "refused") {
			t.Errorf("dial %s error should name the refusal, got: %v", addr, err)
		}
	}
}

// TestWebhookDialContext_UnresolvableFailsClosed: DNS failure is a delivery
// error, never a bypass.
func TestWebhookDialContext_UnresolvableFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := webhookDialContext(ctx, "tcp", "nonexistent.invalid:80")
	if err == nil {
		t.Fatal("unresolvable host must fail closed")
	}
}
