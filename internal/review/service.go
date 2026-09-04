package review

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the shared-family reads (PR header + pr.json sidecar),
// the review event scan, the review submits/dismissals, and the
// review_summary recompute + the merge-time gate.

// --- role helpers (P6, same ladder as internal/issues and internal/pulls) --

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// roleOf resolves the actor's repo role ("" when none). Host admin/write
// flags short-circuit through identity's own resolution.
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	got := s.roleOf(ctx, owner, repo, p)
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// requireRead enforces the read gate (identity require_read hook when
// wired: public visibility or role ≥ read).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// requireAuthenticated rejects anonymous callers (submits/comments need a
// principal to attribute).
func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}

// --- shared reads -----------------------------------------------------------

// loadPRHeader reads the PR thread header; (nil, "", nil) when absent.
func (s *Service) loadPRHeader(ctx context.Context, owner, repo string, num int) (*PRHeader, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, ThreadKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	h, perr := parsePRHeader(raw)
	if perr != nil {
		return nil, "", perr
	}
	return h, ver, nil
}

// loadSidecar reads the pr.json sidecar 03 owns; (nil, nil) when absent.
// Review never writes it — Head.SHA pins commit_sha, Base.Ref scopes the
// gate's rule match.
func (s *Service) loadSidecar(ctx context.Context, owner, repo string, num int) (*PRSidecar, error) {
	raw, _, err := s.getJSON(ctx, PRKey(owner, repo, num))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return parseSidecar(raw)
}

// prHeadOf returns the PR header + sidecar, 404 unless both exist and the
// thread is kind "pr" (review state never leaks across kinds).
func (s *Service) prHeadOf(ctx context.Context, owner, repo string, num int) (*PRHeader, *PRSidecar, error) {
	h, _, err := s.loadPRHeader(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if h == nil || h.Kind != "pr" {
		return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	side, err := s.loadSidecar(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if side == nil {
		return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	return h, side, nil
}

// scanReviews lists every review event of one PR in seq order (P3:
// timeline reads by seq order, not density — gaps skipped). One bounded
// prefix LIST over the low-volume reviews subtree (P5-fine) plus one GET
// per event — linear in review count (human-scale; E5 pins the cost).
func (s *Service) scanReviews(ctx context.Context, owner, repo string, num int) ([]*ReviewEvent, error) {
	var keys []string
	if err := s.Store.List(ctx, ReviewsPrefix(owner, repo, num), "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	// No ctx.Err() poll in the loop: every iteration is one ctx-bound GET
	// and the loop itself is bounded in-memory work (human-scale review
	// counts); cancellation surfaces from the store calls.
	out := make([]*ReviewEvent, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		ev, perr := parseReview(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, ev)
	}
	return out, nil
}

// scanThreadHeaders lists every thread header of one PR in tid order (one
// bounded LIST + one GET per thread — human-scale, P5-fine).
func (s *Service) scanThreadHeaders(ctx context.Context, owner, repo string, num int) ([]*ThreadHeader, error) {
	var keys []string
	if err := s.Store.List(ctx, ReviewThreadsPrefix(owner, repo, num), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, "/thread.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	// No ctx.Err() poll here either (same reasoning as scanReviews).
	out := make([]*ThreadHeader, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		h, perr := parseThreadHeader(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, h)
	}
	return out, nil
}

// --- review_summary recompute (§6) -------------------------------------------

// refreshSummary recomputes review_summary inside a CAS loop from the
// immutable event set (reviews scan + stored requests + thread headers)
// and persists it on the PR header. The summary is a pure function of the
// immutable set, so racing writers converge. Persistence is best-effort
// past the bounded retries (the summary is a render cache — P4
// philosophy: the next mutation repairs it, and the merge gate never
// trusts it), but the returned value always reflects the post-commit
// state.
func (s *Service) refreshSummary(ctx context.Context, owner, repo string, num int) (*ReviewSummary, error) {
	reviews, err := s.scanReviews(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	reqs, err := s.loadRequests(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	threads, err := s.scanThreadHeaders(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	var requested []string
	if reqs != nil {
		for _, r := range reqs.Reviewers {
			requested = append(requested, r.Principal)
		}
	}
	unresolved := 0
	for _, t := range threads {
		if !t.Resolved {
			unresolved++
		}
	}
	sum := Rollup(reviews, requested, len(threads), unresolved)
	key := ThreadKey(owner, repo, num)
	for attempt := 0; attempt < 10; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		h, perr := parsePRHeader(raw)
		if perr != nil {
			return nil, perr
		}
		h.ReviewSummary = sum
		h.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodePRHeader(h),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, perr
		}
		return sum, nil
	}
	// Bounded retries exhausted: the value is still correct (pure function
	// of committed state); the next mutation's CAS repairs the header.
	return sum, nil
}

// --- submit / dismiss / list / get --------------------------------------------

// NewThread is one line thread opened atomically with a review submit (§3).
type NewThread struct {
	Anchor Anchor
	Body   string
}

// SubmitInput shapes POST …/reviews (§7): {state, body?, commit_sha,
// threads?}.
type SubmitInput struct {
	State     string
	Body      string
	CommitSHA string
	Threads   []NewThread
}

// SubmitReview posts one immutable review (§3): server-checks commit_sha
// against the PR head (else 409), rejects author self-approve/
// request-changes (422), atomically opens attached line threads, removes
// the reviewer from review-requests, recomputes the summary, and fans out
// (P8). Auth: read (authenticated).
func (s *Service) SubmitReview(ctx context.Context, owner, repo string, num int, actor auth.Principal, in SubmitInput) (*ReviewEvent, []*ThreadHeader, *ReviewSummary, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, nil, err
	}
	if err := validateState(in.State); err != nil {
		return nil, nil, nil, err
	}
	if err := validateBody(in.Body); err != nil {
		return nil, nil, nil, err
	}
	if err := validateSHA(in.CommitSHA); err != nil {
		return nil, nil, nil, err
	}
	for i, t := range in.Threads {
		if err := validateAnchor(t.Anchor); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: threads[%d]: %v", ErrInvalid, i, err)
		}
		if strings.TrimSpace(t.Body) == "" {
			return nil, nil, nil, fmt.Errorf("%w: threads[%d].body must not be empty", ErrInvalid, i)
		}
		if err := validateBody(t.Body); err != nil {
			return nil, nil, nil, err
		}
	}
	h, side, err := s.prHeadOf(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, nil, err
	}
	who := normPrincipal(actor.Name)
	if normPrincipal(h.Author) == who && in.State != StateCommented {
		return nil, nil, nil, fmt.Errorf("%w: author cannot approve their own pull request", ErrUnprocessable)
	}
	if !strings.EqualFold(side.Head.SHA, in.CommitSHA) {
		return nil, nil, nil, fmt.Errorf("%w: reviewed commit is not the pull request head", ErrConflict)
	}
	// Reserve the review seq + tids on the ONE header CAS (§4: one
	// arbitration point; loser re-reads and retries).
	key := ThreadKey(owner, repo, num)
	seq := 0
	var tids []string
	const maxAttempts = 10
	reserved := false
	for attempt := 0; attempt < maxAttempts && !reserved; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, nil, nil, err
		}
		if raw == nil {
			return nil, nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		cur, perr := parsePRHeader(raw)
		if perr != nil {
			return nil, nil, nil, perr
		}
		if cur.Kind != "pr" {
			return nil, nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		nextSeq := cur.NextReviewSeq
		if nextSeq < 1 {
			nextSeq = 1
		}
		nextTid := cur.NextThreadNum
		if nextTid < 1 {
			nextTid = 1
		}
		seq = nextSeq
		tids = tids[:0]
		for range in.Threads {
			tids = append(tids, fmt.Sprintf("%08x", nextTid))
			nextTid++
		}
		cur.NextReviewSeq = nextSeq + 1
		cur.NextThreadNum = nextTid
		cur.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		cur.Participants = uniqSorted(append(cur.Participants, who))
		cur.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodePRHeader(cur),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, nil, nil, perr
		}
		reserved = true
	}
	if !reserved {
		return nil, nil, nil, fmt.Errorf("%w: pull request %d changed concurrently; reload and retry", ErrConflict, num)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	ev := &ReviewEvent{Kind: KindReview, Seq: seq, At: now, By: who, State: in.State, CommitSHA: strings.ToLower(in.CommitSHA), Body: in.Body}
	if cerr := s.putCreate(ctx, ReviewKey(owner, repo, num, seq), encodeReview(ev)); cerr != nil {
		if store.IsPreconditionFailed(cerr) {
			return nil, nil, nil, fmt.Errorf("%w: review %d already exists", ErrConflict, seq)
		}
		return nil, nil, nil, cerr
	}
	// Atomically opened threads: the review event is the summary record;
	// each thread header lands with its first comment (§4).
	var opened []*ThreadHeader
	for i, t := range in.Threads {
		th := &ThreadHeader{
			TID: tids[i], Num: num, Kind: "review_thread", Anchor: t.Anchor,
			CommentCount: 1, NextEventSeq: 2,
			CreatedAt: now, CreatedBy: who, UpdatedAt: now, Version: 1,
		}
		if cerr := s.putCreate(ctx, ReviewThreadKey(owner, repo, num, tids[i]), encodeThreadHeader(th)); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				return nil, nil, nil, fmt.Errorf("%w: review thread %s already exists", ErrConflict, tids[i])
			}
			return nil, nil, nil, cerr
		}
		c := &ThreadComment{Kind: KindReviewThreadComment, Seq: 1, At: now, By: who, Body: t.Body}
		if cerr := s.putCreate(ctx, ReviewThreadEventKey(owner, repo, num, tids[i], 1), encodeThreadComment(c)); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				return nil, nil, nil, fmt.Errorf("%w: review thread %s already exists", ErrConflict, tids[i])
			}
			return nil, nil, nil, cerr
		}
		opened = append(opened, th)
	}
	if opened == nil {
		opened = []*ThreadHeader{}
	}
	// A submitted review retires the reviewer's request entry (§5).
	if rerr := s.removeRequester(ctx, owner, repo, num, who); rerr != nil {
		return nil, nil, nil, rerr
	}
	sum, err := s.refreshSummary(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, nil, err
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "review_submitted", Actor: who, PullNum: num, Recipients: prParticipants(h, who)})
	s.emitMentioned(ctx, owner, repo, num, who, in.Body)
	s.stream(ctx, StreamEvent{Name: "review", Repo: repoName(owner, repo), Action: "submitted", Num: num, Summary: sum})
	return ev, opened, sum, nil
}

// prParticipants fans subscribed notifications to participants minus actor.
func prParticipants(h *PRHeader, actor string) []string {
	var out []string
	for _, p := range h.Participants {
		if p != "" && p != actor {
			out = append(out, p)
		}
	}
	return out
}

// DismissReview appends a compensating review_dismissed event (§3:
// maintain only; events are never rewritten). The rollup demotes the
// reviewer's latest to DISMISSED while the dismissal targets it.
func (s *Service) DismissReview(ctx context.Context, owner, repo string, num, seq int, actor auth.Principal, reason string) (*ReviewEvent, *ReviewSummary, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "maintain"); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, nil, fmt.Errorf("%w: reason must not be empty", ErrInvalid)
	}
	h, _, err := s.prHeadOf(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	raw, _, err := s.getJSON(ctx, ReviewKey(owner, repo, num, seq))
	if err != nil {
		return nil, nil, err
	}
	if raw == nil {
		return nil, nil, fmt.Errorf("%w: review %d", ErrNotFound, seq)
	}
	target, perr := parseReview(raw)
	if perr != nil {
		return nil, nil, perr
	}
	if target.Kind != KindReview {
		return nil, nil, fmt.Errorf("%w: review %d is not a review", ErrConflict, seq)
	}
	reviews, err := s.scanReviews(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if isDismissed(reviews, target.By, seq) {
		return nil, nil, fmt.Errorf("%w: review %d already dismissed", ErrConflict, seq)
	}
	key := ThreadKey(owner, repo, num)
	dseq := 0
	reserved := false
	for attempt := 0; attempt < 10 && !reserved; attempt++ {
		rraw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		if rraw == nil {
			return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		cur, perr := parsePRHeader(rraw)
		if perr != nil {
			return nil, nil, perr
		}
		nextSeq := cur.NextReviewSeq
		if nextSeq < 1 {
			nextSeq = 1
		}
		dseq = nextSeq
		cur.NextReviewSeq = nextSeq + 1
		cur.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		cur.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodePRHeader(cur),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, nil, perr
		}
		reserved = true
	}
	if !reserved {
		return nil, nil, fmt.Errorf("%w: pull request %d changed concurrently; reload and retry", ErrConflict, num)
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	ev := &ReviewEvent{Kind: KindReviewDismissed, Seq: dseq, At: now, By: who, Dismisses: intPtr(seq), Reason: reason}
	if cerr := s.putCreate(ctx, ReviewKey(owner, repo, num, dseq), encodeReview(ev)); cerr != nil {
		if store.IsPreconditionFailed(cerr) {
			return nil, nil, fmt.Errorf("%w: review %d already exists", ErrConflict, dseq)
		}
		return nil, nil, cerr
	}
	sum, err := s.refreshSummary(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "review_dismissed", Actor: who, PullNum: num, Recipients: prParticipants(h, who)})
	s.stream(ctx, StreamEvent{Name: "review", Repo: repoName(owner, repo), Action: "dismissed", Num: num, Summary: sum})
	return ev, sum, nil
}

// isDismissed reports whether seq is still the reviewer's latest and a
// dismissal targets it (i.e. the rollup currently demotes it).
func isDismissed(reviews []*ReviewEvent, by string, seq int) bool {
	latestSeq := -1
	for _, ev := range reviews {
		if ev.Kind == KindReview && ev.By == by && ev.Seq > latestSeq {
			latestSeq = ev.Seq
		}
	}
	if latestSeq != seq {
		return false
	}
	for _, ev := range reviews {
		if ev.Kind == KindReviewDismissed && ev.Dismisses != nil && *ev.Dismisses == seq {
			return true
		}
	}
	return false
}

// latestOf folds the immutable review events into the per-reviewer latest
// map (the shared core of Rollup and the merge gate — both derive from the
// same scan, never from the cached summary).
func latestOf(reviews []*ReviewEvent) map[string]LatestReview {
	bySeq := map[int]*ReviewEvent{}
	latest := map[string]LatestReview{}
	order := append([]*ReviewEvent(nil), reviews...)
	sort.Slice(order, func(i, j int) bool { return order[i].Seq < order[j].Seq })
	for _, ev := range order {
		switch ev.Kind {
		case KindReview:
			bySeq[ev.Seq] = ev
			latest[ev.By] = LatestReview{State: ev.State, Seq: ev.Seq, CommitSHA: ev.CommitSHA, At: ev.At}
		case KindReviewDismissed:
			if ev.Dismisses == nil {
				continue
			}
			target, ok := bySeq[*ev.Dismisses]
			if !ok {
				continue
			}
			cur, ok := latest[target.By]
			if !ok || cur.Seq != target.Seq {
				continue
			}
			latest[target.By] = LatestReview{State: StateDismissed, Seq: target.Seq, CommitSHA: target.CommitSHA, At: target.At}
		}
	}
	return latest
}

// ListResult is the paged reviews answer.
type ListResult struct {
	Reviews []*ReviewEvent `json:"reviews"`
	More    bool           `json:"more"`
}

// ListReviews serves GET …/reviews (§7): read, newest-first, after =
// exclusive upper bound (seqs strictly below), n default 50 capped 200.
func (s *Service) ListReviews(ctx context.Context, owner, repo string, num int, p auth.Principal, after, n int) (*ListResult, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	reviews, err := s.scanReviews(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	var filtered []*ReviewEvent
	for i := len(reviews) - 1; i >= 0; i-- {
		if after > 0 && reviews[i].Seq >= after {
			continue
		}
		filtered = append(filtered, reviews[i])
		if len(filtered) > n {
			break
		}
	}
	more := len(filtered) > n
	if more {
		filtered = filtered[:n]
	}
	if filtered == nil {
		filtered = []*ReviewEvent{}
	}
	return &ListResult{Reviews: filtered, More: more}, nil
}

// GetReview serves GET …/reviews/{seq} (§7): read, 404 unknown.
func (s *Service) GetReview(ctx context.Context, owner, repo string, num, seq int, p auth.Principal) (*ReviewEvent, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	raw, _, err := s.getJSON(ctx, ReviewKey(owner, repo, num, seq))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: review %d", ErrNotFound, seq)
	}
	return parseReview(raw)
}

// --- merge-time gate (§6) -----------------------------------------------------

// GateVerdict is the authoritative merge-time verdict (re-derived by scan,
// never read from review_summary).
type GateVerdict struct {
	Approvals int      `json:"approvals"`
	Needed    int      `json:"needed"`
	Blocking  []string `json:"blocking"` // reviewer principals with surviving CHANGES_REQUESTED
	StaleOK   bool     `json:"-"`        // dismiss_stale combined OFF (informational)
}

// CheckRequiredReviews is the merge-time half of the required-reviews
// effect, consulted by 03's merge task before publishing the merge ref
// (the review-provided gate function wired into the merge path via pulls'
// ReviewGate seam — the merge logic is NOT forked). It resolves every
// required-reviews rule matching the PR's base ref and requires: surviving
// approvals ≥ min_approvals (most restrictive across matching rules), no
// surviving CHANGES_REQUESTED, and — when dismiss_stale — only approvals
// whose commit_sha equals the current head count. No matching rules ⇒ nil
// (gate passes). The scan runs under its own deadline inside the merge
// task's context; a blown deadline fails closed. Bypass lists apply
// unchanged: a merger bypassing EVERY matching rule skips the gate.
func (s *Service) CheckRequiredReviews(ctx context.Context, owner, repo string, num int, headSHA, baseRef, merger string) error {
	gctx, cancel := context.WithTimeout(ctx, s.gateTimeout())
	defer cancel()
	_, verr := s.EvaluateGate(gctx, owner, repo, num, headSHA, baseRef, merger)
	if verr != nil {
		return verr
	}
	return nil
}

// EvaluateGate runs the authoritative scan and returns the verdict (nil
// verdict + nil error ⇒ gate passes). Exported for tests and the E5
// harness; CheckRequiredReviews is the pulls seam entry point.
func (s *Service) EvaluateGate(ctx context.Context, owner, repo string, num int, headSHA, baseRef, merger string) (*GateVerdict, error) {
	if err := validateSHA(headSHA); err != nil {
		return nil, err
	}
	raw, _, err := s.getJSON(ctx, "repos/"+owner+"/"+repo+"/policy.json")
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil // no policy ⇒ no required-reviews rules ⇒ pass
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return nil, fmt.Errorf("%w: policy.json unparseable: %v", ErrConflict, perr)
	}
	rules := policy.MatchingRules(doc, policy.Request{Principal: merger, Ref: baseRef, Op: policy.OpUpdate})
	var gated []*policy.Rule
	for _, r := range rules {
		if r.RequiredReviews() != nil {
			gated = append(gated, r)
		}
	}
	if len(gated) == 0 {
		return nil, nil
	}
	// Most-restrictive combination (§6): max approvals, dismiss_stale OR,
	// bypass only when EVERY matching rule's bypass admits the merger (an
	// absent/empty bypass admits nobody — fail-closed).
	need := 0
	stale := false
	allBypassed := true
	for _, r := range gated {
		e := r.RequiredReviews()
		if e.MinApprovals > need {
			need = e.MinApprovals
		}
		if e.DismissStale {
			stale = true
		}
		if len(e.Bypass) == 0 || !policy.Bypassed(e.Bypass, merger, nil, doc.Roster()) {
			allBypassed = false
		}
	}
	if allBypassed {
		return nil, nil
	}
	if _, _, err := s.prHeadOf(ctx, owner, repo, num); err != nil {
		return nil, err
	}
	reviews, err := s.scanReviews(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	latest := latestOf(reviews)
	var blocking []string
	approvals := 0
	for who, l := range latest {
		switch l.State {
		case StateChangesRequested:
			blocking = append(blocking, who)
		case StateApproved:
			if !stale || strings.EqualFold(l.CommitSHA, headSHA) {
				approvals++
			}
		}
	}
	sort.Strings(blocking)
	if len(blocking) > 0 {
		return &GateVerdict{Approvals: approvals, Needed: need, Blocking: blocking},
			fmt.Errorf("%w: required-reviews: changes requested by %s", ErrConflict, strings.Join(blocking, ", "))
	}
	if approvals < need {
		return &GateVerdict{Approvals: approvals, Needed: need, Blocking: []string{}},
			fmt.Errorf("%w: required-reviews: need %d approvals, have %d", ErrConflict, need, approvals)
	}
	return &GateVerdict{Approvals: approvals, Needed: need, Blocking: []string{}, StaleOK: !stale}, nil
}
