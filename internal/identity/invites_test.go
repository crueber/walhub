package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestOrgInvites(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrgInvite(ctx, "ghost", "x@y.z", "member", "a@b.c", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("invite ghost org: %v", err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrgInvite(ctx, "acme", "not-an-email", "member", "a@b.c", time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("invite bad email: %v", err)
	}
	if _, err := s.CreateOrgInvite(ctx, "acme", "x@y.z", "root", "a@b.c", time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("invite bad role: %v", err)
	}
	inv, err := s.CreateOrgInvite(ctx, "acme", "PAT@example.com", "member", "alice@example.com", time.Hour)
	if err != nil {
		t.Fatalf("CreateOrgInvite: %v", err)
	}
	if inv.Subject != "pat@example.com" || inv.State != "pending" || inv.Token == "" || inv.ID == "" {
		t.Errorf("bad invite: %+v", inv)
	}
	// Inbox.
	entries, err := s.MyInvites(ctx, "pat@example.com")
	if err != nil || len(entries) != 1 || entries[0].Org != "acme" {
		t.Errorf("MyInvites: %v %+v", err, entries)
	}
	if entries, err := s.MyInvites(ctx, "nobody@example.com"); err != nil || len(entries) != 0 {
		t.Errorf("MyInvites empty: %v %+v", err, entries)
	}
	// Preview with token redacted.
	prev, err := s.PreviewInvite(ctx, "pat@example.com", inv.ID, inv.Token)
	if err != nil {
		t.Fatalf("PreviewInvite: %v", err)
	}
	if prev.Token != "" || prev.Subject != "pat@example.com" {
		t.Errorf("preview broken: %+v", prev)
	}
	// Subject matches: preview works with or without (even a wrong) token.
	if _, err := s.PreviewInvite(ctx, "pat@example.com", inv.ID, ""); err != nil {
		t.Errorf("subject preview: %v", err)
	}
	if _, err := s.PreviewInvite(ctx, "pat@example.com", inv.ID, "wrong"); err != nil {
		t.Errorf("subject preview with wrong token: %v", err)
	}
	if _, err := s.PreviewInvite(ctx, "other@example.com", inv.ID, ""); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrForbidden) {
		t.Errorf("wrong subject: %v", err)
	}
	// Accept by the wrong subject is forbidden.
	if _, err := s.AcceptInvite(ctx, "other@example.com", inv.ID); err == nil {
		t.Error("accept by non-subject must fail")
	}
	// Accept binds membership and deletes the invite.
	bound, err := s.AcceptInvite(ctx, "pat@example.com", inv.ID)
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if bound != InviteOrg {
		t.Errorf("bound = %q", bound)
	}
	m, _ := s.GetMembers(ctx, "acme")
	found := false
	for _, e := range m.Members {
		if e.Principal == "pat@example.com" && e.Role == OrgMember {
			found = true
		}
	}
	if !found {
		t.Errorf("membership not bound: %+v", m.Members)
	}
	// Second accept is done (invite gone).
	if _, err := s.AcceptInvite(ctx, "pat@example.com", inv.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("re-accept must 409: %v", err)
	}
	// Inbox entry dropped.
	if entries, _ := s.MyInvites(ctx, "pat@example.com"); len(entries) != 0 {
		t.Errorf("inbox not cleaned: %+v", entries)
	}
	// Profile lazily created.
	if p, _ := s.GetProfile(ctx, "pat@example.com"); p == nil {
		t.Error("accept must ensure the profile")
	}
}

func TestRepoInvites(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	if _, err := s.CreateRepoInvite(ctx, "acme", "repo", "not-an-email", RoleWrite, "a@b.c", time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad subject: %v", err)
	}
	if _, err := s.CreateRepoInvite(ctx, "acme", "repo", "x@y.z", "super", "a@b.c", time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad role: %v", err)
	}
	inv, err := s.CreateRepoInvite(ctx, "acme", "repo", "dave@example.com", RoleMaintain, "alice@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListRepoInvites(ctx, "acme", "repo", 100)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListRepoInvites: %v %+v", err, list)
	}
	if _, err := s.AcceptInvite(ctx, "dave@example.com", inv.ID); err != nil {
		t.Fatalf("AcceptInvite repo: %v", err)
	}
	role, _ := s.Resolve(ctx, "acme", "repo", daveP())
	if role != RoleMaintain {
		t.Errorf("repo invite must bind role, got %q", role)
	}
	// Cancel path: create then cancel as invitee.
	inv2, err := s.CreateRepoInvite(ctx, "acme", "repo", "erin@example.com", RoleRead, "alice@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelInvite(ctx, "erin@example.com", inv2.ID)
	if err != nil || cancelled.ID != inv2.ID {
		t.Errorf("CancelInvite: %v %+v", err, cancelled)
	}
	if _, err := s.AcceptInvite(ctx, "erin@example.com", inv2.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("accept after cancel must 409: %v", err)
	}
	// Unknown kind rejected.
	bad := &Invitation{Version: 1, ID: "bad", Token: "t", Kind: "weird", Subject: "erin@example.com", State: "pending", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	if _, err := store.PutBytes(ctx, s.Store, OrgInviteKey("acme", "bad"), encodeInvite(bad),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(ctx, "erin@example.com", InboxEntry{ID: "bad", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, "erin@example.com", "bad"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown kind: %v", err)
	}
	// Bad repo shape in invite: reachable via the org key but unusable.
	bad2 := &Invitation{Version: 1, ID: "bad2", Token: "t", Kind: InviteRepo, Repo: "noslash", Subject: "erin@example.com", State: "pending", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	if _, err := store.PutBytes(ctx, s.Store, OrgInviteKey("acme", "bad2"), encodeInvite(bad2),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(ctx, "erin@example.com", InboxEntry{ID: "bad2", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, "erin@example.com", "bad2"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad repo shape: %v", err)
	}
}

func TestInviteExpiry(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateOrgInvite(ctx, "acme", "old@example.com", "member", "alice@example.com", -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewInvite(ctx, "old@example.com", inv.ID, inv.Token); !errors.Is(err, ErrConflict) {
		t.Errorf("expired preview: %v", err)
	}
	if _, err := s.AcceptInvite(ctx, "old@example.com", inv.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("expired accept: %v", err)
	}
}

func TestInboxOps(t *testing.T) {
	s := testService()
	ctx := context.Background()
	// inboxAdd dedups.
	if err := s.inboxAdd(ctx, "x@y.z", InboxEntry{ID: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.inboxAdd(ctx, "x@y.z", InboxEntry{ID: "1"}); err != nil {
		t.Errorf("inboxAdd dup: %v", err)
	}
	// inboxRemove on missing is a no-op.
	if err := s.inboxRemove(ctx, "nobody@example.com", "1"); err != nil {
		t.Errorf("inboxRemove missing: %v", err)
	}
	// Corrupt inbox.
	if _, err := store.PutBytes(ctx, s.Store, InboxKey("bad@x.c"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MyInvites(ctx, "bad@x.c"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt inbox: %v", err)
	}
	if err := s.inboxAdd(ctx, "bad@x.c", InboxEntry{ID: "2"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("inboxAdd corrupt: %v", err)
	}
	if err := s.inboxRemove(ctx, "bad@x.c", "2"); !errors.Is(err, ErrInvalid) {
		t.Errorf("inboxRemove corrupt: %v", err)
	}
	if _, err := s.findInvite(ctx, "bad@x.c", "2"); !errors.Is(err, ErrInvalid) {
		t.Errorf("findInvite corrupt: %v", err)
	}
	sErr := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if _, err := sErr.MyInvites(ctx, "x@y.z"); !errors.Is(err, errBoom) {
		t.Errorf("MyInvites error: %v", err)
	}
	// findInvite with inbox entry but missing object → not pending.
	s2 := testService()
	if err := s2.inboxAdd(ctx, "ghost@y.z", InboxEntry{ID: "nope", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.findInvite(ctx, "ghost@y.z", "nope"); !errors.Is(err, ErrConflict) {
		t.Errorf("dangling inbox entry: %v", err)
	}
	// Corrupt invite object.
	if _, err := store.PutBytes(ctx, s2.Store, OrgInviteKey("acme", "corrupt"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s2.inboxAdd(ctx, "ghost@y.z", InboxEntry{ID: "corrupt", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.findInvite(ctx, "ghost@y.z", "corrupt"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt invite: %v", err)
	}
	// findInvite store error (non-NotFound) surfaces.
	s3 := New(&errStore{ObjectStore: s2.Store, getErr: errBoom}, config.Defaults())
	if _, err := s3.findInvite(ctx, "ghost@y.z", "corrupt"); !errors.Is(err, errBoom) {
		t.Errorf("findInvite inbox error: %v", err)
	}
}

func TestListInvites(t *testing.T) {
	s := testService()
	ctx := context.Background()
	seedOrg(t, s)
	if _, err := s.CreateOrgInvite(ctx, "acme", "a@x.c", "member", "alice@example.com", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepoInvite(ctx, "acme", "r", "b@x.c", RoleRead, "alice@example.com", time.Hour); err != nil {
		t.Fatal(err)
	}
	orgs, err := s.ListOrgInvites(ctx, "acme", 100)
	if err != nil || len(orgs) != 1 {
		t.Errorf("ListOrgInvites: %v %+v", err, orgs)
	}
	repos, err := s.ListRepoInvites(ctx, "acme", "r", 100)
	if err != nil || len(repos) != 1 {
		t.Errorf("ListRepoInvites: %v %+v", err, repos)
	}
	if empty, err := s.ListOrgInvites(ctx, "empty", 100); err != nil || len(empty) != 0 {
		t.Errorf("empty org invites: %v %+v", err, empty)
	}
	// Corrupt objects are skipped.
	if _, err := store.PutBytes(ctx, s.Store, OrgInviteKey("acme", "corrupt"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if orgs, err := s.ListOrgInvites(ctx, "acme", 100); err != nil || len(orgs) != 1 {
		t.Errorf("corrupt invite must be skipped: %v %+v", err, orgs)
	}
	sErr := New(&errStore{ObjectStore: store.NewMemory(), listErr: errBoom}, config.Defaults())
	if _, err := sErr.ListOrgInvites(ctx, "acme", 100); !errors.Is(err, errBoom) {
		t.Errorf("ListOrgInvites error: %v", err)
	}
	if _, err := sErr.ListRepoInvites(ctx, "acme", "r", 100); !errors.Is(err, errBoom) {
		t.Errorf("ListRepoInvites error: %v", err)
	}
	// inviteKeys for repo kind.
	inv := &Invitation{Kind: InviteRepo, Org: "acme", Repo: "acme/r", ID: "x"}
	if k := inviteKeys(inv); k != RepoInviteKey("acme", "r", "x") {
		t.Errorf("inviteKeys repo: %q", k)
	}
	// CancelInvite store error.
	if _, err := sErr.CancelInvite(ctx, "a@x.c", "whatever"); err == nil {
		t.Error("CancelInvite must surface lookup errors")
	}
}
