// mirror320_test.go — issue #320 composition: the summary hook's wire
// mapping (pure mirrorViewOf — the hook itself needs kind registration,
// which panics on duplicates and belongs to buildCollab alone in this
// binary, so it is never called here).
package main

import (
	"testing"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/mirror"
)

func TestMirrorViewOf(t *testing.T) {
	v := mirror.View{
		UpstreamURL: "https://example.com/a.git", Schedule: "hourly",
		NextSyncAt: "2026-09-12T00:00:00Z", LastSyncedAt: "2026-09-11T00:00:00Z",
		LastResult: "ok", ConsecutiveFailures: 0, Due: true,
	}
	got := mirrorViewOf(v, "")
	want := api.MirrorView{
		UpstreamURL: v.UpstreamURL, Schedule: v.Schedule,
		NextSyncAt: v.NextSyncAt, LastSyncedAt: v.LastSyncedAt,
		LastResult: v.LastResult, ConsecutiveFailures: v.ConsecutiveFailures,
		Due: v.Due,
	}
	if got != want {
		t.Fatalf("clean mapping = %+v, want %+v", got, want)
	}
	// The degraded verdict rides along (the #320 agreement field).
	got = mirrorViewOf(v, "serve sync wait timed out")
	if got.DegradedReason != "serve sync wait timed out" {
		t.Fatalf("degraded mapping = %+v", got)
	}
	if got.LastResult != "ok" {
		t.Fatalf("degraded mapping rewrote last_result = %+v", got)
	}
}
