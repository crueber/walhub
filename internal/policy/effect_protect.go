package policy

import (
	"context"
	"encoding/json"
)

// ProtectEffect — the enforced effect. Default if omitted: restrict all
// four ops, empty bypass. Combination: every matching rule applies (AND);
// bypass a rule only if THAT rule's bypass matches. restricts is a closed
// enum; null and [] are parse errors, not "restrict nothing".
type ProtectEffect struct {
	RestrictOps []string // create | update | delete | force-push
	Bypass      []string // actor spellings; bypass a rule only if THIS rule's bypass matches
}

func (p *ProtectEffect) Kind() string { return "protect" }

// ExpandedCopy implements ActorExpander (Seam 3, 01 §6): the bypass list's
// team:/role: spellings resolve at load time; the receiver is never
// mutated. Unresolvable bypass entries warn and match nothing (an empty
// allow-set denies — fail-closed).
func (p *ProtectEffect) ExpandedCopy(ctx context.Context, x Expander) (Effect, []string) {
	cpy := &ProtectEffect{RestrictOps: append([]string{}, p.RestrictOps...)}
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
		case "restricts", "bypass", "_comment":
		default:
			return invalidf("effect protect: unknown key %q", k)
		}
	}

	p.RestrictOps = nil
	p.Bypass = nil

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
	return nil
}

// Evaluate — deny when the op is restricted and the actor is not bypassed
// by THIS rule. An empty/absent bypass list means nobody bypasses. Pure
// and local: no I/O on the push path.
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
