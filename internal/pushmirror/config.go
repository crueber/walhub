// config.go — the push-mirror sidecars: config shape, secret shape,
// presets, next-fire, and the Create-once-then-CAS'd bucket discipline.
package pushmirror

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/bundle"
	"git.packden.us/crueber/walhub/internal/store"
)

// KindPushMirrorSync is the Seam 5 task kind, distinct from pull
// mirror-sync: the (repo,kind) single-flight keeps the two directions
// from ever joining each other.
const KindPushMirrorSync = "mirror-push-sync"

// Auth kinds (per-mirror choice, stored on the config sidecar; the
// material lives in the secret sidecar).
const (
	AuthNone     = "none"
	AuthPassword = "password"
	AuthToken    = "token"
	AuthSSH      = "ssh"
)

// ValidAuthKind reports whether kind names a supported mechanism.
func ValidAuthKind(kind string) bool {
	switch kind {
	case AuthNone, AuthPassword, AuthToken, AuthSSH:
		return true
	}
	return false
}

// Schedule presets (the pull-mirror preset model, internal/bundle/cron.go
// reuse): fixed names only — no freeform cron from the API. "" (empty)
// means scheduled sync is OFF — on-push only (the default).
const (
	ScheduleOff   = ""
	PresetHourly  = "hourly"
	Preset8H      = "8h"
	PresetDaily   = "daily"
	PresetWeekly  = "weekly"
	PresetMonthly = "monthly"
)

// Presets lists every accepted non-empty schedule name in display order.
var Presets = []string{PresetHourly, Preset8H, PresetDaily, PresetWeekly, PresetMonthly}

// presetCrons maps each preset to the existing 6-field UTC cron
// (bundle.Cron reuse is ParseSchedule+Next only — independent of the
// pull-mirror map so either side can evolve alone).
var presetCrons = map[string]string{
	PresetHourly:  "@hourly",
	Preset8H:      "0 0 */8 * * *",
	PresetDaily:   "@daily",
	PresetWeekly:  "@weekly",
	PresetMonthly: "@monthly",
}

// ValidSchedule reports whether schedule is off or a known preset.
func ValidSchedule(schedule string) bool {
	if schedule == ScheduleOff {
		return true
	}
	_, ok := presetCrons[schedule]
	return ok
}

// CronFor resolves a non-empty preset to its parsed cron; unknown presets
// and "" fail closed ("" is not a cron — it is OFF).
func CronFor(preset string) (bundle.Cron, error) {
	spec, ok := presetCrons[preset]
	if !ok {
		return bundle.Cron{}, fmt.Errorf("unknown schedule preset %q: want one of \"\"|%s", preset, strings.Join(Presets, "|"))
	}
	return bundle.ParseSchedule(spec)
}

// Doc is the meta/pushmirror.json body: the upstream pointer, the
// auth-kind choice (material lives in the secret sidecar), the schedule
// ("" = on-push only), the generated public key when applicable
// (public — safe to echo), and the last outcome. next_sync_at is
// DERIVED (NextFire), never stored. Secrets NEVER appear here.
type Doc struct {
	Version             int    `json:"version"`
	UpstreamURL         string `json:"upstream_url"`
	AuthKind            string `json:"auth_kind"`
	Username            string `json:"username,omitempty"`
	Schedule            string `json:"schedule,omitempty"`
	PublicKey           string `json:"public_key,omitempty"`
	KeyFingerprint      string `json:"key_fingerprint,omitempty"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`  // RFC3339 of the last SUCCESS ("" = never)
	LastAttemptAt       string `json:"last_attempt_at,omitempty"` // RFC3339 of the last attempt (success or fail)
	LastResult          string `json:"last_result,omitempty"`     // "ok" | "failed: <scrubbed>" | "" (never attempted)
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
}

// Secret is the meta/pushmirror-secret.json body: the auth material the
// config sidecar only describes. NEVER echoed back in full — the API
// renders presence/last-4 only.
type Secret struct {
	Version       int    `json:"version"`
	AuthKind      string `json:"auth_kind"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`        // password kind: the HTTPS password
	Token         string `json:"token,omitempty"`           // token kind: the bearer-style token
	SSHPrivateKey string `json:"ssh_private_key,omitempty"` // ssh kind: OpenSSH private key PEM
	SSHKnownHosts string `json:"ssh_known_hosts,omitempty"` // ssh kind: known_hosts lines ("" = accept-new)
	// SSHKnownHostsAcceptedAt is the RFC3339 stamp of the first
	// accept-new learn (Forgejo #625, additive — older sidecars simply
	// lack it). Stamped once, preserved after; empty for
	// pinned-only trust (the stamp names learning, not explicit pins).
	SSHKnownHostsAcceptedAt string `json:"ssh_known_hosts_accepted_at,omitempty"`
	UpdatedAt               string `json:"updated_at,omitempty"` // RFC3339 of the last write
}

// HasMaterial reports whether the secret sidecar carries usable auth
// material for its kind (none never needs any).
func (s *Secret) HasMaterial() bool {
	if s == nil {
		return false
	}
	switch s.AuthKind {
	case AuthNone:
		return true
	case AuthPassword:
		return s.Password != ""
	case AuthToken:
		return s.Token != ""
	case AuthSSH:
		return s.SSHPrivateKey != ""
	}
	return false
}

// SecretHint renders the write-only confirmation (presence + last-4)
// for the stored material; "" when nothing is stored.
func (s *Secret) SecretHint() string {
	if s == nil {
		return ""
	}
	switch s.AuthKind {
	case AuthPassword:
		return last4(s.Password)
	case AuthToken:
		return last4(s.Token)
	case AuthSSH:
		return last4(s.SSHPrivateKey)
	}
	return ""
}

// sanitize returns a copy of doc with any accidentally-attached secret
// text scrubbed (defense in depth — callers never attach secrets, but
// last_result strings concatenate upstream-controlled text).
func sanitize(doc *Doc) *Doc {
	if doc == nil {
		return nil
	}
	cp := *doc
	cp.LastResult = scrubText(cp.LastResult)
	return &cp
}

// NextFire derives the next scheduled fire strictly after now from the
// preset cron anchored at the last SUCCESS. OFF ("") never fires (zero
// time, not due). A repo that never synced successfully is due
// immediately (zero time, due). A failed sync never moves the anchor.
func NextFire(doc *Doc, now time.Time) (fire time.Time, due bool, err error) {
	if doc == nil {
		return time.Time{}, false, fmt.Errorf("pushmirror: nil doc")
	}
	if doc.Schedule == ScheduleOff {
		return time.Time{}, false, nil
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
		return time.Time{}, true, nil // unparseable anchor: fail open toward syncing
	}
	next, err := cron.Next(anchor)
	if err != nil {
		return time.Time{}, false, err
	}
	return next, !now.Before(next), nil
}

// Due reports whether a scheduled sync should fire now: scheduled on +
// next-fire reached AND outside the failure backoff window.
func Due(doc *Doc, now time.Time) bool {
	if doc == nil || doc.Schedule == ScheduleOff {
		return false
	}
	_, due, err := NextFire(doc, now)
	if err != nil || !due {
		return false
	}
	return !BackedOff(doc, now)
}

// BackoffDelay is the failure backoff: 15m × 2^(failures-1), capped at
// 24h. Zero failures → zero delay. The delay gates the fire; it never
// moves the computed next fire.
func BackoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	shift := failures - 1
	if shift > 7 {
		shift = 7
	}
	d := 15 * time.Minute * time.Duration(1<<uint(shift))
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

// BackedOff reports whether now is still inside the post-failure
// backoff window (anchored at the last ATTEMPT).
func BackedOff(doc *Doc, now time.Time) bool {
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

// Load probes meta/pushmirror.json (probe, don't list — law 4). Absent →
// (nil, "", nil).
func Load(ctx context.Context, st store.ObjectStore, owner, name string) (*Doc, store.Version, error) {
	body, meta, err := store.GetBytes(ctx, st, store.PushMirrorKey(owner, name), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if body == nil {
		return nil, "", nil
	}
	doc := &Doc{}
	if err := json.Unmarshal(body, doc); err != nil {
		return nil, "", fmt.Errorf("pushmirror: corrupt %s: %w", store.PushMirrorKey(owner, name), err)
	}
	return sanitize(doc), meta.Version, nil
}

// HasConfig probes whether a parseable config sidecar exists (the
// on-push enqueue gate — one exact-key probe, 404s free).
func HasConfig(ctx context.Context, st store.ObjectStore, owner, name string) bool {
	body, _, err := store.GetBytes(ctx, st, store.PushMirrorKey(owner, name), store.GetOptions{})
	if err != nil || body == nil {
		return false
	}
	return true
}

// Create writes the config sidecar Create-once (412 = already configured
// — the caller maps it to 409). upstreamURL must already be canonical;
// authKind must be valid; schedule must be off or a valid preset.
func Create(ctx context.Context, st store.ObjectStore, owner, name, upstreamURL, authKind, username, schedule string) (*Doc, error) {
	if !ValidAuthKind(authKind) {
		return nil, fmt.Errorf("pushmirror: unknown auth kind %q", authKind)
	}
	if !ValidSchedule(schedule) {
		return nil, fmt.Errorf("pushmirror: unknown schedule preset %q", schedule)
	}
	doc := &Doc{Version: 1, UpstreamURL: upstreamURL, AuthKind: authKind, Username: username, Schedule: schedule}
	raw, err := json.Marshal(sanitize(doc))
	if err != nil {
		return nil, err
	}
	if _, err := store.PutBytes(ctx, st, store.PushMirrorKey(owner, name), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		return nil, err
	}
	return doc, nil
}

// UpdateCAS rewrites the config sidecar under the CAS version Load
// returned (412 → the caller re-reads and retries). It scrubs the
// outcome field before writing (defense in depth).
func UpdateCAS(ctx context.Context, st store.ObjectStore, owner, name string, doc *Doc, ver store.Version) error {
	raw, err := json.Marshal(sanitize(doc))
	if err != nil {
		return err
	}
	_, err = store.PutBytes(ctx, st, store.PushMirrorKey(owner, name), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
	return err
}

// Delete removes both sidecars (config + secret). Deleting twice is not
// an error; deleting stops the loop and the on-push fan-out for the
// repo (probe-absent → skip).
func Delete(ctx context.Context, st store.ObjectStore, owner, name string) error {
	if err := st.Delete(ctx, store.PushMirrorKey(owner, name), ""); err != nil {
		return err
	}
	return st.Delete(ctx, store.PushMirrorSecretKey(owner, name), "")
}

// RecordAttempt CAS-updates the outcome fields after one sync fire:
// success clears the failure counter and stamps last_synced_at;
// failure bumps consecutive_failures and records the scrubbed reason.
// last_attempt_at stamps every attempt (the backoff anchor). A lost CAS
// race retries once on a fresh read.
func RecordAttempt(ctx context.Context, st store.ObjectStore, owner, name string, ok bool, reason string, now time.Time) error {
	for attempt := 0; attempt < 2; attempt++ {
		doc, ver, err := Load(ctx, st, owner, name)
		if err != nil {
			return err
		}
		if doc == nil {
			return nil // config deleted mid-sync: outcome has no home, not an error
		}
		stamp := now.UTC().Format(time.RFC3339)
		doc.LastAttemptAt = stamp
		if ok {
			doc.LastSyncedAt = stamp
			doc.LastResult = "ok"
			doc.ConsecutiveFailures = 0
		} else {
			doc.LastResult = "failed: " + scrubText(reason)
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
	return fmt.Errorf("pushmirror: outcome CAS lost twice for %s/%s", owner, name)
}

// LoadSecret probes the secret sidecar. Absent → (nil, "", nil) (a
// none-kind config legitimately has no secret).
func LoadSecret(ctx context.Context, st store.ObjectStore, owner, name string) (*Secret, store.Version, error) {
	body, meta, err := store.GetBytes(ctx, st, store.PushMirrorSecretKey(owner, name), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if body == nil {
		return nil, "", nil
	}
	sec := &Secret{}
	if err := json.Unmarshal(body, sec); err != nil {
		return nil, "", fmt.Errorf("pushmirror: corrupt %s", store.PushMirrorSecretKey(owner, name))
	}
	return sec, meta.Version, nil
}

// SaveSecret CAS-writes the secret sidecar (create when absent, update
// under the loaded version otherwise — one read-modify-CAS loop, human
// rate). The secret body is NEVER scrubbed (it IS the secret) but it is
// never logged or echoed — the only reader is the push runner.
func SaveSecret(ctx context.Context, st store.ObjectStore, owner, name string, sec *Secret) error {
	sec.Version = 1
	sec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	_, _, err = store.GetBytes(ctx, st, store.PushMirrorSecretKey(owner, name), store.GetOptions{})
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	if store.IsNotFound(err) {
		_, err = store.PutBytes(ctx, st, store.PushMirrorSecretKey(owner, name), raw,
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
		if store.IsPreconditionFailed(err) {
			return SaveSecretCAS(ctx, st, owner, name, sec)
		}
		return err
	}
	return SaveSecretCAS(ctx, st, owner, name, sec)
}

// SaveSecretCAS rewrites the secret sidecar under a fresh read version
// (single retry on 412).
func SaveSecretCAS(ctx context.Context, st store.ObjectStore, owner, name string, sec *Secret) error {
	for attempt := 0; attempt < 2; attempt++ {
		_, ver, err := LoadSecret(ctx, st, owner, name)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(sec)
		if err != nil {
			return err
		}
		mode := store.PutUpdate
		if ver == "" {
			mode = store.PutCreate
		}
		_, err = store.PutBytes(ctx, st, store.PushMirrorSecretKey(owner, name), raw,
			store.PutOptions{Mode: mode, IfVersion: ver, ContentType: "application/json"})
		if err != nil {
			if store.IsPreconditionFailed(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("pushmirror: secret CAS lost twice for %s/%s", owner, name)
}

// View is the computed read model for the API + summary projection: the
// stored config plus the derived next_sync_at ("" when off, due-now, or
// broken-schedule) and the due flag, plus the write-only secret
// confirmation. Secrets NEVER appear here.
type View struct {
	UpstreamURL    string `json:"upstream_url"`
	AuthKind       string `json:"auth_kind"`
	Username       string `json:"username,omitempty"`
	HasSecret      bool   `json:"has_secret"`
	SecretHint     string `json:"secret_hint,omitempty"`
	Schedule       string `json:"schedule,omitempty"`
	PublicKey      string `json:"public_key,omitempty"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// HostKeyFingerprint is the presence-style SSH host-key status
	// (Forgejo #625): SHA256 fingerprint(s) of the trusted
	// known_hosts lines, comma-joined ("" = nothing trusted yet).
	// HostKeyAcceptedAt is the RFC3339 first-accepted-at stamp ("" for
	// pinned-only trust). Neither carries key material — safe for the
	// open-read view and the summary projection.
	HostKeyFingerprint  string `json:"host_key_fingerprint,omitempty"`
	HostKeyAcceptedAt   string `json:"host_key_accepted_at,omitempty"`
	NextSyncAt          string `json:"next_sync_at,omitempty"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`
	LastResult          string `json:"last_result,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	Due                 bool   `json:"due"`
}

// ViewOf builds the read model (pure: one function the handlers and the
// summary hook share). sec may be nil (no secret stored yet).
func ViewOf(doc *Doc, sec *Secret, now time.Time) View {
	v := View{
		UpstreamURL:         doc.UpstreamURL,
		AuthKind:            doc.AuthKind,
		Username:            doc.Username,
		Schedule:            doc.Schedule,
		PublicKey:           doc.PublicKey,
		KeyFingerprint:      doc.KeyFingerprint,
		LastSyncedAt:        doc.LastSyncedAt,
		LastResult:          doc.LastResult,
		ConsecutiveFailures: doc.ConsecutiveFailures,
	}
	if sec != nil {
		v.HasSecret = sec.HasMaterial()
		v.SecretHint = sec.SecretHint()
		// Derived at read from the merged trust (single source of
		// truth — no second copy to skew): pre-#625 pinned sidecars
		// surface their fingerprint with no accepted-at stamp.
		v.HostKeyFingerprint = KnownHostsFingerprints(sec.SSHKnownHosts)
		v.HostKeyAcceptedAt = sec.SSHKnownHostsAcceptedAt
	}
	if fire, due, err := NextFire(doc, now); err == nil {
		v.Due = due && !BackedOff(doc, now)
		if !fire.IsZero() && !due {
			v.NextSyncAt = fire.UTC().Format(time.RFC3339)
		}
	}
	return v
}
