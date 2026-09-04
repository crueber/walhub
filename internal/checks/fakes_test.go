package checks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- FakeRoles (P6, same shape as the sibling packages) -------------------------

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

// --- FakeCommits -----------------------------------------------------------------

// FakeCommits is a scripted CommitChecker: Known shas resolve (echoed
// back, lowercased); UnknownErr scripts the failure (unknown ⇒
// ErrNotFound-class, outage ⇒ ErrUnavailable-class).
type FakeCommits struct {
	Known      map[string]bool
	UnknownErr error
	Calls      int
}

func (f *FakeCommits) ResolveCommit(_ context.Context, _, sha string) (string, error) {
	f.Calls++
	lower := strings.ToLower(sha)
	if f.Known[lower] {
		return lower, nil
	}
	if f.UnknownErr != nil {
		return "", f.UnknownErr
	}
	return "", fmt.Errorf("%w: unknown sha %q", ErrNotFound, sha)
}

// --- env ---------------------------------------------------------------------------

// testEnv bundles a service over a memory store with a fixed clock.
type testEnv struct {
	store   *store.Memory
	roles   *FakeRoles
	commits *FakeCommits
	svc     *Service
	now     time.Time
}

func newTestEnv() *testEnv {
	return newTestEnvAt(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
}

func newTestEnvAt(now time.Time) *testEnv {
	st := store.NewMemory()
	roles := &FakeRoles{Roles: map[string]string{
		"jane@example.com":  "write",
		"admin@example.com": "admin",
		"sam@example.com":   "read",
	}, Public: true}
	commits := &FakeCommits{Known: map[string]bool{}}
	e := &testEnv{store: st, roles: roles, commits: commits, now: now}
	svc := New(st, roles)
	svc.Commits = commits
	svc.Now = func() time.Time { e.now = e.now.Add(time.Second); return e.now }
	e.svc = svc
	return e
}

func ctx() context.Context { return context.Background() }

// Principals.
func admin() auth.Principal       { return auth.Principal{Name: "admin@example.com", Admin: true} }
func writer() auth.Principal      { return auth.Principal{Name: "jane@example.com", Write: true} }
func reader() auth.Principal      { return auth.Principal{Name: "sam@example.com"} }
func anon() auth.Principal        { return auth.Anonymous() }
func ci(id string) auth.Principal { return auth.Principal{Name: CIPrincipalName(id)} }

// hexSHA returns a deterministic full sha for tests ("a…a" + 2-digit suffix).
func hexSHA(n int) string {
	return strings.Repeat("a", 38) + fmt.Sprintf("%02d", n)
}

// knowSHA marks a sha as resolving.
func (e *testEnv) knowSHA(sha string) {
	e.commits.Known[strings.ToLower(sha)] = true
}

// report posts one status as writer (fails the test on error).
func (e *testEnv) mustReport(t interface {
	Helper()
	Fatalf(string, ...any)
}, sha, context, state string) *StatusDoc {
	t.Helper()
	st, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: context, State: state})
	if err != nil {
		t.Fatalf("report %s=%s: %v", context, state, err)
	}
	return st
}

// putPolicy writes a policy.json directly (tests only).
func (e *testEnv) putPolicy(t interface {
	Helper()
	Fatalf(string, ...any)
}, raw string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), e.store, "repos/o/r/policy.json", []byte(raw),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		_ = err
		_, _ = store.PutBytes(ctx(), e.store, "repos/o/r/policy.json", []byte(raw),
			store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"})
	}
}
