// collab_discovery_test.go — Forgejo issue #272 (discovery + /api docs
// omitted every feature surface but imports/checks): the composition half
// of the wiring. Every ExtraRoutes constructor must register its
// ExposedTemplates via api.RegisterExposed so GET /api/v1 lists the
// surface; the template↔route correspondence itself is pinned in each
// feature package (TestExposedCoversRoutes). Discovery is read through
// the exported api.Mount surface (the shipped document, not internals).
//
// The test calls the constructors only — never buildCollab — because the
// task-kind registrations (repoimport.RegisterKind, mirror.RegisterKind)
// panic on duplicates and buildCollab is already exercised by the push
// budget test in this same binary.
package main

import (
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/issues"
	"git.packden.us/crueber/walhub/internal/notify"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/releases"
	"git.packden.us/crueber/walhub/internal/review"
	"git.packden.us/crueber/walhub/internal/social"
	"git.packden.us/crueber/walhub/internal/tags"
)

// TestCollabServicesRegisterDiscovery builds every collaboration surface
// exactly as serveHTTP does and asserts its templates reach the shipped
// discovery document. RegisterExposed is additive and render-deduped, so
// asserting the post-call document is robust to other tests registering
// first.
func TestCollabServicesRegisterDiscovery(t *testing.T) {
	cfg := config.Defaults()
	newIdentityService(nil, cfg)
	newIssuesService(nil, nil, cfg)
	newPullsService(nil, nil, nil, nil, "git")
	newReviewService(nil, nil, nil)
	newReleasesService(nil, nil, nil, "git", "", 0)
	newSocialService(nil, nil)
	newNotifyService(nil, nil)
	newTagsService(nil, nil, nil, "git")

	want := [][]string{
		issues.ExposedTemplates,
		identity.ExposedTemplates,
		pulls.ExposedTemplates,
		review.ExposedTemplates,
		releases.ExposedTemplates,
		social.ExposedTemplates,
		notify.ExposedTemplates,
		tags.ExposedTemplates,
	}
	after := discoveryEndpointsGET(t)
	for _, templates := range want {
		if len(templates) == 0 {
			t.Error("ExposedTemplates is empty (nothing to register)")
			continue
		}
		for _, tmpl := range templates {
			if !after[tmpl] {
				t.Errorf("GET /api/v1 lacks %q after collab wiring (endpoints %v)",
					tmpl, after)
			}
		}
	}
}
