// orgactivity.go — Forgejo #364: the org activity surface over the #363
// org-event log.
//
// GET /api/v1/orgs/{org}/activity pages the immutable
// orgs/<org>/orgevents/<seq:012x>.json log newest-first (member, team,
// invite, org-lifecycle, and repo-birth events — every orgActions
// spelling), and GET …/activity/stream carries the same events live.
// The org log is the backfill truth for both; the webhook fan-out
// (DeliverOrg) and this surface read the same objects.
//
// Read cost (law 6): one orghook_state GET for the head plus one exact-key
// GET per probed seq — never a LIST. Pages are bounded (default 50, max
// 200, the tray page convention); a page walks at most n+maxOrgGap
// probes, so a deep page into a retention-deleted prefix comes back
// short with more=true instead of burning GETs to the floor — the client
// keeps paging and converges. Retention (§9) compacts the log on the
// notify-retention pass with the same floor as repo activity (cursors +
// 7 days, capped per pass).
//
// Live frames reuse the repo frame bus verbatim (stream.go): EmitOrgEvent
// publishes one Name "org_activity" frame keyed by the bare org name
// after the event commits, and the stream handler subscribes that key
// (recent-ring replay + live tail, same as collab/stream). Key collision
// with repo frames is impossible — repo keys are always "owner/repo"
// (contain a slash), org keys never do. The stream is live-only cache:
// a restart drops the ring and late attachers backfill via the API.
//
// Gating: both routes are owner-gated by the handler (requireOrgOwner,
// same as the org webhook routes) — anonymous 401, non-owner 403.
//
// ### Concurrency
//
// Hazard: concurrent readers racing retention deletes (a probed event
// vanishing mid-page) or concurrent emitters advancing the head
// mid-page. Avoidance: no locks at all — events are immutable and seqs
// monotonic, so a page is a point-in-time walk that may interleave newer
// appends (they sort above the page, never inside it); a deleted-behind-
// us probe reads as a gap and is skipped. Publishing never blocks (the
// bus is drop-oldest); the handler exits via the request context.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Org activity page bounds (the tray page convention, notify.go):
// default 50, hard max 200 per page.
const (
	OrgActivityPageSize = 50
	OrgActivityMaxPage  = 200
	// maxOrgGap bounds the extra probes of one page past the n collected
	// events: crash-reserved seq gaps are rare single holes (the delivery
	// probe window is 8), while a retention-deleted prefix is contiguous
	// — without a cap a deep page would walk the whole prefix one 404 at
	// a time. A page that exhausts the cap comes back short with
	// more=true; the client keeps paging and converges.
	maxOrgGap = 64
)

// OrgActivityFrameKind is the repo-bus frame name for org-log events
// (the stream envelope reuses collab/stream's bus + writer verbatim).
const OrgActivityFrameKind = "org_activity"

// ListOrgActivity returns the newest-first page of the org's event log:
// events with seq < after (after <= 0 starts at the head), up to n events
// (n <= 0 defaults to OrgActivityPageSize, capped at OrgActivityMaxPage).
// more reports whether older seqs may exist (true when the page filled or
// the probe cap stopped the walk above seq 1 — both mean "keep paging").
// Gaps (crash-reserved seqs, retention-deleted prefixes) are skipped, never
// errors. The slice is []-never-nil.
func (s *Service) ListOrgActivity(ctx context.Context, org string, after, n int) ([]ActivityEvent, bool, error) {
	org = strings.ToLower(strings.TrimSpace(org))
	if !validOrgHookOrg(org) {
		return nil, false, fmt.Errorf("%w: bad org %q", ErrInvalid, org)
	}
	if after < 0 {
		return nil, false, fmt.Errorf("%w: bad after", ErrInvalid)
	}
	if n <= 0 {
		n = OrgActivityPageSize
	}
	if n > OrgActivityMaxPage {
		n = OrgActivityMaxPage
	}
	head := s.orgHead(ctx, org)
	start := head
	if after > 0 {
		start = after - 1
		if start > head {
			start = head
		}
	}
	out := []ActivityEvent{}
	probes := 0
	seq := start
	for ; seq >= 1 && len(out) <= n && probes < n+maxOrgGap; seq-- {
		probes++
		ev := s.readOrgEvent(ctx, org, seq)
		if ev == nil {
			continue // gap — never an error, never deleted unseen
		}
		out = append(out, *ev)
	}
	more := seq >= 1
	if len(out) > n {
		out = out[:n]
		more = true
	}
	return out, more, nil
}

// retainOrgEvents deletes org-log events below the minimum active
// org-hook cursor AND older than CollabEventsFloorDays (the
// retainRepoEvents contract, per org). Events are seq-ordered ≈
// time-ordered, so the scan stops at the first too-new event. Capped per
// pass; hookless orgs compact everything past the floor (the log is an
// audit surface, not a delivery queue — without hooks nothing holds it,
// but the same 7-day floor + per-pass caps keep the growth bounded).
func (s *Service) retainOrgEvents(ctx context.Context, org string, now time.Time) {
	floor := now.AddDate(0, 0, -CollabEventsFloorDays).Format(dateTimeFmt)
	minCursor := -1
	hooks, err := s.ListOrgHooks(ctx, org)
	if err != nil {
		return
	}
	active := 0
	for _, h := range hooks {
		if !h.Active {
			continue
		}
		active++
		c := readCursorAt(ctx, s, OrgHookCursorKey(org, h.ID))
		if minCursor < 0 || c < minCursor {
			minCursor = c
		}
	}
	if active == 0 {
		minCursor = s.orgHead(ctx, org)
	}
	if minCursor <= 1 {
		return
	}
	const maxDeletes = 500
	const maxScan = 600
	deleted, scanned := 0, 0
	for seq := 1; seq < minCursor && deleted < maxDeletes && scanned < maxScan; seq++ {
		scanned++
		ev := s.readOrgEvent(ctx, org, seq)
		if ev == nil {
			continue // gap — never delete what we cannot see
		}
		if ev.At >= floor {
			break // seq-ordered ≈ time-ordered: the rest is newer
		}
		_ = s.Store.Delete(ctx, OrgEventKey(org, seq), "")
		deleted++
	}
}

// orgActivityFrame maps one live org frame to its wire shape for the
// stream (Repo carries the org name — there is no repo to name).
func orgActivityFrame(f RepoFrame) (map[string]any, bool) {
	if f.Name != OrgActivityFrameKind {
		return nil, false
	}
	return map[string]any{
		"seq":    f.Seq,
		"org":    f.Repo,
		"action": f.Action,
		"actor":  f.Actor,
		"title":  f.Title,
		"at":     f.At,
	}, true
}

// orgActivity serves GET /api/v1/orgs/{org}/activity (both lanes):
// newest-first {events, more} over the org log. n defaults to 50, caps
// at 200 (bad n → 400, the tray rule); after is a seq cursor (bad → 400).
func (h *Handler) orgActivity(w http.ResponseWriter, r *http.Request, org string) {
	q := r.URL.Query()
	n := OrgActivityPageSize
	if raw := q.Get("n"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			writePlain(w, http.StatusBadRequest, "bad n")
			return
		}
		n = v
		if n > OrgActivityMaxPage {
			n = OrgActivityMaxPage
		}
	}
	after := 0
	if raw := q.Get("after"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writePlain(w, http.StatusBadRequest, "bad after")
			return
		}
		after = v
	}
	events, more := h.Svc.trayOrgActivity(r.Context(), org, after, n)
	writeNoStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "more": more})
}

// trayOrgActivity is the handler-thin read path (ListOrgActivity with the
// org already gated; a store failure degrades to an empty page — the log
// read is fail-open toward serving, retention never depends on it).
func (s *Service) trayOrgActivity(ctx context.Context, org string, after, n int) ([]ActivityEvent, bool) {
	events, more, err := s.ListOrgActivity(ctx, org, after, n)
	if err != nil {
		return []ActivityEvent{}, false
	}
	return events, more
}

// orgActivityStream serves GET /api/v1/orgs/{org}/activity/stream (both
// lanes): the recent org ring replays first (late attachers backfill),
// then live org_activity frames ride until the client cancels. Owner-
// gated by the caller (handleOrg); anonymous never reaches here.
func (h *Handler) orgActivityStream(w http.ResponseWriter, r *http.Request, org string, p auth.Principal) {
	if r.Method != http.MethodGet {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s, ok := newSSEWriter(w, r)
	if !ok {
		writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
		return
	}
	defer s.close()
	ch, recent, unsub := h.Svc.SubscribeRepo(strings.ToLower(strings.TrimSpace(org)))
	defer unsub()
	for _, f := range recent {
		if f.Name != OrgActivityFrameKind {
			continue
		}
		frame, ok := orgActivityFrame(f)
		if !ok {
			continue
		}
		if !s.event(OrgActivityFrameKind, string(collabJSON(frame))) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-ch:
			if !ok {
				return
			}
			frame, ok := orgActivityFrame(f)
			if !ok {
				continue
			}
			if !s.event(OrgActivityFrameKind, string(collabJSON(frame))) {
				return
			}
		}
	}
}
