package pulls

import (
	"encoding/json"
	"strings"
	"testing"
)

// markMerged stamps pr.json merged (the merge task's outcome write, minus the
// git — ListPRs only reads the sidecar, so the flag path needs no merge).
func markMerged(t *testing.T, e *testEnv, owner, repo string, num int) {
	t.Helper()
	pr, ver, err := e.svc.loadPR(ctx(), owner, repo, num)
	if err != nil || pr == nil {
		t.Fatalf("loadPR %d: %v", num, err)
	}
	pr.Merged = true
	if err := e.svc.savePR(ctx(), owner, repo, pr, ver); err != nil {
		t.Fatalf("savePR merged %d: %v", num, err)
	}
}

// TestListPRsMergedFlag pins Forgejo #530: PROut carries the pr.json merge
// outcome so the pulls list can render the merged chip without any extra
// round trip (the flag rides the per-row sidecar read ListPRs already pays).
func TestListPRsMergedFlag(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main": hexSHA(1),
		"refs/heads/a":    hexSHA(2),
		"refs/heads/b":    hexSHA(3),
	})
	for _, head := range []string{"refs/heads/a", "refs/heads/b"} {
		if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{
			Title: "pr " + head, BaseRef: "refs/heads/main", HeadRef: head,
		}, ""); err != nil {
			t.Fatalf("OpenPR: %v", err)
		}
	}
	// Close #2 (unmerged close is legal), then land the merge outcome on it.
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 2, writer(), PRPatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("UpdatePR close: %v", err)
	}
	markMerged(t, e, "o", "r", 2)

	tests := []struct {
		name   string
		filter ListFilter
		num    int
		merged bool
	}{
		{name: "open unmerged row reads false", filter: ListFilter{}, num: 1, merged: false},
		{name: "merged row reads true", filter: ListFilter{}, num: 2, merged: true},
		{name: "closed filter keeps the flag", filter: ListFilter{State: "closed"}, num: 2, merged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.svc.ListPRs(ctx(), "o", "r", writer(), tc.filter)
			if err != nil {
				t.Fatalf("ListPRs: %v", err)
			}
			var got *PROut
			for i := range res.Pulls {
				if res.Pulls[i].Num == tc.num {
					got = &res.Pulls[i]
				}
			}
			if got == nil {
				t.Fatalf("PR #%d missing from list %+v", tc.num, res.Pulls)
			}
			if got.Merged != tc.merged {
				t.Fatalf("PR #%d merged = %v, want %v (%+v)", tc.num, got.Merged, tc.merged, got)
			}
		})
	}
}

// TestPROutMergedAlwaysPresent pins the wire discipline: merged is emitted
// even when false (the PROut/Draft always-present rule), so old clients keep
// ignoring it and new clients never branch on absence.
func TestPROutMergedAlwaysPresent(t *testing.T) {
	raw, err := json.Marshal(PROut{Num: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"merged":false`) {
		t.Fatalf("unmerged row must still emit the flag: %s", raw)
	}
	raw, err = json.Marshal(PROut{Num: 2, Merged: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"merged":true`) {
		t.Fatalf("merged row must emit the flag: %s", raw)
	}
}
