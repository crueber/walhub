package pulls

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors (07 §2: mapped to plain-text statuses in http.go).
var (
	// ErrNotFound marks an unknown PR/thread/ref (→ 404).
	ErrNotFound = errors.New("unknown pull request")
	// ErrInvalid marks a bad request body/field (→ 400).
	ErrInvalid = errors.New("invalid pull request")
	// ErrUnauthorized marks anonymous-denied access (→ 401 + Bearer).
	ErrUnauthorized = errors.New("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = errors.New("forbidden")
	// ErrConflict marks state conflicts: duplicate open PR, stale CAS,
	// milestone-style 409s (→ 409).
	ErrConflict = errors.New("conflict")
	// ErrUnprocessable marks resolvable-but-unusable refs: unknown revision,
	// unreachable head (→ 422).
	ErrUnprocessable = errors.New("unprocessable")
	// ErrUnavailable marks a down dependency (git pool, identity state)
	// (→ 503 + Retry-After: 15).
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrCorrupt marks an unreadable bucket object (→ 500-class).
	ErrCorrupt = errors.New("corrupt object")
)

// statusFor maps a service error onto its HTTP status.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrInvalid):
		return 400
	case errors.Is(err, ErrUnauthorized):
		return 401
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrConflict):
		return 409
	case errors.Is(err, ErrUnprocessable):
		return 422
	case errors.Is(err, ErrUnavailable):
		return 503
	default:
		return 500
	}
}

// Thread states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// Merge strategies (§5, exact set).
const (
	StrategyMerge  = "merge"
	StrategySquash = "squash"
	StrategyRebase = "rebase"
)

// Mergeable states (§2.2, exact set).
const (
	MergeableClean    = "clean"
	MergeableDirty    = "dirty"
	MergeableBehind   = "behind"
	MergeableUpToDate = "up_to_date"
	MergeableUnknown  = "unknown"
)

// Endpoint describes one PR ref endpoint: the repo it lives in, the ref,
// and the resolved sha.
type Endpoint struct {
	Repo string `json:"repo"` // "owner/name"
	Ref  string `json:"ref"`  // full ref name
	SHA  string `json:"sha"`  // full hex at open (or at last refresh)
}

// ForkInfo marks a cross-fork head: the fork repo holding the head ref.
type ForkInfo struct {
	Repo string `json:"repo"` // "owner/name" of the fork
}

// PRDoc is pulls/<num>/pr.json (§2.1, CAS'd): the PR-sidecar snapshot (refs
// + shas at open, merge outcome). thread.json stays the P3 header exactly
// (02 owns its shape); PR-specific fields MUST NOT leak into it. Body is an
// additive optional description field (14 §14.12 field rule: the opened
// event carries the original body; Body is the editable view).
type PRDoc struct {
	Num               int       `json:"num"`
	Kind              string    `json:"kind"` // always "pr"
	Base              Endpoint  `json:"base"`
	Head              Endpoint  `json:"head"`
	Fork              *ForkInfo `json:"fork"`
	Body              string    `json:"body,omitempty"`
	Merged            bool      `json:"merged"`
	MergedAt          *string   `json:"merged_at"`
	MergedBy          *string   `json:"merged_by"`
	MergeCommitSHA    *string   `json:"merge_commit_sha"`
	MergeStrategy     *string   `json:"merge_strategy"`
	HeadForcePushedAt *string   `json:"head_force_pushed_at"`
	HeadPublished     bool      `json:"head_published"`
	Draft             bool      `json:"draft"`
	Version           int       `json:"version"`
}

// MergeableDoc is pulls/<num>/mergeable.json (§2.2, derived
// Create-then-CAS overwrite): the stamp triple (base_ref, base_sha,
// head_sha) IS the invalidation key — a reader compares it against the live
// ref shas; any mismatch ⇒ the cache is stale. conflicts[] is populated
// only when state is dirty.
type MergeableDoc struct {
	BaseRef    string   `json:"base_ref"`
	BaseSHA    string   `json:"base_sha"`
	HeadSHA    string   `json:"head_sha"`
	MergeBase  string   `json:"merge_base"`
	State      string   `json:"state"`
	Conflicts  []string `json:"conflicts"`
	Rebaseable bool     `json:"rebaseable"`
	ComputedAt string   `json:"computed_at"`
}

// ForkEntry is one child row of the parent-side fork index.
type ForkEntry struct {
	Repo     string `json:"repo"` // "owner/name" of the fork
	ForkedAt string `json:"forked_at"`
}

// ForksIndex is repos/<o>/<r>/meta/forks.json (CAS'd): the parent-side fork
// index listing children. The maintain unit consults children's manifests
// before deleting superseded packs (GC rule, §7); grand-children are
// discovered transitively, one level per pass.
type ForksIndex struct {
	Version int         `json:"version"`
	Forks   []ForkEntry `json:"forks"`
}

// ForkDoc is repos/<o2>/<r2>/fork.json (Create once, then CAS'd for
// merged_upstream_at): fork-side provenance.
type ForkDoc struct {
	Parent           string  `json:"parent"` // "owner/name" of the parent
	ForkedAt         string  `json:"forked_at"`
	MergedUpstreamAt *string `json:"merged_upstream_at"`
	Version          int     `json:"version"`
}

// Thread is the shared P3 header as pulls reads it (02 owns the shape;
// unknown fields ignored on read — PR threads carry no PR fields by
// contract, §2.1). Only the fields pulls needs are modeled. The three
// review-owned fields (04 §7) are preserved opaquely: pulls round-trips
// them verbatim on every header write but never interprets them (04 owns
// the semantics; the merge-time gate reads the events, never this cache).
type Thread struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	Author       string   `json:"author"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Participants []string `json:"participants"`
	NextEventSeq int      `json:"next_event_seq"`
	CommentCount int      `json:"comment_count"`
	Version      int      `json:"version"`
	// NextReviewSeq reserves review seqs (04 §3); NextThreadNum reserves
	// tids (04 §4); ReviewSummary is 04's denormalized render cache (§6).
	NextReviewSeq int             `json:"next_review_seq,omitempty"`
	NextThreadNum int             `json:"next_thread_num,omitempty"`
	ReviewSummary json.RawMessage `json:"review_summary,omitempty"`
}

// Card is the shared P4 index projection as pulls reads it (02 owns the
// shape). PR rows are enriched from pr.json sidecars at list time.
type Card struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Author       string   `json:"author"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	CommentCount int      `json:"comment_count"`
}

// Index is the shared P4 list object as pulls reads it (02 owns the shape).
type Index struct {
	Version          int    `json:"version"`
	CompactedThrough string `json:"compacted_through"`
	Open             []Card `json:"open"`
	ClosedRecent     []Card `json:"closed_recent"`
}

// Counter is the shared {"next": N} allocator shape (P2).
type Counter struct {
	Next int `json:"next"`
}

// Event is one shared P3 immutable event as pulls writes it (opened,
// commented, title/state edits, merged, head_force_pushed, referenced).
type Event struct {
	Seq     int            `json:"seq"`
	Type    string         `json:"type"`
	Actor   string         `json:"actor"`
	At      string         `json:"at"`
	Body    *string        `json:"body,omitempty"`
	From    *string        `json:"from,omitempty"`
	To      *string        `json:"to,omitempty"`
	Reason  *string        `json:"reason,omitempty"`
	Source  map[string]any `json:"source,omitempty"`
	PRNum   *int           `json:"pr_num,omitempty"`
	Keyword *string        `json:"keyword,omitempty"`
	// Merge carries the merge outcome on "merged" events.
	MergeCommitSHA *string `json:"merge_commit_sha,omitempty"`
	Strategy       *string `json:"strategy,omitempty"`
}

// Event types pulls writes (02's set plus the PR-side trigger).
const (
	EventOpened          = "opened"
	EventCommented       = "commented"
	EventTitleChanged    = "title_changed"
	EventStateChanged    = "state_changed"
	EventReferenced      = "referenced"
	EventMerged          = "merged"
	EventHeadForcePushed = "head_force_pushed"
)

// Limits (§§2-3,7).
const (
	MaxTitleLen  = 256
	MaxBodyBytes = 64 << 10
)

// --- validation -----------------------------------------------------------

// validateTitle trims and bounds a PR title (1–256 chars).
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

// validateBody bounds an optional PR body (≤ 64 KiB raw text).
func validateBody(body string) error {
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, MaxBodyBytes)
	}
	return nil
}

// validateRefName checks a full ref name (refs/… shape; the git layer's
// ValidateRefName grammar reduced to the wire check pulls needs).
func validateRefName(ref string) error {
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") || strings.ContainsAny(ref, " \n\r~^:?*[`@{") {
		return fmt.Errorf("%w: invalid ref %q", ErrInvalid, ref)
	}
	return nil
}

// validateSHA checks a full 40/64-hex sha.
func validateSHA(sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return fmt.Errorf("%w: sha must be 40/64-hex, got %q", ErrInvalid, sha)
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: sha must be lowercase hex, got %q", ErrInvalid, sha)
		}
	}
	return nil
}

// validateStrategy checks the merge strategy (exact §5 set).
func validateStrategy(s string) error {
	switch s {
	case StrategyMerge, StrategySquash, StrategyRebase:
		return nil
	}
	return fmt.Errorf("%w: strategy must be merge|squash|rebase, got %q", ErrInvalid, s)
}

// --- codec -----------------------------------------------------------------

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
	return &t, nil
}

func encodeThread(t *Thread) []byte {
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Assignees == nil {
		t.Assignees = []string{}
	}
	if t.Participants == nil {
		t.Participants = []string{}
	}
	raw, _ := json.Marshal(t)
	return raw
}

func parseEvent(raw []byte) (*Event, error) {
	var e Event
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: event: %v", ErrCorrupt, err)
	}
	return &e, nil
}

func encodeEvent(e *Event) []byte {
	raw, _ := json.Marshal(e)
	return raw
}

// parsePR decodes a pr.json sidecar.
func parsePR(raw []byte) (*PRDoc, error) {
	var p PRDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: pr.json: %v", ErrCorrupt, err)
	}
	return &p, nil
}

func encodePR(p *PRDoc) []byte {
	raw, _ := json.Marshal(p)
	return raw
}

// parseMergeable decodes a mergeable.json cache (nil-tolerant: conflicts
// normalize to []).
func parseMergeable(raw []byte) (*MergeableDoc, error) {
	var m MergeableDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: mergeable.json: %v", ErrCorrupt, err)
	}
	if m.Conflicts == nil {
		m.Conflicts = []string{}
	}
	return &m, nil
}

func encodeMergeable(m *MergeableDoc) []byte {
	if m.Conflicts == nil {
		m.Conflicts = []string{}
	}
	raw, _ := json.Marshal(m)
	return raw
}

// parseIndex decodes the shared list index.
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

// parseForks decodes meta/forks.json.
func parseForks(raw []byte) (*ForksIndex, error) {
	var f ForksIndex
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: forks.json: %v", ErrCorrupt, err)
	}
	if f.Forks == nil {
		f.Forks = []ForkEntry{}
	}
	return &f, nil
}

// --- small helpers ----------------------------------------------------------

func nonNilStr(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// uniqSorted sorts and dedups name lists (stored sorted, unique).
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

func strPtr(s string) *string { return &s }
