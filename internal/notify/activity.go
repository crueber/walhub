// activity.go — the §5.3 collaboration activity log: the webhook
// delivery unit and the notification backfill source.
//
// Every fanned-out mutation appends one immutable event at
// repos/<o>/<r>/collab-events/<seq:012x>.json (Create; seq reserved by
// CAS on meta/collab_state.json — the P3 two-step). The payload carries
// the resolved (principal, reason) set so the notify-fanout task can
// complete overflow/shortfall emissions without re-resolving.
package notify

import (
	"context"
	"encoding/json"
	"fmt"

	"git.packden.us/crueber/walhub/internal/store"
)

// activityRecipient is one resolved delivery in the activity payload.
type activityRecipient struct {
	Principal string `json:"principal"`
	Reason    string `json:"reason"`
}

// activityPayload is the ActivityEvent payload for fan-out events.
type activityPayload struct {
	Class      string              `json:"class"`
	Recipients []activityRecipient `json:"recipients"`
	// FanoutPending marks overflow/shortfall emissions whose recipient
	// set still needs the notify-fanout task (issue #77): the sweep
	// re-drains exactly these events after a restart. Sync-complete
	// emissions omit it (omitempty keeps their bytes unchanged).
	FanoutPending bool           `json:"fanout_pending,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
}

// reserveSeq CAS-allocates the next activity seq (P3 two-step, step 1:
// reserve). Bounded retries; exhaustion returns ErrConflict. A crash
// between reservation and the event Create leaves a gap — allowed and
// counted by readers as an honest gap (§5.3).
func (s *Service) reserveSeq(ctx context.Context, owner, repo string) (int, error) {
	var seq int
	_, err := s.casUpdate(ctx, CollabStateKey(owner, repo), 8, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var st CollabState
		if cur != nil {
			if err := json.Unmarshal(cur, &st); err != nil {
				return nil, false, fmt.Errorf("%w: collab_state: %v", ErrInvalid, err)
			}
		}
		seq = st.NextSeq + 1
		if seq < 1 {
			seq = 1
		}
		st.NextSeq = seq
		return encode(st), true, nil
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// appendActivity Creates the immutable activity event for seq (P3
// two-step, step 2). A 412 means the seq was already appended (retried
// overflow path) — success either way. pending marks overflow/shortfall
// emissions for the restart redrain sweep (issue #77); sync-complete
// emissions pass false.
func (s *Service) appendActivity(ctx context.Context, owner, repo string, seq int, e Emission, action, title, actor, at string, targets []target, pending bool) error {
	recips := make([]activityRecipient, 0, len(targets))
	for _, t := range targets {
		recips = append(recips, activityRecipient{Principal: t.principal, Reason: t.reason})
	}
	ev := ActivityEvent{
		Seq: seq, Repo: e.Repo, Action: action, Num: e.Num, Kind: e.Kind,
		Actor: actor, Title: title, At: at,
		Payload: encode(activityPayload{Class: e.Class, Recipients: recips, FanoutPending: pending, Detail: e.Detail}),
	}
	if err := s.putCreate(ctx, ActivityKey(owner, repo, seq), encode(ev)); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil
		}
		return err
	}
	return nil
}

// readActivity loads one activity event; nil when absent.
func (s *Service) readActivity(ctx context.Context, owner, repo string, seq int) *ActivityEvent {
	raw, _, err := s.getJSON(ctx, ActivityKey(owner, repo, seq))
	if err != nil || raw == nil {
		return nil
	}
	var ev ActivityEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}
	return &ev
}

// FanoutDoneDoc is one repos/<o>/<r>/collab-fanout/<seq:012x>.json: the
// per-seq completion record of the notify-fanout task (issue #77). The
// drain writes it after fanoutOne completes a seq; the restart redrain
// sweep skips seqs that carry it. Create-only and tiny; retention deletes
// it alongside its activity event (§9).
type FanoutDoneDoc struct {
	Seq int    `json:"seq"`
	At  string `json:"at"`
}

// FanoutDoneKey returns repos/<o>/<r>/collab-fanout/<seq:012x>.json.
func FanoutDoneKey(owner, repo string, seq int) string {
	return fmt.Sprintf("repos/%s/%s/collab-fanout/%012x.json", owner, repo, seq)
}

// markFanoutDone records a completed drain (412 = already drained —
// success). Best-effort: a lost write re-drains idempotently on the next
// sweep (deterministic ids + Create-412), never skipping.
func (s *Service) markFanoutDone(ctx context.Context, owner, repo string, seq int) {
	_ = s.putCreate(ctx, FanoutDoneKey(owner, repo, seq),
		encode(FanoutDoneDoc{Seq: seq, At: s.nowUTC().Format(dateTimeFmt)}))
}

// fanoutDone reports whether seq already drained (absent/unreadable →
// false — the sweep re-enqueues and the drain proves idempotent).
func (s *Service) fanoutDone(ctx context.Context, owner, repo string, seq int) bool {
	raw, _, err := s.getJSON(ctx, FanoutDoneKey(owner, repo, seq))
	return err == nil && raw != nil
}
