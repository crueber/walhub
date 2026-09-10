package api

import (
	"strconv"
	"strings"
	"time"
)

// --- Commit wire shape (07_api.md §3) -----------------------------------------------

// Trailer is one parsed commit-message trailer ({key, value}, file order;
// folded continuation lines keep embedded newlines).
type Trailer struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Commit is the §9.6 wire shape. body = the message minus the trailer block,
// trimmed; parents[] and trailers[] are always [] (never null).
type Commit struct {
	SHA         string    `json:"sha"`
	Parents     []string  `json:"parents"`
	Author      string    `json:"author"`
	AuthorEmail string    `json:"author_email"`
	AuthorDate  time.Time `json:"author_date"`
	Committer   string    `json:"committer"`
	CommitEmail string    `json:"commit_email"`
	CommitDate  time.Time `json:"commit_date"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	Trailers    []Trailer `json:"trailers"`
}

// Git --format strings (§9.6/§9.8, normative argv). %x00/%x1e are ASCII text
// in argv — argv can never contain a NUL byte; git expands them.
const (
	gitFmtLog  = "%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%s%x00%b%x1e"
	gitFmtShow = "%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%s%x00%b%x00%B"
)

// parseRFC3339 parses a git %aI/%cI value (already RFC 3339).
func parseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// parseLogRecords parses gitFmtLog output: records \x1e-separated, fields
// NUL-separated, parents space-separated.
func parseLogRecords(b []byte) []Commit {
	out := []Commit{}
	for _, rec := range strings.Split(string(b), "\x1e") {
		rec = strings.TrimPrefix(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		f := strings.Split(rec, "\x00")
		if len(f) < 10 {
			continue
		}
		body, trailers := SplitTrailers(f[9])
		out = append(out, Commit{
			SHA:         f[0],
			Parents:     nonNil(strings.Fields(f[1])),
			Author:      f[2],
			AuthorEmail: f[3],
			AuthorDate:  parseRFC3339(f[4]),
			Committer:   f[5],
			CommitEmail: f[6],
			CommitDate:  parseRFC3339(f[7]),
			Subject:     f[8],
			Body:        body,
			Trailers:    trailers,
		})
	}
	return out
}

// parseShowRecord parses gitFmtShow output (one commit; the trailing %B
// field drives the trailer split — §9.7).
func parseShowRecord(b []byte) (Commit, bool) {
	rec := strings.TrimSuffix(string(b), "\n")
	f := strings.Split(rec, "\x00")
	if len(f) < 11 {
		return Commit{}, false
	}
	body, trailers := SplitTrailers(f[10])
	return Commit{
		SHA:         f[0],
		Parents:     nonNil(strings.Fields(f[1])),
		Author:      f[2],
		AuthorEmail: f[3],
		AuthorDate:  parseRFC3339(f[4]),
		Committer:   f[5],
		CommitEmail: f[6],
		CommitDate:  parseRFC3339(f[7]),
		Subject:     f[8],
		Body:        body,
		Trailers:    trailers,
	}, true
}

// --- trailers (§9.7: hand-rolled `git interpret-trailers --parse` semantics) -------

// isTrailerLine matches ^[A-Za-z0-9-]+: (token = alphanumerics and `-`,
// followed by `:`).
func isTrailerLine(line string) bool {
	i := 0
	for i < len(line) {
		c := line[i]
		if c == ':' {
			return i > 0
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
		i++
	}
	return false
}

// SplitTrailers implements §9.7. Returns (body, trailers): the message minus
// the trailer block, right-trimmed; trailers in file order with folded
// continuations appended as "\n" + de-indented line.
func SplitTrailers(message string) (string, []Trailer) {
	lines := strings.Split(message, "\n")
	// Trailing blank lines are ignored when locating the last paragraph.
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end == 0 {
		return strings.TrimSpace(message), []Trailer{}
	}
	// The last paragraph: the maximal run of non-empty lines after the final
	// blank line (or from the top when no blank line separates a candidate
	// block — then the subject line is excluded from the candidate).
	start := end
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	// The trailer block must follow a blank line OR be the entire body: when
	// the run starts at the top of the message, git treats the subject line
	// specially — the candidate is the whole message after the first line
	// (§9.7.1); a single-line message has no candidate at all.
	if start == 0 {
		if end > 1 {
			start = 1
		} else {
			return strings.TrimSpace(message), []Trailer{}
		}
	}
	block := lines[start:end]
	trailers := []Trailer{}
	for _, line := range block {
		switch {
		case isTrailerLine(line):
			k, v, _ := strings.Cut(line, ":")
			trailers = append(trailers, Trailer{Key: k, Value: strings.TrimLeft(v, " \t")})
		case len(line) > 0 && (line[0] == ' ' || line[0] == '\t'):
			if len(trailers) == 0 {
				// continuation with no preceding trailer: not a trailer block
				return strings.TrimSpace(message), []Trailer{}
			}
			t := &trailers[len(trailers)-1]
			t.Value += "\n" + strings.TrimLeft(line, " \t")
		default:
			// any other line fails the block: no trailers (§9.7.2)
			return strings.TrimSpace(message), []Trailer{}
		}
	}
	body := strings.TrimSpace(strings.Join(lines[:start], "\n"))
	return body, trailers
}

// --- ls-tree (-z) parsing (§9.4) ----------------------------------------------------

// parseLsTree parses `git ls-tree -l -z` output: "<mode> SP <type> SP <sha>
// SP <size> TAB <name>" NUL-terminated; size "-" (trees/submodules) → -1.
func parseLsTree(b []byte) []TreeEntry {
	entries := []TreeEntry{}
	for _, rec := range strings.Split(string(b), "\x00") {
		if rec == "" {
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		meta, name := rec[:tab], rec[tab+1:]
		f := strings.Fields(meta)
		if len(f) != 4 {
			continue
		}
		size := int64(-1)
		if f[3] != "-" {
			if n, err := strconv.ParseInt(f[3], 10, 64); err == nil {
				size = n
			}
		}
		entries = append(entries, TreeEntry{
			Name: name,
			Type: f[1],
			Mode: f[0],
			Size: size,
			SHA:  f[2],
		})
	}
	return entries
}

// sortTreeEntries orders directories first, then byte order by name (NOT
// git's order) — §9.4.
func sortTreeEntries(entries []TreeEntry) {
	less := func(i, j int) bool {
		a, b := entries[i], entries[j]
		ta, tb := a.Type == "tree", b.Type == "tree"
		if ta != tb {
			return ta
		}
		return a.Name < b.Name
	}
	// insertion sort keeps this allocation-free for typical directory sizes
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// --- per-entry last-commit dates (§9.4, issue #301) -----------------------------------

// treeLogMarker prefixes every commit header line of the tree-dates walk so
// headers split unambiguously from the 1-char status records. The sha and
// %cI date contain no NUL/space, so a header is exactly three
// space-separated fields.
const treeLogMarker = "WALHUBTREE"

// defaultMaxTreeLog bounds the tree-dates walk when the config carries no
// positive value (zero-value walView in tests, unset key). Mirrors the
// [import] bounds pattern: one cap, validated >= 1, fail-open past it.
const defaultMaxTreeLog = 200

// treeLogArgv is the normative per-entry-dates walk (pinned argv — law 2):
// one `git log` per tree listing, newest-first, first-parent history (a merge
// attributes its whole merged diff, the same semantic as the merge-commit
// row on the commits page), renames off (a rename lands as delete+add, so
// the new path still attributes — no rename-pair parsing), -z for exact
// path bytes (no C-quoting misses), --max-count capping the walk. <sha> is
// the resolved commit; dir is the listing directory ("" = root, no pathspec).
func treeLogArgv(sha, dir string, cap int) []string {
	argv := []string{"log", "--format=" + treeLogMarker + " %H %cI",
		"--name-status", "--no-renames", "--no-color", "--first-parent",
		"-z", "--max-count=" + itoa(cap), sha}
	if dir != "" {
		argv = append(argv, "--", dir)
	}
	return argv
}

// assignTreeCommitMeta stamps each dateable entry (everything but submodules,
// type "commit" — gitlinks carry no history of their own) with the newest
// commit touching its path, parsed from one treeLogArgv walk's stdout.
// Newest-first order makes the first touch per direct child win; parsing
// stops once every dateable entry is assigned (the walk itself is bounded by
// --max-count). Entries untouched within the walk keep no date — the UI
// renders the neutral fallback, never the HEAD stamp. Malformed input
// degrades to unassigned, never to a wrong date.
func assignTreeCommitMeta(entries []TreeEntry, dir string, out []byte) {
	pending := 0
	idx := make(map[string]int, len(entries))
	for i := range entries {
		if entries[i].Type == "commit" {
			continue // submodules are dateless by design
		}
		idx[entries[i].Name] = i
		pending++
	}
	if pending == 0 || len(out) == 0 {
		return
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	// -z shape (verified byte-for-byte against real git): per commit,
	// "MARK <sha> <date>" NUL, "\n" (the --format line ending), then
	// alternating 1-char status and path records, each NUL-terminated; the
	// next commit's MARK follows the previous path's NUL directly.
	// Collapse the header's "\x00\n" first: NUL never occurs inside a path,
	// so this only touches separators, and the "\n" would otherwise glue
	// onto the first status record ("\nM"). Positional parsing (not content
	// sniffing) keeps 1-char paths unambiguous.
	norm := strings.ReplaceAll(string(out), "\x00\n", "\x00")
	var sha, date string
	haveCommit := false
	expectPath := false
	for _, f := range strings.Split(norm, "\x00") {
		// Headers split positionally: a MARK record is a commit header only
		// in status position (!expectPath). In path position the record IS
		// the touched path — even one literally named "WALHUBTREE <sha>
		// <date>" (a valid-looking header there must not clobber haveCommit
		// or poison later attributions; --no-renames emits no rename pairs,
		// so a status is always followed by exactly one path).
		if !expectPath && strings.HasPrefix(f, treeLogMarker+" ") {
			parts := strings.Split(f, " ")
			if len(parts) == 3 && parts[1] != "" && !parseRFC3339(parts[2]).IsZero() {
				sha, date, haveCommit = parts[1], parts[2], true
			} else {
				haveCommit = false
			}
			expectPath = false
			continue
		}
		if !haveCommit || f == "" {
			continue
		}
		if !expectPath {
			if len(f) == 1 && f != "\n" {
				expectPath = true // single-letter status; next record is its path
			}
			continue
		}
		expectPath = false
		rest := f
		if prefix != "" {
			var ok bool
			rest, ok = strings.CutPrefix(f, prefix)
			if !ok {
				continue // outside the listing dir (defensive: pathspec filters)
			}
		}
		name := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name = rest[:i] // nested path attributes its direct child
		}
		if j, ok := idx[name]; ok && entries[j].CommitSHA == "" {
			entries[j].CommitSHA, entries[j].CommitTime = sha, date
			if pending--; pending == 0 {
				return
			}
		}
	}
}

// --- numstat -z parsing (§9.8) -------------------------------------------------------

// parseNumstatPatch splits `git show --format= --no-color -M
// --diff-merges=first-parent --root --numstat -z` output into the --numstat
// records (in output order; rename records carry src then dst as separate NUL
// fields — the dst is emitted once) and the remainder (the unified diff,
// passed through verbatim minus the empty line --format= leaves before it).
func parseNumstatPatch(b []byte) ([]Stat, string) {
	stats := []Stat{}
	s := string(b)
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], "diff --git ") {
			break
		}
		// Counts record, two shapes (verified byte-for-byte against real
		// git show/diff-tree --numstat -z): a RENAME record is
		// "<add>\t<del>\t\0<src>\0<dst>\0" — the NUL terminates the counts
		// segment and the path fields follow, each NUL-terminated. A plain
		// record is "<add>\t<del>\t<path>\0" — one NUL terminates the whole
		// record and the path rides in the counts segment.
		j := strings.IndexByte(s[i:], '\x00')
		if j < 0 {
			break
		}
		counts := strings.Split(s[i:i+j], "\t")
		i += j + 1
		if len(counts) < 2 || !isNumField(counts[0]) || !isNumField(counts[1]) {
			break
		}
		path := ""
		if len(counts) >= 3 && counts[2] != "" {
			path = counts[2] // plain record: path already consumed
		} else {
			// Rename shape: path fields are NUL-terminated; src then dst.
			j = strings.IndexByte(s[i:], '\x00')
			if j < 0 {
				break
			}
			path = s[i : i+j]
			i += j + 1
			if i < len(s) && !isNumStart(s[i]) && !strings.HasPrefix(s[i:], "diff --git ") && s[i] != '\n' {
				k := strings.IndexByte(s[i:], '\x00')
				if k >= 0 {
					path = s[i : i+k] // rename: emit dst, once
					i += k + 1
				}
			}
		}
		stats = append(stats, Stat{Path: path, Additions: numOrNeg1(counts[0]), Deletions: numOrNeg1(counts[1])})
	}
	patch := strings.TrimPrefix(s[i:], "\n") // the empty line --format= leaves
	return stats, patch
}

// isNumStart reports whether b begins a counts record (digit or "-").
func isNumStart(b byte) bool {
	return b >= '0' && b <= '9' || b == '-'
}

func isNumField(s string) bool {
	if s == "-" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

func numOrNeg1(s string) int64 {
	if s == "-" {
		return -1 // binary (§9.8: emit -1/-1)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// --- 6-field UTC cron (schedule_human + next, §11 describe) --------------------------

var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

// cronField parses one cron field into the set of allowed values within
// [lo, hi]. Supports numbers, names, lists, ranges, and */steps.
func cronField(spec string, lo, hi int, names map[string]int) (map[int]bool, bool) {
	set := map[int]bool{}
	expand := func(tok string) ([]int, bool) {
		step := 1
		if i := strings.IndexByte(tok, '/'); i >= 0 {
			n, err := strconv.Atoi(tok[i+1:])
			if err != nil || n <= 0 {
				return nil, false
			}
			step = n
			tok = tok[:i]
		}
		a, b := lo, hi
		if tok != "*" {
			if i := strings.IndexByte(tok, '-'); i >= 0 {
				x, ok1 := cronValue(tok[:i], names)
				y, ok2 := cronValue(tok[i+1:], names)
				if !ok1 || !ok2 {
					return nil, false
				}
				a, b = x, y
			} else {
				v, ok := cronValue(tok, names)
				if !ok {
					return nil, false
				}
				if step != 1 {
					a, b = v, hi
				} else {
					return []int{v}, true
				}
			}
		}
		var out []int
		for v := a; v <= b; v += step {
			out = append(out, v)
		}
		return out, true
	}
	for _, tok := range strings.Split(spec, ",") {
		vals, ok := expand(strings.ToLower(strings.TrimSpace(tok)))
		if !ok {
			return nil, false
		}
		for _, v := range vals {
			if v < lo || v > hi {
				return nil, false
			}
			set[v] = true
		}
	}
	return set, len(set) > 0
}

func cronValue(tok string, names map[string]int) (int, bool) {
	if v, ok := names[tok]; ok {
		return v, true
	}
	n, err := strconv.Atoi(tok)
	return n, err == nil
}

// cronNext returns the next fire time of a 6-field UTC cron (sec min hour
// dom mon dow) strictly after `after`; ok=false when the spec is invalid.
func cronNext(spec string, after time.Time) (time.Time, bool) {
	f := strings.Fields(strings.TrimSpace(spec))
	if len(f) != 6 {
		return time.Time{}, false
	}
	sec, ok1 := cronField(f[0], 0, 59, nil)
	min, ok2 := cronField(f[1], 0, 59, nil)
	hr, ok3 := cronField(f[2], 0, 23, nil)
	dom, ok4 := cronField(f[3], 1, 31, nil)
	mon, ok5 := cronField(f[4], 1, 12, monthNames)
	dow, ok6 := cronField(f[5], 0, 7, dayNames)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return time.Time{}, false
	}
	if dow[7] { // git/cron style: 7 == sunday
		dow[0] = true
	}
	// month/day-of-month/day-of-week OR rule (standard vixie behavior: when
	// both dom and dow are restricted, either matches).
	domRestricted := f[3] != "*"
	dowRestricted := f[5] != "*"

	t := after.Truncate(time.Minute).UTC()
	t = t.Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !mon[int(t.Month())] {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		dayOK := dom[t.Day()]
		if domRestricted && dowRestricted {
			dayOK = dom[t.Day()] || dow[int(t.Weekday())]
		} else if dowRestricted {
			dayOK = dow[int(t.Weekday())]
		}
		if !dayOK {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !hr[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !min[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		if !sec[t.Second()] {
			t = t.Truncate(time.Second).Add(time.Second)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// cronHuman renders a schedule readably ("daily at 23:00 UTC", "weekly on
// Sunday at 23:00 UTC", "hourly at :00 UTC"); falls back to the raw spec.
func cronHuman(spec string) string {
	f := strings.Fields(strings.TrimSpace(spec))
	if len(f) != 6 {
		return spec
	}
	h, err1 := strconv.Atoi(f[2])
	m, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || f[0] != "0" {
		return spec
	}
	dayPart := "daily"
	switch {
	case f[5] != "*":
		dayPart = "weekly on " + dowHuman(f[5])
	case f[4] != "*":
		return spec
	case f[3] != "*":
		dayPart = "monthly on day " + f[3]
	}
	return dayPart + " at " + twoDigits(h) + ":" + twoDigits(m) + " UTC"
}

// dowHuman renders a cron day-of-week token ("Sun", "sun", "6") capitalized.
func dowHuman(tok string) string {
	t := strings.ToLower(tok)
	if d, ok := dayNames[t]; ok {
		t = dowName(d)
	}
	if t == "" {
		return tok
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

func dowName(d int) string {
	names := []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	if d < 0 || d > 6 {
		return ""
	}
	return names[d]
}

func twoDigits(n int) string {
	s := strconv.Itoa(n)
	if len(s) == 1 {
		return "0" + s
	}
	return s
}
