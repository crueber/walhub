// collab_evidence_test.go — E9 measurement harness: the 08 §4 repo
// collaboration stream fan-out shape (in-process repoBus over the memory
// backend: publish latency vs subscriber count, drop-oldest under a
// stalled subscriber, ring-replay bound for late attachers).
//
// Run: go test ./internal/notify/ -run TestEvidenceCollab -v
package notify

import (
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestEvidenceCollabFanout(t *testing.T) {
	for _, subs := range []int{1, 128} {
		svc := New(store.NewMemory(), nil)
		for i := 0; i < subs; i++ {
			_, _, unsub := svc.SubscribeRepo("acme/repo")
			defer unsub()
		}
		const n = 2000
		start := time.Now()
		for i := 0; i < n; i++ {
			svc.PublishFrame(RepoFrame{Name: "check", Repo: "acme/repo", Sha: "abc", Num: i})
		}
		el := time.Since(start)
		t.Logf("publish %d frames × %d subscribers: %s total, %dns/publish", n, subs, el, int64(el)/n)
	}
}

func TestEvidenceCollabDropOldest(t *testing.T) {
	svc := New(store.NewMemory(), nil)
	ch, _, unsub := svc.SubscribeRepo("acme/repo")
	defer unsub()
	// A stalled subscriber (never read) must not stall emission: 200
	// publishes complete, the channel holds the newest 16, the ring the
	// newest 64.
	start := time.Now()
	for i := 0; i < 200; i++ {
		svc.PublishFrame(RepoFrame{Name: "issue", Repo: "acme/repo", Num: i})
	}
	el := time.Since(start)
	got := 0
	var last RepoFrame
	for {
		select {
		case f := <-ch:
			got++
			last = f
		default:
			goto drained
		}
	}
drained:
	_, recent, unsub2 := svc.SubscribeRepo("acme/repo")
	defer unsub2()
	t.Logf("200 publishes with stalled subscriber: %s; channel held %d (newest num %d); ring holds %d",
		el, got, last.Num, len(recent))
	if got != 16 {
		t.Fatalf("channel held %d, want 16 (drop-oldest bound)", got)
	}
	if len(recent) != RepoRing {
		t.Fatalf("ring holds %d, want %d", len(recent), RepoRing)
	}
	if recent[len(recent)-1].Num != 199 {
		t.Fatalf("ring newest num = %d, want 199", recent[len(recent)-1].Num)
	}
}
