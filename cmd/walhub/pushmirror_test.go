// pushmirror_test.go — Forgejo #623 composition: the wire mapping is
// pure (no kind registration — buildCollab owns that), the hook is
// nil-safe, and the discovery templates are registered from the same
// change (law 12, the #272 rule).
package main

import (
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/pushmirror"
)

func TestPushMirrorViewOf(t *testing.T) {
	v := pushMirrorViewOf(pushmirror.View{
		UpstreamURL: "file:///x", AuthKind: "token", Username: "u",
		HasSecret: true, SecretHint: "••••1", Schedule: "daily",
		NextSyncAt: "2026-09-16T00:00:00Z", LastSyncedAt: "2026-09-15T00:00:00Z",
		LastResult: "ok", Due: false,
	})
	wire := api.PushMirrorView{
		UpstreamURL: "file:///x", AuthKind: "token", Username: "u",
		HasSecret: true, SecretHint: "••••1", Schedule: "daily",
		NextSyncAt: "2026-09-16T00:00:00Z", LastSyncedAt: "2026-09-15T00:00:00Z",
		LastResult: "ok", Due: false,
	}
	if v != wire {
		t.Errorf("viewOf = %+v, want %+v", v, wire)
	}
}

func TestPushMirrorHookNilSafe(t *testing.T) {
	if pushMirrorOnPushOf(nil) != nil {
		t.Error("nil service hook non-nil")
	}
	if pushMirrorHookOf(nil) != nil {
		t.Error("nil collab hook non-nil")
	}
	if pushMirrorHookOf(&collabWiring{}) != nil {
		t.Error("service-less collab hook non-nil")
	}
}

func TestPushMirrorLoopInterval(t *testing.T) {
	if pushMirrorLoopInterval != time.Minute {
		t.Errorf("interval = %v, want 1m (the follow.go shape)", pushMirrorLoopInterval)
	}
}

func TestPushMirrorExposedRegistered(t *testing.T) {
	// The templates exist (composition registers them via
	// api.RegisterExposed in newPushMirrorService — the same change,
	// law 12). buildCollab itself is exercised by the collab suite;
	// here pin the list the wiring registers.
	for _, tmpl := range pushmirror.ExposedTemplates {
		if tmpl == "" {
			t.Error("empty template")
		}
	}
	if len(pushmirror.ExposedTemplates) != 3 {
		t.Errorf("templates = %v, want the 3 repo lanes", pushmirror.ExposedTemplates)
	}
	var _ git.RepoId
}
