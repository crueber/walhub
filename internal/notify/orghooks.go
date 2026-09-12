// orghooks.go — org-scoped webhooks (Forgejo #363): one hook config
// fans out across every repo owned by the org plus the org's own
// membership/team/invite event log.
//
// An org hook is the same wire shape as a repo Hook (HookSpec in/out,
// HMAC keepers, deliveries ring) stored under orgs/<org>/webhooks/:
// one org-log cursor per hook plus one per-(hook, member repo) cursor
// for the member repos' collab-activity logs. Delivery reuses postEvent
// verbatim (same headers, same 10 s lane, same HMAC), so an org-hook
// POST is byte-indistinguishable from a repo-hook POST except for the
// org-action events, which carry Kind "org" and the org name in Repo.
//
// Two logs feed one hook:
//
//	orgs/<org>/orgevents/<seq:012x>.json      immutable org events
//	                                            (member/team/invite changes,
//	                                            Create-only, P3 two-step on
//	                                            meta/orghook_state.json)
//	repos/<org>/<repo>/collab-events/…         each member repo's activity
//	                                            log, read from a per-hook
//	                                            cursor (never written)
//
// The org log is appended by EmitOrgEvent, which identity mutations
// invoke through the OrgEvent observer after their CAS commits (P8 —
// the members/teams docs stay the backfill truth). Git pushes and repo
// creation are deliberately OUT of scope: pushes stay on the events
// bridge TOML sinks (Seam 4, git-only by law), and a new repo surfaces
// through its first activity event, not a synthetic record.
//
// Cost: enumeration (eachRepo filtered to the org) and the per-member
// collab_state probes run only on the explicit wake after EmitOrgEvent
// and on the minute org sweep — never on a git hot path (law 4/6).
// Caps mirror the repo surface: maxHooks() per org, 256 events per
// scope per pass, FanoutParallel hooks in flight.
//
// ### Concurrency
//
// Hazard: concurrent DeliverOrg passes (wake + sweep) racing on one
// hook's cursors, and the sweep racing member-repo emissions. Avoidance:
// cursors are CAS-advanced monotonically (a lost CAS redelivers, never
// skips — the same at-least-once contract as repo hooks); the sweep
// gate watermarks (orgHookSeen/orgRepoSeen/orgHookPending) are guarded
// by hookMu and never held across a store or network call. Hooks run in
// parallel under the pre-acquired FanoutParallel semaphore (issue #153);
// repos inside one hook run sequentially so one hook's burst stays
// one-goroutine wide.
package notify

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Org-level webhook actions: membership, team, and invite transitions
// (Forgejo #363) plus org-object lifecycle transitions (Forgejo #364:
// org profile/org lifecycle/team edits/repo births, emitted into the
// same org log so the activity surface and the webhooks share one
// backfill truth). Repo collab actions (activityActions) are
// additionally valid org-hook filters — they select the member repos'
// activity events.
const (
	OrgActionMemberAdded       = "member_added"
	OrgActionMemberRemoved     = "member_removed"
	OrgActionMemberRoleChanged = "member_role_changed"
	OrgActionTeamCreated       = "team_created"
	OrgActionTeamUpdated       = "team_updated"
	OrgActionTeamDeleted       = "team_deleted"
	OrgActionTeamMemberAdded   = "team_member_added"
	OrgActionTeamMemberRemoved = "team_member_removed"
	OrgActionInviteCreated     = "invite_created"
	OrgActionInviteAccepted    = "invite_accepted"
	OrgActionInviteCancelled   = "invite_cancelled"
	OrgActionOrgCreated        = "org_created"
	OrgActionOrgUpdated        = "org_updated"
	OrgActionOrgDeleted        = "org_deleted"
	OrgActionRepoCreated       = "repo_created"
)

// orgActions is the org-log action enum (repo actions ride alongside).
var orgActions = map[string]bool{
	OrgActionMemberAdded: true, OrgActionMemberRemoved: true,
	OrgActionMemberRoleChanged: true,
	OrgActionTeamCreated:       true, OrgActionTeamUpdated: true,
	OrgActionTeamDeleted:     true,
	OrgActionTeamMemberAdded: true, OrgActionTeamMemberRemoved: true,
	OrgActionInviteCreated: true, OrgActionInviteAccepted: true,
	OrgActionInviteCancelled: true,
	OrgActionOrgCreated:      true, OrgActionOrgUpdated: true,
	OrgActionOrgDeleted: true, OrgActionRepoCreated: true,
}

// OrgHookKind marks ActivityEvents appended to the org log (Repo carries
// the org name — there is no repo to name).
const OrgHookKind = "org"

// --- key helpers (bucket-relative) -------------------------------------------

// OrgHookKey returns orgs/<org>/webhooks/<id>.json.
func OrgHookKey(org, id string) string {
	return "orgs/" + org + "/webhooks/" + id + ".json"
}

// OrgWebhooksPrefix returns orgs/<org>/webhooks/ (LIST root, P5).
func OrgWebhooksPrefix(org string) string {
	return "orgs/" + org + "/webhooks/"
}

// OrgHookCursorKey returns the per-hook org-log cursor.
func OrgHookCursorKey(org, id string) string {
	return "orgs/" + org + "/webhooks/cursors/" + id + ".json"
}

// OrgHookRepoCursorKey returns the per-(hook, member repo) cursor over
// the member repo's collab-activity log.
func OrgHookRepoCursorKey(org, id, repo string) string {
	return "orgs/" + org + "/webhooks/cursors/" + id + "/repos/" + repo + ".json"
}

// OrgHookRepoCursorsPrefix returns the per-hook repo-cursor subtree
// (deleted wholesale with the hook).
func OrgHookRepoCursorsPrefix(org, id string) string {
	return "orgs/" + org + "/webhooks/cursors/" + id + "/repos/"
}

// OrgHookDeliveriesKey returns the per-hook deliveries ring key.
func OrgHookDeliveriesKey(org, id string) string {
	return "orgs/" + org + "/webhooks/" + id + "/deliveries/recent.json"
}

// OrgEventKey returns orgs/<org>/orgevents/<seq:012x>.json.
func OrgEventKey(org string, seq int) string {
	return fmt.Sprintf("orgs/%s/orgevents/%012x.json", org, seq)
}

// OrgHookStateKey returns orgs/<org>/meta/orghook_state.json: the org-log
// seq allocator (P3 two-step, same shape as CollabState).
func OrgHookStateKey(org string) string {
	return "orgs/" + org + "/meta/orghook_state.json"
}

// OrgHookState is the org-log seq allocator body.
type OrgHookState struct {
	NextSeq int `json:"next_seq"`
}

// --- owner gate ---------------------------------------------------------------

// OrgOwnerChecker is the narrow owner gate this package consumes
// (satisfied by *identity.Service via CheckOrgOwner; tests substitute a
// fake). Resolution stays owned by 01 — notify only delegates.
type OrgOwnerChecker interface {
	CheckOrgOwner(ctx context.Context, org string, p auth.Principal) *auth.AuthError
}

// requireOrgOwner enforces org ownership (or host admin) for org-hook
// writes and reads: anonymous failures are 401, authenticated-but-
// insufficient are 403, and a nil checker fails closed (deny) instead
// of failing open. Host admin always passes.
func (s *Service) requireOrgOwner(ctx context.Context, org string, p auth.Principal) error {
	if p.Admin {
		return nil
	}
	if s.OrgOwner == nil {
		if p.Anonymous {
			return fmt.Errorf("%w: authentication required", ErrUnauthorized)
		}
		return fmt.Errorf("%w: org owner required", ErrForbidden)
	}
	if cerr := s.OrgOwner.CheckOrgOwner(ctx, org, p); cerr != nil {
		switch cerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, cerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("unavailable: %s", cerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, cerr.Why)
		}
	}
	return nil
}

// --- validation -----------------------------------------------------------------

// validateOrgHookEvents enforces the filter: org actions, repo collab
// actions, [] = all, "*" wildcard.
func validateOrgHookEvents(events []string) error {
	for _, e := range events {
		if e == "*" {
			continue
		}
		if orgActions[e] || activityActions[e] {
			continue
		}
		return fmt.Errorf("%w: unknown webhook event %q", ErrInvalid, e)
	}
	return nil
}

// validOrgHookOrg reports whether org is a legal org spelling (identity
// owns the grammar; notify only borrows it).
func validOrgHookOrg(org string) bool { return identity.ValidOrg(org) }

// --- CRUD ------------------------------------------------------------------------

// CreateOrgHook validates and creates one org hook config (owner-gated
// by the handler). Beyond maxHooks the create is refused with
// ErrConflict — the same per-scope sweep/delivery bound as repo hooks
// (issue #156), mirrored per org.
func (s *Service) CreateOrgHook(ctx context.Context, org, actor string, spec HookSpec) (*Hook, error) {
	if !validOrgHookOrg(org) {
		return nil, fmt.Errorf("%w: bad org %q", ErrInvalid, org)
	}
	if spec.URL == nil || *spec.URL == "" {
		return nil, fmt.Errorf("%w: url is required", ErrInvalid)
	}
	if err := validateHookURL(*spec.URL); err != nil {
		return nil, err
	}
	if err := validateOrgHookEvents(spec.Events); err != nil {
		return nil, err
	}
	if existing, err := s.ListOrgHooks(ctx, org); err != nil {
		return nil, err
	} else if len(existing) >= s.maxHooks() {
		return nil, fmt.Errorf("%w: webhook limit reached (%d per org)", ErrConflict, s.maxHooks())
	}
	now := s.nowUTC().Format(dateTimeFmt)
	h := &Hook{
		ID: newHookID(s.nowUTC(), rand.Reader), URL: *spec.URL,
		Events: append([]string(nil), spec.Events...), Active: true,
		CreatedBy: normPrincipal(actor), CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if spec.Active != nil {
		h.Active = *spec.Active
	}
	if spec.InsecureTLS != nil {
		h.InsecureTLS = *spec.InsecureTLS
	}
	if spec.Secret != nil {
		h.Secret = *spec.Secret
	}
	raw, err := encode(h)
	if err != nil {
		return nil, err
	}
	if err := s.putCreate(ctx, OrgHookKey(org, h.ID), raw); err != nil {
		if store.IsPreconditionFailed(err) {
			// ULID collision is randomness failure, not a conflict —
			// retry once with fresh entropy (repo-hook parity).
			h.ID = newHookID(s.nowUTC(), rand.Reader)
			raw, err := encode(h)
			if err != nil {
				return nil, err
			}
			if err := s.putCreate(ctx, OrgHookKey(org, h.ID), raw); err != nil {
				return nil, err
			}
			s.armOrgSweep(org, h.Active)
			return h, nil
		}
		return nil, err
	}
	s.armOrgSweep(org, h.Active)
	return h, nil
}

// armOrgSweep marks the org for a delivery pass after a new or
// newly-activated hook (the repo armHookSweep backstop, per org: the
// sweep gate skips orgs whose allocator did not advance, so without the
// mark a hook created onto a quiet org's backlog would wait for the next
// emission). Inactive hooks arm nothing.
func (s *Service) armOrgSweep(org string, active bool) {
	if !active {
		return
	}
	s.hookMu.Lock()
	s.orgHookPending[org] = true
	s.hookMu.Unlock()
	s.wakeOrg(org)
}

// GetOrgHook loads one org hook; nil when absent.
func (s *Service) GetOrgHook(ctx context.Context, org, id string) *Hook {
	raw, _, err := s.getJSON(ctx, OrgHookKey(org, id))
	if err != nil || raw == nil {
		return nil
	}
	var h Hook
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil
	}
	return &h
}

// ListOrgHooks returns all org hooks, creation order (ULID sort),
// secrets intact (callers strip before responding).
func (s *Service) ListOrgHooks(ctx context.Context, org string) ([]*Hook, error) {
	keys := []string{}
	err := s.Store.List(ctx, OrgWebhooksPrefix(org), "", func(m store.ObjectMeta) error {
		name := strings.TrimPrefix(m.Key, OrgWebhooksPrefix(org))
		if name == "" || strings.Contains(name, "/") || !strings.HasSuffix(name, ".json") {
			return nil // cursors/ + deliveries/ live below; skip
		}
		keys = append(keys, m.Key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortStrings(keys)
	out := []*Hook{}
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil || raw == nil {
			continue
		}
		var h Hook
		if err := json.Unmarshal(raw, &h); err != nil {
			continue
		}
		hc := h
		out = append(out, &hc)
	}
	return out, nil
}

// PatchOrgHook CAS-updates mutable fields (owner-gated by the handler).
// URL/events validated against the org filter; secret replaced when
// present (never returned).
func (s *Service) PatchOrgHook(ctx context.Context, org, id string, spec HookSpec) (*Hook, error) {
	var result *Hook
	_, err := s.casUpdate(ctx, OrgHookKey(org, id), 8, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown webhook", ErrNotFound)
		}
		var h Hook
		if err := json.Unmarshal(cur, &h); err != nil {
			return nil, false, fmt.Errorf("%w: webhook: %v", ErrInvalid, err)
		}
		if spec.URL != nil {
			if err := validateHookURL(*spec.URL); err != nil {
				return nil, false, err
			}
			h.URL = *spec.URL
		}
		if spec.Events != nil {
			if err := validateOrgHookEvents(spec.Events); err != nil {
				return nil, false, err
			}
			h.Events = append([]string(nil), spec.Events...)
		}
		if spec.Secret != nil {
			h.Secret = *spec.Secret
		}
		if spec.Active != nil {
			h.Active = *spec.Active
		}
		if spec.InsecureTLS != nil {
			h.InsecureTLS = *spec.InsecureTLS
		}
		h.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		h.Version++
		result = &h
		raw, err := encode(h)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
	if err != nil {
		return nil, err
	}
	if result != nil && spec.Active != nil && *spec.Active {
		s.armOrgSweep(org, true)
	}
	return result, nil
}

// DeleteOrgHook removes the config, the org-log cursor, every
// per-(hook, repo) cursor, and the deliveries ring (owner-gated by the
// handler). Sweep watermarks for the hook are dropped too.
func (s *Service) DeleteOrgHook(ctx context.Context, org, id string) error {
	if s.GetOrgHook(ctx, org, id) == nil {
		return fmt.Errorf("%w: unknown webhook", ErrNotFound)
	}
	if err := s.Store.Delete(ctx, OrgHookKey(org, id), ""); err != nil {
		return err
	}
	_ = s.Store.Delete(ctx, OrgHookCursorKey(org, id), "")
	_ = s.Store.Delete(ctx, OrgHookDeliveriesKey(org, id), "")
	// Per-repo cursors: bounded LIST over the hook's cursor subtree
	// (one key per member repo served) then delete each.
	_ = s.Store.List(ctx, OrgHookRepoCursorsPrefix(org, id), "", func(m store.ObjectMeta) error {
		_ = s.Store.Delete(ctx, m.Key, "")
		return nil
	})
	s.hookMu.Lock()
	delete(s.orgHookPending, org)
	for key := range s.orgRepoSeen {
		if strings.HasPrefix(key, org+"\x00") {
			delete(s.orgRepoSeen, key)
		}
	}
	s.hookMu.Unlock()
	return nil
}

// ReadOrgDeliveries loads the org hook's ring (nil entries when absent).
func (s *Service) ReadOrgDeliveries(ctx context.Context, org, id string) *DeliveriesDoc {
	return readDeliveriesAt(ctx, s, OrgHookDeliveriesKey(org, id))
}

// --- org event log ------------------------------------------------------------------

// reserveOrgSeq CAS-allocates the next org-log seq (P3 two-step, step 1).
func (s *Service) reserveOrgSeq(ctx context.Context, org string) (int, error) {
	var seq int
	_, err := s.casUpdate(ctx, OrgHookStateKey(org), 8, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var st OrgHookState
		if cur != nil {
			if err := json.Unmarshal(cur, &st); err != nil {
				return nil, false, fmt.Errorf("%w: orghook_state: %v", ErrInvalid, err)
			}
		}
		seq = st.NextSeq + 1
		if seq < 1 {
			seq = 1
		}
		st.NextSeq = seq
		raw, err := encode(st)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// readOrgEvent loads one org event; nil when absent.
func (s *Service) readOrgEvent(ctx context.Context, org string, seq int) *ActivityEvent {
	raw, _, err := s.getJSON(ctx, OrgEventKey(org, seq))
	if err != nil || raw == nil {
		return nil
	}
	var ev ActivityEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}
	return &ev
}

// orgHead returns the org-log allocator head (0 when absent).
func (s *Service) orgHead(ctx context.Context, org string) int {
	raw, _, err := s.getJSON(ctx, OrgHookStateKey(org))
	if err != nil || raw == nil {
		return 0
	}
	var st OrgHookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return 0
	}
	return st.NextSeq
}

// EmitOrgEvent appends one org-log event (member/team/invite change)
// and wakes org delivery. It is the identity layer's post-commit
// observer (P8): the mutation already committed, so a drop here loses
// one webhook event, never data — and every drop is logged (law 7: no
// silent waiting, no silent loss). Unknown actions are programming
// errors: logged and dropped before any seq is reserved (no gap).
func (s *Service) EmitOrgEvent(ctx context.Context, org, action, actor, title string) {
	if !orgActions[action] {
		s.log().WarnContext(ctx, "notify: org emission dropped: unknown action",
			"org", org, "action", action, "actor", normPrincipal(actor))
		return
	}
	org = strings.ToLower(strings.TrimSpace(org))
	if !validOrgHookOrg(org) {
		s.log().WarnContext(ctx, "notify: org emission dropped: bad org",
			"org", org, "action", action)
		return
	}
	at := s.nowUTC().Format(dateTimeFmt)
	ev := ActivityEvent{
		Seq: 0, Repo: org, Action: action, Kind: OrgHookKind,
		Actor: normPrincipal(actor), Title: title, At: at,
	}
	payload, err := encode(map[string]any{"class": action})
	if err != nil {
		s.log().WarnContext(ctx, "notify: org emission dropped: payload encode failed",
			"org", org, "action", action, "err", err)
		return
	}
	ev.Payload = payload
	seq, err := s.reserveOrgSeq(ctx, org)
	if err != nil {
		s.log().WarnContext(ctx, "notify: org emission dropped: seq reserve failed",
			"org", org, "action", action, "err", err)
		return
	}
	ev.Seq = seq
	raw, err := encode(ev)
	if err != nil {
		s.log().WarnContext(ctx, "notify: org emission dropped: event encode failed",
			"org", org, "action", action, "seq", seq, "err", err)
		return
	}
	if err := s.putCreate(ctx, OrgEventKey(org, seq), raw); err != nil && !store.IsPreconditionFailed(err) {
		s.log().WarnContext(ctx, "notify: org emission dropped: event append failed",
			"org", org, "action", action, "seq", seq, "err", err)
		return
	}
	// Live tail (Forgejo #364): the org activity stream reuses the repo
	// frame bus verbatim (Name "org_activity", keyed by the bare org —
	// repo keys always carry a slash, so the namespaces are disjoint).
	// Publish never blocks (drop-oldest); the log stays the backfill
	// truth, so a shed frame loses nothing durable.
	s.PublishFrame(RepoFrame{
		Name: OrgActivityFrameKind, Repo: org, Action: action,
		Title: title, At: at, Actor: normPrincipal(actor), Seq: seq,
	})
	s.wakeOrg(org)
}

// --- delivery -------------------------------------------------------------------------

// orgRepos snapshots the org's member repos (owner == org) via the shared
// repo enumeration (cold path only — delivery passes and the minute
// sweep, never a git hot path). Sorted for deterministic passes.
func (s *Service) orgRepos(ctx context.Context, org string) []string {
	var repos []string
	s.eachRepo(ctx, func(owner, repo string) {
		if owner == org {
			repos = append(repos, repo)
		}
	})
	sortStrings(repos)
	return repos
}

// DeliverOrg runs one org-hook pass: every active hook delivers the new
// org-log events plus every member repo's new activity events.
// Sequential per hook per scope; hooks run in parallel under the
// pre-acquired FanoutParallel semaphore (issue #153). Best-effort per
// hook; a failed hook holds back only its own cursors. The pass ends
// with the sweep-gate watermark update (finishOrgPass).
func (s *Service) DeliverOrg(ctx context.Context, org string) {
	hooks, err := s.ListOrgHooks(ctx, org)
	if err != nil {
		return
	}
	active := []*Hook{}
	for _, h := range hooks {
		if h.Active {
			active = append(active, h)
		}
	}
	if len(active) == 0 {
		return
	}
	repos := s.orgRepos(ctx, org)
	sem := make(chan struct{}, FanoutParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	deliveredOrg := map[string]int{}
	deliveredRepo := map[string]int{}
loop:
	for _, h := range active {
		h := h
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop // stop launching; still Wait for in-flight below
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			lastOrg, lastRepo := s.deliverOrgHook(ctx, org, h, repos)
			mu.Lock()
			deliveredOrg[h.ID] = lastOrg
			for repo, seq := range lastRepo {
				deliveredRepo[h.ID+"\x00"+repo] = seq
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	s.finishOrgPass(ctx, org, active, repos, deliveredOrg, deliveredRepo)
}

// deliverOrgHook delivers one hook's org-log window then each member
// repo's activity window, and reports the per-scope watermarks (highest
// seq delivered or filter-advanced) for the sweep-gate check.
// At-least-once per scope: a crash or lost CAS redelivers (consumers
// dedup on X-Walgit-Delivery). Compacted gaps count and continue from
// the oldest readable event (honest-gap semantics, repo-hook parity).
func (s *Service) deliverOrgHook(ctx context.Context, org string, h *Hook, repos []string) (int, map[string]int) {
	lastRepo := map[string]int{}
	lastOrg := s.deliverOrgLog(ctx, org, h)
	for _, repo := range repos {
		lastRepo[repo] = s.deliverOrgRepo(ctx, org, h, repo)
	}
	return lastOrg, lastRepo
}

// deliverOrgLog scans orgevents/ from the hook's org cursor and POSTs
// each matching event; the cursor CAS-advances past the delivered prefix
// only. Ping bypasses the filter (repo-hook parity).
func (s *Service) deliverOrgLog(ctx context.Context, org string, h *Hook) int {
	cursor := readCursorAt(ctx, s, OrgHookCursorKey(org, h.ID))
	seq := cursor + 1
	lastDelivered := cursor
	const maxBatch = 256
	for i := 0; i < maxBatch; i++ {
		ev := s.readOrgEvent(ctx, org, seq)
		if ev == nil {
			if ahead := s.probeOrgAhead(ctx, org, seq); ahead > seq {
				seq = ahead
				continue
			}
			break
		}
		seq++
		if ev.Action != ActionPing && !hookMatches(h.Events, ev.Action) {
			if seq-1 > lastDelivered {
				lastDelivered = seq - 1 // filtered events still advance
			}
			continue
		}
		status, derr := s.postEvent(ctx, h, ev)
		recordDeliveryAt(ctx, s, OrgHookDeliveriesKey(org, h.ID), ev, status, derr)
		if derr != nil || status < 200 || status >= 300 {
			break // cursor stays: retry from here next pass
		}
		lastDelivered = seq - 1
	}
	if lastDelivered > cursor {
		advanceCursorAt(ctx, s, OrgHookCursorKey(org, h.ID), lastDelivered)
	}
	return lastDelivered
}

// deliverOrgRepo scans one member repo's collab-events/ from the hook's
// per-(hook, repo) cursor and POSTs each matching event; the cursor
// CAS-advances past the delivered prefix only. Ping bypasses the filter
// (a member repo's ping proves through to org hooks too).
func (s *Service) deliverOrgRepo(ctx context.Context, org string, h *Hook, repo string) int {
	cursor := readCursorAt(ctx, s, OrgHookRepoCursorKey(org, h.ID, repo))
	seq := cursor + 1
	lastDelivered := cursor
	const maxBatch = 256
	for i := 0; i < maxBatch; i++ {
		ev := s.readActivity(ctx, org, repo, seq)
		if ev == nil {
			if ahead := s.probeAhead(ctx, org, repo, seq); ahead > seq {
				seq = ahead
				continue
			}
			break
		}
		seq++
		if ev.Action != ActionPing && !hookMatches(h.Events, ev.Action) {
			if seq-1 > lastDelivered {
				lastDelivered = seq - 1
			}
			continue
		}
		status, derr := s.postEvent(ctx, h, ev)
		recordDeliveryAt(ctx, s, OrgHookDeliveriesKey(org, h.ID), ev, status, derr)
		if derr != nil || status < 200 || status >= 300 {
			break
		}
		lastDelivered = seq - 1
	}
	if lastDelivered > cursor {
		advanceCursorAt(ctx, s, OrgHookRepoCursorKey(org, h.ID, repo), lastDelivered)
	}
	return lastDelivered
}

// probeOrgAhead looks for the next existing org event within a small
// window past seq (gap detection); 0 when none exists (seq is the head).
func (s *Service) probeOrgAhead(ctx context.Context, org string, seq int) int {
	for ahead := seq + 1; ahead <= seq+8; ahead++ {
		if meta, err := s.Store.Head(ctx, OrgEventKey(org, ahead)); err == nil && meta != nil {
			return ahead
		}
	}
	return 0
}

// finishOrgPass stores the sweep-gate watermarks for a completed pass:
// pending is true while any active hook lags the org head or any member
// repo head (POST failure or a 256-event backlog — the at-least-once
// retry contract is unchanged, only orgs that need it pay for it).
// orgHookSeen advances to the org head regardless (pending covers the
// shortfall); orgRepoSeen advances per member repo the same way. A
// head-read failure keeps pending (retry next pass — fail closed toward
// delivery). hookMu is held for the map update only, never across store
// calls.
func (s *Service) finishOrgPass(ctx context.Context, org string, active []*Hook, repos []string, deliveredOrg map[string]int, deliveredRepo map[string]int) {
	// Absent logs read head 0 (nothing to deliver), never -1: a hook
	// created onto an org (or repo) with no log yet must settle
	// pending=false after its first pass, or the sweep would re-pass
	// every minute forever on the create-arm alone.
	orgHead := s.orgHead(ctx, org)
	repoHeads := map[string]int{}
	for _, repo := range repos {
		repoHeads[repo] = s.repoHead(ctx, org, repo)
	}
	pending := false
	for _, h := range active {
		if deliveredOrg[h.ID] < orgHead {
			pending = true
			break
		}
	}
	if !pending {
		for _, h := range active {
			for _, repo := range repos {
				if repoHeads[repo] >= 0 && deliveredRepo[h.ID+"\x00"+repo] < repoHeads[repo] {
					pending = true
					break
				}
			}
			if pending {
				break
			}
		}
	}
	s.hookMu.Lock()
	if orgHead > s.orgHookSeen[org] {
		s.orgHookSeen[org] = orgHead
	}
	for _, repo := range repos {
		if repoHeads[repo] > s.orgRepoSeen[org+"\x00"+repo] {
			s.orgRepoSeen[org+"\x00"+repo] = repoHeads[repo]
		}
	}
	s.orgHookPending[org] = pending
	s.hookMu.Unlock()
}

// PingOrgHook synthesizes a ping org event, appends it to the org log,
// and POSTs exactly that event through the normal delivery path
// (postEvent: same wire shape, keeper headers, HMAC, client, and org
// deliveries ring), so ping success proves URL + secret end to end. The
// backlog is deliberately NOT replayed here (repo PingHook parity,
// issue #155). Ping performs no cursor CAS when a backlog is ahead of
// it; in the no-backlog case it advances the org cursor past the ping
// (monotonic, never beyond) so the loop does not redeliver it. Returns
// delivered=true when the ping POST gets a 2xx; a failed POST is
// (false, nil) with the detail on the deliveries ring.
func (s *Service) PingOrgHook(ctx context.Context, org, id, actor string) (bool, error) {
	h := s.GetOrgHook(ctx, org, id)
	if h == nil {
		return false, fmt.Errorf("%w: unknown webhook", ErrNotFound)
	}
	if !h.Active {
		return false, fmt.Errorf("%w: webhook inactive", ErrInvalid)
	}
	seq, err := s.reserveOrgSeq(ctx, org)
	if err != nil {
		return false, err
	}
	ev := ActivityEvent{
		Seq: seq, Repo: org, Action: ActionPing,
		Kind: OrgHookKind, Actor: normPrincipal(actor), At: s.nowUTC().Format(dateTimeFmt),
	}
	raw, err := encode(ev)
	if err != nil {
		return false, err
	}
	if err := s.putCreate(ctx, OrgEventKey(org, seq), raw); err != nil && !store.IsPreconditionFailed(err) {
		return false, err
	}
	status, derr := s.postEvent(ctx, h, &ev)
	recordDeliveryAt(ctx, s, OrgHookDeliveriesKey(org, h.ID), &ev, status, derr)
	if derr != nil || status < 200 || status >= 300 {
		return false, nil
	}
	if readCursorAt(ctx, s, OrgHookCursorKey(org, id)) == seq-1 {
		advanceCursorAt(ctx, s, OrgHookCursorKey(org, id), seq)
	}
	return true, nil
}

// --- shared key-parameterized cursor/delivery helpers ---------------------------
// The repo path keeps its (owner, repo, id) wrappers below; the org path
// calls these directly. One implementation, two key families.

// readCursorAt loads a cursor doc (0 when absent).
func readCursorAt(ctx context.Context, s *Service, key string) int {
	raw, _, err := s.getJSON(ctx, key)
	if err != nil || raw == nil {
		return 0
	}
	var c CursorDoc
	if err := json.Unmarshal(raw, &c); err != nil || c.PublishedSeq < 0 {
		return 0
	}
	return c.PublishedSeq
}

// advanceCursorAt CASes a cursor forward (monotonic: never retreats).
func advanceCursorAt(ctx context.Context, s *Service, key string, seq int) {
	_, _ = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var c CursorDoc
		if cur != nil {
			if err := json.Unmarshal(cur, &c); err != nil {
				return nil, false, nil // corrupt cursor: leave for the next pass
			}
		}
		if seq <= c.PublishedSeq {
			return nil, false, nil
		}
		c.PublishedSeq = seq
		c.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		raw, err := encode(c)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
}

// recordDeliveryAt appends one row to the last-25 ring at key
// (best-effort CAS; transport errors scrubbed of credential material).
func recordDeliveryAt(ctx context.Context, s *Service, key string, ev *ActivityEvent, status int, derr error) {
	entry := DeliveryEntry{Seq: ev.Seq, Event: ev.Action, Status: status, At: s.nowUTC().Format(dateTimeFmt)}
	if derr != nil {
		entry.Error = scrubDeliveryError(derr.Error())
	}
	_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var d DeliveriesDoc
		if cur != nil {
			_ = json.Unmarshal(cur, &d)
		}
		d.Entries = append(d.Entries, entry)
		if len(d.Entries) > MaxDeliveries {
			d.Entries = d.Entries[len(d.Entries)-MaxDeliveries:]
		}
		d.UpdatedAt = entry.At
		raw, err := encode(d)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	})
}

// readDeliveriesAt loads a ring (nil entries when absent — wire `[]`).
func readDeliveriesAt(ctx context.Context, s *Service, key string) *DeliveriesDoc {
	raw, _, err := s.getJSON(ctx, key)
	if err != nil || raw == nil {
		return &DeliveriesDoc{Entries: []DeliveryEntry{}}
	}
	var d DeliveriesDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return &DeliveriesDoc{Entries: []DeliveryEntry{}}
	}
	if d.Entries == nil {
		d.Entries = []DeliveryEntry{}
	}
	return &d
}

// --- org sweep + tasks ------------------------------------------------------------

// TaskKindOrgWebhooks runs the org-hook delivery loop for one org: the
// org log plus every member repo's activity log, started by sweep +
// wake-up after each org emission.
const TaskKindOrgWebhooks = "org-webhooks"

// orgTaskRepo renders the task-table repo key for an org pass.
func orgTaskRepo(org string) string { return "orgs/" + org }

// wakeOrg triggers an org-hooks pass (non-blocking, coalesced).
func (s *Service) wakeOrg(org string) {
	select {
	case s.wakeOrgCh <- org:
	default:
	}
}

// StartOrgWebhooks starts (or joins) the org-webhooks task for org. The
// leader is tracked in the service WaitGroup and derives its work from
// the service drainCtx (phase-1 drain cancels it promptly, issue #154).
// Refuses fast (nil) once draining.
func (s *Service) StartOrgWebhooks(ctx context.Context, org string) *TaskRecord {
	if !validOrgHookOrg(org) {
		return nil
	}
	if s.Draining() {
		return nil
	}
	repo := orgTaskRepo(org)
	e, joined := s.tasks.begin(repo, TaskKindOrgWebhooks, s.nowUTC())
	if joined {
		return e.rec
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.DeliverOrg(s.drainCtx, org)
		s.tasks.end(repo, TaskKindOrgWebhooks, "org webhooks pass", s.nowUTC())
	}()
	return e.rec
}

// eachOrg visits every org once via the store prefix LIST (minute
// sweeps only — never a git hot path).
func (s *Service) eachOrg(ctx context.Context, fn func(org string)) {
	seen := map[string]bool{}
	_ = s.Store.ListPrefixes(ctx, "orgs/", func(orgSlash string) error {
		org := strings.TrimSuffix(strings.TrimPrefix(orgSlash, "orgs/"), "/")
		if org == "" || strings.Contains(org, "/") || seen[org] {
			return nil
		}
		seen[org] = true
		fn(org)
		return nil
	})
}

// sweepOrgHooks schedules an org delivery pass only where one is due:
// an org costs one hooks LIST plus one org-state GET per pass unless
// its allocator advanced since the last scheduled pass, its last pass
// left work behind (pending), or a member repo advanced — in which case
// one collab_state GET per member repo decides. hookMu guards only the
// watermark maps and is never held across a store call.
func (s *Service) sweepOrgHooks(ctx context.Context) {
	observed := map[string]bool{}
	s.eachOrg(ctx, func(org string) {
		observed[org] = true
		s.sweepOrg(ctx, org)
	})
	// Drop watermarks for deleted orgs (bounded by the enumeration).
	s.hookMu.Lock()
	for org := range s.orgHookSeen {
		if !observed[org] {
			delete(s.orgHookSeen, org)
			delete(s.orgHookPending, org)
		}
	}
	for key := range s.orgRepoSeen {
		if o, _, ok := strings.Cut(key, "\x00"); !ok || !observed[o] {
			delete(s.orgRepoSeen, key)
		}
	}
	s.hookMu.Unlock()
}

// sweepOrg gates one org's minute pass. seen advances only past state
// actually scheduled; pending is cleared only by a completed pass
// (finishOrgPass) or DeleteOrgHook, so a hook created between the LIST
// and the gate cannot be disarmed by this pass.
func (s *Service) sweepOrg(ctx context.Context, org string) {
	hooks, err := s.ListOrgHooks(ctx, org)
	if err != nil {
		return
	}
	if len(hooks) == 0 {
		s.hookMu.Lock()
		delete(s.orgHookSeen, org)
		delete(s.orgHookPending, org)
		for key := range s.orgRepoSeen {
			if strings.HasPrefix(key, org+"\x00") {
				delete(s.orgRepoSeen, key)
			}
		}
		s.hookMu.Unlock()
		return
	}
	active := false
	for _, h := range hooks {
		if h.Active {
			active = true
			break
		}
	}
	s.hookMu.Lock()
	seen := s.orgHookSeen[org]
	pending := s.orgHookPending[org]
	s.hookMu.Unlock()
	head := s.orgHead(ctx, org)
	if head <= seen && !pending {
		// Org log quiet and drained: probe member repos before
		// skipping (one collab_state GET each — the repos are the
		// other half of an org pass).
		for _, repo := range s.orgRepos(ctx, org) {
			if s.repoHead(ctx, org, repo) > s.orgRepoSeenAt(org, repo) {
				s.StartOrgWebhooks(ctx, org)
				return
			}
		}
		return
	}
	if !active && !pending {
		// All hooks parked: no delivery to run. seen still advances —
		// inactivity is inactivity. pending is left alone: only an
		// active create/activation arms it, and those re-check.
		s.hookMu.Lock()
		if head > s.orgHookSeen[org] {
			s.orgHookSeen[org] = head
		}
		s.hookMu.Unlock()
		return
	}
	s.StartOrgWebhooks(ctx, org)
}

// repoHead returns one member repo's activity allocator head (-1 absent).
func (s *Service) repoHead(ctx context.Context, owner, repo string) int {
	raw, _, err := s.getJSON(ctx, CollabStateKey(owner, repo))
	if err != nil || raw == nil {
		return -1
	}
	var st CollabState
	if err := json.Unmarshal(raw, &st); err != nil {
		return -1
	}
	return st.NextSeq
}

// orgRepoSeenAt reads one (org, repo) gate watermark (hookMu only).
func (s *Service) orgRepoSeenAt(org, repo string) int {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.orgRepoSeen[org+"\x00"+repo]
}

// --- retention guard ---------------------------------------------------------------

// orgRepoMinCursor folds the org hooks' per-(hook, repo) cursors into a
// retention floor: the repo-events pass must not delete activity below
// an org hook's cursor (the org pass may not have delivered it yet).
// Returns -1 when no org hook tracks the repo.
func (s *Service) orgRepoMinCursor(ctx context.Context, owner, repo string) int {
	hooks, err := s.ListOrgHooks(ctx, owner)
	if err != nil || len(hooks) == 0 {
		return -1
	}
	min := -1
	for _, h := range hooks {
		if !h.Active {
			continue
		}
		c := readCursorAt(ctx, s, OrgHookRepoCursorKey(owner, h.ID, repo))
		if min < 0 || c < min {
			min = c
		}
	}
	return min
}
