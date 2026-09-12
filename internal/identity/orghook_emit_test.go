// orghook_emit_test.go — Forgejo #363: the identity OrgEvent observer
// fires exactly once per membership/team/invite transition, after the
// CAS commits, with the documented action spellings (which must match
// notify's orgActions — pinned here as literals, same as the wire).
package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

type orgEvent struct {
	org, action, actor, title string
}

type orgEventLog struct {
	events []orgEvent
}

func (l *orgEventLog) sink(ctx context.Context, org, action, actor, title string) {
	l.events = append(l.events, orgEvent{org, action, actor, title})
}

func (l *orgEventLog) actions() []string {
	out := []string{}
	for _, e := range l.events {
		out = append(out, e.action)
	}
	return out
}

func emitService() (*Service, *orgEventLog) {
	s := testService()
	l := &orgEventLog{}
	s.OrgEvent = l.sink
	return s, l
}

func mustEmitOrg(t *testing.T, s *Service, ctx context.Context) {
	t.Helper()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOrgEventMemberLifecycle(t *testing.T) {
	s, l := emitService()
	ctx := context.Background()
	mustEmitOrg(t, s, ctx)
	// CreateOrg itself is not a membership transition: silence.
	if len(l.events) != 0 {
		t.Fatalf("CreateOrg emitted %+v", l.events)
	}
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgOwner); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-set of the same role emits nothing.
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveMember(ctx, "acme", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	// Removing a non-member is a no-op write with no event.
	if _, err := s.RemoveMember(ctx, "acme", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	want := []string{"member_added", "member_role_changed", "member_removed"}
	if got := l.actions(); !equalStrings(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	for _, e := range l.events {
		if e.org != "acme" || e.title == "" {
			t.Fatalf("event = %+v", e)
		}
	}
}

func TestOrgEventTeamLifecycle(t *testing.T) {
	s, l := emitService()
	ctx := context.Background()
	mustEmitOrg(t, s, ctx)
	if _, err := s.CreateTeam(ctx, "acme", "devs", "Devs", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "devs", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-add emits nothing.
	if _, err := s.SetTeamMember(ctx, "acme", "devs", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveTeamMember(ctx, "acme", "devs", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	// Removing a non-member emits nothing.
	if _, err := s.RemoveTeamMember(ctx, "acme", "devs", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeam(ctx, "acme", "devs"); err != nil {
		t.Fatal(err)
	}
	want := []string{"team_created", "team_member_added", "team_member_removed", "team_deleted"}
	if got := l.actions(); !equalStrings(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
}

func TestOrgEventInviteLifecycle(t *testing.T) {
	s, l := emitService()
	ctx := context.Background()
	mustEmitOrg(t, s, ctx)
	inv, err := s.CreateOrgInvite(ctx, "acme", "carol@example.com", string(OrgMember), "alice@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.events) != 1 || l.events[0].action != "invite_created" {
		t.Fatalf("events = %+v", l.events)
	}
	if l.events[0].actor != "carol@example.com" && l.events[0].actor != "alice@example.com" {
		t.Fatalf("invite_created actor = %q", l.events[0].actor)
	}
	// Accept emits invite_accepted (SetMember inside emits member_added).
	kind, err := s.AcceptInvite(ctx, "carol@example.com", inv.ID)
	if err != nil || kind != InviteOrg {
		t.Fatalf("AcceptInvite = %q, %v", kind, err)
	}
	want := []string{"invite_created", "member_added", "invite_accepted"}
	if got := l.actions(); !equalStrings(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	// Invitee decline via CancelInvite emits invite_cancelled.
	inv2, err := s.CreateOrgInvite(ctx, "acme", "dave@example.com", string(OrgMember), "alice@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelInvite(ctx, "dave@example.com", inv2.ID); err != nil {
		t.Fatal(err)
	}
	got := l.actions()
	if len(got) != 5 || got[3] != "invite_created" || got[4] != "invite_cancelled" {
		t.Fatalf("actions = %v", got)
	}
	if l.events[4].actor != "dave@example.com" {
		t.Fatalf("decline actor = %q, want the invitee", l.events[4].actor)
	}
	// Owner cancel via DeleteOrgInvite emits invite_cancelled too.
	inv3, err := s.CreateOrgInvite(ctx, "acme", "erin@example.com", string(OrgMember), "alice@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	del, err := s.DeleteOrgInvite(ctx, "acme", inv3.ID, "alice@example.com")
	if err != nil || del.Subject != "erin@example.com" {
		t.Fatalf("DeleteOrgInvite = %+v, %v", del, err)
	}
	got = l.actions()
	if len(got) != 7 || got[5] != "invite_created" || got[6] != "invite_cancelled" {
		t.Fatalf("actions = %v", got)
	}
	if l.events[6].actor != "alice@example.com" {
		t.Fatalf("owner-cancel actor = %q, want the owner", l.events[6].actor)
	}
	// Unknown ids cancel with 404-shaped errors and emit nothing.
	if _, err := s.DeleteOrgInvite(ctx, "acme", "nope", "alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteOrgInvite unknown = %v", err)
	}
	if len(l.actions()) != 7 {
		t.Fatalf("unknown cancel emitted: %v", l.actions())
	}
}

func TestOrgEventNilObserver(t *testing.T) {
	// Nil OrgEvent never panics, anywhere in the lifecycle.
	s := testService()
	ctx := context.Background()
	mustEmitOrg(t, s, ctx)
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "devs", "D", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "devs", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveTeamMember(ctx, "acme", "devs", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeam(ctx, "acme", "devs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveMember(ctx, "acme", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
}
