package policy

import (
	"context"
	"testing"
)

func TestRequiredReviewsParse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   string
		ok    bool
		min   int
		stale bool
	}{
		{"full", `{"version":1,"rules":[{"name":"pr-gate","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2,"dismiss_stale":true,"bypass":["svc:merge-queue"]}}}]}`,
			true, 2, true},
		{"minimal", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"min_approvals":1}}}]}`,
			true, 1, false},
		{"missing min", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"dismiss_stale":true}}}]}`,
			false, 0, false},
		{"zero min", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"min_approvals":0}}}]}`,
			false, 0, false},
		{"fractional min", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"min_approvals":1.5}}}]}`,
			false, 0, false},
		{"unknown key", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"min_approvals":1,"owners":["x"]}}}]}`,
			false, 0, false},
		{"null effect", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":null}}]}`,
			false, 0, false},
		{"bad bypass", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"min_approvals":1,"bypass":["^x"]}}}]}`,
			false, 0, false},
		{"bad stale type", `{"version":1,"rules":[{"name":"g","effect":{"required-reviews":{"min_approvals":1,"dismiss_stale":"yes"}}}]}`,
			false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.doc))
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
			if !tc.ok {
				return
			}
			e := doc.Rules[0].RequiredReviews()
			if e == nil || e.MinApprovals != tc.min || e.DismissStale != tc.stale {
				t.Fatalf("%+v", e)
			}
		})
	}
}

func TestRequiredReviewsPushHalf(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"rules":[
		{"name":"pr-gate","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2,"bypass":["svc:merge-queue"]}}},
		{"name":"open","match":{"refs":["refs/heads/topic"]},"effect":{"protect":{"restricts":["update"]}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		principal, ref, op string
		allow              bool
		rule               string
	}{
		{"alice", "refs/heads/main", "update", false, "pr-gate"},
		{"alice", "refs/heads/main", "create", false, "pr-gate"},
		{"svc:merge-queue", "refs/heads/main", "update", true, ""},
		{"alice", "refs/heads/topic", "update", false, "open"},
		{"alice", "refs/heads/other", "update", true, ""},
	} {
		v := Evaluate(ctx, doc, Request{Principal: tc.principal, Ref: tc.ref, Op: tc.op})
		if v.Allow != tc.allow || (!v.Allow && v.Rule != tc.rule) {
			t.Errorf("%s %s %s = %+v, want allow=%v rule=%q", tc.principal, tc.ref, tc.op, v, tc.allow, tc.rule)
		}
	}
}

func TestMatchingRulesBypassed(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"groups":[{"name":"admins","members":["erin"]}],
		"rules":[{"name":"pr-gate","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2,"bypass":["group:admins","svc:merge-queue"]}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := MatchingRules(doc, Request{Principal: "erin", Ref: "refs/heads/main", Op: OpUpdate})
	if len(got) != 1 || got[0].RequiredReviews() == nil {
		t.Fatalf("%+v", got)
	}
	if n := len(MatchingRules(doc, Request{Principal: "erin", Ref: "refs/heads/other", Op: OpUpdate})); n != 0 {
		t.Fatalf("unmatched: %d", n)
	}
	if MatchingRules(nil, Request{}) != nil {
		t.Fatalf("nil doc")
	}
	e := got[0].RequiredReviews()
	if !Bypassed(e.Bypass, "erin", nil, doc.Roster()) {
		t.Fatalf("group bypass should admit erin")
	}
	if !Bypassed(e.Bypass, "svc:merge-queue", nil, doc.Roster()) {
		t.Fatalf("exact bypass should admit the bot")
	}
	if Bypassed(e.Bypass, "alice", nil, doc.Roster()) {
		t.Fatalf("alice bypassed")
	}
	if Bypassed(nil, "alice", nil, doc.Roster()) {
		t.Fatalf("empty bypass admitted")
	}
}

func TestEvaluateProtectSkipsObservationEffects(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"rules":[
		{"name":"pr-gate","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2}}},
		{"name":"lock","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["delete"]}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The required-reviews push-time half does NOT deny here: this is the
	// server-side publisher's check (merge task), whose required-reviews
	// verdict comes from the review gate, not the push eval.
	if v := EvaluateProtect(ctx, doc, Request{Principal: "alice", Ref: "refs/heads/main", Op: OpUpdate}); !v.Allow {
		t.Fatalf("update denied by observation effect: %+v", v)
	}
	// Protect still enforces its own ops.
	if v := EvaluateProtect(ctx, doc, Request{Principal: "alice", Ref: "refs/heads/main", Op: OpDelete}); v.Allow || v.Rule != "lock" {
		t.Fatalf("delete not denied: %+v", v)
	}
	// Empty/nil documents allow.
	if v := EvaluateProtect(ctx, nil, Request{}); !v.Allow {
		t.Fatalf("nil denied: %+v", v)
	}
	// The full Evaluate DOES apply the push-time half (receive-pack's
	// enforcement point, once the push pipeline consults policy).
	if v := Evaluate(ctx, doc, Request{Principal: "alice", Ref: "refs/heads/main", Op: OpUpdate}); v.Allow || v.Rule != "pr-gate" {
		t.Fatalf("push eval missed the gate: %+v", v)
	}
}
