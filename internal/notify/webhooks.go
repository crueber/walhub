// webhooks.go — repo webhooks (§1.4/§5.3): config CRUD, the per-hook
// delivery loop, and ping.
//
// v1 webhooks are repo-level configs delivered from the collaboration
// activity log, NOT from the WAL bridge (which stays git-only, P1 law).
// One loop pass per repo: for each active hook, scan collab-events/ from
// the hook's cursor, POST each matching event, CAS-advance the cursor.
// Per hook, delivery is sequential; hooks on one repo run in parallel
// under the fan-out cap. A slow webhook holds back only its own cursor
// (per-sink isolation, 14 §14.6).
//
// Wire shape (09_events §12.2 keepers intact): one POST per event, JSON
// body = the activity event, headers Content-Type: application/json,
// X-Walgit-Delivery: <hex(sha256(body+seq))>, X-Walgit-Signature:
// sha256=<hex HMAC-SHA256(body, secret)> (omitted when no secret),
// X-Walgit-Event: <action>. 10 s timeout; 2xx = delivered; anything else
// = cursor not advanced (at-least-once; consumers dedup on
// X-Walgit-Delivery). Webhook secrets are never logged.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/egress"
	"git.packden.us/crueber/walhub/internal/store"
)

// activityActions is the §5.3 action enum (+ "*" wildcard for filters).
var activityActions = map[string]bool{
	ActionCommented: true, ActionOpened: true, ActionClosed: true,
	ActionReopened: true, ActionAssigned: true, ActionReviewRequested: true,
	ActionReviewPosted: true, ActionCheckReported: true,
	ActionReleasePublished: true, ActionMentioned: true, ActionPing: true,
}

// --- hook ids (ULID, lowercase per §1.4) -----------------------------------------

// newHookID mints a 24-char lowercase ULID (time-ordered; lexical sort =
// creation order): 48-bit millis + 80-bit randomness, Crockford base32
// lowercased.
func newHookID(now time.Time, rnd io.Reader) string {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	var b [10]byte
	_, _ = io.ReadFull(rnd, b[:])
	ms := uint64(now.UnixMilli())
	var out [24]byte
	v := ms
	for i := 9; i >= 0; i-- {
		out[i] = alphabet[v&0x1f]
		v >>= 5
	}
	// 80 random bits across chars 10..23 (16 chars × 5 bits).
	acc := uint(0)
	bits := uint(0)
	j := 0
	for i := 10; i < 24; i++ {
		for bits < 5 {
			acc = (acc << 8) | uint(b[j%len(b)])
			j++
			bits += 8
		}
		bits -= 5
		out[i] = alphabet[(acc>>bits)&0x1f]
		acc &= (1 << bits) - 1
	}
	return string(out[:])
}

// --- validation -------------------------------------------------------------------

// validateHookURL enforces https-only (http allowed on loopback for dev).
func validateHookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: bad webhook url", ErrInvalid)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%w: webhook url must be https (http only on loopback)", ErrInvalid)
	default:
		return fmt.Errorf("%w: bad webhook url scheme", ErrInvalid)
	}
}

// validateHookEvents enforces the action filter ([] = all; "*" wildcard).
func validateHookEvents(events []string) error {
	for _, e := range events {
		if e == "*" {
			continue
		}
		if !activityActions[e] {
			return fmt.Errorf("%w: unknown webhook event %q", ErrInvalid, e)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// hookMatches reports whether the events filter selects action.
func hookMatches(events []string, action string) bool {
	if len(events) == 0 {
		return true
	}
	for _, e := range events {
		if e == "*" || e == action {
			return true
		}
	}
	return false
}

// --- CRUD ----------------------------------------------------------------------------

// HookSpec is the create/patch body (secret write-only).
type HookSpec struct {
	URL         *string  `json:"url,omitempty"`
	Events      []string `json:"events,omitempty"`
	Secret      *string  `json:"secret,omitempty"`
	Active      *bool    `json:"active,omitempty"`
	InsecureTLS *bool    `json:"insecure_tls,omitempty"`
}

// CreateHook validates and Creates one hook config (admin).
func (s *Service) CreateHook(ctx context.Context, owner, repo string, actor string, spec HookSpec) (*Hook, error) {
	if spec.URL == nil || *spec.URL == "" {
		return nil, fmt.Errorf("%w: url is required", ErrInvalid)
	}
	if err := validateHookURL(*spec.URL); err != nil {
		return nil, err
	}
	if err := validateHookEvents(spec.Events); err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	h := &Hook{
		ID: newHookID(s.nowUTC(), rand.Reader), URL: *spec.URL,
		Events: append([]string(nil), spec.Events...), Active: true,
		CreatedBy: normPrincipal(actor), CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if spec.Active != nil {
		h.Active = *spec.Active
	}
	if spec.InsecureTLS != nil {
		h.InsecureTLS = *spec.InsecureTLS
	}
	if spec.Secret != nil {
		h.Secret = *spec.Secret
	}
	raw, err := encode(h)
	if err != nil {
		return nil, err
	}
	if err := s.putCreate(ctx, HookKey(owner, repo, h.ID), raw); err != nil {
		if store.IsPreconditionFailed(err) {
			// ULID collision is randomness failure, not a conflict —
			// retry once with fresh entropy.
			h.ID = newHookID(s.nowUTC(), rand.Reader)
			raw, err := encode(h)
			if err != nil {
				return nil, err
			}
			if err := s.putCreate(ctx, HookKey(owner, repo, h.ID), raw); err != nil {
				return nil, err
			}
			return h, nil
		}
		return nil, err
	}
	return h, nil
}

// GetHook loads one hook; nil when absent.
func (s *Service) GetHook(ctx context.Context, owner, repo, id string) *Hook {
	raw, _, err := s.getJSON(ctx, HookKey(owner, repo, id))
	if err != nil || raw == nil {
		return nil
	}
	var h Hook
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil
	}
	return &h
}

// ListHooks returns all hooks, creation order (ULID sort), secrets intact
// (callers strip before responding).
func (s *Service) ListHooks(ctx context.Context, owner, repo string) ([]*Hook, error) {
	keys := []string{}
	err := s.Store.List(ctx, WebhooksPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		name := strings.TrimPrefix(m.Key, WebhooksPrefix(owner, repo))
		if name == "" || strings.Contains(name, "/") || !strings.HasSuffix(name, ".json") {
			return nil // cursors/ + deliveries/ live below; skip
		}
		keys = append(keys, m.Key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortStrings(keys)
	out := []*Hook{}
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil || raw == nil {
			continue
		}
		var h Hook
		if err := json.Unmarshal(raw, &h); err != nil {
			continue
		}
		hc := h
		out = append(out, &hc)
	}
	return out, nil
}

// PatchHook CAS-updates mutable fields (admin). URL/events validated;
// secret replaced when present (never returned).
func (s *Service) PatchHook(ctx context.Context, owner, repo, id string, spec HookSpec) (*Hook, error) {
	var result *Hook
	_, err := s.casUpdate(ctx, HookKey(owner, repo, id), 8, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown webhook", ErrNotFound)
		}
		var h Hook
		if err := json.Unmarshal(cur, &h); err != nil {
			return nil, false, fmt.Errorf("%w: webhook: %v", ErrInvalid, err)
		}
		if spec.URL != nil {
			if err := validateHookURL(*spec.URL); err != nil {
				return nil, false, err
			}
			h.URL = *spec.URL
		}
		if spec.Events != nil {
			if err := validateHookEvents(spec.Events); err != nil {
				return nil, false, err
			}
			h.Events = append([]string(nil), spec.Events...)
		}
		if spec.Secret != nil {
			h.Secret = *spec.Secret
		}
		if spec.Active != nil {
			h.Active = *spec.Active
		}
		if spec.InsecureTLS != nil {
			h.InsecureTLS = *spec.InsecureTLS
		}
		h.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		h.Version++
		result = &h
		raw, err := encode(h)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteHook removes the config, cursor, and deliveries (admin).
func (s *Service) DeleteHook(ctx context.Context, owner, repo, id string) error {
	if s.GetHook(ctx, owner, repo, id) == nil {
		return fmt.Errorf("%w: unknown webhook", ErrNotFound)
	}
	if err := s.Store.Delete(ctx, HookKey(owner, repo, id), ""); err != nil {
		return err
	}
	_ = s.Store.Delete(ctx, CursorKey(owner, repo, id), "")
	_ = s.Store.Delete(ctx, DeliveriesKey(owner, repo, id), "")
	return nil
}

// --- delivery loop ----------------------------------------------------------------------

// hookClient is the dedicated delivery lane (bulk bytes never share a
// client with control-plane traffic — here webhooks get their own lane).
// Redirects are refused outright and every dial is screened against the
// non-public ranges with the check and the connect pinned to one
// resolution (internal/egress, shared with the events bridge so the two
// sinks cannot drift apart); either layer failing fails the delivery
// with the cursor untouched.
//
// Refuse-all (not same-host-only): a same-host https→http downgrade
// would still leak the secret and body in plaintext, and "same host" is
// DNS-defined anyway. Trade-off, stated plainly: benign redirects fail
// too — a trailing-slash normalization hop or an http→https upgrade is a
// delivery error, not a silent follow. Hook URLs must be configured in
// canonical (final, https) form; the failure lands on the deliveries
// ring and retries next pass.
var hookClient = &http.Client{
	Timeout:       WebhookTimeout,
	CheckRedirect: egress.RefuseRedirect,
	Transport:     egress.Transport(false),
}

// hookClientInsecure is the same hardened lane with TLS verification
// disabled, selected per hook by insecure_tls (§1.4, default false).
// Insecure never means unpinned: redirects are still refused and the
// dial screen still applies; only the cert check is skipped.
var hookClientInsecure = &http.Client{
	Timeout:       WebhookTimeout,
	CheckRedirect: egress.RefuseRedirect,
	Transport:     egress.Transport(true),
}

// hookClientFor selects the delivery client for h (insecure only when the
// hook opts in — the default verifies like any https client).
func hookClientFor(h *Hook) *http.Client {
	if h != nil && h.InsecureTLS {
		return hookClientInsecure
	}
	return hookClient
}

// DeliverRepo runs one webhooks pass for repo: every active hook,
// sequentially per hook, parallel across hooks (cap 8). The slot is
// acquired BEFORE spawning (issue #153), so at most FanoutParallel
// delivery goroutines exist at any instant no matter how many hooks are
// configured. Best-effort per hook; a failed hook holds back only its
// own cursor.
func (s *Service) DeliverRepo(ctx context.Context, owner, repo string) {
	hooks, err := s.ListHooks(ctx, owner, repo)
	if err != nil {
		return
	}
	sem := make(chan struct{}, FanoutParallel)
	var wg sync.WaitGroup
	for _, h := range hooks {
		if !h.Active {
			continue
		}
		h := h
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.deliverHook(ctx, owner, repo, h)
		}()
	}
	wg.Wait()
}

// deliverHook scans collab-events/ from the hook's cursor and POSTs each
// matching event; the cursor CAS-advances past the delivered prefix only.
// At-least-once: a crash or lost CAS redelivers (consumers dedup on
// X-Walgit-Delivery). A compacted gap counts and continues from the
// oldest readable event (honest-gap semantics).
func (s *Service) deliverHook(ctx context.Context, owner, repo string, h *Hook) {
	cursor := s.readCursor(ctx, owner, repo, h.ID)
	seq := cursor + 1
	lastDelivered := cursor
	const maxBatch = 256
	for i := 0; i < maxBatch; i++ {
		ev := s.readActivity(ctx, owner, repo, seq)
		if ev == nil {
			// Absent: either the head (done) or a gap (compacted or
			// reserved-but-unwritten). Probe ahead boundedly: if any
			// of the next few seqs exists, this is a gap — count and
			// continue; else this is the head — stop.
			if ahead := s.probeAhead(ctx, owner, repo, seq); ahead > seq {
				seq = ahead
				continue
			}
			break
		}
		seq++
		if ev.Action != ActionPing && !hookMatches(h.Events, ev.Action) {
			if seq-1 > lastDelivered {
				lastDelivered = seq - 1 // filtered events still advance
			}
			continue
		}
		status, derr := s.postEvent(ctx, h, ev)
		s.recordDelivery(ctx, owner, repo, h.ID, ev, status, derr)
		if derr != nil || status < 200 || status >= 300 {
			break // cursor stays: retry from here next pass
		}
		lastDelivered = seq - 1
	}
	if lastDelivered > cursor {
		s.advanceCursor(ctx, owner, repo, h.ID, lastDelivered)
	}
}

// probeAhead looks for the next existing event within a small window past
// seq (gap detection); 0 when none exists (seq is the head).
func (s *Service) probeAhead(ctx context.Context, owner, repo string, seq int) int {
	for ahead := seq + 1; ahead <= seq+8; ahead++ {
		if meta, err := s.Store.Head(ctx, ActivityKey(owner, repo, ahead)); err == nil && meta != nil {
			return ahead
		}
	}
	return 0
}

// postEvent POSTs one activity event; returns the status (0 + err on
// transport failure). The secret never leaves the HMAC + TLS: any 3xx
// fails (no redirect is followed, so no signature/body crosses hosts)
// and any non-public dial target fails closed before any SYN.
func (s *Service) postEvent(ctx context.Context, h *Hook, ev *ActivityEvent) (int, error) {
	body, err := encode(ev)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", h.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(append(append([]byte(nil), body...), []byte(strconv.Itoa(ev.Seq))...))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Walgit-Delivery", hex.EncodeToString(sum[:]))
	req.Header.Set("X-Walgit-Event", ev.Action)
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		req.Header.Set("X-Walgit-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := hookClientFor(h).Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// readCursor loads the per-hook cursor (0 when absent).
func (s *Service) readCursor(ctx context.Context, owner, repo, id string) int {
	raw, _, err := s.getJSON(ctx, CursorKey(owner, repo, id))
	if err != nil || raw == nil {
		return 0
	}
	var c CursorDoc
	if err := json.Unmarshal(raw, &c); err != nil || c.PublishedSeq < 0 {
		return 0
	}
	return c.PublishedSeq
}

// advanceCursor CASes the cursor forward (monotonic: never retreats).
func (s *Service) advanceCursor(ctx context.Context, owner, repo, id string, seq int) {
	_, _ = s.casUpdate(ctx, CursorKey(owner, repo, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var c CursorDoc
		if cur != nil {
			if err := json.Unmarshal(cur, &c); err != nil {
				return nil, false, nil // corrupt cursor: leave for the next pass
			}
		}
		if seq <= c.PublishedSeq {
			return nil, false, nil
		}
		c.PublishedSeq = seq
		c.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		raw, err := encode(c)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
}

// deliveryURLRe finds http(s) URLs inside error text (Go transport
// errors echo the request URL, e.g. Post "https://user:pass@host/hook").
var deliveryURLRe = regexp.MustCompile(`https?://\S+`)

// bearerRe finds Authorization-style bearer material in free text.
var bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-._~+/=]+`)

// sensitiveDeliveryParams are query keys whose values are credential
// material and must not persist in the deliveries ring.
var sensitiveDeliveryParams = map[string]bool{
	"token": true, "access_token": true, "auth_token": true,
	"api_key": true, "apikey": true, "key": true,
	"secret": true, "client_secret": true,
	"password": true, "passwd": true,
}

// scrubDeliveryError strips credential material from a delivery failure
// before it lands in the deliveries ring (bucket + admin API): hook URLs
// may carry userinfo (basic-auth sinks) or query tokens, and both echo
// in transport errors. The ring keeps the diagnosable shape (host, path,
// non-secret query keys, status text) with secrets replaced. Pure
// function, no shared state — safe under the DeliverRepo fan-out.
func scrubDeliveryError(s string) string {
	out := deliveryURLRe.ReplaceAllStringFunc(s, scrubDeliveryURL)
	out = redactDeliveryKV(out, "password=")
	out = redactDeliveryKV(out, "passwd=")
	out = redactDeliveryKV(out, "secret=")
	out = redactDeliveryKV(out, "client_secret=")
	out = redactDeliveryKV(out, "token=")
	out = redactDeliveryKV(out, "access_token=")
	out = redactDeliveryKV(out, "api_key=")
	out = redactDeliveryKV(out, "apikey=")
	return bearerRe.ReplaceAllString(out, "Bearer [redacted]")
}

// scrubDeliveryURL drops userinfo, redacts sensitive query values, and
// clears the fragment of one URL found in error text; returns the match
// untouched when there is nothing credential-shaped to remove (so clean
// errors keep their exact text).
func scrubDeliveryURL(m string) string {
	// Peel trailing error-text punctuation that is not part of the URL.
	core := strings.TrimRight(m, "\"',.;:!?()<>")
	tail := m[len(core):]
	u, err := url.Parse(core)
	if err != nil || u.Host == "" {
		return m
	}
	changed := false
	if u.User != nil {
		u.User = nil
		changed = true
	}
	if u.RawQuery != "" {
		if q, err := url.ParseQuery(u.RawQuery); err == nil {
			qChanged := false
			for k := range q {
				if sensitiveDeliveryParams[strings.ToLower(k)] {
					q[k] = []string{"[redacted]"}
					qChanged = true
				}
			}
			if qChanged {
				u.RawQuery = q.Encode()
				changed = true
			}
		}
	}
	if u.Fragment != "" {
		u.Fragment = ""
		changed = true
	}
	if !changed {
		return m
	}
	return u.String() + tail
}

// redactDeliveryKV cuts `key<value>` at the next delimiter (whitespace,
// quote, semicolon, comma, ampersand, closing bracket, or end of string).
func redactDeliveryKV(s, key string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(key)])
		b.WriteString("[redacted]")
		j := i + len(key)
		for j < len(s) && !strings.ContainsRune(" \t\n\r\"';,&)]>", rune(s[j])) {
			j++
		}
		s = s[j:]
	}
}

// recordDelivery appends one row to the last-25 ring (debugging surface,
// not durability — best-effort CAS). Transport errors are scrubbed of
// credential material before storing: they echo the hook URL, which may
// carry userinfo or query tokens.
func (s *Service) recordDelivery(ctx context.Context, owner, repo, id string, ev *ActivityEvent, status int, derr error) {
	entry := DeliveryEntry{Seq: ev.Seq, Event: ev.Action, Status: status, At: s.nowUTC().Format(dateTimeFmt)}
	if derr != nil {
		entry.Error = scrubDeliveryError(derr.Error())
	}
	_, _ = s.casUpdate(ctx, DeliveriesKey(owner, repo, id), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var d DeliveriesDoc
		if cur != nil {
			_ = json.Unmarshal(cur, &d)
		}
		d.Entries = append(d.Entries, entry)
		if len(d.Entries) > MaxDeliveries {
			d.Entries = d.Entries[len(d.Entries)-MaxDeliveries:]
		}
		d.UpdatedAt = entry.At
		raw, err := encode(d)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
}

// ReadDeliveries loads the ring (nil entries when absent — wire `[]`).
func (s *Service) ReadDeliveries(ctx context.Context, owner, repo, id string) *DeliveriesDoc {
	raw, _, err := s.getJSON(ctx, DeliveriesKey(owner, repo, id))
	if err != nil || raw == nil {
		return &DeliveriesDoc{Entries: []DeliveryEntry{}}
	}
	var d DeliveriesDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return &DeliveriesDoc{Entries: []DeliveryEntry{}}
	}
	if d.Entries == nil {
		d.Entries = []DeliveryEntry{}
	}
	return &d
}

// PingHook synthesizes a ping activity event (num 0) and runs one
// delivery pass for that hook, so ping success proves URL + secret end
// to end. Returns delivered=true when the ping event left the cursor.
func (s *Service) PingHook(ctx context.Context, owner, repo, id, actor string) (bool, error) {
	h := s.GetHook(ctx, owner, repo, id)
	if h == nil {
		return false, fmt.Errorf("%w: unknown webhook", ErrNotFound)
	}
	if !h.Active {
		return false, fmt.Errorf("%w: webhook inactive", ErrInvalid)
	}
	seq, err := s.reserveSeq(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	ev := ActivityEvent{
		Seq: seq, Repo: owner + "/" + repo, Action: ActionPing,
		Kind: "repo", Actor: normPrincipal(actor), At: s.nowUTC().Format(dateTimeFmt),
	}
	raw, err := encode(ev)
	if err != nil {
		return false, err
	}
	if err := s.putCreate(ctx, ActivityKey(owner, repo, seq), raw); err != nil && !store.IsPreconditionFailed(err) {
		return false, err
	}
	s.deliverHook(ctx, owner, repo, h)
	return s.readCursor(ctx, owner, repo, id) >= seq, nil
}
