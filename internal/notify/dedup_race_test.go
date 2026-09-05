package notify

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// TestEmitConcurrentSameTripleDedups (issue #91): concurrent emissions for
// the same (user, thread, reason) carry distinct activity seqs, hence
// distinct ids — the unread-index CAS is the only arbitration point. All N
// must converge to ONE unread index row and ONE notification object. The
// pre-fix hasUnread check-then-act mints one row per emission.
func TestEmitConcurrentSameTripleDedups(t *testing.T) {
	x := newHarness(t)
	const n = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]createStatus, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, statuses[i] = x.svc.createOne(ctx(), Emission{
				Repo: "acme/repo", Num: 7, Kind: "issue",
			}, "Bug title", "bob@example.com", x.now.Format(dateTimeFmt), 100+i,
				target{principal: "carol@example.com", reason: ReasonSubscribed})
		}(i)
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent emission fan-out stalled")
	}
	created, skipped, failed := 0, 0, 0
	for _, st := range statuses {
		switch st {
		case createCreated:
			created++
		case createSkipped:
			skipped++
		default:
			failed++
		}
	}
	if failed != 0 || created != 1 || skipped != n-1 {
		t.Fatalf("statuses: created=%d skipped=%d failed=%d, want 1/%d/0", created, skipped, failed, n-1)
	}
	raw, _, err := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("carol@example.com"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		t.Fatal(err)
	}
	unread := 0
	for _, en := range ix.Entries {
		if en.State == StateUnread && en.Repo == "acme/repo" && en.Num == 7 && en.Reason == ReasonSubscribed {
			unread++
		}
	}
	if unread != 1 || ix.UnreadCount != 1 {
		t.Fatalf("index holds %d unread triple rows (count=%d), want 1/1: %+v", unread, ix.UnreadCount, ix)
	}
	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("notification objects = %d, want 1 (dedup loser orphaned its object)", got)
	}
}
