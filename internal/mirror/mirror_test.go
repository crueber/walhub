// mirror_test.go — schedule mapping, next-fire, backoff, and the
// sidecar bucket discipline (R1 (b)/(d)/(f)).
package mirror

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestPresetsParse(t *testing.T) {
	for _, p := range Presets {
		cron, err := CronFor(p)
		if err != nil {
			t.Fatalf("CronFor(%q): %v", p, err)
		}
		anchor := time.Date(2026, 9, 9, 12, 34, 56, 0, time.UTC)
		next, err := cron.Next(anchor)
		if err != nil {
			t.Fatalf("Next(%q): %v", p, err)
		}
		if !next.After(anchor) {
			t.Fatalf("Next(%q) = %v, not after anchor", p, next)
		}
	}
	if !ValidPreset(DefaultPreset) {
		t.Fatalf("default preset %q invalid", DefaultPreset)
	}
	if _, err := CronFor("never"); err == nil {
		t.Fatal("CronFor(never) succeeded")
	}
	if ValidPreset("never") {
		t.Fatal("ValidPreset(never) true")
	}
}

func TestPresetFireShapes(t *testing.T) {
	anchor := time.Date(2026, 9, 9, 12, 34, 56, 0, time.UTC) // a Wednesday
	cases := map[string]time.Duration{
		PresetHourly:  time.Hour,
		Preset8H:      8 * time.Hour,
		PresetDaily:   24 * time.Hour,
		PresetWeekly:  7 * 24 * time.Hour,
		PresetMonthly: 31 * 24 * time.Hour, // upper bound: next month's 1st at worst
	}
	for preset, maxGap := range cases {
		cron, err := CronFor(preset)
		if err != nil {
			t.Fatalf("%s: %v", preset, err)
		}
		next, err := cron.Next(anchor)
		if err != nil {
			t.Fatalf("%s next: %v", preset, err)
		}
		if gap := next.Sub(anchor); gap <= 0 || gap > maxGap {
			t.Fatalf("%s gap = %v, want (0, %v]", preset, gap, maxGap)
		}
	}
	// Hourly fires on the hour; daily at midnight UTC.
	hourly, _ := CronFor(PresetHourly)
	if next, _ := hourly.Next(anchor); next.Minute() != 0 || next.Second() != 0 {
		t.Fatalf("hourly next = %v, want top of hour", next)
	}
	daily, _ := CronFor(PresetDaily)
	if next, _ := daily.Next(anchor); next.Hour() != 0 || next.Minute() != 0 {
		t.Fatalf("daily next = %v, want midnight", next)
	}
}

func TestNextFire(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// Never synced: due immediately, no fire time.
	doc := &MirrorDoc{Version: 1, UpstreamURL: "https://example.com/a.git", Schedule: PresetDaily}
	if fire, due, err := NextFire(doc, now); err != nil || !due || !fire.IsZero() {
		t.Fatalf("never-synced = %v %v %v, want zero true nil", fire, due, err)
	}
	// Anchored: next midnight after the anchor.
	doc.LastSyncedAt = "2026-09-09T00:00:00Z"
	fire, due, err := NextFire(doc, now)
	if err != nil {
		t.Fatalf("anchored: %v", err)
	}
	if want := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC); !fire.Equal(want) {
		t.Fatalf("fire = %v, want %v", fire, want)
	}
	if due {
		t.Fatal("due at noon for a midnight-anchored daily")
	}
	if _, due, _ := NextFire(doc, fire); !due {
		t.Fatal("not due exactly at fire time")
	}
	// A failed sync never moves the anchor: LastSyncedAt is the only input.
	doc.ConsecutiveFailures = 3
	doc.LastResult = "failed: boom"
	doc.LastAttemptAt = "2026-09-09T12:00:00Z"
	if fire2, _, _ := NextFire(doc, now); !fire2.Equal(fire) {
		t.Fatalf("failure moved next fire: %v vs %v", fire2, fire)
	}
	// Nil doc / bad schedule / bad anchor.
	if _, _, err := NextFire(nil, now); err == nil {
		t.Fatal("nil doc succeeded")
	}
	bad := &MirrorDoc{Schedule: "never"}
	if _, _, err := NextFire(bad, now); err == nil {
		t.Fatal("bad schedule succeeded")
	}
	badAnchor := &MirrorDoc{Schedule: PresetDaily, LastSyncedAt: "not-a-time"}
	if _, due, err := NextFire(badAnchor, now); err != nil || !due {
		t.Fatalf("bad anchor = %v %v, want due-nil", due, err)
	}
}

func TestBackoff(t *testing.T) {
	if BackoffDelay(0) != 0 || BackoffDelay(-2) != 0 {
		t.Fatal("zero failures must have zero delay")
	}
	if got, want := BackoffDelay(1), 15*time.Minute; got != want {
		t.Fatalf("f1 = %v, want %v", got, want)
	}
	if got, want := BackoffDelay(2), 30*time.Minute; got != want {
		t.Fatalf("f2 = %v, want %v", got, want)
	}
	if got := BackoffDelay(100); got != 24*time.Hour {
		t.Fatalf("f100 = %v, want cap 24h", got)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	doc := &MirrorDoc{Schedule: PresetHourly, LastSyncedAt: "2026-09-09T10:00:00Z"}
	if BackedOff(doc, now) {
		t.Fatal("no failures but backed off")
	}
	if Due(doc, now.Add(-3*time.Hour)) {
		t.Fatal("due returned true before fire")
	}
	doc.ConsecutiveFailures = 1
	doc.LastAttemptAt = "2026-09-09T11:55:00Z" // 5m ago, delay 15m → backed off
	if !BackedOff(doc, now) {
		t.Fatal("not backed off inside the window")
	}
	if Due(doc, now.Add(5*time.Minute)) {
		t.Fatal("Due true inside backoff window")
	}
	if Due(doc, now.Add(-time.Hour)) {
		t.Fatal("Due true before fire even with failures")
	}
	doc.LastAttemptAt = "2026-09-09T11:00:00Z" // 60m ago → window passed
	if BackedOff(doc, now) {
		t.Fatal("still backed off past the window")
	}
	doc.LastAttemptAt = "not-a-time"
	if BackedOff(doc, now) {
		t.Fatal("bad attempt time backed off")
	}
	if Due(nil, now) {
		t.Fatal("nil doc due")
	}
}

func TestSidecarCRUD(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	// Absent → nil probe; IsMirror false.
	if doc, ver, err := Load(ctx, st, "acme", "m"); err != nil || doc != nil || ver != "" {
		t.Fatalf("absent load = %+v %q %v", doc, ver, err)
	}
	if IsMirror(ctx, st, "acme", "m") {
		t.Fatal("absent IsMirror true")
	}
	// Bad preset refuses.
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", "never"); err == nil {
		t.Fatal("bad preset created")
	}
	doc, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.Version != 1 || doc.Schedule != PresetDaily {
		t.Fatalf("doc = %+v", doc)
	}
	if !IsMirror(ctx, st, "acme", "m") {
		t.Fatal("present IsMirror false")
	}
	// Duplicate create → 412 (the caller maps to 409).
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily); !store.IsPreconditionFailed(err) {
		t.Fatalf("duplicate create = %v, want precondition-failed", err)
	}
	// Load + CAS update + conflict.
	loaded, ver, err := Load(ctx, st, "acme", "m")
	if err != nil || loaded == nil || ver == "" {
		t.Fatalf("load = %+v %q %v", loaded, ver, err)
	}
	loaded.Schedule = PresetHourly
	if err := UpdateCAS(ctx, st, "acme", "m", loaded, ver); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := UpdateCAS(ctx, st, "acme", "m", loaded, ver); !store.IsPreconditionFailed(err) {
		t.Fatalf("stale CAS = %v, want precondition-failed", err)
	}
	// Corrupt body: Load errors, IsMirror fails closed (true).
	raw := []byte("{oops")
	if _, err := store.PutBytes(ctx, st, store.MirrorKey("acme", "m"), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: mustVersion(t, ctx, st, "acme", "m")}); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	if _, _, err := Load(ctx, st, "acme", "m"); err == nil {
		t.Fatal("corrupt load succeeded")
	}
	if !IsMirror(ctx, st, "acme", "m") {
		t.Fatal("corrupt IsMirror false (must fail closed)")
	}
	// Delete (twice: idempotent).
	if err := Delete(ctx, st, "acme", "m"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := Delete(ctx, st, "acme", "m"); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
	if IsMirror(ctx, st, "acme", "m") {
		t.Fatal("deleted IsMirror true")
	}
}

func mustVersion(t *testing.T, ctx context.Context, st store.ObjectStore, owner, name string) store.Version {
	t.Helper()
	_, meta, err := store.GetBytes(ctx, st, store.MirrorKey(owner, name), store.GetOptions{})
	if err != nil {
		t.Fatalf("version probe: %v", err)
	}
	return meta.Version
}

func TestRecordAttempt(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// Missing sidecar mid-sync: no home, no error.
	if err := RecordAttempt(ctx, st, "acme", "gone", true, "", now); err != nil {
		t.Fatalf("missing record: %v", err)
	}
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	if err := RecordAttempt(ctx, st, "acme", "m", false, "boom", now); err != nil {
		t.Fatalf("fail record: %v", err)
	}
	doc, _, _ := Load(ctx, st, "acme", "m")
	if doc.ConsecutiveFailures != 1 || doc.LastResult != "failed: boom" || doc.LastSyncedAt != "" || doc.LastAttemptAt == "" {
		t.Fatalf("fail doc = %+v", doc)
	}
	if err := RecordAttempt(ctx, st, "acme", "m", false, "again", now); err != nil {
		t.Fatal(err)
	}
	doc, _, _ = Load(ctx, st, "acme", "m")
	if doc.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d, want 2", doc.ConsecutiveFailures)
	}
	if err := RecordAttempt(ctx, st, "acme", "m", true, "", now); err != nil {
		t.Fatal(err)
	}
	doc, _, _ = Load(ctx, st, "acme", "m")
	if doc.ConsecutiveFailures != 0 || doc.LastResult != "ok" || doc.LastSyncedAt == "" {
		t.Fatalf("success doc = %+v", doc)
	}
}

func TestSetSchedule(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := SetSchedule(ctx, st, "acme", "m", PresetHourly); err == nil {
		t.Fatal("schedule on non-mirror succeeded")
	}
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	if _, err := SetSchedule(ctx, st, "acme", "m", "never"); err == nil {
		t.Fatal("bad preset scheduled")
	}
	updated, err := SetSchedule(ctx, st, "acme", "m", PresetWeekly)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if updated.Schedule != PresetWeekly || updated.UpstreamURL != "https://example.com/a.git" {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestViewOf(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	doc := &MirrorDoc{Version: 1, UpstreamURL: "https://example.com/a.git", Schedule: PresetDaily,
		LastSyncedAt: "2026-09-09T00:00:00Z", LastResult: "ok"}
	v := ViewOf(doc, now)
	if v.UpstreamURL != doc.UpstreamURL || v.Schedule != PresetDaily || v.LastResult != "ok" || v.Due {
		t.Fatalf("view = %+v", v)
	}
	if v.NextSyncAt != "2026-09-10T00:00:00Z" {
		t.Fatalf("next = %q", v.NextSyncAt)
	}
	// Never synced: due, no next time.
	v = ViewOf(&MirrorDoc{Schedule: PresetHourly, UpstreamURL: "u"}, now)
	if !v.Due || v.NextSyncAt != "" {
		t.Fatalf("fresh view = %+v", v)
	}
	// Broken schedule: zero view, no crash.
	bad := &MirrorDoc{Schedule: "never", UpstreamURL: "u"}
	if v := ViewOf(bad, now); v.NextSyncAt != "" || v.Due {
		t.Fatalf("bad view = %+v", v)
	}
}

func TestRegisterKindDuplicate(t *testing.T) {
	// Hermetic across -count=N and shuffles: the kinds map is
	// process-global, so a previous run's registration would trip the
	// duplicate panic on the FIRST call below, outside the recover
	// (Forgejo #397).
	ResetKindsForTest()
	RegisterKind("mirror-test-kind")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("duplicate kind did not panic")
		}
	}()
	RegisterKind("mirror-test-kind")
}

func TestSmallHelpers(t *testing.T) {
	if isZeroHex("") || !isZeroHex("0000") || isZeroHex("0001") {
		t.Fatal("isZeroHex broken")
	}
	if zeroHex(4) != "0000" || zeroHex(0) != strings.Repeat("0", 40) {
		t.Fatal("zeroHex broken")
	}
	if shortOid("abcdef123") != "abcdef1" || shortOid("ab") != "ab" {
		t.Fatal("shortOid broken")
	}
	if shortList([]string{"a", "b"}) != "a, b" {
		t.Fatal("shortList broken")
	}
	if o, n, ok := splitRepo("acme/m"); !ok || o != "acme" || n != "m" {
		t.Fatal("splitRepo broken")
	}
	for _, bad := range []string{"", "a", "/b", "a/", "a/b/c"} {
		if _, _, ok := splitRepo(bad); ok && bad != "a/b/c" {
			t.Fatalf("splitRepo(%q) ok", bad)
		}
	}
	if _, _, ok := splitRepo("a/b/c"); !ok {
		t.Fatal("splitRepo(a/b/c) must split on first slash")
	}
}
