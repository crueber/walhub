package pulls

import (
	"context"
	"fmt"

	"git.packden.us/crueber/walhub/internal/policy"
)

// This file owns the 05 checks coordination surface
// (docs/features/05_checks_statuses.md §6): the merge-time half of the
// require_checks extension is evaluated by 05's checks package (which
// reads the stored combined view for the head sha), and 03's merge task
// consults it through the ChecksGate seam below — the merge logic is NOT
// forked. The exact call site is runMerge step 4 in merge.go (after the
// protected-ref check, next to the 04 required-reviews gate).

// ChecksGate is the 05-provided merge-time half of the require_checks
// extension. Satisfied by *checks.Service (CheckRequiredChecks); tests
// substitute a fake. Nil skips the gate ONLY when no policy rule carries
// it — a rule carrying require_checks with no checks backend fails
// closed (the merge refuses "no checks backend" rather than silently
// allowing).
type ChecksGate interface {
	// CheckRequiredChecks resolves every require_checks list matching
	// baseRef (union across matching rules the merger does not bypass)
	// and requires every required context present AND success on
	// headSHA. headSHA is the LIVE head. A failed gate refuses with the
	// verbatim message: merge refused: required checks not green for
	// <sha>: <ctx> (<state|missing>), …; the merge ref is not published.
	CheckRequiredChecks(ctx context.Context, owner, repo, headSHA, baseRef, merger string) error
}

// checkRequiredChecksGate consults the 05 gate for one merge (runMerge
// step 4, after the protected-ref check): with a checks backend wired it
// delegates the verdict; without one it fails closed only when a policy
// rule actually carries the gate (plain protect rules merge fine).
func (s *Service) checkRequiredChecksGate(ctx context.Context, owner, repo, headSHA, baseRef, merger string) error {
	if s.Checks != nil {
		return s.Checks.CheckRequiredChecks(ctx, owner, repo, headSHA, baseRef, merger)
	}
	names, err := requiredChecksCarried(ctx, s, owner, repo, baseRef, merger)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%w: rule '%s' requires checks: no checks backend wired", ErrConflict, names[0])
}

// requiredChecksCarried reports the names of rules carrying a
// require_checks gate for (merger, baseRef, update) — the fail-closed
// probe for the unwired-backend case. It parses the strict policy (an
// unparseable file fails closed upstream in loadPolicy; here it is a
// nil verdict — the caller runs this only after checkProtectedRef
// already failed closed on it).
func requiredChecksCarried(ctx context.Context, s *Service, owner, repo, baseRef, merger string) ([]string, error) {
	raw, _, err := s.getJSON(ctx, PolicyKey(owner, repo))
	if err != nil || raw == nil {
		return nil, err
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return nil, nil // loadPolicy fails closed next; no gate verdict here
	}
	var names []string
	for _, r := range policy.MatchingRules(doc, policy.Request{Principal: merger, Ref: baseRef, Op: policy.OpUpdate}) {
		pe := r.Protect()
		if pe == nil || len(pe.RequireChecks) == 0 {
			continue
		}
		if policy.Bypassed(pe.Bypass, merger, nil, doc.Roster()) {
			continue
		}
		names = append(names, r.Name)
	}
	return names, nil
}
