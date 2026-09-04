package policy

import (
	"context"
	"encoding/json"
)

// RequiredReviewsEffect — the `required-reviews` policy effect owned by
// docs/features/04_code_review.md §6 (one effect with two honest halves).
//
//	{ "name": "pr-gate", "match": { "refs": ["refs/heads/main"] },
//	  "effect": { "required-reviews": { "min_approvals": 2, "dismiss_stale": true,
//	                                    "bypass": ["group:admins", "svc:merge-queue"] } } }
//
// Push-time half (enforced here): at receive-pack the effect behaves exactly
// like the `review-required` sketch of 14_extensibility.md §14.5 — deny
// direct (non-bypass) pushes to matched refs on every op. Pure and local
// per the Seam-3 concurrency rule (no I/O on the push path; the verdict
// names the rule on the wire via the shared Evaluate loop).
//
// Merge-time half (the review gate) is NOT evaluated here — receive-pack
// can never observe "the PR had two approvals" (the policy docs' honesty
// rule). It lives in internal/review, which scans the immutable review
// events at merge time; 03's merge task consults it before publishing the
// merge ref. The envelope rules are frozen: unknown keys inside the effect
// are parse errors (fail closed: 400 on PUT, REJECT on push).
//
// Combination: overlapping required-reviews rules on the same ref combine
// most-restrictively at the merge gate (max min_approvals, dismiss_stale
// OR, every matching rule's bypass must admit); they never fail the load
// (unlike disjoint-bypass `protect` — see the load-time check in policy.go,
// which only constrains protect pairs).
type RequiredReviewsEffect struct {
	MinApprovals int      // surviving fresh approvals required (≥ 1)
	DismissStale bool     // only approvals on the current head count
	Bypass       []string // actor spellings; bypass a rule only if THIS rule's bypass matches
}

func (e *RequiredReviewsEffect) Kind() string { return "required-reviews" }

// ExpandedCopy implements ActorExpander (Seam 3, same contract as
// ProtectEffect): the bypass list's team:/role: spellings resolve at load
// time; the receiver is never mutated. Unresolvable bypass entries warn and
// match nothing (an empty allow-set denies — fail-closed).
func (e *RequiredReviewsEffect) ExpandedCopy(ctx context.Context, x Expander) (Effect, []string) {
	cpy := &RequiredReviewsEffect{MinApprovals: e.MinApprovals, DismissStale: e.DismissStale}
	if len(e.Bypass) == 0 {
		return cpy, nil
	}
	expanded, warnings := x.ExpandGroups(ctx, e.Bypass)
	cpy.Bypass = dedup(expanded)
	return cpy, warnings
}

func (e *RequiredReviewsEffect) Parse(raw json.RawMessage) error {
	if effectNull(raw) {
		return invalidf("effect required-reviews: must be an object")
	}
	m, err := effectObject(raw, "required-reviews")
	if err != nil {
		return err
	}
	for k := range m {
		switch k {
		case "min_approvals", "dismiss_stale", "bypass", "_comment":
		default:
			return invalidf("effect required-reviews: unknown key %q", k)
		}
	}

	e.MinApprovals = 0
	e.DismissStale = false
	e.Bypass = nil

	rawMin, ok := m["min_approvals"]
	if !ok || string(rawMin) == "null" {
		return invalidf("effect required-reviews.min_approvals: required, an integer >= 1")
	}
	var min float64
	if err := json.Unmarshal(rawMin, &min); err != nil || min != float64(int(min)) || int(min) < 1 {
		return invalidf("effect required-reviews.min_approvals: required, an integer >= 1")
	}
	e.MinApprovals = int(min)

	if raw, ok := m["dismiss_stale"]; ok && string(raw) != "null" {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return invalidf("effect required-reviews.dismiss_stale: must be a boolean")
		}
		e.DismissStale = b
	}

	if raw, ok := m["bypass"]; ok && string(raw) != "null" {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return invalidf("effect required-reviews.bypass: must be an array of actor spellings")
		}
		for _, sp := range list {
			if err := validInclude(sp); err != nil {
				return invalidf("effect required-reviews.bypass: %v", err)
			}
		}
		e.Bypass = list
	}
	return nil
}

// Evaluate — deny every direct push op to a matched ref unless the actor is
// bypassed by THIS rule. An empty/absent bypass list means nobody bypasses
// (fail-closed: a ref that requires reviews has no direct-push path; the
// merge task publishes server-side, outside receive-pack). Pure and local:
// no I/O on the push path.
func (e *RequiredReviewsEffect) Evaluate(u Request, g Groups) Verdict {
	if len(e.Bypass) > 0 && matchActorList(e.Bypass, Actor{Principal: u.Principal, Tags: u.Tags}, g) {
		return Verdict{Allow: true}
	}
	return Verdict{Allow: false}
}
