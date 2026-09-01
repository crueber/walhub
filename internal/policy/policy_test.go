package policy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The doc's shipped repo policy (docs/POLICY.md "A repo people would
// actually ship") — pin it verbatim: load real JSON, drive
// (principal, ref, op) through the real matcher, assert allow / deny.
const shippedPolicyJSON = `{
  "version": 1,
  "groups": [
    { "name": "admins", "members": ["@okta:sre"] },
    { "name": "queue",  "members": ["svc:merge-queue"] }
  ],
  "rules": [
    {
      "name": "lock-main",
      "match": { "refs": ["refs/heads/main"] },
      "effect": {
        "protect": {
          "restricts": ["create", "delete", "update"],
          "bypass": ["group:admins", "group:queue"]
        }
      }
    },
    {
      "name": "tags-immutable",
      "match": { "refs": ["refs/tags/**", "^refs/tags/tmp/**"] },
      "effect": {
        "protect": {
          "restricts": ["update", "delete"],
          "bypass": ["group:admins"]
        }
      }
    },
    {
      "name": "reserve-queue-ns",
      "match": { "refs": ["refs/heads/mq/**"] },
      "effect": { "protect": { "bypass": ["group:queue", "group:admins"] } }
    }
  ]
}`

// The §14.1 envelope example, verbatim.
const envelopeExampleJSON = `{
  "version": 1,
  "groups": [{ "name": "admins", "members": ["@okta:sre"] }, { "name": "bots", "members": ["svc:ci"] }],
  "rules": [
    { "name": "lock-main",
      "match": { "refs": ["refs/heads/main"] },
      "effect": { "protect": { "restricts": ["create", "update", "delete"], "bypass": ["group:admins", "svc:merge-queue"] } } }
  ]
}`

func mustParse(t *testing.T, data string) *Document {
	t.Helper()
	d, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	return d
}

func verdicts(d *Document, reqs []Request) []Verdict {
	ctx := context.Background()
	out := make([]Verdict, len(reqs))
	for i, r := range reqs {
		out[i] = Evaluate(ctx, d, r)
	}
	return out
}

func TestParseShippedPolicy(t *testing.T) {
	d := mustParse(t, shippedPolicyJSON)
	if d.Version != 1 {
		t.Errorf("Version = %d, want 1", d.Version)
	}
	if len(d.Rules) != 3 {
		t.Fatalf("len(Rules) = %d, want 3", len(d.Rules))
	}
	names := make([]string, len(d.Rules))
	for i, r := range d.Rules {
		names[i] = r.Name
	}
	want := []string{"lock-main", "tags-immutable", "reserve-queue-ns"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("rules[%d].name = %q, want %q (order preserved)", i, names[i], want[i])
		}
	}
	if len(d.Groups) != 2 || d.Groups[0].Name != "admins" || d.Groups[1].Name != "queue" {
		t.Errorf("Groups = %+v", d.Groups)
	}
	if m, ok := d.Roster().Lookup("admins"); !ok || len(m) != 1 || m[0] != "@okta:sre" {
		t.Errorf("roster[admins] = %v, %v", m, ok)
	}
	// reserve-queue-ns omits restricts: defaults to all four ops.
	p := d.Rules[2].Protect()
	if p == nil {
		t.Fatalf("reserve-queue-ns effect is not protect")
	}
	wantOps := []string{OpCreate, OpUpdate, OpDelete, OpForcePush}
	if len(p.RestrictOps) != len(wantOps) {
		t.Fatalf("default restricts = %v, want %v", p.RestrictOps, wantOps)
	}
	for i := range wantOps {
		if p.RestrictOps[i] != wantOps[i] {
			t.Errorf("default restricts[%d] = %q, want %q", i, p.RestrictOps[i], wantOps[i])
		}
	}
}

func TestEvaluateShippedPolicy(t *testing.T) {
	d := mustParse(t, shippedPolicyJSON)
	tests := []struct {
		name     string
		req      Request
		allow    bool
		denyRule string
	}{
		// lock-main: only admins (via @okta:sre) and the merge queue may
		// create/update/delete main. force-push is a separate restricts enum
		// value and is NOT in this rule's list, so the derived op passes it.
		{"lock-main: admin updates main", Request{Principal: "alice@example.com", Tags: []string{"okta:sre"}, Ref: "refs/heads/main", Op: OpUpdate}, true, ""},
		{"lock-main: merge-queue bot updates main", Request{Principal: "svc:merge-queue", Ref: "refs/heads/main", Op: OpUpdate}, true, ""},
		{"lock-main: ordinary dev updates main", Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpUpdate}, false, "lock-main"},
		{"lock-main: dev deletes main", Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpDelete}, false, "lock-main"},
		{"lock-main: dev creates main", Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpCreate}, false, "lock-main"},
		{"lock-main: force-push is its own enum value, not restricted here", Request{Principal: "svc:merge-queue", Ref: "refs/heads/main", Op: OpUpdate, Force: true}, true, ""},
		{"lock-main: principal spelling is case-insensitive", Request{Principal: "SVC:Merge-Queue", Ref: "refs/heads/main", Op: OpUpdate}, true, ""},

		// tags-immutable: tags are still, except tmp tags and admins.
		{"tags: dev pushes a new tag (create not restricted)", Request{Principal: "dev@example.com", Ref: "refs/tags/v1.0", Op: OpCreate}, true, ""},
		{"tags: dev retags v1.0 (update)", Request{Principal: "dev@example.com", Ref: "refs/tags/v1.0", Op: OpUpdate}, false, "tags-immutable"},
		{"tags: dev deletes v1.0", Request{Principal: "dev@example.com", Ref: "refs/tags/v1.0", Op: OpDelete}, false, "tags-immutable"},
		{"tags: admin retags v1.0", Request{Principal: "alice@example.com", Tags: []string{"okta:sre"}, Ref: "refs/tags/v1.0", Op: OpUpdate}, true, ""},
		{"tags: tmp tags excluded from the rule", Request{Principal: "dev@example.com", Ref: "refs/tags/tmp/scratch", Op: OpDelete}, true, ""},
		{"tags: tag retarget maps to force-push, which the rule does not restrict", Request{Principal: "dev@example.com", Ref: "refs/tags/v1.0", Op: OpUpdate, Force: true}, true, ""},

		// reserve-queue-ns: the mq namespace is for the queue (and admins);
		// any other op by others is denied (default restrict-all).
		{"queue ns: bot updates mq branch", Request{Principal: "svc:merge-queue", Ref: "refs/heads/mq/pr-42", Op: OpUpdate}, true, ""},
		{"queue ns: bot force-pushes mq branch", Request{Principal: "svc:merge-queue", Ref: "refs/heads/mq/pr-42", Op: OpUpdate, Force: true}, true, ""},
		{"queue ns: bot deletes mq branch", Request{Principal: "svc:merge-queue", Ref: "refs/heads/mq/pr-42", Op: OpDelete}, true, ""},
		{"queue ns: admin creates mq branch", Request{Principal: "alice@example.com", Tags: []string{"okta:sre"}, Ref: "refs/heads/mq/pr-42", Op: OpCreate}, true, ""},
		{"queue ns: ordinary dev denied on mq branch", Request{Principal: "dev@example.com", Ref: "refs/heads/mq/pr-42", Op: OpUpdate}, false, "reserve-queue-ns"},

		// Branches outside main and mq/** are untouched by all rules.
		{"unmatched: dev pushes feature branch", Request{Principal: "dev@example.com", Ref: "refs/heads/feature/x", Op: OpUpdate, Force: true}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(context.Background(), d, tt.req)
			if got.Allow != tt.allow {
				t.Errorf("Evaluate(%+v) = allow=%v rule=%q, want allow=%v", tt.req, got.Allow, got.Rule, tt.allow)
			}
			if !tt.allow && got.Rule != tt.denyRule {
				t.Errorf("deny rule = %q, want %q (wire: rejected by rule '%s')", got.Rule, tt.denyRule, tt.denyRule)
			}
			if tt.allow && got.Rule != "" {
				t.Errorf("allow verdict names rule %q", got.Rule)
			}
		})
	}
}

// AND-combination across matching protect rules: overlap is AND, and a
// rule is bypassed only if THAT rule's bypass matches.
func TestProtectCombinesWithAnd(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "rules": [
	    { "name": "freeze", "match": { "refs": ["refs/heads/**"] },
	      "effect": { "protect": { "bypass": ["svc:release-bot", "svc:mq"] } } },
	    { "name": "qa-branch", "match": { "refs": ["refs/heads/qa/**"] },
	      "effect": { "protect": { "bypass": ["svc:qa-bot", "svc:mq"] } } }
	  ]
	}`)
	tests := []struct {
		name  string
		req   Request
		allow bool
		rule  string
	}{
		{"qa rule's own bypass matches, freeze's does not", Request{Principal: "svc:qa-bot", Ref: "refs/heads/qa/x", Op: OpUpdate}, false, "freeze"},
		{"release-bot bypasses freeze but not qa-branch", Request{Principal: "svc:release-bot", Ref: "refs/heads/qa/x", Op: OpUpdate}, false, "qa-branch"},
		{"both rules' bypasses match → allowed", Request{Principal: "svc:mq", Ref: "refs/heads/qa/x", Op: OpUpdate}, true, ""},
		{"only freeze matches, release-bot passes", Request{Principal: "svc:release-bot", Ref: "refs/heads/main", Op: OpUpdate}, true, ""},
		{"first denying rule in file order is named", Request{Principal: "dev@example.com", Ref: "refs/heads/qa/x", Op: OpDelete}, false, "freeze"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(context.Background(), d, tt.req)
			if got.Allow != tt.allow || (!tt.allow && got.Rule != tt.rule) {
				t.Errorf("Evaluate(%+v) = %+v, want allow=%v rule=%q", tt.req, got, tt.allow, tt.rule)
			}
		})
	}
}

func TestUpdateForceBecomesForcePushOp(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "rules": [
	    { "name": "no-force", "match": { "refs": ["refs/heads/main"] },
	      "effect": { "protect": { "restricts": ["force-push"] } } }
	  ]
	}`)
	ctx := context.Background()
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpUpdate, Force: true}); got.Allow {
		t.Error("update+Force must evaluate as force-push")
	}
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpUpdate}); !got.Allow {
		t.Errorf("plain fast-forward update denied: %+v", got)
	}
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpForcePush}); got.Allow {
		t.Error("explicit force-push op must be denied")
	}
	// Explicit op=force-push + Force must not double-map.
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpForcePush, Force: true}); got.Allow {
		t.Error("explicit force-push with Force denied")
	}
}

func TestAllowAllDefaults(t *testing.T) {
	ctx := context.Background()
	req := Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpDelete}
	// Missing file = allow-all.
	if got := Evaluate(ctx, nil, req); !got.Allow {
		t.Errorf("nil document: %+v", got)
	}
	for _, doc := range []string{`{"version": 1, "rules": []}`, `{"version": 1, "groups": [], "rules": []}`} {
		d := mustParse(t, doc)
		if got := Evaluate(ctx, d, req); !got.Allow {
			t.Errorf("%s: %+v", doc, got)
		}
	}
}

func TestEnvelopeRollingUpgradeKeysIgnored(t *testing.T) {
	// Unknown keys BESIDE groups/rules/version are ignored (fleet rolling
	// upgrade); _comment is legal everywhere and never read.
	d := mustParse(t, `{
	  "version": 1,
	  "_comment": "top-level comment",
	  "future_knob": {"anything": true},
	  "groups": [{"name": "g", "_comment": "group comment", "members": ["svc:ci"]}],
	  "rules": [
	    { "name": "r", "_comment": "rule comment",
	      "match": {"refs": ["refs/heads/main"], "_comment": "match comment"},
	      "effect": {"protect": {"restricts": ["delete"], "bypass": ["svc:ci"], "_comment": "effect comment"}} }
	  ]
	}`)
	if len(d.Rules) != 1 || d.Rules[0].Name != "r" {
		t.Fatalf("rules = %+v", d.Rules)
	}
	if got := Evaluate(context.Background(), d, Request{Principal: "svc:ci", Ref: "refs/heads/main", Op: OpDelete}); !got.Allow {
		t.Errorf("bypass via roster failed: %+v", got)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string // substring of the error
	}{
		{"not json", `nonsense`, "envelope"},
		{"version absent", `{"rules": []}`, "version"},
		{"version 2", `{"version": 2, "rules": []}`, "version"},
		{"version string", `{"version": "1", "rules": []}`, "version"},
		{"version null", `{"version": null, "rules": []}`, "version"},
		{"rules not array", `{"version": 1, "rules": {}}`, "rules"},
		{"rule not object", `{"version": 1, "rules": [7]}`, "rules[0]"},
		{"rule missing name", `{"version": 1, "rules": [{"match": {}, "effect": {"protect": {}}}]}`, "missing name"},
		{"rule bad name caps", `{"version": 1, "rules": [{"name": "Lock-Main", "effect": {"protect": {}}}]}`, "name"},
		{"rule bad name leading digit", `{"version": 1, "rules": [{"name": "1lock", "effect": {"protect": {}}}]}`, "name"},
		{"rule name too long", `{"version": 1, "rules": [{"name": "` + strings.Repeat("a", 64) + `", "effect": {"protect": {}}}]}`, "name"},
		{"rule duplicate name", `{"version": 1, "rules": [
			{"name": "r", "effect": {"protect": {}}},
			{"name": "r", "effect": {"protect": {}}}]}`, "duplicate"},
		{"rule unknown key inside", `{"version": 1, "rules": [{"name": "r", "match": {}, "effect": {"protect": {}}, "mod": "enforce"}]}`, "unknown key"},
		{"rule missing effect", `{"version": 1, "rules": [{"name": "r", "match": {}}]}`, "missing effect"},
		{"rule bad mode", `{"version": 1, "rules": [{"name": "r", "mode": "off", "effect": {"protect": {}}}]}`, "mode"},
		{"match unknown key", `{"version": 1, "rules": [{"name": "r", "match": {"refs_x": []}, "effect": {"protect": {}}}]}`, "unknown key"},
		{"match refs not strings", `{"version": 1, "rules": [{"name": "r", "match": {"refs": [1]}, "effect": {"protect": {}}}]}`, "refs"},
		{"match refs empty pattern", `{"version": 1, "rules": [{"name": "r", "match": {"refs": [""]}, "effect": {"protect": {}}}]}`, "empty"},
		{"match principals bad spelling", `{"version": 1, "rules": [{"name": "r", "match": {"principals": ["^"]}, "effect": {"protect": {}}}]}`, "spelling"},
		{"effect not object", `{"version": 1, "rules": [{"name": "r", "effect": "protect"}]}`, "effect"},
		{"effect two keys", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {}, "size": {}}}]}`, "exactly one"},
		{"effect empty", `{"version": 1, "rules": [{"name": "r", "effect": {}}]}`, "exactly one"},
		{"effect unknown kind", `{"version": 1, "rules": [{"name": "r", "effect": {"protekt": {}}}]}`, "unknown effect kind"},
		{"effect unknown kind fail closed", `{"version": 1, "rules": [{"name": "r", "effect": {"review-required": {"bypass": ["svc:mq"]}}}]}`, "unknown effect kind"},
		{"protect restricts null", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"restricts": null}}}]}`, "restricts"},
		{"protect restricts empty", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"restricts": []}}}]}`, "restricts"},
		{"protect restricts bad op", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"restricts": ["Create"]}}}]}`, "restricts"},
		{"protect restricts not array", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"restricts": "delete"}}}]}`, "restricts"},
		{"protect unknown key", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"bypass_actrs": ["svc:ci"]}}}]}`, "unknown key"},
		{"protect bypass bad spelling", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"bypass": ["^dev@x"]}}}]}`, "exclusions"},
		{"protect bypass empty name", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"bypass": ["group:"]}}}]}`, "empty"},
		{"history unknown key", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"history": {"allowd_forwards": 5}}}]}`, "unknown key"},
		{"history bad type", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"history": {"allowed_forwards": "x"}}}]}`, "allowed_forwards"},
		{"history negative", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"history": {"allowed_forwards": -1}}}]}`, "allowed_forwards"},
		{"size unknown key", `{"version": 1, "rules": [{"name": "r", "match": {"paths": ["vendor/**"]}, "effect": {"size": {"blob_byte": 5}}}]}`, "unknown key"},
		{"size negative", `{"version": 1, "rules": [{"name": "r", "match": {"paths": ["vendor/**"]}, "effect": {"size": {"push_bytes": -5}}}]}`, "push_bytes"},
		{"size absent paths", `{"version": 1, "rules": [{"name": "r", "effect": {"size": {"blob_bytes": 10}}}]}`, "paths"},
		{"size empty paths", `{"version": 1, "rules": [{"name": "r", "match": {"paths": []}, "effect": {"size": {"blob_bytes": 10}}}]}`, "paths"},
		{"history exclusion refused", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["^refs/heads/tmp/**"]}, "effect": {"history": {"allowed_forwards": 5}}}]}`, "exclusion"},
		{"size exclusion refused", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"], "paths": ["^vendor/**"]}, "effect": {"size": {"blob_bytes": 10}}}]}`, "exclusion"},
		{"groups not array", `{"version": 1, "groups": {"admins": []}, "rules": []}`, "groups"},
		{"group unknown key", `{"version": 1, "groups": [{"name": "g", "membres": ["svc:ci"]}], "rules": []}`, "unknown key"},
		{"group missing name", `{"version": 1, "groups": [{"members": []}], "rules": []}`, "name"},
		{"group duplicate name", `{"version": 1, "groups": [{"name": "g"}, {"name": "g"}], "rules": []}`, "duplicate"},
		{"group members not strings", `{"version": 1, "groups": [{"name": "g", "members": [1]}], "rules": []}`, "members"},
		{"group member empty", `{"version": 1, "groups": [{"name": "g", "members": [""]}], "rules": []}`, "empty"},
		{"lockout disjoint bypass", `{"version": 1, "rules": [
			{"name": "a", "match": {"refs": ["refs/heads/**"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["group:one"]}}},
			{"name": "b", "match": {"refs": ["refs/heads/main"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["group:two"]}}}]}`,
			"bypass lists are disjoint"},
		{"lockout bypass groups are different spellings", `{"version": 1, "groups": [
			{"name": "one", "members": ["svc:bot"]}, {"name": "two", "members": ["svc:bot"]}],
			"rules": [
			{"name": "a", "match": {"refs": ["refs/heads/**"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["group:one"]}}},
			{"name": "b", "match": {"refs": ["refs/heads/maim/**"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["group:two"]}}}]}`,
			"bypass lists are disjoint"}, // static analysis cannot prove group:one == group:two
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Parse([]byte(tt.doc))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Parse: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Parse: expected error containing %q, got document %+v", tt.want, d)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid (fail closed)", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// Overlapping-protect disjoint-bypass lockout: the exact §14.4 load check.
func TestLockoutCheck(t *testing.T) {
	base := func(refsA, refsB string, bypassA, bypassB string) string {
		return `{"version": 1, "rules": [
		  {"name": "a", "match": {"refs": [` + refsA + `]}, "effect": {"protect": {"restricts": ["update", "delete"], "bypass": [` + bypassA + `]}}},
		  {"name": "b", "match": {"refs": [` + refsB + `]}, "effect": {"protect": {"restricts": ["delete", "force-push"], "bypass": [` + bypassB + `]}}}
		]}`
	}
	tests := []struct {
		name    string
		doc     string
		fails   bool
		wantSub string
	}{
		{"overlapping refs+ops, disjoint bypass → load fails",
			base(`"refs/heads/**"`, `"refs/heads/main"`, `"svc:a"`, `"svc:b"`), true, "disjoint"},
		{"disjoint refs → fine",
			base(`"refs/heads/main"`, `"refs/heads/dev"`, `"svc:a"`, `"svc:b"`), false, ""},
		{"same refs, disjoint ops → fine",
			`{"version": 1, "rules": [
			   {"name": "a", "match": {"refs": ["refs/heads/main"]}, "effect": {"protect": {"restricts": ["create"], "bypass": ["svc:a"]}}},
			   {"name": "b", "match": {"refs": ["refs/heads/main"]}, "effect": {"protect": {"restricts": ["delete"], "bypass": ["svc:b"]}}}
			 ]}`, false, ""},
		{"intersecting bypass → fine",
			base(`"refs/heads/**"`, `"refs/heads/main"`, `"svc:shared"`, `"SVC:SHARED"`), false, ""},
		{"empty bypass on one side → intended hard lock, fine",
			base(`"refs/heads/**"`, `"refs/heads/main"`, `"svc:a"`, ``), false, ""},
		{"both empty bypass → intended hard lock, fine",
			base(`"refs/heads/**"`, `"refs/heads/main"`, ``, ``), false, ""},
		{"glob prefix overlap is overlap",
			base(`"refs/heads/release-*"`, `"refs/heads/release-1"`, `"svc:a"`, `"svc:b"`), true, "disjoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if tt.fails {
				if err == nil {
					t.Fatal("expected load failure")
				}
				if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("error %q missing %q", err, tt.wantSub)
				}
			} else if err != nil {
				t.Fatalf("unexpected load failure: %v", err)
			}
		})
	}
}

// group: resolves at eval, not parse — the roster index is consulted per
// push, so an unresolvable include does not admit and an unresolvable
// exclude still excludes.
func TestGroupResolutionSemantics(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "groups": [{"name": "bots", "members": ["svc:ci", "group:nested"]},
	             {"name": "nested", "members": ["svc:inner"]}],
	  "rules": [
	    {"name": "inc", "match": {"refs": ["refs/heads/a"]}, "effect": {"protect": {"bypass": ["group:missing"]}}},
	    {"name": "exc", "match": {"refs": ["refs/heads/b"], "principals": ["^group:missing"]}, "effect": {"protect": {"bypass": ["svc:x"]}}}
	  ]
	}`)
	ctx := context.Background()
	tests := []struct {
		name  string
		req   Request
		allow bool
		rule  string
	}{
		// inc: bypass list is group:missing — unresolvable include does not
		// admit, so everyone is denied on refs/heads/a.
		{"unresolvable include denies bot", Request{Principal: "svc:ci", Ref: "refs/heads/a", Op: OpUpdate}, false, "inc"},
		// exc: the exclusion ^group:missing is unresolvable — it still
		// excludes, so nobody matches principals and the rule never fires.
		{"unresolvable exclude still excludes", Request{Principal: "svc:ci", Ref: "refs/heads/b", Op: OpUpdate}, true, ""},
		{"nested group resolves through roster", Request{Principal: "svc:inner", Ref: "refs/heads/z", Op: OpUpdate}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(ctx, d, tt.req)
			if got.Allow != tt.allow || (!tt.allow && got.Rule != tt.rule) {
				t.Errorf("Evaluate(%+v) = %+v, want allow=%v rule=%q", tt.req, got, tt.allow, tt.rule)
			}
		})
	}
}

func TestTagAndExactPrincipalSpellings(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "rules": [
	    {"name": "tags", "match": {"principals": ["@okta:sre"]}, "effect": {"protect": {"bypass": []}}},
	    {"name": "exact", "match": {"principals": ["Alice@Example.com"]}, "effect": {"protect": {"bypass": []}}}
	  ]
	}`)
	ctx := context.Background()
	tests := []struct {
		name  string
		req   Request
		allow bool
	}{
		{"edge-bound tag matches", Request{Principal: "a@b", Tags: []string{"okta:sre"}, Ref: "refs/heads/x", Op: OpUpdate}, false},
		{"other tag does not", Request{Principal: "a@b", Tags: []string{"okta:other"}, Ref: "refs/heads/x", Op: OpUpdate}, true},
		{"exact principal case-insensitive", Request{Principal: "alice@example.com", Ref: "refs/heads/y", Op: OpUpdate}, false},
		{"different principal does not match", Request{Principal: "bob@example.com", Ref: "refs/heads/y", Op: OpUpdate}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(ctx, d, tt.req)
			if got.Allow != tt.allow {
				t.Errorf("Evaluate(%+v) = %+v, want allow=%v", tt.req, got, tt.allow)
			}
		})
	}
}

func TestGlobDialect(t *testing.T) {
	tests := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/main", "refs/heads/mainx", false},
		{"refs/heads/*", "refs/heads/main", true},
		{"refs/heads/*", "refs/heads/feature/x", false}, // '*' stops at '/'
		{"refs/heads/**", "refs/heads/feature/x", true}, // '**' crosses
		{"refs/heads/**", "refs/heads", false},
		{"refs/heads/tmp/**", "refs/heads/tmp/a/b", true},
		{"refs/tags/v?", "refs/tags/v1", true},
		{"refs/tags/v?", "refs/tags/v", false},
		{"refs/tags/v?", "refs/tags/v/1", false}, // '?' stops at '/'
		{"**", "refs/heads/deep/x", true},
		{"refs/**/x", "refs/a/b/x", true}, // '**/' spans segments
		{"refs/**/x", "refs/x", true},     // '**/' spans zero segments
		{"refs/*/x", "refs/a/b/x", false}, // '*/' spans exactly one segment
		{"", "refs/heads/main", false},
		{"*oid", "refs/oid", false}, // single '*' stops at '/'
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestMatchKeysAndValuesOr(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "groups": [{"name": "bots", "members": ["svc:ci"]}],
	  "rules": [
	    {"name": "both-keys", "match": {"refs": ["refs/heads/main", "refs/heads/release/**"], "principals": ["group:bots"]},
	     "effect": {"protect": {"bypass": []}}}
	  ]
	}`)
	ctx := context.Background()
	tests := []struct {
		name  string
		req   Request
		allow bool
	}{
		{"ref matches, principal matches → rule applies", Request{Principal: "svc:ci", Ref: "refs/heads/main", Op: OpDelete}, false},
		{"second refs value matches → rule applies", Request{Principal: "svc:ci", Ref: "refs/heads/release/1", Op: OpDelete}, false},
		{"ref matches, principal does not → no rule", Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpDelete}, true},
		{"principal matches, ref does not → no rule", Request{Principal: "svc:ci", Ref: "refs/heads/other", Op: OpDelete}, true},
		{"absent match keys are catch-all", Request{Principal: "svc:ci", Ref: "refs/notes/x", Op: OpDelete}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(ctx, d, tt.req)
			if got.Allow != tt.allow {
				t.Errorf("Evaluate(%+v) = %+v, want allow=%v", tt.req, got, tt.allow)
			}
		})
	}
}

// history/size are parsed and stored, NOT enforced — expose them on the
// struct.
func TestHistorySizeParsedNotEnforced(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "rules": [
	    {"name": "floors", "match": {"refs": ["refs/heads/**"]},
	     "effect": {"history": {"allowed_forwards": 50000, "allow_unrelated": false}}},
	    {"name": "ceilings", "match": {"refs": ["refs/heads/**"], "paths": ["vendor/**"]},
	     "effect": {"size": {"blob_bytes": 10485760, "push_bytes": 104857600}}}
	  ]
	}`)
	ctx := context.Background()
	// These must NOT change a verdict.
	for _, req := range []Request{
		{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpUpdate},
		{Principal: "dev@example.com", Ref: "refs/heads/vendor/x", Op: OpDelete},
	} {
		if got := Evaluate(ctx, d, req); !got.Allow {
			t.Errorf("history/size enforced on %+v: %+v", req, got)
		}
	}
	hs := d.Histories()
	if len(hs) != 1 || hs[0].AllowedForwards == nil || *hs[0].AllowedForwards != 50000 {
		t.Fatalf("Histories() = %+v", hs)
	}
	if hs[0].AllowUnrelated == nil || *hs[0].AllowUnrelated {
		t.Errorf("AllowUnrelated = %v", *hs[0].AllowUnrelated)
	}
	sizes := d.Sizes()
	if len(sizes) != 1 || sizes[0].BlobBytes == nil || *sizes[0].BlobBytes != 10485760 ||
		sizes[0].PushBytes == nil || *sizes[0].PushBytes != 104857600 {
		t.Fatalf("Sizes() = %+v", sizes)
	}
}

func TestEnvelopeExampleVerbatim(t *testing.T) {
	d := mustParse(t, envelopeExampleJSON)
	ctx := context.Background()
	if got := Evaluate(ctx, d, Request{Principal: "svc:merge-queue", Ref: "refs/heads/main", Op: OpUpdate}); !got.Allow {
		t.Errorf("exact bypass principal denied: %+v", got)
	}
	if got := Evaluate(ctx, d, Request{Principal: "a@b", Tags: []string{"okta:sre"}, Ref: "refs/heads/main", Op: OpUpdate}); !got.Allow {
		t.Error("group:admins member (@tag via roster) not admitted")
	}
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpCreate}); got.Allow {
		t.Error("create on main allowed")
	}
}

func TestTypedEffectAccessors(t *testing.T) {
	d := mustParse(t, shippedPolicyJSON)
	if d.Rules[0].Protect() == nil || d.Rules[0].History() != nil || d.Rules[0].Size() != nil {
		t.Errorf("typed accessors wrong on lock-main")
	}
}

func TestRuleCommentAndModeStored(t *testing.T) {
	d := mustParse(t, `{"version": 1, "rules": [
	  {"name": "r", "_comment": "why", "mode": "audit", "effect": {"protect": {}}} ]}`)
	if d.Rules[0].Comment != "why" || d.Rules[0].Mode != "audit" {
		t.Errorf("Comment/Mode not stored: %+v", d.Rules[0])
	}
}

func TestRuleNameRegexBoundary(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"a", "z9", "a-b-c", strings.Repeat("a", 63)} {
		d, err := Parse([]byte(`{"version": 1, "rules": [{"name": "` + name + `", "effect": {"protect": {}}}]}`))
		if err != nil {
			t.Errorf("name %q rejected: %v", name, err)
			continue
		}
		if got := Evaluate(ctx, d, Request{Principal: "svc:ci", Ref: "refs/heads/x", Op: OpUpdate}); got.Allow {
			t.Errorf("name %q: protect-all rule did not deny", name)
		}
	}
	for _, name := range []string{"", "A", "1a", "-a", "a_b", "a b", strings.Repeat("a", 64), "a-B"} {
		if _, err := Parse([]byte(`{"version": 1, "rules": [{"name": "` + name + `", "effect": {"protect": {}}}]}`)); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q accepted: %v", name, err)
		}
	}
}

// Registry laws: duplicate kinds panic; prototypes must be struct pointers;
// unknown kinds are parse errors; Parse is strict.
func TestEffectRegistry(t *testing.T) {
	if _, dup := effectRegistry["protect"]; !dup {
		t.Fatal("protect not registered")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("duplicate effect kind did not panic")
		}
	}()
	RegisterEffect((*ProtectEffect)(nil))
}

func TestEffectRegistryBadPrototype(t *testing.T) {
	tests := []struct {
		name string
		e    Effect
	}{
		{"nil prototype", nil},
		{"non-pointer struct", fakeEffect{}},
		{"pointer to non-struct", func() Effect { var i int; return (*intAsEffect)(&i) }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("bad prototype did not panic")
				}
			}()
			RegisterEffect(tt.e)
		})
	}
}

type fakeEffect struct{}

func (fakeEffect) Kind() string { return "fake" }
func (fakeEffect) Parse(raw json.RawMessage) error {
	return nil // a plain (non-push) effect parses; it never decides a verdict
}

type intAsEffect int

func (intAsEffect) Kind() string { return "int" }
func (intAsEffect) Parse(raw json.RawMessage) error {
	return invalidf("effect int: not parseable")
}

// Extension effects: registration makes the kind loadable; a PushEffect
// participates in eval; a plain Effect is parsed and stored but never
// decides a verdict.
func TestRegisteredExtensionEffect(t *testing.T) {
	RegisterEffect(&fakePushEffect{})
	t.Cleanup(func() { delete(effectRegistry, "fake-push") })

	d := mustParse(t, `{"version": 1, "rules": [
	  {"name": "gate", "match": {"refs": ["refs/heads/main"]}, "effect": {"fake-push": {"reviewer": "svc:mq"}}} ]}`)
	ctx := context.Background()

	// deny when the update principal is not the reviewer.
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpUpdate}); got.Allow || got.Rule != "gate" {
		t.Errorf("fake-push effect not evaluated: %+v", got)
	}
	if got := Evaluate(ctx, d, Request{Principal: "svc:reviewer", Ref: "refs/heads/main", Op: OpUpdate}); !got.Allow {
		t.Errorf("fake-push bypass not honored: %+v", got)
	}
	if got := Evaluate(ctx, d, Request{Principal: "svc:reviewer", Ref: "refs/heads/dev", Op: OpUpdate}); !got.Allow {
		t.Errorf("match.refs not applied for extension effect: %+v", got)
	}

	// Plain (non-push) effects parse but never deny.
	RegisterEffect(&fakeEffect{})
	t.Cleanup(func() { delete(effectRegistry, "fake") })
	d2, err := Parse([]byte(`{"version": 1, "rules": [
	  {"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"fake": {}}} ]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := Evaluate(ctx, d2, Request{Principal: "dev@example.com", Ref: "refs/heads/main", Op: OpUpdate}); !got.Allow {
		t.Errorf("non-push effect decided a verdict: %+v", got)
	}
}

type fakePushEffect struct{}

func (fakePushEffect) Kind() string { return "fake-push" }
func (fakePushEffect) Parse(raw json.RawMessage) error {
	m, err := effectObject(raw, "fake-push")
	if err != nil {
		return err
	}
	for k := range m {
		switch k {
		case "reviewer", "_comment":
		default:
			return invalidf("effect fake-push: unknown key %q", k)
		}
	}
	return nil
}
func (fakePushEffect) Evaluate(u Request, g Groups) Verdict {
	if matchActorList([]string{"svc:reviewer"}, Actor{Principal: u.Principal, Tags: u.Tags}, g) {
		return Verdict{Allow: true}
	}
	return Verdict{Allow: false}
}
