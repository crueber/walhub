package pulls

import (
	"errors"
	"strings"
	"testing"
)

// Merge-durability tests (§5, issue #424 amendment): server-made merge
// commits (commit-tree/replay) reach the bucket atomically with their ref
// update — the merged ref never dangles bucket-missing objects (a fork of
// a merged repo must materialize full history, so a missing merge object
// breaks the fork's serve path, not just a future clone).

func TestMergePublishesObjects(t *testing.T) {
	mk := func(t *testing.T) *testEnv {
		t.Helper()
		e := newTestEnv()
		e.roles.Roles["jane@example.com"] = "write"
		e.roles.Roles["merger@example.com"] = "maintain"
		openBasic(t, e, "o", "r")
		seedMergeable(t, e, hexSHA(1), hexSHA(2))
		e.closer.Closed = []int{3}
		return e
	}
	t.Run("merge carries the pack", func(t *testing.T) {
		e := mk(t)
		rec := &TaskRecord{Progress: []string{}}
		out, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		_ = out
		var found *RefCall
		for _, c := range e.refs.Calls {
			if c.Op == "update-pack" && c.Ref == "refs/heads/main" {
				c := c
				found = &c
			}
		}
		if found == nil {
			t.Fatalf("no pack-carrying update: %+v", e.refs.Calls)
		}
		if found.Pack == "" || found.Old != hexSHA(1) {
			t.Fatalf("call: %+v", found)
		}
		// The pack tip was requested for the merge commit.
		seen := false
		for _, op := range e.git.CallLog() {
			if op == "packtip" {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("PackTip never called: %v", e.git.CallLog())
		}
	})
	t.Run("pack failure fails loud", func(t *testing.T) {
		e := mk(t)
		e.git.PackTipErr = errors.New("pack-objects down")
		rec := &TaskRecord{Progress: []string{}}
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); err == nil {
			t.Fatal("pack outage must fail the merge, never a dangling ref")
		}
		for _, c := range e.refs.Calls {
			if c.Op == "update" || c.Op == "update-pack" {
				t.Fatalf("ref moved without its objects: %+v", c)
			}
		}
	})
	t.Run("empty pack falls back to ref-only", func(t *testing.T) {
		e := mk(t)
		e.git.PackTipEmpty = true
		rec := &TaskRecord{Progress: []string{}}
		if _, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec); err != nil {
			t.Fatalf("merge: %v", err)
		}
		plain := 0
		for _, c := range e.refs.Calls {
			if c.Op == "update" && c.Ref == "refs/heads/main" {
				plain++
			}
			if c.Op == "update-pack" {
				t.Fatalf("empty pack must not publish pack-carrying: %+v", c)
			}
		}
		if plain != 1 {
			t.Fatalf("ref-only updates: %+v", e.refs.Calls)
		}
	})
	t.Run("update-branch carries the pack", func(t *testing.T) {
		e := mk(t)
		e.git.MergeBaseSHA = hexSHA(7)
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{Progress: []string{}}); err != nil {
			t.Fatalf("update-branch: %v", err)
		}
		found := false
		for _, c := range e.refs.Calls {
			if c.Op == "update-pack" && c.Ref == "refs/heads/topic" && c.Pack != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("no pack-carrying head update: %+v", e.refs.Calls)
		}
	})
	t.Run("update-branch pack failure fails loud", func(t *testing.T) {
		e := mk(t)
		e.git.MergeBaseSHA = hexSHA(7)
		e.git.PackTipErr = errors.New("pack-objects down")
		if _, err := e.svc.runUpdateBranch(ctx(), "o", "r", 1, writer(), &TaskRecord{Progress: []string{}}); err == nil ||
			!strings.Contains(err.Error(), "pack-objects down") {
			t.Fatalf("pack outage: %v", err)
		}
	})
}
