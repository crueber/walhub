// summary240_test.go — Forgejo #240 mirror projection on the repo
// summary: the wire field (nil on non-mirrors), and the ETag/cache
// story for outcome-only changes (same head sha, new result).
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSummaryMirrorWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	// No hook → no mirror field at all (never null).
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"mirror"`) {
		t.Fatalf("mirror field without hook: %s", w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("etag = %q, want bare head sha", etag)
	}

	// Hook declining (non-mirror) → same shape.
	f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
		return MirrorView{}, false
	}
	w = f.req("GET", "/demo/walgit/api")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"mirror"`) {
		t.Fatalf("declined hook: %d %s", w.Code, w.Body.String())
	}

	// Hook projecting → the view renders + the ETag carries ~m.
	f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
		if owner != "demo" || repo != "walgit" {
			return MirrorView{}, false
		}
		return MirrorView{
			UpstreamURL: "https://example.com/a.git", Schedule: "daily",
			NextSyncAt: "2026-09-10T00:00:00Z", LastResult: "ok", Due: false,
		}, true
	}
	w = f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Mirror *MirrorView `json:"mirror"`
	}
	decodeJSON(t, w, &body)
	if body.Mirror == nil || body.Mirror.UpstreamURL != "https://example.com/a.git" || body.Mirror.Schedule != "daily" {
		t.Fatalf("mirror = %+v", body.Mirror)
	}
	etag := w.Header().Get("ETag")
	if !strings.Contains(etag, "~m") {
		t.Fatalf("etag = %q, want ~m suffix", etag)
	}

	// An outcome-only change (same head, new result) must NOT 304.
	f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
		return MirrorView{
			UpstreamURL: "https://example.com/a.git", Schedule: "daily",
			LastResult: "failed: upstream gone", ConsecutiveFailures: 1, Due: false,
		}, true
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": etag}, readP())
	if w.Code != 200 {
		t.Fatalf("stale mirror etag must revalidate to 200, got %d", w.Code)
	}
	// The new etag 304s while the outcome is current.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": w.Header().Get("ETag")}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current mirror etag must 304, got %d", w.Code)
	}
}

func TestMirrorHash(t *testing.T) {
	a := MirrorView{UpstreamURL: "u", Schedule: "daily", LastResult: "ok"}
	b := MirrorView{UpstreamURL: "u", Schedule: "daily", LastResult: "failed: x"}
	if mirrorHash(a) == mirrorHash(b) {
		t.Fatal("outcome change did not move the hash")
	}
	if mirrorHash(a) != mirrorHash(a) {
		t.Fatal("hash unstable")
	}
}
