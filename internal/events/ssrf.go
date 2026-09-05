// ssrf.go — webhook egress hardening (09 §4.2): the events bridge POSTs
// operator-configured URLs with an HMAC secret, so delivery must not be
// steerable onto the host's own network. Two layers, stdlib only,
// shared with the tenant webhook path via internal/egress (one range
// table, one dial screen — the two sinks cannot drift apart):
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
//
// The names below are thin wrappers over internal/egress (kept so the
// 09 §4.2 history and existing call sites read unchanged); the range
// table and dial logic live in egress and are tested there.
package events

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"git.packden.us/crueber/walhub/internal/egress"
)

// errWebhookRedirect fails every redirect: signature and body must never
// leave the validated host.
var errWebhookRedirect = egress.ErrRedirect

// webhookRefuseRedirect is the http.Client CheckRedirect policy: refuse all.
func webhookRefuseRedirect(req *http.Request, via []*http.Request) error {
	return egress.RefuseRedirect(req, via)
}

// webhookBlockedIP reports whether addr must never receive a webhook POST.
// The input is unmapped first so IPv4-mapped IPv6 (e.g. ::ffff:10.0.0.1)
// is judged as its IPv4 self.
func webhookBlockedIP(addr netip.Addr) bool {
	return egress.BlockedIP(addr)
}

// webhookDialContext resolves host, drops every non-public address, and
// dials only the survivors in resolver order. No allowed address (or a
// resolution failure) fails the delivery closed — the cursor stays
// untouched upstream.
func webhookDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return egress.DialContext(ctx, network, addr)
}

// webhookTransport is the shared transport: default settings with the
// dial-time SSRF screen pinned in. Safe for concurrent use (http.Transport
// is goroutine-safe; the dialer carries no mutable state).
func webhookTransport() *http.Transport {
	return egress.Transport(false)
}
