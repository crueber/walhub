package policy

import (
	"context"
	"sort"
	"strings"
)

// Expansion of team:/role: group-member spellings (docs/features/01 §6).
//
// policy.json stays the frozen envelope; effects are untouched. A
// groups[].members entry (or a match.principals spelling) MAY be
// "team:<org>/<slug>" or "role:<owner>/<repo>:<role>". Expansion happens at
// policy LOAD under the existing per-repo policy cache single-flight;
// Evaluate stays pure and local. A reference that fails to resolve is a
// warning and evaluates to the empty set (fail-closed for protect
// semantics: an empty allow-set denies — in particular an unresolvable
// bypass entry never admits).
//
// ### Concurrency
//
// Hazard: team expansion putting a blocking bucket read on the push path.
// Avoidance: expansion runs once per load (not per evaluation); Evaluate
// never calls the Expander. A concurrent team edit either lands before the
// load (seen) or after (next reload) — never torn, because objects are read
// whole. No locks: the Expander owns its own caching.

// Expander resolves team:/role: spellings at load time. Implementations
// (internal/identity) read whole CAS'd objects; unresolvable references
// yield warnings, never errors.
type Expander interface {
	// ExpandGroups maps member spellings to principal lists: team:/role:
	// spellings are replaced by their expansion, all other spellings pass
	// through untouched. Warnings name references that resolved to empty.
	ExpandGroups(ctx context.Context, members []string) (expanded []string, warnings []string)
}

// ActorExpander is implemented by effects carrying actor spellings (bypass
// lists, reviewer sets) that need load-time team:/role: expansion.
// ExpandedCopy returns the effect with its actor lists replaced (the
// original is never mutated) plus any warnings.
type ActorExpander interface {
	ExpandedCopy(ctx context.Context, x Expander) (Effect, []string)
}

// ExpandDocument returns a copy of d with every team:/role: spelling in
// groups[].members and match.principals replaced by its expansion, plus
// the warnings. The roster index is rebuilt over the expanded groups so
// group: indirection keeps working. A nil Expander returns d unchanged.
func ExpandDocument(ctx context.Context, d *Document, x Expander) (*Document, []string) {
	if d == nil || x == nil {
		return d, nil
	}
	var warnings []string
	out := &Document{Version: d.Version, roster: Groups{byName: map[string][]string{}}}
	for _, g := range d.Groups {
		expanded, w := x.ExpandGroups(ctx, g.Members)
		warnings = append(warnings, w...)
		out.Groups = append(out.Groups, Group{Name: g.Name, Members: dedup(expanded)})
		out.roster.byName[g.Name] = dedup(expanded)
	}
	for _, r := range d.Rules {
		nr := &Rule{Name: r.Name, Comment: r.Comment, Match: r.Match, Mode: r.Mode, Effect: r.Effect}
		if len(r.Match.Principals) > 0 {
			expanded, w := x.ExpandGroups(ctx, r.Match.Principals)
			warnings = append(warnings, w...)
			nr.Match.Principals = dedup(expanded)
		}
		// Effects carrying actor spellings (protect bypass lists) expand
		// on a copy — the source document is never mutated.
		if xa, ok := r.Effect.(ActorExpander); ok {
			eff, w := xa.ExpandedCopy(ctx, x)
			warnings = append(warnings, w...)
			nr.Effect = eff
		}
		out.Rules = append(out.Rules, nr)
	}
	sort.Strings(warnings)
	return out, warnings
}

// IsTeamRef reports whether spelling is a team: reference.
func IsTeamRef(spelling string) bool { return strings.HasPrefix(spelling, "team:") }

// IsRoleRef reports whether spelling is a role: reference.
func IsRoleRef(spelling string) bool { return strings.HasPrefix(spelling, "role:") }

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}
