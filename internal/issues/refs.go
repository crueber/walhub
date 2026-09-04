package issues

import (
	"regexp"
	"sort"
	"strings"
)

// Cross-references (§6) and mentions (§10) parse at WRITE TIME in the
// commenting handler (opened/commented bodies, raw body pre-markdown):
// the event log is the backfill source of truth (P8), so references must
// be durable events, never render-time derivations.

// refSkipper state: skip fenced code blocks (``` / ~~~) and inline code
// spans — same tokenizer stance as 12_web_ui.md markdown-lite (§5 uses
// the same skipper for closing keywords).

// stripCode removes fenced blocks and inline spans, replacing them with
// spaces (positions otherwise preserved well enough for word-boundary
// matching).
func stripCode(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	lines := strings.Split(body, "\n")
	inFence := ""
	for li, line := range lines {
		if li > 0 {
			b.WriteByte('\n')
		}
		trim := strings.TrimSpace(line)
		fence := ""
		if strings.HasPrefix(trim, "```") {
			fence = "```"
		} else if strings.HasPrefix(trim, "~~~") {
			fence = "~~~"
		}
		if fence != "" {
			if inFence == "" {
				inFence = fence
			} else if inFence == fence {
				inFence = ""
			}
			b.WriteString(strings.Repeat(" ", len(line)))
			continue
		}
		if inFence != "" {
			b.WriteString(strings.Repeat(" ", len(line)))
			continue
		}
		b.WriteString(stripInlineCode(line))
	}
	return b.String()
}

// stripInlineCode blanks `...` spans on one line (single-backtick runs;
// unmatched backticks are literal).
func stripInlineCode(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	i := 0
	for i < len(line) {
		if line[i] == '`' {
			j := i + 1
			for j < len(line) && line[j] != '`' {
				j++
			}
			if j < len(line) {
				for k := i; k <= j; k++ {
					b.WriteByte(' ')
				}
				i = j + 1
				continue
			}
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

var (
	// sameRepoRef matches #digits; digit-count (1–7) and the trailing
	// boundary are enforced in code (RE2 has no lookahead). The match is
	// deliberately permissive on the leading side ("abc#12",
	// "example.com#11" both link — #N is not part of a larger token on
	// its right edge, which is what the trailing check enforces); "#8x"
	// and "#12345678" do not match.
	sameRepoRef = regexp.MustCompile(`#([0-9]+)`)
	// crossRepoRef matches owner/repo#digits (same skipper; recorded as
	// cross_referenced only — cross-repo closing keywords are out of scope).
	crossRepoRef = regexp.MustCompile(`((?:[A-Za-z0-9_.-]+)/(?:[A-Za-z0-9_.-]+))#([0-9]+)`)
	// mentionRe matches @principal mentions (@-form of the §6 parser;
	// principals are emails, so require an @-address shape).
	mentionRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_@])@([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`)
	// closingRe is the §5 keyword grammar (case-insensitive):
	// (close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)\s+#N.
	// The keyword itself needs a left boundary ("encloses #3" must not
	// match); the number needs the trailing check.
	closingRe = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)\s+#([0-9]+)`)
)

// isWordChar reports ASCII word characters for boundary checks.
func isWordChar(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// refNum validates a digit run (1–7 digits) at match [start,end) inside
// clean: the trailing byte must not be a word char. Returns (num, true)
// on success.
func refNum(clean string, digits string, matchEnd int) (int, bool) {
	if len(digits) < 1 || len(digits) > 7 {
		return 0, false
	}
	if matchEnd < len(clean) && isWordChar(clean[matchEnd]) {
		return 0, false
	}
	n := atoi(digits)
	if n < 1 {
		return 0, false
	}
	return n, true
}

// Ref is one parsed #N mention: same-repo nums plus cross-repo targets.
type Ref struct {
	// Num is the target issue number (same repo).
	Num int
	// CrossRepo is "owner/repo" when the target is a different repo.
	CrossRepo string
}

// ParseRefs extracts up to MaxRefsPerBody deduped references from a raw
// body: fenced/inline code skipped, self-references recorded, over-cap
// stops parsing silently. Cross-repo owner/repo#N targets are returned
// with CrossRepo set.
func ParseRefs(body string) []Ref {
	clean := stripCode(body)
	seen := map[string]bool{}
	var out []Ref
	add := func(r Ref) {
		key := r.CrossRepo + "#" + itoa(r.Num)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}
	for _, loc := range crossRepoRef.FindAllStringSubmatchIndex(clean, -1) {
		if len(out) >= MaxRefsPerBody {
			return out
		}
		// loc: [fullStart, fullEnd, repoStart, repoEnd, digitsStart, digitsEnd].
		num, ok := refNum(clean, clean[loc[4]:loc[5]], loc[1])
		if !ok {
			continue
		}
		add(Ref{Num: num, CrossRepo: strings.ToLower(clean[loc[2]:loc[3]])})
	}
	// Blank cross-repo matches so their #N tails are not re-matched as
	// same-repo refs.
	blanked := crossRepoRef.ReplaceAllString(clean, " ")
	for _, loc := range sameRepoRef.FindAllStringSubmatchIndex(blanked, -1) {
		if len(out) >= MaxRefsPerBody {
			return out
		}
		num, ok := refNum(blanked, blanked[loc[2]:loc[3]], loc[1])
		if !ok {
			continue
		}
		add(Ref{Num: num})
	}
	if out == nil {
		return []Ref{}
	}
	return out
}

// ParseMentions extracts deduped @principal mentions (lowercased) from a
// raw body under the same code-skipping rules.
func ParseMentions(body string) []string {
	clean := stripCode(body)
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRe.FindAllStringSubmatch(clean, -1) {
		if len(out) >= MaxRefsPerBody {
			break
		}
		p := strings.ToLower(m[1])
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if out == nil {
		return []string{}
	}
	sort.Strings(out)
	return out
}

// ClosingRef is one §5 closing-keyword match: the target issue num plus
// the matched keyword (lowercased).
type ClosingRef struct {
	Num     int
	Keyword string
}

// ParseClosingRefs matches the §5 grammar against texts (PR body + merged
// commit messages), outside code spans/fences; one issue matches at most
// once per call (first keyword wins).
func ParseClosingRefs(texts ...string) []ClosingRef {
	seen := map[int]bool{}
	var out []ClosingRef
	for _, text := range texts {
		clean := stripCode(text)
		for _, loc := range closingRe.FindAllStringSubmatchIndex(clean, -1) {
			// loc: [fullStart, fullEnd, kwStart, kwEnd, digitsStart, digitsEnd].
			num, ok := refNum(clean, clean[loc[4]:loc[5]], loc[1])
			if !ok || seen[num] {
				continue
			}
			seen[num] = true
			out = append(out, ClosingRef{Num: num, Keyword: strings.ToLower(clean[loc[2]:loc[3]])})
		}
	}
	if out == nil {
		return []ClosingRef{}
	}
	return out
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
