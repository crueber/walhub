package pulls

import (
	"errors"
	"sync"
	"testing"
)

// Regression tests for issue #64: pr.json CAS retries (and stale-struct
// saves) must never clobber a concurrently-landed merge outcome. The merge
// outcome fields (merged/merged_at/merged_by/merge_commit_sha/
// merge_strategy) are write-once/monotonic: once merged:true, no writer may
// unset them.

// landMerge records a merge outcome through the public load/save path (the
// shape runMerge's commit step writes).
func landMerge(t *testing.T, e *testEnv, sha string) {
	t.Helper()
	m, mVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || m == nil {
		t.Fatalf("loadPR: %v %v", m, err)
	}
	at, by, strategy := "2026-09-04T12:30:00Z", "merger@example.com", StrategyMerge
	m.Merged = true
	m.MergedAt = &at
	m.MergedBy = &by
	m.MergeCommitSHA = &sha
	m.MergeStrategy = &strategy
	if err := e.svc.savePR(ctx(), "o", "r", m, mVer); err != nil {
		t.Fatalf("landMerge savePR: %v", err)
	}
}

func checkMerged(t *testing.T, e *testEnv, sha string) *PRDoc {
	t.Helper()
	got, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || got == nil {
		t.Fatalf("loadPR: %v %v", got, err)
	}
	if !got.Merged || got.MergeCommitSHA == nil || *got.MergeCommitSHA != sha ||
		got.MergedAt == nil || got.MergedBy == nil || got.MergeStrategy == nil {
		t.Fatalf("merge outcome clobbered: %+v", got)
	}
	return got
}

// A head-update writer holding a pre-merge snapshot must preserve the landed
// outcome across its CAS retry while still applying its head delta.
func TestSavePRRetryPreservesLandedMerge(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	stale, staleVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	landMerge(t, e, hexSHA(9))
	stale.Head.SHA = hexSHA(3)
	if err := e.svc.savePR(ctx(), "o", "r", stale, staleVer); err != nil {
		t.Fatalf("head-writer savePR: %v", err)
	}
	got := checkMerged(t, e, hexSHA(9))
	if got.Head.SHA != hexSHA(3) {
		t.Fatalf("head delta lost: %s", got.Head.SHA)
	}
	if got.Body != "fixes #3" {
		t.Fatalf("body changed: %q", got.Body)
	}
}

// A merge-completion writer holding a pre-edit snapshot must preserve
// concurrently-landed editorial/head fields across its CAS retry.
func TestSavePRMergeRetryPreservesFreshFields(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	staleOutcome, staleVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	at, by, strategy, sha := "2026-09-04T12:30:00Z", "merger@example.com", StrategyMerge, hexSHA(9)
	staleOutcome.Merged = true
	staleOutcome.MergedAt = &at
	staleOutcome.MergedBy = &by
	staleOutcome.MergeCommitSHA = &sha
	staleOutcome.MergeStrategy = &strategy
	// A body edit lands first.
	fresh, freshVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	fresh.Body = "edited description"
	if err := e.svc.savePR(ctx(), "o", "r", fresh, freshVer); err != nil {
		t.Fatalf("body savePR: %v", err)
	}
	// The merge completion retries onto the edited doc.
	if err := e.svc.savePR(ctx(), "o", "r", staleOutcome, staleVer); err != nil {
		t.Fatalf("merge savePR: %v", err)
	}
	got := checkMerged(t, e, hexSHA(9))
	if got.Body != "edited description" {
		t.Fatalf("body edit clobbered: %q", got.Body)
	}
}

// refreshHead saves with a FRESH version but a potentially STALE struct, so
// no 412 fires — the landed outcome must survive the direct write.
func TestRefreshHeadPreservesLandedMerge(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	stale, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	landMerge(t, e, hexSHA(9))
	th, _, err := e.svc.loadThread(ctx(), "o", "r", 1)
	if err != nil || th == nil {
		t.Fatalf("loadThread: %v %v", th, err)
	}
	e.svc.refreshHead(ctx(), "o", "r", stale, th, hexSHA(5), writer())
	got := checkMerged(t, e, hexSHA(9))
	if got.Head.SHA != hexSHA(5) {
		t.Fatalf("head delta lost: %s", got.Head.SHA)
	}
}

// Unmerged retries keep last-writer-wins for non-outcome fields.
func TestSavePRRetryLastWriterWinsWhenUnmerged(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	a, aVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	b, bVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	b.Body = "from b"
	if err := e.svc.savePR(ctx(), "o", "r", b, bVer); err != nil {
		t.Fatalf("b savePR: %v", err)
	}
	a.Body = "from a"
	if err := e.svc.savePR(ctx(), "o", "r", a, aVer); err != nil {
		t.Fatalf("a savePR: %v", err)
	}
	got, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || got.Body != "from a" || got.Merged {
		t.Fatalf("got = %+v %v", got, err)
	}
}

// Once merged, concurrent body/head churn must never unset the outcome.
func TestMergeOutcomeMonotonicUnderChurn(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	landMerge(t, e, hexSHA(9))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Bounded client-style retry on 412: contention may exhaust
			// one savePR CAS window; the assertion is monotonicity of
			// the outcome, not first-try success.
			for attempt := 0; attempt < 20; attempt++ {
				body := "churn"
				_, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Body: &body})
				if err == nil {
					break
				}
				if !errors.Is(err, ErrConflict) {
					t.Errorf("UpdatePR: %v", err)
					break
				}
			}
			if th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1); th != nil {
				if live, _, lerr := e.svc.loadPR(ctx(), "o", "r", 1); lerr == nil && live != nil {
					e.svc.refreshHead(ctx(), "o", "r", live, th, hexSHA(20+i), writer())
				}
			}
		}(i)
	}
	wg.Wait()
	checkMerged(t, e, hexSHA(9))
}
