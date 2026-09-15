package review

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// Forgejo #594: inline-thread writes (open/reply/submit) lock on merged or
// closed PRs (current PR state, not a one-way latch); reopening a
// closed-but-unmerged PR restores threading; merged is terminal.

func setPRState(t *testing.T, svc *Service, state string) {
	t.Helper()
	ctx := context.Background()
	raw, ver, err := svc.getJSON(ctx, ThreadKey(testOwner, testRepo, testPR))
	if err != nil || raw == nil {
		t.Fatalf("get header: %v", err)
	}
	h, err := parsePRHeader(raw)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	h.State = state
	h.Version++
	if _, err := store.PutBytes(ctx, svc.Store, ThreadKey(testOwner, testRepo, testPR), encodePRHeader(h),
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatalf("put header: %v", err)
	}
}

func setPRMerged(t *testing.T, svc *Service, merged bool) {
	t.Helper()
	ctx := context.Background()
	raw, ver, err := svc.getJSON(ctx, PRKey(testOwner, testRepo, testPR))
	if err != nil || raw == nil {
		t.Fatalf("get sidecar: %v", err)
	}
	side, err := parseSidecar(raw)
	if err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	side.Merged = merged
	enc, _ := json.Marshal(side)
	if _, err := store.PutBytes(ctx, svc.Store, PRKey(testOwner, testRepo, testPR), enc,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatalf("put sidecar: %v", err)
	}
}

func mustThreadLocked(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("%s err = %v, want ErrLocked", what, err)
	}
	if got := statusFor(err); got != 409 {
		t.Fatalf("%s status = %d, want 409", what, got)
	}
	if msg := err.Error(); len(msg) < 10 {
		t.Fatalf("%s error must carry a human-readable reason, got %q", what, msg)
	}
}

func TestThreadWritesLockedOnClosedPR(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)

	// Open → all three write paths pass.
	th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), testAnchor(), "first")
	if err != nil {
		t.Fatalf("open thread: %v", err)
	}
	if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), "reply"); err != nil {
		t.Fatalf("open reply: %v", err)
	}
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateCommented, testHead)); err != nil {
		t.Fatalf("open submit: %v", err)
	}

	// Closed → all three refuse 409.
	setPRState(t, svc, "closed")
	_, err = svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), testAnchor(), "late")
	mustThreadLocked(t, err, "closed open")
	_, err = svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), "late")
	mustThreadLocked(t, err, "closed reply")
	_, _, _, err = svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateCommented, testHead))
	mustThreadLocked(t, err, "closed submit")

	// Reopened → all three pass again (lock keys on current state).
	setPRState(t, svc, "open")
	if _, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), testAnchor(), "back"); err != nil {
		t.Fatalf("reopened thread: %v", err)
	}
	if _, err := svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), "back"); err != nil {
		t.Fatalf("reopened reply: %v", err)
	}
	if _, _, _, err := svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateCommented, testHead)); err != nil {
		t.Fatalf("reopened submit: %v", err)
	}
}

func TestThreadWritesLockedOnMergedPR(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	th, err := svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), testAnchor(), "first")
	if err != nil {
		t.Fatalf("open thread: %v", err)
	}

	// Merge stamps closed + merged (the terminal shape): permanent lock.
	setPRState(t, svc, "closed")
	setPRMerged(t, svc, true)
	_, err = svc.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), testAnchor(), "late")
	mustThreadLocked(t, err, "merged open")
	_, err = svc.AddThreadComment(ctx, testOwner, testRepo, testPR, th.TID, testPrincipal("carol"), "late")
	mustThreadLocked(t, err, "merged reply")
	_, _, _, err = svc.SubmitReview(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), submitAs(svc, "bob", StateCommented, testHead))
	mustThreadLocked(t, err, "merged submit")

	// Stamp-in-flight shape (merged true, header still open) is locked
	// too — the sidecar arm, not just the state.
	svc2, _ := testSvc()
	seedPR(t, svc2)
	setPRMerged(t, svc2, true)
	_, err = svc2.OpenThread(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), testAnchor(), "late")
	mustThreadLocked(t, err, "in-flight open")
}

func TestLockedErrorShape(t *testing.T) {
	if got := statusFor(ErrLocked); got != 409 {
		t.Fatalf("statusFor(ErrLocked) = %d, want 409", got)
	}
	if ErrLocked.Error() == "" {
		t.Fatal("ErrLocked must carry a human-readable message the SPA can toast")
	}
}
