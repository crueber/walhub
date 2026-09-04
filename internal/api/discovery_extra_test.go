// discovery_extra_test.go — the additive feature-route registry
// (docs/features/10): RegisterExposed appends feature-owned templates
// (served by ExtraRoutes, which core must not import) to the discovery
// endpoints[], deduped, after the table-derived entries. Save/restore
// keeps the exact-list TestDiscoveryShape order-independent.
package api

import (
	"testing"
)

func TestRegisterExposed(t *testing.T) {
	exposedMu.Lock()
	saved := exposedExtra
	exposedExtra = nil
	exposedMu.Unlock()
	defer func() {
		exposedMu.Lock()
		exposedExtra = saved
		exposedMu.Unlock()
	}()
	base := len(discoveryEndpoints())
	RegisterExposed("/api/v1/repos/imports", "/api/v1/repos/imports", "/api/v1/repos/imports/{id}")
	eps := discoveryEndpoints()
	if len(eps) != base+2 {
		t.Fatalf("endpoints = %v, want base+2 (deduped)", eps)
	}
	if eps[base] != "/api/v1/repos/imports" || eps[base+1] != "/api/v1/repos/imports/{id}" {
		t.Fatalf("extras = %v", eps[base:])
	}
}
