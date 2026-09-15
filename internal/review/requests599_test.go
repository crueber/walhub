package review

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Forgejo #599: review-request writes (POST/DELETE) are open-PR only.
// Closed or merged PRs refuse 422 (ErrUnprocessable) — uniformly, so
// self-removal is refused too (named decision: a terminal PR's request
// list is frozen curation, not an inbox the requestee still owns). GET
// stays allowed on terminal PRs; reopening a closed-but-unmerged PR
// restores writes; merged is terminal.

func mustUnprocessable(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("%s err = %v, want ErrUnprocessable", what, err)
	}
	if got := statusFor(err); got != 422 {
		t.Fatalf("%s status = %d, want 422", what, got)
	}
	if msg := err.Error(); !strings.Contains(msg, "only accepted on open") {
		t.Fatalf("%s error must name the open-PR rule, got %q", what, msg)
	}
}

func TestReviewRequestsBlockedOnClosedPR(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	var notified []NotifyEvent
	svc.Notify = func(_ context.Context, ev NotifyEvent) { notified = append(notified, ev) }

	// Open: add works, wire shape + notify class pinned (no change for
	// open PRs — the gate is a pure refusal on terminal state).
	reqs, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"})
	if err != nil {
		t.Fatalf("open add: %v", err)
	}
	if len(reqs.Reviewers) != 1 || reqs.Reviewers[0].Principal != "bob" || reqs.Reviewers[0].By != "alice" || reqs.Reviewers[0].At == "" {
		t.Fatalf("open wire shape: %+v", reqs)
	}
	if len(notified) != 1 || notified[0].Class != "review_requested" || notified[0].Actor != "alice" {
		t.Fatalf("open notify: %+v", notified)
	}

	// Closed: add refused 422, nothing emitted, nothing stored.
	setPRState(t, svc, "closed")
	if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"carol"}); err == nil {
		t.Fatal("closed add: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "closed add")
	}
	if len(notified) != 1 {
		t.Fatalf("closed add must not emit: %+v", notified)
	}
	got, err := svc.GetRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"))
	if err != nil || len(got.Reviewers) != 1 || got.Reviewers[0].Principal != "bob" {
		t.Fatalf("closed add must not store: %+v %v", got, err)
	}

	// Closed: author/triage removal refused 422 — and self-removal too
	// (uniform block, the #599 decision).
	if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); err == nil {
		t.Fatal("closed remove: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "closed remove")
	}
	if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), []string{"bob"}); err == nil {
		t.Fatal("closed self-remove: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "closed self-remove")
	}
	if len(notified) != 1 {
		t.Fatalf("closed removals must not emit: %+v", notified)
	}

	// GET stays allowed on the closed PR.
	got, err = svc.GetRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"))
	if err != nil || len(got.Reviewers) != 1 {
		t.Fatalf("closed GET: %+v %v", got, err)
	}

	// Reopen restores both writes (lock keys on current state).
	setPRState(t, svc, "open")
	if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"carol"}); err != nil {
		t.Fatalf("reopened add: %v", err)
	}
	reqs, err = svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), []string{"bob"})
	if err != nil || len(reqs.Reviewers) != 1 || reqs.Reviewers[0].Principal != "carol" {
		t.Fatalf("reopened self-remove: %+v %v", reqs, err)
	}
	if len(notified) != 3 || notified[1].Class != "review_requested" || notified[2].Class != "review_request_removed" {
		t.Fatalf("reopened notify classes: %+v", notified)
	}
}

func TestReviewRequestsBlockedOnMergedPR(t *testing.T) {
	ctx := context.Background()
	svc, _ := testSvc()
	seedPR(t, svc)
	if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); err != nil {
		t.Fatal(err)
	}

	// Merge stamps closed + merged (the terminal shape): both writes 422,
	// self-removal included; GET allowed.
	setPRState(t, svc, "closed")
	setPRMerged(t, svc, true)
	if _, err := svc.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"carol"}); err == nil {
		t.Fatal("merged add: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "merged add")
	}
	if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("bob"), []string{"bob"}); err == nil {
		t.Fatal("merged self-remove: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "merged self-remove")
	}
	if _, err := svc.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); err == nil {
		t.Fatal("merged remove: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "merged remove")
	}
	got, err := svc.GetRequests(ctx, testOwner, testRepo, testPR, testPrincipal("carol"))
	if err != nil || len(got.Reviewers) != 1 {
		t.Fatalf("merged GET: %+v %v", got, err)
	}

	// Stamp-in-flight shape (merged true, header still open) is refused
	// too — the sidecar arm, not just the state.
	svc2, _ := testSvc()
	seedPR(t, svc2)
	setPRMerged(t, svc2, true)
	if _, err := svc2.AddRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); err == nil {
		t.Fatal("in-flight add: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "in-flight add")
	}
	if _, err := svc2.RemoveRequests(ctx, testOwner, testRepo, testPR, testPrincipal("alice"), []string{"bob"}); err == nil {
		t.Fatal("in-flight remove: want 422, got nil")
	} else {
		mustUnprocessable(t, err, "in-flight remove")
	}
}
