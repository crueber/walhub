package bundle

// gaps_test.go covers the pure helpers and strategy validation arms the
// existing suites leave open.

import (
	"os"
	"path/filepath"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestFromConfigConvertsAndValidates(t *testing.T) {
	in := []config.BundleStrategy{
		{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 3, BackfillMax: 1},
		{Name: "daily", Kind: "incremental", Base: "weekly", Schedule: "@daily", Chain: true, BackfillMax: 7},
	}
	out, err := FromConfig(in, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Base != "weekly" || !out[1].Chain {
		t.Fatalf("converted = %+v", out)
	}
	// bad schedule → error naming the strategy
	if _, err := FromConfig([]config.BundleStrategy{{Name: "x", Kind: "full", Schedule: "junk"}}, 0); err == nil {
		t.Fatal("bad schedule must fail")
	}
}

func TestValidateStrategiesArms(t *testing.T) {
	full := Strategy{Name: "full", Kind: KindFull, Schedule: cronOf(t, "@weekly"), Keep: 2}
	inc := Strategy{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily")}

	if err := ValidateStrategies([]Strategy{{Kind: KindFull, Schedule: cronOf(t, "@weekly")}}); err == nil {
		t.Fatal("missing name must fail")
	}
	if err := ValidateStrategies([]Strategy{full, {Name: "full", Kind: KindFull, Schedule: cronOf(t, "@weekly")}}); err == nil {
		t.Fatal("duplicate name must fail")
	}
	if err := ValidateStrategies([]Strategy{{Name: "inc", Kind: KindIncremental, Schedule: cronOf(t, "@daily")}}); err == nil {
		t.Fatal("incremental without base must fail")
	}
	if err := ValidateStrategies([]Strategy{{Name: "inc", Kind: KindIncremental, Base: "nope", Schedule: cronOf(t, "@daily")}}); err == nil {
		t.Fatal("unknown base must fail")
	}
	if err := ValidateStrategies([]Strategy{{Name: "inc", Kind: KindIncremental, Base: "full", Schedule: cronOf(t, "@daily"), Keep: 2}, full}); err == nil {
		t.Fatal("keep on incremental must fail")
	}
	// a valid table passes
	if err := ValidateStrategies([]Strategy{full, inc}); err != nil {
		t.Fatalf("valid table: %v", err)
	}
}

func cronOf(t *testing.T, s string) Cron {
	t.Helper()
	c, err := ParseSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChainRoot(t *testing.T) {
	strats := []Strategy{
		{Name: "root", Kind: KindFull, Schedule: cronOf(t, "@weekly")},
		{Name: "mid", Kind: KindIncremental, Base: "root", Schedule: cronOf(t, "@daily")},
		{Name: "leaf", Kind: KindIncremental, Base: "mid", Schedule: cronOf(t, "@hourly")},
	}
	if got := ChainRoot(strats, &strats[2]); got.Name != "root" {
		t.Fatalf("chain root = %s", got.Name)
	}
	if got := ChainRoot(strats, &strats[0]); got.Name != "root" {
		t.Fatalf("root is its own root: %s", got.Name)
	}
	// broken base link returns the current strategy
	orphan := []Strategy{{Name: "o", Kind: KindIncremental, Base: "ghost", Schedule: cronOf(t, "@daily")}}
	if got := ChainRoot(orphan, &orphan[0]); got.Name != "o" {
		t.Fatalf("orphan root = %s", got.Name)
	}
}

func TestURLQuote(t *testing.T) {
	if got := urlQuote(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("urlQuote = %q", got)
	}
	if got := urlQuote("plain"); got != `"plain"` {
		t.Fatalf("plain = %q", got)
	}
}

func TestFileSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := FileSize(p)
	if err != nil || n != 5 {
		t.Fatalf("size = %d, %v", n, err)
	}
	if _, err := FileSize(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestEntryByIDAndSorted(t *testing.T) {
	list := &proto.BundleList{Bundles: []*proto.BundleEntry{
		{ID: "b", Strategy: "daily", Slot: 5},
		{ID: "a", Strategy: "daily", Slot: 9},
		{ID: "c", Strategy: "weekly", Slot: 1},
	}}
	if got := EntryByID(list, "a"); got == nil || got.ID != "a" {
		t.Fatalf("EntryByID = %+v", got)
	}
	if EntryByID(list, "zz") != nil {
		t.Fatal("unknown id must be nil")
	}
	sorted := entriesOfStrategySorted(list, "daily")
	if len(sorted) != 2 || sorted[0].ID != "b" || sorted[1].ID != "a" {
		t.Fatalf("sorted = %+v", sorted)
	}
	if got := entriesOfStrategySorted(list, "nope"); len(got) != 0 {
		t.Fatalf("unknown strategy = %+v", got)
	}
}
