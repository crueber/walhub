package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors (07 §2: mapped to plain-text statuses in http.go).
var (
	// ErrNotFound marks an unknown PR/review/thread (→ 404).
	ErrNotFound = errors.New("unknown pull request")
	// ErrInvalid marks a bad request body/field (→ 400).
	ErrInvalid = errors.New("invalid review")
	// ErrUnauthorized marks anonymous-denied access (→ 401 + Bearer).
	ErrUnauthorized = errors.New("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = errors.New("forbidden")
	// ErrConflict marks state conflicts: stale CAS, double dismissal
	// (→ 409).
	ErrConflict = errors.New("conflict")
	// ErrUnprocessable marks submittable-but-unusable input: author
	// self-approval (→ 422).
	ErrUnprocessable = errors.New("unprocessable")
	// ErrUnavailable marks a down dependency (→ 503 + Retry-After: 15).
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

// Review states (§3, exact set).
const (
	StateApproved         = "APPROVED"
	StateChangesRequested = "CHANGES_REQUESTED"
	StateCommented        = "COMMENTED"
	StateDismissed        = "DISMISSED" // rollup-only: latest demoted by a review_dismissed event
)

// Decisions (§6, exact set).
const (
	DecisionApproved         = "APPROVED"
	DecisionChangesRequested = "CHANGES_REQUESTED"
	DecisionReviewRequired   = "REVIEW_REQUIRED"
)

// Thread sides (§4, exact set).
const (
	SideNew = "NEW"
	SideOld = "OLD"
)

// Kinds of the immutable event objects this package writes.
const (
	KindReview              = "review"
	KindReviewDismissed     = "review_dismissed"
	KindReviewThreadComment = "review_thread_comment"
)

// ReviewEvent is one immutable review (Create-only, §3).
type ReviewEvent struct {
	Kind      string `json:"kind"` // "review" | "review_dismissed"
	Seq       int    `json:"seq"`
	At        string `json:"at"` // RFC 3339 UTC
	By        string `json:"by"`
	State     string `json:"state,omitempty"`      // review: APPROVED|CHANGES_REQUESTED|COMMENTED
	CommitSHA string `json:"commit_sha,omitempty"` // review: full-hex head sha at submit (pinned)
	Body      string `json:"body,omitempty"`       // markdown-lite, "" allowed
	Dismisses *int   `json:"dismisses,omitempty"`  // review_dismissed: target review seq
	Reason    string `json:"reason,omitempty"`     // review_dismissed
}

// Anchor is the normative §4 line anchor.
type Anchor struct {
	Path       string `json:"path"`
	Side       string `json:"side"` // NEW | OLD
	OldStart   int    `json:"old_start"`
	OldLines   int    `json:"old_lines"`
	NewStart   int    `json:"new_start"`
	NewLines   int    `json:"new_lines"`
	CommitSHA  string `json:"commit_sha"`
	ContextSHA string `json:"context_sha"` // hex SHA-256 drift hash (§4)
}

// ThreadHeader is the CAS'd header of one line-anchored thread (§4).
type ThreadHeader struct {
	TID          string `json:"tid"`
	Num          int    `json:"num"`
	Kind         string `json:"kind"` // always "review_thread"
	Anchor       Anchor `json:"anchor"`
	Resolved     bool   `json:"resolved"`
	ResolvedBy   string `json:"resolved_by,omitempty"`
	ResolvedAt   string `json:"resolved_at,omitempty"`
	CommentCount int    `json:"comment_count"`
	NextEventSeq int    `json:"next_event_seq"`
	CreatedAt    string `json:"created_at"`
	CreatedBy    string `json:"created_by"`
	UpdatedAt    string `json:"updated_at"`
	Version      int    `json:"version"`
}

// ThreadComment is one immutable comment event in a thread (§4).
type ThreadComment struct {
	Kind string `json:"kind"` // always "review_thread_comment"
	Seq  int    `json:"seq"`
	At   string `json:"at"`
	By   string `json:"by"`
	Body string `json:"body"`
}

// RequestedReviewer is one entry of the current-state index (§5).
type RequestedReviewer struct {
	Principal string `json:"principal"`
	By        string `json:"by"`
	At        string `json:"at"`
}

// ReviewRequests is pulls/<num>/review-requests.json (CAS'd, §5).
type ReviewRequests struct {
	Version   int                 `json:"version"`
	UpdatedAt string              `json:"updated_at"`
	Reviewers []RequestedReviewer `json:"reviewers"`
}

// LatestReview is one entry of review_summary.latest (§6).
type LatestReview struct {
	State     string `json:"state"` // APPROVED|CHANGES_REQUESTED|COMMENTED|DISMISSED
	Seq       int    `json:"seq"`
	CommitSHA string `json:"commit_sha"`
	At        string `json:"at"`
}

// ReviewSummary is the denormalized rollup on the PR header (§6). It is a
// pure-function render cache: recomputed inside the CAS loop from the
// immutable event set; racing writers converge, and the merge gate never
// trusts it (it re-derives by scan).
type ReviewSummary struct {
	Decision          string                  `json:"decision"`
	Latest            map[string]LatestReview `json:"latest"`
	Approvals         int                     `json:"approvals"`
	Requested         []string                `json:"requested"`
	ThreadsTotal      int                     `json:"threads_total"`
	ThreadsUnresolved int                     `json:"threads_unresolved"`
}

// PRHeader is the PR thread header as review reads/writes it: the full
// 02/03 shape (mirrored — 02 owns thread.json, 03's pulls.Thread owns the
// PR view; unknown fields are preserved because every field either side
// writes is modeled here) plus the three review-owned fields §7 names
// (next_review_seq, next_thread_num, review_summary). A test pins the
// round-trip against a pulls-written header so neither side's writes drop
// the other's fields.
type PRHeader struct {
	Num           int            `json:"num"`
	Kind          string         `json:"kind"`
	Title         string         `json:"title"`
	State         string         `json:"state"`
	Author        string         `json:"author"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
	Labels        []string       `json:"labels"`
	Assignees     []string       `json:"assignees"`
	Participants  []string       `json:"participants"`
	NextEventSeq  int            `json:"next_event_seq"`
	CommentCount  int            `json:"comment_count"`
	Version       int            `json:"version"`
	NextReviewSeq int            `json:"next_review_seq,omitempty"`
	NextThreadNum int            `json:"next_thread_num,omitempty"`
	ReviewSummary *ReviewSummary `json:"review_summary,omitempty"`
}

// PRSidecar is the pr.json sidecar as review reads it (03 owns the shape;
// only the fields review needs are modeled — head SHA for the commit_sha
// pin, base ref for the gate's rule match).
type PRSidecar struct {
	Num  int `json:"num"`
	Base struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo string `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo string `json:"repo"`
	} `json:"head"`
	Merged bool `json:"merged"`
}

// Limits (§§3-4).
const (
	MaxBodyBytes = 64 << 10
	MaxPathLen   = 1024
)

// --- validation ------------------------------------------------------------

// validateState checks a review verdict (exact §3 set).
func validateState(state string) error {
	switch state {
	case StateApproved, StateChangesRequested, StateCommented:
		return nil
	}
	return fmt.Errorf("%w: state must be APPROVED|CHANGES_REQUESTED|COMMENTED, got %q", ErrInvalid, state)
}

// validateBody bounds an optional review/thread body (≤ 64 KiB raw text;
// empty allowed for reviews, not for thread comments — enforced by the
// caller).
func validateBody(body string) error {
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, MaxBodyBytes)
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

// validateContextSHA checks the §4 drift hash (hex SHA-256, 64 chars).
func validateContextSHA(h string) error {
	if len(h) != 64 {
		return fmt.Errorf("%w: context_sha must be 64-hex SHA-256, got %q", ErrInvalid, h)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: context_sha must be lowercase hex, got %q", ErrInvalid, h)
		}
	}
	return nil
}

// validateAnchor checks the normative §4 anchor: side selects the indexed
// file (NEW ⇒ new range > 0, OLD ⇒ old range > 0), path non-empty,
// commit_sha pinned, context_sha well-formed. Drift-hash *correctness* is
// derived at view time by the client (§4/§8); the server validates shape
// only (no git subprocesses on this path, §7 Concurrency).
func validateAnchor(a Anchor) error {
	if strings.TrimSpace(a.Path) == "" || len(a.Path) > MaxPathLen || strings.Contains(a.Path, "\n") {
		return fmt.Errorf("%w: anchor path must be 1-%d chars with no newlines", ErrInvalid, MaxPathLen)
	}
	switch a.Side {
	case SideNew:
		if a.NewStart < 1 || a.NewLines < 1 {
			return fmt.Errorf("%w: NEW anchors need new_start/new_lines >= 1", ErrInvalid)
		}
	case SideOld:
		if a.OldStart < 1 || a.OldLines < 1 {
			return fmt.Errorf("%w: OLD anchors need old_start/old_lines >= 1", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: anchor side must be NEW|OLD, got %q", ErrInvalid, a.Side)
	}
	if err := validateSHA(a.CommitSHA); err != nil {
		return err
	}
	if err := validateContextSHA(a.ContextSHA); err != nil {
		return err
	}
	return nil
}

// validateTID checks the zero-padded 8-hex thread id.
func validateTID(tid string) error {
	if len(tid) != 8 {
		return fmt.Errorf("%w: unknown review thread %q", ErrNotFound, tid)
	}
	for i := 0; i < 8; i++ {
		c := tid[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: unknown review thread %q", ErrNotFound, tid)
		}
	}
	return nil
}

// DriftHash computes the normative §4 drift hash: hex SHA-256 over
// `path + "\n"` followed by up to 3 unchanged context lines before and up
// to 3 after the anchored range, verbatim except trailing-whitespace
// trimmed, LF-joined. before/after are the context lines in display order
// (callers pass at most 3 each; extras are ignored, documenting the
// "up to 3" bound in the signature). This is the server-side twin of the
// single `anchorContextSha` implementation in web/src/lib/diff.js (§8
// dogfood rule: UI + server share semantics — both hash the same bytes).
func DriftHash(path string, before, after []string) string {
	if len(before) > 3 {
		before = before[len(before)-3:]
	}
	if len(after) > 3 {
		after = after[:3]
	}
	var b strings.Builder
	b.WriteString(path)
	b.WriteString("\n")
	for _, l := range before {
		b.WriteString(strings.TrimRight(l, " \t\r"))
		b.WriteString("\n")
	}
	for _, l := range after {
		b.WriteString(strings.TrimRight(l, " \t\r"))
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// --- rollup (pure function, §6) ---------------------------------------------

// Rollup recomputes review_summary from the immutable event set. Pure:
// any racing writer computes the same value, so 412-retry converges
// without coordination. requested/threads are inputs because they live in
// other objects (review-requests.json, thread headers), not the review
// log. The per-reviewer fold is latestOf — shared with the merge gate, so
// the cached summary and the authoritative verdict can never disagree on
// what "latest" means.
func Rollup(reviews []*ReviewEvent, requested []string, threadsTotal, threadsUnresolved int) *ReviewSummary {
	latest := latestOf(reviews)
	decision := DecisionReviewRequired
	approvals := 0
	for _, l := range latest {
		if l.State == StateChangesRequested {
			decision = DecisionChangesRequested
			break
		}
	}
	for _, l := range latest {
		if l.State == StateApproved {
			approvals++
		}
	}
	if decision != DecisionChangesRequested && approvals > 0 {
		decision = DecisionApproved
	}
	if requested == nil {
		requested = []string{}
	}
	return &ReviewSummary{
		Decision: decision, Latest: latest, Approvals: approvals,
		Requested: requested, ThreadsTotal: threadsTotal, ThreadsUnresolved: threadsUnresolved,
	}
}

// --- codec ------------------------------------------------------------------

func parsePRHeader(raw []byte) (*PRHeader, error) {
	var h PRHeader
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("%w: thread.json: %v", ErrCorrupt, err)
	}
	if h.Labels == nil {
		h.Labels = []string{}
	}
	if h.Assignees == nil {
		h.Assignees = []string{}
	}
	if h.Participants == nil {
		h.Participants = []string{}
	}
	if h.ReviewSummary != nil {
		if h.ReviewSummary.Latest == nil {
			h.ReviewSummary.Latest = map[string]LatestReview{}
		}
		if h.ReviewSummary.Requested == nil {
			h.ReviewSummary.Requested = []string{}
		}
	}
	return &h, nil
}

func encodePRHeader(h *PRHeader) []byte {
	if h.Labels == nil {
		h.Labels = []string{}
	}
	if h.Assignees == nil {
		h.Assignees = []string{}
	}
	if h.Participants == nil {
		h.Participants = []string{}
	}
	raw, _ := json.Marshal(h)
	return raw
}

func parseReview(raw []byte) (*ReviewEvent, error) {
	var e ReviewEvent
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: review event: %v", ErrCorrupt, err)
	}
	return &e, nil
}

func encodeReview(e *ReviewEvent) []byte {
	raw, _ := json.Marshal(e)
	return raw
}

func parseThreadHeader(raw []byte) (*ThreadHeader, error) {
	var h ThreadHeader
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("%w: thread.json: %v", ErrCorrupt, err)
	}
	return &h, nil
}

func encodeThreadHeader(h *ThreadHeader) []byte {
	raw, _ := json.Marshal(h)
	return raw
}

func parseThreadComment(raw []byte) (*ThreadComment, error) {
	var e ThreadComment
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: thread comment: %v", ErrCorrupt, err)
	}
	return &e, nil
}

func encodeThreadComment(e *ThreadComment) []byte {
	raw, _ := json.Marshal(e)
	return raw
}

func parseReviewRequests(raw []byte) (*ReviewRequests, error) {
	var r ReviewRequests
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: review-requests.json: %v", ErrCorrupt, err)
	}
	if r.Reviewers == nil {
		r.Reviewers = []RequestedReviewer{}
	}
	return &r, nil
}

func encodeReviewRequests(r *ReviewRequests) []byte {
	if r.Reviewers == nil {
		r.Reviewers = []RequestedReviewer{}
	}
	raw, _ := json.Marshal(r)
	return raw
}

func parseSidecar(raw []byte) (*PRSidecar, error) {
	var p PRSidecar
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: pr.json: %v", ErrCorrupt, err)
	}
	return &p, nil
}

// --- small helpers ----------------------------------------------------------

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

func intPtr(i int) *int { return &i }
