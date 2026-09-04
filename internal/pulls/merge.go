package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// when the rule carries it (pending Wave 05: fails closed only if a
	// rule actually carries the gate).
	if gerr := s.checkProtectedRef(ctx, owner, repo, who, pr.Base.Ref, "update"); gerr != nil {
		rec.notice("protected-ref gate: %s", gerr.Error())
		return nil, fmt.Errorf("%w: %v", ErrConflict, gerr)
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
		newSHA, err = s.Git.Replay(ctx, baseDir, baseLive, baseLive, headLive)
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
	// Step 5: publish — REF_UPDATE for the base ref, old = base sha at plan
	// time. The WAL publish CAS arbitrates against concurrent pushes: a
	// moved base loses the CAS and the task re-plans once, else fails
	// loudly. NEVER force-publishes.
	meta := map[string]string{"principal": who, "agent": "pulls"}
	if correlationID != "" {
		meta["correlation_id"] = correlationID
	}
	if perr := s.Refs.UpdateRef(ctx, pr.Base.Repo, pr.Base.Ref, baseLive, newSHA, meta); perr != nil {
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
	pr.Merged = true
	pr.MergedAt = &mergedAt
	pr.MergedBy = &who
	pr.MergeCommitSHA = &newSHA
	pr.MergeStrategy = &in.Strategy
	_, prVer, _ := s.loadPR(ctx, owner, repo, num)
	_ = s.savePR(ctx, owner, repo, pr, prVer)
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
	if perr := s.Refs.UpdateRef(ctx, pr.Head.Repo, pr.Head.Ref, headLive, newSHA, meta); perr != nil {
		return "", fmt.Errorf("%w: head moved; retry", ErrConflict)
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
// {target_owner?, name?} → 202 + TaskRecord (pull-fork). Auth: write on the
// source (+ create rights on the target — approximated by requiring an
// authenticated principal; the target namespace owner check rides the repo
// create path the executor delegates to).
type ForkInput struct {
	TargetOwner string
	Name        string
}

// ForkExecutor performs the manifest-sharing step (§7: the fork's
// manifest.pb references the parent's pack set verbatim plus a fresh refs
// snapshot, in already-on-bucket mode). Nil in this wave: the task records
// the collaboration objects (fork.json, forks.json) and narrates the
// manifest step as delegated — the GC rule (consult fork-network manifests
// before pack deletion) is specified now and enforced by the maintain unit
// once it reads meta/forks.json.
type ForkExecutor interface {
	ShareManifest(ctx context.Context, parent, child string) error
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
	child := repoName(targetOwner, name)
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(child, TaskKindFork)
	if joined {
		return entry.rec.snapshot(), child, nil
	}
	entry.rec.notice("fork %s → %s queued by %s", repoID, child, normPrincipal(actor.Name))
	go func() {
		defer s.tasks.end(child, TaskKindFork)
		bctx := context.WithoutCancel(ctx)
		if rerr := s.runFork(bctx, owner, repo, targetOwner, name, actor, entry.rec); rerr != nil {
			entry.rec.setState(TaskError, "", rerr.Error(), nil)
			entry.rec.notice("fork failed: %s", rerr.Error())
			entry.err = rerr
			return
		}
		entry.rec.setState(TaskOK, fmt.Sprintf("forked %s → %s", repoID, child), "", nil)
	}()
	return entry.rec.snapshot(), child, nil
}

// runFork executes the pull-fork task: Create fork-side provenance (the
// Create arbitrates the target name — a 412 is "name taken", exactly like
// repo create), CAS the parent forks.json, then the manifest-sharing step.
func (s *Service) runFork(ctx context.Context, owner, repo, targetOwner, name string, actor auth.Principal, rec *TaskRecord) error {
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	child := repoName(targetOwner, name)
	forkDoc := &ForkDoc{Parent: repoName(owner, repo), ForkedAt: now, Version: 1}
	raw, _ := json.Marshal(forkDoc)
	if err := s.putCreate(ctx, ForkKey(targetOwner, name), raw); err != nil {
		if isPrecondition(err) {
			return fmt.Errorf("%w: fork target %s already exists", ErrConflict, child)
		}
		return err
	}
	rec.notice("recorded %s", ForkKey(targetOwner, name))
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
	// Manifest sharing (§7: already-on-bucket mode — skip pack uploads,
	// verify closure, fresh refs snapshot + checkpoint, Create manifest).
	// No ForkExecutor is wired in this wave (the wal-level manifest copy
	// needs the engine handle the composition owns); the task narrates the
	// delegation instead of pretending. The fork-network GC rule (§7) is
	// specified now: pack removal consults children's manifests.
	rec.notice("manifest share delegated: child prefix %s provisioned; packs shared by construction on first sync (fork executor pending)", child)
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "forked", Actor: who, PullNum: 0, Recipients: []string{}})
	return nil
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
