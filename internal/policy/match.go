package policy

import (
	"errors"
	"fmt"
	"strings"
)

// Actor is who is pushing: the principal name plus the @tags the edge
// already bound.
type Actor struct {
	Principal string
	Tags      []string
}

type spellingKindT int

const (
	spellingPrincipal spellingKindT = iota // exact principal, case-insensitive
	spellingTag                            // @-prefixed tag ("@okta:sre"), bound by the edge
	spellingGroup                          // group:<name>, roster in this file
)

// spellingKind classifies an actor spelling. There are exactly three
// spellings, nothing else: an exact principal (so "svc:merge-queue" is a
// principal name, not a scheme), a tag bound by the edge ("@" prefix,
// e.g. "@okta:sre" → tag "okta:sre"), and "group:<name>".
func spellingKind(sp string) (spellingKindT, string) {
	switch {
	case strings.HasPrefix(sp, "@"):
		return spellingTag, strings.TrimPrefix(sp, "@")
	case strings.HasPrefix(sp, "group:"):
		return spellingGroup, strings.TrimPrefix(sp, "group:")
	default:
		return spellingPrincipal, sp
	}
}

// validInclude checks an actor spelling where only inclusion makes sense
// (roster members, bypass lists): exact principal, @-tag, group:<name> —
// "^" exclusions are not spellings here.
func validInclude(sp string) error {
	if sp == "" {
		return errors.New("empty actor spelling")
	}
	if sp[0] == '^' {
		return fmt.Errorf("%q: ^ exclusions are not valid here", sp)
	}
	if strings.HasPrefix(sp, "@") && sp == "@" {
		return errors.New(`"@": tag name must not be empty`)
	}
	if rest, ok := strings.CutPrefix(sp, "group:"); ok && rest == "" {
		return fmt.Errorf("%q: group: name must not be empty", sp)
	}
	return nil
}

// Groups is the roster index (Document.Roster). group: spellings resolve
// at eval time against it; a group absent from the index is unresolvable —
// an unresolvable include does not admit, an unresolvable exclude still
// excludes.
type Groups struct {
	byName map[string][]string
}

// Lookup returns the members of a roster group and whether it exists.
func (g Groups) Lookup(name string) ([]string, bool) {
	m, ok := g.byName[name]
	return m, ok
}

// matchGlobList — values OR over the positive patterns; ^ exclusions veto;
// an absent key (nil) matches everything.
func matchGlobList(patterns []string, s string) bool {
	if len(patterns) == 0 {
		return true
	}
	matched := false
	hasPositive := false
	for _, p := range patterns {
		if p[0] != '^' {
			hasPositive = true
			if globMatch(p, s) {
				matched = true
			}
		}
	}
	if hasPositive && !matched {
		return false
	}
	for _, p := range patterns {
		if p[0] == '^' && globMatch(p[1:], s) {
			return false
		}
	}
	return true
}

// matchActorList — the actor spellings under the same keys-AND/values-OR
// law, with the unresolvable-group semantics: an unresolvable include does
// not admit, an unresolvable exclude still excludes.
func matchActorList(spellings []string, actor Actor, g Groups) bool {
	if len(spellings) == 0 {
		return true
	}
	hasInclude := false
	matched := false
	for _, sp := range spellings {
		if sp[0] == '^' {
			continue
		}
		hasInclude = true
		if m, resolvable := matchSpelling(sp, actor, g, nil); resolvable && m {
			matched = true
			break
		}
	}
	if hasInclude && !matched {
		return false
	}
	for _, sp := range spellings {
		if sp[0] != '^' {
			continue
		}
		m, resolvable := matchSpelling(sp[1:], actor, g, nil)
		if m || !resolvable { // unresolvable exclude still excludes
			return false
		}
	}
	return true
}

// matchSpelling resolves one actor spelling. resolvable is false only for
// a group: spelling naming a group absent from the roster (indeterminate —
// and indeterminate is not "no": callers apply the include/exclude rule).
// Nested group spellings resolve recursively; a cycle does not admit.
func matchSpelling(sp string, actor Actor, g Groups, seen map[string]bool) (match, resolvable bool) {
	kind, name := spellingKind(sp)
	switch kind {
	case spellingTag:
		for _, t := range actor.Tags {
			if t == name {
				return true, true
			}
		}
		return false, true
	case spellingGroup:
		if seen[name] {
			return false, true
		}
		members, ok := g.Lookup(name)
		if !ok {
			return false, false
		}
		if seen == nil {
			seen = map[string]bool{}
		}
		seen[name] = true
		for _, mem := range members {
			if m, r := matchSpelling(mem, actor, g, seen); r && m {
				return true, true
			}
		}
		return false, true
	default:
		return strings.EqualFold(name, actor.Principal), true
	}
}

// globMatch is the one glob dialect: doublestar. '*' and '?' stop at '/',
// '**' crosses; everything else is a literal byte compare.
func globMatch(pattern, s string) bool {
	p, t := pattern, s
	for len(p) > 0 {
		switch p[0] {
		case '*':
			if len(p) > 1 && p[1] == '*' {
				p = p[2:]
				if strings.HasPrefix(p, "/") {
					// "**/" spans zero or more segments.
					p = p[1:]
					if globMatch(p, t) {
						return true
					}
					for i := range len(t) {
						if t[i] == '/' && globMatch(p, t[i+1:]) {
							return true
						}
					}
					return false
				}
				if len(p) == 0 {
					return true // trailing "**" crosses everything
				}
				for i := range len(t) + 1 {
					if globMatch(p, t[i:]) {
						return true
					}
				}
				return false
			}
			p = p[1:]
			for i := range len(t) + 1 {
				if i > 0 && t[i-1] == '/' {
					break // single '*' stops at '/'
				}
				if globMatch(p, t[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(t) == 0 || t[0] == '/' {
				return false // '?' never crosses '/'
			}
			p, t = p[1:], t[1:]
		default:
			if len(t) == 0 || t[0] != p[0] {
				return false
			}
			p, t = p[1:], t[1:]
		}
	}
	return len(t) == 0
}

// refSetsOverlap conservatively decides whether two rules' ref pattern
// sets can match the same ref. An absent set matches everything.
// Exclusions only narrow and are ignored (the analysis errs toward
// overlap, never toward proven disjointness).
func refSetsOverlap(a, b []string) bool {
	a = positives(a)
	b = positives(b)
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, pa := range a {
		for _, pb := range b {
			if patternsOverlap(pa, pb) {
				return true
			}
		}
	}
	return false
}

func positives(pats []string) []string {
	var out []string
	for _, p := range pats {
		if p[0] != '^' {
			out = append(out, p)
		}
	}
	return out
}

// patternsOverlap decides whether one string could match both patterns.
// Two literals: equality. Literal vs pattern: the pattern must match that
// exact string. Two patterns: provably disjoint only when their literal
// prefixes share no possible string.
func patternsOverlap(a, b string) bool {
	ga, gb := hasGlob(a), hasGlob(b)
	switch {
	case !ga && !gb:
		return a == b
	case !ga:
		return globMatch(b, a)
	case !gb:
		return globMatch(a, b)
	}
	la, lb := literalPrefix(a), literalPrefix(b)
	return la == lb || strings.HasPrefix(la, lb) || strings.HasPrefix(lb, la)
}

func hasGlob(p string) bool { return strings.ContainsAny(p, "*?") }

func literalPrefix(p string) string {
	if i := strings.IndexAny(p, "*?"); i >= 0 {
		return p[:i]
	}
	return p
}

func opsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// bypassesDisjoint reports whether no bypass spelling in a can equal one
// in b. Static equality only: exact principals compare case-insensitively,
// tags and groups by exact name. Membership across a group boundary cannot
// be proven statically and counts as disjoint — fail closed at load.
func bypassesDisjoint(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if spellingEqual(x, y) {
				return false
			}
		}
	}
	return true
}

func spellingEqual(x, y string) bool {
	kx, nx := spellingKind(x)
	ky, ny := spellingKind(y)
	if kx != ky {
		return false
	}
	if kx == spellingPrincipal {
		return strings.EqualFold(nx, ny)
	}
	return nx == ny
}
