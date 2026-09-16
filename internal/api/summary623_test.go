// summary623_test.go — Forgejo #623 push-mirror projection on the repo
// summary: the wire field (nil on repos without one, independent of the
// pull-mirror field), and the ETag story for outcome-only changes (same
// head sha, new result → ~p suffix busts the cache).
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSummaryPushMirrorWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	// No hook → no push_mirror field at all (never null).
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"push_mirror"`) {
		t.Fatalf("push_mirror field without hook: %s", w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`~k0~t111111"` {
		t.Fatalf("etag = %q, want head sha + ~k0 + ~t111111", etag)
	}

	// Hook declining → same shape.
	f.env.PushMirrorSummary = func(ctx context.Context, owner, repo string) (PushMirrorView, bool) {
		return PushMirrorView{}, false
	}
	w = f.req("GET", "/demo/walgit/api")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"push_mirror"`) {
		t.Fatalf("declined hook: %d %s", w.Code, w.Body.String())
	}

	// Hook projecting → the view renders + the ETag carries ~p.
	// Secrets never appear: the hook only sets presence + hint.
	f.env.PushMirrorSummary = func(ctx context.Context, owner, repo string) (PushMirrorView, bool) {
		if owner != "demo" || repo != "walgit" {
			return PushMirrorView{}, false
		}
		return PushMirrorView{
			UpstreamURL: "https://example.com/a.git", AuthKind: "token",
			HasSecret: true, SecretHint: "••••9999", Schedule: "",
			LastResult: "ok", Due: false,
		}, true
	}
	w = f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		PushMirror *PushMirrorView `json:"push_mirror"`
	}
	decodeJSON(t, w, &body)
	if body.PushMirror == nil || body.PushMirror.UpstreamURL != "https://example.com/a.git" || !body.PushMirror.HasSecret {
		t.Fatalf("push_mirror = %+v", body.PushMirror)
	}
	if strings.Contains(w.Body.String(), `"token":"`) || strings.Contains(w.Body.String(), "tok-secret") {
		t.Fatalf("secret material on the wire: %s", w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if !strings.Contains(etag, "~p") {
		t.Fatalf("etag = %q, want ~p suffix", etag)
	}

	// An outcome-only change (same head, new result) must NOT 304.
	f.env.PushMirrorSummary = func(ctx context.Context, owner, repo string) (PushMirrorView, bool) {
		return PushMirrorView{
			UpstreamURL: "https://example.com/a.git", AuthKind: "token",
			HasSecret: true, LastResult: "failed: upstream gone", Due: false,
		}, true
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": etag}, readP())
	if w.Code != 200 {
		t.Fatalf("stale push-mirror etag must revalidate to 200, got %d", w.Code)
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": w.Header().Get("ETag")}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current push-mirror etag must 304, got %d", w.Code)
	}
}

func TestSummaryPushMirrorIndependentOfPull(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	// Both hooks set: both fields render (either may exist alone).
	f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
		return MirrorView{UpstreamURL: "https://example.com/pull.git", Schedule: "daily"}, true
	}
	f.env.PushMirrorSummary = func(ctx context.Context, owner, repo string) (PushMirrorView, bool) {
		return PushMirrorView{UpstreamURL: "https://example.com/push.git", AuthKind: "none"}, true
	}
	w := f.req("GET", "/demo/walgit/api")
	var body struct {
		Mirror     *MirrorView     `json:"mirror"`
		PushMirror *PushMirrorView `json:"push_mirror"`
	}
	decodeJSON(t, w, &body)
	if body.Mirror == nil || body.PushMirror == nil {
		t.Fatalf("both projections must render: %+v %+v", body.Mirror, body.PushMirror)
	}
	if body.Mirror.UpstreamURL == body.PushMirror.UpstreamURL {
		t.Fatal("projections share state")
	}
	if etag := w.Header().Get("ETag"); !strings.Contains(etag, "~m") || !strings.Contains(etag, "~p") {
		t.Fatalf("etag = %q, want both ~m and ~p", etag)
	}
}

// TestSummaryPushMirrorHostKeyETag — Forgejo #625: learning host-key
// trust changes neither the head sha nor any outcome field, so the ~p
// ETag input must cover the new fields or a revalidating client 304s
// and keeps showing "not yet observed". The wire carries the
// fingerprint + accepted-at (presence-style, never key material).
func TestSummaryPushMirrorHostKeyETag(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	base := PushMirrorView{
		UpstreamURL: "ssh://example.com/a.git", AuthKind: "ssh",
		HasSecret: true, LastResult: "ok", Due: false,
	}
	f.env.PushMirrorSummary = func(ctx context.Context, owner, repo string) (PushMirrorView, bool) {
		return base, true
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"host_key_fingerprint"`) {
		t.Fatalf("untrusted mirror must omit host_key_fingerprint: %s", w.Body.String())
	}
	etagBefore := w.Header().Get("ETag")

	// Trust learned: same head, same outcome — new fields on the wire,
	// new ETag (no 304), and no key material anywhere.
	base.HostKeyFingerprint = "SHA256:abcdefghij1234567890123456789012345678901"
	base.HostKeyAcceptedAt = "2026-09-16T12:00:00Z"
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": etagBefore}, readP())
	if w.Code != 200 {
		t.Fatalf("learned-trust etag must revalidate to 200, got %d", w.Code)
	}
	etagAfter := w.Header().Get("ETag")
	if etagAfter == etagBefore {
		t.Fatal("~p ETag input must change when trust is learned")
	}
	var body struct {
		PushMirror *PushMirrorView `json:"push_mirror"`
	}
	decodeJSON(t, w, &body)
	if body.PushMirror == nil || body.PushMirror.HostKeyFingerprint != base.HostKeyFingerprint ||
		body.PushMirror.HostKeyAcceptedAt != base.HostKeyAcceptedAt {
		t.Fatalf("push_mirror = %+v", body.PushMirror)
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": etagAfter}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current host-key etag must 304, got %d", w.Code)
	}
}
