// Package policy implements the per-repo push policy rule language
// (repos/<o>/<r>/policy.json; MASTER_RUST_SPEC.md §14, docs/POLICY.md) and
// the Seam 3 effect registry (docs/go/14_extensibility.md §14.5).
//
// The file is a small rule language whose combination law is fixed, then
// serialized. Fail closed: an unparseable file is a 400 on PUT and a REJECT
// on the next push — never "skip policy". Missing file / empty rules =
// allow-all (anyone with write may move any ref) — the only implicit default.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ErrInvalid wraps every parse/load failure so callers can map it to a 400
// on PUT and a REJECT on push.
var ErrInvalid = errors.New("policy: invalid policy.json")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// The closed op enum (protect restricts; the eval input verbs).
const (
	OpCreate    = "create"
	OpUpdate    = "update"
	OpDelete    = "delete"
	OpForcePush = "force-push"
)

// ruleNameRe pins rule names: they are metric labels and the word in the
// wire rejection `rejected by rule '<name>'`.
var ruleNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Group is one roster entry: names, not decisions, resolved at eval time.
// Editing the roster applies to the next push.
type Group struct {
	Name    string
	Members []string
}

// Match — keys AND, values OR; an absent key matches everything (except
// paths on a size rule, which must be explicit). One glob dialect:
// doublestar — '*' and '?' stop at '/', '**' crosses; '^' prefix = exclusion.
type Match struct {
	Refs       []string
	Principals []string
	Paths      []string
}

// Effect is the parsed tagged union inside a rule (Seam 3). Parse is
// STRICT: unknown keys inside the effect are parse errors.
type Effect interface {
	Kind() string // "protect", "history", "size", … unique key
	Parse(raw json.RawMessage) error
}

// PushEffect is an Effect that participates in push evaluation. Evaluate
// MUST be pure and local — it receives already-loaded data and returns in
// memory; anything external is resolved before eval or it is not a push
// effect (policy eval is on the push path).
type PushEffect interface {
	Effect
	Evaluate(u Request, g Groups) Verdict
}

// Request is the eval input, per update, at receive-pack step 5.
type Request struct {
	Principal string   // principal name; "svc:merge-queue" is an exact principal
	Tags      []string // @tags the edge already bound (e.g. "okta:sre")
	Ref       string   // full ref name ("refs/heads/main")
	Op        string   // create | update | delete | force-push
	Force     bool     // update+Force is force-push: fast-forward and force share the wire triple; the server derives it (merge-base --is-ancestor) after ingest
}

// Verdict — deny names the rule on the wire: `rejected by rule '<name>'`.
type Verdict struct {
	Allow bool
	Rule  string
}

// Rule is one ordered named decision. Nothing else lives at the rule level.
type Rule struct {
	Name    string
	Comment string // _comment, never read
	Match   Match
	Mode    string // optional enforce|audit; no umbrella exists today — stored and ignored
	Effect  Effect // tagged union with exactly one key
}

// Protect, History, Size are typed views of Rule.Effect.
func (r *Rule) Protect() *ProtectEffect { e, _ := r.Effect.(*ProtectEffect); return e }
func (r *Rule) History() *HistoryEffect { e, _ := r.Effect.(*HistoryEffect); return e }
func (r *Rule) Size() *SizeEffect       { e, _ := r.Effect.(*SizeEffect); return e }

// Document is the parsed policy.json.
type Document struct {
	Version int
	Groups  []Group
	Rules   []*Rule // ordered named decisions

	roster Groups // group index built at parse, used for eval-time resolution
}

// Roster returns the group index used to resolve `group:` spellings at
// eval time.
func (d *Document) Roster() Groups { return d.roster }

// Histories returns every parsed history effect, in rule order. Parsed and
// stored, NOT enforced.
func (d *Document) Histories() []*HistoryEffect {
	var out []*HistoryEffect
	for _, r := range d.Rules {
		if h := r.History(); h != nil {
			out = append(out, h)
		}
	}
	return out
}

// Sizes returns every parsed size effect, in rule order. Parsed and stored,
// NOT enforced.
func (d *Document) Sizes() []*SizeEffect {
	var out []*SizeEffect
	for _, r := range d.Rules {
		if s := r.Size(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Parse parses and validates policy.json. Every failure wraps ErrInvalid
// (fail closed; no last-good fallback — a corrupt object fails the push).
// Envelope: unknown keys BESIDE groups/rules/version are ignored (fleet
// rolling upgrade); unknown keys INSIDE a rule/match/effect are parse
// errors (a typo must not become an empty list).
func Parse(data []byte) (*Document, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, invalidf("envelope: %v", err)
	}

	version := 0
	if raw, ok := env["version"]; ok {
		if err := json.Unmarshal(raw, &version); err != nil {
			return nil, invalidf("version: must be the integer 1")
		}
	}
	if version != 1 {
		return nil, invalidf("version: readers accept version 1, got %d", version)
	}

	d := &Document{Version: version, roster: Groups{byName: map[string][]string{}}}

	if raw, ok := env["groups"]; ok && string(raw) != "null" {
		groups, err := parseGroups(raw)
		if err != nil {
			return nil, err
		}
		d.Groups = groups
		for _, g := range groups {
			d.roster.byName[g.Name] = g.Members
		}
	}

	if raw, ok := env["rules"]; ok && string(raw) != "null" {
		rules, err := parseRules(raw)
		if err != nil {
			return nil, err
		}
		d.Rules = rules
	}

	if err := checkLockout(d.Rules); err != nil {
		return nil, err
	}
	return d, nil
}

func parseGroups(raw json.RawMessage) ([]Group, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, invalidf("groups: must be an array of {name, members}")
	}
	seen := map[string]bool{}
	groups := make([]Group, 0, len(entries))
	for i, ent := range entries {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ent, &m); err != nil {
			return nil, invalidf("groups[%d]: must be an object", i)
		}
		for k := range m {
			switch k {
			case "name", "members", "_comment":
			default:
				return nil, invalidf("groups[%d]: unknown key %q (a typo must not silently change the roster)", i, k)
			}
		}
		nameRaw, ok := m["name"]
		if !ok {
			return nil, invalidf("groups[%d]: missing name", i)
		}
		var name string
		if err := json.Unmarshal(nameRaw, &name); err != nil || name == "" {
			return nil, invalidf("groups[%d].name: must be a non-empty string", i)
		}
		if seen[name] {
			return nil, invalidf("groups[%d]: duplicate group name %q", i, name)
		}
		seen[name] = true
		var members []string
		if raw, ok := m["members"]; ok && string(raw) != "null" {
			if err := json.Unmarshal(raw, &members); err != nil {
				return nil, invalidf("groups[%d].members: must be an array of actor spellings", i)
			}
			for _, mem := range members {
				if err := validInclude(mem); err != nil {
					return nil, invalidf("groups[%d].members: %v", i, err)
				}
			}
		}
		groups = append(groups, Group{Name: name, Members: members})
	}
	return groups, nil
}

func parseRules(raw json.RawMessage) ([]*Rule, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, invalidf("rules: must be an array of rule objects")
	}
	seen := map[string]bool{}
	rules := make([]*Rule, 0, len(entries))
	for i, ent := range entries {
		r, err := parseRule(i, ent)
		if err != nil {
			return nil, err
		}
		if seen[r.Name] {
			return nil, invalidf("rules[%d]: duplicate rule name %q (names appear in metrics and on the wire)", i, r.Name)
		}
		seen[r.Name] = true
		if err := checkRuleFamily(r); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func parseRule(i int, ent json.RawMessage) (*Rule, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ent, &m); err != nil {
		return nil, invalidf("rules[%d]: must be an object", i)
	}
	for k := range m {
		switch k {
		case "name", "_comment", "match", "effect", "mode":
		default:
			return nil, invalidf("rules[%d]: unknown key %q (a typo must not become an empty list)", i, k)
		}
	}

	r := &Rule{}

	nameRaw, ok := m["name"]
	if !ok {
		return nil, invalidf("rules[%d]: missing name", i)
	}
	if err := json.Unmarshal(nameRaw, &r.Name); err != nil || !ruleNameRe.MatchString(r.Name) {
		return nil, invalidf("rules[%d].name: must match ^[a-z][a-z0-9-]{0,62}$", i)
	}

	if raw, ok := m["_comment"]; ok {
		_ = json.Unmarshal(raw, &r.Comment) // legal everywhere, never read
	}

	if raw, ok := m["mode"]; ok {
		if err := json.Unmarshal(raw, &r.Mode); err != nil || (r.Mode != "enforce" && r.Mode != "audit") {
			return nil, invalidf("rules[%d].mode: must be \"enforce\" or \"audit\"", i)
		}
	}

	if raw, ok := m["match"]; ok && string(raw) != "null" {
		match, err := parseMatch(i, raw)
		if err != nil {
			return nil, err
		}
		r.Match = match
	}

	effectRaw, ok := m["effect"]
	if !ok {
		return nil, invalidf("rules[%d]: missing effect", i)
	}
	eff, err := parseEffect(i, effectRaw)
	if err != nil {
		return nil, err
	}
	r.Effect = eff
	return r, nil
}

func parseMatch(i int, raw json.RawMessage) (Match, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Match{}, invalidf("rules[%d].match: must be an object", i)
	}
	var match Match
	keys := []struct {
		key string
		dst *[]string
	}{
		{"refs", &match.Refs},
		{"principals", &match.Principals},
		{"paths", &match.Paths},
	}
	for _, k := range keys {
		listRaw, ok := m[k.key]
		if !ok || string(listRaw) == "null" {
			continue
		}
		delete(m, k.key)
		var list []string
		if err := json.Unmarshal(listRaw, &list); err != nil {
			return Match{}, invalidf("rules[%d].match.%s: must be an array of strings", i, k.key)
		}
		for _, p := range list {
			if p == "" {
				return Match{}, invalidf("rules[%d].match.%s: patterns must not be empty", i, k.key)
			}
			if k.key == "principals" {
				if err := validInclude(strings.TrimPrefix(p, "^")); err != nil {
					return Match{}, invalidf("rules[%d].match.principals: %v", i, err)
				}
			}
		}
		*k.dst = list
	}
	for k := range m {
		if k != "_comment" { // _comment is legal everywhere
			return Match{}, invalidf("rules[%d].match: unknown key %q", i, k)
		}
	}
	return match, nil
}

// parseEffect resolves the tagged union: exactly one key, a registered
// effect kind, parsed STRICTLY. Unknown kinds are parse errors (fail
// closed: deploy the binary that knows the effect before any repo adopts
// it — an old binary rejects the file).
func parseEffect(i int, raw json.RawMessage) (Effect, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, invalidf("rules[%d].effect: must be an object with exactly one effect kind", i)
	}
	var kinds []string
	for k := range m {
		if k != "_comment" { // _comment is legal everywhere
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	if len(kinds) != 1 {
		return nil, invalidf("rules[%d].effect: tagged union needs exactly one key, got %q", i, kinds)
	}
	kind := kinds[0]
	factory, ok := effectRegistry[kind]
	if !ok {
		return nil, invalidf("rules[%d].effect: unknown effect kind %q (fail closed)", i, kind)
	}
	eff := factory()
	if err := eff.Parse(m[kind]); err != nil {
		return nil, err
	}
	return eff, nil
}

// checkRuleFamily enforces per-effect family rules at load:
//
//   - size must spell match.paths explicitly (an absent paths would raise
//     the ceiling for the whole repo);
//   - ^ exclusions are refused outside protect (a carve-out on a family
//     that is not most-restrictive is a no-op that looks like a revoke).
//     protect is most-restrictive (AND) and may use ^.
func checkRuleFamily(r *Rule) error {
	switch r.Effect.(type) {
	case *ProtectEffect:
		// paths is legal here but ignored until a quarantine path walk exists.
	case *HistoryEffect, *SizeEffect:
		for _, list := range []struct {
			key  string
			pats []string
		}{
			{"refs", r.Match.Refs},
			{"principals", r.Match.Principals},
			{"paths", r.Match.Paths},
		} {
			for _, p := range list.pats {
				if len(p) > 0 && p[0] == '^' {
					return invalidf("rule %q: ^ exclusion in match.%s is refused outside protect", r.Name, list.key)
				}
			}
		}
		if _, ok := r.Effect.(*SizeEffect); ok && len(r.Match.Paths) == 0 {
			return invalidf("rule %q: size must spell match.paths explicitly (absent paths would raise the ceiling repo-wide)", r.Name)
		}
	default:
		// Registered extension effects declare their own compatibility rules.
	}
	return nil
}

// checkLockout — the load-time check: two protect rules that can match the
// same (ref, op) with non-empty, disjoint bypass lists lock the (ref, op)
// out, because AND would make the intended bot unable to land. Load fails.
func checkLockout(rules []*Rule) error {
	for i := range rules {
		a := rules[i].Protect()
		if a == nil {
			continue
		}
		for j := i + 1; j < len(rules); j++ {
			b := rules[j].Protect()
			if b == nil {
				continue
			}
			if len(a.Bypass) == 0 || len(b.Bypass) == 0 {
				continue // an unbypassable rule is an intended hard lock
			}
			if !refSetsOverlap(rules[i].Match.Refs, rules[j].Match.Refs) {
				continue
			}
			if !opsOverlap(a.RestrictOps, b.RestrictOps) {
				continue
			}
			if bypassesDisjoint(a.Bypass, b.Bypass) {
				return invalidf("protect rules %q and %q can both match the same (ref, op) but their bypass lists are disjoint — the AND would lock out the intended bot", rules[i].Name, rules[j].Name)
			}
		}
	}
	return nil
}

// Evaluate answers, per update: is this publish allowed, and if not, which
// named rule said so? protect rules AND-combine: every matching rule
// applies; a rule is bypassed only if THAT rule's bypass matches. The
// first denying rule in file order is named. history/size are parsed and
// stored, never enforced here. A missing/empty document is allow-all.
func Evaluate(ctx context.Context, d *Document, req Request) Verdict {
	if d == nil || len(d.Rules) == 0 {
		return Verdict{Allow: true}
	}
	effective := req
	effective.Op = effectiveOp(req)
	for _, r := range d.Rules {
		pe, ok := r.Effect.(PushEffect)
		if !ok {
			continue // parsed, not enforced
		}
		if !ruleMatches(r, effective, d.roster) {
			continue
		}
		if v := pe.Evaluate(effective, d.roster); !v.Allow {
			return Verdict{Allow: false, Rule: r.Name}
		}
	}
	return Verdict{Allow: true}
}

// effectiveOp: fast-forward and force have the same wire triple; the server
// derives force-push (tags are never fast-forward in any useful sense — a
// tag retarget is force-push) and passes it via Force.
func effectiveOp(req Request) string {
	if req.Op == OpUpdate && req.Force {
		return OpForcePush
	}
	return req.Op
}

// ruleMatches — keys AND, values OR; absent key matches everything. paths
// is reserved for size and ignored on the eval path until a quarantine
// path walk exists.
func ruleMatches(r *Rule, req Request, g Groups) bool {
	if !matchGlobList(r.Match.Refs, req.Ref) {
		return false
	}
	return matchActorList(r.Match.Principals, Actor{Principal: req.Principal, Tags: req.Tags}, g)
}
