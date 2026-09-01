package bundle

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func goldEntry(id, strategy, kind string, token uint64, filter string) *proto.BundleEntry {
	return &proto.BundleEntry{
		ID:            id,
		Key:           "bundles/" + strategy + "/file.bundle",
		Strategy:      strategy,
		Kind:          kind,
		CreationToken: token,
		Slot:          token,
		Seq:           token,
		Size:          1_234_567_890,
		Filter:        filter,
	}
}

// TestRenderListGolden is §8.11: git-config text, mode/heuristic header,
// entries ascending creationToken, id = "<strategy>/<slotRFC3339Z>", proxy
// URIs, filter line only on the filtered family.
func TestRenderListGolden(t *testing.T) {
	weekly := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	daily1 := goldEntry("daily/2026-08-31T00:00:00Z", "daily", KindIncremental, 1788134400, "")
	daily1.BaseID = weekly.ID
	daily2 := goldEntry("daily/2026-09-01T00:00:00Z", "daily", KindIncremental, 1788220800, "")
	daily2.BaseID = daily1.ID
	// An orphaned incremental whose base entry is gone → dropped (§8.11).
	orphan := goldEntry("hourly/2026-09-01T05:00:00Z", "hourly", KindIncremental, 1788238800, "")
	orphan.BaseID = "daily/2026-09-01T04:00:00Z"

	list := protoBundleList(orphan, daily2, weekly, daily1)
	srv := &Server{PublicBase: "https://git.example.com", ServeVia: ServeProxy}

	got, err := srv.Render(context.Background(), "acme/monorepo", list, true, "")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `[bundle]
    version = 1
    mode = all
    heuristic = creationToken
[bundle "weekly/2026-08-30T00:00:00Z"]
    uri = https://git.example.com/acme/monorepo.git/bundles/weekly/file.bundle
    creationToken = 1788048000
[bundle "daily/2026-08-31T00:00:00Z"]
    uri = https://git.example.com/acme/monorepo.git/bundles/daily/file.bundle
    creationToken = 1788134400
[bundle "daily/2026-09-01T00:00:00Z"]
    uri = https://git.example.com/acme/monorepo.git/bundles/daily/file.bundle
    creationToken = 1788220800
`
	if got != want {
		t.Fatalf("clone list =\n%s\nwant\n%s", got, want)
	}

	// Catchup = the same without fulls (§8.11: recipes record the catchup URL
	// in fetch.bundleURI so a fetching client never re-pulls the new full).
	gotCatchup, err := srv.Render(context.Background(), "acme/monorepo", list, false, "")
	if err != nil {
		t.Fatalf("Render catchup: %v", err)
	}
	if strings.Contains(gotCatchup, "weekly/") {
		t.Fatalf("catchup list must not carry fulls:\n%s", gotCatchup)
	}
	if !strings.Contains(gotCatchup, "daily/2026-08-31T00:00:00Z") {
		t.Fatalf("catchup list lost the chain:\n%s", gotCatchup)
	}
	if gotCatchup == got {
		t.Fatal("catchup and clone lists must differ")
	}
}

// TestRenderFilteredFamily pins families-never-mix (§8.11/§8.13): the
// blobless list carries only filtered entries and always the filter line; any
// other filter value is an error (HTTP 400 at the seam).
func TestRenderFilteredFamily(t *testing.T) {
	full := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	blobless := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, FilterBlobNone)
	list := protoBundleList(full, blobless)
	srv := &Server{PublicBase: "https://git.example.com", ServeVia: ServeProxy}

	got, err := srv.Render(context.Background(), "acme/monorepo", list, true, FilterBlobNone)
	if err != nil {
		t.Fatalf("Render filtered: %v", err)
	}
	if !strings.Contains(got, "filter = blob:none") {
		t.Fatalf("filtered family must carry the filter line:\n%s", got)
	}
	if strings.Contains(got, "uri = https://git.example.com/acme/monorepo.git/bundles/weekly/file.bundle\n    creationToken = 1788048000\n[bundle") {
		// only one entry present
	}
	if n := strings.Count(got, "[bundle "); n != 1 {
		t.Fatalf("filtered family must be exactly the blobless entries, got %d:\n%s", n, got)
	}

	if _, err := srv.Render(context.Background(), "acme/monorepo", list, true, "blob:some"); err == nil {
		t.Fatal("unknown filter must error (400 at the seam)")
	}
}

// TestOrphanDropped covers §8.11: orphaned incrementals whose base entry is
// gone are dropped from rendering.
func TestOrphanDropped(t *testing.T) {
	weekly := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	orphan := goldEntry("daily/2026-09-01T00:00:00Z", "daily", KindIncremental, 1788220800, "")
	orphan.BaseID = "daily/2026-08-31T00:00:00Z" // not in the list
	list := protoBundleList(weekly, orphan)
	if got := CloneEntries(list); len(got) != 1 || got[0].ID != weekly.ID {
		t.Fatalf("CloneEntries = %+v, want only the weekly", got)
	}
	if got := CatchupEntries(list); len(got) != 0 {
		t.Fatalf("CatchupEntries = %+v, want empty (orphan dropped)", got)
	}
}

// TestServeSignedURLFallback covers §8.11 serving: signed_url for listed
// repos, proxy otherwise, and the warn-once proxy fallback on signing failure.
func TestServeSignedURLFallback(t *testing.T) {
	mem := store.NewMemory()
	entry := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")

	t.Run("proxy mode", func(t *testing.T) {
		srv := &Server{St: mem, ServeVia: ServeProxy, PublicBase: "https://git.example.com"}
		got := srv.URI(context.Background(), "acme/monorepo", entry)
		want := "https://git.example.com/acme/monorepo.git/bundles/weekly/file.bundle"
		if got != want {
			t.Fatalf("proxy uri = %q, want %q", got, want)
		}
	})

	t.Run("signed mode + fallback with warn-once", func(t *testing.T) {
		fails := store.NewMemory()
		fails.SigningFails = true
		var warnings []string
		srv := &Server{
			St: fails, ServeVia: ServeSignedURL, PublicBase: "https://git.example.com",
			SignedURLFor: map[string]bool{"acme/monorepo": true, "other/repo": true},
			WarnOnce:     func(repo, msg string) { warnings = append(warnings, repo+": "+msg) },
		}
		// Listed repo → presign attempted; failing store → proxy fallback.
		got := srv.URI(context.Background(), "acme/monorepo", entry)
		if got != "https://git.example.com/acme/monorepo.git/bundles/weekly/file.bundle" {
			t.Fatalf("fallback uri = %q", got)
		}
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want exactly 1", warnings)
		}
		// Second failure for the same repo is silent (warn once per repo).
		_ = srv.URI(context.Background(), "acme/monorepo", entry)
		if len(warnings) != 1 {
			t.Fatalf("warn-once violated: %v", warnings)
		}
		// A different repo warns separately.
		_ = srv.URI(context.Background(), "other/repo", entry)
		if len(warnings) != 2 {
			t.Fatalf("second repo should warn once: %v", warnings)
		}
	})

	t.Run("unlisted repo gets proxy without presigning", func(t *testing.T) {
		fails := store.NewMemory()
		fails.SigningFails = true
		var warnings []string
		srv := &Server{
			St: fails, ServeVia: ServeSignedURL, PublicBase: "https://git.example.com",
			SignedURLFor: map[string]bool{"acme/monorepo": true},
			WarnOnce:     func(repo, msg string) { warnings = append(warnings, msg) },
		}
		got := srv.URI(context.Background(), "quiet/repo", entry)
		if !strings.HasPrefix(got, "https://git.example.com/quiet/repo.git/") {
			t.Fatalf("proxy uri = %q", got)
		}
		if len(warnings) != 0 {
			t.Fatalf("no presign attempted → no warning: %v", warnings)
		}
	})
}

// TestRecordVerdictsAndSettle covers the verdict-batch CAS and the settle step
// (§8.7): keyed final per (strategy, slot, base_id); records dropped when the
// slot gains an entry, the base moves, or the slot leaves the window.
func TestRecordVerdictsAndSettle(t *testing.T) {
	st := newMemStore(t)
	ctx := context.Background()
	now := at("2026-09-01T12:00:00Z")

	// Two verdicts + a duplicate key replace, in ONE CAS.
	v1 := &proto.SkippedSlot{Strategy: "daily", Slot: 1788220800, BaseID: "weekly/2026-08-30T00:00:00Z", AsOfSeq: 70, Reason: "too-small: 3 commits (min 25)"}
	v1dup := &proto.SkippedSlot{Strategy: "daily", Slot: 1788220800, BaseID: "weekly/2026-08-30T00:00:00Z", AsOfSeq: 71, Reason: "too-small: 4 commits (min 25)"}
	v2 := &proto.SkippedSlot{Strategy: "hourly", Slot: 1788260400, BaseID: "daily/2026-09-01T00:00:00Z", AsOfSeq: 0, Reason: "no state as of the slot"} // hourly Sep 1 11:00: in-window, base matches
	if err := RecordVerdicts(ctx, st, []*proto.SkippedSlot{v1, v1dup, v2}); err != nil {
		t.Fatalf("RecordVerdicts: %v", err)
	}
	l, _, err := storeGetList(t, st)
	if err != nil {
		t.Fatalf("get list: %v", err)
	}
	if len(l.Skipped) != 2 {
		t.Fatalf("skipped = %d, want 2 (duplicate key replaced)", len(l.Skipped))
	}
	if l.Skipped[0].AsOfSeq != 71 {
		t.Fatalf("duplicate verdict not replaced: %+v", l.Skipped[0])
	}
	if l.Mode != "all" || l.Heuristic != "creationToken" {
		t.Fatalf("list defaults not stamped: %+v", l)
	}

	// Settle: strategy config order + window.
	strategies := DefaultStrategies()
	byName := ByName(strategies)
	// Build the entries the windows need: weekly Aug 30, daily Aug 31/Sep 1.
	weekly := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	daily := goldEntry("daily/2026-08-31T00:00:00Z", "daily", KindIncremental, 1788134400, "")
	daily.BaseID = weekly.ID
	tue := goldEntry("daily/2026-09-01T00:00:00Z", "daily", KindIncremental, 1788220800, "")
	tue.BaseID = daily.ID
	if err := UpsertEntry(ctx, st, weekly); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEntry(ctx, st, daily); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEntry(ctx, st, tue); err != nil {
		t.Fatal(err)
	}
	// v1's slot (Sep 1 00:00) now HAS an entry → dropped. v2's hourly slot
	// (Sep 1 11:00) stays inside the hourly window (2 newest fire slots) → kept.
	if _, err := SettleAndPrune(ctx, st, strategies, now); err != nil {
		t.Fatalf("SettleAndPrune: %v", err)
	}
	l, _, err = storeGetList(t, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Skipped) != 1 || l.Skipped[0].Reason != "no state as of the slot" || l.Skipped[0].Strategy != "hourly" {
		t.Fatalf("settle result = %+v, want only the no-state record", l.Skipped)
	}
	_ = byName
}

// TestUpsertEntryCAS pins §8.9.6 step 3: the CAS upsert stamps mode/heuristic,
// replaces by id, and orders nothing (rendering orders).
func TestUpsertEntryCAS(t *testing.T) {
	st := newMemStore(t)
	ctx := context.Background()
	e1 := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	if err := UpsertEntry(ctx, st, e1); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	e1b := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	e1b.Size = 42
	if err := UpsertEntry(ctx, st, e1b); err != nil {
		t.Fatalf("UpsertEntry replace: %v", err)
	}
	e2 := goldEntry("daily/2026-08-31T00:00:00Z", "daily", KindIncremental, 1788134400, "")
	if err := UpsertEntry(ctx, st, e2); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	l, _, err := storeGetList(t, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Bundles) != 2 {
		t.Fatalf("bundles = %d, want 2", len(l.Bundles))
	}
	for _, e := range l.Bundles {
		if e.ID == e1.ID && e.Size != 42 {
			t.Fatalf("replace lost the new body: %+v", e)
		}
	}
}

// TestRemoveEntries covers §8.11 bundle-rm: the CAS removes the ids and
// reports the keys present in the old list and absent from the new.
func TestRemoveEntries(t *testing.T) {
	st := newMemStore(t)
	ctx := context.Background()
	a := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000, "")
	a.Key = "bundles/weekly/a.bundle"
	b := goldEntry("daily/2026-08-31T00:00:00Z", "daily", KindIncremental, 1788134400, "")
	b.Key = "bundles/daily/b.bundle"
	if err := UpsertEntry(ctx, st, a); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEntry(ctx, st, b); err != nil {
		t.Fatal(err)
	}
	keys, err := RemoveEntries(ctx, st, []string{a.ID})
	if err != nil {
		t.Fatalf("RemoveEntries: %v", err)
	}
	if len(keys) != 1 || keys[0] != "bundles/weekly/a.bundle" {
		t.Fatalf("removed keys = %v, want [bundles/weekly/a.bundle]", keys)
	}
	l, _, _ := storeGetList(t, st)
	if len(l.Bundles) != 1 || l.Bundles[0].ID != b.ID {
		t.Fatalf("post-removal list = %+v", l.Bundles)
	}
}
