package cachepolicy

import (
	"strings"
	"testing"
)

// TestHeaderValuesExact pins the wire strings byte-for-byte (07_api.md §4):
// handlers alias these, so a typo here fails every package's contract test
// instead of shipping a divergent header.
func TestHeaderValuesExact(t *testing.T) {
	if Immutable != "private, max-age=31536000, immutable" {
		t.Fatalf("Immutable = %q", Immutable)
	}
	if SWR != "private, max-age=0, stale-while-revalidate=60" {
		t.Fatalf("SWR = %q", SWR)
	}
	if Mutable != "private, no-cache" {
		t.Fatalf("Mutable = %q", Mutable)
	}
	if NoStore != "no-store" {
		t.Fatalf("NoStore = %q", NoStore)
	}
	if NoCache != "no-cache" {
		t.Fatalf("NoCache = %q", NoCache)
	}
}

// TestCheckTable pins the mutability-rule enforcement (Forgejo #382): SWR
// admits sha ETags (or none — the §4 listings boundary) and rejects every
// version-derived shape; every other class passes unconditionally.
func TestCheckTable(t *testing.T) {
	sha40 := strings.Repeat("a", 40)
	sha64 := strings.Repeat("b", 64)
	rows := []struct {
		name string
		cc   string
		etag string
		want bool // true = Check passes
	}{
		{"swr sha40", SWR, `"` + sha40 + `"`, true},
		{"swr sha64 weak", SWR, `W/"` + sha64 + `"`, true},
		{"swr bare sha", SWR, sha40, true},
		{"swr no etag", SWR, "", true},
		{"swr version token", SWR, `"v12"`, false},
		{"swr store version", SWR, `"17"`, false},
		{"swr summary suffix", SWR, `"` + sha40 + `~vprivate"`, false},
		{"swr content hash", SWR, `"ddeadbee"`, false},
		{"swr folded stamp", SWR, `"folded.1.2"`, false},
		{"swr non-hex 40", SWR, `"` + strings.Repeat("z", 40) + `"`, false},
		{"mutable version", Mutable, `"v12"`, true},
		{"mutable suffix", Mutable, `"` + sha40 + `~vprivate"`, true},
		{"mutable none", Mutable, "", true},
		{"nostore anything", NoStore, `"v12"`, true},
		{"nocache none", NoCache, "", true},
		{"immutable sha", Immutable, `"` + sha40 + `"`, true},
		{"immutable version url", Immutable, `"avatar-v3"`, true},
		{"empty pair", "", "", true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := Check(row.cc, row.etag)
			if row.want && err != nil {
				t.Fatalf("Check(%q, %q) = %v, want nil", row.cc, row.etag, err)
			}
			if !row.want && err == nil {
				t.Fatalf("Check(%q, %q) = nil, want error", row.cc, row.etag)
			}
		})
	}
}
