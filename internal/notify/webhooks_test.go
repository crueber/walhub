package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sinkServer is a test webhook sink: verifies HMAC keepers and records posts.
type sinkServer struct {
	t      *testing.T
	secret string
	mu     sync.Mutex
	posts  []sinkPost
	fail   bool
}

type sinkPost struct {
	body      []byte
	delivery  string
	signature string
	event     string
}

func newSink(t *testing.T, secret string) (*sinkServer, *httptest.Server) {
	t.Helper()
	s := &sinkServer{t: t, secret: secret}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if s.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		delivery := r.Header.Get("X-Walgit-Delivery")
		event := r.Header.Get("X-Walgit-Event")
		sig := r.Header.Get("X-Walgit-Signature")
		if delivery == "" || event == "" {
			t.Errorf("missing keeper headers: delivery=%q event=%q", delivery, event)
		}
		var ev ActivityEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("bad body: %v", err)
		}
		// Delivery key = hex(sha256(body+seq)).
		sum := sha256.Sum256(append(append([]byte(nil), body...), []byte(strconv.Itoa(ev.Seq))...))
		if delivery != hex.EncodeToString(sum[:]) {
			t.Errorf("delivery key mismatch")
		}
		if s.secret != "" {
			mac := hmac.New(sha256.New, []byte(s.secret))
			mac.Write(body)
			if sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
				t.Errorf("HMAC mismatch: %q", sig)
			}
		} else if sig != "" {
			t.Errorf("signature must be omitted without secret")
		}
		s.mu.Lock()
		s.posts = append(s.posts, sinkPost{body: body, delivery: delivery, signature: sig, event: event})
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return s, srv
}

func (s *sinkServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func TestWebhookDeliveryEndToEnd(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	sink, srv := newSink(t, "s3cr3t")
	defer srv.Close()

	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{"commented"}, Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hk.ID) != 24 {
		t.Fatalf("hook id = %q", hk.ID)
	}
	// Secret round-trips write-only: stored, never serialized on read.
	if got := x.svc.GetHook(ctx(), "acme", "repo", hk.ID); got == nil || got.Secret != "s3cr3t" {
		t.Fatalf("secret not stored: %+v", got)
	}

	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.svc.DeliverRepo(ctx(), "acme", "repo")

	if got := sink.count(); got != 1 {
		t.Fatalf("sink posts = %d, want 1", got)
	}
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 1 {
		t.Fatalf("cursor = %d, want 1", cur)
	}
	d := x.svc.ReadDeliveries(ctx(), "acme", "repo", hk.ID)
	if len(d.Entries) != 1 || d.Entries[0].Status != 200 || d.Entries[0].Event != "commented" {
		t.Fatalf("deliveries = %+v", d.Entries)
	}
	// Second pass: nothing new, no redelivery.
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if got := sink.count(); got != 1 {
		t.Fatalf("redelivered: %d", got)
	}
}

func TestWebhookFilterAndFailure(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	sink, srv := newSink(t, "")
	defer srv.Close()

	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{"opened"},
	})
	if err != nil {
		t.Fatal(err)
	}
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	// Filtered out (commented ∉ {opened}) but still advances the cursor.
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if got := sink.count(); got != 0 {
		t.Fatalf("filtered event posted: %d", got)
	}
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 1 {
		t.Fatalf("filtered cursor = %d, want 1", cur)
	}
	// Failure: cursor held, delivery row records the 500.
	sink.fail = true
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", []string{"amy@example.com"})
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, HookSpec{Events: []string{}}); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 1 {
		t.Fatalf("failed cursor advanced to %d", cur)
	}
	d := x.svc.ReadDeliveries(ctx(), "acme", "repo", hk.ID)
	last := d.Entries[len(d.Entries)-1]
	if last.Status != 500 || last.Event != "mentioned" {
		t.Fatalf("failure row = %+v", last)
	}
	// Recovery: cursor advances past both.
	sink.fail = false
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 2 {
		t.Fatalf("recovered cursor = %d, want 2", cur)
	}
}

func TestWebhookPing(t *testing.T) {
	x := newHarness(t)
	sink, srv := newSink(t, "pw")
	defer srv.Close()
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Secret: strPtr("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := x.svc.PingHook(ctx(), "acme", "repo", hk.ID, "amy@example.com")
	if err != nil || !delivered {
		t.Fatalf("ping = %v, %v", delivered, err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.posts) != 1 || sink.posts[0].event != "ping" {
		t.Fatalf("ping posts = %+v", sink.posts)
	}
	var ev ActivityEvent
	if err := json.Unmarshal(sink.posts[0].body, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Action != "ping" || ev.Num != 0 || ev.Kind != "repo" {
		t.Fatalf("ping event = %+v", ev)
	}
}

func TestPingBypassesEventFilter(t *testing.T) {
	x := newHarness(t)
	sink, srv := newSink(t, "")
	defer srv.Close()
	// Filter excludes ping — a filtered ping must still POST (ping
	// success proves URL + secret end to end).
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{"commented"},
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := x.svc.PingHook(ctx(), "acme", "repo", hk.ID, "amy@example.com")
	if err != nil || !delivered {
		t.Fatalf("ping = %v, %v", delivered, err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("filtered ping must still POST: %d", got)
	}
}

func TestHookValidation(t *testing.T) {
	x := newHarness(t)
	cases := []struct {
		name   string
		spec   HookSpec
		errSub string
	}{
		{name: "missing-url", spec: HookSpec{}, errSub: "url is required"},
		{name: "bad-scheme", spec: HookSpec{URL: strPtr("gopher://x/y")}, errSub: "scheme"},
		{name: "http-remote", spec: HookSpec{URL: strPtr("http://example.com/hook")}, errSub: "https"},
		{name: "bad-event", spec: HookSpec{URL: strPtr("https://example.com/h"), Events: []string{"bogus"}}, errSub: "unknown webhook event"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", tc.spec); err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want %q", err, tc.errSub)
			}
		})
	}
	// http on loopback is allowed (dev).
	for _, u := range []string{"http://localhost:9999/h", "http://127.0.0.1:9999/h", "https://example.com/h"} {
		if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr(u)}); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
}

func TestHookCRUDListPatchDelete(t *testing.T) {
	x := newHarness(t)
	mk := func(url string) *Hook {
		hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: strPtr(url)})
		if err != nil {
			t.Fatal(err)
		}
		return hk
	}
	a, b := mk("https://a.example/h"), mk("https://b.example/h")
	if a.ID == b.ID {
		t.Fatal("hook ids must be unique")
	}
	hooks, err := x.svc.ListHooks(ctx(), "acme", "repo")
	if err != nil || len(hooks) != 2 {
		t.Fatalf("list = %v, %v", len(hooks), err)
	}
	if hooks[0].ID > hooks[1].ID {
		t.Fatal("hooks must list in creation (ULID) order")
	}
	inactive := false
	patched, err := x.svc.PatchHook(ctx(), "acme", "repo", a.ID, HookSpec{Active: &inactive, Events: []string{"ping"}})
	if err != nil || patched.Active || len(patched.Events) != 1 {
		t.Fatalf("patch = %+v, %v", patched, err)
	}
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", "nope", HookSpec{}); err == nil {
		t.Fatal("patch of missing hook must 404")
	}
	if err := x.svc.DeleteHook(ctx(), "acme", "repo", a.ID); err != nil {
		t.Fatal(err)
	}
	if x.svc.GetHook(ctx(), "acme", "repo", a.ID) != nil {
		t.Fatal("deleted hook still readable")
	}
	if err := x.svc.DeleteHook(ctx(), "acme", "repo", "nope"); err == nil {
		t.Fatal("delete of missing hook must 404")
	}
}

func TestHookIDShape(t *testing.T) {
	seen := map[string]bool{}
	var prev string
	base := testTime()
	for i := 0; i < 50; i++ {
		id := newHookID(base.Add(time.Duration(i)*time.Millisecond), testReader(i))
		if len(id) != 24 {
			t.Fatalf("id len = %d", len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdefghjkmnpqrstvwxyz", c) {
				t.Fatalf("bad char %q in %q", c, id)
			}
		}
		if seen[id] {
			t.Fatalf("dup id %q", id)
		}
		seen[id] = true
		if prev != "" && id < prev {
			t.Fatalf("ids not time-ordered: %q then %q", prev, id)
		}
		prev = id
	}
}

func strPtr(s string) *string { return &s }

// TestWebhookInsecureTLS pins the §1.4 insecure_tls contract: a hook
// against a self-signed TLS sink fails closed by default (cursor held,
// delivery row records the transport error) and delivers once the hook
// opts into insecure_tls (cursor advances).
func TestWebhookInsecureTLS(t *testing.T) {
	x := newHarness(t)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsSrv.Close()

	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(tlsSrv.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	seq, err := x.svc.reserveSeq(ctx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", Kind: "issue", At: x.now.Format(dateTimeFmt)}
	if err := x.svc.putCreate(ctx(), ActivityKey("acme", "repo", seq), encode(ev)); err != nil {
		t.Fatal(err)
	}
	// Default (verify): self-signed fails, cursor stays at 0.
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 0 {
		t.Fatalf("self-signed cursor advanced to %d without insecure_tls", cur)
	}
	// Opt-in: the same event delivers, cursor advances past it.
	insecure := true
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, HookSpec{InsecureTLS: &insecure}); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != seq {
		t.Fatalf("insecure cursor = %d, want %d", cur, seq)
	}
}

func testTime() (t time.Time) { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }

// testReader yields deterministic entropy distinct per i.
func testReader(i int) io.Reader {
	b := make([]byte, 64)
	for j := range b {
		b[j] = byte((i*31 + j*7) & 0xff)
	}
	return bytes.NewReader(b)
}
