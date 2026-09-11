// servehealth.go — the serve-health sidecar (issue #320, 05 §5.2.1): the
// bucket record of "this repo's objects are not servable".
//
// Writers: the serve path itself (RepoHandle.Sync marks its own pack-phase
// failures and clears on success) and the mirror sync/heal loop (probe and
// heal outcomes). Overwrite-always, last writer wins — contention is a
// repeated identical verdict, so no CAS ladder. Best-effort sideband: marker
// writes never fail a serve or a sync; they only narrate via logWarnf.
//
// Readers: the summary health fold and the mirror projection treat a
// present-and-parseable marker as degraded. The marker is sticky until
// re-proven — there is no TTL, so a quiet-but-broken repo cannot age back
// to healthy without a successful serve-level sync, probe, or heal
// deleting it (fail closed; demand traffic re-proves on first use).
//
// ### Concurrency
// Hazard: concurrent markers from N request goroutines + the heal loop
// interleave arbitrarily; a success-clear can race a failure-mark.
// Avoidance: the object is tiny JSON under PutOverwrite (every write is
// whole), so any interleave leaves a well-formed marker; the in-handle
// serveDegraded flag gates success-clears to instances that actually
// failed (no steady-state delete traffic), and the next serve re-marks
// when the failure is real — the signal converges, never wedges.
package wal

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// ServeHealthVersion is the sidecar schema version.
const ServeHealthVersion = 1

// ServeHealthDegraded is the only status value: presence of the sidecar
// (parseable) MEANS degraded.
const ServeHealthDegraded = "degraded"

// serveHealthWriteTimeout bounds best-effort marker writes/clears issued
// on detached contexts (they must never stall a serve or a sync task).
const serveHealthWriteTimeout = 10 * time.Second

// ServeHealth is the meta/serve-health.json body: the last serve failure
// (or heal attempt) for one repo.
type ServeHealth struct {
	Version int    `json:"version"`
	Status  string `json:"status"` // "degraded"
	Reason  string `json:"reason"` // scrubbed one-liner, safe for logs/UI
	At      string `json:"at"`     // RFC3339 of the last mark
	// Attempts/LastHealAt are the heal loop's backoff state (mirror
	// feature): incremented/stamped by heal attempts, preserved across
	// demand-side marks. Zero on a fresh demand mark.
	Attempts   int    `json:"attempts,omitempty"`
	LastHealAt string `json:"last_heal_at,omitempty"`
}

// LoadServeHealth probes the sidecar (probe, don't list — law 4). It
// returns (nil, false) when absent; a present-but-unreadable marker
// reads as degraded with a synthetic reason (fail closed — a broken
// sidecar must not report healthy).
func LoadServeHealth(ctx context.Context, st store.ObjectStore, owner, name string) (*ServeHealth, bool) {
	if st == nil {
		return nil, false
	}
	body, _, err := store.GetBytes(ctx, st, store.ServeHealthKey(owner, name), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, false
		}
		return nil, false // unreadable = never marked (fail open on transport; the next serve re-proves)
	}
	if body == nil {
		return nil, false
	}
	doc := &ServeHealth{}
	if err := json.Unmarshal(body, doc); err != nil {
		return &ServeHealth{Version: ServeHealthVersion, Status: ServeHealthDegraded,
			Reason: "unreadable serve-health marker", At: time.Now().UTC().Format(time.RFC3339)}, true
	}
	if doc.Status == "" {
		doc.Status = ServeHealthDegraded
	}
	return doc, true
}

// WriteServeHealth records a serve failure mark, preserving any heal
// backoff state already stored (a demand-side failure is not a heal
// attempt). failures parameter carries the heal state to preserve:
// callers pass the loaded doc (or nil). lastHeal advances only via
// WriteHealAttempt.
func WriteServeHealth(ctx context.Context, st store.ObjectStore, owner, name, reason string, prev *ServeHealth) error {
	doc := &ServeHealth{
		Version: ServeHealthVersion,
		Status:  ServeHealthDegraded,
		Reason:  reason,
		At:      time.Now().UTC().Format(time.RFC3339),
	}
	if prev != nil {
		doc.Attempts = prev.Attempts
		doc.LastHealAt = prev.LastHealAt
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = store.PutBytes(ctx, st, store.ServeHealthKey(owner, name), raw,
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"})
	return err
}

// WriteHealAttempt records a failed heal: attempts+1 stamped at now
// (the backoff anchor), preserving the last failure reason.
func WriteHealAttempt(ctx context.Context, st store.ObjectStore, owner, name, reason string, prev *ServeHealth) error {
	attempts := 1
	if prev != nil {
		attempts = prev.Attempts + 1
		if reason == "" {
			reason = prev.Reason
		}
	}
	if reason == "" {
		reason = "heal attempt failed"
	}
	doc := &ServeHealth{
		Version:    ServeHealthVersion,
		Status:     ServeHealthDegraded,
		Reason:     reason,
		At:         time.Now().UTC().Format(time.RFC3339),
		Attempts:   attempts,
		LastHealAt: time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = store.PutBytes(ctx, st, store.ServeHealthKey(owner, name), raw,
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"})
	return err
}

// ClearServeHealth deletes the sidecar (a successful serve, probe, or
// heal proving servability). Deleting an absent key is not an error;
// other errors return to the caller (best-effort sideband — callers
// log, never fail).
func ClearServeHealth(ctx context.Context, st store.ObjectStore, owner, name string) error {
	if st == nil {
		return nil
	}
	return st.Delete(ctx, store.ServeHealthKey(owner, name), "")
}

// markServeDegraded records this handle's serve failure: best-effort
// sidecar write on a detached bounded context (the caller's ctx may
// already be expired — that is precisely when marks happen) and sets
// the in-handle flag gating the success-clear.
func (h *RepoHandle) markServeDegraded(reason string) {
	h.serveDegraded.Store(true)
	wctx, cancel := context.WithTimeout(context.Background(), serveHealthWriteTimeout)
	defer cancel()
	prev, _ := LoadServeHealth(wctx, h.reg.st, h.owner(), h.name())
	if err := WriteServeHealth(wctx, h.reg.st, h.owner(), h.name(), reason, prev); err != nil {
		logWarnf("%s: serve-health mark lost: %v", h.ID, err)
	}
}

// clearServeDegraded deletes the sidecar after a successful serve —
// but ONLY when this handle previously failed (the flag): the common
// case (never failed) pays zero store round trips, and a foreign
// instance's mark is left for its owner's success or the heal loop.
func (h *RepoHandle) clearServeDegraded() {
	if !h.serveDegraded.Load() {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), serveHealthWriteTimeout)
	defer cancel()
	if err := ClearServeHealth(wctx, h.reg.st, h.owner(), h.name()); err != nil {
		logWarnf("%s: serve-health clear lost: %v", h.ID, err)
		return
	}
	h.serveDegraded.Store(false)
}

// owner/name split the "owner/name" handle ID for sidecar keys.
func (h *RepoHandle) owner() string {
	o, _, _ := cut2(h.ID, "/")
	return o
}

func (h *RepoHandle) name() string {
	_, n, _ := cut2(h.ID, "/")
	return n
}

// serveDegradedFlag exposes the in-handle mark for tests.
func (h *RepoHandle) serveDegradedFlag() *atomic.Bool { return &h.serveDegraded }
