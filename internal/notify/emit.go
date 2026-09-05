// emit.go — the 06 §4 emission contract (P8: synchronous in the
// mutating handler, after the CAS commits).
//
// Source packages (issues/pulls/review/checks) own their NotifyEvent
// shapes and recipient pre-resolution (participants, assignees, review
// requests, parsed @-names, team spellings); composition adapts each
// shape onto Emission and calls the matching Emit method. notify then
// performs, in order:
//
//  0. resolve: validate mentions (profile probe), expand teams, union
//     watchers for subscribed-mapped classes, add the thread author —
//     minus the actor, deduped.
//  1. reserve the activity seq (collab_state CAS). The id embeds this
//     seq, so the reservation precedes the Creates; a crash between the
//     reservation and the activity Create leaves a gap, which is allowed
//     (honest-gap semantics, §5.3).
//  2. Create users/<p>/notifications/<id>.json (deterministic id; 412 =
//     already emitted, success). Overflow (> MaxSyncRecipients) writes
//     the activity event first and defers to the notify-fanout task.
//  3. CAS each user's notifications/index.json (retry loop, own version).
//  4. Create the immutable activity event (webhook unit + backfill).
//  5. Publish one `notification` frame per recipient, non-blocking.
//
// Order 2→3→4→5 is normative: a crash leaves at most a missing tray
// entry, never a phantom index entry. A crash before step 2 loses one
// notification — the thread timeline is the backfill source (P8).
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"git.packden.us/crueber/walhub/internal/store"
)

// Emission is one source mutation translated for fan-out. Composition
// (cmd/walhub/notify.go) builds these from the per-package NotifyEvents
// — notify never imports the feature packages (09 §2), and the feature
// packages never import notify.
type Emission struct {
	Repo       string         // "owner/name"
	Num        int            // shared issue/PR number; 0 for repo-level
	Kind       string         // "issue"|"pull"|"release"|"repo"
	Class      string         // source class (assigned, mentioned, subscribed, opened, …)
	Action     string         // activity action override ("" = derive from Class)
	Actor      string         // causing principal; "" for system
	At         string         // RFC 3339; "" = now
	Title      string         // "" = read thread.json
	Recipients []string       // primary recipients; "org/team" spellings expanded
	Detail     map[string]any // merged into the activity payload (checks fields, …)
}

// target is one resolved (principal, reason) delivery.
type target struct {
	principal string
	reason    string
}

// --- Emit adapters (one per source package) ----------------------------------

// EmitIssue fans out an issues.NotifyEvent (02 §10: classes assigned,
// mentioned, subscribed; Action carries the true activity action).
func (s *Service) EmitIssue(ctx context.Context, repo string, num int, class, actor, at, action string, recipients []string) {
	s.emit(ctx, Emission{Repo: repo, Num: num, Kind: "issue", Class: class, Action: action, Actor: actor, At: at, Recipients: recipients})
}

// EmitPull fans out a pulls.NotifyEvent (03 §8).
func (s *Service) EmitPull(ctx context.Context, repo string, num int, class, actor, at string, recipients []string) {
	kind := "pull"
	if num == 0 {
		kind = "repo"
	}
	s.emit(ctx, Emission{Repo: repo, Num: num, Kind: kind, Class: class, Actor: actor, At: at, Recipients: recipients})
}

// EmitReview fans out a review.NotifyEvent (04 §7).
func (s *Service) EmitReview(ctx context.Context, repo string, num int, class, actor, at string, recipients []string) {
	s.emit(ctx, Emission{Repo: repo, Num: num, Kind: "pull", Class: class, Actor: actor, At: at, Recipients: recipients})
}

// EmitCheck fans out a checks.NotifyEvent (05 §8: failure/error
// transitions on an open PR head; participants resolve from thread.json).
func (s *Service) EmitCheck(ctx context.Context, repo, sha, checkCtx, state, desc, targetURL, actor, at string, pr int) {
	s.emit(ctx, Emission{
		Repo: repo, Num: pr, Kind: "pull", Class: "check_reported", Actor: actor, At: at,
		Detail: map[string]any{"sha": sha, "context": checkCtx, "state": state, "description": desc, "target_url": targetURL, "pr": pr},
	})
}

// EmitRelease fans out a release publish (07 §3; 07 absent — exported for
// the releases wave; watchers resolve from social.json like any
// subscribed-mapped class).
func (s *Service) EmitRelease(ctx context.Context, repo, tag, actor, at string) {
	s.emit(ctx, Emission{Repo: repo, Kind: "release", Class: "release_published", Actor: actor, At: at, Title: tag})
}

// --- class → reason/action ----------------------------------------------------

// classReason maps a source class onto the §1.1 reason for its primary
// recipients. Everything not explicitly addressed is participation.
func classReason(class string) string {
	switch class {
	case "assigned":
		return ReasonAssigned
	case "mentioned":
		return ReasonMentioned
	case "review_requested":
		return ReasonReviewRequested
	default:
		return ReasonSubscribed
	}
}

// subscribedClass reports whether the class maps onto subscribed (and
// hence unions repo watchers + the thread author per §2).
func subscribedClass(class string) bool { return classReason(class) == ReasonSubscribed }

// classAction maps a source class onto the §5.3 activity action. Issues'
// coarse "subscribed" class is disambiguated by the Action override the
// issues package sets at its emit sites; pulls/review/checks classes are
// already precise.
func classAction(class, override string) string {
	if override != "" {
		return override
	}
	switch class {
	case "opened", "forked":
		return ActionOpened
	case "closed", "merged":
		return ActionClosed
	case "reopened":
		return ActionReopened
	case "assigned":
		return ActionAssigned
	case "mentioned":
		return ActionMentioned
	case "review_requested", "review_request_removed":
		return ActionReviewRequested
	case "review_submitted", "review_dismissed":
		return ActionReviewPosted
	case "check_reported":
		return ActionCheckReported
	case "release_published":
		return ActionReleasePublished
	default:
		return ActionCommented
	}
}

// --- emit ---------------------------------------------------------------------

// threadHead is the minimal thread.json projection notify reads (title,
// author, participants). 06 reads thread.json alone, never event scans (§2).
type threadHead struct {
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	Participants []string `json:"participants"`
}

// threadKey renders the shared P3 header key (02/03 family).
func threadKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/thread.json", owner, repo, num)
}

// emit performs the §4 sequence for one emission.
func (s *Service) emit(ctx context.Context, e Emission) {
	owner, name, ok := splitRepo(e.Repo)
	if !ok {
		return // malformed repo — nothing to fan out to (P8: data already committed)
	}
	actor := normPrincipal(e.Actor)
	at := e.At
	if at == "" {
		at = s.nowUTC().Format(dateTimeFmt)
	}
	title, author := e.Title, ""
	var head *threadHead
	if e.Num > 0 {
		head = s.readThreadHead(ctx, owner, name, e.Num)
		if head != nil {
			if title == "" {
				title = head.Title
			}
			author = normPrincipal(head.Author)
		}
	}
	targets := s.resolve(ctx, owner, name, e, actor, author, head)
	action := classAction(e.Class, e.Action)

	// Reserve the activity seq first: the notification ids embed it.
	seq, err := s.reserveSeq(ctx, owner, name)
	if err != nil {
		return // CAS exhaustion — timeline stays the backfill truth (P8)
	}

	if len(targets) > MaxSyncRecipients {
		// Overflow: the activity event (with the full recipient set
		// in its payload) is the durable queue; notify-fanout drains
		// it. The request never extends past this point (§4). The
		// pending flag arms the restart redrain sweep (issue #77).
		_ = s.appendActivity(ctx, owner, name, seq, e, action, title, actor, at, targets, true)
		s.enqueueFanout(e.Repo, seq)
		s.wakeRepo(e.Repo)
		return
	}

	fctx, cancel := context.WithTimeout(ctx, FanoutBudget)
	defer cancel()
	done, failed := s.createAll(fctx, owner, name, e, title, actor, at, seq, targets)
	if failed {
		// Shortfall (budget or errors): the activity event must still
		// land (webhooks + backfill), then the task completes the
		// remainder idempotently. Dedup-skips are NOT failures — the
		// live entry already covers them, so they never arm the task
		// (a task racing a later read-flip could otherwise mint a
		// duplicate live entry for the same thread+reason). The pending
		// flag arms the restart redrain sweep (issue #77).
		_ = s.appendActivity(ctx, owner, name, seq, e, action, title, actor, at, targets, true)
		s.enqueueFanout(e.Repo, seq)
		s.wakeRepo(e.Repo)
		// Publish what did complete — a partial tray beats silence.
		for _, d := range done {
			s.ubus.publish(d.principal, d.notif)
		}
		return
	}
	_ = s.appendActivity(ctx, owner, name, seq, e, action, title, actor, at, targets, false)
	for _, d := range done {
		s.ubus.publish(d.principal, d.notif)
	}
	s.wakeRepo(e.Repo)
}

// resolve computes the (principal, reason) set: validated primary
// recipients ∪ team expansions ∪ watchers (subscribed classes) ∪ thread
// author — minus the actor, deduped by (principal, reason).
func (s *Service) resolve(ctx context.Context, owner, repo string, e Emission, actor, author string, head *threadHead) []target {
	reason := classReason(e.Class)
	var mu sync.Mutex
	out := []target{}
	seen := map[string]bool{}
	add := func(principal, r string) {
		p := normPrincipal(principal)
		if p == "" || p == actor {
			return
		}
		k := p + "\x00" + r
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, target{principal: p, reason: r})
	}
	// Primary recipients, bounded parallel probes (cap 8).
	sem := make(chan struct{}, FanoutParallel)
	var wg sync.WaitGroup
	for _, r := range e.Recipients {
		r := r
		if strings.Contains(r, "/") {
			addTeam(s, ctx, owner, repo, r, e.Class, actor, author, add, &mu)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p := normPrincipal(r)
			if p == "" || p == actor {
				return
			}
			// The thread author receives the author reason (below),
			// not a duplicate subscribed entry.
			if subscribedClass(e.Class) && p == author {
				return
			}
			if e.Class == "mentioned" && !s.validPrincipal(ctx, p) {
				return // silent-ignore per §3 (never a 400)
			}
			mu.Lock()
			add(p, reason)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if subscribedClass(e.Class) {
		// Checks carry no recipient list (05 §8): participants resolve
		// from thread.json here — the one read 06 performs (§2).
		if e.Class == "check_reported" && head != nil {
			for _, p := range head.Participants {
				if p == author {
					continue // author reason below, not subscribed
				}
				add(p, ReasonSubscribed)
			}
		}
		// Repo watchers (§2) + thread author join subscribed classes.
		for _, w := range s.watchers(ctx, owner, repo) {
			if w == author {
				continue // author gets the author reason below, not subscribed
			}
			add(w, ReasonSubscribed)
		}
		if author != "" && author != actor {
			add(author, ReasonAuthor)
		}
	}
	return out
}

// addTeam expands one "org/team" spelling to its members (reason
// team_mention; sorted, capped at MaxTeamFanout). Unknown teams and
// teams without a reader are silently ignored (§3).
func addTeam(s *Service, ctx context.Context, owner, repo, spelling, class, actor, author string, add func(string, string), mu *sync.Mutex) {
	parts := strings.SplitN(normPrincipal(spelling), "/", 2)
	if len(parts) != 2 || s.Teams == nil {
		return
	}
	t, _, err := s.Teams.GetTeam(ctx, parts[0], parts[1])
	if err != nil || t == nil {
		return
	}
	members := append([]string(nil), t.Members...)
	sortStrings(members)
	if len(members) > MaxTeamFanout {
		members = members[:MaxTeamFanout]
	}
	for _, m := range members {
		p := normPrincipal(m)
		if p == "" || p == actor {
			continue
		}
		if subscribedClass(class) && p == author {
			continue // author reason below, not a duplicate
		}
		if !s.validPrincipal(ctx, p) {
			continue
		}
		mu.Lock()
		add(p, ReasonTeamMention)
		mu.Unlock()
	}
}

// validPrincipal probes users/<principal>/profile.json (06 §3: 404 =
// invalid; any other store error keeps the recipient — fail open toward
// notify, the Create is idempotent anyway). Nil prober drops everyone:
// without the identity surface there is nothing valid to say.
func (s *Service) validPrincipal(ctx context.Context, p string) bool {
	if s.Profiles == nil {
		return false
	}
	prof, err := s.Profiles.GetProfile(ctx, p)
	if err != nil {
		return true
	}
	return prof != nil
}

// watchers returns social.json watcher_list (06 §2 source; 07 owns the
// record, 06 only reads). Absent/unreadable → no watchers.
func (s *Service) watchers(ctx context.Context, owner, repo string) []string {
	raw, _, err := s.getJSON(ctx, SocialKey(owner, repo))
	if err != nil || raw == nil {
		return nil
	}
	var soc SocialDoc
	if err := json.Unmarshal(raw, &soc); err != nil {
		return nil
	}
	return append([]string(nil), soc.WatcherList...)
}

// readThreadHead reads the title/author projection (one GET, §2).
func (s *Service) readThreadHead(ctx context.Context, owner, repo string, num int) *threadHead {
	raw, _, err := s.getJSON(ctx, threadKey(owner, repo, num))
	if err != nil || raw == nil {
		return nil
	}
	var th threadHead
	if err := json.Unmarshal(raw, &th); err != nil {
		return nil
	}
	return &th
}

// --- phases 2+3: creates + index CAS -------------------------------------------

type completed struct {
	principal string
	notif     Notification
}

// createAll runs phases 2 (Creates) and 3 (index CAS) for every target
// under the fan-out budget, returning the created deliveries and whether
// any target FAILED (dedup-skips are complete, not failures).
func (s *Service) createAll(ctx context.Context, owner, repo string, e Emission, title, actor, at string, seq int, targets []target) ([]completed, bool) {
	sem := make(chan struct{}, FanoutParallel)
	var mu sync.Mutex
	done := []completed{}
	failed := false
	var wg sync.WaitGroup
	for _, t := range targets {
		t := t
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				mu.Lock()
				failed = true
				mu.Unlock()
				return
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				failed = true
				mu.Unlock()
				return
			}
			n, status := s.createOne(ctx, e, title, actor, at, seq, t)
			mu.Lock()
			defer mu.Unlock()
			switch status {
			case createFailed:
				failed = true
			case createCreated:
				done = append(done, completed{principal: t.principal, notif: n})
			case createSkipped:
				// Live entry already covers it — complete, no frame.
			}
		}()
	}
	wg.Wait()
	return done, failed
}

// createStatus is the per-target outcome.
type createStatus int

const (
	createCreated createStatus = iota // notification written (or 412-replayed)
	createSkipped                     // unread dedup: live entry covers it
	createFailed                      // budget/store shortfall: task backfills
)

// errDedupLive aborts an index CAS write from inside the loop: a live
// (unread, same-triple) entry committed first, so this emission's row must
// not land. It never escapes indexClaim — it maps to (live=false, nil).
var errDedupLive = errors.New("notify: live entry covers (user, thread, reason)")

// createOne Creates one notification (deduped) and CASes its index entry.
// Idempotent: a retried fan-out re-derives the id and 412s into success,
// and the index CAS skips present ids.
//
// The §2 "one live notification per (user, thread, reason)" dedup is
// arbitrated by the unread-index CAS, not by the hasUnread pre-check: the
// pre-check is a fast path only (it saves a Create in the sequential case),
// while indexClaim re-checks the triple INSIDE the CAS loop — two emissions
// racing past the pre-check converge there because their ids differ (each
// embeds its own reserved seq). A loser deletes its just-Created orphan
// object (best-effort) and reports createSkipped, so the tray converges to
// one row AND one object. No locks: CAS loops are the only tool (13 §2).
func (s *Service) createOne(ctx context.Context, e Emission, title, actor, at string, seq int, t target) (Notification, createStatus) {
	id := NotificationID(t.principal, e.Repo, e.Num, t.reason, seq)
	if s.hasUnread(ctx, t.principal, e.Repo, e.Num, t.reason) {
		// One live notification per (user, thread, reason) (§2 dedup).
		return Notification{}, createSkipped
	}
	n := Notification{
		ID: id, Repo: e.Repo, Num: e.Num, Kind: e.Kind,
		Reason: t.reason, Title: title, Actor: actor,
		State: StateUnread, CreatedAt: at,
	}
	fresh := true
	if err := s.putCreate(ctx, NotifKey(t.principal, id), encode(n)); err != nil {
		if !store.IsPreconditionFailed(err) {
			return Notification{}, createFailed
		}
		// 412 = already emitted (retry/backfill) — success, and no
		// orphan exists to clean up below.
		fresh = false
	}
	live, err := s.indexClaim(ctx, t.principal, IndexEntry{
		ID: id, Repo: e.Repo, Num: e.Num, Kind: e.Kind,
		Reason: t.reason, Title: title, State: StateUnread, At: at,
	})
	if err != nil {
		return Notification{}, createFailed
	}
	if !live {
		// Lost the dedup race inside the index CAS: a live entry for
		// the same triple committed first. Our object (fresh only —
		// a 412 created nothing) is an orphan no index row will ever
		// reference: remove it so LIST overflow never surfaces a
		// phantom second tray entry.
		if fresh {
			_ = s.Store.Delete(ctx, NotifKey(t.principal, id), "")
		}
		return Notification{}, createSkipped
	}
	return n, createCreated
}

// hasUnread reports whether the user's index holds an unread entry for
// the same (repo, num, reason). Index-absent → false.
func (s *Service) hasUnread(ctx context.Context, principal, repo string, num int, reason string) bool {
	raw, _, err := s.getJSON(ctx, NotifIndexKey(principal))
	if err != nil || raw == nil {
		return false
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		return false
	}
	for _, en := range ix.Entries {
		if en.State == StateUnread && en.Repo == repo && en.Num == num && en.Reason == reason {
			return true
		}
	}
	return false
}

// indexAdd CAS-adds one entry (id-deduped inside the loop: a present id
// is never duplicated — a state change updates in place with a count
// delta) and maintains unread_count. Bounded attempts; exhaustion fails
// the target (the notify-fanout task repairs via the activity backfill).
// indexAdd carries id-only semantics: the (user, thread, reason) unread
// dedup lives in indexClaim (the emission path), never here — direct
// index writers (retention, tests) must not inherit emission arbitration.
func (s *Service) indexAdd(ctx context.Context, principal string, en IndexEntry) error {
	_, err := s.indexUpsert(ctx, principal, en, false)
	return err
}

// indexClaim is indexAdd plus the §2 unread-dedup re-check INSIDE the CAS
// loop: if an unread entry for the same (repo, num, reason) with a
// different id is present, the write aborts and live=false reports that a
// live entry already covers the triple. Same bounded attempts; exhaustion
// is an error (the notify-fanout task repairs via the activity backfill).
func (s *Service) indexClaim(ctx context.Context, principal string, en IndexEntry) (live bool, err error) {
	return s.indexUpsert(ctx, principal, en, true)
}

// indexUpsert is the shared CAS body. It reports live=false ONLY on the
// triple-dedup abort (dedupTriple and a live same-triple row won); every
// other path — inserted, id already present, id state-flipped — leaves the
// entry live in the index and reports live=true.
func (s *Service) indexUpsert(ctx context.Context, principal string, en IndexEntry, dedupTriple bool) (bool, error) {
	_, err := s.casUpdate(ctx, NotifIndexKey(principal), 8, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var ix IndexDoc
		if cur != nil {
			if err := json.Unmarshal(cur, &ix); err != nil {
				return nil, false, fmt.Errorf("%w: notifications index: %v", ErrInvalid, err)
			}
		}
		changed := false
		found := false
		for i, have := range ix.Entries {
			if have.ID != en.ID {
				continue
			}
			found = true
			if have.State != en.State {
				ix.Entries[i].State = en.State
				if en.State == StateUnread {
					ix.UnreadCount++
				} else if ix.UnreadCount > 0 {
					ix.UnreadCount--
				}
				changed = true
			}
			break
		}
		if !found {
			if dedupTriple {
				for _, have := range ix.Entries {
					if have.ID != en.ID && have.State == StateUnread &&
						have.Repo == en.Repo && have.Num == en.Num && have.Reason == en.Reason {
						return nil, false, errDedupLive
					}
				}
			}
			ix.Version = 1
			ix.Entries = append([]IndexEntry{en}, ix.Entries...)
			if len(ix.Entries) > TrayPageSize {
				ix.Entries = ix.Entries[:TrayPageSize]
			}
			if en.State == StateUnread {
				ix.UnreadCount++
			}
			changed = true
		}
		if !changed {
			return nil, false, nil
		}
		return encode(ix), true, nil
	})
	if errors.Is(err, errDedupLive) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// splitRepo splits "owner/name".
func splitRepo(repo string) (string, string, bool) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
