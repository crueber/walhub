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
	Detail     map[string]any      `json:"detail,omitempty"`
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
// overflow path) — success either way.
func (s *Service) appendActivity(ctx context.Context, owner, repo string, seq int, e Emission, action, title, actor, at string, targets []target) error {
	recips := make([]activityRecipient, 0, len(targets))
	for _, t := range targets {
		recips = append(recips, activityRecipient{Principal: t.principal, Reason: t.reason})
	}
	ev := ActivityEvent{
		Seq: seq, Repo: e.Repo, Action: action, Num: e.Num, Kind: e.Kind,
		Actor: actor, Title: title, At: at,
		Payload: encode(activityPayload{Class: e.Class, Recipients: recips, Detail: e.Detail}),
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
