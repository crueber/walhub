// mirror.go — the mirror sidecar: shape, presets, next-fire, and the
// Create-once-then-CAS'd bucket discipline (R1 (b)).
package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/bundle"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// KindMirrorSync is the Seam 5 task kind: one value, registered once
// from composition (the maintain.RegisterKind panic-on-duplicate
// contract — RegisterKind below, called once from cmd/walhub).
const KindMirrorSync = "mirror-sync"

// Schedule presets (R1 (b)/(d)): fixed names only — no freeform cron
// from the API (no cron-injection surface; unknown preset fails
// closed). DefaultPreset is the creation default.
const (
	PresetHourly  = "hourly"
	Preset8H      = "8h"
	PresetDaily   = "daily"
	PresetWeekly  = "weekly"
	PresetMonthly = "monthly"

	DefaultPreset = PresetDaily
)

// Presets lists every accepted schedule name in display order.
var Presets = []string{PresetHourly, Preset8H, PresetDaily, PresetWeekly, PresetMonthly}

// presetCrons maps each preset to the existing 6-field UTC cron
// (bundle.Cron reuse is ParseSchedule+Next only — the bundle Planner
// is NOT reused here). hourly=@hourly, 8h=0 0 */8 * * *,
// daily=@daily, weekly=@weekly, monthly=@monthly.
var presetCrons = map[string]string{
	PresetHourly:  "@hourly",
	Preset8H:      "0 0 */8 * * *",
	PresetDaily:   "@daily",
	PresetWeekly:  "@weekly",
	PresetMonthly: "@monthly",
}

// CronFor resolves a preset to its parsed cron; unknown presets fail
// closed (400 at the API, never a default substitution).
func CronFor(preset string) (bundle.Cron, error) {
	spec, ok := presetCrons[preset]
	if !ok {
		return bundle.Cron{}, fmt.Errorf("unknown schedule preset %q: want one of %s", preset, strings.Join(Presets, "|"))
	}
	return bundle.ParseSchedule(spec)
}

// ValidPreset reports whether preset is an accepted schedule name.
func ValidPreset(preset string) bool {
	_, ok := presetCrons[preset]
	return ok
}

// MirrorDoc is the meta/mirror.json body (R1 (b)): the preset name +
// last outcome. next_sync_at is DERIVED (NextFire), never stored —
// stored next-fire invites writer skew and clock bugs.
type MirrorDoc struct {
	Version             int    `json:"version"`
	UpstreamURL         string `json:"upstream_url"`
	Schedule            string `json:"schedule"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`  // RFC3339 of the last SUCCESS ("" = never)
	LastAttemptAt       string `json:"last_attempt_at,omitempty"` // RFC3339 of the last attempt (success or fail)
	LastResult          string `json:"last_result,omitempty"`     // "ok" | "failed: <scrubbed>" | "" (never attempted)
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
}

// NextFire derives the next scheduled fire strictly after now from the
// preset cron anchored at the last SUCCESS (bundle.Cron.Next). A repo
// that never synced successfully is due immediately (zero time, due).
// A failed sync never moves the anchor, so it never moves next fire.
func NextFire(doc *MirrorDoc, now time.Time) (fire time.Time, due bool, err error) {
	if doc == nil {
		return time.Time{}, false, fmt.Errorf("mirror: nil doc")
	}
	cron, err := CronFor(doc.Schedule)
	if err != nil {
		return time.Time{}, false, err
	}
	if doc.LastSyncedAt == "" {
		return time.Time{}, true, nil // never synced: due now
	}
	anchor, err := time.Parse(time.RFC3339, doc.LastSyncedAt)
	if err != nil {
		return time.Time{}, true, nil // unparseable anchor: fail open toward syncing, not toward silence
	}
	next, err := cron.Next(anchor)
	if err != nil {
		return time.Time{}, false, err
	}
	return next, !now.Before(next), nil
}

// Due reports whether a sync should fire now: never-synced → always;
// otherwise next-fire reached AND outside the failure backoff window.
func Due(doc *MirrorDoc, now time.Time) bool {
	if doc == nil {
		return false
	}
	_, due, err := NextFire(doc, now)
	if err != nil || !due {
		return false
	}
	return !BackedOff(doc, now)
}

// BackoffDelay is the failure backoff (R1 (f)): 15m × 2^(failures-1),
// capped at 24h. Zero failures → zero delay. The delay gates the fire;
// it never moves the computed next fire (NextFire ignores failures).
func BackoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	shift := failures - 1
	if shift > 7 {
		shift = 7 // cap the doubling before the multiply
	}
	d := 15 * time.Minute * time.Duration(1<<uint(shift))
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

// BackedOff reports whether now is still inside the post-failure
// backoff window (anchored at the last ATTEMPT, not the last success,
// so repeated immediate retries cannot hot-loop a dead upstream).
func BackedOff(doc *MirrorDoc, now time.Time) bool {
	if doc == nil || doc.ConsecutiveFailures <= 0 {
		return false
	}
	if doc.LastAttemptAt == "" {
		return false
	}
	attempt, err := time.Parse(time.RFC3339, doc.LastAttemptAt)
	if err != nil {
		return false
	}
	return now.Before(attempt.Add(BackoffDelay(doc.ConsecutiveFailures)))
}

// --- bucket discipline ------------------------------------------------------

// Load probes meta/mirror.json (probe, don't list — law 4). Absent →
// (nil, "", nil): the probe-absent skip is what stops the loop for a
// repo whose mirror.json was deleted — no extra machinery.
func Load(ctx context.Context, st store.ObjectStore, owner, name string) (*MirrorDoc, store.Version, error) {
	body, meta, err := store.GetBytes(ctx, st, store.MirrorKey(owner, name), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if body == nil {
		return nil, "", nil
	}
	doc := &MirrorDoc{}
	if err := json.Unmarshal(body, doc); err != nil {
		return nil, "", fmt.Errorf("mirror: corrupt %s: %w", store.MirrorKey(owner, name), err)
	}
	return doc, meta.Version, nil
}

// IsMirror is the push-refusal probe (server funnel + SSH gate): true
// iff a parseable mirror.json exists. Corrupt-but-present counts as a
// mirror (fail closed — a broken sidecar must not silently reopen a
// pull-only repo to pushes).
func IsMirror(ctx context.Context, st store.ObjectStore, owner, name string) bool {
	body, _, err := store.GetBytes(ctx, st, store.MirrorKey(owner, name), store.GetOptions{})
	if err != nil || body == nil {
		return false
	}
	doc := &MirrorDoc{}
	if err := json.Unmarshal(body, doc); err != nil {
		return true
	}
	return true
}

// Create writes the sidecar Create-once (412 = already a mirror — the
// caller maps it to 409). upstreamURL must already be canonical
// (repoimport.NormalizeSource); schedule must be a valid preset.
func Create(ctx context.Context, st store.ObjectStore, owner, name, upstreamURL, schedule string) (*MirrorDoc, error) {
	if !ValidPreset(schedule) {
		return nil, fmt.Errorf("mirror: unknown schedule preset %q", schedule)
	}
	doc := &MirrorDoc{Version: 1, UpstreamURL: upstreamURL, Schedule: schedule}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, err := store.PutBytes(ctx, st, store.MirrorKey(owner, name), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		return nil, err
	}
	return doc, nil
}

// UpdateCAS rewrites the sidecar under the CAS version Load returned
// (412 → the caller re-reads and retries, the §3.2 ladder). It never
// changes UpstreamURL or Schedule silently: callers pass the full
// intended doc.
func UpdateCAS(ctx context.Context, st store.ObjectStore, owner, name string, doc *MirrorDoc, ver store.Version) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = store.PutBytes(ctx, st, store.MirrorKey(owner, name), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
	return err
}

// Delete removes the sidecar (unconditional CAS delete of an absent
// key is Ok — deleting twice is not an error). Deleting stops the
// loop for the repo (Load probes absent → skip).
func Delete(ctx context.Context, st store.ObjectStore, owner, name string) error {
	return st.Delete(ctx, store.MirrorKey(owner, name), "")
}

// RecordAttempt CAS-updates the outcome fields after one sync fire:
// success clears the failure counter and stamps last_synced_at;
// failure bumps consecutive_failures and records the scrubbed reason.
// last_attempt_at stamps every attempt (the backoff anchor). A lost
// CAS race retries once on a fresh read (single retry — the writer is
// the only per-repo sync path plus rare admin edits).
func RecordAttempt(ctx context.Context, st store.ObjectStore, owner, name string, ok bool, reason string, now time.Time) error {
	for attempt := 0; attempt < 2; attempt++ {
		doc, ver, err := Load(ctx, st, owner, name)
		if err != nil {
			return err
		}
		if doc == nil {
			return nil // mirror deleted mid-sync: outcome has no home, not an error
		}
		stamp := now.UTC().Format(time.RFC3339)
		doc.LastAttemptAt = stamp
		if ok {
			doc.LastSyncedAt = stamp
			doc.LastResult = "ok"
			doc.ConsecutiveFailures = 0
		} else {
			doc.LastResult = "failed: " + reason
			doc.ConsecutiveFailures++
		}
		if uerr := UpdateCAS(ctx, st, owner, name, doc, ver); uerr != nil {
			if store.IsPreconditionFailed(uerr) {
				continue
			}
			return uerr
		}
		return nil
	}
	return fmt.Errorf("mirror: outcome CAS lost twice for %s/%s", owner, name)
}

// SetSchedule CAS-updates the preset (schedule changes take effect
// for the next fire — trivially, since next fire is computed at read
// from the stored preset; no migration, no stored field to update).
func SetSchedule(ctx context.Context, st store.ObjectStore, owner, name, preset string) (*MirrorDoc, error) {
	if !ValidPreset(preset) {
		return nil, fmt.Errorf("mirror: unknown schedule preset %q", preset)
	}
	doc, ver, err := Load(ctx, st, owner, name)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("mirror: %s/%s is not a mirror", owner, name)
	}
	doc.Schedule = preset
	if err := UpdateCAS(ctx, st, owner, name, doc, ver); err != nil {
		return nil, err
	}
	return doc, nil
}

// View is the computed read model for the summary API + UI: the stored
// doc plus the derived next_sync_at (RFC3339, "" when due-now or the
// schedule is broken) and the due flag.
type View struct {
	UpstreamURL         string `json:"upstream_url"`
	Schedule            string `json:"schedule"`
	NextSyncAt          string `json:"next_sync_at,omitempty"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`
	LastResult          string `json:"last_result,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	Due                 bool   `json:"due"`
	// DegradedReason carries the serve-health verdict (issue #320):
	// the last serve failure's reason while the repo's objects are
	// unservable, "" when servable. Filled by the summary hook (which
	// probes the sidecar), never stored — like NextSyncAt, derived at
	// read. Additive wire field (omitempty); old clients ignore it.
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// ViewOf builds the read model (pure: one function the summary
// handler already has — R1 (b)).
func ViewOf(doc *MirrorDoc, now time.Time) View {
	v := View{
		UpstreamURL:         doc.UpstreamURL,
		Schedule:            doc.Schedule,
		LastSyncedAt:        doc.LastSyncedAt,
		LastResult:          doc.LastResult,
		ConsecutiveFailures: doc.ConsecutiveFailures,
	}
	if fire, due, err := NextFire(doc, now); err == nil {
		v.Due = due && !BackedOff(doc, now)
		if !fire.IsZero() && !due {
			v.NextSyncAt = fire.UTC().Format(time.RFC3339)
		}
	}
	return v
}

// ServeDegraded reports the serve-health verdict for one repo (issue
// #320): the last serve failure's reason while its objects are
// unservable. False when no marker is present — the summary hook fills
// View.DegradedReason from this (one exact-key probe; 404s are free).
func ServeDegraded(ctx context.Context, st store.ObjectStore, owner, name string) (reason string, ok bool) {
	doc, found := wal.LoadServeHealth(ctx, st, owner, name)
	if !found || doc == nil {
		return "", false
	}
	return doc.Reason, true
}
