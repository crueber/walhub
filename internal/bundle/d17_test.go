package bundle

import (
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestD17RefusalText pins the exact §8.13 refusal text (pkt ERR).
func TestD17RefusalText(t *testing.T) {
	want := "unbounded fetch refused: this repo requires bundle-uri; use bundle-uri per the setup recipe, or pass -c transfer.bundleURI=false for shallow/filtered fetches"
	if D17Refusal != want {
		t.Fatalf("D17Refusal =\n%q\nwant\n%q", D17Refusal, want)
	}
	if RefusalLine() != "ERR "+want {
		t.Fatalf("RefusalLine = %q", RefusalLine())
	}
}

// TestIsUnboundedZeroHave pins §8.13 classification: unbounded = no haves, no
// deepen*, no filter. Bounded zero-have fetches (CI --depth/--filter) and all
// fetches with haves proceed.
func TestIsUnboundedZeroHave(t *testing.T) {
	if !IsUnboundedZeroHave(nil, "", "") {
		t.Fatal("zero haves + no deepen/filter must be unbounded")
	}
	if IsUnboundedZeroHave([]string{"abc"}, "", "") {
		t.Fatal("haves make it bounded")
	}
	if IsUnboundedZeroHave(nil, "depth=1", "") {
		t.Fatal("deepen makes it bounded")
	}
	if IsUnboundedZeroHave(nil, "", "blob:none") {
		t.Fatal("filter makes it bounded")
	}
}

// TestD17Tracker is the §8.13 guard math:
//
//	allow := now.Sub(e.listFetch) <= 1h && (e.fallback.IsZero() || now.Sub(e.fallback) >= 6h)
//	on allow: e.fallback = now; emit the band-2 WARNING
func TestD17Tracker(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewD17Tracker()

	// No list fetch on record → refused, no warning.
	if allow, warn := g.Decide("acme/monorepo", "alice", now); allow || warn != "" {
		t.Fatalf("no list fetch: allow=%v warn=%q", allow, warn)
	}

	// List fetch 30 min ago, no fallback yet → ONE fallback with the WARNING.
	g.RecordListFetch("acme/monorepo", "alice", now.Add(-30*time.Minute))
	allow, warn := g.Decide("acme/monorepo", "alice", now)
	if !allow || warn != D17Warning {
		t.Fatalf("first fallback: allow=%v warn=%q", allow, warn)
	}
	if warn != "warning: full fetch allowed without bundle-uri (fallback, once per 6 h); switch to the bundle-uri recipe" {
		t.Fatalf("warning text = %q", warn)
	}

	// The next one (any time within 6 h) is refused — "the next one and
	// everyone else is refused".
	if allow, _ := g.Decide("acme/monorepo", "alice", now.Add(time.Minute)); allow {
		t.Fatal("second fallback must be refused")
	}
	if allow, _ := g.Decide("acme/monorepo", "bob", now.Add(time.Minute)); allow {
		t.Fatal("another principal within 6 h must be refused")
	}

	// 6 h after the fallback: alice's fresh list fetch (within the last hour
	// of the decision) grants exactly one again.
	g.RecordListFetch("acme/monorepo", "alice", now.Add(6*time.Hour).Add(-30*time.Minute))
	if allow, _ := g.Decide("acme/monorepo", "alice", now.Add(6*time.Hour)); !allow {
		t.Fatal("fallback after 6 h with a fresh list fetch must be allowed")
	}

	// A list fetch older than 1 h does not demonstrate a bundle-uri attempt.
	g2 := NewD17Tracker()
	g2.RecordListFetch("acme/monorepo", "carol", now.Add(-2*time.Hour))
	if allow, _ := g2.Decide("acme/monorepo", "carol", now); allow {
		t.Fatal("stale list fetch (> 1 h) must not allow a fallback")
	}

	// Per-repo keys are independent.
	g3 := NewD17Tracker()
	g3.RecordListFetch("other/repo", "alice", now)
	if allow, _ := g3.Decide("acme/monorepo", "alice", now); allow {
		t.Fatal("list fetch of another repo must not grant a fallback")
	}
}

// TestD17Sweep covers lazy expiry + the 10 m sweep dropping entries older
// than 6 h (§8.13).
func TestD17Sweep(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewD17Tracker()
	g.RecordListFetch("acme/monorepo", "alice", now)
	g.Sweep(now.Add(7 * time.Hour))
	// Entry dropped → even a fresh decision window cannot allow (no entry).
	if allow, _ := g.Decide("acme/monorepo", "alice", now.Add(7*time.Hour)); allow {
		t.Fatal("swept entry must not allow a fallback")
	}
	g.RecordListFetch("acme/monorepo", "bob", now)
	g.Sweep(now.Add(30 * time.Minute))
	if allow, _ := g.Decide("acme/monorepo", "bob", now.Add(30*time.Minute)); !allow {
		t.Fatal("entry within 6 h must survive the sweep")
	}
}

// TestNarrationLines pins §8.12 band-2 narration: one line per plain-list
// entry ascending token, exact format, human size one decimal 1000-based.
func TestNarrationLines(t *testing.T) {
	a := goldEntry("daily/2026-08-31T00:00:00Z", "daily", KindIncremental, 1788134400, "")
	a.Key = "/acme/monorepo/bundles/daily/20260831T000000Z-abc.bundle"
	a.Size = 12_300_000
	a.Seq = 42
	b := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	b.Key = "/acme/monorepo/bundles/weekly/20260830T000000Z-3f2a.bundle"
	b.Size = 32_300_000_000
	b.Seq = 1

	lines := NarrationLines([]*proto.BundleEntry{a, b})
	want := []string{
		"* bundle-uri: /acme/monorepo/bundles/weekly/20260830T000000Z-3f2a.bundle (32.3 GB, full, seq 1, token 1788048000)",
		"* bundle-uri: /acme/monorepo/bundles/daily/20260831T000000Z-abc.bundle (12.3 MB, incremental, seq 42, token 1788134400)",
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d =\n%q\nwant\n%q", i, lines[i], want[i])
		}
	}
}

// TestHumanBytes covers the 1000-based scale (§8.12).
func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 KB"},
		{12_300_000, "12.3 MB"},
		{32_300_000_000, "32.3 GB"},
		{4_200_000_000_000, "4.2 TB"},
	}
	for _, tt := range tests {
		if got := HumanBytes(tt.n); got != tt.want {
			t.Fatalf("HumanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
