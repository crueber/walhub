package api

import (
	"reflect"
	"testing"
	"time"
)

func TestSplitTrailers(t *testing.T) {
	cases := []struct {
		name     string
		message  string
		body     string
		trailers []Trailer
	}{
		{
			name:     "no trailers subject only",
			message:  "Fix the thing",
			body:     "Fix the thing",
			trailers: []Trailer{},
		},
		{
			name:     "trailer block after blank line",
			message:  "Fix the thing\n\nSigned-off-by: Jane <jane@example.com>",
			body:     "Fix the thing",
			trailers: []Trailer{{Key: "Signed-off-by", Value: "Jane <jane@example.com>"}},
		},
		{
			name:     "multiple trailers file order",
			message:  "Sub\n\nReviewed-by: Bob\nSigned-off-by: Jane\n",
			body:     "Sub",
			trailers: []Trailer{{Key: "Reviewed-by", Value: "Bob"}, {Key: "Signed-off-by", Value: "Jane"}},
		},
		{
			name:    "non-final paragraph key is not a trailer",
			message: "Sub\n\nKey: value\n\nActual body text.",
			body:    "Sub\n\nKey: value\n\nActual body text.",
		},
		{
			name:    "body line fails the block",
			message: "Sub\n\nSigned-off-by: Jane\nnot a trailer",
			body:    "Sub\n\nSigned-off-by: Jane\nnot a trailer",
		},
		{
			name:     "empty value legal",
			message:  "Sub\n\nAcked-by:",
			body:     "Sub",
			trailers: []Trailer{{Key: "Acked-by", Value: ""}},
		},
		{
			name:     "block without blank line: whole message after subject qualifies",
			message:  "Subject line\nAcked-by: X",
			body:     "Subject line",
			trailers: []Trailer{{Key: "Acked-by", Value: "X"}},
		},
		{
			name:     "folded continuation preserved",
			message:  "Sub\n\nBug: 1234\nComment: one\n two\n\tthree",
			body:     "Sub",
			trailers: []Trailer{{Key: "Bug", Value: "1234"}, {Key: "Comment", Value: "one\ntwo\nthree"}},
		},
		{
			name:     "continuation after empty value folds with newline",
			message:  "Sub\n\nSee:\n docs/readme.md",
			body:     "Sub",
			trailers: []Trailer{{Key: "See", Value: "\ndocs/readme.md"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, trailers := SplitTrailers(tc.message)
			if body != tc.body {
				t.Fatalf("body = %q, want %q", body, tc.body)
			}
			want := tc.trailers
			if want == nil {
				want = []Trailer{}
			}
			if !reflect.DeepEqual(trailers, want) {
				t.Fatalf("trailers = %+v, want %+v", trailers, want)
			}
		})
	}
}

func TestParseLsTree(t *testing.T) {
	in := "100644 blob e69de29bb2d1d6434b8b29ae775ad8c2e48c5391       0\tfile.txt\x00" +
		"040000 tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904       -\tsub\x00" +
		"160000 commit 0123456789abcdef0123456789abcdef01234567       -\tmod\x00"
	got := parseLsTree([]byte(in))
	want := []TreeEntry{
		{Name: "file.txt", Type: "blob", Mode: "100644", Size: 0, SHA: "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{Name: "sub", Type: "tree", Mode: "040000", Size: -1, SHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
		{Name: "mod", Type: "commit", Mode: "160000", Size: -1, SHA: "0123456789abcdef0123456789abcdef01234567"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSortTreeEntries(t *testing.T) {
	entries := []TreeEntry{
		{Name: "zeta.txt", Type: "blob"},
		{Name: "sub", Type: "tree"},
		{Name: "alpha.txt", Type: "blob"},
		{Name: "adir", Type: "tree"},
	}
	sortTreeEntries(entries)
	want := []string{"adir", "sub", "alpha.txt", "zeta.txt"}
	for i, e := range entries {
		if e.Name != want[i] {
			t.Fatalf("entry %d = %s, want %s", i, e.Name, want[i])
		}
	}
}

func TestParseNumstatPatch(t *testing.T) {
	// Byte shape verified against real `git show --numstat -z`:
	// counts are TAB-separated and NUL-terminated; each path field is
	// NUL-terminated (renames carry src then dst).
	in := "3\t1\t\x00a.txt\x00" +
		"-\t-\t\x00bin.dat\x00" +
		"0\t2\t\x00src/old.go\x00src/new.go\x00" +
		"\n" +
		"diff --git a/a.txt b/a.txt\n" +
		"index 1..2 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n"
	stats, patch := parseNumstatPatch([]byte(in))
	want := []Stat{
		{Path: "a.txt", Additions: 3, Deletions: 1},
		{Path: "bin.dat", Additions: -1, Deletions: -1},
		{Path: "src/new.go", Additions: 0, Deletions: 2},
	}
	if !reflect.DeepEqual(stats, want) {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if want := "diff --git a/a.txt b/a.txt\nindex 1..2 100644\n--- a/a.txt\n+++ b/a.txt\n"; patch != want {
		t.Fatalf("patch = %q, want %q", patch, want)
	}
}

func TestParseLogRecords(t *testing.T) {
	in := "0123456789abcdef0123456789abcdef01234567\x00" +
		"1111111111111111111111111111111111111111 2222222222222222222222222222222222222222\x00" +
		"Jane\x00jane@example.com\x002026-01-02T03:04:05Z\x00" +
		"Jim\x00jim@example.com\x002026-01-02T03:04:06Z\x00" +
		"Subject line\x00Body text.\n\x1e"
	commits := parseLogRecords([]byte(in))
	if len(commits) != 1 {
		t.Fatalf("got %d commits", len(commits))
	}
	c := commits[0]
	if c.SHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("sha = %s", c.SHA)
	}
	if len(c.Parents) != 2 || c.Parents[0] != "1111111111111111111111111111111111111111" {
		t.Fatalf("parents = %v", c.Parents)
	}
	if c.Author != "Jane" || c.AuthorEmail != "jane@example.com" {
		t.Fatalf("author = %s/%s", c.Author, c.AuthorEmail)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !c.AuthorDate.Equal(want) {
		t.Fatalf("author_date = %v", c.AuthorDate)
	}
	if c.Subject != "Subject line" || c.Body != "Body text." {
		t.Fatalf("subject/body = %q/%q", c.Subject, c.Body)
	}
	if c.Trailers == nil {
		t.Fatal("trailers must be [] not nil")
	}
}

func TestParseShowRecord(t *testing.T) {
	in := "0123456789abcdef0123456789abcdef01234567\x00\x00Jane\x00jane@x\x002026-01-02T03:04:05Z\x00" +
		"Jane\x00jane@x\x002026-01-02T03:04:05Z\x00Sub\x00\x00Sub\n\nAcked-by: X\n"
	c, ok := parseShowRecord([]byte(in))
	if !ok {
		t.Fatal("parse failed")
	}
	if len(c.Parents) != 0 {
		t.Fatalf("parents = %v", c.Parents)
	}
	if c.Body != "Sub" || len(c.Trailers) != 1 || c.Trailers[0].Key != "Acked-by" {
		t.Fatalf("body/trailers = %q/%+v", c.Body, c.Trailers)
	}
}

func TestCronNext(t *testing.T) {
	after := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC) // a Tuesday
	cases := []struct {
		spec string
		want string
	}{
		{"0 0 23 * * *", "2026-09-01T23:00:00Z"},   // daily at 23:00
		{"0 0 23 * * Sun", "2026-09-06T23:00:00Z"}, // next Sunday
		{"0 0 * * * *", "2026-09-01T11:00:00Z"},    // hourly
		{"0 30 8 15 * *", "2026-09-15T08:30:00Z"},  // monthly on the 15th
		{"30 0 9 1 9 *", "2027-09-01T09:00:30Z"},   // next Sept 1st
		{"0 0 6-8 * * *", "2026-09-02T06:00:00Z"},  // range (6,7,8 already past 10:00)
		{"0 */15 * * * *", "2026-09-01T10:15:00Z"}, // step
	}
	for _, tc := range cases {
		got, ok := cronNext(tc.spec, after)
		if !ok {
			t.Fatalf("cronNext(%q) failed", tc.spec)
		}
		if got.UTC().Format(time.RFC3339) != tc.want {
			t.Fatalf("cronNext(%q) = %s, want %s", tc.spec, got.UTC().Format(time.RFC3339), tc.want)
		}
	}
	if _, ok := cronNext("0 0 25 * *", after); ok { // 5 fields → invalid
		t.Fatal("5-field spec must not parse")
	}
	if _, ok := cronNext("0 0 99 * * *", after); ok {
		t.Fatal("out-of-range hour must not parse")
	}
}

func TestCronHuman(t *testing.T) {
	if got := cronHuman("0 0 23 * * *"); got != "daily at 23:00 UTC" {
		t.Fatalf("got %q", got)
	}
	if got := cronHuman("0 0 23 * * Sun"); got != "weekly on Sunday at 23:00 UTC" {
		t.Fatalf("got %q", got)
	}
	if got := cronHuman("0 0 23 1 * *"); got != "monthly on day 1 at 23:00 UTC" {
		t.Fatalf("got %q", got)
	}
}
