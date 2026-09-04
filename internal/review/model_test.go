package review

import (
	"strings"
	"testing"
)

func TestValidateState(t *testing.T) {
	for _, tc := range []struct {
		state string
		ok    bool
	}{
		{"APPROVED", true},
		{"CHANGES_REQUESTED", true},
		{"COMMENTED", true},
		{"", false},
		{"approved", false},
		{"DISMISSED", false},
		{"COMMENT", false},
	} {
		err := validateState(tc.state)
		if (err == nil) != tc.ok {
			t.Errorf("validateState(%q) err=%v, want ok=%v", tc.state, err, tc.ok)
		}
	}
}

func TestValidateSHA(t *testing.T) {
	good40 := strings.Repeat("a", 40)
	good64 := strings.Repeat("b", 64)
	for _, tc := range []struct {
		sha string
		ok  bool
	}{
		{good40, true},
		{good64, true},
		{"", false},
		{strings.Repeat("a", 39), false},
		{strings.Repeat("a", 41), false},
		{strings.Repeat("A", 40), false},
		{strings.Repeat("g", 40), false},
		{"not a sha at all........................", false},
	} {
		if err := validateSHA(tc.sha); (err == nil) != tc.ok {
			t.Errorf("validateSHA(%q) err=%v, want ok=%v", tc.sha, err, tc.ok)
		}
	}
}

func TestValidateAnchor(t *testing.T) {
	good := testAnchor()
	if err := validateAnchor(good); err != nil {
		t.Fatalf("good anchor: %v", err)
	}
	old := testAnchor()
	old.Side = SideOld
	old.OldStart, old.OldLines = 10, 2
	old.NewStart, old.NewLines = 0, 0
	if err := validateAnchor(old); err != nil {
		t.Fatalf("good OLD anchor: %v", err)
	}
	for name, mutate := range map[string]func(*Anchor){
		"empty path":     func(a *Anchor) { a.Path = "" },
		"newline path":   func(a *Anchor) { a.Path = "a\nb" },
		"bad side":       func(a *Anchor) { a.Side = "LEFT" },
		"NEW zero lines": func(a *Anchor) { a.NewLines = 0 },
		"NEW zero start": func(a *Anchor) { a.NewStart = 0 },
		"OLD zero range": func(a *Anchor) { a.Side = SideOld; a.OldLines = 0 },
		"bad commit":     func(a *Anchor) { a.CommitSHA = "xyz" },
		"bad context":    func(a *Anchor) { a.ContextSHA = "abc" },
		"upper context":  func(a *Anchor) { a.ContextSHA = strings.ToUpper(a.ContextSHA) },
		"empty context":  func(a *Anchor) { a.ContextSHA = "" },
	} {
		a := testAnchor()
		mutate(&a)
		if err := validateAnchor(a); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateTID(t *testing.T) {
	for _, tc := range []struct {
		tid string
		ok  bool
	}{
		{"00000001", true},
		{"0000002a", true},
		{"ffffffff", true},
		{"", false},
		{"1", false},
		{"0000000G", false},
		{"0000000A", false},
		{"000000010", false},
	} {
		if err := validateTID(tc.tid); (err == nil) != tc.ok {
			t.Errorf("validateTID(%q) err=%v, want ok=%v", tc.tid, err, tc.ok)
		}
	}
}

func TestDriftHash(t *testing.T) {
	// Normative shape: SHA-256 hex over path+"\n"+before+after,
	// trailing-whitespace-trimmed, LF-joined, ≤3 lines each side.
	h := DriftHash("src/main.go", []string{"a  ", "b"}, []string{"c"})
	if len(h) != 64 {
		t.Fatalf("hash len %d", len(h))
	}
	// Trailing whitespace is insignificant.
	if h2 := DriftHash("src/main.go", []string{"a", "b"}, []string{"c"}); h2 != h {
		t.Fatalf("trailing whitespace changed the hash: %s vs %s", h, h2)
	}
	// Leading whitespace IS significant.
	if h2 := DriftHash("src/main.go", []string{"  a", "b"}, []string{"c"}); h2 == h {
		t.Fatalf("leading whitespace ignored")
	}
	// Path matters.
	if h2 := DriftHash("src/other.go", []string{"a", "b"}, []string{"c"}); h2 == h {
		t.Fatalf("path ignored")
	}
	// Only the nearest 3 context lines count.
	many := []string{"1", "2", "3", "4", "5"}
	if got, want := DriftHash("p", many, nil), DriftHash("p", []string{"3", "4", "5"}, nil); got != want {
		t.Fatalf("before window wrong: %s vs %s", got, want)
	}
	if got, want := DriftHash("p", nil, many), DriftHash("p", nil, []string{"1", "2", "3"}); got != want {
		t.Fatalf("after window wrong: %s vs %s", got, want)
	}
	// Empty context is a valid hash (whole-file anchors).
	if h := DriftHash("p", nil, nil); len(h) != 64 {
		t.Fatalf("empty context hash len %d", len(h))
	}
}

func TestDriftHashVector(t *testing.T) {
	// Fixed cross-implementation vector: web/src/lib/diff.js
	// anchorContextSha MUST produce the same hex for the same inputs
	// (the §8 dogfood rule — UI + server share semantics). Both suites
	// pin this value; changing the hash is a wire-level break.
	if got, want := DriftHash("src/main.go", []string{"a", "b"}, []string{"c"}),
		"89e40705caa54ab6ad1deb39b0de14005dad1361f2289531ed05af1a065f7610"; got != want {
		t.Fatalf("vector: got %s want %s", got, want)
	}
}

func TestRollup(t *testing.T) {
	mk := func(seq int, by, state, sha string) *ReviewEvent {
		return &ReviewEvent{Kind: KindReview, Seq: seq, At: "2026-09-04T12:00:00Z", By: by, State: state, CommitSHA: sha}
	}
	dm := func(seq int, by string, target int) *ReviewEvent {
		return &ReviewEvent{Kind: KindReviewDismissed, Seq: seq, At: "2026-09-04T12:00:00Z", By: by, Dismisses: intPtr(target)}
	}
	head := strings.Repeat("a", 40)
	old := strings.Repeat("b", 40)

	t.Run("empty is REVIEW_REQUIRED", func(t *testing.T) {
		sum := Rollup(nil, nil, 0, 0)
		if sum.Decision != DecisionReviewRequired || sum.Approvals != 0 {
			t.Fatalf("%+v", sum)
		}
		if sum.Latest == nil || sum.Requested == nil {
			t.Fatalf("nil maps/slices: %+v", sum)
		}
	})

	t.Run("one approval", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{mk(1, "bob", StateApproved, head)}, []string{"carol"}, 2, 1)
		if sum.Decision != DecisionApproved || sum.Approvals != 1 {
			t.Fatalf("%+v", sum)
		}
		if len(sum.Requested) != 1 || sum.ThreadsTotal != 2 || sum.ThreadsUnresolved != 1 {
			t.Fatalf("%+v", sum)
		}
	})

	t.Run("changes requested dominates", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{
			mk(1, "bob", StateApproved, head),
			mk(2, "carol", StateChangesRequested, head),
		}, nil, 0, 0)
		if sum.Decision != DecisionChangesRequested {
			t.Fatalf("%+v", sum)
		}
		if sum.Approvals != 1 {
			t.Fatalf("approvals still counted: %+v", sum)
		}
	})

	t.Run("latest wins per reviewer", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{
			mk(1, "bob", StateChangesRequested, head),
			mk(2, "bob", StateApproved, head),
		}, nil, 0, 0)
		if sum.Decision != DecisionApproved || sum.Approvals != 1 {
			t.Fatalf("%+v", sum)
		}
		if sum.Latest["bob"].Seq != 2 {
			t.Fatalf("%+v", sum.Latest["bob"])
		}
	})

	t.Run("commented alone stays required", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{mk(1, "bob", StateCommented, head)}, nil, 0, 0)
		if sum.Decision != DecisionReviewRequired || sum.Approvals != 0 {
			t.Fatalf("%+v", sum)
		}
	})

	t.Run("dismissal demotes while targeted is latest", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{mk(1, "bob", StateApproved, head), dm(2, "alice", 1)}, nil, 0, 0)
		if sum.Decision != DecisionReviewRequired || sum.Approvals != 0 {
			t.Fatalf("%+v", sum)
		}
		if sum.Latest["bob"].State != StateDismissed || sum.Latest["bob"].Seq != 1 {
			t.Fatalf("%+v", sum.Latest["bob"])
		}
	})

	t.Run("dismissal of history is a no-op", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{
			mk(1, "bob", StateApproved, old),
			mk(2, "bob", StateChangesRequested, head),
			dm(3, "alice", 1),
		}, nil, 0, 0)
		if sum.Decision != DecisionChangesRequested {
			t.Fatalf("%+v", sum)
		}
	})

	t.Run("dismissal of unknown seq ignored", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{mk(1, "bob", StateApproved, head), dm(2, "alice", 99)}, nil, 0, 0)
		if sum.Decision != DecisionApproved {
			t.Fatalf("%+v", sum)
		}
	})

	t.Run("dismissal without target ignored", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{
			mk(1, "bob", StateApproved, head),
			{Kind: KindReviewDismissed, Seq: 2, By: "alice"},
		}, nil, 0, 0)
		if sum.Decision != DecisionApproved {
			t.Fatalf("%+v", sum)
		}
	})

	t.Run("out-of-order input sorted by seq", func(t *testing.T) {
		sum := Rollup([]*ReviewEvent{
			mk(2, "bob", StateApproved, head),
			mk(1, "bob", StateChangesRequested, head),
		}, nil, 0, 0)
		if sum.Decision != DecisionApproved {
			t.Fatalf("%+v", sum)
		}
	})
}

func TestStatusFor(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{ErrNotFound, 404},
		{ErrInvalid, 400},
		{ErrUnauthorized, 401},
		{ErrForbidden, 403},
		{ErrConflict, 409},
		{ErrUnprocessable, 422},
		{ErrUnavailable, 503},
		{ErrCorrupt, 500},
	} {
		if got := statusFor(tc.err); got != tc.want {
			t.Errorf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestCodecRoundTrips(t *testing.T) {
	h := &PRHeader{Num: 7, Kind: "pr", Title: "t", State: "open", Author: "alice",
		NextReviewSeq: 3, NextThreadNum: 2,
		ReviewSummary: Rollup(nil, []string{"x"}, 1, 1)}
	raw := encodePRHeader(h)
	back, err := parsePRHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.NextReviewSeq != 3 || back.ReviewSummary.Decision != DecisionReviewRequired {
		t.Fatalf("%+v", back)
	}
	// Corrupt bytes fail closed.
	for name, fn := range map[string]func() error{
		"header":   func() error { _, err := parsePRHeader([]byte("{")); return err },
		"review":   func() error { _, err := parseReview([]byte("{")); return err },
		"thread":   func() error { _, err := parseThreadHeader([]byte("{")); return err },
		"comment":  func() error { _, err := parseThreadComment([]byte("{")); return err },
		"requests": func() error { _, err := parseReviewRequests([]byte("{")); return err },
		"sidecar":  func() error { _, err := parseSidecar([]byte("{")); return err },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: expected corrupt error", name)
		}
	}
}
