package issues

import (
	"errors"
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Forgejo #594: commenting locks on closed issues (current thread state,
// not a one-way latch) and reopens restore it. Reaction removal stays
// available (personal undo, not conversation activity).

func mustClose(t *testing.T, s *Service, num int, actor auth.Principal) {
	t.Helper()
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", num, actor, IssuePatch{State: strPtr("closed")}); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func mustReopen(t *testing.T, s *Service, num int, actor auth.Principal) {
	t.Helper()
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", num, actor, IssuePatch{State: strPtr("open")}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestCommentLockedOnClosed(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")

	// Open → passes.
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "first"); err != nil {
		t.Fatalf("open comment: %v", err)
	}
	// Closed → 409 with a human-readable reason.
	mustClose(t, s, th.Num, janeP)
	mustLockErr(t, s, th.Num)
	// Reopened → passes again (lock keys on current state).
	mustReopen(t, s, th.Num, janeP)
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "back"); err != nil {
		t.Fatalf("reopened comment: %v", err)
	}
	cur, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
	if cur.CommentCount != 2 {
		t.Fatalf("comment_count = %d, want 2 (locked write added nothing)", cur.CommentCount)
	}
}

func mustLockErr(t *testing.T, s *Service, num int) error {
	t.Helper()
	_, err := s.AddComment(reqCtx(), "acme", "repo", num, bobP, "late")
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("closed comment err = %v, want ErrLocked", err)
	}
	if got := statusFor(err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	if msg := err.Error(); msg == "" || len(msg) < 10 {
		t.Fatalf("locked error must carry a human-readable reason, got %q", msg)
	}
	return err
}

func TestReactionAddLockedRemoveAllowed(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")

	// Seed one reaction pre-lock (target seq 0 = opened event).
	if _, _, added, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1"); err != nil || !added {
		t.Fatalf("seed add: %v added=%v", err, added)
	}
	mustClose(t, s, th.Num, janeP)

	// Add on locked → 409.
	if _, _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "heart"); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked add err = %v, want ErrLocked", err)
	} else if got := statusFor(err); got != http.StatusConflict {
		t.Fatalf("locked add status = %d, want 409", got)
	}
	// Remove own pre-lock reaction → still allowed (personal undo).
	if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1"); err != nil {
		t.Fatalf("locked remove: %v", err)
	}
	// Reopen → add passes again.
	mustReopen(t, s, th.Num, janeP)
	if _, _, added, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "heart"); err != nil || !added {
		t.Fatalf("reopened add: %v added=%v", err, added)
	}
}

func TestLockedErrorShape(t *testing.T) {
	if got := statusFor(ErrLocked); got != http.StatusConflict {
		t.Fatalf("statusFor(ErrLocked) = %d, want 409", got)
	}
	if ErrLocked.Error() == "" {
		t.Fatal("ErrLocked must carry a human-readable message the SPA can toast")
	}
}
