// webhooks_ssrf_test.go — tenant webhook SSRF regression (issue #78):
// a validated hook URL must not be steerable onto private/loopback
// targets via redirects, and the HMAC secret + body must never cross
// hosts. Loopback stays deliverable (dev rule).
package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// seedOneEvent reserves one activity seq and writes a minimal commented
// event; returns the seq.
func seedOneEvent(t *testing.T, x *harness) int {
	t.Helper()
	seq, err := x.svc.reserveSeq(ctx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", Kind: "issue", At: x.now.Format(dateTimeFmt)}
	if err := x.svc.putCreate(ctx(), ActivityKey("acme", "repo", seq), mustEncode(t, ev)); err != nil {
		t.Fatal(err)
	}
	return seq
}

// redirectTrap builds an origin that redirects (code) to a target that
// records every arrival (hits, signature, body length). Both are loopback
// httptest servers, so the dial screen lets the origin through and the
// redirect policy is the only thing standing between the secret and the
// target.
type redirectTrap struct {
	origin *httptest.Server
	target *httptest.Server
	hits   *atomic.Int64
	sig    *atomic.Value // string, set on arrival
	body   *atomic.Int64
}

func newRedirectTrap(t *testing.T, code int) *redirectTrap {
	t.Helper()
	tr := &redirectTrap{hits: &atomic.Int64{}, sig: &atomic.Value{}, body: &atomic.Int64{}}
	tr.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		tr.body.Add(int64(len(b)))
		tr.sig.Store(r.Header.Get("X-Walgit-Signature"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tr.target.Close)
	tr.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tr.target.URL+"/hook", code)
	}))
	t.Cleanup(tr.origin.Close)
	return tr
}

// TestWebhookNoRedirectCrossHost302: a 302 off the validated host fails
// the delivery and the target sees nothing — no hit, no signature, no body.
func TestWebhookNoRedirectCrossHost302(t *testing.T) {
	x := newHarness(t)
	tr := newRedirectTrap(t, http.StatusFound)

	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(tr.origin.URL), Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	seedOneEvent(t, x)
	x.svc.DeliverRepo(ctx(), "acme", "repo")

	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 0 {
		t.Fatalf("redirected cursor advanced to %d; must stay (retry next pass)", cur)
	}
	if n := tr.hits.Load(); n != 0 {
		t.Fatalf("redirect target hit %d times; signature/body must never leave the validated host", n)
	}
	if v := tr.sig.Load(); v != nil && v.(string) != "" {
		t.Fatalf("signature leaked cross-host: %q", v.(string))
	}
	d := x.svc.ReadDeliveries(ctx(), "acme", "repo", hk.ID)
	if len(d.Entries) != 1 {
		t.Fatalf("deliveries = %+v, want one failure row", d.Entries)
	}
	if d.Entries[0].Status != 0 || !strings.Contains(d.Entries[0].Error, "redirects are not followed") {
		t.Fatalf("failure row = %+v, want redirect refusal", d.Entries[0])
	}
}

// TestWebhookNoRedirectBodyPreserving: 307/308 preserve method+body by
// design, so refusing them (not just 302) is what keeps the event body
// on the validated host.
func TestWebhookNoRedirectBodyPreserving(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			x := newHarness(t)
			tr := newRedirectTrap(t, code)

			hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
				URL: strPtr(tr.origin.URL), Secret: strPtr("s3cr3t"),
			})
			if err != nil {
				t.Fatal(err)
			}
			seedOneEvent(t, x)
			x.svc.DeliverRepo(ctx(), "acme", "repo")

			if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 0 {
				t.Fatalf("code %d: cursor advanced to %d; must stay", code, cur)
			}
			if n := tr.hits.Load(); n != 0 {
				t.Fatalf("code %d: target hit %d times; body must never cross hosts", code, n)
			}
			if n := tr.body.Load(); n != 0 {
				t.Fatalf("code %d: target received %d body bytes", code, n)
			}
		})
	}
}

// TestWebhookPrivateLiteralRefused: https passes validation for any host
// (the scheme rule), so the delivery-time dial screen is what refuses
// private literals — fast, before any SYN, cursor held. Covers
// RFC1918 + link-local + benchmark + TEST-NET + ULA + mapped-v6.
func TestWebhookPrivateLiteralRefused(t *testing.T) {
	urls := []string{
		"https://10.0.0.1/hook",
		"https://172.16.5.4:8443/hook",
		"https://192.168.1.1/hook",
		"https://169.254.169.254/latest/meta-data/",
		"https://100.64.0.1/hook",
		"https://198.18.0.1/hook",
		"https://192.0.2.1/hook",
		"https://198.51.100.1/hook",
		"https://203.0.113.1/hook",
		"https://[fd00::1]/hook",
		"https://[fe80::1]/hook",
		"https://[::ffff:10.0.0.1]/hook",
		"https://[::ffff:169.254.169.254]/hook",
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			x := newHarness(t)
			hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
				URL: strPtr(raw), Secret: strPtr("s3cr3t"),
			})
			if err != nil {
				t.Fatalf("https private literal must pass validation (screen is at delivery): %v", err)
			}
			seedOneEvent(t, x)
			x.svc.DeliverRepo(ctx(), "acme", "repo")
			if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 0 {
				t.Fatalf("private literal %q delivered (cursor %d); must be refused", raw, cur)
			}
			d := x.svc.ReadDeliveries(ctx(), "acme", "repo", hk.ID)
			if len(d.Entries) != 1 {
				t.Fatalf("deliveries = %+v, want one failure row", d.Entries)
			}
			if msg := d.Entries[0].Error; !strings.Contains(msg, "refused") && !strings.Contains(msg, "no reachable allowed") {
				t.Errorf("delivery error should name the refusal, got: %q", msg)
			}
		})
	}
}

// TestWebhookLoopbackDelivers: the screen must not break the legitimate
// dev path — http loopback (allowed by validateHookURL) still delivers
// with the signature intact.
func TestWebhookLoopbackDelivers(t *testing.T) {
	x := newHarness(t)
	sink, srv := newSink(t, "s3cr3t")
	defer srv.Close()

	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	seq := seedOneEvent(t, x)
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != seq {
		t.Fatalf("loopback cursor = %d, want %d", cur, seq)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("loopback sink posts = %d, want 1", got)
	}
}
