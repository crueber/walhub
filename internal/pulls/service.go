package pulls

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the shared-family helpers (P2 numbering, P3 two-step, P4
// index over the SHARED issues keys) plus the read/write PR surface: open,
// get, list, update, comment.

// --- shared P2 numbering -----------------------------------------------------

// allocNum allocates one number from the shared meta/next_num via a
// PutUpdate CAS loop (P2; human-rate ⇒ CAS contention is a non-issue).
// Bounded at 10 attempts, then ErrConflict.
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

// --- shared P3 two-step ------------------------------------------------------

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

// appendEvent runs the P3 two-step on one thread: CAS the header with mutate
// (which MUST increment NextEventSeq by exactly 1), then Create the event.
// Bounded at 5 attempts, then ErrConflict. A nil event from mutate is a
// no-op (nothing reserved, nothing written).
func (s *Service) appendEvent(ctx context.Context, owner, repo string, num int, mutate func(t *Thread, seq int) (*Event, error)) (*Thread, *Event, error) {
	key := ThreadKey(owner, repo, num)
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		if raw == nil {
			return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
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
			return t, nil, nil
		}
		ev.Seq = seq
		if _, cerr := store.PutBytes(ctx, s.Store, key, encodeThread(t),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				continue
			}
			return nil, nil, cerr
		}
		if cerr := s.putCreate(ctx, EventKey(owner, repo, num, seq), encodeEvent(ev)); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				continue
			}
			return nil, nil, cerr
		}
		nt, _, _ := s.loadThread(ctx, owner, repo, num)
		if nt != nil {
			t = nt
		}
		return t, ev, nil
	}
	return nil, nil, fmt.Errorf("%w: pull request %d changed concurrently; reload and retry", ErrConflict, num)
}

// loadPR reads a pr.json sidecar; (nil, "", nil) when absent.
func (s *Service) loadPR(ctx context.Context, owner, repo string, num int) (*PRDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, PRKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	p, perr := parsePR(raw)
	if perr != nil {
		return nil, "", perr
	}
	return p, ver, nil
}

// savePR CAS-writes a pr.json sidecar (bounded 5, then conflict).
func (s *Service) savePR(ctx context.Context, owner, repo string, p *PRDoc, ver store.Version) error {
	p.Version++
	raw := encodePR(p)
	opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
	if ver == "" {
		opts.Mode = store.PutCreate
	}
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := store.PutBytes(ctx, s.Store, PRKey(owner, repo, p.Num), raw, opts); err != nil {
			if store.IsPreconditionFailed(err) {
				cur, cver, gerr := s.loadPR(ctx, owner, repo, p.Num)
				if gerr != nil || cur == nil {
					return fmt.Errorf("%w: pull request %d changed concurrently", ErrConflict, p.Num)
				}
				// Re-apply onto the fresh doc (sidecar fields are
				// last-writer-wins per field group; the merge outcome is
				// written once, so convergence is safe).
				freshVersion := cur.Version
				*cur = *p
				cur.Version = freshVersion
				p = cur
				p.Version++
				raw = encodePR(p)
				opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: cver, ContentType: "application/json"}
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: pull request %d changed concurrently", ErrConflict, p.Num)
}

// --- shared P4 index ---------------------------------------------------------

// loadIndex reads the shared issues/index.json; (empty, "", nil) when absent.
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

// prCardOf projects a PR thread header onto its shared-index card
// (kind:"pr"; the header alone — 02's list rendering reads cards alone).
func prCardOf(t *Thread) Card {
	return Card{
		Num: t.Num, Kind: "pr", Title: t.Title, State: t.State,
		Labels: nonNilStr(t.Labels), Assignees: nonNilStr(t.Assignees),
		Author: t.Author, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		CommentCount: t.CommentCount,
	}
}

// updateIndex upserts one card by its own CAS loop (P4). Bounded at 10
// attempts, then it PROCEEDS WITHOUT the index update — LIST fallback
// covers reads, so staleness is a performance gap, never correctness.
func (s *Service) updateIndex(ctx context.Context, owner, repo string, card Card) {
	key := IndexKey(owner, repo)
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
		written, _ := json.Marshal(ix)
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
}

// upsertCard inserts or replaces a card, newest-activity-first in both
// pages. Cards of either kind are carried (one index, one numbering space).
func upsertCard(ix *Index, card Card) {
	drop := func(cards []Card, num int) []Card {
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
	ix.Open = drop(ix.Open, card.Num)
	ix.ClosedRecent = drop(ix.ClosedRecent, card.Num)
	if card.State == StateOpen {
		ix.Open = append(ix.Open, card)
		sortCards(ix.Open)
	} else {
		ix.ClosedRecent = append(ix.ClosedRecent, card)
		sortCards(ix.ClosedRecent)
	}
}

// sortCards orders newest-activity-first (updated_at desc, num desc).
func sortCards(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].UpdatedAt != cards[j].UpdatedAt {
			return cards[i].UpdatedAt > cards[j].UpdatedAt
		}
		return cards[i].Num > cards[j].Num
	})
}

// --- open --------------------------------------------------------------------

// OpenInput shapes POST …/pulls (§8): {title, base_ref, head_ref, body?, fork?}.
type OpenInput struct {
	Title   string
	BaseRef string
	HeadRef string
	Body    string
	Fork    *ForkInfo // cross-fork head: fork repo holding head_ref
}

// OpenPR opens a PR (§3): resolve + verify, allocate the number (shared
// P2), create the thread (kind:"pr") + opened event (shared P3), Create
// pr.json, update the shared index (P4, card kind:"pr"), fan out (P8), then
// publish refs/pull/<num>/head server-side through the WAL publish path —
// ONLY when the head commit is already reachable (422 otherwise). Auth:
// write on the base repo (the server acts as the creator's principal for
// the ref create, so the creator needs only base-repo write).
func (s *Service) OpenPR(ctx context.Context, owner, repo string, actor auth.Principal, in OpenInput, correlationID string) (*Thread, *PRDoc, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "write"); err != nil {
		return nil, nil, err
	}
	title, err := validateTitle(in.Title)
	if err != nil {
		return nil, nil, err
	}
	if err := validateBody(in.Body); err != nil {
		return nil, nil, err
	}
	if err := validateRefName(in.BaseRef); err != nil {
		return nil, nil, err
	}
	if err := validateRefName(in.HeadRef); err != nil {
		return nil, nil, err
	}
	if s.Git == nil || s.Dirs == nil {
		return nil, nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	baseRepo := repoName(owner, repo)
	headOwner, headRepo := owner, repo
	if in.Fork != nil {
		parts := strings.SplitN(in.Fork.Repo, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, nil, fmt.Errorf("%w: fork.repo must be owner/name", ErrInvalid)
		}
		headOwner, headRepo = parts[0], parts[1]
	}
	baseDir, err := s.Dirs.Dir(ctx, baseRepo)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, repoName(headOwner, headRepo))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	baseSHA, err := s.Git.ResolveRef(ctx, baseDir, in.BaseRef)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, in.BaseRef)
	}
	headSHA, err := s.Git.ResolveRef(ctx, headDir, in.HeadRef)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unknown revision %q", ErrUnprocessable, in.HeadRef)
	}
	// Reachability in the BASE object set (§3: no PR ever publishes git
	// objects, only a ref to objects that already arrived). Cross-fork
	// heads not yet merged-reachable skip the base-side publish and record
	// the fork-local head instead (§7: the diff endpoint resolves through
	// the fork; the merge task fetches nothing — shared packs make it local).
	reachable, err := s.Git.Reachable(ctx, baseDir, headSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reachability check: %v", ErrUnavailable, err)
	}
	sameRepo := headOwner == owner && headRepo == repo
	if !reachable && sameRepo {
		return nil, nil, fmt.Errorf("%w: head commit not reachable — push first", ErrUnprocessable)
	}
	// 409 if an OPEN PR already pairs base+head (index hot window; open PRs
	// are human-scale — one bounded read set, no LIST).
	if dup, derr := s.findOpenPair(ctx, owner, repo, in.BaseRef, in.HeadRef, repoName(headOwner, headRepo)); derr != nil {
		return nil, nil, derr
	} else if dup > 0 {
		return nil, nil, fmt.Errorf("%w: open pull request #%d already pairs %s and %s", ErrConflict, dup, in.BaseRef, in.HeadRef)
	}
	num, err := s.allocNum(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	who := normPrincipal(actor.Name)
	th := &Thread{
		Num: num, Kind: "pr", Title: title, State: StateOpen,
		Author: who, CreatedAt: now, UpdatedAt: now,
		Labels: []string{}, Assignees: []string{}, Participants: []string{who},
		NextEventSeq: 1, CommentCount: 0, Version: 1,
	}
	ev := &Event{Seq: 0, Type: EventOpened, Actor: who, At: now, Body: strPtr(in.Body)}
	if _, err := store.PutBytes(ctx, s.Store, ThreadKey(owner, repo, num), encodeThread(th),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, nil, fmt.Errorf("%w: pull request %d already exists", ErrConflict, num)
		}
		return nil, nil, err
	}
	if err := s.putCreate(ctx, EventKey(owner, repo, num, 0), encodeEvent(ev)); err != nil {
		return nil, nil, err
	}
	pr := &PRDoc{
		Num: num, Kind: "pr",
		Base:  Endpoint{Repo: baseRepo, Ref: in.BaseRef, SHA: baseSHA},
		Head:  Endpoint{Repo: repoName(headOwner, headRepo), Ref: in.HeadRef, SHA: headSHA},
		Body:  in.Body,
		Draft: false, Version: 1,
	}
	if !sameRepo {
		pr.Fork = &ForkInfo{Repo: repoName(headOwner, headRepo)}
	}
	if err := s.putCreate(ctx, PRKey(owner, repo, num), encodePR(pr)); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, nil, fmt.Errorf("%w: pull request %d already exists", ErrConflict, num)
		}
		return nil, nil, err
	}
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	s.emit(ctx, NotifyEvent{Repo: baseRepo, Class: "opened", Actor: who, PullNum: num, Recipients: []string{}})
	s.emitMentioned(ctx, owner, repo, num, who, in.Body)
	s.stream(ctx, StreamEvent{Name: "pull", Repo: baseRepo, Action: "opened", Num: num, Title: title, State: StateOpen, Author: who, BaseRef: in.BaseRef, HeadRef: in.HeadRef, HeadSHA: headSHA})
	// Server-side refs/pull/<num>/head publish (WAL publish path, §3):
	// only for reachable heads; unreachable same-repo heads already 422'd
	// above, unreachable cross-fork heads record fork-local (no publish).
	headPublished := false
	if reachable && s.Refs != nil {
		meta := map[string]string{"principal": who, "agent": "pulls"}
		if correlationID != "" {
			meta["correlation_id"] = correlationID
		}
		if cerr := s.Refs.CreateRef(ctx, baseRepo, PullHeadRef(num), headSHA, meta); cerr != nil {
			// Recovery is a named repair (§3 Concurrency): the PR exists
			// with refs/pull/<num>/head absent; GET re-verifies reachability
			// and re-publishes idempotently. The open still 201s.
			s.stream(ctx, StreamEvent{Name: "pull", Repo: baseRepo, Action: "opened", Num: num, Title: title, State: StateOpen, Author: who, BaseRef: in.BaseRef, HeadRef: in.HeadRef, HeadSHA: headSHA})
		} else {
			headPublished = true
		}
	}
	pr.HeadPublished = headPublished
	nt, _, _ := s.loadThread(ctx, owner, repo, num)
	if nt != nil {
		th = nt
	}
	return th, pr, nil
}

// findOpenPair returns the num of an OPEN PR pairing (baseRef, headRef,
// headRepo), or 0. Index hot window only (open PRs, human-scale).
func (s *Service) findOpenPair(ctx context.Context, owner, repo, baseRef, headRef, headRepo string) (int, error) {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	for _, c := range ix.Open {
		if c.Kind != "pr" {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil {
			continue
		}
		if pr.Base.Ref == baseRef && pr.Head.Ref == headRef && pr.Head.Repo == headRepo && !pr.Merged {
			return pr.Num, nil
		}
	}
	return 0, nil
}

// --- get ---------------------------------------------------------------------

// PullView is GET …/pulls/{num}: header + pr.json + live mergeable (§4:
// stamped; mismatch ⇒ unknown + enqueued recompute) + the last-K timeline
// events (newest-first; older on demand via after_seq, like 02 §7).
type PullView struct {
	Thread     *Thread       `json:"thread"`
	PR         *PRDoc        `json:"pr"`
	Mergeable  *MergeableDoc `json:"mergeable"`
	HeadLive   string        `json:"head_live_sha"`
	BaseLive   string        `json:"base_live_sha"`
	HeadRefOk  bool          `json:"head_ref_ok"`
	Events     []*Event      `json:"events"`
	EventsMore bool          `json:"events_more"`
}

// GetPR reads header + pr.json + live mergeable. Auth: read. Recovery (§3):
// re-verifies reachability and re-publishes refs/pull/<num>/head
// idempotently when absent; HeadRefOk reports the outcome (the UI shows
// head_ref pending until then). afterSeq/n window the timeline older on
// demand (afterSeq ≤ 0 = newest; n ≤ 0 defaults 50, capped at 200).
func (s *Service) GetPR(ctx context.Context, owner, repo string, num int, p auth.Principal, afterSeq, n int) (*PullView, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	view := &PullView{Thread: th, PR: pr}
	events, more, eerr := s.eventWindow(ctx, owner, repo, num, afterSeq, n)
	if eerr != nil {
		return nil, eerr
	}
	view.Events, view.EventsMore = events, more
	if s.Git == nil || s.Dirs == nil {
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		return view, nil
	}
	// Live shas (2 git calls through the bounded pool — the §4
	// stamp comparison; neither blocks a read on git beyond the pool).
	baseDir, derr := s.Dirs.Dir(ctx, pr.Base.Repo)
	if derr != nil {
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		return view, nil
	}
	headDir, derr := s.Dirs.Dir(ctx, pr.Head.Repo)
	if derr != nil {
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		return view, nil
	}
	baseLive, berr := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	headLive, herr := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if berr != nil || herr != nil {
		// A deleted branch reports head.ref retained as snapshot (§3:
		// deleted branch ⇒ head sha retained; GET still serves).
		if berr == nil {
			view.BaseLive = baseLive
		} else {
			view.BaseLive = pr.Base.SHA
		}
		if herr == nil {
			view.HeadLive = headLive
		} else {
			view.HeadLive = pr.Head.SHA
		}
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		view.HeadRefOk = s.headRefMatches(ctx, pr, view.HeadLive)
		return view, nil
	}
	view.BaseLive = baseLive
	view.HeadLive = headLive
	// Head-sha drift (normal push or force-push, §6): refresh pr.json when
	// the live head moved, record head_force_pushed evidence, enqueue
	// mergeability. Best-effort on the read path (failures degrade to
	// unknown, never a 500 for the reader).
	if headLive != pr.Head.SHA {
		s.refreshHead(ctx, owner, repo, pr, th, headLive, p)
		if npr, _, nerr := s.loadPR(ctx, owner, repo, num); nerr == nil && npr != nil {
			pr = npr
			view.PR = pr
		}
		if nth, _, nerr := s.loadThread(ctx, owner, repo, num); nerr == nil && nth != nil {
			th = nth
			view.Thread = th
		}
	}
	// Recovery: re-publish refs/pull/<num>/head idempotently when absent.
	view.HeadRefOk = s.ensurePullHead(ctx, pr, headLive)
	// Stamped cache: match ⇒ serve; mismatch ⇒ unknown + enqueue.
	cached, _, _ := s.loadMergeable(ctx, owner, repo, num)
	if cached != nil && cached.BaseRef == pr.Base.Ref && cached.BaseSHA == baseLive && cached.HeadSHA == headLive {
		view.Mergeable = cached
		return view, nil
	}
	view.Mergeable = &MergeableDoc{
		BaseRef: pr.Base.Ref, BaseSHA: baseLive, HeadSHA: headLive,
		MergeBase: cachedMergeBase(cached), State: MergeableUnknown,
		Conflicts: []string{}, Rebaseable: true, ComputedAt: s.nowUTC().Format(dateTimeFmt),
	}
	s.enqueueMergeable(ctx, owner, repo, num)
	return view, nil
}

// eventWindow returns at most n events newest-first with seq < afterSeq
// (afterSeq ≤ 0 = newest), plus whether older remain. n ≤ 0 defaults 50,
// capped at 200 (P5 page sizes). One bounded prefix LIST per call —
// collaboration page, never the git hot path.
func (s *Service) eventWindow(ctx context.Context, owner, repo string, num, afterSeq, n int) ([]*Event, bool, error) {
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	events, err := s.scanEvents(ctx, owner, repo, num)
	if err != nil {
		return nil, false, err
	}
	var filtered []*Event
	for i := len(events) - 1; i >= 0; i-- {
		if afterSeq > 0 && events[i].Seq >= afterSeq {
			continue
		}
		filtered = append(filtered, events[i])
		if len(filtered) > n {
			break
		}
	}
	more := len(filtered) > n
	if more {
		filtered = filtered[:n]
	}
	if filtered == nil {
		filtered = []*Event{}
	}
	return filtered, more, nil
}

// scanEvents lists every event of one thread in seq order (P3: timeline
// reads by seq order, not density — gaps skipped).
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

// cachedMergeBase keeps the last merge base across unknown stamps (display
// continuity; the stamp triple is what invalidates).
func cachedMergeBase(cached *MergeableDoc) string {
	if cached == nil {
		return ""
	}
	return cached.MergeBase
}

// headRefMatches checks refs/pull/<num>/head == sha without publishing.
func (s *Service) headRefMatches(ctx context.Context, pr *PRDoc, sha string) bool {
	if s.Git == nil || s.Dirs == nil || s.Refs == nil {
		return false
	}
	dir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return false
	}
	got, err := s.Git.ResolveRef(ctx, dir, PullHeadRef(pr.Num))
	if err != nil {
		return false
	}
	return got == sha
}

// ensurePullHead publishes refs/pull/<num>/head idempotently (§3): a
// missing ref is created when the head is reachable; a DRIFTED server ref
// is CAS-forwarded to the live head (never forced) — the ref tracks the PR
// head for fetchers, like any topic ref. Unreachable cross-fork heads
// resolve through the fork (§7). Returns whether the ref matches now.
func (s *Service) ensurePullHead(ctx context.Context, pr *PRDoc, headLive string) bool {
	if s.Refs == nil || s.Git == nil || s.Dirs == nil {
		return pr.HeadPublished
	}
	dir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return false
	}
	meta := map[string]string{"principal": pr.Head.Repo, "agent": "pulls"}
	if got, rerr := s.Git.ResolveRef(ctx, dir, PullHeadRef(pr.Num)); rerr == nil {
		if got == headLive {
			return true
		}
		reachable, rerr := s.Git.Reachable(ctx, dir, headLive)
		if rerr != nil || !reachable {
			return false
		}
		if uerr := s.Refs.UpdateRef(ctx, pr.Base.Repo, PullHeadRef(pr.Num), got, headLive, meta); uerr != nil {
			return false
		}
		return true
	}
	reachable, rerr := s.Git.Reachable(ctx, dir, headLive)
	if rerr != nil || !reachable {
		return false
	}
	if cerr := s.Refs.CreateRef(ctx, pr.Base.Repo, PullHeadRef(pr.Num), headLive, meta); cerr != nil {
		return false
	}
	return true
}

// --- list --------------------------------------------------------------------

// PROut is one row of GET …/pulls (§8): the index card enriched from pr.json.
type PROut struct {
	Num       int    `json:"num"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Author    string `json:"author"`
	BaseRef   string `json:"base_ref"`
	HeadRef   string `json:"head_ref"`
	HeadSHA   string `json:"head_sha"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updated_at"`
}

// ListFilter shapes GET …/pulls (§8): state/base/head/sort/n/after.
type ListFilter struct {
	State string
	Base  string
	Head  string
	Sort  string // updated (default) | created — display order only
	After int
	N     int
}

// ListResult is the paged PR list answer.
type ListResult struct {
	Pulls []PROut `json:"pulls"`
	More  bool    `json:"more"`
}

// ListPRs serves the PR list index-first (P4): the shared index supplies
// nums in newest-activity order; each row is enriched from its pr.json
// sidecar (one GET per listed row — page-bounded, never a LIST). Base/head
// filters apply post-enrichment. Auth: read.
func (s *Service) ListPRs(ctx context.Context, owner, repo string, p auth.Principal, f ListFilter) (*ListResult, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	n := f.N
	if n <= 0 {
		n = 50
	}
	if n > 100 {
		n = 100
	}
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	pool := append(append([]Card{}, ix.Open...), ix.ClosedRecent...)
	sortCards(pool)
	var rows []PROut
	for _, c := range pool {
		if c.Kind != "pr" {
			continue
		}
		if f.State != "" && c.State != f.State {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil {
			continue
		}
		if f.Base != "" && pr.Base.Ref != f.Base {
			continue
		}
		if f.Head != "" && pr.Head.Ref != f.Head {
			continue
		}
		rows = append(rows, PROut{
			Num: pr.Num, Title: c.Title, State: c.State, Author: c.Author,
			BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA,
			Draft: pr.Draft, UpdatedAt: c.UpdatedAt,
		})
	}
	if rows == nil {
		rows = []PROut{}
	}
	page, more := windowRows(rows, f.After, n)
	return &ListResult{Pulls: page, More: more}, nil
}

// windowRows slices the newest-first rows after the after-num cursor
// (unknown cursor ⇒ from the top). Callers always pass a non-nil pool
// (ListPRs normalizes), so the page is non-nil by construction.
func windowRows(pool []PROut, after, n int) ([]PROut, bool) {
	start := 0
	if after > 0 {
		start = len(pool)
		for i, r := range pool {
			if r.Num == after {
				start = i + 1
				break
			}
		}
	}
	end := start + n
	more := false
	if end < len(pool) {
		more = true
	} else {
		end = len(pool)
	}
	return pool[start:end], more
}

// --- update + comment ---------------------------------------------------------

// PRPatch carries the PUT fields (§8): title/body/state. Unknown keys are
// rejected at the HTTP layer before this is built.
type PRPatch struct {
	Title *string
	Body  *string
	State *string
}

// UpdatePR applies title/body/state edits (§8 PUT). Title/state append
// events; body updates the pr.json description (additive optional field —
// P3 events are never rewritten). Close/reopen append state events. Auth:
// title/state/body — author or triage (triage may close others').
func (s *Service) UpdatePR(ctx context.Context, owner, repo string, num int, actor auth.Principal, p PRPatch) (*Thread, *PRDoc, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, prVer, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if pr == nil {
		return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	who := normPrincipal(actor.Name)
	isAuthor := normPrincipal(th.Author) == who
	triage := s.requireRole(ctx, owner, repo, actor, "triage") == nil
	canMod := isAuthor || triage

	var title *string
	if p.Title != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: title changes need author or triage", ErrForbidden)
		}
		t, verr := validateTitle(*p.Title)
		if verr != nil {
			return nil, nil, verr
		}
		if t != th.Title {
			title = &t
		}
	}
	var state *string
	if p.State != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: state changes need author or triage", ErrForbidden)
		}
		want := strings.ToLower(strings.TrimSpace(*p.State))
		if want != StateOpen && want != StateClosed {
			return nil, nil, fmt.Errorf("%w: state must be open|closed, got %q", ErrInvalid, *p.State)
		}
		if want != th.State {
			if want == StateClosed && pr.Merged {
				return nil, nil, fmt.Errorf("%w: pull request #%d is merged", ErrConflict, num)
			}
			state = &want
		}
	}
	if p.Body != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: body changes need author or triage", ErrForbidden)
		}
		if err := validateBody(*p.Body); err != nil {
			return nil, nil, err
		}
	}
	now := s.nowUTC().Format(dateTimeFmt)
	if title != nil {
		from := th.Title
		to := *title
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Title = to
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventTitleChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to)}, nil
		})
		if aerr != nil {
			return nil, nil, aerr
		}
		th = nt
	}
	if state != nil {
		from := th.State
		to := *state
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.State = to
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventStateChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to)}, nil
		})
		if aerr != nil {
			return nil, nil, aerr
		}
		th = nt
		action := "reopened"
		class := "reopened"
		if to == StateClosed {
			action, class = "closed", "closed"
		}
		s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: class, Actor: who, PullNum: num, Recipients: prParticipants(th, who)})
		s.stream(ctx, StreamEvent{Name: "pull", Repo: repoName(owner, repo), Action: action, Num: num, Title: th.Title, State: th.State, Author: th.Author, BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA})
	}
	if p.Body != nil && *p.Body != pr.Body {
		pr.Body = *p.Body
		if serr := s.savePR(ctx, owner, repo, pr, prVer); serr != nil {
			return nil, nil, serr
		}
		if npr, _, nerr := s.loadPR(ctx, owner, repo, num); nerr == nil && npr != nil {
			pr = npr
		}
	}
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	return th, pr, nil
}

// prParticipants fans subscribed notifications to participants minus actor.
func prParticipants(th *Thread, actor string) []string {
	var out []string
	for _, p := range th.Participants {
		if p != "" && p != actor {
			out = append(out, p)
		}
	}
	return out
}

// AddComment appends a commented event to a PR thread (the conversation
// box; same P3 two-step as issues). Auth: read (authenticated).
func (s *Service) AddComment(ctx context.Context, owner, repo string, num int, actor auth.Principal, body string) (*Event, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: body must not be empty", ErrInvalid)
	}
	if err := validateBody(body); err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	th, ev, err := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
		if t.Kind != "pr" {
			return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		t.NextEventSeq = seq + 1
		t.UpdatedAt = now
		t.CommentCount++
		t.Participants = uniqSorted(append(t.Participants, who))
		t.Version++
		return &Event{Seq: seq, Type: EventCommented, Actor: who, At: now, Body: strPtr(body)}, nil
	})
	if err != nil {
		return nil, err
	}
	pr, _, _ := s.loadPR(ctx, owner, repo, num)
	_ = pr
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "subscribed", Actor: who, PullNum: num, Recipients: prParticipants(th, who)})
	s.emitMentioned(ctx, owner, repo, num, who, body)
	return ev, nil
}

// emitMentioned fans "mentioned" for @-parsed principals and @org/team
// spellings in a PR opened/commented body (06 §3; the consumer validates
// and expands). Bodies without tokens emit nothing.
func (s *Service) emitMentioned(ctx context.Context, owner, repo string, num int, actor, body string) {
	if body == "" {
		return
	}
	users, teams := identity.ParseMentions(body)
	var recips []string
	for _, m := range users {
		if m != actor && identity.ValidPrincipal(m) {
			recips = append(recips, m)
		}
	}
	for _, t := range teams {
		if t != actor {
			recips = append(recips, t)
		}
	}
	if len(recips) == 0 {
		return
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "mentioned", Actor: actor, PullNum: num, Recipients: recips})
}
