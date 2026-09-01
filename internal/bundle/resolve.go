package bundle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Refs is the tip set of a point-in-time fold (§8.2): []proto.Ref sorted by
// name. Ref is (name, oid, peeled).
type Refs = []proto.Ref

// protoRef is an alias of the canonical ref message (§8.2: name, oid, peeled).
type protoRef = proto.Ref

// Verdict classifies a slot's content resolution (§8.4, §8.7).
type Verdict string

const (
	VerdictOK        Verdict = ""            // content resolvable
	VerdictNoState   Verdict = "no-state"    // the WAL cannot replay the cut
	VerdictUnavail   Verdict = "unavailable" // slot before first_state_at
	VerdictTooSmall  Verdict = "too-small"   // min_commits gate
	VerdictUnchanged Verdict = "unchanged"   // unchanged gate
)

// WalView is the as-of fold seam (§8.2; implemented by internal/wal).
type WalView interface {
	// RefsAsOf returns the tips + as_of_seq: the highest WAL seq whose
	// created_at ≤ at. A cut the WAL cannot replay (predates min_seq with no
	// usable checkpoint) is an error → no-state verdict.
	RefsAsOf(ctx context.Context, repo string, at time.Time) (Refs, uint64, error)
	// RefsAtSeq returns the tips at exactly a WAL seq (checkpoint written on
	// the spot when none exists and no ref moved since — §8.4).
	RefsAtSeq(ctx context.Context, repo string, seq uint64) (Refs, error)
	// FirstStateAt reports when repo state begins; earlier slots are
	// "unavailable" (never built, never recorded, never backfilled).
	FirstStateAt(repo string) (time.Time, bool)
}

// Content is a slot's resolved as-of content (§8.4).
type Content struct {
	Tips       Refs   // tip set (name+oid), sorted by name
	AsOfSeq    uint64 // 0 for no-state
	Verdict    Verdict
	NoStateErr error // the RefsAsOf error behind a no-state verdict
}

// ContentAt resolves a slot's content (§8.4). A slot earlier than
// first_state_at is unavailable; an unresolvable cut is no-state
// (as_of_seq = 0, reason "no state as of the slot").
func ContentAt(ctx context.Context, w WalView, repo string, slot time.Time) (Content, error) {
	if first, ok := w.FirstStateAt(repo); ok && slot.Before(first) {
		return Content{Verdict: VerdictUnavail}, nil
	}
	tips, seq, err := w.RefsAsOf(ctx, repo, slot)
	if err != nil {
		return Content{Verdict: VerdictNoState, NoStateErr: err}, nil
	}
	SortRefs(tips)
	return Content{Tips: tips, AsOfSeq: seq}, nil
}

// SortRefs sorts a tip set by name (§8.2 invariant).
func SortRefs(refs Refs) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
}

// TipOids extracts the oid of every tip, in order.
func TipOids(refs Refs) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Oid != "" {
			out = append(out, r.Oid)
		}
	}
	return out
}

// SameTips compares two tip sets as name+oid pairs (§8.7 unchanged gate):
// order-insensitive over names, exact on oids.
func SameTips(a, b Refs) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(r proto.Ref) string { return r.Name + "\x00" + r.Oid }
	ka := make([]string, len(a))
	for i, r := range a {
		ka[i] = key(r)
	}
	kb := make([]string, len(b))
	for i, r := range b {
		kb[i] = key(r)
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// derefRefs converts entry tip pointers to the value-slice Refs form.
func derefRefs(refs []*proto.Ref) Refs {
	out := make(Refs, 0, len(refs))
	for _, r := range refs {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// BaseFor is the single function that decides what an incremental slot is cut
// on (§8.6): the one place that knows both rules.
//
//	ownPrev := newest entry of s with entry.slot < slot    (skipped slots have no entry)
//	base    := newest entry of s.Base with entry.slot <= slot; when s.Base has none,
//	           repeat with s.Base's base (nearest ancestor that does)
//	if s.Chain && ownPrev != nil && (base == nil || slot == base.slot || ownPrev.slot > base.slot):
//	    return ownPrev
//	return base   (may be nil → slot is "blocked")
//
// The tie rule: at a shared fire instant (weekly/daily Sunday 00:00) the slot
// being cut has slot == base.slot, so the chain continues through its own link.
func BaseFor(s *Strategy, slot uint64, list *proto.BundleList, byName map[string]*Strategy) *proto.BundleEntry {
	ownPrev := NewestEntry(list, s.Name, slot, false)
	base := newestAncestorEntry(s, slot, list, byName)
	if s.Chain && ownPrev != nil &&
		(base == nil || slot == base.Slot || ownPrev.Slot > base.Slot) {
		return ownPrev
	}
	return base
}

// newestAncestorEntry walks up the base chain to the nearest ancestor strategy
// with an entry at or before the slot.
func newestAncestorEntry(s *Strategy, slot uint64, list *proto.BundleList, byName map[string]*Strategy) *proto.BundleEntry {
	name := s.Base
	for name != "" {
		b := byName[name]
		if b == nil {
			return nil
		}
		if e := NewestEntry(list, b.Name, slot, true); e != nil {
			return e
		}
		name = b.Base
	}
	return nil
}

// NewestEntry returns the newest entry of a strategy with slot < at (atMost
// false) or slot <= at (atMost true); nil when none. Skipped slots have no
// entry, so "newest" is over built bundles only.
func NewestEntry(list *proto.BundleList, strategy string, slot uint64, atMost bool) *proto.BundleEntry {
	var best *proto.BundleEntry
	if list == nil {
		return nil
	}
	for _, e := range list.Bundles {
		if e.Strategy != strategy {
			continue
		}
		if e.Slot > slot || (!atMost && e.Slot == slot) {
			continue
		}
		if best == nil || e.Slot > best.Slot ||
			(e.Slot == best.Slot && e.CreationToken > best.CreationToken) {
			best = e
		}
	}
	return best
}

// EntryByID finds an entry by its stable id (§8.11: "<strategy>/<slotRFC3339Z>").
func EntryByID(list *proto.BundleList, id string) *proto.BundleEntry {
	if list == nil {
		return nil
	}
	for _, e := range list.Bundles {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// ErrBlocked reports an incremental slot with no resolvable base bundle (§8.10
// plan state "blocked": waiting for the first base bundle).
var ErrBlocked = errors.New("bundle: no resolvable base bundle for the slot")

// BaseIDFor returns the base_id a verdict for (strategy, slot) must carry:
// the resolved base entry's id, or "" when the base is nil (fulls and no-state).
func BaseIDFor(s *Strategy, slot uint64, list *proto.BundleList, byName map[string]*Strategy) (string, error) {
	if s.Kind == KindFull {
		return "", nil
	}
	base := BaseFor(s, slot, list, byName)
	if base == nil {
		return "", fmt.Errorf("%w: strategy %q slot %d", ErrBlocked, s.Name, slot)
	}
	return base.ID, nil
}
