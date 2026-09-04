package policy

import (
	"strings"
	"testing"
)

// require_checks inside protect (05 §6): strict parse, push-ignored,
// union-ready. Table-driven over the accept/reject matrix.

func TestProtectRequireChecksParse(t *testing.T) {
	parse := func(body string) (*ProtectEffect, error) {
		doc, err := Parse([]byte(`{"version":1,"rules":[{"name":"r","match":{"refs":["refs/heads/main"]},"effect":{"protect":` + body + `}}]}`))
		if err != nil {
			return nil, err
		}
		return doc.Rules[0].Protect(), nil
	}
	t.Run("absent is nil", func(t *testing.T) {
		pe, err := parse(`{"restricts":["update"]}`)
		if err != nil || pe.RequireChecks != nil {
			t.Fatalf("pe=%+v err=%v", pe, err)
		}
	})
	t.Run("valid list", func(t *testing.T) {
		pe, err := parse(`{"require_checks":["ci/build","lint"]}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(pe.RequireChecks) != 2 || pe.RequireChecks[0] != "ci/build" {
			t.Fatalf("pe=%+v", pe)
		}
	})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"null", `{"require_checks":null}`},
		{"empty", `{"require_checks":[]}`},
		{"too many", `{"require_checks":["` + strings.Join(make([]string, 33), `","`) + `"]}`},
		{"not an array", `{"require_checks":"ci"}`},
		{"bad charset", `{"require_checks":["has space"]}`},
		{"leading slash", `{"require_checks":["/ci"]}`},
		{"trailing slash", `{"require_checks":["ci/"]}`},
		{"dot json", `{"require_checks":["ci.json"]}`},
		{"too long", `{"require_checks":["` + strings.Repeat("x", 101) + `"]}`},
		{"duplicate", `{"require_checks":["ci","ci"]}`},
		{"non-string", `{"require_checks":[42]}`},
	} {
		t.Run("reject "+tc.name, func(t *testing.T) {
			if _, err := parse(tc.body); err == nil {
				t.Fatalf("accepted %s", tc.body)
			}
		})
	}
	t.Run("single context ok", func(t *testing.T) {
		pe, err := parse(`{"require_checks":["x"]}`)
		if err != nil || len(pe.RequireChecks) != 1 {
			t.Fatalf("pe=%+v err=%v", pe, err)
		}
	})
	t.Run("thirty-two ok", func(t *testing.T) {
		names := make([]string, 32)
		for i := range names {
			names[i] = "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		}
		pe, err := parse(`{"require_checks":["` + strings.Join(names, `","`) + `"]}`)
		if err != nil || len(pe.RequireChecks) != 32 {
			t.Fatalf("pe=%+v err=%v", pe, err)
		}
	})
}

func TestProtectRequireChecksPushIgnored(t *testing.T) {
	// The push half never consults RequireChecks: a rule carrying only
	// the gate (default restricts) still denies by restricts, and one
	// that does not restrict update allows it — the gate is merge-time
	// only (05 §6).
	doc, err := Parse([]byte(`{"version":1,"rules":[{"name":"g","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create"],"require_checks":["ci"]}}}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v := Evaluate(nil, doc, Request{Principal: "m", Ref: "refs/heads/main", Op: OpUpdate}); !v.Allow {
		t.Fatalf("update must pass the push half: %+v", v)
	}
	if v := Evaluate(nil, doc, Request{Principal: "m", Ref: "refs/heads/main", Op: OpCreate}); v.Allow {
		t.Fatalf("create must stay restricted: %+v", v)
	}
	// ExpandedCopy carries the list without expanding it (contexts are
	// not actor spellings).
	pe := doc.Rules[0].Protect()
	cpy, warnings := pe.ExpandedCopy(nil, nil)
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if got := cpy.(*ProtectEffect).RequireChecks; len(got) != 1 || got[0] != "ci" {
		t.Fatalf("copy: %+v", cpy)
	}
}

func TestValidCheckContext(t *testing.T) {
	for _, good := range []string{"ci", "ci/build", "a.b_c-d/e", strings.Repeat("x", 100)} {
		if err := ValidCheckContext(good); err != nil {
			t.Fatalf("good %q: %v", good, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 101), "/a", "a/", "a.json", "a b", "a;b"} {
		if err := ValidCheckContext(bad); err == nil {
			t.Fatalf("bad %q accepted", bad)
		}
	}
}
