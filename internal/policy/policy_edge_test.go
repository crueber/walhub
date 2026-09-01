package policy

import (
	"context"
	"strings"
	"testing"
)

// Coverage-completion cases: error paths and glob-dialect edges the main
// tables do not reach. Every case here must still hold per the spec.

func TestParseErrorsExtra(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"protect payload not object", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": [1]}}]}`, "must be an object"},
		{"history payload null", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"history": null}}]}`, "must be an object"},
		{"history payload array", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"history": [1]}}]}`, "must be an object"},
		{"history allow_unrelated not bool", `{"version": 1, "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"history": {"allow_unrelated": "yes"}}}]}`, "allow_unrelated"},
		{"size payload null", `{"version": 1, "rules": [{"name": "r", "match": {"paths": ["vendor/**"]}, "effect": {"size": null}}]}`, "must be an object"},
		{"size blob_bytes not number", `{"version": 1, "rules": [{"name": "r", "match": {"paths": ["vendor/**"]}, "effect": {"size": {"blob_bytes": true}}}]}`, "blob_bytes"},
		{"size push_bytes not number", `{"version": 1, "rules": [{"name": "r", "match": {"paths": ["vendor/**"]}, "effect": {"size": {"push_bytes": "10MB"}}}]}`, "push_bytes"},
		{"protect bypass not array", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"bypass": "svc:ci"}}}]}`, "bypass"},
		{"protect bypass null is empty", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"bypass": null}}}]}`, ""},
		{"protect bypass bare tag", `{"version": 1, "rules": [{"name": "r", "effect": {"protect": {"bypass": ["@"]}}}]}`, "tag name must not be empty"},
		{"lockout mixed kinds are disjoint", `{"version": 1, "rules": [
			{"name": "a", "match": {"refs": ["refs/heads/**"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["svc:a"]}}},
			{"name": "b", "match": {"refs": ["refs/heads/main"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["group:b"]}}}]}`,
			"bypass lists are disjoint"},
		{"lockout with only-exclusion refs matches all", `{"version": 1, "rules": [
			{"name": "a", "match": {"refs": ["^refs/heads/tmp/**"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["svc:a"]}}},
			{"name": "b", "match": {"refs": ["refs/heads/main"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["svc:b"]}}}]}`,
			"bypass lists are disjoint"},
		{"history rule between protects does not disturb lockout", `{"version": 1, "rules": [
			{"name": "a", "match": {"refs": ["refs/heads/**"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["svc:a"]}}},
			{"name": "h", "match": {"refs": ["refs/**"]}, "effect": {"history": {"allowed_forwards": 1}}},
			{"name": "b", "match": {"refs": ["refs/heads/main"]}, "effect": {"protect": {"restricts": ["update"], "bypass": ["svc:b"]}}}]}`,
			"bypass lists are disjoint"},
		{"empty effect kind key is unknown", `{"version": 1, "rules": [{"name": "r", "effect": {"": {}}}]}`, "unknown effect kind"},
		{"group members null", `{"version": 1, "groups": [{"name": "g", "members": null}], "rules": []}`, ""},
		{"group name not string", `{"version": 1, "groups": [{"name": 7}], "rules": []}`, "name"},
		{"group entry not object", `{"version": 1, "groups": [7], "rules": []}`, "must be an object"},
		{"mode null", `{"version": 1, "rules": [{"name": "r", "mode": null, "effect": {"protect": {}}}]}`, "mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Parse: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestGlobEdges(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"refs/*", "refs/", true},     // trailing '*' matches empty segment
		{"refs/*", "refs/a/b", false}, // '*' still stops at '/'
		{"refs/heads/main", "refs/heads", false},
		{"v?", "", false},     // '?' with nothing left
		{"**x", "a/bx", true}, // '**' followed by literal
		{"**", "", true},      // '**' matches empty string
		{"*", "", true},       // '*' matches empty string
		{"?", "/", false},     // '?' never crosses '/'
		{"*", "a/b", false},   // '*' stops at '/'
		{"refs/**/x/y", "refs/a/x/y", true},
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestPatternsOverlapLiterals(t *testing.T) {
	if !patternsOverlap("refs/heads/main", "refs/heads/main") {
		t.Error("equal literals must overlap")
	}
	if patternsOverlap("refs/heads/main", "refs/heads/dev") {
		t.Error("different literals must be disjoint")
	}
	if !patternsOverlap("refs/heads/main", "refs/heads/*") {
		t.Error("literal must overlap a matching pattern")
	}
	if literalPrefix("refs/heads/main") != "refs/heads/main" {
		t.Error("literalPrefix of a glob-free pattern is the whole pattern")
	}
	if literalPrefix("refs/heads/*") != "refs/heads/" {
		t.Error("literalPrefix stops at the first glob metacharacter")
	}
}

func TestSpellingEqualKinds(t *testing.T) {
	if spellingEqual("svc:a", "group:b") {
		t.Error("different kinds must not equal")
	}
	if !spellingEqual("@tag:x", "@tag:x") {
		t.Error("same tag must equal")
	}
}

func TestNestedGroupCycle(t *testing.T) {
	d := mustParse(t, `{
	  "version": 1,
	  "groups": [{"name": "a", "members": ["group:b"]}, {"name": "b", "members": ["group:a", "svc:x"]}],
	  "rules": [{"name": "r", "match": {"refs": ["refs/**"]}, "effect": {"protect": {"bypass": ["group:b"]}}}]
	}`)
	ctx := context.Background()
	// svc:inner-member through the cycle: group:b → group:a → group:b (cycle
	// does not admit via the cycle, but the cycle member list is checked once).
	if got := Evaluate(ctx, d, Request{Principal: "dev@example.com", Ref: "refs/heads/x", Op: OpUpdate}); got.Allow {
		t.Error("cycle admitted a non-member")
	}
}
