// sink.go — the Sink abstraction and the built-in webhook sink (09 §4): one
// JSON array POST per catch-up, sha1 delivery id, optional sha256 HMAC
// signature, 10 s request-context timeout, 2xx acks, at-least-once.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Sink delivers a whole batch of events for one repo. Deliver must ack (return
// nil) only when the batch is durably accepted; any error leaves the cursor
// untouched and replays the range at the next wake-up (at-least-once).
type Sink interface {
	// Name is the metric label, e.g. "webhook".
	Name() string
	Deliver(ctx context.Context, repo string, batch []RefEvent) error
}

// deliverTimeout bounds the entire POST — connect through body read — per
// wake-up. It lives on the request context, NOT on http.Client.Timeout (which
// would also cap connection reuse).
const deliverTimeout = 10 * time.Second

// webhookClient is the one client per process; the default transport is fine.
var webhookClient = &http.Client{} //nolint:exhaustruct // http.Client.Timeout deliberately unset (09 §4.2)

// WebhookSink POSTs the batch as one JSON array to events.webhook_url.
type WebhookSink struct {
	URL    string
	Secret string // optional; enables X-Walgit-Signature
}

// Name implements Sink.
func (s *WebhookSink) Name() string { return "webhook" }

// Deliver implements Sink (09 §4.1, normative wire contract).
func (s *WebhookSink) Deliver(ctx context.Context, repo string, batch []RefEvent) error {
	if len(batch) == 0 {
		return nil
	}
	body, err := json.Marshal(batch) // slice is non-empty → always a JSON array, never "null"
	if err != nil {
		return fmt.Errorf("webhook: encode batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	sum := sha1.Sum(body)
	req.Header.Set("X-Walgit-Delivery", hex.EncodeToString(sum[:]))
	if s.Secret != "" {
		mac := hmac.New(sha256.New, []byte(s.Secret))
		mac.Write(body)
		req.Header.Set("X-Walgit-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	dctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	resp, err := webhookClient.Do(req.WithContext(dctx))
	if err != nil {
		return fmt.Errorf("webhook: POST %s: %w", s.URL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: POST %s: status %d", s.URL, resp.StatusCode)
	}
	return nil
}
