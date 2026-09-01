// sink_test.go — the webhook wire contract (09 §4.1): JSON array batch,
// X-Walgit-Delivery sha1 hex, X-Walgit-Signature sha256 HMAC over the exact
// body bytes, 2xx acks, and the 10 s request-context timeout.
package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func captureWebhook(t *testing.T, status int) (*httptest.Server, *atomic.Pointer[http.Header], *atomic.Pointer[[]byte]) {
	var hdr atomic.Pointer[http.Header]
	var body atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h := r.Header.Clone()
		hdr.Store(&h)
		body.Store(&b)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hdr, &body
}

func twoEvents() []RefEvent {
	return []RefEvent{
		{Action: ActionCreate, RefName: "refs/heads/main", Repo: "o/r",
			Walgit: Walgit{SchemaVersion: 1, Seq: "1", EntryKind: KindPushWire}},
		{Action: ActionUpdate, RefName: "refs/heads/dev", Repo: "o/r",
			Walgit: Walgit{SchemaVersion: 1, Seq: "2", EntryKind: KindPushWire}},
	}
}

func TestWebhookSink_HeadersAndBody(t *testing.T) {
	srv, hdr, body := captureWebhook(t, http.StatusOK)
	sink := &WebhookSink{URL: srv.URL, Secret: "rotate-me-regularly"}

	batch := twoEvents()
	if err := sink.Deliver(context.Background(), "o/r", batch); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	gotBody := *body.Load()
	var got []RefEvent
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("body is not a JSON array: %s (%v)", gotBody, err)
	}
	if len(got) != 2 || got[0].RefName != "refs/heads/main" {
		t.Errorf("body batch = %+v", got)
	}

	h := *hdr.Load()
	if ct := h.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	sum := sha1.Sum(gotBody)
	if d := h.Get("X-Walgit-Delivery"); d != hex.EncodeToString(sum[:]) {
		t.Errorf("X-Walgit-Delivery = %q, want sha1 of body %x", d, sum)
	}
	mac := hmac.New(sha256.New, []byte("rotate-me-regularly"))
	mac.Write(gotBody)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig := h.Get("X-Walgit-Signature"); sig != wantSig {
		t.Errorf("X-Walgit-Signature = %q, want %q", sig, wantSig)
	}
}

func TestWebhookSink_NoSecretNoSignature(t *testing.T) {
	srv, hdr, _ := captureWebhook(t, http.StatusOK)
	sink := &WebhookSink{URL: srv.URL}
	if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if sig := (*hdr.Load()).Get("X-Walgit-Signature"); sig != "" {
		t.Errorf("signature header present without secret: %q", sig)
	}
}

func TestWebhookSink_Acks(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204, 299} {
		srv, _, _ := captureWebhook(t, status)
		sink := &WebhookSink{URL: srv.URL}
		if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err != nil {
			t.Errorf("status %d must ack, got %v", status, err)
		}
	}
	for _, status := range []int{301, 400, 404, 500, 503} {
		srv, _, _ := captureWebhook(t, status)
		sink := &WebhookSink{URL: srv.URL}
		if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err == nil {
			t.Errorf("status %d must not ack", status)
		}
	}
}

func TestWebhookSink_EmptyBatchNoPOST(t *testing.T) {
	srv, _, _ := captureWebhook(t, http.StatusOK)
	sink := &WebhookSink{URL: srv.URL}
	if err := sink.Deliver(context.Background(), "o/r", nil); err != nil {
		t.Fatalf("empty batch must be a no-op, got %v", err)
	}
}

// TestWebhookSink_TimeoutLeavesRequestBound: a slow webhook fails the delivery
// (the cursor stays untouched upstream); the timeout lives on the request
// context, not on http.Client.Timeout.
func TestWebhookSink_TimeoutLeavesRequestBound(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { close(released); _ = srv.Close })

	sink := &WebhookSink{URL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := sink.Deliver(ctx, "o/r", twoEvents())
	if err == nil {
		t.Fatal("timed-out webhook must fail the delivery")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("delivery took %v; the per-request timeout must bound it", elapsed)
	}
	if webhookClient.Timeout != 0 {
		t.Errorf("http.Client.Timeout = %v; must stay unset (09 §4.2)", webhookClient.Timeout)
	}
}
