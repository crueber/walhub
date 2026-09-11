// counts_test.go — Forgejo #319 OpenCounts: the repo-level open numerators
// for the tab badges, read index-first (O(1) object, no list scan), with
// the shared index version for the summary ETag.
package issues

import (
	"encoding/json"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestOpenCountsEmpty(t *testing.T) {
	s := testService(newFakeRoles())
	// No index yet (a repo that never used issues/PRs) → not-ok: the
	// summary carries zero counts with the byte-identical ETag.
	oi, op, ver, ok, err := s.OpenCounts(reqCtx(), "acme", "repo")
	if err != nil || ok || oi != 0 || op != 0 || ver != 0 {
		t.Fatalf("empty = %d/%d v%d ok=%v err=%v", oi, op, ver, ok, err)
	}
}

func TestOpenCountsIssues(t *testing.T) {
	s := testService(newFakeRoles())
	ctx := reqCtx()
	mustCreate(t, s, "acme", "repo", janeP, "first", "")
	mustCreate(t, s, "acme", "repo", janeP, "second", "")
	th3 := mustCreate(t, s, "acme", "repo", janeP, "third", "")
	if _, err := s.PatchIssue(ctx, "acme", "repo", th3.Num, janeP, IssuePatch{State: strPtr("closed")}); err != nil {
		t.Fatal(err)
	}
	oi, op, ver, ok, err := s.OpenCounts(ctx, "acme", "repo")
	if err != nil || !ok {
		t.Fatalf("OpenCounts = err %v ok %v", err, ok)
	}
	if oi != 2 || op != 0 {
		t.Fatalf("counts = %d/%d, want 2/0", oi, op)
	}
	if ver < 1 {
		t.Fatalf("version = %d, want >0", ver)
	}
	// Correct against the filtered list (the acceptance check): the
	// open window holds exactly the two open issues.
	lr, err := s.ListIssues(ctx, "acme", "repo", janeP, ListFilter{State: StateOpen, N: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.Issues) != oi {
		t.Fatalf("open list = %d, counts = %d", len(lr.Issues), oi)
	}
	// A close/reopen moves the version (the ETag basis) with no ref move.
	if _, err := s.PatchIssue(ctx, "acme", "repo", th3.Num, janeP, IssuePatch{State: strPtr("open")}); err != nil {
		t.Fatal(err)
	}
	oi2, _, ver2, ok2, err := s.OpenCounts(ctx, "acme", "repo")
	if err != nil || !ok2 || oi2 != 3 || ver2 == ver {
		t.Fatalf("after reopen = %d v%d (was v%d) ok=%v err=%v", oi2, ver2, ver, ok2, err)
	}
}

func TestOpenCountsMixedKinds(t *testing.T) {
	s := testService(newFakeRoles())
	ctx := reqCtx()
	mustCreate(t, s, "acme", "repo", janeP, "an issue", "")
	// PR cards share the same index object (kind:"pr" — the 03 family);
	// seed them through the index upsert path directly.
	s.updateIndex(ctx, "acme", "repo", Card{Num: 2, Kind: "pr", Title: "a pr", State: StateOpen, UpdatedAt: "2026-09-04T12:00:00Z"})
	s.updateIndex(ctx, "acme", "repo", Card{Num: 3, Kind: "pr", Title: "old pr", State: StateClosed, UpdatedAt: "2026-09-04T12:00:00Z"})
	oi, op, ver, ok, err := s.OpenCounts(ctx, "acme", "repo")
	if err != nil || !ok {
		t.Fatalf("OpenCounts = err %v ok %v", err, ok)
	}
	if oi != 1 || op != 1 {
		t.Fatalf("counts = %d/%d, want 1/1 (closed PR excluded)", oi, op)
	}
	if ver < 1 {
		t.Fatalf("version = %d, want >0", ver)
	}
}

func TestOpenCountsCorrupt(t *testing.T) {
	s := testService(newFakeRoles())
	ctx := reqCtx()
	_, err := store.PutBytes(ctx, s.Store, IndexKey("acme", "repo"), []byte("{broken"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := s.OpenCounts(ctx, "acme", "repo"); err == nil || ok {
		t.Fatalf("corrupt index: ok=%v err=%v, want an error", ok, err)
	}
}

func TestOpenCountsStoreError(t *testing.T) {
	inner := testService(newFakeRoles())
	fl := &flakyStore{ObjectStore: inner.Store, failGet: func(key string) error {
		return errStoreDown(key)
	}}
	s := New(fl, newFakeRoles())
	if _, _, _, ok, err := s.OpenCounts(reqCtx(), "acme", "repo"); err == nil || ok {
		t.Fatalf("store error: ok=%v err=%v, want an error", ok, err)
	}
}

func TestOpenCountsIgnoresMisfiledCards(t *testing.T) {
	s := testService(newFakeRoles())
	ctx := reqCtx()
	// A hand-built index with cards that violate the storage invariant
	// (a closed card in Open, an unknown kind): the count filters by
	// state AND kind, so neither leaks into a badge.
	raw, _ := json.Marshal(&Index{Version: 3, Open: []Card{
		{Num: 1, Kind: "issue", Title: "open", State: StateOpen},
		{Num: 2, Kind: "issue", Title: "misfiled", State: StateClosed},
		{Num: 3, Kind: "note", Title: "other", State: StateOpen},
	}, ClosedRecent: []Card{}})
	if _, err := store.PutBytes(ctx, s.Store, IndexKey("acme", "repo"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	oi, op, ver, ok, err := s.OpenCounts(ctx, "acme", "repo")
	if err != nil || !ok || oi != 1 || op != 0 || ver != 3 {
		t.Fatalf("counts = %d/%d v%d ok=%v err=%v, want 1/0 v3", oi, op, ver, ok, err)
	}
}
