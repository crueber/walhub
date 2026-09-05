// Package egress — shared webhook egress hardening (stdlib only).
//
// Both the operator-configured WAL events bridge (docs/go/09_events.md
// §4.2) and the tenant-configured repo webhooks (docs/features/06_
// notifications.md §5.3) POST caller-influenced URLs carrying an HMAC
// secret, so delivery must not be steerable onto the host's own network.
// Two layers, shared here so the two sinks cannot drift apart:
//
//  1. No redirects. RefuseRedirect refuses every redirect (refuse-all,
//     not same-host-only). A 3xx is a delivery failure: cursor untouched,
//     replay on the next wake-up. Refuse-all because a same-host
//     https→http downgrade would still leak the secret and body in
//     plaintext, and "same host" is DNS-defined anyway. Trade-off, stated
//     plainly: benign redirects also fail — a trailing-slash
//     normalization hop or an http→https upgrade on the same host is a
//     delivery error, not a silent follow. Webhook URLs must be
//     configured in canonical (final, https) form; the failure is
//     recorded on the deliveries ring and retried next pass.
//  2. Delivery-time IP screening with pinned dialing. DialContext
//     resolves the URL host, drops every non-public address, and dials
//     only the survivors — the check and the connect see the same
//     resolution, so there is no resolve-vs-connect (TOCTOU) gap and
//     literal private IPs never even dial.
//
// Loopback (127.0.0.0/8, ::1) is deliberately ALLOWED: dev/CI webhooks
// target localhost and contract tests deliver to httptest loopback
// servers. Everything else non-public is refused — RFC1918, link-local
// (incl. the 169.254.169.254 cloud-metadata address), CGNAT, benchmark
// (198.18/15), TEST-NETs, ULA, multicast, unspecified, reserved.
//
// Stdlib only (net, net/http, net/netip, crypto/tls for the opt-in
// insecure lane); safe for concurrent use (http.Transport is
// goroutine-safe; the dialer carries no mutable state). No goroutines,
// no locks, so no new concurrency hazard.
package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// ErrRedirect fails every redirect: signature and body must never leave
// the validated host.
var ErrRedirect = errors.New("webhook: redirects are not followed")

// RefuseRedirect is the http.Client CheckRedirect policy: refuse all.
func RefuseRedirect(_ *http.Request, _ []*http.Request) error {
	return ErrRedirect
}

// blockedNets are the non-public ranges a webhook delivery must never
// reach. Loopback is NOT listed (allowed by decision above).
var blockedNets = [...]string{
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

var blockedPrefixes = func() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(blockedNets))
	for _, s := range blockedNets {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// BlockedIP reports whether addr must never receive a webhook POST.
// The input is unmapped first so IPv4-mapped IPv6 (e.g. ::ffff:10.0.0.1)
// is judged as its IPv4 self.
func BlockedIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// dialTimeout bounds one screened dial (the request context still bounds
// the whole POST; this keeps a dead IP from stalling the screen).
const dialTimeout = 10 * time.Second

// DialContext resolves host, drops every non-public address, and dials
// only the survivors in resolver order. No allowed address (or a
// resolution failure) fails the delivery closed — the cursor stays
// untouched upstream.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
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
	dialer := &net.Dialer{Timeout: dialTimeout}
	var blocked int
	for _, ip := range ips {
		if BlockedIP(ip) {
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

// Transport returns the shared delivery transport: default settings with
// the dial-time screen pinned in. When insecure is true, TLS verification
// is disabled (the per-hook insecure_tls opt-in); the redirect refusal
// still lives on the http.Client, and the dial screen still applies —
// insecure never means unpinned. Proxy/env behavior is preserved by
// cloning the default transport.
func Transport(insecure bool) *http.Transport {
	var tr *http.Transport
	if dtr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dtr.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.DialContext = DialContext
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return tr
}
