package pulls

import (
	"context"
	"errors"
	"fmt"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns §4 (mergeability: the stamped derived cache, the `pulls`
// event sink, the `pull-mergeable` task) plus the diff/commits read paths.

// loadMergeable reads a mergeable.json cache; (nil, "", nil) when absent.
func (s *Service) loadMergeable(ctx context.Context, owner, repo string, num int) (*MergeableDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, MergeableKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	m, perr := parseMergeable(raw)
	if perr != nil {
		return nil, "", perr
	}
	return m, ver, nil
}

// unknownMergeable renders the stale-cache answer (§4: stamp mismatch ⇒
// state:"unknown" + enqueued recompute; neither blocks a read on git).
func unknownMergeable(pr *PRDoc, at string) *MergeableDoc {
	return &MergeableDoc{
		BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA, HeadSHA: pr.Head.SHA,
		MergeBase: "", State: MergeableUnknown,
		Conflicts: []string{}, Rebaseable: true, ComputedAt: at,
	}
}

// saveMergeable CAS-writes a mergeable.json cache (derived state — losing
// the race only costs a recompute: a 412 loser re-runs after the winner and
// converges). Bounded at 5 attempts.
func (s *Service) saveMergeable(ctx context.Context, owner, repo string, num int, m *MergeableDoc) error {
	_, err := s.casUpdate(ctx, MergeableKey(owner, repo, num), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return encodeMergeable(m), true, nil
	})
	return err
}

// ComputeMergeable recomputes the mergeability stamp (§4) with the doc-04
// machinery: ancestry (`merge-base --is-ancestor` both directions), the
// trial merge (`merge-tree --write-tree --name-only`), and behind
// (`rev-list --count head..base`). All git runs go through the bounded
// per-repo pool (never bare on request goroutines). Recompute goes through
// the in-process single-flight keyed "mergeable:"+repo+"/"+num — joiners get
// the same result (bounded join). The result is CAS-written with the fresh
// stamp; no lock is held across any store call or git subprocess.
//
// Cost model (EVIDENCE.md E4): 0 LIST, 2 ref resolves (pool-gated) + 1
// merge-base + 1 merge-tree + 1 rev-list --count, 2 bucket GETs + 1 CAS PUT.
func (s *Service) ComputeMergeable(ctx context.Context, owner, repo string, num int) (*MergeableDoc, error) {
	if s.Git == nil || s.Dirs == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	v, err := s.flight.Do(ctx, "mergeable:"+repoName(owner, repo)+"/"+itoa(num), func() (any, error) {
		return s.computeMergeable(ctx, owner, repo, num)
	})
	if err != nil {
		return nil, err
	}
	m, ok := v.(*MergeableDoc)
	if !ok || m == nil {
		return nil, fmt.Errorf("%w: mergeable recompute produced no result", ErrCorrupt)
	}
	return m, nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// computeMergeable is the single-flight leader body.
func (s *Service) computeMergeable(ctx context.Context, owner, repo string, num int) (*MergeableDoc, error) {
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
	if pr.Merged || th.State == StateClosed {
		m := &MergeableDoc{
			BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA, HeadSHA: pr.Head.SHA,
			MergeBase: "", State: MergeableUpToDate,
			Conflicts: []string{}, Rebaseable: true, ComputedAt: s.nowUTC().Format(dateTimeFmt),
		}
		_ = s.saveMergeable(ctx, owner, repo, num, m)
		return m, nil
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	// Live shas: the stamp triple (base_ref, base_sha, head_sha) IS the
	// invalidation key (§2.2).
	baseLive, err := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, pr.Base.Ref)
	}
	headLive, err := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if err != nil {
		m := unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		m.BaseSHA, m.HeadSHA = baseLive, pr.Head.SHA
		return m, nil
	}
	if headLive != pr.Head.SHA {
		s.refreshHead(ctx, owner, repo, pr, th, headLive, auth.Principal{})
		if npr, _, nerr := s.loadPR(ctx, owner, repo, num); nerr == nil && npr != nil {
			pr = npr
		}
	}
	// Ancestry: head fully merged into base (head is an ancestor of base)
	// ⇒ up_to_date, nothing to merge. (The base-contained-in-head
	// direction is the NORMAL PR shape — head branched off base — and
	// proceeds to the trial merge below; see the §4 deviation note in
	// docs/features/03: the first ancestry parenthetical there would refuse
	// every ordinary PR, so the implementation follows the second clause,
	// which names the genuinely terminal condition.)
	merged, err := s.Git.IsAncestor(ctx, baseDir, headLive, baseLive)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	if merged {
		m := &MergeableDoc{BaseRef: pr.Base.Ref, BaseSHA: baseLive, HeadSHA: headLive,
			MergeBase: headLive, State: MergeableUpToDate,
			Conflicts: []string{}, Rebaseable: true, ComputedAt: now}
		_ = s.saveMergeable(ctx, owner, repo, num, m)
		return m, nil
	}
	mb, err := s.Git.MergeBase(ctx, baseDir, baseLive, headLive)
	if err != nil {
		return nil, err
	}
	tree, conflicts, terr := s.Git.TrialMerge(ctx, baseDir, baseLive, headLive)
	_ = tree
	m := &MergeableDoc{BaseRef: pr.Base.Ref, BaseSHA: baseLive, HeadSHA: headLive,
		MergeBase: mb, Conflicts: []string{}, Rebaseable: true, ComputedAt: now}
	if terr == nil {
		behind, berr := s.Git.BehindCount(ctx, baseDir, baseLive, headLive)
		if berr != nil {
			return nil, berr
		}
		if behind > 0 {
			m.State = MergeableBehind
		} else {
			m.State = MergeableClean
		}
	} else if errors.Is(terr, errDirty) {
		m.State = MergeableDirty
		m.Conflicts = nonNilStr(conflicts)
		m.Rebaseable = len(conflicts) == 0
	} else {
		return nil, terr
	}
	_ = s.saveMergeable(ctx, owner, repo, num, m)
	return m, nil
}

// refreshHead records head-sha drift (§6): CASes pr.json to the live sha,
// stamps head_force_pushed_at, and appends a head_force_pushed event when
// the move is not a fast-forward (old !ancestor-of new). Review threads
// survive (doc 04 anchors render "outdated", never block). Best-effort:
// failures degrade the read to unknown, never a 500.
func (s *Service) refreshHead(ctx context.Context, owner, repo string, pr *PRDoc, th *Thread, headLive string, actor auth.Principal) {
	if headLive == "" || headLive == pr.Head.SHA {
		return
	}
	old := pr.Head.SHA
	forced := true
	if s.Git != nil && s.Dirs != nil && old != "" {
		if dir, derr := s.Dirs.Dir(ctx, pr.Head.Repo); derr == nil {
			if ff, ferr := s.Git.IsAncestor(ctx, dir, old, headLive); ferr == nil && ff {
				forced = false
			}
		}
	}
	now := s.nowUTC().Format(dateTimeFmt)
	pr.Head.SHA = headLive
	if forced {
		pr.HeadForcePushedAt = &now
	}
	_, prVer, _ := s.loadPR(ctx, owner, repo, pr.Num)
	_ = s.savePR(ctx, owner, repo, pr, prVer)
	if forced {
		who := normPrincipal(actor.Name)
		if who == "" {
			who = "pulls-sink"
		}
		_, _, _ = s.appendEvent(ctx, owner, repo, pr.Num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventHeadForcePushed, Actor: who, At: now, From: strPtr(old), To: strPtr(headLive)}, nil
		})
		if nth, _, nerr := s.loadThread(ctx, owner, repo, pr.Num); nerr == nil && nth != nil {
			*th = *nth
		}
		s.stream(ctx, StreamEvent{Name: "pull", Repo: repoName(owner, repo), Action: "head_force_pushed", Num: pr.Num, Title: th.Title, State: th.State, Author: th.Author, BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: headLive})
	}
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	s.enqueueMergeable(ctx, owner, repo, pr.Num)
}

// enqueueMergeable schedules a `pull-mergeable` recompute for one PR without
// blocking the reader: a detached background pass under the (repo, kind)
// task join (a second enqueue joins the running pass and reuses its
// outcome). Both paths converge; neither blocks a read on git.
func (s *Service) enqueueMergeable(ctx context.Context, owner, repo string, num int) {
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(repoID, TaskKindMergeable)
	entry.attach(num)
	if joined {
		return
	}
	go func() {
		defer s.tasks.end(repoID, TaskKindMergeable)
		bctx := context.WithoutCancel(ctx)
		for _, n := range entry.drain() {
			_, _ = s.ComputeMergeable(bctx, owner, repo, n)
		}
	}()
}

// --- the `pulls` event sink (§4 invalidation seam) -----------------------------

// RefEvent is one WAL ref event as the sink consumes it (Seam 4 shape,
// reduced to the fields the invalidation needs).
type RefEvent struct {
	Repo    string // "owner/name"
	RefName string
	Old     string
	New     string
}

// HandleRefEvent consumes one WAL ref event (Seam 4 `pulls` sink, per-sink
// cursor repos/<o>/<r>/events/cursors/pulls.json owned by internal/events).
// For each event whose ref_name matches an open PR's base.ref or head.ref
// (looked up from the shared issues/index.json filtered to kind:"pr" — the
// index is authoritative for open items per P4), the PR num is enqueued onto
// the recompute batch (task kind pull-mergeable, (repo, kind) single-flight
// batches all dirty PRs of the repo into one pass). Head drift also runs
// the §6 refresh (force-push evidence + sha snapshot).
//
// Cost: 1 index GET + 1 GET per open PR sidecar (index-authoritative, no
// LIST); git only on the recompute path (pool-gated).
func (s *Service) HandleRefEvent(ctx context.Context, owner, repo string, ev RefEvent) {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return
	}
	var dirty []int
	for _, c := range ix.Open {
		if c.Kind != "pr" {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil || pr.Merged {
			continue
		}
		matched := false
		if pr.Base.Repo == ev.Repo && pr.Base.Ref == ev.RefName {
			matched = true
		}
		if pr.Head.Repo == ev.Repo && pr.Head.Ref == ev.RefName {
			matched = true
			if ev.New != "" && ev.New != pr.Head.SHA {
				th, _, terr := s.loadThread(ctx, owner, repo, pr.Num)
				if terr == nil && th != nil {
					s.refreshHead(ctx, owner, repo, pr, th, ev.New, auth.Principal{})
				}
			}
		}
		if matched {
			dirty = append(dirty, pr.Num)
		}
	}
	if len(dirty) == 0 {
		return
	}
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(repoID, TaskKindMergeable)
	for _, n := range dirty {
		entry.attach(n)
	}
	if joined {
		return
	}
	go func() {
		defer s.tasks.end(repoID, TaskKindMergeable)
		bctx := context.WithoutCancel(ctx)
		for _, n := range entry.drain() {
			_, _ = s.ComputeMergeable(bctx, owner, repo, n)
		}
	}()
}

// --- diff / commits ------------------------------------------------------------

// diffDir selects the git dir for base...head reads: the base repo dir when
// the head sha is reachable there, else the fork dir (shared packs make it
// local, §7). One pool-gated reachability probe, no LIST.
func (s *Service) diffDir(ctx context.Context, pr *PRDoc) (string, error) {
	if s.Git == nil || s.Dirs == nil {
		return "", fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return "", fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	if pr.Head.Repo == pr.Base.Repo {
		return baseDir, nil
	}
	ok, err := s.Git.Reachable(ctx, baseDir, pr.Head.SHA)
	if err != nil {
		return "", fmt.Errorf("%w: reachability check: %v", ErrUnavailable, err)
	}
	if ok {
		return baseDir, nil
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return "", fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	return headDir, nil
}

// Diff returns the text/plain unified diff base…head (§9.5). Auth: read.
func (s *Service) Diff(ctx context.Context, owner, repo string, num int, p auth.Principal) (string, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return "", err
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return "", err
	}
	if pr == nil {
		if th, _, terr := s.loadThread(ctx, owner, repo, num); terr != nil || th == nil || th.Kind != "pr" {
			return "", fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		return "", fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	dir, err := s.diffDir(ctx, pr)
	if err != nil {
		return "", err
	}
	return s.Git.Diff(ctx, dir, pr.Base.SHA, pr.Head.SHA)
}

// Commits returns {commits, more} of base..head (doc 07 Commit shape,
// skip/n pagination). Auth: read.
func (s *Service) Commits(ctx context.Context, owner, repo string, num int, p auth.Principal, skip, n int) ([]CommitEntry, bool, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, false, err
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, false, err
	}
	if pr == nil {
		return nil, false, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	dir, err := s.diffDir(ctx, pr)
	if err != nil {
		return nil, false, err
	}
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	if skip < 0 {
		skip = 0
	}
	rows, err := s.Git.LogRange(ctx, dir, pr.Base.SHA, pr.Head.SHA, skip, n+1)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > n
	if more {
		rows = rows[:n]
	}
	return rows, more, nil
}

// threadTitleOf renders "owner/repo#num" for narration.
func threadTitleOf(owner, repo string, num int) string {
	return repoName(owner, repo) + "#" + itoa(num)
}
