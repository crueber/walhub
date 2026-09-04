package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns raw bucket I/O: the P2 counter, the P3 two-step, the P4
// index, labels.json, and milestones. Policy (who may mutate, what events
// mean) lives in service.go; wire shapes live in http.go.

// --- P2 numbering ------------------------------------------------------------

// allocNum allocates one issue/PR number from meta/next_num via a
// PutUpdate CAS loop. Freshness ~1/creation; creation is human-rate so
// CAS contention is a non-issue (P2 reasoning). Bounded at 10 attempts,
// then ErrConflict.
func (s *Service) allocNum(ctx context.Context, owner, repo string) (int, error) {
	key := CounterKey(owner, repo)
	var num int
	_, err := s.casUpdate(ctx, key, 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		next := 1
		if cur != nil {
			var c Counter
			if jerr := json.Unmarshal(cur, &c); jerr != nil || c.Next < 1 {
				return nil, false, fmt.Errorf("%w: meta/next_num: corrupt", ErrCorrupt)
			}
			next = c.Next
		}
		num = next
		raw, _ := json.Marshal(Counter{Next: next + 1})
		return raw, true, nil
	})
	if err != nil {
		return 0, err
	}
	return num, nil
}

// allocMilestoneID allocates one milestone id from
// meta/milestones/index.json (the P2 CAS-counter pattern, §3.2).
func (s *Service) allocMilestoneID(ctx context.Context, owner, repo string) (string, error) {
	key := MilestoneCounterKey(owner, repo)
	var id string
	_, err := s.casUpdate(ctx, key, 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		next := 1
		if cur != nil {
			var c Counter
			if jerr := json.Unmarshal(cur, &c); jerr != nil || c.Next < 1 {
				return nil, false, fmt.Errorf("%w: milestones/index.json: corrupt", ErrCorrupt)
			}
			next = c.Next
		}
		id = fmt.Sprintf("%06x", next)
		raw, _ := json.Marshal(Counter{Next: next + 1})
		return raw, true, nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// --- P3 two-step -------------------------------------------------------------

// threadState is one header read: the parsed thread plus its CAS version
// ("" when absent).
type threadState struct {
	thread *Thread
	ver    store.Version
}

// loadThread reads a thread header; (nil, "", nil) when absent.
func (s *Service) loadThread(ctx context.Context, owner, repo string, num int) (*Thread, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, ThreadKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	t, perr := parseThread(raw)
	if perr != nil {
		return nil, "", perr
	}
	return t, ver, nil
}

// appendEvent runs the P3 two-step on one thread: CAS the header with
// mutate (which MUST increment NextEventSeq by exactly 1 and apply the
// mutation's header effects), then Create the event object the mutator
// built for the reserved seq.
//
// A 412 on the Create is a bug signal (seq is reserved — impossible
// unless a retry already wrote it): the loop retries from the fresh
// header, reserving a new seq; the skipped seq is a harmless gap (P3).
// Bounded at 5 attempts, then ErrConflict. Returns the committed thread,
// the written event, and its seq.
func (s *Service) appendEvent(ctx context.Context, owner, repo string, num int, mutate func(t *Thread, seq int) (*Event, error)) (*Thread, *Event, error) {
	key := ThreadKey(owner, repo, num)
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		if raw == nil {
			return nil, nil, fmt.Errorf("%w: unknown issue", ErrNotFound)
		}
		t, perr := parseThread(raw)
		if perr != nil {
			return nil, nil, perr
		}
		seq := t.NextEventSeq
		ev, merr := mutate(t, seq)
		if merr != nil {
			return nil, nil, merr
		}
		if ev == nil {
			// No-op mutation: nothing reserved, nothing written.
			return t, nil, nil
		}
		ev.Seq = seq
		m, cerr := store.PutBytes(ctx, s.Store, key, encodeThread(t),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
		if cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				continue
			}
			return nil, nil, cerr
		}
		_ = m
		if cerr := s.putCreate(ctx, EventKey(owner, repo, num, seq), encodeEvent(ev)); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				// Reserved seq already taken (a retried request won the
				// Create): retry the loop; the gap is harmless.
				continue
			}
			// The header CAS committed but the event write failed. The
			// reserved seq is skipped (P3: gaps allowed); the mutation is
			// NOT retried blindly — the caller sees the error and the
			// timeline stays the truth about what happened.
			return nil, nil, cerr
		}
		nt, _, _ := s.loadThread(ctx, owner, repo, num)
		if nt != nil {
			t = nt
		}
		return t, ev, nil
	}
	return nil, nil, fmt.Errorf("%w: issue %d changed concurrently; reload and retry", ErrConflict, num)
}

// loadEvent reads one immutable event; (nil, nil) when absent.
func (s *Service) loadEvent(ctx context.Context, owner, repo string, num, seq int) (*Event, error) {
	raw, _, err := s.getJSON(ctx, EventKey(owner, repo, num, seq))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return parseEvent(raw)
}

// scanEvents lists every event of one thread in seq order (P3: timeline
// reads by seq order, not density — gaps skipped). Threads are
// human-scale; one bounded prefix LIST per scan.
func (s *Service) scanEvents(ctx context.Context, owner, repo string, num int) ([]*Event, error) {
	prefix := EventsPrefix(owner, repo, num)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*Event, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		ev, perr := parseEvent(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, ev)
	}
	return out, nil
}

// --- P4 index -----------------------------------------------------------------

// loadIndex reads issues/index.json; (empty, "", nil) when absent (a repo
// with no issues yet, or a pre-issues repo — LIST fallback covers reads).
func (s *Service) loadIndex(ctx context.Context, owner, repo string) (*Index, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, IndexKey(owner, repo))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return &Index{Open: []Card{}, ClosedRecent: []Card{}}, "", nil
	}
	ix, perr := parseIndex(raw)
	if perr != nil {
		return nil, "", perr
	}
	return ix, ver, nil
}

// updateIndex upserts one card by its own CAS loop (P4). Bounded at 10
// attempts, then it PROCEEDS WITHOUT the index update — the repair path
// (next mutation re-reads, diffs, repairs; LIST fallback covers reads)
// makes staleness a performance gap, never a correctness gap. When the
// written bytes exceed IndexSizeLimit the compaction runs inline (§9
// opportunistic trigger: size-checked on every write, no sampling timer).
func (s *Service) updateIndex(ctx context.Context, owner, repo string, card Card) {
	key := IndexKey(owner, repo)
	var written []byte
	for attempt := 0; attempt < 10; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return
		}
		var ix *Index
		if raw == nil {
			ix = &Index{Open: []Card{}, ClosedRecent: []Card{}}
		} else if ix, err = parseIndex(raw); err != nil {
			return
		}
		upsertCard(ix, card)
		ix.Version++
		written, _ = json.Marshal(ix)
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, written, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return
		}
		break
	}
	if len(written) > IndexSizeLimit {
		_, _ = s.CompactIndex(ctx, owner, repo)
	}
}

// upsertCard inserts or replaces a card, keeping open newest-first by
// (updated_at desc, num desc) and closed_recent newest-first the same
// way. Cards of either kind are carried (one index for one numbering
// space; issue endpoints filter kind:"issue" at read).
func upsertCard(ix *Index, card Card) {
	removeNum := func(cards []Card, num int) []Card {
		out := cards[:0]
		for _, c := range cards {
			if c.Num != num {
				out = append(out, c)
			}
		}
		if out == nil {
			return []Card{}
		}
		return out
	}
	ix.Open = removeNum(ix.Open, card.Num)
	ix.ClosedRecent = removeNum(ix.ClosedRecent, card.Num)
	if card.State == StateOpen {
		ix.Open = append(ix.Open, card)
		sortCards(ix.Open)
	} else {
		ix.ClosedRecent = append(ix.ClosedRecent, card)
		sortCards(ix.ClosedRecent)
	}
}

// sortCards orders newest-activity-first (updated_at desc, num desc).
// This is the INDEX storage order (§2) — it is NOT the list render order
// (lists render number-desc via sortCardsByNum; see the §2 Decisions
// amendment in docs/features/02_issues.md).
func sortCards(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].UpdatedAt != cards[j].UpdatedAt {
			return cards[i].UpdatedAt > cards[j].UpdatedAt
		}
		return cards[i].Num > cards[j].Num
	})
}

// sortCardsByNum orders lists newest-first by issue number descending
// (num desc). List render paths (ListIssues) use this AFTER merging the
// open + closed_recent pages, so the combined list is always #N…#1
// regardless of the state filter — activity recency never reorders it.
func sortCardsByNum(cards []Card) {
	sort.Slice(cards, func(i, j int) bool { return cards[i].Num > cards[j].Num })
}

// CompactIndex evicts the oldest closed_recent entries while the object
// exceeds IndexSizeLimit and advances compacted_through monotonically in
// the same CAS (§2). Evicted threads are served by paginated LIST (P5).
// Returns true when it compacted.
func (s *Service) CompactIndex(ctx context.Context, owner, repo string) (bool, error) {
	key := IndexKey(owner, repo)
	compacted := false
	_, err := s.casUpdate(ctx, key, 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		ix, perr := parseIndex(cur)
		if perr != nil {
			return nil, false, perr
		}
		raw, _ := json.Marshal(ix)
		if len(raw) <= IndexSizeLimit {
			return nil, false, nil
		}
		// Oldest-first by (updated_at asc, num asc); evict until at most
		// half the byte budget, keeping at least the newest closed page.
		ordered := append([]Card(nil), ix.ClosedRecent...)
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].UpdatedAt != ordered[j].UpdatedAt {
				return ordered[i].UpdatedAt < ordered[j].UpdatedAt
			}
			return ordered[i].Num < ordered[j].Num
		})
		keep := map[int]bool{}
		for _, c := range ordered {
			keep[c.Num] = true
		}
		maxEvicted := ix.CompactedThrough
		for _, c := range ordered {
			trial := &Index{Version: ix.Version + 1, CompactedThrough: ix.CompactedThrough, Open: ix.Open}
			var kept []Card
			for _, k := range ix.ClosedRecent {
				if keep[k.Num] {
					kept = append(kept, k)
				}
			}
			if kept == nil {
				kept = []Card{}
			}
			trial.ClosedRecent = kept
			traw, _ := json.Marshal(trial)
			if len(traw) <= IndexSizeLimit/2 {
				break
			}
			delete(keep, c.Num)
			if k := fmt.Sprintf("%06x", c.Num); k > maxEvicted {
				maxEvicted = k
			}
			compacted = true
		}
		if !compacted {
			// Nothing evictable (open-only overflow): closed eviction is
			// all this task owns (§2); report no work.
			return nil, false, nil
		}
		var kept []Card
		for _, k := range ix.ClosedRecent {
			if keep[k.Num] {
				kept = append(kept, k)
			}
		}
		if kept == nil {
			kept = []Card{}
		}
		ix.ClosedRecent = kept
		if maxEvicted > ix.CompactedThrough {
			ix.CompactedThrough = maxEvicted
		}
		ix.Version++
		out, _ := json.Marshal(ix)
		return out, true, nil
	})
	if err != nil {
		return false, err
	}
	return compacted, nil
}

// --- labels ------------------------------------------------------------------

// loadLabels reads meta/labels.json; (empty set, "", nil) when absent.
func (s *Service) loadLabels(ctx context.Context, owner, repo string) (*LabelSet, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, LabelsKey(owner, repo))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return &LabelSet{Labels: []Label{}}, "", nil
	}
	var ls LabelSet
	if jerr := json.Unmarshal(raw, &ls); jerr != nil {
		return nil, "", fmt.Errorf("%w: labels.json: %v", ErrCorrupt, jerr)
	}
	if ls.Labels == nil {
		ls.Labels = []Label{}
	}
	return &ls, ver, nil
}

// findLabel locates a label case-insensitively.
func findLabel(ls *LabelSet, name string) *Label {
	for i := range ls.Labels {
		if strings.EqualFold(ls.Labels[i].Name, name) {
			return &ls.Labels[i]
		}
	}
	return nil
}

// --- milestones ---------------------------------------------------------------

// loadMilestone reads one milestone; (nil, "", nil) when absent.
func (s *Service) loadMilestone(ctx context.Context, owner, repo, id string) (*Milestone, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, MilestoneKey(owner, repo, id))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var m Milestone
	if jerr := json.Unmarshal(raw, &m); jerr != nil {
		return nil, "", fmt.Errorf("%w: milestone %s: %v", ErrCorrupt, id, jerr)
	}
	m.Percent = milestonePercent(m.OpenIssues, m.ClosedIssues)
	return &m, ver, nil
}

// milestonePercent derives progress (percent complete) on read (§3.2).
func milestonePercent(open, closed int) int {
	total := open + closed
	if total <= 0 {
		return 0
	}
	return (closed * 100) / total
}

// saveMilestone CAS-writes one milestone object (bounded 5, then conflict).
func (s *Service) saveMilestone(ctx context.Context, owner, repo string, m *Milestone, ver store.Version) error {
	m.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
	stored := *m
	stored.Percent = 0 // never stored (§3.2)
	raw, _ := json.Marshal(&stored)
	opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
	if ver == "" {
		opts.Mode = store.PutCreate
	}
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := store.PutBytes(ctx, s.Store, MilestoneKey(owner, repo, m.ID), raw, opts); err != nil {
			if store.IsPreconditionFailed(err) {
				cur, cver, gerr := s.loadMilestone(ctx, owner, repo, m.ID)
				if gerr != nil || cur == nil {
					return fmt.Errorf("%w: milestone %s changed concurrently", ErrConflict, m.ID)
				}
				cur.Title, cur.Description, cur.DueOn, cur.State = m.Title, m.Description, m.DueOn, m.State
				cur.OpenIssues, cur.ClosedIssues = m.OpenIssues, m.ClosedIssues
				cur.UpdatedAt = stored.UpdatedAt
				raw, _ = json.Marshal(cur)
				opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: cver, ContentType: "application/json"}
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: milestone %s changed concurrently", ErrConflict, m.ID)
}

// bumpMilestone adjusts a milestone's denormalized counters by delta
// (best-effort display state: thread headers are the truth; a lost update
// is repaired by the next issue event touching the milestone, §3).
func (s *Service) bumpMilestone(ctx context.Context, owner, repo, id string, dOpen, dClosed int) {
	if id == "" {
		return
	}
	_, _ = s.casUpdate(ctx, MilestoneKey(owner, repo, id), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var m Milestone
		if jerr := json.Unmarshal(cur, &m); jerr != nil {
			return nil, false, nil
		}
		m.OpenIssues += dOpen
		m.ClosedIssues += dClosed
		if m.OpenIssues < 0 {
			m.OpenIssues = 0
		}
		if m.ClosedIssues < 0 {
			m.ClosedIssues = 0
		}
		m.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		out, _ := json.Marshal(&m)
		return out, true, nil
	})
}

// listMilestones enumerates milestones via delimiter listing (P5:
// collaboration/admin path, paginated by the caller).
func (s *Service) listMilestones(ctx context.Context, owner, repo string) ([]*Milestone, error) {
	prefix := MilestonePrefix(owner, repo)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && !strings.HasSuffix(m.Key, "/index.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*Milestone, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil || raw == nil {
			if err != nil {
				return nil, err
			}
			continue
		}
		var m Milestone
		if jerr := json.Unmarshal(raw, &m); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, k, jerr)
		}
		m.Percent = milestonePercent(m.OpenIssues, m.ClosedIssues)
		out = append(out, &m)
	}
	return out, nil
}
