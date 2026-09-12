// orgactivity_test.go — Forgejo #364: the org activity surface (API
// paging over the org-event log, owner gating, live frames, retention).
// Package notify (white-box, same harness as the org webhook tests).
package notify

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// seedOrgEvents emits actions in order (seqs 1..N) and returns them.
func seedOrgEvents(x *harness, org string, actions ...string) {
	for _, a := range actions {
		x.svc.EmitOrgEvent(context.Background(), org, a, "amy@example.com", a+" title")
	}
}

// activitySeqs returns the seqs of a page (newest-first).
func activitySeqs(events []ActivityEvent) []int {
	out := []int{}
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListOrgActivityPaging(t *testing.T) {
	ctx := context.Background()
	seed := []string{"member_added", "team_created", "invite_created", "member_removed", "team_deleted"}
	rows := []struct {
		name      string
		org       string
		after     int
		n         int
		wantSeqs  []int
		wantMore  bool
		wantErr   bool
		wantFirst string // expected action of the newest event
	}{
		{"empty org", "ghost", 0, 0, []int{}, false, false, ""},
		{"default page newest-first", "acme", 0, 0, []int{5, 4, 3, 2, 1}, false, false, "team_deleted"},
		{"explicit n fills", "acme", 0, 2, []int{5, 4}, true, false, ""},
		{"after cursor pages older", "acme", 4, 0, []int{3, 2, 1}, false, false, ""},
		{"after cursor with n", "acme", 5, 2, []int{4, 3}, true, false, ""},
		{"after at floor", "acme", 1, 0, []int{}, false, false, ""},
		{"after beyond head clamps", "acme", 999, 0, []int{5, 4, 3, 2, 1}, false, false, ""},
		{"negative n defaults", "acme", 0, -3, []int{5, 4, 3, 2, 1}, false, false, ""},
		{"huge n caps without error", "acme", 0, 100000, []int{5, 4, 3, 2, 1}, false, false, ""},
		{"negative after errors", "acme", -1, 0, nil, false, true, ""},
		{"bad org errors", "ACME!", 0, 0, nil, false, true, ""},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			if tc.org == "acme" {
				seedOrgEvents(x, "acme", seed...)
			}
			got, more, err := x.svc.ListOrgActivity(ctx, tc.org, tc.after, tc.n)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ListOrgActivity(%q, %d, %d) = no error", tc.org, tc.after, tc.n)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListOrgActivity: %v", err)
			}
			if got == nil {
				t.Fatalf("events nil, want []-never-nil")
			}
			if !equalInts(activitySeqs(got), tc.wantSeqs) {
				t.Fatalf("seqs = %v, want %v", activitySeqs(got), tc.wantSeqs)
			}
			if more != tc.wantMore {
				t.Fatalf("more = %v, want %v", more, tc.wantMore)
			}
			if tc.wantFirst != "" && got[0].Action != tc.wantFirst {
				t.Fatalf("newest action = %q, want %q", got[0].Action, tc.wantFirst)
			}
			for _, e := range got {
				if e.Kind != OrgHookKind || e.Repo != "acme" {
					t.Fatalf("event = %+v, want Kind org + Repo acme", e)
				}
			}
		})
	}
}

func TestListOrgActivitySkipsGaps(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	// Reserve seq 1 without appending (a crash-between-reserve-and-
	// create): the page skips the hole and still reports more=false at
	// the floor.
	if _, err := x.svc.reserveOrgSeq(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	seedOrgEvents(x, "acme", "member_added", "team_created")
	got, more, err := x.svc.ListOrgActivity(ctx, "acme", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(activitySeqs(got), []int{3, 2}) {
		t.Fatalf("seqs = %v, want [3 2]", activitySeqs(got))
	}
	if more {
		t.Fatalf("more = true, want false (walk reached seq 0)")
	}
}

func TestOrgActivityHandlerGating(t *testing.T) {
	rows := []struct {
		name      string
		method    string
		path      string
		principal string
		admin     bool
		want      int
	}{
		{"owner reads empty log", "GET", "/api/v1/orgs/acme/activity", "amy@example.com", false, 200},
		{"browser lane", "GET", "/api-browser/v1/orgs/acme/activity", "amy@example.com", false, 200},
		{"host admin reads", "GET", "/api/v1/orgs/acme/activity", "root@example.com", true, 200},
		{"anonymous 401", "GET", "/api/v1/orgs/acme/activity", "", false, 401},
		{"non-owner 403", "GET", "/api/v1/orgs/acme/activity", "bob@example.com", false, 403},
		{"bad org 404", "GET", "/api/v1/orgs/ACME!/activity", "amy@example.com", false, 404},
		{"bad n 400", "GET", "/api/v1/orgs/acme/activity?n=0", "amy@example.com", false, 400},
		{"bad n text 400", "GET", "/api/v1/orgs/acme/activity?n=many", "amy@example.com", false, 400},
		{"bad after 400", "GET", "/api/v1/orgs/acme/activity?after=-2", "amy@example.com", false, 400},
		{"bad after text 400", "GET", "/api/v1/orgs/acme/activity?after=soon", "amy@example.com", false, 400},
		{"wrong method 404", "POST", "/api/v1/orgs/acme/activity", "amy@example.com", false, 404},
		{"stream wrong method 405", "POST", "/api/v1/orgs/acme/activity/stream", "amy@example.com", false, 405},
		{"stream gated 403", "GET", "/api/v1/orgs/acme/activity/stream", "bob@example.com", false, 403},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			x, _ := orgHarness(t)
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.principal != "" {
				r.Header.Set("X-Test-Principal", tc.principal)
			}
			if tc.admin {
				r.Header.Set("X-Test-Admin", "1")
			}
			rec := do(x.handler, r)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestOrgActivityHandlerPage(t *testing.T) {
	x, _ := orgHarness(t)
	seedOrgEvents(x, "acme", "member_added", "team_created", "invite_created")
	r := httptest.NewRequest("GET", "/api/v1/orgs/acme/activity?n=2", nil)
	r.Header.Set("X-Test-Principal", "amy@example.com")
	rec := do(x.handler, r)
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Events []ActivityEvent `json:"events"`
		More   bool            `json:"more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !equalInts(activitySeqs(body.Events), []int{3, 2}) || !body.More {
		t.Fatalf("page = %+v", body)
	}
	// Follow the cursor: the second page holds the tail with more=false.
	r2 := httptest.NewRequest("GET", "/api/v1/orgs/acme/activity?after=2", nil)
	r2.Header.Set("X-Test-Principal", "amy@example.com")
	rec2 := do(x.handler, r2)
	var body2 struct {
		Events []ActivityEvent `json:"events"`
		More   bool            `json:"more"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if !equalInts(activitySeqs(body2.Events), []int{1}) || body2.More {
		t.Fatalf("page2 = %+v", body2)
	}
	// no-store on the owner-private read.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestOrgActivityLiveFrame(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	ch, recent, unsub := x.svc.SubscribeRepo("acme")
	defer unsub()
	if len(recent) != 0 {
		t.Fatalf("recent = %d frames, want empty", len(recent))
	}
	x.svc.EmitOrgEvent(ctx, "acme", "member_added", "amy@example.com", "amy joined acme")
	select {
	case f := <-ch:
		frame, ok := orgActivityFrame(f)
		if !ok {
			t.Fatalf("frame dropped: %+v", f)
		}
		if frame["action"] != "member_added" || frame["org"] != "acme" || frame["seq"] != 1 {
			t.Fatalf("frame = %v", frame)
		}
	default:
		t.Fatalf("no live frame after EmitOrgEvent")
	}
	// Unknown action drops before the bus (no frame, no gap).
	x.svc.EmitOrgEvent(ctx, "acme", "bogus", "amy@example.com", "x")
	select {
	case f := <-ch:
		t.Fatalf("bogus action published: %+v", f)
	default:
	}
	// Foreign kinds never map (the stream drops them, never breaks).
	if _, ok := orgActivityFrame(RepoFrame{Name: "issue", Repo: "acme"}); ok {
		t.Fatalf("non-org frame mapped")
	}
}

func TestOrgActivityStreamReplayAndLive(t *testing.T) {
	x, _ := orgHarness(t)
	ctx := context.Background()
	// Seed the ring: one org frame plus one foreign-kind frame on the
	// same key (dropped on read) — a frame for another org rides a
	// different bus key and never appears.
	x.svc.EmitOrgEvent(ctx, "acme", "member_added", "amy@example.com", "amy joined acme")
	x.svc.PublishFrame(RepoFrame{Name: "issue", Repo: "acme"})
	x.svc.EmitOrgEvent(ctx, "other", "member_added", "amy@example.com", "x")

	rec := newSafeRecorder()
	r := httptest.NewRequest("GET", "/api/v1/orgs/acme/activity/stream", nil)
	r.Header.Set("X-Test-Principal", "amy@example.com")
	r.Header.Set("Accept", "text/event-stream")
	rctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(rctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.handler.ServeHTTP(rec, r)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for x.svc.repoLiveCount("acme") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("org activity stream never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitFor := func(needle string) string {
		for {
			_, hdr, body := rec.snapshot()
			if ct := hdr.Get("Content-Type"); ct != "" && ct != "text/event-stream; charset=utf-8" {
				t.Fatalf("content-type = %q", ct)
			}
			if strings.Contains(body, needle) {
				return body
			}
			if time.Now().After(deadline) {
				t.Fatalf("never saw %q in %q", needle, body)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	body := waitFor("event: org_activity")
	if !strings.HasPrefix(body, ": walgit\n\n") {
		t.Fatalf("missing opener: %q", body)
	}
	if strings.Contains(body, `"org":"other"`) || strings.Contains(body, "event: issue") {
		t.Fatalf("foreign frame leaked: %q", body)
	}
	// A live emission rides the same connection (a foreign-kind live
	// frame on the same key is dropped, never breaking the stream).
	x.svc.EmitOrgEvent(ctx, "acme", "team_created", "", "team acme/devs created")
	x.svc.PublishFrame(RepoFrame{Name: "issue", Repo: "acme"})
	body = waitFor(`"action":"team_created"`)
	found := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"action":"team_created"`) {
			var got map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
				t.Fatal(err)
			}
			if got["seq"] != float64(2) || got["org"] != "acme" {
				t.Fatalf("frame = %v", got)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("live frame missing in %q", body)
	}
}

func TestTrayOrgActivityDegradesToEmpty(t *testing.T) {
	x := newHarness(t)
	// Unreachable through the handler (org/after pre-validated) — the
	// read degrades to an empty page instead of failing the request.
	if got, more := x.svc.trayOrgActivity(context.Background(), "ACME!", -1, 0); len(got) != 0 || more {
		t.Fatalf("page = %+v more=%v, want empty + false", got, more)
	}
}

func TestRetainOrgEvents(t *testing.T) {
	ctx := context.Background()
	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("hookless compacts past the floor", func(t *testing.T) {
		x := newHarness(t)
		x.svc.Now = func() time.Time { return old }
		seedOrgEvents(x, "acme", "member_added", "team_created", "invite_created")
		x.svc.Now = func() time.Time { return old.AddDate(0, 0, 30) }
		x.svc.retainOrgEvents(ctx, "acme", old.AddDate(0, 0, 30))
		got, more, err := x.svc.ListOrgActivity(ctx, "acme", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		// The head event always survives (the floor deletes strictly
		// below the cursor/head — the retainRepoEvents contract).
		if !equalInts(activitySeqs(got), []int{3}) || more {
			t.Fatalf("page = %v more=%v, want [3] + false", activitySeqs(got), more)
		}
	})

	t.Run("fresh events survive", func(t *testing.T) {
		x := newHarness(t)
		seedOrgEvents(x, "acme", "member_added")
		x.svc.retainOrgEvents(ctx, "acme", x.now)
		got, _, err := x.svc.ListOrgActivity(ctx, "acme", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !equalInts(activitySeqs(got), []int{1}) {
			t.Fatalf("seqs = %v, want [1]", activitySeqs(got))
		}
	})

	t.Run("hook cursor holds the floor", func(t *testing.T) {
		x := newHarness(t)
		x.svc.Now = func() time.Time { return old }
		seedOrgEvents(x, "acme", "member_added", "team_created", "invite_created", "member_removed")
		hk, err := x.svc.CreateOrgHook(ctx, "acme", "amy@example.com", HookSpec{URL: strPtr("https://hooks.example/x")})
		if err != nil {
			t.Fatal(err)
		}
		// Cursor at 3: seqs below it are collectible, 3..4 held.
		advanceCursorAt(ctx, x.svc, OrgHookCursorKey("acme", hk.ID), 3)
		now := old.AddDate(0, 0, 30)
		x.svc.Now = func() time.Time { return now }
		x.svc.retainOrgEvents(ctx, "acme", now)
		got, _, err := x.svc.ListOrgActivity(ctx, "acme", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !equalInts(activitySeqs(got), []int{4, 3}) {
			t.Fatalf("seqs = %v, want [4 3]", activitySeqs(got))
		}
	})

	t.Run("inactive hooks hold nothing", func(t *testing.T) {
		x := newHarness(t)
		x.svc.Now = func() time.Time { return old }
		seedOrgEvents(x, "acme", "member_added")
		inactive := false
		if _, err := x.svc.CreateOrgHook(ctx, "acme", "amy@example.com",
			HookSpec{URL: strPtr("https://hooks.example/x"), Active: &inactive}); err != nil {
			t.Fatal(err)
		}
		now := old.AddDate(0, 0, 30)
		x.svc.retainOrgEvents(ctx, "acme", now)
		got, _, err := x.svc.ListOrgActivity(ctx, "acme", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !equalInts(activitySeqs(got), []int{1}) {
			t.Fatalf("seqs = %v, want [1] (inactive hook compacts like hookless: head survives)", activitySeqs(got))
		}
	})

	t.Run("unknown org is a no-op", func(t *testing.T) {
		x := newHarness(t)
		x.svc.retainOrgEvents(ctx, "ghost", x.now) // must not panic or create state
		if head := x.svc.orgHead(ctx, "ghost"); head != 0 {
			t.Fatalf("head = %d, want 0", head)
		}
	})
}

func TestOrgActivityStreamWriteFailure(t *testing.T) {
	x := newHarness(t)
	// Writer that accepts the opener then fails: both event-write
	// failures (replay loop and live loop) return instead of spinning.
	w := &failAfterN{rec: httptest.NewRecorder(), allow: 1}
	r := httptest.NewRequest("GET", "/api/v1/orgs/acme/activity/stream", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.handler.orgActivityStream(w, r, "acme", auth.Principal{Name: "amy@example.com"})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for x.svc.repoLiveCount("acme") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A live frame hits the failed writer and ends the stream.
	x.svc.EmitOrgEvent(context.Background(), "acme", "member_added", "amy@example.com", "amy joined acme")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("failed write never ended the stream")
	}
}

// failListStore (cover_test.go) fails every LIST: the hooks LIST fails,
// so retention keeps the log (fail closed toward keeping — a lost race
// defers to the next pass).
func TestRetainOrgEventsListFailureKeepsLog(t *testing.T) {
	x := newHarness(t)
	seedOrgEvents(x, "acme", "member_added")
	inner := x.svc.Store
	x.svc.Store = failListStore{ObjectStore: inner}
	x.svc.retainOrgEvents(context.Background(), "acme", x.now.AddDate(0, 0, 30))
	x.svc.Store = inner
	got, _, err := x.svc.ListOrgActivity(context.Background(), "acme", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(activitySeqs(got), []int{1}) {
		t.Fatalf("seqs = %v, want [1] (failed LIST keeps everything)", activitySeqs(got))
	}
}
