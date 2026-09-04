package issues

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Wire/storage shapes (§1). Conventions: arrays serialize as [] never null
// (constructors normalize), timestamps are RFC 3339 UTC, errors are plain
// text. Unknown JSON fields on read are ignored; unknown keys on mutating
// requests are rejected 400 (fail closed, same rule as policy effects).

// Event types (exact set, §1.2).
const (
	EventOpened           = "opened"
	EventCommented        = "commented"
	EventTitleChanged     = "title_changed"
	EventLabelsChanged    = "labels_changed"
	EventAssigneesChanged = "assignees_changed"
	EventStateChanged     = "state_changed"
	EventMilestoneChanged = "milestone_changed"
	EventReferenced       = "referenced"
	EventCrossReferenced  = "cross_referenced"
	EventReactionChanged  = "reaction_changed"
	EventClosedByPR       = "closed_by_pr"
)

// Thread states and close reasons.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

const (
	ReasonCompleted  = "completed"
	ReasonNotPlanned = "not_planned"
)

// Limits (§§1-3,7).
const (
	MaxTitleLen    = 256
	MaxBodyBytes   = 64 << 10
	MaxAssignees   = 10
	MaxLabelLen    = 64
	MaxLabelDesc   = 200
	MaxRefsPerBody = 100
)

// ReactionContents is the closed emoji-name set (§8); unknown → 400.
var ReactionContents = map[string]bool{
	"+1": true, "-1": true, "laugh": true, "hooray": true,
	"confused": true, "heart": true, "rocket": true, "eyes": true,
}

var colorRe = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

// Thread is the CAS'd header (§1.1, P3). Num equals the <num:06x> key
// value. Num, Kind, Author, CreatedAt never change after Create;
// everything else is CAS-mutable through the two-step. ReactionSummary
// keys are %06x event seqs (minimum width 6, growing naturally); values
// are emoji name → count.
type Thread struct {
	Num             int                       `json:"num"`
	Kind            string                    `json:"kind"`
	Title           string                    `json:"title"`
	State           string                    `json:"state"`
	StateReason     *string                   `json:"state_reason"`
	Author          string                    `json:"author"`
	CreatedAt       string                    `json:"created_at"`
	UpdatedAt       string                    `json:"updated_at"`
	Labels          []string                  `json:"labels"`
	Assignees       []string                  `json:"assignees"`
	Milestone       *string                   `json:"milestone"`
	Participants    []string                  `json:"participants"`
	NextEventSeq    int                       `json:"next_event_seq"`
	CommentCount    int                       `json:"comment_count"`
	ReactionSummary map[string]map[string]int `json:"reaction_summary"`
	Version         int                       `json:"version"`
}

// Card is the list projection (§2): the header minus version,
// next_event_seq, and reaction_summary. Never authoritative; the header
// wins.
type Card struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	StateReason  *string  `json:"state_reason"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Milestone    *string  `json:"milestone"`
	Author       string   `json:"author"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	CommentCount int      `json:"comment_count"`
}

// cardOf projects a header onto its card.
func cardOf(t *Thread) Card {
	return Card{
		Num: t.Num, Kind: t.Kind, Title: t.Title, State: t.State,
		StateReason: t.StateReason, Labels: nonNilStr(t.Labels),
		Assignees: nonNilStr(t.Assignees), Milestone: t.Milestone,
		Author: t.Author, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		CommentCount: t.CommentCount,
	}
}

// Event is the immutable log object (§1.2, P3). Seq matches the key
// digits. Payload fields are flat alongside the envelope; Body/Source
// pointers are set per type.
type Event struct {
	Seq     int            `json:"seq"`
	Type    string         `json:"type"`
	Actor   string         `json:"actor"`
	At      string         `json:"at"`
	Body    *string        `json:"body,omitempty"`
	From    *string        `json:"from,omitempty"`
	To      *string        `json:"to,omitempty"`
	Added   []string       `json:"added,omitempty"`
	Removed []string       `json:"removed,omitempty"`
	Reason  *string        `json:"reason,omitempty"`
	Source  map[string]any `json:"source,omitempty"`

	TargetEventSeq *int    `json:"target_event_seq,omitempty"`
	Content        *string `json:"content,omitempty"`
	Op             *string `json:"op,omitempty"`
	PRNum          *int    `json:"pr_num,omitempty"`
	Keyword        *string `json:"keyword,omitempty"`
}

// Index is the CAS'd list object (§2, P4). Open holds every kind:"issue"
// open thread newest-activity-first; ClosedRecent holds closed threads
// newer than CompactedThrough. CompactedThrough is a <num:06x> watermark
// advanced monotonically by compaction; compacted threads are served by
// paginated LIST (P5).
type Index struct {
	Version          int    `json:"version"`
	CompactedThrough string `json:"compacted_through"`
	Open             []Card `json:"open"`
	ClosedRecent     []Card `json:"closed_recent"`
}

// Label is one entry of the repo label set (§3.1). Names are immutable
// identities — no rename endpoint; rename = delete + create.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// LabelSet is meta/labels.json (CAS'd).
type LabelSet struct {
	Version int     `json:"version"`
	Labels  []Label `json:"labels"`
}

// Milestone is one meta/milestones/<id>.json (§3.2). Progress (percent
// complete) is DERIVED on read, never stored; open/closed counters are
// denormalized display state (thread headers are the truth).
type Milestone struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	DueOn        *string `json:"due_on"`
	State        string  `json:"state"`
	OpenIssues   int     `json:"open_issues"`
	ClosedIssues int     `json:"closed_issues"`
	CreatedBy    string  `json:"created_by"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	// Percent is derived on read only (0-100); never stored.
	Percent int `json:"percent"`
}

// Counter is the {"next": N} allocator shape (P2 counter + milestone ids).
type Counter struct {
	Next int `json:"next"`
}

// --- validation -----------------------------------------------------------

// validateTitle trims and bounds an issue title (1–256 chars).
func validateTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", fmt.Errorf("%w: title must not be empty", ErrInvalid)
	}
	if len([]rune(t)) > MaxTitleLen {
		return "", fmt.Errorf("%w: title exceeds %d characters", ErrInvalid, MaxTitleLen)
	}
	return t, nil
}

// validateBody bounds a comment/open body (1 byte–64 KiB raw text).
func validateBody(body string) error {
	if body == "" {
		return fmt.Errorf("%w: body must not be empty", ErrInvalid)
	}
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, MaxBodyBytes)
	}
	return nil
}

// validateLabelName bounds a label name (1–64 chars).
func validateLabelName(name string) (string, error) {
	t := strings.TrimSpace(name)
	if t == "" || len([]rune(t)) > MaxLabelLen {
		return "", fmt.Errorf("%w: label name must be 1-%d characters", ErrInvalid, MaxLabelLen)
	}
	return t, nil
}

// validateColor checks 6-hex RGB without '#'.
func validateColor(color string) (string, error) {
	c := strings.TrimSpace(color)
	if !colorRe.MatchString(c) {
		return "", fmt.Errorf("%w: color must be 6-hex RGB without '#', got %q", ErrInvalid, color)
	}
	return strings.ToLower(c), nil
}

// validateMilestoneTitle bounds a milestone title (1–256 chars).
func validateMilestoneTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" || len([]rune(t)) > MaxTitleLen {
		return "", fmt.Errorf("%w: milestone title must be 1-%d characters", ErrInvalid, MaxTitleLen)
	}
	return t, nil
}

// --- small helpers ----------------------------------------------------------

func nonNilStr(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// uniqSorted sorts and dedups principal/name lists (stored sorted, unique).
func uniqSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:1]
	for _, v := range cp[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// seqKey renders a reaction_summary map key (%06x, minimum width 6).
func seqKey(seq int) string { return fmt.Sprintf("%06x", seq) }

// parseThread decodes a thread header; unknown fields are ignored on read.
func parseThread(raw []byte) (*Thread, error) {
	var t Thread
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%w: thread.json: %v", ErrCorrupt, err)
	}
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Assignees == nil {
		t.Assignees = []string{}
	}
	if t.Participants == nil {
		t.Participants = []string{}
	}
	if t.ReactionSummary == nil {
		t.ReactionSummary = map[string]map[string]int{}
	}
	return &t, nil
}

// encodeThread renders a thread header.
func encodeThread(t *Thread) []byte {
	t.Labels = nonNilStr(t.Labels)
	t.Assignees = nonNilStr(t.Assignees)
	t.Participants = nonNilStr(t.Participants)
	if t.ReactionSummary == nil {
		t.ReactionSummary = map[string]map[string]int{}
	}
	raw, _ := json.Marshal(t)
	return raw
}

// parseEvent decodes one immutable event; unknown fields ignored on read.
func parseEvent(raw []byte) (*Event, error) {
	var e Event
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: event: %v", ErrCorrupt, err)
	}
	return &e, nil
}

// encodeEvent renders one immutable event.
func encodeEvent(e *Event) []byte {
	raw, _ := json.Marshal(e)
	return raw
}

// parseIndex decodes the list index.
func parseIndex(raw []byte) (*Index, error) {
	var ix Index
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&ix); err != nil {
		return nil, fmt.Errorf("%w: index.json: %v", ErrCorrupt, err)
	}
	if ix.Open == nil {
		ix.Open = []Card{}
	}
	if ix.ClosedRecent == nil {
		ix.ClosedRecent = []Card{}
	}
	for i := range ix.Open {
		ix.Open[i].Labels = nonNilStr(ix.Open[i].Labels)
		ix.Open[i].Assignees = nonNilStr(ix.Open[i].Assignees)
	}
	for i := range ix.ClosedRecent {
		ix.ClosedRecent[i].Labels = nonNilStr(ix.ClosedRecent[i].Labels)
		ix.ClosedRecent[i].Assignees = nonNilStr(ix.ClosedRecent[i].Assignees)
	}
	return &ix, nil
}
