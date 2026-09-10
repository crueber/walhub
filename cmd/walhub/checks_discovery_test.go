// checks_discovery_test.go — Forgejo issue #271 (checks ARE dynamically
// reportable, but discovery + /api docs omitted them): the composition
// half of the wiring. newChecksService must register checks'
// ExposedTemplates via api.RegisterExposed so GET /api/v1 lists the
// surface; the template↔route correspondence itself is pinned in
// internal/checks (TestExposedCoversRoutes). Discovery is read through
// the exported api.Mount surface (the shipped document, not internals).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/checks"
)

// discoveryEndpointsGET reads endpoints[] off a fresh api.Mount (zero Env:
// discovery is AuthOpen, so no store/config is needed).
func discoveryEndpointsGET(t *testing.T) map[string]bool {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1", nil)
	rec := httptest.NewRecorder()
	api.Mount(&api.Env{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1 = %d, want 200", rec.Code)
	}
	var doc struct {
		Endpoints []string `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	out := make(map[string]bool, len(doc.Endpoints))
	for _, e := range doc.Endpoints {
		out[e] = true
	}
	return out
}

func TestNewChecksServiceRegistersDiscovery(t *testing.T) {
	if len(checks.ExposedTemplates) == 0 {
		t.Fatal("checks.ExposedTemplates is empty (nothing to register)")
	}
	// RegisterExposed is additive and render-deduped, so asserting the
	// post-call document is robust to other tests registering first.
	newChecksService(nil, nil, nil, nil, "git")
	after := discoveryEndpointsGET(t)
	for _, tmpl := range checks.ExposedTemplates {
		if !after[tmpl] {
			t.Errorf("GET /api/v1 lacks %q after newChecksService (endpoints %v)",
				tmpl, after)
		}
	}
}
