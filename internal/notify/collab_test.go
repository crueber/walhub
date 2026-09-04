// collab_test.go — Feature 08 §4: the repo collaboration stream.
//
// Table-driven httptest: route matching on both lanes, method gating,
// read-gate auth, the 07 §6 envelope (opener, content-type, no-store),
// ring replay for late attachers, live frames until client cancel,
// unknown-kind drops, and the frameFor kind table.
package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollabRouteTable(t *testing.T) {
	x := newHarness(t)
	authed := "amy@example.com"
	rows := []struct {
		name      string
		method    string
		path      string
		principal string
		want      int
	}{
		{"anon denied", "GET", "/acme/repo/api/collab/stream", "", http.StatusUnauthorized},
		{"browser lane anon denied", "GET", "/acme/repo/api-browser/collab/stream", "", http.StatusUnauthorized},
		{"post rejected", "POST", "/acme/repo/api/collab/stream", authed, http.StatusMethodNotAllowed},
		{"unknown repo id reports false", "GET", "/Bad%20Owner/repo/api/collab/stream", authed, -1},
		{"prefix without stream needs admin", "GET", "/acme/repo/api/collab", authed, http.StatusForbidden},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			r := req(t, tc.method, tc.path, tc.principal)
			rec := httptest.NewRecorder()
			handled := x.handler.Handle(rec, r)
			if tc.want == -1 {
				if handled {
					t.Fatalf("Handle = true, want false")
				}
				return
			}
			if !handled {
				t.Fatal("Handle = false, want true")
			}
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestFrameForKinds(t *testing.T) {
	for kind := range collabKinds {
		f, ok := frameFor(RepoFrame{Name: kind, Repo: "acme/repo", Num: 7, At: "t"})
		if !ok {
			t.Fatalf("kind %q dropped", kind)
		}
		if f.Kind != kind || f.Num != 7 {
			t.Fatalf("kind %q mapped to %+v", kind, f)
		}
	}
	if _, ok := frameFor(RepoFrame{Name: "bogus-future-kind"}); ok {
		t.Fatal("unknown kind must drop")
	}
}

func TestCollabStreamReplayAndLive(t *testing.T) {
	x := newHarness(t)
	who := "amy@example.com"
	// Seed the ring with two frames (one unknown-kind, dropped on read).
	x.svc.PublishFrame(RepoFrame{Name: "issue", Repo: "acme/repo", Num: 3, Seq: 12})
	x.svc.PublishFrame(RepoFrame{Name: "bogus", Repo: "acme/repo"})
	x.svc.PublishFrame(RepoFrame{Name: "check", Repo: "acme/repo", Sha: strings.Repeat("a", 40), Context: "ci", State: "success", Combined: "success"})

	rec := newSafeRecorder()
	r := httptest.NewRequest("GET", "/acme/repo/api/collab/stream", nil)
	r.Header.Set("X-Test-Principal", who)
	rctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(rctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.handler.ServeHTTP(rec, r)
	}()
	deadline := time.Now().Add(5 * time.Second)
	// Wait for the subscription first (the bus-mutex edge orders the
	// SSE header writes before every snapshot below — -race clean).
	for x.svc.repoLiveCount("acme/repo") == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("collab stream never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitFor := func(needle string) string {
		for {
			_, hdr, body := rec.snapshot()
			if ct := hdr.Get("Content-Type"); ct != "" && ct != "text/event-stream; charset=utf-8" {
				cancel()
				t.Fatalf("content-type = %q", ct)
			}
			if strings.Contains(body, needle) {
				return body
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("never saw %q in %q", needle, body)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	body := waitFor("event: check")
	if !strings.HasPrefix(body, ": walgit\n\n") {
		cancel()
		t.Fatalf("missing opener: %q", body)
	}
	if !strings.Contains(body, "event: issue") {
		cancel()
		t.Fatalf("ring replay missed issue frame: %q", body)
	}
	if strings.Contains(body, "bogus") {
		cancel()
		t.Fatalf("unknown kind leaked: %q", body)
	}
	// A live frame rides the same connection.
	x.svc.PublishFrame(RepoFrame{Name: "pull", Repo: "acme/repo", Action: "opened", Num: 9, Title: "T", State: "open", Sha: strings.Repeat("b", 40)})
	body = waitFor("event: pull")
	var got collabFrame
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"kind":"pull"`) {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got)
		}
	}
	if got.Num != 9 || got.Action != "opened" || got.Sha != strings.Repeat("b", 40) {
		cancel()
		t.Fatalf("pull frame = %+v", got)
	}
	cancel()
	<-done
	_, hdr, _ := rec.snapshot()
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache = %q, want no-store", cc)
	}
}

func TestCollabStreamOtherRepoIsolated(t *testing.T) {
	x := newHarness(t)
	who := "amy@example.com"
	x.svc.PublishFrame(RepoFrame{Name: "issue", Repo: "acme/other", Num: 1})
	rec := newSafeRecorder()
	r := httptest.NewRequest("GET", "/acme/repo/api/collab/stream", nil)
	r.Header.Set("X-Test-Principal", who)
	rctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(rctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.handler.ServeHTTP(rec, r)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for x.svc.repoLiveCount("acme/repo") == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("collab stream never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	_, _, body := rec.snapshot()
	if strings.Contains(body, "acme/other") {
		cancel()
		t.Fatalf("cross-repo leak: %q", body)
	}
	cancel()
	<-done
}
