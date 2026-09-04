package review

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the line-anchored threads (§4: open/list/get/comment/
// resolve), the review-requests index (§5), and review-suggest.

// --- threads ---------------------------------------------------------------

// reserveTID allocates one thread id from next_thread_num on the PR header
// CAS (one arbitration point with review submits, §4 Concurrency (b)).
// Bounded at 10 attempts, then ErrConflict.
func (s *Service) reserveTID(ctx context.Context, owner, repo string, num int, who string) (string, error) {
	key := ThreadKey(owner, repo, num)
	for attempt := 0; attempt < 10; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return "", err
		}
		if raw == nil {
			return "", fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		cur, perr := parsePRHeader(raw)
		if perr != nil {
			return "", perr
		}
		// No kind re-check here: prHeadOf verified kind=="pr" and kind is
		// immutable (P3) — no writer ever changes it, so the CAS loop
		// cannot observe a different kind.
		next := cur.NextThreadNum
		if next < 1 {
			next = 1
		}
		tid := fmt.Sprintf("%08x", next)
		cur.NextThreadNum = next + 1
		cur.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		cur.Participants = uniqSorted(append(cur.Participants, who))
		cur.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodePRHeader(cur),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return "", perr
		}
		return tid, nil
	}
	return "", fmt.Errorf("%w: pull request %d changed concurrently; reload and retry", ErrConflict, num)
}

// OpenThread opens one line-anchored thread with its first comment (§4):
// reserve tid from the PR header CAS, Create the header, Create comment
// seq 1. Auth: read (authenticated). Returns the header.
func (s *Service) OpenThread(ctx context.Context, owner, repo string, num int, actor auth.Principal, anchor Anchor, body string) (*ThreadHeader, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := validateAnchor(anchor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: body must not be empty", ErrInvalid)
	}
	if err := validateBody(body); err != nil {
		return nil, err
	}
	h, _, err := s.prHeadOf(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	tid, err := s.reserveTID(ctx, owner, repo, num, who)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	th := &ThreadHeader{
		TID: tid, Num: num, Kind: "review_thread", Anchor: anchor,
		CommentCount: 1, NextEventSeq: 2,
		CreatedAt: now, CreatedBy: who, UpdatedAt: now, Version: 1,
	}
	if cerr := s.putCreate(ctx, ReviewThreadKey(owner, repo, num, tid), encodeThreadHeader(th)); cerr != nil {
		if store.IsPreconditionFailed(cerr) {
			return nil, fmt.Errorf("%w: review thread %s already exists", ErrConflict, tid)
		}
		return nil, cerr
	}
	c := &ThreadComment{Kind: KindReviewThreadComment, Seq: 1, At: now, By: who, Body: body}
	if cerr := s.putCreate(ctx, ReviewThreadEventKey(owner, repo, num, tid, 1), encodeThreadComment(c)); cerr != nil {
		if store.IsPreconditionFailed(cerr) {
			return nil, fmt.Errorf("%w: review thread %s already exists", ErrConflict, tid)
		}
		return nil, cerr
	}
	if _, serr := s.refreshSummary(ctx, owner, repo, num); serr != nil {
		return nil, serr
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "thread_opened", Actor: who, PullNum: num, Recipients: prParticipants(h, who)})
	s.stream(ctx, StreamEvent{Name: "thread", Repo: repoName(owner, repo), Action: "opened", Num: num, TID: tid})
	return th, nil
}

// ThreadListResult is the paged threads answer.
type ThreadListResult struct {
	Threads []*ThreadHeader `json:"threads"`
	More    bool            `json:"more"`
}

// ListThreads serves GET …/threads (§7): read, tid order, after = exclusive
// lower bound (tids strictly greater), n default 50 capped 100.
// resolved nil = all; non-nil filters.
func (s *Service) ListThreads(ctx context.Context, owner, repo string, num int, p auth.Principal, resolved *bool, after string, n int) (*ThreadListResult, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 50
	}
	if n > 100 {
		n = 100
	}
	headers, err := s.scanThreadHeaders(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	var filtered []*ThreadHeader
	for _, th := range headers {
		if after != "" && th.TID <= after {
			continue
		}
		if resolved != nil && th.Resolved != *resolved {
			continue
		}
		filtered = append(filtered, th)
		if len(filtered) > n {
			break
		}
	}
	more := len(filtered) > n
	if more {
		filtered = filtered[:n]
	}
	if filtered == nil {
		filtered = []*ThreadHeader{}
	}
	return &ThreadListResult{Threads: filtered, More: more}, nil
}

// ThreadView is GET …/threads/{tid}: header + comment window.
type ThreadView struct {
	Thread   *ThreadHeader    `json:"thread"`
	Comments []*ThreadComment `json:"comments"`
	More     bool             `json:"more"`
}

// scanThreadComments lists every comment of one thread in seq order.
func (s *Service) scanThreadComments(ctx context.Context, owner, repo string, num int, tid string) ([]*ThreadComment, error) {
	prefix := "repos/" + owner + "/" + repo + fmt.Sprintf("/pulls/%06x/threads/%s/events/", num, tid)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*ThreadComment, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		c, perr := parseThreadComment(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, c)
	}
	return out, nil
}

// GetThread serves GET …/threads/{tid} (§7): read, 404 unknown; comments
// newest-first, after = exclusive upper bound, n default 50 capped 200.
func (s *Service) GetThread(ctx context.Context, owner, repo string, num int, tid string, p auth.Principal, after, n int) (*ThreadView, error) {
	if err := validateTID(tid); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	raw, _, err := s.getJSON(ctx, ReviewThreadKey(owner, repo, num, tid))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: review thread %s", ErrNotFound, tid)
	}
	th, perr := parseThreadHeader(raw)
	if perr != nil {
		return nil, perr
	}
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	comments, err := s.scanThreadComments(ctx, owner, repo, num, tid)
	if err != nil {
		return nil, err
	}
	var filtered []*ThreadComment
	for i := len(comments) - 1; i >= 0; i-- {
		if after > 0 && comments[i].Seq >= after {
			continue
		}
		filtered = append(filtered, comments[i])
		if len(filtered) > n {
			break
		}
	}
	more := len(filtered) > n
	if more {
		filtered = filtered[:n]
	}
	if filtered == nil {
		filtered = []*ThreadComment{}
	}
	return &ThreadView{Thread: th, Comments: filtered, More: more}, nil
}

// AddThreadComment appends a comment via the P3 two-step on the thread
// header (reserve seq → Create). Concurrent comments race the header CAS —
// the loser re-reads and retries; the reserved-seq discipline makes Create
// unambiguous. Auth: read (authenticated).
func (s *Service) AddThreadComment(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal, body string) (*ThreadComment, error) {
	if err := validateTID(tid); err != nil {
		return nil, err
	}
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
	h, _, err := s.prHeadOf(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	key := ReviewThreadKey(owner, repo, num, tid)
	var out *ThreadComment
	for attempt := 0; attempt < 10; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, fmt.Errorf("%w: review thread %s", ErrNotFound, tid)
		}
		th, perr := parseThreadHeader(raw)
		if perr != nil {
			return nil, perr
		}
		seq := th.NextEventSeq
		if seq < 1 {
			seq = 1
		}
		th.NextEventSeq = seq + 1
		th.CommentCount++
		th.UpdatedAt = now
		th.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodeThreadHeader(th),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, perr
		}
		c := &ThreadComment{Kind: KindReviewThreadComment, Seq: seq, At: now, By: who, Body: body}
		if cerr := s.putCreate(ctx, ReviewThreadEventKey(owner, repo, num, tid, seq), encodeThreadComment(c)); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				continue // reserved seq taken (bug-signal path) — retry the loop, never leak
			}
			return nil, cerr
		}
		out = c
		break
	}
	if out == nil {
		return nil, fmt.Errorf("%w: review thread %s changed concurrently; reload and retry", ErrConflict, tid)
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "thread_commented", Actor: who, PullNum: num, Recipients: prParticipants(h, who)})
	s.emitMentioned(ctx, owner, repo, num, who, body)
	s.stream(ctx, StreamEvent{Name: "thread", Repo: repoName(owner, repo), Action: "commented", Num: num, TID: tid})
	return out, nil
}

// canResolve reports whether actor may resolve th: the opener, a review
// participant (has commented), or triage+. The comment scan is bounded by
// the thread's comment count (human-scale).
func (s *Service) canResolve(ctx context.Context, owner, repo string, num int, th *ThreadHeader, actor auth.Principal) bool {
	who := normPrincipal(actor.Name)
	if normPrincipal(th.CreatedBy) == who {
		return true
	}
	if s.requireRole(ctx, owner, repo, actor, "triage") == nil {
		return true
	}
	comments, err := s.scanThreadComments(ctx, owner, repo, num, th.TID)
	if err != nil {
		return false
	}
	for _, c := range comments {
		if normPrincipal(c.By) == who {
			return true
		}
	}
	return false
}

// setResolved CASes the thread header's resolution state (resolve and
// unresolve share the loop; resolve racing a comment re-reads, recomputes
// the full header, and retries — last writer wins on a re-read, never on
// a stale blind write).
func (s *Service) setResolved(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal, resolve bool) (*ThreadHeader, error) {
	if err := validateTID(tid); err != nil {
		return nil, err
	}
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	key := ReviewThreadKey(owner, repo, num, tid)
	for attempt := 0; attempt < 10; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, fmt.Errorf("%w: review thread %s", ErrNotFound, tid)
		}
		th, perr := parseThreadHeader(raw)
		if perr != nil {
			return nil, perr
		}
		if !s.canResolve(ctx, owner, repo, num, th, actor) {
			return nil, fmt.Errorf("%w: resolve needs the opener, a participant, or triage", ErrForbidden)
		}
		th.Resolved = resolve
		if resolve {
			th.ResolvedBy = who
			th.ResolvedAt = now
		} else {
			th.ResolvedBy = ""
			th.ResolvedAt = ""
		}
		th.UpdatedAt = now
		th.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodeThreadHeader(th),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, perr
		}
		action := "unresolved"
		class := "thread_unresolved"
		if resolve {
			action, class = "resolved", "thread_resolved"
		}
		s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: class, Actor: who, PullNum: num})
		s.stream(ctx, StreamEvent{Name: "thread", Repo: repoName(owner, repo), Action: action, Num: num, TID: tid})
		if _, serr := s.refreshSummary(ctx, owner, repo, num); serr != nil {
			return nil, serr
		}
		// Re-read so the caller sees the committed header (a racing
		// comment may have advanced counters underneath).
		if nraw, _, nerr := s.getJSON(ctx, key); nerr == nil && nraw != nil {
			if nth, nperr := parseThreadHeader(nraw); nperr == nil {
				return nth, nil
			}
		}
		return th, nil
	}
	return nil, fmt.Errorf("%w: review thread %s changed concurrently; reload and retry", ErrConflict, tid)
}

// ResolveThread marks a thread resolved (§7: opener, participants, or
// triage+).
func (s *Service) ResolveThread(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal) (*ThreadHeader, error) {
	return s.setResolved(ctx, owner, repo, num, tid, actor, true)
}

// UnresolveThread reopens a resolved thread (same auth as resolve).
func (s *Service) UnresolveThread(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal) (*ThreadHeader, error) {
	return s.setResolved(ctx, owner, repo, num, tid, actor, false)
}

// --- review requests (§5) ----------------------------------------------------

// loadRequests reads review-requests.json; (nil, nil) when absent (no
// requests yet — not an error).
func (s *Service) loadRequests(ctx context.Context, owner, repo string, num int) (*ReviewRequests, error) {
	raw, _, err := s.getJSON(ctx, ReviewRequestsKey(owner, repo, num))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return parseReviewRequests(raw)
}

// GetRequests serves GET …/review-requests (§7): read → {reviewers}.
func (s *Service) GetRequests(ctx context.Context, owner, repo string, num int, p auth.Principal) (*ReviewRequests, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	reqs, err := s.loadRequests(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if reqs == nil {
		return &ReviewRequests{Reviewers: []RequestedReviewer{}}, nil
	}
	return reqs, nil
}

// mutateRequests runs one idempotent CAS loop over review-requests.json
// (dedup by principal — re-request is a no-op — so 412-retry converges; a
// review-submitted removal racing a re-request is last-writer-wins by CAS
// order, §5 Concurrency).
func (s *Service) mutateRequests(ctx context.Context, owner, repo string, num int, mutate func(reqs *ReviewRequests) bool) (*ReviewRequests, error) {
	key := ReviewRequestsKey(owner, repo, num)
	var result *ReviewRequests
	_, err := s.casUpdate(ctx, key, 10, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var reqs *ReviewRequests
		if cur == nil {
			reqs = &ReviewRequests{Reviewers: []RequestedReviewer{}}
		} else {
			var perr error
			if reqs, perr = parseReviewRequests(cur); perr != nil {
				return nil, false, perr
			}
		}
		if !mutate(reqs) {
			result = reqs
			return nil, false, nil
		}
		reqs.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		reqs.Version++
		result = reqs
		return encodeReviewRequests(reqs), true, nil
	})
	if err != nil {
		// casUpdate's exhaustion error already names the key; normalize
		// to the review-requests conflict for a stable wire message.
		if strings.Contains(err.Error(), "changed concurrently") {
			return nil, fmt.Errorf("%w: review requests changed concurrently; reload and retry", ErrConflict)
		}
		return nil, err
	}
	return result, nil
}

// AddRequests serves POST …/review-requests (§7: PR author or triage+):
// adds entries (dedup by principal), records by/at.
func (s *Service) AddRequests(ctx context.Context, owner, repo string, num int, actor auth.Principal, principals []string) (*ReviewRequests, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	h, _, err := s.prHeadOf(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	if normPrincipal(h.Author) != who {
		if err := s.requireRole(ctx, owner, repo, actor, "triage"); err != nil {
			return nil, err
		}
	}
	var add []string
	seen := map[string]bool{}
	for _, p := range principals {
		n := normPrincipal(p)
		if n == "" {
			return nil, fmt.Errorf("%w: reviewer must not be empty", ErrInvalid)
		}
		if !seen[n] {
			seen[n] = true
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return nil, fmt.Errorf("%w: reviewers must not be empty", ErrInvalid)
	}
	if len(add) > 50 {
		return nil, fmt.Errorf("%w: at most 50 reviewers per request", ErrInvalid)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	reqs, err := s.mutateRequests(ctx, owner, repo, num, func(reqs *ReviewRequests) bool {
		changed := false
		have := map[string]bool{}
		for _, r := range reqs.Reviewers {
			have[normPrincipal(r.Principal)] = true
		}
		for _, n := range add {
			if !have[n] {
				reqs.Reviewers = append(reqs.Reviewers, RequestedReviewer{Principal: n, By: who, At: now})
				have[n] = true
				changed = true
			}
		}
		return changed
	})
	if err != nil {
		return nil, err
	}
	if _, serr := s.refreshSummary(ctx, owner, repo, num); serr != nil {
		return nil, serr
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "review_requested", Actor: who, PullNum: num, Recipients: add})
	return reqs, nil
}

// RemoveRequests serves DELETE …/review-requests (§7: author/triage+ or
// self-removal — a requested principal may remove only themselves).
func (s *Service) RemoveRequests(ctx context.Context, owner, repo string, num int, actor auth.Principal, principals []string) (*ReviewRequests, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	h, _, err := s.prHeadOf(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	var drop []string
	seen := map[string]bool{}
	for _, p := range principals {
		n := normPrincipal(p)
		if n == "" {
			return nil, fmt.Errorf("%w: reviewer must not be empty", ErrInvalid)
		}
		if !seen[n] {
			seen[n] = true
			drop = append(drop, n)
		}
	}
	if len(drop) == 0 {
		return nil, fmt.Errorf("%w: reviewers must not be empty", ErrInvalid)
	}
	selfOnly := len(drop) == 1 && drop[0] == who
	if !selfOnly {
		if normPrincipal(h.Author) != who {
			if err := s.requireRole(ctx, owner, repo, actor, "triage"); err != nil {
				return nil, err
			}
		}
	}
	dropSet := map[string]bool{}
	for _, n := range drop {
		dropSet[n] = true
	}
	reqs, err := s.mutateRequests(ctx, owner, repo, num, func(reqs *ReviewRequests) bool {
		changed := false
		kept := reqs.Reviewers[:0]
		for _, r := range reqs.Reviewers {
			if dropSet[normPrincipal(r.Principal)] {
				changed = true
				continue
			}
			kept = append(kept, r)
		}
		// kept is nil only when Reviewers was nil (fresh/absent doc);
		// encodeReviewRequests normalizes nil to [] on the wire.
		reqs.Reviewers = kept
		return changed
	})
	if err != nil {
		return nil, err
	}
	if _, serr := s.refreshSummary(ctx, owner, repo, num); serr != nil {
		return nil, serr
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "review_request_removed", Actor: who, PullNum: num, Recipients: drop})
	return reqs, nil
}

// removeRequester retires one principal from review-requests (implicit
// removal on review submit, §5). Best-effort within the submit flow:
// bounded CAS, then ErrConflict — the submit fails rather than leaving a
// stale request behind (human-rate; retries converge).
func (s *Service) removeRequester(ctx context.Context, owner, repo string, num int, who string) error {
	_, err := s.mutateRequests(ctx, owner, repo, num, func(reqs *ReviewRequests) bool {
		changed := false
		kept := reqs.Reviewers[:0]
		for _, r := range reqs.Reviewers {
			if normPrincipal(r.Principal) == who {
				changed = true
				continue
			}
			kept = append(kept, r)
		}
		// kept is nil only when Reviewers was nil (fresh/absent doc);
		// encodeReviewRequests normalizes nil to [] on the wire.
		reqs.Reviewers = kept
		return changed
	})
	return err
}

// --- review-suggest (§5) -------------------------------------------------------

// accessBinding is the minimal access.json projection suggest needs (01
// owns the shape — subject/role only).
type accessBinding struct {
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

// Suggest serves GET …/review-suggest (§7: read): merges, in order,
// access.json role bindings with role ≥ read, org-team members of those
// bindings (via the GroupExpander seam; skipped when nil), then head-branch
// commit authors (via the CommitAuthors seam; skipped when nil).
// Prefix-filtered by q (case-insensitive), page size 20, no-store. LIST
// scope is bounded by the binding set (collaboration page, P5 fine).
func (s *Service) Suggest(ctx context.Context, owner, repo string, num int, p auth.Principal, q string) ([]string, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	var ordered []string
	seen := map[string]bool{}
	add := func(name string) {
		n := normPrincipal(name)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		ordered = append(ordered, n)
	}
	// Source 1+2: access.json bindings (user: directly; team: expanded).
	if raw, _, err := s.getJSON(ctx, AccessKey(owner, repo)); err != nil {
		return nil, err
	} else if raw != nil {
		var doc struct {
			RoleBindings []accessBinding `json:"role_bindings"`
		}
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: access.json: %v", ErrCorrupt, jerr)
		}
		for _, b := range doc.RoleBindings {
			if roleRank(b.Role) < roleRank("read") {
				continue
			}
			if name, ok := strings.CutPrefix(b.Subject, "user:"); ok {
				add(name)
				continue
			}
			if _, ok := strings.CutPrefix(b.Subject, "team:"); ok && s.Expander != nil {
				members, _ := s.Expander.ExpandGroups(ctx, []string{b.Subject})
				for _, m := range members {
					add(m)
				}
			}
		}
	}
	// Source 3: head-branch commit authors.
	if s.Authors != nil {
		if authors, aerr := s.Authors.HeadAuthors(ctx, owner, repo, num, 20); aerr == nil {
			for _, a := range authors {
				add(a)
			}
		}
	}
	q = strings.ToLower(strings.TrimSpace(q))
	var out []string
	for _, n := range ordered {
		if q != "" && !strings.HasPrefix(n, q) {
			continue
		}
		out = append(out, n)
		if len(out) >= 20 {
			break
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
