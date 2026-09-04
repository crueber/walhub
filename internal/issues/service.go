package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns mutation policy: who may change what (P6 gates via
// RoleService), which events each change emits, and the §10 notification
// fan-out points. Raw bucket I/O stays in store.go.

// --- role helpers (P6) --------------------------------------------------------

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin (local copy: identity.Role.rank is unexported and this
// package must not reach into identity internals beyond the RoleService
// seam).
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
// wired: public visibility or role ≥ read; private + stranger →
// 401 anonymous / 403 authenticated).
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

// requireAuthenticated rejects anonymous callers (create/comment/react
// need a principal to attribute).
func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}

// checkAssignees validates assignee names: each must resolve to role ≥
// triage (P6), naming the offender 400. Stored sorted, unique, ≤ 10.
func (s *Service) checkAssignees(ctx context.Context, owner, repo string, names []string) ([]string, error) {
	if len(names) > MaxAssignees {
		return nil, fmt.Errorf("%w: at most %d assignees", ErrInvalid, MaxAssignees)
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		name := normPrincipal(n)
		if !identity.ValidPrincipal(name) {
			return nil, fmt.Errorf("%w: invalid assignee %q", ErrInvalid, n)
		}
		role := s.roleOf(ctx, owner, repo, auth.Principal{Name: name})
		if roleRank(role) < roleRank(string(identity.RoleTriage)) {
			return nil, fmt.Errorf("%w: assignee %q resolves below triage", ErrInvalid, name)
		}
		out = append(out, name)
	}
	return uniqSorted(out), nil
}

// --- create ------------------------------------------------------------------

// CreateIssue allocates a number (P2), Creates the thread header, Creates
// the opened event, parses #N refs at write time (§6), updates the index,
// and emits subscribed/mentioned notifications (§10). Auth: read
// (authenticated) — any principal passing the read gate.
func (s *Service) CreateIssue(ctx context.Context, owner, repo string, actor auth.Principal, title, body string) (*Thread, *Event, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	t, err := validateTitle(title)
	if err != nil {
		return nil, nil, err
	}
	if body != "" {
		if err := validateBody(body); err != nil {
			return nil, nil, err
		}
	}
	num, err := s.allocNum(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	th := &Thread{
		Num: num, Kind: "issue", Title: t, State: StateOpen,
		Author: normPrincipal(actor.Name), CreatedAt: now, UpdatedAt: now,
		Labels: []string{}, Assignees: []string{},
		Participants: []string{normPrincipal(actor.Name)},
		NextEventSeq: 1, CommentCount: 0,
		ReactionSummary: map[string]map[string]int{}, Version: 1,
	}
	ev := &Event{Seq: 0, Type: EventOpened, Actor: normPrincipal(actor.Name), At: now, Body: strPtr(body)}
	if _, err := store.PutBytes(ctx, s.Store, ThreadKey(owner, repo, num), encodeThread(th),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, nil, fmt.Errorf("%w: issue %d already exists", ErrConflict, num)
		}
		return nil, nil, err
	}
	if err := s.putCreate(ctx, EventKey(owner, repo, num, 0), encodeEvent(ev)); err != nil {
		return nil, nil, err
	}
	s.updateIndex(ctx, owner, repo, cardOf(th))
	// Opened-body refs source from the thread itself; comment bodies
	// source from their event seq.
	s.writeRefs(ctx, owner, repo, num, -1, normPrincipal(actor.Name), body)
	s.emitSubscribed(ctx, owner, repo, th, normPrincipal(actor.Name))
	s.emitMentioned(ctx, owner, repo, num, normPrincipal(actor.Name), body)
	s.stream(ctx, StreamEvent{Name: "issue", Repo: repoName(owner, repo), IssueNum: num})
	nt, _, _ := s.loadThread(ctx, owner, repo, num)
	if nt != nil {
		th = nt
	}
	return th, ev, nil
}

// --- comment -----------------------------------------------------------------

// AddComment appends a commented event through the two-step, fans out #N
// references (§6), maintains participants[] + comment_count in the same
// CAS, and emits subscribed/mentioned notifications. Auth: read
// (authenticated).
func (s *Service) AddComment(ctx context.Context, owner, repo string, num int, actor auth.Principal, body string) (*Event, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := validateBody(body); err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	th, ev, err := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
		if t.Kind != "issue" {
			return nil, fmt.Errorf("%w: unknown issue", ErrNotFound)
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
	s.updateIndex(ctx, owner, repo, cardOf(th))
	s.writeRefs(ctx, owner, repo, num, ev.Seq, who, body)
	s.emitSubscribed(ctx, owner, repo, th, who)
	s.emitMentioned(ctx, owner, repo, num, who, body)
	s.stream(ctx, StreamEvent{Name: "issue_event", Repo: repoName(owner, repo), IssueNum: num})
	return ev, nil
}

// writeRefs parses #N mentions at write time and writes one durable event
// per surviving ref onto the TARGET thread (§6): referenced (same repo)
// or cross_referenced (different repo). The commenting actor needs read
// on the target repo; missing targets are silently skipped (best-effort,
// never a 4xx for the commenter).
func (s *Service) writeRefs(ctx context.Context, owner, repo string, srcNum, srcSeq int, actor, body string) {
	if body == "" {
		return
	}
	sourceRepo := repoName(owner, repo)
	for _, ref := range ParseRefs(body) {
		tOwner, tRepo := owner, repo
		typ := EventReferenced
		if ref.CrossRepo != "" {
			// CrossRepo always carries owner/repo (the §6 parser
			// contract); no other shape reaches this loop.
			parts := strings.SplitN(ref.CrossRepo, "/", 2)
			tOwner, tRepo = parts[0], parts[1]
			typ = EventCrossReferenced
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := s.requireRead(cctx, tOwner, tRepo, auth.Principal{Name: actor}); err != nil {
				cancel()
				continue
			}
			cancel()
		}
		src := map[string]any{"repo": sourceRepo, "num": srcNum}
		if srcSeq >= 0 {
			// Comment bodies source kind:"comment" with their event
			// seq; opened bodies source kind:"thread" (no event_seq).
			src["kind"] = "comment"
			src["event_seq"] = srcSeq
		} else {
			src["kind"] = "thread"
		}
		now := s.nowUTC().Format(dateTimeFmt)
		_, _, _ = s.appendEvent(ctx, tOwner, tRepo, ref.Num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Version++
			return &Event{Seq: seq, Type: typ, Actor: actor, At: now, Source: src}, nil
		})
		// A missing target (unknown num) is silently skipped: appendEvent
		// reports ErrNotFound and the comment itself already committed.
	}
}

// --- patch -------------------------------------------------------------------

// IssuePatch carries the optional PATCH fields (§7). Unknown keys are
// rejected at the HTTP layer before this is built.
type IssuePatch struct {
	Title       *string
	State       *string
	StateReason *string
	Labels      *[]string
	Assignees   *[]string
	Milestone   **string // nil = untouched; *nil = clear
}

// PatchIssue applies one event group per changed field, in deterministic
// order (title, labels, assignees, milestone, state), each through its own
// P3 two-step. No-op fields are omitted (no event). Auth: title/state —
// author or triage; labels/assignees/milestone — triage.
func (s *Service) PatchIssue(ctx context.Context, owner, repo string, num int, actor auth.Principal, p IssuePatch) (*Thread, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "issue" {
		return nil, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	who := normPrincipal(actor.Name)
	isAuthor := normPrincipal(th.Author) == who
	canMod := isAuthor
	if !canMod {
		if rerr := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); rerr != nil {
			// Non-triage non-authors may still touch nothing; the per-key
			// checks below produce the precise error.
			_ = rerr
		} else {
			canMod = true
		}
	}
	triage := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)) == nil

	// Validate everything before mutating (fail closed, no partial PATCH).
	var title *string
	if p.Title != nil {
		if !canMod {
			return nil, fmt.Errorf("%w: title changes need author or triage", ErrForbidden)
		}
		t, verr := validateTitle(*p.Title)
		if verr != nil {
			return nil, verr
		}
		if t != th.Title {
			title = &t
		}
	}
	var labels *[]string
	if p.Labels != nil {
		if !triage {
			return nil, fmt.Errorf("%w: label changes need triage", ErrForbidden)
		}
		ls, _, lerr := s.loadLabels(ctx, owner, repo)
		if lerr != nil {
			return nil, lerr
		}
		norm := make([]string, 0, len(*p.Labels))
		for _, n := range *p.Labels {
			ln, verr := validateLabelName(n)
			if verr != nil {
				return nil, verr
			}
			// Canonicalize to the stored spelling.
			if found := findLabel(ls, ln); found != nil {
				ln = found.Name
			} else {
				return nil, fmt.Errorf("%w: unknown label %q", ErrInvalid, n)
			}
			norm = append(norm, ln)
		}
		norm = uniqSorted(norm)
		if !equalStr(th.Labels, norm) {
			labels = &norm
		}
	}
	var assignees *[]string
	var addedAssignees []string
	if p.Assignees != nil {
		if !triage {
			return nil, fmt.Errorf("%w: assignee changes need triage", ErrForbidden)
		}
		norm, verr := s.checkAssignees(ctx, owner, repo, *p.Assignees)
		if verr != nil {
			return nil, verr
		}
		if !equalStr(th.Assignees, norm) {
			have := map[string]bool{}
			for _, a := range th.Assignees {
				have[a] = true
			}
			for _, a := range norm {
				if !have[a] {
					addedAssignees = append(addedAssignees, a)
				}
			}
			assignees = &norm
		}
	}
	var milestone **string
	var msFrom, msTo *string
	if p.Milestone != nil {
		if !triage {
			return nil, fmt.Errorf("%w: milestone changes need triage", ErrForbidden)
		}
		want := *p.Milestone // may be nil = clear
		if want != nil {
			id := strings.ToLower(strings.TrimSpace(*want))
			m, _, merr := s.loadMilestone(ctx, owner, repo, id)
			if merr != nil {
				return nil, merr
			}
			if m == nil {
				return nil, fmt.Errorf("%w: unknown milestone %q", ErrInvalid, *want)
			}
			want = &m.ID
		}
		if !equalPtr(th.Milestone, want) {
			msFrom, msTo = th.Milestone, want
			milestone = &want
		}
	}
	var state, reason *string
	if p.State != nil {
		if !canMod {
			return nil, fmt.Errorf("%w: state changes need author or triage", ErrForbidden)
		}
		want := strings.ToLower(strings.TrimSpace(*p.State))
		if want != StateOpen && want != StateClosed {
			return nil, fmt.Errorf("%w: state must be open|closed, got %q", ErrInvalid, *p.State)
		}
		if want == th.State && (want == StateOpen || equalPtrReason(th.StateReason, p.StateReason)) {
			// no-op
		} else {
			if want == StateClosed {
				r := ReasonCompleted
				if p.StateReason != nil && *p.StateReason != "" {
					rl := strings.ToLower(strings.TrimSpace(*p.StateReason))
					if rl != ReasonCompleted && rl != ReasonNotPlanned {
						return nil, fmt.Errorf("%w: state_reason must be completed|not_planned", ErrInvalid)
					}
					r = rl
				}
				reason = &r
			}
			state = &want
		}
	}

	now := s.nowUTC().Format(dateTimeFmt)
	apply := func(mutate func(t *Thread, seq int) (*Event, error)) error {
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, mutate)
		if aerr != nil {
			return aerr
		}
		th = nt
		return nil
	}
	if title != nil {
		from := th.Title
		to := *title
		if err := apply(func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Title = to
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventTitleChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to)}, nil
		}); err != nil {
			return nil, err
		}
	}
	if labels != nil {
		added, removed := diffStr(th.Labels, *labels)
		want := *labels
		if err := apply(func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Labels = want
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventLabelsChanged, Actor: who, At: now, Added: added, Removed: removed}, nil
		}); err != nil {
			return nil, err
		}
	}
	if assignees != nil {
		added, removed := diffStr(th.Assignees, *assignees)
		want := *assignees
		if err := apply(func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Assignees = want
			t.Participants = uniqSorted(append(append(t.Participants, who), want...))
			t.Version++
			return &Event{Seq: seq, Type: EventAssigneesChanged, Actor: who, At: now, Added: added, Removed: removed}, nil
		}); err != nil {
			return nil, err
		}
		for _, a := range addedAssignees {
			s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "assigned", Actor: who, IssueNum: num, Recipients: []string{a}})
		}
	}
	if milestone != nil {
		from, to := msFrom, msTo
		want := *milestone
		if err := apply(func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Milestone = want
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventMilestoneChanged, Actor: who, At: now, From: from, To: to}, nil
		}); err != nil {
			return nil, err
		}
		s.moveMilestone(ctx, owner, repo, th.State, from, to)
	}
	if state != nil {
		from := th.State
		to := *state
		r := reason
		if err := apply(func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.State = to
			if to == StateClosed {
				t.StateReason = r
			} else {
				t.StateReason = nil
			}
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventStateChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to), Reason: t.StateReason}, nil
		}); err != nil {
			return nil, err
		}
		if th.Milestone != nil {
			if to == StateClosed {
				s.bumpMilestone(ctx, owner, repo, *th.Milestone, -1, +1)
			} else {
				s.bumpMilestone(ctx, owner, repo, *th.Milestone, +1, -1)
			}
		}
	}
	s.updateIndex(ctx, owner, repo, cardOf(th))
	if title != nil || labels != nil || assignees != nil || milestone != nil || state != nil {
		s.emitSubscribed(ctx, owner, repo, th, who)
		s.stream(ctx, StreamEvent{Name: "issue", Repo: repoName(owner, repo), IssueNum: num})
	}
	return th, nil
}

// moveMilestone transfers denormalized counters between milestones on
// milestone (re)assignment: leaving decrements the open or closed bucket
// matching the issue state; joining increments it. Callers only invoke it
// on change (from != to).
func (s *Service) moveMilestone(ctx context.Context, owner, repo, state string, from, to *string) {
	dec := func(id *string) {
		if id == nil {
			return
		}
		if state == StateClosed {
			s.bumpMilestone(ctx, owner, repo, *id, 0, -1)
		} else {
			s.bumpMilestone(ctx, owner, repo, *id, -1, 0)
		}
	}
	inc := func(id *string) {
		if id == nil {
			return
		}
		if state == StateClosed {
			s.bumpMilestone(ctx, owner, repo, *id, 0, +1)
		} else {
			s.bumpMilestone(ctx, owner, repo, *id, +1, 0)
		}
	}
	dec(from)
	inc(to)
}

// --- reactions ---------------------------------------------------------------

// reactionState folds a thread's reaction_changed events into the live
// per-(actor, target, content) set (log is the truth; the header summary
// is the O(1) view).
func reactionState(events []*Event) map[string]bool {
	live := map[string]bool{}
	for _, e := range events {
		if e.Type != EventReactionChanged || e.TargetEventSeq == nil || e.Content == nil || e.Op == nil {
			continue
		}
		key := e.Actor + "\x00" + itoa(*e.TargetEventSeq) + "\x00" + *e.Content
		live[key] = *e.Op == "add"
	}
	return live
}

// AddReaction appends a reaction_changed add (§8): target must be an
// opened/commented event, content in the closed set, one
// (principal, target, content) UNIQUE — a duplicate add is a no-op
// returning the summary, not an event. The summary ±1 rides the SAME
// header CAS that reserves the seq (one CAS, no extra round trip).
// Returns the committed thread, the written event (nil on duplicate
// no-op), whether an event was written, and an error.
func (s *Service) AddReaction(ctx context.Context, owner, repo string, num, target int, actor auth.Principal, content string) (*Thread, *Event, bool, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, false, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, false, err
	}
	c := strings.ToLower(strings.TrimSpace(content))
	if !ReactionContents[c] {
		return nil, nil, false, fmt.Errorf("%w: unknown reaction %q", ErrInvalid, content)
	}
	who := normPrincipal(actor.Name)
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, false, err
	}
	if th == nil || th.Kind != "issue" {
		return nil, nil, false, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	te, err := s.loadEvent(ctx, owner, repo, num, target)
	if err != nil {
		return nil, nil, false, err
	}
	if te == nil {
		return nil, nil, false, fmt.Errorf("%w: unknown target event", ErrInvalid)
	}
	if te.Type != EventOpened && te.Type != EventCommented {
		return nil, nil, false, fmt.Errorf("%w: reactions target opened/commented events only", ErrInvalid)
	}
	events, err := s.scanEvents(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, false, err
	}
	if reactionState(events)[who+"\x00"+itoa(target)+"\x00"+c] {
		th, _, lerr := s.loadThread(ctx, owner, repo, num)
		if lerr != nil {
			return nil, nil, false, lerr
		}
		if th == nil {
			return nil, nil, false, fmt.Errorf("%w: unknown issue", ErrNotFound)
		}
		return th, nil, false, nil // duplicate add: no-op, current summary
	}
	now := s.nowUTC().Format(dateTimeFmt)
	th, ev, err := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
		t.NextEventSeq = seq + 1
		t.UpdatedAt = now
		t.Participants = uniqSorted(append(t.Participants, who))
		t.Version++
		key := seqKey(target)
		m := t.ReactionSummary[key]
		if m == nil {
			m = map[string]int{}
		}
		m[c]++
		t.ReactionSummary[key] = m
		return &Event{Seq: seq, Type: EventReactionChanged, Actor: who, At: now,
			TargetEventSeq: &target, Content: strPtr(c), Op: strPtr("add")}, nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	s.updateIndex(ctx, owner, repo, cardOf(th))
	s.stream(ctx, StreamEvent{Name: "issue_event", Repo: repoName(owner, repo), IssueNum: num})
	return th, ev, true, nil
}

// RemoveReaction appends a reaction_changed remove for the actor's OWN
// reaction only (unknown → 404); the summary −1 rides the same CAS.
func (s *Service) RemoveReaction(ctx context.Context, owner, repo string, num, target int, actor auth.Principal, content string) (*Thread, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	c := strings.ToLower(strings.TrimSpace(content))
	if !ReactionContents[c] {
		return nil, fmt.Errorf("%w: unknown reaction %q", ErrInvalid, content)
	}
	who := normPrincipal(actor.Name)
	if th, _, err := s.loadThread(ctx, owner, repo, num); err != nil {
		return nil, err
	} else if th == nil || th.Kind != "issue" {
		return nil, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	events, err := s.scanEvents(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if !reactionState(events)[who+"\x00"+itoa(target)+"\x00"+c] {
		return nil, fmt.Errorf("%w: unknown reaction", ErrNotFound)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	th, _, err := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
		t.NextEventSeq = seq + 1
		t.UpdatedAt = now
		t.Version++
		key := seqKey(target)
		if m := t.ReactionSummary[key]; m != nil {
			if m[c] > 1 {
				m[c]--
			} else {
				delete(m, c)
			}
			if len(m) == 0 {
				delete(t.ReactionSummary, key)
			}
		}
		return &Event{Seq: seq, Type: EventReactionChanged, Actor: who, At: now,
			TargetEventSeq: &target, Content: strPtr(c), Op: strPtr("remove")}, nil
	})
	if err != nil {
		return nil, err
	}
	s.updateIndex(ctx, owner, repo, cardOf(th))
	s.stream(ctx, StreamEvent{Name: "issue_event", Repo: repoName(owner, repo), IssueNum: num})
	return th, nil
}

// --- closing keywords (§5 seam) ------------------------------------------------

// ApplyClosingReferences is the seam 03's merge task calls at PR MERGE
// time (never at push): it parses texts (PR body + merged commit
// messages) with the §5 grammar and, per matched num, writes a
// referenced event (source thread prNum) and a state_changed close
// (reason completed) through the normal P3 two-step. Already-closed
// issues are skipped. Returns the closed nums, sorted.
func (s *Service) ApplyClosingReferences(ctx context.Context, owner, repo string, prNum int, mergedSHA, actor string, texts []string) ([]int, error) {
	matches := ParseClosingRefs(texts...)
	var closed []int
	for _, m := range matches {
		th, _, err := s.loadThread(ctx, owner, repo, m.Num)
		if err != nil || th == nil || th.Kind != "issue" || th.State == StateClosed {
			continue
		}
		who := normPrincipal(actor)
		if who == "" {
			who = "merge-queue"
		}
		now := s.nowUTC().Format(dateTimeFmt)
		src := map[string]any{"repo": repoName(owner, repo), "kind": "thread", "num": prNum}
		if _, _, aerr := s.appendEvent(ctx, owner, repo, m.Num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Version++
			return &Event{Seq: seq, Type: EventReferenced, Actor: who, At: now, Source: src}, nil
		}); aerr != nil {
			continue
		}
		kw := m.Keyword
		sha := mergedSHA
		nt, _, cerr := s.appendEvent(ctx, owner, repo, m.Num, func(t *Thread, seq int) (*Event, error) {
			if t.State == StateClosed {
				return nil, nil
			}
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.State = StateClosed
			r := ReasonCompleted
			t.StateReason = &r
			t.Version++
			return &Event{Seq: seq, Type: EventClosedByPR, Actor: who, At: now, PRNum: &prNum, Keyword: &kw}, nil
		})
		if cerr != nil || nt == nil {
			continue
		}
		_ = sha
		s.updateIndex(ctx, owner, repo, cardOf(nt))
		if nt.Milestone != nil {
			s.bumpMilestone(ctx, owner, repo, *nt.Milestone, -1, +1)
		}
		s.emitSubscribed(ctx, owner, repo, nt, who)
		s.stream(ctx, StreamEvent{Name: "issue", Repo: repoName(owner, repo), IssueNum: m.Num})
		closed = append(closed, m.Num)
	}
	sort.Ints(closed)
	if closed == nil {
		closed = []int{}
	}
	return closed, nil
}

// --- read paths ----------------------------------------------------------------

// ListFilter shapes GET …/api/issues (§7): labels comma-list AND,
// assignee login or "*", milestone id or "none", since RFC3339 on
// updated_at, after num cursor, n ≤ 100 default 50.
type ListFilter struct {
	State     string
	Labels    []string
	Assignee  string
	Milestone string
	Since     string
	After     int
	N         int
}

// ListResult is the paged list answer.
type ListResult struct {
	Issues []Card `json:"issues"`
	More   bool   `json:"more"`
}

// ListIssues serves the list index-first (P4): when the CAS'd index is
// provably complete — every number below the P2 counter has a card —
// the requested window is filled from the index alone (2 GETs: index +
// counter, O(1) requests, no LIST). Otherwise (absent index, lost index
// update, crash between the header CAS and the index CAS, compacted
// history) the page falls through to the paginated LIST scan and merges
// union-by-num with the header winning over a stale card — LIST fallback
// makes staleness a performance gap, never a correctness gap. A LIST
// failure degrades to the index window instead of erroring.
func (s *Service) ListIssues(ctx context.Context, owner, repo string, p auth.Principal, f ListFilter) (*ListResult, error) {
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
	next := s.loadCounter(ctx, owner, repo)
	if indexComplete(ix, next) {
		pool := filterCards(append(append([]Card{}, ix.Open...), ix.ClosedRecent...), f)
		sortCards(pool)
		page, more := windowCards(pool, f.After, n)
		return &ListResult{Issues: page, More: more}, nil
	}
	// Fall through to the LIST scan and merge (dedup by num; a scanned
	// header replaces its stale card).
	scanned, lerr := s.scanHeaders(ctx, owner, repo)
	pool := append(append([]Card{}, ix.Open...), ix.ClosedRecent...)
	if lerr != nil {
		// LIST failure degrades to the index window, not an error.
		pool = filterCards(pool, f)
		sortCards(pool)
		page, _ := windowCards(pool, f.After, n)
		return &ListResult{Issues: page, More: false}, nil
	}
	byNum := map[int]Card{}
	for _, c := range pool {
		byNum[c.Num] = c
	}
	for _, c := range scanned {
		byNum[c.Num] = c
	}
	merged := make([]Card, 0, len(byNum))
	for _, c := range byNum {
		merged = append(merged, c)
	}
	merged = filterCards(merged, f)
	sortCards(merged)
	page, more := windowCards(merged, f.After, n)
	return &ListResult{Issues: page, More: more}, nil
}

// loadCounter reads the P2 shared counter (meta/next_num); 0 when absent
// or unreadable — both mean "not provably complete", the safe direction
// (the LIST fallback covers the read).
func (s *Service) loadCounter(ctx context.Context, owner, repo string) int {
	raw, _, err := s.getJSON(ctx, CounterKey(owner, repo))
	if err != nil || raw == nil {
		return 0
	}
	var c Counter
	if jerr := json.Unmarshal(raw, &c); jerr != nil || c.Next < 1 {
		return 0
	}
	return c.Next
}

// indexComplete reports whether every allocated number below next has a
// card in the index (either page, either kind — 03 shares the numbering
// space and cards pr threads here). next <= 0 (no counter yet) is never
// complete: pre-issues repos read through the LIST scan.
func indexComplete(ix *Index, next int) bool {
	if next <= 0 {
		return false
	}
	have := make(map[int]bool, len(ix.Open)+len(ix.ClosedRecent))
	for _, c := range ix.Open {
		have[c.Num] = true
	}
	for _, c := range ix.ClosedRecent {
		have[c.Num] = true
	}
	for num := 1; num < next; num++ {
		if !have[num] {
			return false
		}
	}
	return true
}

// scanHeaders scans …/<num>/thread.json headers (P5 page: at most
// scanCap headers per call, numeric order). Only kind:"issue" threads are
// returned. Callers merge with the index (header wins) and window by
// cursor — the scan always starts at the top so the fallback is complete,
// never cursor-truncated.
func (s *Service) scanHeaders(ctx context.Context, owner, repo string) ([]Card, error) {
	const scanCap = 2000
	prefix := IssuesPrefix(owner, repo)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, "/thread.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]Card, 0, len(keys))
	for _, k := range keys {
		if len(out) >= scanCap {
			break
		}
		raw, _, gerr := s.getJSON(ctx, k)
		if gerr != nil {
			return out, gerr
		}
		if raw == nil {
			continue
		}
		t, perr := parseThread(raw)
		if perr != nil || t.Kind != "issue" {
			continue
		}
		out = append(out, cardOf(t))
	}
	return out, nil
}

// filterCards applies kind/state/labels/assignee/milestone/since
// predicates. Non-issue kinds never match (one index, one numbering
// space — 03 lists PRs from the same object). Labels are AND; assignee
// "*none" selects unassigned threads; milestone "none" selects
// un-milestoned threads; since keeps threads with updated_at strictly
// after.
func filterCards(cards []Card, f ListFilter) []Card {
	out := make([]Card, 0, len(cards))
	for _, c := range cards {
		if c.Kind != "issue" {
			continue
		}
		if f.State != "" && c.State != f.State {
			continue
		}
		ok := true
		for _, want := range f.Labels {
			found := false
			for _, have := range c.Labels {
				if strings.EqualFold(have, want) {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if f.Assignee != "" {
			if f.Assignee == "*none" {
				if len(c.Assignees) != 0 {
					continue
				}
			} else {
				found := false
				for _, a := range c.Assignees {
					if normPrincipal(a) == normPrincipal(f.Assignee) {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
		}
		if f.Milestone != "" {
			if f.Milestone == "none" {
				if c.Milestone != nil {
					continue
				}
			} else if c.Milestone == nil || *c.Milestone != strings.ToLower(f.Milestone) {
				continue
			}
		}
		if f.Since != "" && c.UpdatedAt <= f.Since {
			continue
		}
		out = append(out, c)
	}
	return out
}

// windowCards slices the newest-first pool after the after-num cursor.
// The after cursor is positional in display order: the page starts after
// the card with that num (unknown cursor → from the top).
func windowCards(pool []Card, after, n int) ([]Card, bool) {
	start := 0
	if after > 0 {
		start = len(pool)
		for i, c := range pool {
			if c.Num == after {
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
	page := pool[start:end]
	if page == nil {
		page = []Card{}
	}
	return page, more
}

// ThreadView is GET …/api/issues/{num}: header + last-K events.
type ThreadView struct {
	Thread     *Thread  `json:"thread"`
	Events     []*Event `json:"events"`
	EventsMore bool     `json:"events_more"`
}

// GetThread reads header + last 50 events (events_more when older remain).
// afterSeq windows the log older-on-demand: only seqs < afterSeq (0 =
// newest). ETag material is Thread.Version (the HTTP layer renders
// "v<version>").
func (s *Service) GetThread(ctx context.Context, owner, repo string, num int, p auth.Principal, afterSeq, n int) (*ThreadView, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "issue" {
		return nil, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	events, more, err := s.eventWindow(ctx, owner, repo, num, afterSeq, n)
	if err != nil {
		return nil, err
	}
	return &ThreadView{Thread: th, Events: events, EventsMore: more}, nil
}

// eventWindow returns at most n events newest-first with seq < afterSeq
// (afterSeq ≤ 0 = newest), plus whether older remain. n ≤ 0 defaults 50,
// capped at 200 (P5 page sizes).
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

// --- §10 fan-out ---------------------------------------------------------------

// emitSubscribed fans the subscribed class to participants[] minus the
// actor (opened/commented/state/label changes by a non-participant path
// all converge here; referenced events never subscribe).
func (s *Service) emitSubscribed(ctx context.Context, owner, repo string, th *Thread, actor string) {
	var recips []string
	for _, p := range th.Participants {
		if p != "" && p != actor {
			recips = append(recips, p)
		}
	}
	if len(recips) == 0 {
		return
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "subscribed", Actor: actor, IssueNum: th.Num, Recipients: recips})
}

// emitMentioned fans the mentioned class to @-parsed principals in the
// body (resolvable names only — the Emitter's consumer resolves; here we
// pass through what the parser found, minus the actor).
func (s *Service) emitMentioned(ctx context.Context, owner, repo string, num int, actor, body string) {
	if body == "" {
		return
	}
	var recips []string
	for _, m := range ParseMentions(body) {
		if m != actor && identity.ValidPrincipal(m) {
			recips = append(recips, m)
		}
	}
	if len(recips) == 0 {
		return
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "mentioned", Actor: actor, IssueNum: num, Recipients: recips})
}

// --- label/milestone management --------------------------------------------------

// CreateLabel CAS-appends one label (names unique case-insensitively).
// Auth: triage.
func (s *Service) CreateLabel(ctx context.Context, owner, repo string, actor auth.Principal, name, color, desc string) (*Label, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return nil, err
	}
	n, err := validateLabelName(name)
	if err != nil {
		return nil, err
	}
	c, err := validateColor(color)
	if err != nil {
		return nil, err
	}
	if len(desc) > MaxLabelDesc {
		return nil, fmt.Errorf("%w: description exceeds %d characters", ErrInvalid, MaxLabelDesc)
	}
	var created *Label
	_, err = s.casUpdate(ctx, LabelsKey(owner, repo), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		ls := &LabelSet{Labels: []Label{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, ls); jerr != nil {
				return nil, false, fmt.Errorf("%w: labels.json: %v", ErrCorrupt, jerr)
			}
		}
		if findLabel(ls, n) != nil {
			return nil, false, fmt.Errorf("%w: label %q already exists", ErrConflict, n)
		}
		created = &Label{Name: n, Color: c, Description: desc}
		ls.Labels = append(ls.Labels, *created)
		sort.Slice(ls.Labels, func(i, j int) bool { return ls.Labels[i].Name < ls.Labels[j].Name })
		ls.Version++
		out, _ := json.Marshal(ls)
		return out, true, nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateLabel edits color/description (names immutable, §3.1). Auth: triage.
func (s *Service) UpdateLabel(ctx context.Context, owner, repo string, actor auth.Principal, name string, color *string, desc *string) (*Label, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return nil, err
	}
	var updated *Label
	_, err := s.casUpdate(ctx, LabelsKey(owner, repo), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown label %q", ErrNotFound, name)
		}
		var ls LabelSet
		if jerr := json.Unmarshal(cur, &ls); jerr != nil {
			return nil, false, fmt.Errorf("%w: labels.json: %v", ErrCorrupt, jerr)
		}
		found := findLabel(&ls, name)
		if found == nil {
			return nil, false, fmt.Errorf("%w: unknown label %q", ErrNotFound, name)
		}
		if color != nil {
			c, verr := validateColor(*color)
			if verr != nil {
				return nil, false, verr
			}
			found.Color = c
		}
		if desc != nil {
			if len(*desc) > MaxLabelDesc {
				return nil, false, fmt.Errorf("%w: description exceeds %d characters", ErrInvalid, MaxLabelDesc)
			}
			found.Description = *desc
		}
		cp := *found
		updated = &cp
		ls.Version++
		out, _ := json.Marshal(&ls)
		return out, true, nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteLabel removes a label (rename = delete + create, §3.1): headers
// keep the old name string; a compensating labels_changed event is
// emitted per affected open thread in the index hot window (older threads
// self-heal when next touched — a nonexistent label renders as the bare
// string). Returns the index-count threads_affected (best-effort).
// Auth: triage.
func (s *Service) DeleteLabel(ctx context.Context, owner, repo string, actor auth.Principal, name string) (int, error) {
	if err := requireAuthenticated(actor); err != nil {
		return 0, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return 0, err
	}
	_, err := s.casUpdate(ctx, LabelsKey(owner, repo), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown label %q", ErrNotFound, name)
		}
		var ls LabelSet
		if jerr := json.Unmarshal(cur, &ls); jerr != nil {
			return nil, false, fmt.Errorf("%w: labels.json: %v", ErrCorrupt, jerr)
		}
		kept := ls.Labels[:0]
		found := false
		for _, l := range ls.Labels {
			if strings.EqualFold(l.Name, name) {
				found = true
				continue
			}
			kept = append(kept, l)
		}
		if !found {
			return nil, false, fmt.Errorf("%w: unknown label %q", ErrNotFound, name)
		}
		ls.Labels = kept
		if ls.Labels == nil {
			ls.Labels = []Label{}
		}
		ls.Version++
		out, _ := json.Marshal(&ls)
		return out, true, nil
	})
	if err != nil {
		return 0, err
	}
	// Compensating events over the index hot window.
	ix, _, lerr := s.loadIndex(ctx, owner, repo)
	if lerr != nil {
		return 0, nil
	}
	affected := 0
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	for _, card := range ix.Open {
		has := false
		for _, l := range card.Labels {
			if strings.EqualFold(l, name) {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		nt, _, aerr := s.appendEvent(ctx, owner, repo, card.Num, func(t *Thread, seq int) (*Event, error) {
			kept := make([]string, 0, len(t.Labels))
			var removed []string
			for _, l := range t.Labels {
				if strings.EqualFold(l, name) {
					removed = append(removed, l)
					continue
				}
				kept = append(kept, l)
			}
			if len(removed) == 0 {
				return nil, nil
			}
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Labels = kept
			t.Version++
			return &Event{Seq: seq, Type: EventLabelsChanged, Actor: who, At: now, Added: []string{}, Removed: removed}, nil
		})
		if aerr != nil || nt == nil {
			continue
		}
		affected++
		s.updateIndex(ctx, owner, repo, cardOf(nt))
	}
	return affected, nil
}

// CreateMilestone allocates an id and Creates the milestone object.
// Auth: triage.
func (s *Service) CreateMilestone(ctx context.Context, owner, repo string, actor auth.Principal, title, desc string, dueOn *string) (*Milestone, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return nil, err
	}
	t, err := validateMilestoneTitle(title)
	if err != nil {
		return nil, err
	}
	if dueOn != nil && *dueOn != "" {
		if _, perr := time.Parse(time.RFC3339, *dueOn); perr != nil {
			return nil, fmt.Errorf("%w: due_on must be RFC 3339", ErrInvalid)
		}
	}
	id, err := s.allocMilestoneID(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	m := &Milestone{ID: id, Title: t, Description: desc, DueOn: dueOn, State: StateOpen,
		CreatedBy: normPrincipal(actor.Name), CreatedAt: now, UpdatedAt: now}
	raw, _ := json.Marshal(m)
	if err := s.putCreate(ctx, MilestoneKey(owner, repo, id), raw); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, fmt.Errorf("%w: milestone %s already exists", ErrConflict, id)
		}
		return nil, err
	}
	m.Percent = 0
	return m, nil
}

// UpdateMilestone edits title/description/due_on/state. Closing is
// metadata-only; issues keep their own state (§3.2). Auth: triage.
func (s *Service) UpdateMilestone(ctx context.Context, owner, repo string, actor auth.Principal, id string, title, desc, dueOn, state *string) (*Milestone, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return nil, err
	}
	m, ver, err := s.loadMilestone(ctx, owner, repo, strings.ToLower(id))
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("%w: unknown milestone", ErrNotFound)
	}
	if title != nil {
		t, verr := validateMilestoneTitle(*title)
		if verr != nil {
			return nil, verr
		}
		m.Title = t
	}
	if desc != nil {
		m.Description = *desc
	}
	if dueOn != nil {
		if *dueOn == "" {
			m.DueOn = nil
		} else {
			if _, perr := time.Parse(time.RFC3339, *dueOn); perr != nil {
				return nil, fmt.Errorf("%w: due_on must be RFC 3339", ErrInvalid)
			}
			m.DueOn = dueOn
		}
	}
	if state != nil {
		st := strings.ToLower(strings.TrimSpace(*state))
		if st != StateOpen && st != StateClosed {
			return nil, fmt.Errorf("%w: state must be open|closed", ErrInvalid)
		}
		m.State = st
	}
	if err := s.saveMilestone(ctx, owner, repo, m, ver); err != nil {
		return nil, err
	}
	m.Percent = milestonePercent(m.OpenIssues, m.ClosedIssues)
	return m, nil
}

// DeleteMilestone requires open_issues == 0 (else 409) and clears the
// milestone from affected open threads via compensating
// milestone_changed events. Auth: triage.
func (s *Service) DeleteMilestone(ctx context.Context, owner, repo string, actor auth.Principal, id string) error {
	if err := requireAuthenticated(actor); err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return err
	}
	id = strings.ToLower(id)
	m, ver, err := s.loadMilestone(ctx, owner, repo, id)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: unknown milestone", ErrNotFound)
	}
	if m.OpenIssues > 0 {
		return fmt.Errorf("%w: milestone has open issues", ErrConflict)
	}
	if derr := s.Store.Delete(ctx, MilestoneKey(owner, repo, id), ver); derr != nil {
		if store.IsPreconditionFailed(derr) {
			return fmt.Errorf("%w: milestone changed concurrently", ErrConflict)
		}
		return derr
	}
	// Compensating clears over every thread referencing the id. The index
	// hot window covers the common case; the LIST scan covers compacted
	// history (delete is rare — correctness over round trips here).
	// Closed threads are cleared too (a superset of §3.2's "open
	// threads": a dangling milestone id on a closed header helps
	// nothing and the clear is one CAS per thread).
	affected := map[int]bool{}
	ix, _, _ := s.loadIndex(ctx, owner, repo)
	for _, card := range ix.Open {
		if card.Milestone != nil && *card.Milestone == id {
			affected[card.Num] = true
		}
	}
	for _, card := range ix.ClosedRecent {
		if card.Milestone != nil && *card.Milestone == id {
			affected[card.Num] = true
		}
	}
	scanned, _ := s.scanHeaders(ctx, owner, repo)
	for _, card := range scanned {
		if card.Milestone != nil && *card.Milestone == id {
			affected[card.Num] = true
		}
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	for num := range affected {
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
			if t.Milestone == nil || *t.Milestone != id {
				return nil, nil
			}
			from := *t.Milestone
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Milestone = nil
			t.Version++
			return &Event{Seq: seq, Type: EventMilestoneChanged, Actor: who, At: now, From: strPtr(from), To: nil}, nil
		})
		if aerr != nil || nt == nil {
			continue
		}
		s.updateIndex(ctx, owner, repo, cardOf(nt))
	}
	return nil
}

// --- diff helpers ------------------------------------------------------------

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalPtrReason(cur *string, want *string) bool {
	if want == nil {
		return true
	}
	return equalPtr(cur, want)
}

// diffStr returns (added, removed) to turn old into new; both []-not-null.
func diffStr(old, new []string) ([]string, []string) {
	have := map[string]bool{}
	for _, v := range old {
		have[v] = true
	}
	want := map[string]bool{}
	for _, v := range new {
		want[v] = true
	}
	var added, removed []string
	for _, v := range new {
		if !have[v] {
			added = append(added, v)
		}
	}
	for _, v := range old {
		if !want[v] {
			removed = append(removed, v)
		}
	}
	if added == nil {
		added = []string{}
	}
	if removed == nil {
		removed = []string{}
	}
	return added, removed
}
