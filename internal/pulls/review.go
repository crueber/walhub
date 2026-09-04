package pulls

import (
	"context"
	"strings"
)

// This file owns the 04 code-review coordination surface
// (docs/features/04_code_review.md §6): the merge-time half of the
// required-reviews effect is evaluated by 04's review package (which scans
// the immutable review events at merge time), and 03's merge task consults
// it through the ReviewGate seam below — the merge logic is NOT forked.
// The exact call site is runMerge step 4 in merge.go (after the
// protected-ref check, before the strategy argv).

// ReviewGate is the 04-provided merge-time half of the required-reviews
// effect. Satisfied by *review.Service (CheckRequiredReviews); tests
// substitute a fake. Nil skips the gate (no review backend wired).
type ReviewGate interface {
	// CheckRequiredReviews resolves every required-reviews rule matching
	// baseRef and requires surviving approvals ≥ min_approvals
	// (most-restrictive), no surviving CHANGES_REQUESTED, and — when
	// dismiss_stale — only approvals on headSHA. headSHA is the LIVE head
	// (the gate re-derives by event scan; it never trusts
	// review_summary). A failed gate names the shortfall; the merge ref
	// is not published.
	CheckRequiredReviews(ctx context.Context, owner, repo string, num int, headSHA, baseRef, merger string) error
}

// HeadAuthors returns up to n author principals of commits in base..head
// of PR num, newest-first (04 §5 review-suggest third source, via
// review.CommitAuthors — structural, no import). Nil Git/Dirs ⇒ empty, nil
// (suggest degrades to bindings). Best-effort: git author names are
// normalized lowercase and may not match login names.
func (s *Service) HeadAuthors(ctx context.Context, owner, repo string, num, n int) ([]string, error) {
	if n <= 0 {
		n = 20
	}
	if n > 50 {
		n = 50
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return []string{}, nil
	}
	if s.Git == nil || s.Dirs == nil {
		return []string{}, nil
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return []string{}, nil
	}
	head := pr.Head.SHA
	if live, rerr := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref); rerr == nil && live != "" {
		head = live
	}
	rows, err := s.Git.LogRange(ctx, headDir, pr.Base.SHA, head, 0, n)
	if err != nil {
		return []string{}, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		a := strings.ToLower(strings.TrimSpace(r.Author))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
