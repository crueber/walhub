package issues

import (
	"strings"
	"testing"
)

func TestStripCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// substrings that must NOT survive stripping (they were code)
		gone []string
		// substrings that must survive (prose)
		kept []string
	}{
		{"plain", "fixes #7 and #8", nil, []string{"#7", "#8"}},
		{"fence", "prose #1\n```\n#2\n```\nmore #3", []string{"#2"}, []string{"#1", "#3"}},
		{"tilde fence", "~~~ \n#9\n~~~\n#10", []string{"#9"}, []string{"#10"}},
		{"unclosed fence", "#11\n```\n#12", []string{"#12"}, []string{"#11"}},
		{"inline", "see `#13` and #14", []string{"#13"}, []string{"#14"}},
		{"unmatched tick", "a ` #15", nil, []string{"#15"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripCode(c.in)
			for _, g := range c.gone {
				if strings.Contains(got, g) {
					t.Errorf("stripCode(%q) kept code %q → %q", c.in, g, got)
				}
			}
			for _, k := range c.kept {
				if !strings.Contains(got, k) {
					t.Errorf("stripCode(%q) dropped prose %q → %q", c.in, k, got)
				}
			}
		})
	}
}

func TestParseRefs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Ref
	}{
		{"none", "no refs here", []Ref{}},
		{"single", "fixes #7", []Ref{{Num: 7}}},
		{"multi", "#1 #2, and #1 again", []Ref{{Num: 1}, {Num: 2}}},
		{"self", "see #3", []Ref{{Num: 3}}},
		{"code skipped", "`#4`\n```\n#5\n```\n#6", []Ref{{Num: 6}}},
		{"bounds", "#0 is not a ref, #8x neither", []Ref{}},
		{"long digits capped", "#12345678", []Ref{}},
		{"seven digits", "#1234567", []Ref{{Num: 1234567}}},
		{"cross repo", "see acme/other#9 and #10", []Ref{{Num: 9, CrossRepo: "acme/other"}, {Num: 10}}},
		{"cross too long", "acme/other#12345678", []Ref{}},
		{"cross trailing word", "acme/other#9x", []Ref{}},
		{"email not ref", "mail bob@example.com#11", []Ref{{Num: 11}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseRefs(c.body)
			if len(got) != len(c.want) {
				t.Fatalf("ParseRefs(%q) = %v, want %v", c.body, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("ParseRefs(%q)[%d] = %v, want %v", c.body, i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseRefsCap(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxRefsPerBody+10; i++ {
		b.WriteString(" #" + itoa(i))
	}
	if got := ParseRefs(b.String()); len(got) != MaxRefsPerBody {
		t.Fatalf("over-cap parse = %d refs, want %d (silent stop)", len(got), MaxRefsPerBody)
	}
	// The cap also stops the cross-repo pass mid-loop.
	var c strings.Builder
	for i := 1; i <= MaxRefsPerBody+10; i++ {
		c.WriteString(" acme/other#" + itoa(i))
	}
	if got := ParseRefs(c.String()); len(got) != MaxRefsPerBody {
		t.Fatalf("over-cap cross parse = %d refs, want %d", len(got), MaxRefsPerBody)
	}
	// ...and the mentions pass.
	var m strings.Builder
	for i := 1; i <= MaxRefsPerBody+10; i++ {
		m.WriteString(" @u" + itoa(i) + "@example.com")
	}
	if got := ParseMentions(m.String()); len(got) != MaxRefsPerBody {
		t.Fatalf("over-cap mentions = %d, want %d", len(got), MaxRefsPerBody)
	}
}

func TestParseMentions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"none", "hello world", []string{}},
		{"single", "cc @alice@example.com please", []string{"alice@example.com"}},
		{"dedup case", "@Bob@Example.com and @bob@example.com", []string{"bob@example.com"}},
		{"code skipped", "`@code@example.com` @real@example.com", []string{"real@example.com"}},
		{"bare word", "@alice is not an email mention", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseMentions(c.body)
			if len(got) != len(c.want) {
				t.Fatalf("ParseMentions(%q) = %v, want %v", c.body, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("ParseMentions(%q)[%d] = %q, want %q", c.body, i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseClosingRefs(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		want  []ClosingRef
	}{
		{"fix", []string{"Fixes #7"}, []ClosingRef{{Num: 7, Keyword: "fixes"}}},
		{"case", []string{"CLOSED #8"}, []ClosingRef{{Num: 8, Keyword: "closed"}}},
		{"all keywords", []string{"close #1 closes #2 closed #3 fix #4 fixes #5 fixed #6 resolve #7 resolves #8 resolved #9"},
			[]ClosingRef{{1, "close"}, {2, "closes"}, {3, "closed"}, {4, "fix"}, {5, "fixes"}, {6, "fixed"}, {7, "resolve"}, {8, "resolves"}, {9, "resolved"}}},
		{"dedup across texts", []string{"fixes #10", "body fixes #10 and closes #11"},
			[]ClosingRef{{10, "fixes"}, {11, "closes"}}},
		{"code skipped", []string{"`fixes #12`"}, []ClosingRef{}},
		{"no keyword", []string{"see #13"}, []ClosingRef{}},
		{"encloses is not closes", []string{"this encloses #15"}, []ClosingRef{}},
		{"multi text", []string{"nothing", "Resolves #14"}, []ClosingRef{{14, "resolves"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseClosingRefs(c.texts...)
			if len(got) != len(c.want) {
				t.Fatalf("ParseClosingRefs = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("ParseClosingRefs[%d] = %v, want %v", i, got[i], c.want[i])
				}
			}
		})
	}
}
