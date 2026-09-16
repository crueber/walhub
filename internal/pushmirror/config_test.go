// config_test.go — sidecars, schedules, backoff, redaction.
package pushmirror

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func testStore() store.ObjectStore { return store.NewMemory() }

func TestValidKindsAndSchedules(t *testing.T) {
	for _, k := range []string{AuthNone, AuthPassword, AuthToken, AuthSSH} {
		if !ValidAuthKind(k) {
			t.Errorf("ValidAuthKind(%q) = false", k)
		}
	}
	if ValidAuthKind("keys") {
		t.Error("ValidAuthKind(keys) = true")
	}
	if !ValidSchedule(ScheduleOff) {
		t.Error("off schedule invalid")
	}
	for _, p := range Presets {
		if !ValidSchedule(p) {
			t.Errorf("ValidSchedule(%q) = false", p)
		}
	}
	if ValidSchedule("*/5 * * * * *") {
		t.Error("freeform cron accepted")
	}
	if _, err := CronFor("nope"); err == nil {
		t.Error("CronFor(unknown) succeeded")
	}
	if _, err := CronFor(""); err == nil {
		t.Error("CronFor(off) succeeded — off is not a cron")
	}
	for _, p := range Presets {
		if _, err := CronFor(p); err != nil {
			t.Errorf("CronFor(%q): %v", p, err)
		}
	}
}

func TestNextFireOffAndNever(t *testing.T) {
	now := time.Now().UTC()
	off := &Doc{Version: 1, Schedule: ScheduleOff}
	if fire, due, err := NextFire(off, now); err != nil || due || !fire.IsZero() {
		t.Errorf("off NextFire = %v,%v,%v", fire, due, err)
	}
	if Due(off, now) {
		t.Error("off Due = true")
	}
	never := &Doc{Version: 1, Schedule: PresetDaily}
	if _, due, err := NextFire(never, now); err != nil || !due {
		t.Errorf("never-synced NextFire due=%v err=%v", due, err)
	}
	if !Due(never, now) {
		t.Error("never-synced Due = false")
	}
	if _, _, err := NextFire(nil, now); err == nil {
		t.Error("nil doc NextFire succeeded")
	}
}

func TestNextFireAnchored(t *testing.T) {
	anchor := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	doc := &Doc{Version: 1, Schedule: PresetDaily, LastSyncedAt: anchor.Format(time.RFC3339)}
	fire, due, err := NextFire(doc, anchor.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("daily fire due 1h after anchor")
	}
	if !fire.After(anchor) {
		t.Errorf("fire %v not after anchor", fire)
	}
	if _, due, _ := NextFire(doc, fire); !due {
		t.Error("not due at fire time")
	}
	// Failure never moves the anchor: last_attempt recent + failures.
	doc.LastAttemptAt = fire.Format(time.RFC3339)
	doc.ConsecutiveFailures = 3
	if fire2, _, _ := NextFire(doc, fire); !fire2.Equal(fire) {
		t.Errorf("failure moved next fire: %v -> %v", fire, fire2)
	}
	// Unparseable anchor fails open toward syncing.
	doc.LastSyncedAt = "garbage"
	if _, due, _ := NextFire(doc, anchor); !due {
		t.Error("unparseable anchor not due")
	}
}

func TestBackoff(t *testing.T) {
	if BackoffDelay(0) != 0 {
		t.Error("zero failures must have zero delay")
	}
	if BackoffDelay(1) != 15*time.Minute {
		t.Errorf("1 failure delay = %v", BackoffDelay(1))
	}
	if BackoffDelay(2) != 30*time.Minute {
		t.Errorf("2 failure delay = %v", BackoffDelay(2))
	}
	if BackoffDelay(100) != 24*time.Hour {
		t.Errorf("cap = %v", BackoffDelay(100))
	}
	now := time.Now().UTC()
	doc := &Doc{ConsecutiveFailures: 1, LastAttemptAt: now.Format(time.RFC3339)}
	if !BackedOff(doc, now) {
		t.Error("inside backoff window, BackedOff = false")
	}
	if BackedOff(doc, now.Add(time.Hour)) {
		t.Error("outside backoff window, BackedOff = true")
	}
	if BackedOff(nil, now) {
		t.Error("nil BackedOff = true")
	}
}

func TestSidecarCRUD(t *testing.T) {
	ctx := context.Background()
	st := testStore()
	if _, _, err := Load(ctx, st, "o", "r"); err != nil {
		t.Fatal(err)
	} else {
		_ = err
	}
	doc, _, err := Load(ctx, st, "o", "r")
	if err != nil || doc != nil {
		t.Fatalf("absent Load = %v,%v", doc, err)
	}
	if HasConfig(ctx, st, "o", "r") {
		t.Error("absent HasConfig = true")
	}
	if _, err := Create(ctx, st, "o", "r", "file:///x", "bogus", "", ""); err == nil {
		t.Error("bad auth kind created")
	}
	if _, err := Create(ctx, st, "o", "r", "file:///x", AuthNone, "", "cron"); err == nil {
		t.Error("bad schedule created")
	}
	made, err := Create(ctx, st, "o", "r", "file:///x", AuthNone, "", ScheduleOff)
	if err != nil {
		t.Fatal(err)
	}
	if made.UpstreamURL != "file:///x" || made.Schedule != ScheduleOff {
		t.Errorf("created doc = %+v", made)
	}
	if _, err := Create(ctx, st, "o", "r", "file:///x", AuthNone, "", ""); err == nil {
		t.Error("second Create succeeded (must be 412)")
	} else if !store.IsPreconditionFailed(err) {
		t.Errorf("second Create err = %v (want precondition)", err)
	}
	if !HasConfig(ctx, st, "o", "r") {
		t.Error("present HasConfig = false")
	}
	loaded, ver, err := Load(ctx, st, "o", "r")
	if err != nil || loaded == nil || loaded.UpstreamURL != "file:///x" {
		t.Fatalf("Load = %+v,%v", loaded, err)
	}
	loaded.Schedule = PresetDaily
	if err := UpdateCAS(ctx, st, "o", "r", loaded, ver); err != nil {
		t.Fatal(err)
	}
	sec, _, err := LoadSecret(ctx, st, "o", "r")
	if err != nil || sec != nil {
		t.Fatalf("absent secret = %+v,%v", sec, err)
	}
	if err := SaveSecret(ctx, st, "o", "r", &Secret{AuthKind: AuthToken, Token: "tok123456"}); err != nil {
		t.Fatal(err)
	}
	sec, _, err = LoadSecret(ctx, st, "o", "r")
	if err != nil || sec == nil || !sec.HasMaterial() || sec.SecretHint() != "••••3456" {
		t.Fatalf("secret = %+v,%v", sec, err)
	}
	if (&Secret{AuthKind: AuthNone}).HasMaterial() != true {
		t.Error("none HasMaterial = false")
	}
	if (&Secret{AuthKind: AuthToken}).HasMaterial() {
		t.Error("empty token HasMaterial = true")
	}
	now := time.Now().UTC()
	if err := RecordAttempt(ctx, st, "o", "r", false, "boom password=hunter2", now); err != nil {
		t.Fatal(err)
	}
	loaded, _, _ = Load(ctx, st, "o", "r")
	if loaded.ConsecutiveFailures != 1 || !strings.Contains(loaded.LastResult, "[redacted]") || strings.Contains(loaded.LastResult, "hunter2") {
		t.Errorf("failure not scrubbed: %+v", loaded)
	}
	if err := RecordAttempt(ctx, st, "o", "r", true, "", now); err != nil {
		t.Fatal(err)
	}
	loaded, _, _ = Load(ctx, st, "o", "r")
	if loaded.LastResult != "ok" || loaded.ConsecutiveFailures != 0 || loaded.LastSyncedAt == "" {
		t.Errorf("success outcome = %+v", loaded)
	}
	if err := Delete(ctx, st, "o", "r"); err != nil {
		t.Fatal(err)
	}
	if HasConfig(ctx, st, "o", "r") {
		t.Error("deleted HasConfig = true")
	}
	if err := RecordAttempt(ctx, st, "o", "r", true, "", now); err != nil {
		t.Errorf("RecordAttempt on deleted config = %v (must be nil)", err)
	}
}

func TestViewOf(t *testing.T) {
	now := time.Now().UTC()
	doc := &Doc{Version: 1, UpstreamURL: "file:///x", AuthKind: AuthToken, Username: "u", Schedule: ScheduleOff}
	v := ViewOf(doc, &Secret{AuthKind: AuthToken, Token: "tok123456"}, now)
	if v.UpstreamURL != "file:///x" || v.AuthKind != AuthToken || !v.HasSecret || v.SecretHint != "••••3456" {
		t.Errorf("view = %+v", v)
	}
	if v.NextSyncAt != "" || v.Due {
		t.Errorf("off view fires: %+v", v)
	}
	v = ViewOf(doc, nil, now)
	if v.HasSecret || v.SecretHint != "" {
		t.Errorf("nil-secret view leaks: %+v", v)
	}
	daily := &Doc{Version: 1, UpstreamURL: "file:///x", AuthKind: AuthNone, Schedule: PresetDaily}
	if v := ViewOf(daily, nil, now); !v.Due {
		t.Error("never-synced scheduled view not due")
	}
}
