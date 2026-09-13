package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns §5 (the pull-merge task), §7 (the pull-fork task), and the
// pull-update-branch task: strategy argv, protected-ref gates, WAL publish
// (never force), P3/P4/P8 commit, and head cleanup.

// MergeInput shapes POST …/pulls/{num}/merge (§8): {strategy,
// commit_title?, commit_message?, delete_head?}.
type MergeInput struct {
	Strategy      string
	CommitTitle   string
	CommitMessage string
	DeleteHead    bool
}

// StartMerge starts (or joins) the pull-merge task (§5: narrated, P7 unique
// id, progress packets, SSE attach via GET, (repo, kind) single-flight — a
// second start JOINS the running one and reuses its outcome). Auth:
// maintain or above (P6). Returns the shared task record immediately (202 +
// poll — the handler never blocks on git).
func (s *Service) StartMerge(ctx context.Context, owner, repo string, num int, actor auth.Principal, in MergeInput, correlationID string) (*TaskRecord, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "maintain"); err != nil {
		return nil, err
	}
	if err := validateStrategy(in.Strategy); err != nil {
		return nil, err
	}
	if s.Git == nil || s.Dirs == nil || s.Refs == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(repoID, TaskKindMerge)
	if joined {
		return entry.rec.snapshot(), nil
	}
	entry.rec.initMerge(num, in.Strategy)
	entry.rec.notice("merge queued for %s by %s (strategy %s)", threadTitleOf(owner, repo, num), normPrincipal(actor.Name), in.Strategy)
	go func() {
		defer s.tasks.end(repoID, TaskKindMerge)
		bctx := context.WithoutCancel(ctx)
		outcome, rerr := s.runMerge(bctx, owner, repo, num, actor, in, correlationID, entry.rec)
		if rerr != nil {
			entry.rec.setState(TaskError, "", rerr.Error(), nil)
			entry.rec.notice("merge failed: %s", rerr.Error())
			entry.err = rerr
			return
		}
		entry.rec.setState(TaskOK, fmt.Sprintf("merged %s as %s", threadTitleOf(owner, repo, num), outcome["sha"]), "", outcome)
	}()
	return entry.rec.snapshot(), nil
}

// MergeTask returns the running pull-merge record, if any (SSE attach/poll).
func (s *Service) MergeTask(owner, repo string) *TaskRecord {
	return s.tasks.get(repoName(owner, repo), TaskKindMerge)
}

// runMerge executes the §5 steps. No repo lock is held across the git
// subprocesses (13 §2 rule 4); the task holds no syncMu/packMu/rw — it goes
// through the same Dir/pool path as any reader. Arbitration is the task
// single-flight plus the WAL publish CAS.
func (s *Service) runMerge(ctx context.Context, owner, repo string, num int, actor auth.Principal, in MergeInput, correlationID string, rec *TaskRecord) (map[string]any, error) {
	who := normPrincipal(actor.Name)
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
	if pr.Merged {
		return nil, fmt.Errorf("%w: pull request #%d already merged as %s", ErrConflict, num, strOrEmpty(pr.MergeCommitSHA))
	}
	if th.State == StateClosed {
		return nil, fmt.Errorf("%w: pull request #%d is closed", ErrConflict, num)
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	// Step 1: re-verify under the task — re-resolve live shas; refresh the
	// mergeability stamp first when either moved since pr.json.
	rec.notice("resolving %s and %s", pr.Base.Ref, pr.Head.Ref)
	baseLive, err := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, pr.Base.Ref)
	}
	headLive, err := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown head revision %q", ErrUnprocessable, pr.Head.Ref)
	}
	if headLive != pr.Head.SHA {
		s.refreshHead(ctx, owner, repo, pr, th, headLive, actor)
		if npr, _, nerr := s.loadPR(ctx, owner, repo, num); nerr == nil && npr != nil {
			pr = npr
		}
	}
	m, err := s.ComputeMergeable(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	// Refuse dirty/up_to_date with a narrated reason (step 1).
	switch m.State {
	case MergeableDirty:
		rec.notice("refusing: conflicts in %s", strings.Join(m.Conflicts, ", "))
		return nil, fmt.Errorf("%w: pull request #%d has conflicts", ErrConflict, num)
	case MergeableUpToDate:
		return nil, fmt.Errorf("%w: pull request #%d is already merged (up to date)", ErrConflict, num)
	}
	rec.notice("mergeability %s (base %s, head %s)", m.State, shortSHA(baseLive), shortSHA(headLive))
	// Step 4: protected-ref gate — explicitly evaluate policy.json for
	// (merger, base.ref, update). The required-checks gate is consulted
	// next, when a rule carries it (05 §6 merge-time half).
	if gerr := s.checkProtectedRef(ctx, owner, repo, who, pr.Base.Ref, "update"); gerr != nil {
		rec.notice("protected-ref gate: %s", gerr.Error())
		return nil, fmt.Errorf("%w: %v", ErrConflict, gerr)
	}
	// Required-checks gate (05 §6 merge-time half): the checks-provided
	// gate function with the LIVE head sha (it reads the stored combined
	// view — it never trusts a cache). A failed gate narrates the
	// shortfall (law 7) and the merge ref is not published. Nil Checks
	// fails closed only when a rule actually carries the gate.
	if gerr := s.checkRequiredChecksGate(ctx, owner, repo, headLive, pr.Base.Ref, who); gerr != nil {
		rec.notice("required-checks gate: %s", gerr.Error())
		return nil, fmt.Errorf("%w: %v", ErrConflict, gerr)
	}
	rec.notice("required-checks gate: satisfied")
	// Required-reviews gate (04 §6 merge-time half): the review-provided
	// gate function with the LIVE head sha (it re-derives by event scan —
	// it never trusts review_summary). A failed gate narrates the
	// shortfall (law 7) and the merge ref is not published. Nil Reviews
	// skips the gate (no review backend wired).
	if s.Reviews != nil {
		if gerr := s.Reviews.CheckRequiredReviews(ctx, owner, repo, num, headLive, pr.Base.Ref, who); gerr != nil {
			rec.notice("required-reviews gate: %s", gerr.Error())
			return nil, fmt.Errorf("%w: %v", ErrConflict, gerr)
		}
		rec.notice("required-reviews gate: satisfied")
	}
	// Step 2: strategy argv (stock git only, plumbing — no worktree).
	authorName, authorEmail := who, who
	cName, cEmail := s.committer()
	now := s.nowUTC()
	var newSHA, title, body string
	var commitTexts []string
	switch in.Strategy {
	case StrategyMerge, StrategySquash:
		rec.notice("trial merge %s...%s", shortSHA(baseLive), shortSHA(headLive))
		tree, conflicts, terr := s.Git.TrialMerge(ctx, baseDir, baseLive, headLive)
		if terr != nil {
			if errors.Is(terr, errDirty) {
				rec.notice("conflicts in %s", strings.Join(conflicts, ", "))
				return nil, fmt.Errorf("%w: pull request #%d has conflicts", ErrConflict, num)
			}
			return nil, terr
		}
		subject, _ := s.Git.Subject(ctx, headDir, headLive)
		title, body = mergeMessage(in.Strategy, num, pr.Head.Ref, subject, "", in.CommitTitle, in.CommitMessage)
		parents := []string{baseLive, headLive}
		if in.Strategy == StrategySquash {
			parents = []string{baseLive}
		}
		rec.notice("commit-tree (%s)", in.Strategy)
		newSHA, err = s.Git.CommitTree(ctx, baseDir, tree, parents, fullMessage(title, body), authorName, authorEmail, cName, cEmail, now)
		if err != nil {
			return nil, err
		}
		commitTexts = []string{fullMessage(title, body)}
	case StrategyRebase:
		rec.notice("replay %s onto %s", shortSHA(headLive), shortSHA(baseLive))
		mb, merr := s.Git.MergeBase(ctx, baseDir, baseLive, headLive)
		if merr != nil {
			return nil, merr
		}
		_ = mb
		newSHA, err = s.Git.Replay(ctx, baseDir, baseLive, baseLive, headLive, cName, cEmail)
		if err != nil {
			return nil, err
		}
		rows, _ := s.Git.LogRange(ctx, headDir, baseLive, headLive, 0, 100)
		title, body = mergeMessage(in.Strategy, num, pr.Head.Ref, firstSubject(rows), "", in.CommitTitle, in.CommitMessage)
		for _, r := range rows {
			commitTexts = append(commitTexts, r.Subject)
		}
		// Default-branch merges of protected refs append (<full sha>)
		// per GitHub convention — the UI renders the sha link.
		title += fmt.Sprintf(" (%s)", newSHA)
	default:
		return nil, fmt.Errorf("%w: strategy must be merge|squash|rebase", ErrInvalid)
	}
	rec.notice("publishing %s → %s", pr.Base.Ref, shortSHA(newSHA))
	// Step 5: publish — the base ref move PLUS the server-made objects in
	// ONE WAL entry (the merged ref never dangles bucket-missing objects:
	// commit-tree/replay mint objects only the serving copy holds, so the
	// tip is packed here and uploaded atomically with the txn). The WAL
	// publish CAS arbitrates against concurrent pushes: a moved base loses
	// the CAS and the task re-plans once, else fails loudly. NEVER
	// force-publishes.
	meta := map[string]string{"principal": who, "agent": "pulls"}
	if correlationID != "" {
		meta["correlation_id"] = correlationID
	}
	if perr := s.publishWithObjects(ctx, pr.Base.Repo, pr.Base.Ref, baseLive, newSHA, baseDir, meta, rec, "merge"); perr != nil {
		if isCASConflict(perr) {
			rec.notice("base moved under the merge; re-planning once")
			baseRelive, rerr := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
			if rerr != nil {
				return nil, fmt.Errorf("%w: base %q moved and is now unresolvable", ErrConflict, pr.Base.Ref)
			}
			if baseRelive == baseLive {
				return nil, fmt.Errorf("%w: publish conflicted; retry the merge", ErrConflict)
			}
			m2, merr := s.ComputeMergeable(ctx, owner, repo, num)
			if merr != nil || m2.State == MergeableDirty || m2.State == MergeableUpToDate {
				return nil, fmt.Errorf("%w: base moved; merge no longer clean", ErrConflict)
			}
			// Recompute the commit onto the moved base (merge/squash only;
			// rebase replays are base-relative already — still recompute
			// for a fresh stamp).
			return nil, fmt.Errorf("%w: base moved; merge re-planned — retry", ErrConflict)
		}
		return nil, perr
	}
	// Step 6: commit (P3/P4/P8) — merged event (state → closed,
	// merged:true), CAS pr.json, shared index, the PR-closing cross-ref via
	// 02's ApplyClosingReferences seam (keyword list + event shapes owned
	// there; 03 only supplies merged head sha + title/body), fan-out.
	mergedAt := s.nowUTC().Format(dateTimeFmt)
	nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
		t.NextEventSeq = seq + 1
		t.UpdatedAt = mergedAt
		t.State = StateClosed
		t.Participants = uniqSorted(append(t.Participants, who))
		t.Version++
		return &Event{Seq: seq, Type: EventMerged, Actor: who, At: mergedAt,
			MergeCommitSHA: strPtr(newSHA), Strategy: strPtr(in.Strategy)}, nil
	})
	if aerr != nil {
		// The ref published but the event failed: the merge LANDED (the
		// bucket records converge via the pr.json CAS below + the sink
		// recompute). Narrate loudly; do not roll back the publish.
		rec.notice("warning: base published but the merged event failed: %s", aerr.Error())
	}
	if nt != nil {
		th = nt
	}
	// Owned-delta re-apply (§2.3: this writer owns the outcome fields
	// only): record onto the fresh doc. A body edit or head refresh may
	// have landed while the task ran — saving the task-start snapshot
	// wholesale with a fresh version would clobber it with no 412.
	target := pr
	fresh, prVer, _ := s.loadPR(ctx, owner, repo, num)
	if fresh != nil {
		target = fresh
	}
	target.Merged = true
	target.MergedAt = &mergedAt
	target.MergedBy = &who
	target.MergeCommitSHA = &newSHA
	target.MergeStrategy = &in.Strategy
	_ = s.savePR(ctx, owner, repo, target, prVer)
	pr = target
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	texts := append([]string{pr.Body, fullMessage(title, body)}, commitTexts...)
	var closed []int
	if s.Closer != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		closed, _ = s.Closer.ApplyClosingReferences(cctx, owner, repo, num, newSHA, who, texts)
		cancel()
	}
	if closed == nil {
		closed = []int{}
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "merged", Actor: who, PullNum: num, Recipients: prParticipants(th, who)})
	s.stream(ctx, StreamEvent{Name: "pull", Repo: repoName(owner, repo), Action: "merged", Num: num, Title: th.Title, State: th.State, Author: th.Author, BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: newSHA})
	rec.notice("merged as %s; closed issues %v", shortSHA(newSHA), closed)
	// Step 7: head cleanup — same-repo heads only (fork heads are never
	// deleted by the base repo), policy-checked like any ref delete.
	if in.DeleteHead && pr.Head.Repo == pr.Base.Repo {
		if derr := s.checkProtectedRef(ctx, owner, repo, who, pr.Head.Ref, "delete"); derr != nil {
			rec.notice("head cleanup skipped: %s", derr.Error())
		} else if derr := s.Refs.DeleteRef(ctx, pr.Head.Repo, pr.Head.Ref, meta); derr != nil {
			rec.notice("head cleanup failed: %s", derr.Error())
		} else {
			rec.notice("deleted %s", pr.Head.Ref)
		}
	}
	return map[string]any{"sha": newSHA, "strategy": in.Strategy, "closed_issues": closed}, nil
}

// firstSubject returns the first row's subject (rebase title fallback).
func firstSubject(rows []CommitEntry) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Subject
}

// shortSHA renders the first 12 hex chars (narration only, never storage).
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// isCASConflict reports a publish CAS loss (moved base): the failure the
// task re-plans on, once, instead of force-publishing.
func isCASConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") || strings.Contains(msg, "cas") ||
		strings.Contains(msg, "changed concurrently") || strings.Contains(msg, "precondition")
}

// --- update-branch ------------------------------------------------------------

// UpdateBranchInput shapes POST …/pulls/{num}/update-branch (§8):
// {expected_head_sha?} → task pull-update-branch (merge base→head; 409 if
// dirty or sha mismatch). Auth: write.
func (s *Service) UpdateBranch(ctx context.Context, owner, repo string, num int, actor auth.Principal, expectedHeadSHA string) (*TaskRecord, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "write"); err != nil {
		return nil, err
	}
	if s.Git == nil || s.Dirs == nil || s.Refs == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
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
	if pr.Merged || th.State == StateClosed {
		return nil, fmt.Errorf("%w: pull request #%d is closed", ErrConflict, num)
	}
	if expectedHeadSHA != "" && expectedHeadSHA != pr.Head.SHA {
		return nil, fmt.Errorf("%w: expected_head_sha mismatch", ErrConflict)
	}
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(repoID, TaskKindUpdateBranch)
	if joined {
		return entry.rec.snapshot(), nil
	}
	entry.rec.initMerge(num, "")
	go func() {
		defer s.tasks.end(repoID, TaskKindUpdateBranch)
		bctx := context.WithoutCancel(ctx)
		sha, rerr := s.runUpdateBranch(bctx, owner, repo, num, actor, entry.rec)
		if rerr != nil {
			entry.rec.setState(TaskError, "", rerr.Error(), nil)
			entry.rec.notice("update-branch failed: %s", rerr.Error())
			entry.err = rerr
			return
		}
		entry.rec.setState(TaskOK, fmt.Sprintf("updated branch of %s to %s", threadTitleOf(owner, repo, num), shortSHA(sha)), "", map[string]any{"sha": sha})
	}()
	return entry.rec.snapshot(), nil
}

// runUpdateBranch merges base into head (the reverse of the merge task):
// trial merge-tree with base=headSHA, head=baseSHA, commit parents
// [headSHA, baseSHA], publish head update old=headSHA. 409 if dirty.
func (s *Service) runUpdateBranch(ctx context.Context, owner, repo string, num int, actor auth.Principal, rec *TaskRecord) (string, error) {
	who := normPrincipal(actor.Name)
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return "", err
	}
	if pr == nil {
		return "", fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return "", fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return "", fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	baseLive, err := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	if err != nil {
		return "", fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, pr.Base.Ref)
	}
	headLive, err := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if err != nil {
		return "", fmt.Errorf("%w: unknown head revision %q", ErrUnprocessable, pr.Head.Ref)
	}
	contained, err := s.Git.IsAncestor(ctx, headDir, baseLive, headLive)
	if err != nil {
		return "", err
	}
	if contained {
		return headLive, nil // already up to date — no-op success
	}
	tree, conflicts, terr := s.Git.TrialMerge(ctx, headDir, headLive, baseLive)
	if terr != nil {
		if errors.Is(terr, errDirty) {
			return "", fmt.Errorf("%w: update-branch conflicts in %s", ErrConflict, strings.Join(conflicts, ", "))
		}
		return "", terr
	}
	cName, cEmail := s.committer()
	now := s.nowUTC()
	msg := fmt.Sprintf("Merge %s into %s", pr.Base.Ref, pr.Head.Ref)
	newSHA, err := s.Git.CommitTree(ctx, headDir, tree, []string{headLive, baseLive}, msg, who, who, cName, cEmail, now)
	if err != nil {
		return "", err
	}
	meta := map[string]string{"principal": who, "agent": "pulls"}
	if perr := s.publishWithObjects(ctx, pr.Head.Repo, pr.Head.Ref, headLive, newSHA, headDir, meta, rec, "update-branch"); perr != nil {
		if isCASConflict(perr) {
			return "", fmt.Errorf("%w: head moved; retry", ErrConflict)
		}
		return "", perr
	}
	th, _, _ := s.loadThread(ctx, owner, repo, num)
	if th != nil {
		s.refreshHead(ctx, owner, repo, pr, th, newSHA, actor)
	}
	rec.notice("updated %s to %s", pr.Head.Ref, shortSHA(newSHA))
	return newSHA, nil
}

// --- delete-head ---------------------------------------------------------------

// DeleteHead deletes the head branch post-merge (policy-checked like any
// ref delete). Auth: maintain. Only merged PRs; fork heads are never
// deleted by the base repo (404-class refusal, not a cross-repo write).
func (s *Service) DeleteHead(ctx context.Context, owner, repo string, num int, actor auth.Principal) error {
	if err := requireAuthenticated(actor); err != nil {
		return err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "maintain"); err != nil {
		return err
	}
	if s.Refs == nil {
		return fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return err
	}
	if th == nil || th.Kind != "pr" {
		return fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return err
	}
	if pr == nil {
		return fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	if !pr.Merged {
		return fmt.Errorf("%w: pull request #%d is not merged", ErrConflict, num)
	}
	if pr.Head.Repo != pr.Base.Repo {
		return fmt.Errorf("%w: fork heads are never deleted by the base repo", ErrForbidden)
	}
	who := normPrincipal(actor.Name)
	if derr := s.checkProtectedRef(ctx, owner, repo, who, pr.Head.Ref, "delete"); derr != nil {
		return fmt.Errorf("%w: %v", ErrConflict, derr)
	}
	meta := map[string]string{"principal": who, "agent": "pulls"}
	return s.Refs.DeleteRef(ctx, pr.Head.Repo, pr.Head.Ref, meta)
}

// --- fork ----------------------------------------------------------------------

// ForkInput shapes POST /api/v1/repos/{owner}/{repo}/forks (§8):
// {target_owner?, name?, visibility?, branch?, description?} → 202 +
// TaskRecord (pull-fork). Auth: write on the source + create rights on
// the target (the #346 owner-admission gate — self or member org, host
// admin bypass; nil gate = legacy-open). Field semantics:
//   - visibility: public|authenticated|private ("" = public, the repo-create
//     default); threaded to EnsureRepoAccess for the child's access.json.
//   - branch: starting branch for the child's Head — "" = parent Head, a
//     short name selects refs/heads/<name>, a full refs/heads/<...> ref is
//     accepted verbatim. refs/tags/* and other namespaces are rejected
//     explicitly (out of scope: tag-anchored forks), as is a branch missing
//     from the parent (422 under the task).
//   - description: optional short display string (≤ 1024 bytes, no C0
//     controls); threaded into the child manifest's inline settings TOML
//     atomically at Create — the same store the summary description reads,
//     so no post-create write is needed.
type ForkInput struct {
	TargetOwner string
	Name        string
	Visibility  string
	Branch      string
	Description string
}

// MaxForkDescription bounds the fork description (the settings TOML the
// child manifest carries inline is ≤ 16 KiB; the description is one key).
const MaxForkDescription = 1024

// ForkOptions carries the resolved fork parameters into the manifest step.
type ForkOptions struct {
	// Branch is the full child HEAD ref ("" = parent HEAD).
	Branch string
	// Description is the trimmed display string ("" = none).
	Description string
	// Creator is the forking principal (settings authorship).
	Creator string
}

// ForkExecutor performs the manifest-sharing step (§7: the fork's
// manifest.pb references the parent's pack set verbatim plus a fresh refs
// snapshot, in already-on-bucket mode; Create on the child manifest
// arbitrates the target name exactly like repo create). A 412-class
// failure surfaces as ErrConflict so the caller can run the adopt check.
// RollbackShare releases a share reservation won by THIS attempt when a
// later pre-commit step fails (issue #432); see runFork.
type ForkExecutor interface {
	ShareManifest(ctx context.Context, parent, child string, opt ForkOptions) error
	// RollbackShare deletes exactly the child-prefix keys ShareManifest
	// Creates (child manifest.pb, its checkpoint objects, and the
	// bootstrapped access.json when no fork.json disputes the prefix),
	// and only when the child manifest is provably still this attempt's
	// (Repo == child, Revision == 1). Anything else — absent, foreign, or
	// WAL-advanced manifest — refuses without deleting (absent is a nil
	// no-op). Failure-path only: never packs, never parent keys, never
	// the parent index (a rolled-back child was never listed, so the
	// fork-network GC walk in internal/maintain cannot reference it; a
	// racing pass reads manifest-404 and skips the subtree).
	RollbackShare(ctx context.Context, parent, child string) error
}

// OwnerGate is the creation owner-admission gate the fork target inherits
// (Forgejo #346, mirroring the repo-create path): the target owner must
// equal the principal's own username or be an org the principal belongs
// to; host admins bypass. Satisfied by *identity.Service; nil skips the
// check (legacy-open, tests without the identity surface).
type OwnerGate interface {
	CheckCreateOwner(ctx context.Context, owner string, p auth.Principal) *auth.AuthError
}

// AccessBootstrapper materializes the child's access.json at fork time
// (the placeholder-create path's EnsureRepoAccess, Forgejo #210 §3):
// Create-wins, adopt-don't-overwrite. Satisfied by *identity.Service;
// nil skips the write (read-time synthesis only).
type AccessBootstrapper interface {
	EnsureRepoAccess(ctx context.Context, owner, repo, creator, visibility string) error
}

// StartFork starts (or joins) the pull-fork task.
func (s *Service) StartFork(ctx context.Context, owner, repo string, actor auth.Principal, in ForkInput) (*TaskRecord, string, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, "", err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, "", err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "write"); err != nil {
		return nil, "", err
	}
	targetOwner := strings.ToLower(strings.TrimSpace(in.TargetOwner))
	if targetOwner == "" {
		targetOwner = strings.ToLower(owner)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = repo + "-fork"
	}
	if err := validateRepoPart(targetOwner); err != nil {
		return nil, "", fmt.Errorf("%w: invalid target_owner %q", ErrInvalid, in.TargetOwner)
	}
	if err := validateRepoPart(name); err != nil {
		return nil, "", fmt.Errorf("%w: invalid name %q", ErrInvalid, in.Name)
	}
	visibility := strings.ToLower(strings.TrimSpace(in.Visibility))
	if visibility == "" {
		visibility = "public"
	}
	switch visibility {
	case "public", "authenticated", "private":
	default:
		return nil, "", fmt.Errorf("%w: visibility must be public|authenticated|private, got %q", ErrInvalid, in.Visibility)
	}
	branch, err := normalizeForkBranch(in.Branch)
	if err != nil {
		return nil, "", err
	}
	description := strings.TrimSpace(in.Description)
	if len(description) > MaxForkDescription {
		return nil, "", fmt.Errorf("%w: description exceeds %d bytes", ErrInvalid, MaxForkDescription)
	}
	for i := 0; i < len(description); i++ {
		c := description[i]
		if c < 0x20 && c != '\n' && c != '\t' {
			return nil, "", fmt.Errorf("%w: description must not contain control characters", ErrInvalid)
		}
	}
	if s.OwnerGate != nil {
		if gerr := s.OwnerGate.CheckCreateOwner(ctx, targetOwner, actor); gerr != nil {
			return nil, "", gerr
		}
	}
	child := repoName(targetOwner, name)
	// Fail fast on a taken name (the form surfaces this inline): fork.json
	// provenance claims the name, and a child manifest (real repo or a
	// completed fork) occupies it. Either present → sync 409. The CAS
	// steps in runFork still arbitrate races (the loser fails loud under
	// the task); this probe is advisory, never authoritative.
	if raw, _, _ := s.getJSON(ctx, ForkKey(targetOwner, name)); raw != nil {
		return nil, "", fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
	}
	if _, ver, _ := s.getJSON(ctx, manifestKey(targetOwner, name)); ver != "" {
		return nil, "", fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
	}
	repoID := repoName(owner, repo)
	resolved := ForkInput{TargetOwner: targetOwner, Name: name, Visibility: visibility, Branch: branch, Description: description}
	entry, joined := s.tasks.begin(child, TaskKindFork)
	if joined {
		return entry.rec.snapshot(), child, nil
	}
	entry.rec.notice("fork %s → %s queued by %s", repoID, child, normPrincipal(actor.Name))
	go func() {
		defer s.tasks.end(child, TaskKindFork)
		bctx := context.WithoutCancel(ctx)
		if rerr := s.runFork(bctx, owner, repo, resolved, actor, entry.rec); rerr != nil {
			entry.rec.setState(TaskError, "", rerr.Error(), nil)
			entry.rec.notice("fork failed: %s", rerr.Error())
			entry.err = rerr
			return
		}
		entry.rec.setState(TaskOK, fmt.Sprintf("forked %s → %s", repoID, child), "", nil)
	}()
	return entry.rec.snapshot(), child, nil
}

// normalizeForkBranch resolves the branch input to a full child-HEAD ref:
// "" stays "" (parent Head), a short name selects refs/heads/<name>, a full
// refs/heads/<...> ref passes through. Anything else (tags, HEAD, other
// namespaces, bad shapes) is rejected explicitly — out of scope, never
// silently ignored.
func normalizeForkBranch(branch string) (string, error) {
	b := strings.TrimSpace(branch)
	if b == "" {
		return "", nil
	}
	if strings.HasPrefix(b, "refs/") {
		if !strings.HasPrefix(b, "refs/heads/") || len(b) <= len("refs/heads/") {
			return "", fmt.Errorf("%w: branch must be a branch (refs/heads/...), got %q", ErrInvalid, branch)
		}
		if err := validateRefName(b); err != nil {
			return "", err
		}
		return b, nil
	}
	full := "refs/heads/" + b
	if err := validateRefName(full); err != nil {
		return "", fmt.Errorf("%w: invalid branch %q", ErrInvalid, branch)
	}
	return full, nil
}

// manifestKey returns the bucket-relative manifest.pb key for owner/repo
// (the fork pre-check probes it; the manifest step Creates it).
func manifestKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/manifest.pb"
}

// runFork executes the pull-fork task in commit order: the manifest-sharing
// step first (its Create arbitrates the target name — a 412 is "name
// taken", exactly like repo create), then fork-side provenance
// (Create-once, the second arbitration), the CAS'd parent index, the
// best-effort social counter, the child access bootstrap, and the event.
//
// Order matters for convergence: everything before the fork.json commit is
// retryable (share adopts when the manifest is ours, access adopts
// unconditionally); everything after is committed-or-narrated (the social
// counter shortfall precedent). A share-412 with our own provenance
// adopts and continues; a share-412 owned by anyone else (a real repo, or
// another parent's fork) reports 409 without touching it.
//
// A share that SUCCEEDS but is followed by a pre-commit failure (access
// bootstrap, provenance write) rolls the reservation back (issue #432):
// without the rollback the child manifest strands the target name — no
// data loss, but the pre-check 409s every retry until someone hand-deletes
// the prefix. The rollback deletes only keys this attempt Created (never
// an adopted share, never anything past the fork.json commit) and the
// executor re-verifies ownership before deleting, so a raced live repo is
// never touched; a rollback shortfall is narrated, never masking the root
// error. A provenance-412 whose occupying fork.json is already ours (stale
// reservation from a deleted fork) adopts and continues — ownership is
// certain there (Parent == parent).
func (s *Service) runFork(ctx context.Context, owner, repo string, in ForkInput, actor auth.Principal, rec *TaskRecord) error {
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	child := repoName(in.TargetOwner, in.Name)
	parentID := repoName(owner, repo)
	// Network root: the parent's own Root when the parent is itself a fork
	// (its Root, or its Parent when Root predates the field), else the
	// parent. A corrupt parent provenance fails closed — the tree pointer
	// is load-bearing for GC, never guessed.
	root := parentID
	if praw, _, _ := s.getJSON(ctx, ForkKey(owner, repo)); praw != nil {
		var pdoc ForkDoc
		if err := json.Unmarshal(praw, &pdoc); err != nil {
			return fmt.Errorf("%w: parent fork.json: %v", ErrCorrupt, err)
		}
		if pdoc.Root != "" {
			root = pdoc.Root
		} else if pdoc.Parent != "" {
			root = pdoc.Parent
		}
	}
	adoptedShare := false
	sharedThisAttempt := false
	if s.ForkExec != nil {
		serr := s.ForkExec.ShareManifest(ctx, parentID, child, ForkOptions{Branch: in.Branch, Description: in.Description, Creator: who})
		if serr != nil {
			if errors.Is(serr, ErrConflict) {
				// Adopt when the occupying manifest is ours (a retried
				// share after a transient failure past the Create): our
				// provenance claims it. Anything else — a real repo, or
				// another parent's fork — is 409, hands off.
				if craw, _, _ := s.getJSON(ctx, ForkKey(in.TargetOwner, in.Name)); craw != nil {
					var cdoc ForkDoc
					if jerr := json.Unmarshal(craw, &cdoc); jerr == nil && cdoc.Parent == parentID {
						rec.notice("adopted existing child manifest for %s", child)
						adoptedShare = true
					} else {
						return fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
					}
				} else {
					return fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
				}
			} else {
				return serr
			}
		} else {
			sharedThisAttempt = true
			rec.notice("shared manifest for %s", child)
		}
	} else {
		// Manifest sharing (§7: already-on-bucket mode — skip pack uploads,
		// verify closure, fresh refs snapshot + checkpoint, Create manifest).
		// No ForkExecutor is wired in this wave (the wal-level manifest copy
		// needs the engine handle the composition owns); the task narrates the
		// delegation instead of pretending. The fork-network GC rule (§7) is
		// specified now: pack removal consults children's manifests.
		rec.notice("manifest share delegated: child prefix %s provisioned; packs shared by construction on first sync (fork executor pending)", child)
	}
	// rollback releases the share reservation this attempt won when a
	// later pre-commit step fails (issue #432). It runs only when THIS
	// attempt Created the keys (never on an adopted share, never when no
	// executor ran); the executor re-verifies ownership before deleting.
	// Best-effort by design: a rollback shortfall is narrated, never
	// masking the root error the task returns.
	rollback := func(step string) {
		if !sharedThisAttempt || s.ForkExec == nil {
			return
		}
		if rerr := s.ForkExec.RollbackShare(ctx, parentID, child); rerr != nil {
			rec.notice("fork rollback after %s failure shortfall: %s", step, rerr.Error())
			return
		}
		rec.notice("rolled back share reservation for %s after %s failure; name reusable", child, step)
	}
	// Child access bootstrap (pre-commit: Create-wins/adopt, safe to
	// retry). A store failure here fails the task LOUD — an unreadable or
	// mis-visible child is never a silent shortfall.
	if s.AccessBoot != nil {
		if aerr := s.AccessBoot.EnsureRepoAccess(ctx, in.TargetOwner, in.Name, who, in.Visibility); aerr != nil {
			rollback("access bootstrap")
			return aerr
		}
		rec.notice("materialized %s access (%s)", child, in.Visibility)
	}
	forkDoc := &ForkDoc{Parent: parentID, Root: root, ForkedAt: now, Version: 1}
	raw, _ := json.Marshal(forkDoc)
	if !adoptedShare {
		if err := s.putCreate(ctx, ForkKey(in.TargetOwner, in.Name), raw); err != nil {
			if isPrecondition(err) {
				// Provenance lost the Create race. When the
				// occupying reservation is already ours (a stale
				// fork.json from a deleted fork, retried by its
				// owner), adopt and continue — ownership is
				// certain (Parent == parentID) and the backfill
				// below converges Root. Anything else —
				// unreadable, corrupt, or foreign — is 409, and
				// our share reservation rolls back so the name
				// is free for its owner to adopt-retry.
				if existing, _, gerr := s.getJSON(ctx, ForkKey(in.TargetOwner, in.Name)); gerr == nil && existing != nil {
					var edoc ForkDoc
					if jerr := json.Unmarshal(existing, &edoc); jerr == nil && edoc.Parent == parentID {
						rec.notice("adopted existing fork.json for %s", child)
						adoptedShare = true
					} else {
						rollback("provenance arbitration")
						return fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
					}
				} else {
					rollback("provenance arbitration")
					return fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
				}
			} else {
				rollback("provenance write")
				return err
			}
		} else {
			rec.notice("recorded %s", ForkKey(in.TargetOwner, in.Name))
		}
	}
	if adoptedShare {
		if _, err := s.casUpdate(ctx, ForkKey(in.TargetOwner, in.Name), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			// Adopted provenance: backfill Root when a pre-Root wave (or a
			// crashed run) left it empty. Already-correct docs are untouched.
			if cur == nil {
				return nil, false, nil
			}
			var doc ForkDoc
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, jerr
			}
			if doc.Root == root {
				return nil, false, nil
			}
			doc.Root = root
			out, _ := json.Marshal(&doc)
			return out, true, nil
		}); err != nil {
			// A backfill failure after THIS attempt won the share still
			// strands the reservation (issue #432) — roll it back so a
			// retry re-runs the full order. Pure-adopt runs (share lost,
			// nothing Created) are a no-op inside rollback.
			rollback("provenance backfill")
			return err
		}
	}
	_, err := s.casUpdate(ctx, ForksKey(owner, repo), 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		fx := &ForksIndex{Forks: []ForkEntry{}}
		if cur != nil {
			var perr error
			fx, perr = parseForks(cur)
			if perr != nil {
				return nil, false, perr
			}
		}
		for _, f := range fx.Forks {
			if f.Repo == child {
				return nil, false, nil // already listed — idempotent
			}
		}
		fx.Forks = append(fx.Forks, ForkEntry{Repo: child, ForkedAt: now})
		fx.Version++
		out, _ := json.Marshal(fx)
		return out, true, nil
	})
	if err != nil {
		return err
	}
	rec.notice("listed in %s", ForksKey(owner, repo))
	// Social counter (07 §6): the parent's social.json forks field is 07's
	// shape — incremented here, at completion, through the ForksCounter
	// seam (nil-safe). Best-effort by design: the fork objects above are
	// committed, and the counter is denormalized display state — a
	// shortfall is narrated, never fatal to the task.
	if s.Forks != nil {
		if ferr := s.Forks.IncForks(ctx, owner, repo); ferr != nil {
			rec.notice("social forks counter shortfall: %s", ferr)
		} else {
			rec.notice("incremented social.json forks")
		}
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "forked", Actor: who, PullNum: 0, Recipients: []string{}})
	return nil
}

// publishWithObjects packs the server-made tip and publishes the ref move
// with it (§5 durability): commit-tree/replay mint objects only the serving
// copy holds, so the tip is packed (tip --not --all) and uploaded
// atomically with the txn in ONE WAL entry — the merged ref never dangles
// bucket-missing objects. A pack failure fails the task LOUD (fail closed:
// a ref without its objects bricks clones); an empty pack falls back to
// the ref-only path (nothing new to upload — no hole to leave).
func (s *Service) publishWithObjects(ctx context.Context, repo, ref, old, newSHA, dir string, meta map[string]string, rec *TaskRecord, what string) error {
	if s.Git == nil {
		return fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	tp, perr := s.Git.PackTip(ctx, dir, newSHA)
	if perr != nil {
		return perr
	}
	if tp == nil {
		rec.notice("%s objects already on bucket; ref-only publish", what)
		return s.Refs.UpdateRef(ctx, repo, ref, old, newSHA, meta)
	}
	defer os.RemoveAll(tp.Scratch)
	rec.notice("%s objects packed %s (%d objects)", what, shortSHA(tp.Checksum), tp.ObjectCount)
	return s.Refs.UpdateRefWithPack(ctx, repo, ref, old, newSHA, &RefPack{
		PackPath: tp.PackPath, IdxPath: tp.IdxPath, Checksum: tp.Checksum,
		PackSize: tp.PackSize, IdxSize: tp.IdxSize, ObjectCount: tp.ObjectCount,
	}, meta)
}

// validateRepoPart checks one owner/name path part (charset/length, no
// leading dot, not "..").
func validateRepoPart(part string) error {
	if len(part) < 1 || len(part) > 100 || strings.HasPrefix(part, ".") || part == ".." {
		return fmt.Errorf("invalid name %q", part)
	}
	for i := 0; i < len(part); i++ {
		c := part[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return fmt.Errorf("invalid name %q", part)
		}
	}
	return nil
}

// isPrecondition reports a 412-style store refusal.
func isPrecondition(err error) bool {
	return err != nil && (store.IsPreconditionFailed(err) || strings.Contains(strings.ToLower(err.Error()), "precondition"))
}
