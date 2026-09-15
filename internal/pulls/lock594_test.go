package pulls

import (
	"errors"
	"testing"
)

// Forgejo #594: conversation comments lock on merged/closed PRs (current
// thread state, not a one-way latch); reopening a closed-but-unmerged PR
// restores commenting; merged PRs are terminal (reopen refused 409).

func markMerged594(t *testing.T, e *testEnv, owner, repo string, num int) {
	t.Helper()
	pr, ver, err := e.svc.loadPR(ctx(), owner, repo, num)
	if err != nil || pr == nil {
		t.Fatalf("loadPR: %v", err)
	}
	pr.Merged = true
	if err := e.svc.savePR(ctx(), owner, repo, pr, ver); err != nil {
		t.Fatalf("savePR: %v", err)
	}
}

func mustCommentLocked(t *testing.T, e *testEnv, num int) {
	t.Helper()
	_, err := e.svc.AddComment(ctx(), "o", "r", num, writer(), "late")
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("locked comment err = %v, want ErrLocked", err)
	}
	if got := statusFor(err); got != 409 {
		t.Fatalf("locked comment status = %d, want 409", got)
	}
	if msg := err.Error(); len(msg) < 10 {
		t.Fatalf("locked error must carry a human-readable reason, got %q", msg)
	}
}

func TestCommentLockedOnClosedPR(t *testing.T) {
	e := newTestEnv()
	th, _ := openBasic(t, e, "o", "r")

	// Open → passes.
	if _, err := e.svc.AddComment(ctx(), "o", "r", th.Num, writer(), "first"); err != nil {
		t.Fatalf("open comment: %v", err)
	}
	// Closed → 409.
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("close: %v", err)
	}
	mustCommentLocked(t, e, th.Num)
	// Reopened (closed-but-unmerged may reopen) → passes again.
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: strPtr("open")}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := e.svc.AddComment(ctx(), "o", "r", th.Num, writer(), "back"); err != nil {
		t.Fatalf("reopened comment: %v", err)
	}
	cur, _, _ := e.svc.loadThread(ctx(), "o", "r", th.Num)
	if cur.CommentCount != 2 {
		t.Fatalf("comment_count = %d, want 2 (locked write added nothing)", cur.CommentCount)
	}
}

func TestCommentLockedOnMergedPR(t *testing.T) {
	e := newTestEnv()
	th, _ := openBasic(t, e, "o", "r")

	// Merge stamps closed + merged (the terminal shape).
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("close: %v", err)
	}
	markMerged594(t, e, "o", "r", th.Num)
	mustCommentLocked(t, e, th.Num)

	// Merged with the header stamp still in flight (state open, merged
	// true) is locked too — the sidecar arm, not just the state.
	e2 := newTestEnv()
	th2, _ := openBasic(t, e2, "o", "r")
	markMerged594(t, e2, "o", "r", th2.Num)
	mustCommentLocked(t, e2, th2.Num)
}

func TestReopenMergedRefused(t *testing.T) {
	e := newTestEnv()
	th, _ := openBasic(t, e, "o", "r")
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("close: %v", err)
	}
	markMerged594(t, e, "o", "r", th.Num)

	// state:"open" on a merged PR → 409 (today only closing is refused).
	_, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: strPtr("open")})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("reopen-on-merged err = %v, want ErrConflict", err)
	}
	if got := statusFor(err); got != 409 {
		t.Fatalf("reopen-on-merged status = %d, want 409", got)
	}
	// The lock is permanent: commenting stays refused.
	mustCommentLocked(t, e, th.Num)
}

func TestLockedErrorShape(t *testing.T) {
	if got := statusFor(ErrLocked); got != 409 {
		t.Fatalf("statusFor(ErrLocked) = %d, want 409", got)
	}
	if ErrLocked.Error() == "" {
		t.Fatal("ErrLocked must carry a human-readable message the SPA can toast")
	}
}
