package issues

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// --- Forgejo #564: Card-projection version gate ---------------------------------

// TestIndexCompleteProjectionGate pins the freshness half of indexComplete:
// coverage alone is not enough — an absent or older CardVersion must read
// incomplete so the LIST fallback heals the window.
func TestIndexCompleteProjectionGate(t *testing.T) {
	mk := func(version int, nums ...int) *Index {
		ix := &Index{Open: []Card{}, ClosedRecent: []Card{}, CardVersion: version}
		for _, n := range nums {
			ix.Open = append(ix.Open, Card{Num: n, Kind: "issue"})
		}
		return ix
	}
	cases := []struct {
		name string
		ix   *Index
		next int
		want bool
	}{
		{"nil index never complete", nil, 2, false},
		{"no counter never complete", mk(CardProjectionVersion, 1), 0, false},
		{"coverage without version is stale", mk(0, 1), 2, false},
		{"coverage with current version is complete", mk(CardProjectionVersion, 1), 2, true},
		{"older version is stale", mk(CardProjectionVersion-1, 1), 2, false},
		{"future version stays complete", mk(CardProjectionVersion+1, 1), 2, true},
		{"current version with a gap is incomplete", mk(CardProjectionVersion, 2), 3, false},
		{"closed_recent covers too", &Index{Open: []Card{}, ClosedRecent: []Card{{Num: 1, Kind: "issue"}}, CardVersion: CardProjectionVersion}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := indexComplete(tc.ix, tc.next); got != tc.want {
				t.Fatalf("indexComplete = %v, want %v", got, tc.want)
			}
		})
	}
}

// staleMilestoneSetup builds one issue on one milestone, then rewrites the
// persisted index as a pre-#564 object: full coverage but no CardVersion
// and cards missing the milestone id (the observed live state).
func staleMilestoneSetup(t *testing.T, s *Service) (msID string, num int) {
	t.Helper()
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1.1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	th := mustCreate(t, s, "acme", "repo", janeP, "milestoned bug", "")
	id := m.ID
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: ptrTo(&id)}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the index the pre-#564 way: coverage-complete, no version,
	// card without the milestone field.
	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	for i := range ix.Open {
		ix.Open[i].Milestone = nil
	}
	for i := range ix.ClosedRecent {
		ix.ClosedRecent[i].Milestone = nil
	}
	ix.CardVersion = 0
	raw, _ := json.Marshal(ix)
	mustPut(t, s, IndexKey("acme", "repo"), raw)
	return m.ID, th.Num
}

// TestListFallbackHealsStaleMilestoneCard reproduces #564 end to end at the
// service layer: the stale card matches nothing, but ListIssues serves the
// header truth through the fallback.
func TestListFallbackHealsStaleMilestoneCard(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	msID, _ := staleMilestoneSetup(t, s)

	// The stale card alone matches nothing (the card-list symptom).
	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got := filterCards(append(append([]Card{}, ix.Open...), ix.ClosedRecent...), ListFilter{Milestone: msID}); len(got) != 0 {
		t.Fatalf("stale cards match %d, want 0 (the bug symptom)", len(got))
	}
	// The gate refuses the fast path …
	if indexComplete(ix, s.loadCounter(reqCtx(), "acme", "repo")) {
		t.Fatal("stale index reads complete (fast path would suppress the list)")
	}
	// … so the LIST fallback heals the window (header wins).
	res, err := s.ListIssues(reqCtx(), "acme", "repo", aliceP, ListFilter{Milestone: msID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 1 || res.Issues[0].Milestone == nil || *res.Issues[0].Milestone != msID {
		t.Fatalf("healed list = %+v, want the milestoned issue", res.Issues)
	}
}

// TestRepairIndexHealsAndStamps pins the one-shot backfill: disagreeing
// cards are rebuilt from headers, the version is stamped, the milestone
// filter matches, and a second run is a no-op.
func TestRepairIndexHealsAndStamps(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	msID, _ := staleMilestoneSetup(t, s)

	fixed, err := s.RepairIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if fixed != 1 {
		t.Fatalf("repaired = %d, want 1", fixed)
	}
	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if ix.CardVersion != CardProjectionVersion {
		t.Fatalf("CardVersion = %d, want %d", ix.CardVersion, CardProjectionVersion)
	}
	if got := filterCards(append(append([]Card{}, ix.Open...), ix.ClosedRecent...), ListFilter{Milestone: msID}); len(got) != 1 {
		t.Fatalf("repaired cards match %d, want 1", len(got))
	}
	if !indexComplete(ix, s.loadCounter(reqCtx(), "acme", "repo")) {
		t.Fatal("repaired index still reads incomplete")
	}
	// Second run: nothing disagrees, nothing rewritten.
	again, err := s.RepairIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second repair = %d, want 0", again)
	}
}

// TestRepairIndexPreservesPRCards pins the shared-numbering-space rule:
// non-issue cards ride through the rebuild untouched.
func TestRepairIndexPreservesPRCards(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	_, _ = staleMilestoneSetup(t, s)

	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	pr := Card{Num: 99, Kind: "pr", Title: "big change", State: StateOpen, Labels: []string{}, Assignees: []string{}, Author: "bob", UpdatedAt: ix.Open[0].UpdatedAt}
	ix.Open = append(ix.Open, pr)
	raw, _ := json.Marshal(ix)
	mustPut(t, s, IndexKey("acme", "repo"), raw)

	if _, err := s.RepairIndex(reqCtx(), "acme", "repo"); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range after.Open {
		if c.Num == 99 && c.Kind == "pr" && c.Title == "big change" {
			found = true
		}
	}
	if !found {
		t.Fatalf("PR card lost in repair: %+v", after.Open)
	}
}

// TestRepairIndexKeepsCompactedEvicted pins the watermark rule: threads at
// or below compacted_through are served by LIST and must not be re-added.
func TestRepairIndexKeepsCompactedEvicted(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	_, num := staleMilestoneSetup(t, s)

	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	ix.Open = []Card{} // evict the only card …
	ix.CompactedThrough = "0000ff"
	raw, _ := json.Marshal(ix)
	mustPut(t, s, IndexKey("acme", "repo"), raw)

	if _, err := s.RepairIndex(reqCtx(), "acme", "repo"); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(append([]Card{}, after.Open...), after.ClosedRecent...) {
		if c.Num == num {
			t.Fatalf("compacted thread %d re-added by repair", num)
		}
	}
	if after.CompactedThrough != "0000ff" {
		t.Fatalf("watermark = %q, want 0000ff", after.CompactedThrough)
	}
}

// --- failure observability ------------------------------------------------------

// TestUpdateIndexFailureCounted pins the #564 no-silent-drop rule: every
// lost index write bumps IndexDrops (and logs when a logger is wired)
// while the mutation itself still succeeds.
func TestUpdateIndexFailureCounted(t *testing.T) {
	t.Run("nil logger stays quiet and still counts", func(t *testing.T) {
		inner := store.NewMemory()
		fl := &flakyStore{ObjectStore: inner, failPut: func(key string, n int) error {
			if strings.HasSuffix(key, "index.json") {
				return errStoreDown(key)
			}
			return nil
		}}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		if got := s.IndexDrops(); got < 1 {
			t.Fatalf("IndexDrops = %d, want ≥ 1", got)
		}
	})
	t.Run("wired logger emits the drop", func(t *testing.T) {
		inner := store.NewMemory()
		fl := &flakyStore{ObjectStore: inner, failPut: func(key string, n int) error {
			if strings.HasSuffix(key, "index.json") {
				return errStoreDown(key)
			}
			return nil
		}}
		s := New(fl, newFakeRoles())
		var sb strings.Builder
		s.Log = slog.New(slog.NewTextHandler(&sb, nil))
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		if got := s.IndexDrops(); got < 1 {
			t.Fatalf("IndexDrops = %d, want ≥ 1", got)
		}
		if !strings.Contains(sb.String(), "dropped index update") {
			t.Fatalf("log has no drop line: %q", sb.String())
		}
	})
	t.Run("CAS contention give-up counts", func(t *testing.T) {
		inner := store.NewMemory()
		fl := &flakyStore{ObjectStore: inner, failPut: func(key string, n int) error {
			if strings.HasSuffix(key, "index.json") {
				return precond(key)
			}
			return nil
		}}
		s := New(fl, newFakeRoles())
		mustCreate(t, s, "acme", "repo", janeP, "t", "")
		if got := s.IndexDrops(); got < 1 {
			t.Fatalf("IndexDrops = %d, want ≥ 1", got)
		}
	})
}

// TestBumpMilestoneFailureCounted pins the same rule for the denormalized
// counters: a lost bump is counted (and logged) while the state change
// itself commits.
func TestBumpMilestoneFailureCounted(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	inner := store.NewMemory()
	fl := &flakyStore{ObjectStore: inner}
	s := New(fl, roles)
	var sb strings.Builder
	s.Log = slog.New(slog.NewTextHandler(&sb, nil))
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")
	// Fail milestone-object writes only from here on (creation above must
	// succeed): the patch below commits its header P3 two-step while the
	// counter bump is lost.
	fl.failPut = func(key string, n int) error {
		if strings.Contains(key, "/milestones/") && !strings.HasSuffix(key, "/index.json") {
			return errStoreDown(key)
		}
		return nil
	}
	id := m.ID
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: ptrTo(&id)}); err != nil {
		t.Fatal(err)
	}
	if got := s.MilestoneDrops(); got < 1 {
		t.Fatalf("MilestoneDrops = %d, want ≥ 1", got)
	}
	if !strings.Contains(sb.String(), "dropped milestone counter bump") {
		t.Fatalf("log has no bump-drop line: %q", sb.String())
	}
	// The header truth still carries the milestone.
	nt, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
	if nt.Milestone == nil || *nt.Milestone != id {
		t.Fatalf("header lost the milestone: %+v", nt)
	}
}

// TestFreshIndexStampedCurrent pins the stamp points: a brand-new index
// (first mutation) carries the current projection, so warm repos keep the
// O(1) fast path.
func TestFreshIndexStampedCurrent(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	mustCreate(t, s, "acme", "repo", janeP, "t", "")
	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if ix.CardVersion != CardProjectionVersion {
		t.Fatalf("new CardVersion = %d, want %d", ix.CardVersion, CardProjectionVersion)
	}
	if !indexComplete(ix, s.loadCounter(reqCtx(), "acme", "repo")) {
		t.Fatal("fresh single-issue index reads incomplete")
	}
}

// TestCardsEqualMatrix pins the diff predicate RepairIndex is built on.
func TestCardsEqualMatrix(t *testing.T) {
	base := Card{Num: 1, Kind: "issue", Title: "t", State: StateOpen, Labels: []string{}, Assignees: []string{}}
	same := base
	if !cardsEqual(base, same) {
		t.Fatal("identical cards compare unequal")
	}
	m1, m2 := "000001", "000002"
	withMS := base
	withMS.Milestone = &m1
	otherMS := base
	otherMS.Milestone = &m2
	noMS := base
	if cardsEqual(withMS, noMS) || cardsEqual(withMS, otherMS) {
		t.Fatal("milestone drift compares equal")
	}
	renamed := base
	renamed.Title = "other"
	if cardsEqual(base, renamed) {
		t.Fatal("title drift compares equal")
	}
}
