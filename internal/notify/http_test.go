package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// req builds an authed request (principal "" = anonymous).
func req(t *testing.T, method, path, principal string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if principal != "" {
		r.Header.Set("X-Test-Principal", principal)
	}
	return r
}

func do(h *Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func TestUserRoutesAuthTable(t *testing.T) {
	x := newHarness(t)
	paths := []struct{ method, path string }{
		{"GET", "/api/v1/notifications"},
		{"GET", "/api/v1/notifications/unread_count"},
		{"POST", "/api/v1/notifications/read_all"},
		{"GET", "/api/v1/notifications/stream"},
		{"POST", "/api/v1/notifications/" + strings.Repeat("0", 32) + "/read"},
		{"GET", "/api-browser/v1/notifications"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(x.handler, req(t, tc.method, tc.path, ""))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("anon = %d, want 401", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("error content-type = %q", ct)
			}
			if h := rec.Header().Get("WWW-Authenticate"); h == "" {
				t.Fatal("401 must carry WWW-Authenticate")
			}
		})
	}
}

func TestTrayUnreadFlipReadAll(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	who := "amy@example.com"

	rec := do(x.handler, req(t, "GET", "/api/v1/notifications", who))
	if rec.Code != 200 {
		t.Fatalf("tray = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("tray cache = %q", cc)
	}
	var tray struct {
		Notifications []Notification `json:"notifications"`
		More          bool           `json:"more"`
	}
	mustJSON(t, rec, &tray)
	if tray.Notifications == nil || len(tray.Notifications) != 0 || tray.More {
		t.Fatalf("empty tray = %+v", tray)
	}

	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	// Amy's tray: an author entry (bob's comment) + a mentioned entry
	// (carol's mention) — two reasons, no dedup between them.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "carol@example.com", "", "", []string{"amy@example.com"})
	rec = do(x.handler, req(t, "GET", "/api/v1/notifications?state=unread", who))
	mustJSON(t, rec, &tray)
	if len(tray.Notifications) != 2 || tray.More {
		t.Fatalf("tray = %+v", tray)
	}
	if tray.Notifications[0].CreatedAt < tray.Notifications[1].CreatedAt {
		t.Fatal("tray must be newest-first")
	}
	rec = do(x.handler, req(t, "GET", "/api/v1/notifications/unread_count", who))
	var cnt struct {
		Count int `json:"count"`
	}
	mustJSON(t, rec, &cnt)
	if cnt.Count != 2 {
		t.Fatalf("count = %d", cnt.Count)
	}

	// Flip the first to read via its id.
	id := tray.Notifications[0].ID
	rec = do(x.handler, req(t, "POST", "/api/v1/notifications/"+id+"/read", who))
	if rec.Code != 200 {
		t.Fatalf("read = %d %q", rec.Code, rec.Body.String())
	}
	var n Notification
	mustJSON(t, rec, &n)
	if n.State != StateRead {
		t.Fatalf("state = %+v", n)
	}
	rec = do(x.handler, req(t, "GET", "/api/v1/notifications?state=read", who))
	mustJSON(t, rec, &tray)
	if len(tray.Notifications) != 1 {
		t.Fatalf("read filter = %+v", tray)
	}
	// after-cursor pages past it.
	rec = do(x.handler, req(t, "GET", "/api/v1/notifications?after="+id, who))
	mustJSON(t, rec, &tray)
	if len(tray.Notifications) != 1 || tray.More {
		t.Fatalf("after page = %+v", tray)
	}
	// Flip back to unread, then read_all.
	rec = do(x.handler, req(t, "POST", "/api/v1/notifications/"+id+"/unread", who))
	mustJSON(t, rec, &n)
	if n.State != StateUnread {
		t.Fatalf("unread flip = %+v", n)
	}
	rec = do(x.handler, req(t, "POST", "/api/v1/notifications/read_all", who))
	var all struct {
		Updated int `json:"updated"`
	}
	mustJSON(t, rec, &all)
	if all.Updated != 2 {
		t.Fatalf("read_all = %+v", all)
	}
	rec = do(x.handler, req(t, "GET", "/api/v1/notifications/unread_count", who))
	mustJSON(t, rec, &cnt)
	if cnt.Count != 0 {
		t.Fatalf("count after read_all = %d", cnt.Count)
	}
}

func TestFlipForeignIDIs404(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "zed@example.com")
	emitComment(t, x, "bob@example.com", []string{"zed@example.com"})
	// Amy flips ZED's notification id → 404 (never 403).
	raw, _, _ := x.svc.getJSON(ctx(), NotifIndexKey("zed@example.com"))
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	rec := do(x.handler, req(t, "POST", "/api/v1/notifications/"+ix.Entries[0].ID+"/read", "amy@example.com"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign id = %d, want 404", rec.Code)
	}
	// Malformed id → 404 as well.
	rec = do(x.handler, req(t, "POST", "/api/v1/notifications/nothex/read", "amy@example.com"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404", rec.Code)
	}
}

func TestTrayBadQuery(t *testing.T) {
	x := newHarness(t)
	for _, path := range []string{
		"/api/v1/notifications?state=bogus",
		"/api/v1/notifications?n=0",
		"/api/v1/notifications?n=abc",
	} {
		rec := do(x.handler, req(t, "GET", path, "amy@example.com"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("error must be plain text: %q", ct)
		}
	}
}

// safeRecorder is a goroutine-safe Flusher for stream tests.
type safeRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
	code   int
}

func newSafeRecorder() *safeRecorder { return &safeRecorder{header: http.Header{}} }

func (s *safeRecorder) Header() http.Header { return s.header }

func (s *safeRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(b)
}

func (s *safeRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
}

func (s *safeRecorder) Flush() {}

func (s *safeRecorder) snapshot() (int, http.Header, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.header.Clone(), s.body.String()
}

func TestStreamDeliversFrame(t *testing.T) {
	x := newHarness(t)
	rec := newSafeRecorder()
	r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil)
	r.Header.Set("X-Test-Principal", "amy@example.com")
	rctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(rctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.handler.ServeHTTP(rec, r)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for x.svc.ubus.liveCount("amy@example.com") == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("stream never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	x.svc.ubus.publish("amy@example.com", Notification{ID: strings.Repeat("f", 32), Repo: "acme/repo", Reason: ReasonSubscribed, State: StateUnread})
	frame := ""
	for {
		_, hdr, body := rec.snapshot()
		if ct := hdr.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
			cancel()
			t.Fatalf("content-type = %q", ct)
		}
		if strings.Contains(body, "event: notification") {
			frame = body
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("frame never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.HasPrefix(frame, ": walgit\n\n") {
		t.Fatalf("missing opener: %q", frame)
	}
	var got Notification
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data: ") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got)
		}
	}
	if got.ID != strings.Repeat("f", 32) || got.Reason != ReasonSubscribed {
		t.Fatalf("frame = %+v", got)
	}
	cancel()
	<-done
}

func TestWatchEndpoints(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	// Anonymous → 401 everywhere.
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		rec := do(x.handler, req(t, m, "/acme/repo/api/watch", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s anon = %d", m, rec.Code)
		}
	}
	// PUT watches (twin lane too).
	for _, lane := range []string{"api", "api-browser"} {
		rec := do(x.handler, req(t, "PUT", "/acme/repo/"+lane+"/watch", "amy@example.com"))
		if rec.Code != 200 {
			t.Fatalf("PUT %s = %d %q", lane, rec.Code, rec.Body.String())
		}
		var st map[string]any
		mustJSON(t, rec, &st)
		if st["watching"] != true {
			t.Fatalf("PUT = %v", st)
		}
	}
	// GET reflects it (no-store).
	rec := do(x.handler, req(t, "GET", "/acme/repo/api/watch", "amy@example.com"))
	var get map[string]any
	mustJSON(t, rec, &get)
	if get["watching"] != true {
		t.Fatalf("GET = %v", get)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("watch GET cache = %q", cc)
	}
	// DELETE unwatches.
	rec = do(x.handler, req(t, "DELETE", "/acme/repo/api/watch", "amy@example.com"))
	var del map[string]any
	mustJSON(t, rec, &del)
	if del["watching"] != false || del["watchers"] != float64(0) {
		t.Fatalf("DELETE = %v", del)
	}
	// Second DELETE is idempotent.
	rec = do(x.handler, req(t, "DELETE", "/acme/repo/api/watch", "amy@example.com"))
	if rec.Code != 200 {
		t.Fatalf("re-DELETE = %d", rec.Code)
	}
}

func TestWebhookEndpointsTable(t *testing.T) {
	x := newHarness(t)
	x.roles.grant("acme", "repo", "amy@example.com", "admin")

	// Non-admin authenticated → 403; anonymous → 401.
	rec := do(x.handler, req(t, "GET", "/acme/repo/api/webhooks", "bob@example.com"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
	rec = do(x.handler, req(t, "GET", "/acme/repo/api/webhooks", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon = %d, want 401", rec.Code)
	}

	// Create with secret (loopback http allowed for dev test servers).
	create := func(path, principal, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if principal != "" {
			r.Header.Set("X-Test-Principal", principal)
		}
		return do(x.handler, r)
	}
	rec = create("/acme/repo/api/webhooks", "amy@example.com", `{"url":"http://127.0.0.1:9/hook","events":["commented"],"secret":"s3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	var created map[string]any
	mustJSON(t, rec, &created)
	id, _ := created["id"].(string)
	if len(id) != 24 || created["secret_set"] != true {
		t.Fatalf("created = %v", created)
	}
	if _, has := created["secret"]; has {
		t.Fatal("secret must never be returned")
	}

	// Get hides the secret too.
	rec = do(x.handler, req(t, "GET", "/acme/repo/api/webhooks/"+id, "amy@example.com"))
	var got map[string]any
	mustJSON(t, rec, &got)
	if got["secret_set"] != true {
		t.Fatalf("get = %v", got)
	}
	if _, has := got["secret"]; has {
		t.Fatal("secret must never be returned")
	}

	// List shows it (no secret).
	rec = do(x.handler, req(t, "GET", "/acme/repo/api/webhooks", "amy@example.com"))
	var list struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	mustJSON(t, rec, &list)
	if len(list.Webhooks) != 1 {
		t.Fatalf("list = %+v", list)
	}

	// Patch + deliveries + ping against a dead URL (records failure, no crash).
	r := httptest.NewRequest("PATCH", "/acme/repo/api/webhooks/"+id, strings.NewReader(`{"events":[]}`))
	r.Header.Set("X-Test-Principal", "amy@example.com")
	rec = do(x.handler, r)
	if rec.Code != 200 {
		t.Fatalf("patch = %d %q", rec.Code, rec.Body.String())
	}
	rec = do(x.handler, req(t, "POST", "/acme/repo/api/webhooks/"+id+"/ping", "amy@example.com"))
	if rec.Code != 200 {
		t.Fatalf("ping = %d %q", rec.Code, rec.Body.String())
	}
	rec = do(x.handler, req(t, "GET", "/acme/repo/api/webhooks/"+id+"/deliveries", "amy@example.com"))
	var deliv DeliveriesDoc
	mustJSON(t, rec, &deliv)
	if deliv.Entries == nil || len(deliv.Entries) != 1 {
		t.Fatalf("deliveries = %+v", deliv)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("deliveries cache = %q", cc)
	}

	// Unknown hook → 404 on every sub-route.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/acme/repo/api/webhooks/nope"},
		{"PATCH", "/acme/repo/api/webhooks/nope"},
		{"DELETE", "/acme/repo/api/webhooks/nope"},
		{"POST", "/acme/repo/api/webhooks/nope/ping"},
		{"GET", "/acme/repo/api/webhooks/nope/deliveries"},
	} {
		var rr *http.Request
		if tc.method == "PATCH" {
			rr = httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		} else {
			rr = httptest.NewRequest(tc.method, tc.path, nil)
		}
		rr.Header.Set("X-Test-Principal", "amy@example.com")
		if rec := do(x.handler, rr); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}

	// Delete → 204, then gone.
	rec = do(x.handler, req(t, "DELETE", "/acme/repo/api/webhooks/"+id, "amy@example.com"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = do(x.handler, req(t, "GET", "/acme/repo/api/webhooks/"+id, "amy@example.com"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", rec.Code)
	}
}

func TestHandleRouting(t *testing.T) {
	x := newHarness(t)
	for _, path := range []string{
		"/api/v1/repos", "/acme/repo/api/issues", "/acme/repo/api/pulls",
		"/not-a-repo/api/watch", "/api/v1/notificationsx",
	} {
		rec := do(x.handler, req(t, "GET", path, "amy@example.com"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s handled = %d, want fallthrough 404", path, rec.Code)
		}
	}
}

// TestNotifyGhostRepoTable pins the #63 HTTP surface for deleted repos
// (no manifest seeded): watching fails closed, unwatching cleans up, and
// the tray filters the dead rows.
func TestNotifyGhostRepoTable(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "live")
	// Stale userspace rows with no repo behind them: a watch record and
	// two tray rows (index + overflow object).
	writeRaw(t, x.svc.Store, WatchingKey("amy@example.com", "acme", "gone"),
		[]byte(`{"repo":"acme/gone","watched_at":"2026-09-04T12:00:00Z"}`))
	at := x.now.Format(dateTimeFmt)
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), mustEncode(t, IndexDoc{
		Version: 1, UnreadCount: 2,
		Entries: []IndexEntry{
			{ID: strings.Repeat("a", 32), Repo: "acme/gone", Num: 1, Kind: "issue", Reason: ReasonSubscribed, Title: "G", State: StateUnread, At: at},
			{ID: strings.Repeat("b", 32), Repo: "acme/live", Num: 2, Kind: "issue", Reason: ReasonSubscribed, Title: "L", State: StateUnread, At: at},
		},
	}))
	writeRaw(t, x.svc.Store, NotifKey("amy@example.com", strings.Repeat("a", 32)), mustEncode(t, Notification{
		ID: strings.Repeat("a", 32), Repo: "acme/gone", Num: 1, Kind: "issue",
		Reason: ReasonSubscribed, Title: "G", State: StateUnread, CreatedAt: at,
	}))
	writeRaw(t, x.svc.Store, NotifKey("amy@example.com", strings.Repeat("c", 32)), mustEncode(t, Notification{
		ID: strings.Repeat("c", 32), Repo: "acme/gone", Num: 3, Kind: "issue",
		Reason: ReasonMentioned, Title: "O", State: StateUnread, CreatedAt: at,
	}))
	rows := []struct {
		name   string
		method string
		path   string
		status int
		body   string
	}{
		{"watch ghost 404", "PUT", "/acme/gone/api/watch", 404, "not found"},
		{"get watch ghost hides record", "GET", "/acme/gone/api/watch", 200, `"watching":false`},
		{"unwatch ghost cleans record", "DELETE", "/acme/gone/api/watch", 200, `"watching":false`},
		{"tray skips dead rows", "GET", "/api/v1/notifications", 200, strings.Repeat("b", 32)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := do(x.handler, req(t, row.method, row.path, "amy@example.com"))
			if rec.Code != row.status {
				t.Fatalf("%s %s: got %d want %d (%q)", row.method, row.path, rec.Code, row.status, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), row.body) {
				t.Fatalf("%s %s: body %q lacks %q", row.method, row.path, rec.Body.String(), row.body)
			}
		})
	}
	// The dead rows are gone from the tray body (only the live row serves).
	rec := do(x.handler, req(t, "GET", "/api/v1/notifications", "amy@example.com"))
	if strings.Contains(rec.Body.String(), strings.Repeat("a", 32)) ||
		strings.Contains(rec.Body.String(), strings.Repeat("c", 32)) {
		t.Fatalf("tray leaks dead rows: %q", rec.Body.String())
	}
	// Unwatching the ghost resurrected no social.json.
	if raw, _, err := store.GetBytes(ctx(), x.svc.Store, SocialKey("acme", "gone"), store.GetOptions{}); err == nil && raw != nil {
		t.Fatalf("resurrected social.json: %s", raw)
	}
}

// TestWatchGhostRepo pins the service-level #63 watch contract: watch on
// a ghost 404s, GetWatch hides stale records, unwatch cleans without
// resurrecting counters.
func TestWatchGhostRepo(t *testing.T) {
	x := newHarness(t)
	if _, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "gone", true); !isErr(err, ErrNotFound) {
		t.Fatalf("watch ghost: %v", err)
	}
	// Seed a stale record directly, then sweep the (absent) manifest —
	// GetWatch must not render it.
	writeRaw(t, x.svc.Store, WatchingKey("amy@example.com", "acme", "gone"),
		[]byte(`{"repo":"acme/gone","watched_at":"2026-09-04T12:00:00Z"}`))
	if got := x.svc.GetWatch(ctx(), "amy@example.com", "acme", "gone"); got.Watching || got.Watchers != 0 {
		t.Fatalf("getwatch ghost: %+v", got)
	}
	st, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "gone", false)
	if err != nil || st.Watching || st.Watchers != 0 {
		t.Fatalf("unwatch ghost: %+v %v", st, err)
	}
	if _, _, err := store.GetBytes(ctx(), x.svc.Store, WatchingKey("amy@example.com", "acme", "gone"), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("record must be gone: %v", err)
	}
}
