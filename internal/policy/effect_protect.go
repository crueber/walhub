package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ProtectEffect — the enforced effect. Default if omitted: restrict all
// four ops, empty bypass. Combination: every matching rule applies (AND);
// bypass a rule only if THAT rule's bypass matches. restricts is a closed
// enum; null and [] are parse errors, not "restrict nothing".
//
// require_checks (docs/features/05 §6) is the observational half of this
// effect: a strict list of 1–32 CI context names that must be green on the
// merge head. It is parsed here (strict, fail-closed) but NEVER evaluated
// here — receive-pack cannot observe external CI (the 14.5 honest note),
// so the push half ignores it and 05's merge-time gate (internal/checks,
// consulted by 03's merge task) owns the verdict. Combination across
// matching rules is union (05 §6); the load-time disjoint-bypass check
// below is unchanged by this field.
type ProtectEffect struct {
	RestrictOps   []string // create | update | delete | force-push
	Bypass        []string // actor spellings; bypass a rule only if THIS rule's bypass matches
	RequireChecks []string // optional CI contexts (05 §6); push path ignores, merge gate unions
}

func (p *ProtectEffect) Kind() string { return "protect" }

// ExpandedCopy implements ActorExpander (Seam 3, 01 §6): the bypass list's
// team:/role: spellings resolve at load time; the receiver is never
// mutated. Unresolvable bypass entries warn and match nothing (an empty
// allow-set denies — fail-closed).
func (p *ProtectEffect) ExpandedCopy(ctx context.Context, x Expander) (Effect, []string) {
	cpy := &ProtectEffect{RestrictOps: append([]string{}, p.RestrictOps...), RequireChecks: append([]string{}, p.RequireChecks...)}
	if len(p.Bypass) == 0 {
		return cpy, nil
	}
	expanded, warnings := x.ExpandGroups(ctx, p.Bypass)
	cpy.Bypass = dedup(expanded)
	return cpy, warnings
}

var opEnum = map[string]bool{OpCreate: true, OpUpdate: true, OpDelete: true, OpForcePush: true}

func (p *ProtectEffect) Parse(raw json.RawMessage) error {
	if effectNull(raw) {
		return invalidf("effect protect: must be an object")
	}
	m, err := effectObject(raw, "protect")
	if err != nil {
		return err
	}
	for k := range m {
		switch k {
		case "restricts", "bypass", "require_checks", "_comment":
		default:
			return invalidf("effect protect: unknown key %q", k)
		}
	}

	p.RestrictOps = nil
	p.Bypass = nil
	p.RequireChecks = nil

	if raw, ok := m["restricts"]; ok {
		if string(raw) == "null" {
			return invalidf("effect protect.restricts: null is a parse error, not \"restrict nothing\"")
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return invalidf("effect protect.restricts: must be an array of ops")
		}
		if len(list) == 0 {
			return invalidf("effect protect.restricts: [] is a parse error, not \"restrict nothing\"")
		}
		for _, op := range list {
			if !opEnum[op] {
				return invalidf("effect protect.restricts: %q is not one of create|update|delete|force-push", op)
			}
		}
		p.RestrictOps = list
	} else {
		p.RestrictOps = []string{OpCreate, OpUpdate, OpDelete, OpForcePush}
	}

	if raw, ok := m["bypass"]; ok && string(raw) != "null" {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return invalidf("effect protect.bypass: must be an array of actor spellings")
		}
		for _, sp := range list {
			if err := validInclude(sp); err != nil {
				return invalidf("effect protect.bypass: %v", err)
			}
		}
		p.Bypass = list
	}

	if raw, ok := m["require_checks"]; ok {
		if string(raw) == "null" {
			return invalidf("effect protect.require_checks: null is a parse error, not \"no required checks\"")
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return invalidf("effect protect.require_checks: must be an array of context names")
		}
		if len(list) < 1 || len(list) > 32 {
			return invalidf("effect protect.require_checks: must carry 1–32 contexts, got %d", len(list))
		}
		seen := map[string]bool{}
		for _, c := range list {
			if err := ValidCheckContext(c); err != nil {
				return invalidf("effect protect.require_checks: %v", err)
			}
			if seen[c] {
				return invalidf("effect protect.require_checks: duplicate context %q", c)
			}
			seen[c] = true
		}
		p.RequireChecks = list
	}
	return nil
}

// ValidCheckContext validates one CI context name (05 §2, shared by the
// protect parse above and the checks package — the grammar is one rule in
// both places): charset [A-Za-z0-9._/-], 1–100 chars, must not start or
// end with "/", must not end with ".json" (contexts containing "/" extend
// the bucket key; LIST by the checks/<sha>/ prefix still groups them).
func ValidCheckContext(c string) error {
	if len(c) < 1 || len(c) > 100 {
		return fmt.Errorf("invalid context %q: must be 1–100 chars", c)
	}
	if strings.HasPrefix(c, "/") || strings.HasSuffix(c, "/") {
		return fmt.Errorf("invalid context %q: must not start or end with \"/\"", c)
	}
	if strings.HasSuffix(c, ".json") {
		return fmt.Errorf("invalid context %q: must not end with \".json\"", c)
	}
	for i := 0; i < len(c); i++ {
		ch := c[i]
		if (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') &&
			ch != '.' && ch != '_' && ch != '/' && ch != '-' {
			return fmt.Errorf("invalid context %q: charset is [A-Za-z0-9._/-]", c)
		}
	}
	return nil
}

// Evaluate — deny when the op is restricted and the actor is not bypassed
// by THIS rule. An empty/absent bypass list means nobody bypasses. Pure
// and local: no I/O on the push path. RequireChecks is deliberately NOT
// consulted here (05 §6: direct pushes are never gated; the merge task
// owns that verdict through its own gate).
func (p *ProtectEffect) Evaluate(u Request, g Groups) Verdict {
	restricted := false
	for _, op := range p.RestrictOps {
		if op == u.Op {
			restricted = true
			break
		}
	}
	if !restricted {
		return Verdict{Allow: true}
	}
	if len(p.Bypass) > 0 && matchActorList(p.Bypass, Actor{Principal: u.Principal, Tags: u.Tags}, g) {
		return Verdict{Allow: true}
	}
	return Verdict{Allow: false}
}
