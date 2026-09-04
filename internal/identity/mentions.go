// mentions.go — the 06 §3 mention parser (shared by the collaboration
// packages that emit "mentioned" fan-out).
//
// Runs once, at event write time, inside the mutating handler (P8) on the
// new event's text body. Token grammar: `@<principal>` (email-shaped, per
// ValidPrincipal) and `@<org>/<team>` (ValidOrg/ValidSlug halves), matched
// only at word boundaries, case-insensitive, canonical lowercase keys.
// Fenced code blocks and inline code spans are skipped. Bounded: at most
// MaxMentionsPerBody tokens per event; beyond that, ignored (the caller
// counts walhub_mentions_dropped_total{repo}).
//
// An unresolvable mention is the consumer's (internal/notify) problem:
// validation is a bucket probe there (profile GET / team doc read) and
// invalid mentions are silently ignored — never a 400. The text is stored
// verbatim regardless.
package identity

import (
	"regexp"
	"sort"
	"strings"
)

// MaxMentionsPerBody bounds mention tokens parsed from one event body
// (06 §3: at most 50; beyond that, ignored).
const MaxMentionsPerBody = 50

var (
	// mentionTok finds candidate @-tokens after code stripping: either an
	// email-shaped principal or an org/slug team spelling. The left
	// boundary keeps `a@b.com` (no leading @-mention) and `@@x` from
	// matching; classification/validation happens after the match.
	mentionTok = regexp.MustCompile(`(?:^|[^A-Za-z0-9_@-])@([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
)

// ParseMentions extracts mentioned principals and teams from body.
// users holds lowercase email principals; teams holds lowercase
// "org/slug" spellings. Both are sorted, deduped, never nil. At most
// MaxMentionsPerBody raw tokens are considered (by body order); tokens
// past the cap are dropped. Candidates failing ValidPrincipal (users) or
// ValidOrg+ValidSlug (teams) are dropped here — cheap local shape checks.
// Existence (profile/team doc probes) stays with the consumer.
func ParseMentions(body string) (users []string, teams []string) {
	users = []string{}
	teams = []string{}
	clean := stripMentionCode(body)
	seenU := map[string]bool{}
	seenT := map[string]bool{}
	n := 0
	for _, m := range mentionTok.FindAllStringSubmatch(clean, -1) {
		if n >= MaxMentionsPerBody {
			break
		}
		n++
		tok := strings.ToLower(strings.Trim(m[1], ".,;:!?\"')]}"))
		if strings.Contains(tok, "/") {
			parts := strings.SplitN(tok, "/", 2)
			if len(parts) != 2 || !ValidOrg(parts[0]) || !ValidSlug(parts[1]) {
				continue
			}
			if !seenT[tok] {
				seenT[tok] = true
				teams = append(teams, tok)
			}
			continue
		}
		if !ValidPrincipal(tok) {
			continue
		}
		if !seenU[tok] {
			seenU[tok] = true
			users = append(users, tok)
		}
	}
	sort.Strings(users)
	sort.Strings(teams)
	return users, teams
}

// stripMentionCode blanks fenced code blocks (``` / ~~~) and inline code
// spans (backticks) so mentions inside code never emit. Same stance as the
// issues §6 skipper: positions are otherwise preserved for boundary
// matching.
func stripMentionCode(body string) string {
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

// stripInlineCode blanks `…` spans on one line (unpaired backtick blanks
// to end of line — fail closed toward fewer mentions).
func stripInlineCode(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	inSpan := false
	for i := 0; i < len(line); i++ {
		if line[i] == '`' {
			inSpan = !inSpan
			b.WriteByte(' ')
			continue
		}
		if inSpan {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(line[i])
	}
	return b.String()
}
