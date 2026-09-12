package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the notify surface
// (Forgejo #272): one entry per distinct path shape, in handler order.
// Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/api/v1/notifications",
		"/api/v1/notifications/unread_count",
		"/api/v1/notifications/read_all",
		"/api/v1/notifications/stream",
		"/api/v1/notifications/{id}/read",
		"/api/v1/notifications/{id}/unread",
		"/api/v1/orgs/{org}/webhooks",
		"/api/v1/orgs/{org}/webhooks/{id}",
		"/api/v1/orgs/{org}/webhooks/{id}/ping",
		"/api/v1/orgs/{org}/webhooks/{id}/deliveries",
		"/{owner}/{repo}/api/watch",
		"/{owner}/{repo}/api/webhooks",
		"/{owner}/{repo}/api/webhooks/{id}",
		"/{owner}/{repo}/api/webhooks/{id}/ping",
		"/{owner}/{repo}/api/webhooks/{id}/deliveries",
		"/{owner}/{repo}/api/collab/stream",
	}
	if len(ExposedTemplates) != len(want) {
		t.Fatalf("ExposedTemplates = %v, want %v", ExposedTemplates, want)
	}
	for i := range want {
		if ExposedTemplates[i] != want[i] {
			t.Fatalf("ExposedTemplates[%d] = %q, want %q", i, ExposedTemplates[i], want[i])
		}
	}
}

// exposedMatch reports whether path fits template: literals must match,
// {name} matches any single non-empty segment.
func exposedMatch(template, path string) bool {
	toks := strings.Split(strings.TrimPrefix(template, "/"), "/")
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(toks) != len(segs) {
		return false
	}
	for i, tok := range toks {
		if strings.HasPrefix(tok, "{") && strings.HasSuffix(tok, "}") {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if tok != segs[i] {
			return false
		}
	}
	return true
}

// TestExposedCoversRoutes audits the full notify surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #272).
//
// The two SSE routes run against an already-cancelled context: the stream
// handlers exit on client cancel, so claiming is asserted without holding
// a live connection.
func TestExposedCoversRoutes(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "jane", Admin: true}, nil
	}
	h := x.handler

	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
		ctx       context.Context
	}{
		{"tray", "GET", "/api/v1/notifications", true, "/api/v1/notifications", nil},
		{"tray browser lane", "GET", "/api-browser/v1/notifications", true, "/api/v1/notifications", nil},
		{"tray wrong method recognized", "POST", "/api/v1/notifications", true, "/api/v1/notifications", nil},
		{"unread count", "GET", "/api/v1/notifications/unread_count", true, "/api/v1/notifications/unread_count", nil},
		{"read all", "POST", "/api/v1/notifications/read_all", true, "/api/v1/notifications/read_all", nil},
		{"stream", "GET", "/api/v1/notifications/stream", true, "/api/v1/notifications/stream", cancelled()},
		{"flip read", "POST", "/api/v1/notifications/0123456789abcdef0123456789abcdef/read", true, "/api/v1/notifications/{id}/read", nil},
		{"flip unread", "POST", "/api/v1/notifications/0123456789abcdef0123456789abcdef/unread", true, "/api/v1/notifications/{id}/unread", nil},
		{"org webhooks list", "GET", "/api/v1/orgs/acme/webhooks", true, "/api/v1/orgs/{org}/webhooks", nil},
		{"org webhooks create", "POST", "/api/v1/orgs/acme/webhooks", true, "/api/v1/orgs/{org}/webhooks", nil},
		{"org webhooks browser lane", "GET", "/api-browser/v1/orgs/acme/webhooks", true, "/api/v1/orgs/{org}/webhooks", nil},
		{"org webhook get", "GET", "/api/v1/orgs/acme/webhooks/abc", true, "/api/v1/orgs/{org}/webhooks/{id}", nil},
		{"org webhook patch", "PATCH", "/api/v1/orgs/acme/webhooks/abc", true, "/api/v1/orgs/{org}/webhooks/{id}", nil},
		{"org webhook delete", "DELETE", "/api/v1/orgs/acme/webhooks/abc", true, "/api/v1/orgs/{org}/webhooks/{id}", nil},
		{"org webhook ping", "POST", "/api/v1/orgs/acme/webhooks/abc/ping", true, "/api/v1/orgs/{org}/webhooks/{id}/ping", nil},
		{"org webhook deliveries", "GET", "/api/v1/orgs/acme/webhooks/abc/deliveries", true, "/api/v1/orgs/{org}/webhooks/{id}/deliveries", nil},
		{"watch get", "GET", "/o/r/api/watch", true, "/{owner}/{repo}/api/watch", nil},
		{"watch put", "PUT", "/o/r/api/watch", true, "/{owner}/{repo}/api/watch", nil},
		{"watch delete", "DELETE", "/o/r/api/watch", true, "/{owner}/{repo}/api/watch", nil},
		{"watch browser lane", "GET", "/o/r/api-browser/watch", true, "/{owner}/{repo}/api/watch", nil},
		{"webhooks list", "GET", "/o/r/api/webhooks", true, "/{owner}/{repo}/api/webhooks", nil},
		{"webhooks create", "POST", "/o/r/api/webhooks", true, "/{owner}/{repo}/api/webhooks", nil},
		{"webhook get", "GET", "/o/r/api/webhooks/abc", true, "/{owner}/{repo}/api/webhooks/{id}", nil},
		{"webhook patch", "PATCH", "/o/r/api/webhooks/abc", true, "/{owner}/{repo}/api/webhooks/{id}", nil},
		{"webhook delete", "DELETE", "/o/r/api/webhooks/abc", true, "/{owner}/{repo}/api/webhooks/{id}", nil},
		{"webhook ping", "POST", "/o/r/api/webhooks/abc/ping", true, "/{owner}/{repo}/api/webhooks/{id}/ping", nil},
		{"webhook deliveries", "GET", "/o/r/api/webhooks/abc/deliveries", true, "/{owner}/{repo}/api/webhooks/{id}/deliveries", nil},
		{"collab stream", "GET", "/o/r/api/collab/stream", true, "/{owner}/{repo}/api/collab/stream", cancelled()},
		// Anything else must NOT claim the notify surface (falls through
		// to the core mux or the social surface): uncovered paths need no
		// template.
		{"other family", "GET", "/o/r/api/social", false, "", nil},
		{"top level", "GET", "/api/v1/repos", false, "", nil},
		{"non repo", "GET", "/o/r/watch", false, "", nil},
	}

	covered := make([]bool, len(ExposedTemplates))
	for _, tc := range served {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.ctx != nil {
			req = req.WithContext(tc.ctx)
		}
		// SSE needs an SSE-accepting request (else 406 — still claimed,
		// but the cancelled context covers the claim either way).
		if strings.HasSuffix(tc.path, "/stream") {
			req.Header.Set("Accept", "text/event-stream")
		}
		rec := httptest.NewRecorder()
		if got := h.Handle(rec, req); got != tc.want {
			t.Errorf("%s: Handle(%s %s) = %v, want %v (status %d)",
				tc.name, tc.method, tc.path, got, tc.want, rec.Code)
			continue
		}
		if !tc.want {
			continue
		}
		hits := 0
		canonical := false
		for i, tmpl := range ExposedTemplates {
			if exposedMatch(tmpl, exposedLane(tc.path)) {
				hits++
				covered[i] = true
				if tmpl == tc.canonical {
					canonical = true
				}
			}
		}
		if hits == 0 {
			t.Errorf("%s: %s matches no template (missing discovery entry)", tc.name, tc.path)
		}
		if !canonical {
			t.Errorf("%s: %s not covered by canonical template %q",
				tc.name, tc.path, tc.canonical)
		}
	}
	for i, tmpl := range ExposedTemplates {
		if !covered[i] {
			t.Errorf("template %q covers no served route (phantom entry)", tmpl)
		}
	}
}

// exposedLane normalizes a request path to the spelling the templates use
// (both lanes serve the same shapes).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
