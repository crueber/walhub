package policy

import (
	"context"
	"testing"
)

// stubExpander is a canned Expander for load-time expansion tests.
type stubExpander struct {
	teams map[string][]string
	roles map[string][]string
}

func (s *stubExpander) ExpandGroups(_ context.Context, members []string) ([]string, []string) {
	var out, warn []string
	for _, m := range members {
		if len(m) > 5 && m[:5] == "team:" {
			if t, ok := s.teams[m[5:]]; ok {
				out = append(out, t...)
				continue
			}
			warn = append(warn, "unresolvable team reference "+m)
			continue
		}
		if len(m) > 5 && m[:5] == "role:" {
			if r, ok := s.roles[m[5:]]; ok {
				out = append(out, r...)
				continue
			}
			warn = append(warn, "unresolvable role reference "+m)
			continue
		}
		out = append(out, m)
	}
	return out, warn
}

func TestExpandDocument(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,
		"groups":[{"name":"devs","members":["team:acme/platform","svc:bot"]}],
		"rules":[
			{"name":"main","match":{"refs":["refs/heads/main"],"principals":["group:devs"]},
			 "effect":{"protect":{"restricts":["force-push"],"bypass":["team:acme/bots"]}}},
			{"name":"open","match":{},"effect":{"protect":{"restricts":["delete"]}}}
		]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	x := &stubExpander{
		teams: map[string][]string{"acme/platform": {"bob@example.com"}, "acme/bots": {"svc:bot"}},
		roles: map[string][]string{"acme/repo:write": {"carol@example.com"}},
	}
	out, warnings := ExpandDocument(context.Background(), doc, x)
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	devs, ok := out.Roster().Lookup("devs")
	if !ok || len(devs) != 2 || devs[0] != "bob@example.com" || devs[1] != "svc:bot" {
		t.Errorf("expanded group = %v", devs)
	}
	// group: indirection survives expansion (resolved at eval time).
	if len(out.Rules[0].Match.Principals) != 1 || out.Rules[0].Match.Principals[0] != "group:devs" {
		t.Errorf("group ref must survive: %+v", out.Rules[0].Match)
	}
	// Bypass expanded: the bot passes, the teammate does not.
	v := Evaluate(context.Background(), out, Request{Principal: "svc:bot", Ref: "refs/heads/main", Op: OpForcePush})
	if !v.Allow {
		t.Errorf("bot bypass must allow: %+v", v)
	}
	v = Evaluate(context.Background(), out, Request{Principal: "bob@example.com", Ref: "refs/heads/main", Op: OpForcePush})
	if v.Allow || v.Rule != "main" {
		t.Errorf("teammate force-push must deny by rule 'main': %+v", v)
	}
	// The original document is untouched (expansion copies).
	orig, _ := doc.Roster().Lookup("devs")
	if len(orig) != 2 || orig[0] != "team:acme/platform" {
		t.Errorf("original mutated: %v", orig)
	}
}

func TestExpandDocumentWarnings(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,
		"groups":[{"name":"g","members":["team:acme/ghost"]}],
		"rules":[{"name":"r","match":{"principals":["role:o/r:admin"]},"effect":{"protect":{"restricts":["delete"]}}}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, warnings := ExpandDocument(context.Background(), doc, &stubExpander{})
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v", warnings)
	}
	// Unresolvable → empty set: an empty match.principals matches everyone,
	// so a protect rule applies repo-wide and denies (fail-closed). The
	// bypass direction is likewise deny (no bypass entry can match).
	v := Evaluate(context.Background(), out, Request{Principal: "anyone@example.com", Ref: "refs/heads/main", Op: OpDelete})
	if v.Allow || v.Rule != "r" {
		t.Errorf("empty expansion must deny fail-closed: %+v", v)
	}
	// Nil doc / nil expander pass through.
	if _, w := ExpandDocument(context.Background(), nil, &stubExpander{}); w != nil {
		t.Errorf("nil doc warnings = %v", w)
	}
	if got, w := ExpandDocument(context.Background(), doc, nil); got != doc || w != nil {
		t.Errorf("nil expander must pass through: %v %v", got, w)
	}
}

func TestProtectExpandedCopy(t *testing.T) {
	x := &stubExpander{teams: map[string][]string{"acme/bots": {"svc:bot"}}}
	// Empty bypass stays empty without warnings.
	pe := &ProtectEffect{RestrictOps: []string{OpDelete}}
	cpy, w := pe.ExpandedCopy(context.Background(), x)
	if len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
	if got := cpy.(*ProtectEffect); len(got.Bypass) != 0 || len(got.RestrictOps) != 1 {
		t.Errorf("empty copy = %+v", got)
	}
	if len(pe.Bypass) != 0 {
		t.Error("receiver must not mutate")
	}
	// Unresolvable bypass warns and matches nothing.
	pe2 := &ProtectEffect{RestrictOps: []string{OpDelete}, Bypass: []string{"team:acme/ghost"}}
	cpy2, w2 := pe2.ExpandedCopy(context.Background(), x)
	if len(w2) != 1 {
		t.Fatalf("warnings = %v", w2)
	}
	if got := cpy2.(*ProtectEffect); len(got.Bypass) != 0 {
		t.Errorf("unresolvable bypass must be empty: %+v", got)
	}
	if len(pe2.Bypass) != 1 {
		t.Error("receiver must not mutate")
	}
}

func TestRefKindHelpers(t *testing.T) {
	if !IsTeamRef("team:a/b") || IsTeamRef("role:a/b:c") || IsTeamRef("user:x") {
		t.Error("IsTeamRef broken")
	}
	if !IsRoleRef("role:a/b:c") || IsRoleRef("team:a/b") || IsRoleRef("group:g") {
		t.Error("IsRoleRef broken")
	}
}
