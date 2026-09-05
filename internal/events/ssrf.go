// ssrf.go — webhook egress hardening (09 §4.2): the events bridge POSTs
// operator-configured URLs with an HMAC secret, so delivery must not be
// steerable onto the host's own network. Two layers, stdlib only:
//
//  1. No redirects. webhookClient refuses every redirect (CheckRedirect
//     always fails). A 3xx is a delivery failure: cursor untouched, replay
//     on the next wake-up. Refuse-all (not same-host-only) because a
//     same-host https→http downgrade would still leak the secret and body
//     in plaintext, and "same host" is DNS-defined anyway.
//  2. Delivery-time IP screening with pinned dialing. The transport's
//     DialContext resolves the URL host, drops every non-public address,
//     and dials only the survivors — the check and the connect see the
//     same resolution, so there is no resolve-vs-connect (TOCTOU) gap and
//     literal private IPs never even dial.
//
// Loopback (127.0.0.0/8, ::1) is deliberately ALLOWED: the webhook URL is
// operator config (not tenant input), dev/CI webhooks target localhost,
// and the package's own contract tests deliver to httptest loopback
// servers. Everything else non-public is refused — RFC1918, link-local
// (incl. the 169.254.169.254 cloud-metadata address), CGNAT, benchmark
// (198.18/15), TEST-NETs, ULA, multicast, unspecified, reserved.
package events

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// errWebhookRedirect fails every redirect: signature and body must never
// leave the validated host.
var errWebhookRedirect = errors.New("webhook: redirects are not followed")

// webhookRefuseRedirect is the http.Client CheckRedirect policy: refuse all.
func webhookRefuseRedirect(_ *http.Request, _ []*http.Request) error {
	return errWebhookRedirect
}

// webhookBlockedNets are the non-public ranges a webhook delivery must
// never reach. Loopback is NOT listed (allowed by decision above).
var webhookBlockedNets = [...]string{
	"0.0.0.0/8",       // this network
	"10.0.0.0/8",      // RFC1918
	"172.16.0.0/12",   // RFC1918
	"192.168.0.0/16",  // RFC1918
	"100.64.0.0/10",   // CGNAT
	"169.254.0.0/16",  // link-local (cloud metadata lives here)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"198.18.0.0/15",   // benchmark testing
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved
	"::/128",          // unspecified
	"fc00::/7",        // unique-local
	"fe80::/10",       // link-local
	"ff00::/8",        // multicast
	"2001:db8::/32",   // documentation
}

var webhookBlockedPrefixes = func() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(webhookBlockedNets))
	for _, s := range webhookBlockedNets {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// webhookBlockedIP reports whether addr must never receive a webhook POST.
// The input is unmapped first so IPv4-mapped IPv6 (e.g. ::ffff:10.0.0.1)
// is judged as its IPv4 self.
func webhookBlockedIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range webhookBlockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// webhookDialContext resolves host, drops every non-public address, and
// dials only the survivors in resolver order. No allowed address (or a
// resolution failure) fails the delivery closed — the cursor stays
// untouched upstream.
func webhookDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("webhook: bad dial address %q: %w", addr, err)
	}
	var ips []netip.Addr
	if lit, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{lit}
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("webhook: cannot resolve %q: %w", host, err)
		}
		ips = resolved
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var blocked int
	for _, ip := range ips {
		if webhookBlockedIP(ip) {
			blocked++
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("webhook: dial %q: %w", host, ctx.Err())
		}
	}
	if blocked > 0 && blocked == len(ips) {
		return nil, fmt.Errorf("webhook: host %q resolves to a non-public address (refused)", host)
	}
	return nil, fmt.Errorf("webhook: host %q has no reachable allowed address", host)
}

// webhookTransport is the shared transport: default settings with the
// dial-time SSRF screen pinned in. Safe for concurrent use (http.Transport
// is goroutine-safe; the dialer carries no mutable state).
func webhookTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = webhookDialContext
	return tr
}
