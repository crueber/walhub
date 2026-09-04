package issues

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func TestCreateIssue(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	cases := []struct {
		name    string
		actor   auth.Principal
		title   string
		body    string
		wantErr error
	}{
		{"basic", janeP, "First bug", "details here", nil},
		{"empty body ok", janeP, "No body", "", nil},
		{"empty title", janeP, "   ", "", ErrInvalid},
		{"title too long", janeP, strings.Repeat("x", MaxTitleLen+1), "", ErrInvalid},
		{"body too long", janeP, "t", strings.Repeat("x", MaxBodyBytes+1), ErrInvalid},
		{"anon denied", anonP, "t", "", ErrUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th, ev, err := s.CreateIssue(reqCtx(), "acme", "repo", c.actor, c.title, c.body)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if th.Kind != "issue" || th.State != StateOpen || ev.Type != EventOpened || ev.Seq != 0 {
				t.Fatalf("bad create: %+v %+v", th, ev)
			}
			if th.Num < 1 || th.NextEventSeq != 1 || th.CommentCount != 0 {
				t.Fatalf("bad counters: %+v", th)
			}
		})
	}
	// Numbering is dense: the two successful creates took 1 and 2.
	th := mustCreate(t, s, "acme", "repo", janeP, "third", "")
	if th.Num != 3 {
		t.Fatalf("num = %d, want 3", th.Num)
	}
	// Private repo: stranger denied, member allowed.
	roles.private["acme/priv"] = true
	if _, _, err := s.CreateIssue(reqCtx(), "acme", "priv", bobP, "x", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("private stranger err = %v", err)
	}
	roles.grant("acme", "priv", "bob@example.com", "read")
	mustCreate(t, s, "acme", "priv", bobP, "member post", "")
}

func TestComment(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")
	cases := []struct {
		name    string
		actor   auth.Principal
		body    string
		wantErr error
	}{
		{"comment", bobP, "repro steps", nil},
		{"empty", bobP, "", ErrInvalid},
		{"anon", anonP, "hi", ErrUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, c.actor, c.body)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type != EventCommented {
				t.Fatalf("type = %q", ev.Type)
			}
		})
	}
	cur, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
	if cur.CommentCount != 1 {
		t.Fatalf("comment_count = %d, want 1", cur.CommentCount)
	}
	if len(cur.Participants) != 2 { // jane + bob
		t.Fatalf("participants = %v", cur.Participants)
	}
	if _, err := s.AddComment(reqCtx(), "acme", "repo", 999, bobP, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost comment err = %v", err)
	}
}

func TestCommentRefsFanout(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	// #N at write time writes a referenced event on the TARGET.
	if _, err := s.AddComment(reqCtx(), "acme", "repo", a.Num, bobP, "dupe of #"+itoa(b.Num)); err != nil {
		t.Fatal(err)
	}
	events, err := s.scanEvents(reqCtx(), "acme", "repo", b.Num)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == EventReferenced && e.Actor == "bob@example.com" {
			found = true
			if e.Source["num"] != float64(a.Num) && e.Source["num"] != a.Num {
				t.Fatalf("source = %v", e.Source)
			}
		}
	}
	if !found {
		t.Fatalf("no referenced event on target; events=%+v", events)
	}
	// Missing target is silently skipped (comment still commits).
	ev, err := s.AddComment(reqCtx(), "acme", "repo", a.Num, bobP, "see #424242")
	if err != nil || ev == nil {
		t.Fatalf("missing-target comment err=%v ev=%v", err, ev)
	}
	// Cross-repo ref to a missing repo is skipped silently.
	if _, err := s.AddComment(reqCtx(), "acme", "repo", a.Num, bobP, "see acme/other#1"); err != nil {
		t.Fatalf("cross-repo comment err = %v", err)
	}
	mustCreate(t, s, "acme", "other", janeP, "other issue", "")
	// Cross-repo ref without read on a PRIVATE target is skipped silently.
	roles.private["acme/other"] = true
	if _, err := s.AddComment(reqCtx(), "acme", "repo", a.Num, bobP, "see acme/other#1"); err != nil {
		t.Fatalf("denied cross-repo comment err = %v", err)
	}
	otevents, err := s.scanEvents(reqCtx(), "acme", "other", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range otevents {
		if e.Type == EventCrossReferenced {
			t.Fatalf("denied ref wrote an event: %+v", e)
		}
	}
	// Cross-repo ref WITH read writes cross_referenced on the target.
	roles.grant("acme", "other", "bob@example.com", "read")
	if _, err := s.AddComment(reqCtx(), "acme", "repo", a.Num, bobP, "see acme/other#1"); err != nil {
		t.Fatal(err)
	}
	otevents, err = s.scanEvents(reqCtx(), "acme", "other", 1)
	if err != nil {
		t.Fatal(err)
	}
	xfound := false
	for _, e := range otevents {
		if e.Type == EventCrossReferenced {
			xfound = true
		}
	}
	if !xfound {
		t.Fatalf("no cross_referenced event; events=%+v", otevents)
	}
}

func TestPatchIssue(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	roles.grant("acme", "repo", "bob@example.com", "read")
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "d73a4a", ""); err != nil {
		t.Fatal(err)
	}
	roles.grant("acme", "repo", "carol@example.com", "write")
	carol := auth.Principal{Name: "carol@example.com"}

	t.Run("author retitles", func(t *testing.T) {
		nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{Title: strPtr("bug fix")})
		if err != nil {
			t.Fatal(err)
		}
		if nt.Title != "bug fix" {
			t.Fatalf("title = %q", nt.Title)
		}
	})
	t.Run("stranger title denied", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, bobP, IssuePatch{Title: strPtr("hijack")}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("triage labels", func(t *testing.T) {
		nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Labels: &[]string{"bug"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(nt.Labels) != 1 || nt.Labels[0] != "bug" {
			t.Fatalf("labels = %v", nt.Labels)
		}
	})
	t.Run("unknown label", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Labels: &[]string{"nope"}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-triage labels denied", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, bobP, IssuePatch{Labels: &[]string{}}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("assign needs triage target", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Assignees: &[]string{"bob@example.com"}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("below-triage assignee err = %v", err)
		}
		nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Assignees: &[]string{"carol@example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(nt.Assignees) != 1 {
			t.Fatalf("assignees = %v", nt.Assignees)
		}
	})
	t.Run("close and reopen", func(t *testing.T) {
		nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{State: strPtr("closed"), StateReason: strPtr("not_planned")})
		if err != nil {
			t.Fatal(err)
		}
		if nt.State != StateClosed || nt.StateReason == nil || *nt.StateReason != ReasonNotPlanned {
			t.Fatalf("closed = %+v", nt)
		}
		nt, err = s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{State: strPtr("open")})
		if err != nil {
			t.Fatal(err)
		}
		if nt.State != StateOpen || nt.StateReason != nil {
			t.Fatalf("reopened = %+v", nt)
		}
	})
	t.Run("bad state", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{State: strPtr("frozen")}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad reason", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{State: strPtr("closed"), StateReason: strPtr("later")}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown issue", func(t *testing.T) {
		if _, err := s.PatchIssue(reqCtx(), "acme", "repo", 4242, aliceP, IssuePatch{Title: strPtr("x")}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no-op patch writes nothing", func(t *testing.T) {
		before, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
		nt, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{Title: strPtr(before.Title)})
		if err != nil {
			t.Fatal(err)
		}
		if nt.NextEventSeq != before.NextEventSeq {
			t.Fatalf("no-op consumed seq %d → %d", before.NextEventSeq, nt.NextEventSeq)
		}
		_ = carol
	})
}

func TestPatchMilestoneMove(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	m1, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v2", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "")
	id1 := m1.ID
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: ptrTo(&id1)}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.loadMilestone(reqCtx(), "acme", "repo", m1.ID)
	if got.OpenIssues != 1 {
		t.Fatalf("m1 open = %d", got.OpenIssues)
	}
	id2 := m2.ID
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: ptrTo(&id2)}); err != nil {
		t.Fatal(err)
	}
	got1, _, _ := s.loadMilestone(reqCtx(), "acme", "repo", m1.ID)
	got2, _, _ := s.loadMilestone(reqCtx(), "acme", "repo", m2.ID)
	if got1.OpenIssues != 0 || got2.OpenIssues != 1 {
		t.Fatalf("move: m1=%+v m2=%+v", got1, got2)
	}
	// Close moves open→closed on the milestone.
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{State: strPtr("closed")}); err != nil {
		t.Fatal(err)
	}
	got2, _, _ = s.loadMilestone(reqCtx(), "acme", "repo", m2.ID)
	if got2.OpenIssues != 0 || got2.ClosedIssues != 1 || milestonePercent(0, 1) != 100 {
		t.Fatalf("close counters: %+v", got2)
	}
	// Clear.
	var nilID *string
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: &nilID}); err != nil {
		t.Fatal(err)
	}
	got2, _, _ = s.loadMilestone(reqCtx(), "acme", "repo", m2.ID)
	if got2.ClosedIssues != 0 {
		t.Fatalf("clear counters: %+v", got2)
	}
	// Unknown milestone id.
	bad := "0000ff"
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: ptrTo(&bad)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown milestone err = %v", err)
	}
}

func TestReactions(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "body")
	// React to the opened event (seq 0).
	nt, added, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1")
	if err != nil || !added {
		t.Fatalf("add = %v,%v", nt, err)
	}
	if nt.ReactionSummary[seqKey(0)]["+1"] != 1 {
		t.Fatalf("summary = %v", nt.ReactionSummary)
	}
	// Duplicate add: no-op, same summary.
	nt2, added, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1")
	if err != nil || added {
		t.Fatalf("dup = %v,%v", added, err)
	}
	if nt2.ReactionSummary[seqKey(0)]["+1"] != 1 {
		t.Fatalf("dup summary = %v", nt2.ReactionSummary)
	}
	// Unknown content.
	if _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "party"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad content err = %v", err)
	}
	// Unknown target event.
	if _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 99, bobP, "+1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad target err = %v", err)
	}
	// Non-comment target (title_changed) rejected: make one first.
	nt3, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{Title: strPtr("bug!")})
	if err != nil {
		t.Fatal(err)
	}
	_ = nt3
	events, _ := s.scanEvents(reqCtx(), "acme", "repo", th.Num)
	titleSeq := -1
	for _, e := range events {
		if e.Type == EventTitleChanged {
			titleSeq = e.Seq
		}
	}
	if _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, titleSeq, bobP, "+1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-comment target err = %v", err)
	}
	// Remove own reaction.
	rt, err := s.RemoveReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.ReactionSummary[seqKey(0)]) != 0 {
		t.Fatalf("after remove: %v", rt.ReactionSummary)
	}
	// Remove again → 404; remove another's → 404.
	if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "+1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double remove err = %v", err)
	}
	if _, added, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, aliceP, "heart"); err != nil || !added {
		t.Fatal(err)
	}
	if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", th.Num, 0, bobP, "heart"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("steal remove err = %v", err)
	}
	// Anon denied both ways.
	if _, _, err := s.AddReaction(reqCtx(), "acme", "repo", th.Num, 0, anonP, "+1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon add err = %v", err)
	}
	if _, err := s.RemoveReaction(reqCtx(), "acme", "repo", th.Num, 0, anonP, "+1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon remove err = %v", err)
	}
}

func TestLabels(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	// Create validation.
	for _, c := range []struct {
		name, color, desc string
		wantErr           error
	}{
		{"bug", "d73a4a", "", nil},
		{"BUG", "ffffff", "", ErrConflict}, // case-insensitive dup
		{"", "ffffff", "", ErrInvalid},
		{strings.Repeat("x", 65), "ffffff", "", ErrInvalid},
		{"ok", "red", "", ErrInvalid},
		{"ok", "#d73a4a", "", ErrInvalid},
		{"ok", "ffffff", strings.Repeat("x", 201), ErrInvalid},
	} {
		t.Run("create "+c.name+"/"+c.color, func(t *testing.T) {
			_, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, c.name, c.color, c.desc)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	// Non-triage denied.
	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", bobP, "x", "ffffff", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob create err = %v", err)
	}
	// Update.
	upd, err := s.UpdateLabel(reqCtx(), "acme", "repo", aliceP, "BUG", strPtr("000000"), strPtr("broken"))
	if err != nil {
		t.Fatal(err)
	}
	if upd.Color != "000000" || upd.Name != "bug" {
		t.Fatalf("updated = %+v", upd)
	}
	if _, err := s.UpdateLabel(reqCtx(), "acme", "repo", aliceP, "ghost", strPtr("ffffff"), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost update err = %v", err)
	}
	if _, err := s.UpdateLabel(reqCtx(), "acme", "repo", aliceP, "bug", strPtr("zzz"), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad color err = %v", err)
	}
	// Delete with compensating events on open threads.
	th := mustCreate(t, s, "acme", "repo", janeP, "buggy", "")
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Labels: &[]string{"bug"}}); err != nil {
		t.Fatal(err)
	}
	affected, err := s.DeleteLabel(reqCtx(), "acme", "repo", aliceP, "bug")
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	cur, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
	if len(cur.Labels) != 0 {
		t.Fatalf("labels after delete = %v", cur.Labels)
	}
	if _, err := s.DeleteLabel(reqCtx(), "acme", "repo", aliceP, "bug"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v", err)
	}
}

func TestMilestones(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1.1", "next", strPtr("2026-10-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "000001" || m.State != StateOpen || m.Percent != 0 {
		t.Fatalf("milestone = %+v", m)
	}
	if _, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "", "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title err = %v", err)
	}
	if _, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "x", "", strPtr("tomorrow")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad due err = %v", err)
	}
	if _, err := s.CreateMilestone(reqCtx(), "acme", "repo", bobP, "x", "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob create err = %v", err)
	}
	// Update.
	um, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, strPtr("v1.2"), nil, strPtr(""), strPtr("closed"))
	if err != nil {
		t.Fatal(err)
	}
	if um.Title != "v1.2" || um.DueOn != nil || um.State != StateClosed {
		t.Fatalf("updated = %+v", um)
	}
	if _, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, "0000ff", strPtr("x"), nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost update err = %v", err)
	}
	if _, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, nil, nil, nil, strPtr("shipped")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad state err = %v", err)
	}
	// Delete blocked while open issues reference it.
	th := mustCreate(t, s, "acme", "repo", janeP, "m", "")
	id := m.ID
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Milestone: ptrTo(&id)}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete-blocked err = %v", err)
	}
	// Close the issue, then delete clears the milestone from the thread.
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{State: strPtr("closed")}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := s.loadThread(reqCtx(), "acme", "repo", th.Num)
	if cur.Milestone != nil {
		t.Fatalf("milestone after delete = %v", cur.Milestone)
	}
	if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v", err)
	}
	// Derived progress.
	m2, _ := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v2", "", nil)
	list, err := s.listMilestones(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != m2.ID {
		t.Fatalf("list = %+v", list)
	}
}

func TestApplyClosingReferences(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	closed, err := s.ApplyClosingReferences(reqCtx(), "acme", "repo", 12, "abc123", "merge-queue", []string{"PR body fixes #" + itoa(a.Num), "commit closes #" + itoa(b.Num)})
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 2 {
		t.Fatalf("closed = %v", closed)
	}
	for _, num := range []int{a.Num, b.Num} {
		th, _, _ := s.loadThread(reqCtx(), "acme", "repo", num)
		if th.State != StateClosed || th.StateReason == nil || *th.StateReason != ReasonCompleted {
			t.Fatalf("issue %d = %+v", num, th)
		}
		events, _ := s.scanEvents(reqCtx(), "acme", "repo", num)
		types := map[string]bool{}
		for _, e := range events {
			types[e.Type] = true
		}
		if !types[EventReferenced] || !types[EventClosedByPR] {
			t.Fatalf("issue %d events = %+v", num, events)
		}
	}
	// Already closed: skipped, empty close set.
	closed, err = s.ApplyClosingReferences(reqCtx(), "acme", "repo", 13, "def456", "", []string{"fixes #" + itoa(a.Num), "see #" + itoa(b.Num)})
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 0 {
		t.Fatalf("re-close = %v", closed)
	}
	// Unknown num: skipped silently.
	closed, err = s.ApplyClosingReferences(reqCtx(), "acme", "repo", 14, "x", "", []string{"fixes #424242"})
	if err != nil || len(closed) != 0 {
		t.Fatalf("ghost = %v %v", closed, err)
	}
}

func TestNotifyEmission(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	roles.grant("acme", "repo", "carol@example.com", "write")
	s := testService(roles)
	var got []NotifyEvent
	s.Notify = func(ctx context.Context, ev NotifyEvent) { got = append(got, ev) }
	// Mention in the opened body → mentioned for carol.
	th := mustCreate(t, s, "acme", "repo", janeP, "bug", "ping @carol@example.com")
	// Comment by bob → subscribed for jane, mentioned for carol.
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "cc @carol@example.com"); err != nil {
		t.Fatal(err)
	}
	// Assign carol → assigned.
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, aliceP, IssuePatch{Assignees: &[]string{"carol@example.com"}}); err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	for _, e := range got {
		classes[e.Class] = append(classes[e.Class], e.Recipients...)
		if e.Repo != "acme/repo" || e.Actor == "" || e.At == "" {
			t.Fatalf("bad event envelope: %+v", e)
		}
	}
	contains := func(ss []string, want string) bool {
		for _, v := range ss {
			if v == want {
				return true
			}
		}
		return false
	}
	if !contains(classes["mentioned"], "carol@example.com") {
		t.Fatalf("no mentioned for carol: %v", classes)
	}
	if !contains(classes["subscribed"], "jane@example.com") {
		t.Fatalf("no subscribed for jane: %v", classes)
	}
	if !contains(classes["assigned"], "carol@example.com") {
		t.Fatalf("no assigned for carol: %v", classes)
	}
	// Nil emitter never panics.
	s.Notify = nil
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "again"); err != nil {
		t.Fatal(err)
	}
}

func ptrTo(s *string) **string { return &s }
