package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- FakeRoles (P6, same shape as internal/pulls) ------------------------------

// FakeRoles is a scripted RoleService: roles by principal name, Public
// toggles anonymous reads.
type FakeRoles struct {
	Roles  map[string]string
	Public bool
}

func (f *FakeRoles) roleOf(name string) string {
	if f.Roles == nil {
		return ""
	}
	return f.Roles[strings.ToLower(name)]
}

func (f *FakeRoles) Resolve(_ context.Context, _, _ string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	if p.Admin {
		return identity.RoleAdmin, nil
	}
	if r := f.roleOf(p.Name); r != "" {
		return identity.Role(r), nil
	}
	if p.Anonymous {
		return "", nil
	}
	return identity.RoleRead, nil
}

func (f *FakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Admin || p.Write {
		return nil
	}
	if p.Anonymous {
		if f.Public {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if r := f.roleOf(p.Name); r == "" && !f.Public {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private repository"}
	}
	return nil
}

// --- fixture -------------------------------------------------------------------

const (
	testOwner = "o"
	testRepo  = "r"
	testPR    = 7
	testHead  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testHead2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testBase  = "cccccccccccccccccccccccccccccccccccccccc"
)

func testPrincipal(name string) auth.Principal { return auth.Principal{Name: name} }

func testSvc() (*Service, *FakeRoles) {
	roles := &FakeRoles{Roles: map[string]string{
		"alice": "write", "bob": "maintain", "carol": "read", "dave": "triage", "erin": "admin",
		"fred": "read", "gina": "read",
	}}
	svc := New(store.NewMemory(), roles)
	svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	return svc, roles
}

// seedPR writes a PR header (kind pr, author alice) + pr.json sidecar
// (head testHead, base refs/heads/main). Returns the header version
// carrier for CAS-race tests.
func seedPR(t *testing.T, svc *Service) {
	t.Helper()
	now := "2026-09-04T12:00:00Z"
	h := &PRHeader{
		Num: testPR, Kind: "pr", Title: "Add feature", State: "open",
		Author: "alice", CreatedAt: now, UpdatedAt: now,
		Labels: []string{}, Assignees: []string{}, Participants: []string{"alice"},
		NextEventSeq: 1, CommentCount: 0, Version: 1,
	}
	put(t, svc, ThreadKey(testOwner, testRepo, testPR), encodePRHeader(h), store.PutCreate, "")
	side := &PRSidecar{Num: testPR, Merged: false}
	side.Base.Ref = "refs/heads/main"
	side.Base.SHA = testBase
	side.Base.Repo = "o/r"
	side.Head.Ref = "refs/heads/topic"
	side.Head.SHA = testHead
	side.Head.Repo = "o/r"
	raw, _ := json.Marshal(side)
	put(t, svc, PRKey(testOwner, testRepo, testPR), raw, store.PutCreate, "")
}

func put(t *testing.T, svc *Service, key string, body []byte, mode store.PutMode, ver store.Version) {
	t.Helper()
	_, err := store.PutBytes(context.Background(), svc.Store, key, body,
		store.PutOptions{Mode: mode, IfVersion: ver, ContentType: "application/json"})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func get(t *testing.T, svc *Service, key string) []byte {
	t.Helper()
	raw, _, err := store.GetBytes(context.Background(), svc.Store, key, store.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return raw
}

func testAnchor() Anchor {
	return Anchor{
		Path: "src/main.go", Side: SideNew,
		NewStart: 120, NewLines: 3,
		CommitSHA:  testHead,
		ContextSHA: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}
