package tags

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- fakes -------------------------------------------------------------------

type fakeRoles struct {
	mu    sync.Mutex
	roles map[string]string // "owner/repo\x00principal" → role
}

func newFakeRoles() *fakeRoles { return &fakeRoles{roles: map[string]string{}} }

func (f *fakeRoles) grant(owner, repo, principal, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[owner+"/"+repo+"\x00"+principal] = role
}

func (f *fakeRoles) Resolve(_ context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.roles[owner+"/"+repo+"\x00"+p.Name]; ok {
		return identity.Role(r), nil
	}
	return "", nil
}

func (f *fakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrInvalid, Why: "unauthorized"}
	}
	return nil
}

// fakeGit scripts commit resolution; err forces every call to fail.
type fakeGit struct {
	mu      sync.Mutex
	commits map[string]string // sha → resolved sha
	err     error
	calls   int
}

func newFakeGit() *fakeGit { return &fakeGit{commits: map[string]string{}} }

func (f *fakeGit) CommitExists(_ context.Context, _ string, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if r, ok := f.commits[sha]; ok {
		return r, nil
	}
	return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, sha)
}

type fakeDirs struct{ dir string }

func (f *fakeDirs) Dir(_ context.Context, _ string) (string, error) { return f.dir, nil }

type errDirs struct{}

func (errDirs) Dir(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("no git dir")
}

// fakeRefs records creates and scripts outcomes: existing names fail with a
// conflict-class error (the publish verify verdict); err forces a generic
// failure.
type fakeRefs struct {
	mu       sync.Mutex
	created  []refCreate
	existing map[string]string
	err      error
}

type refCreate struct {
	repo string
	name string
	sha  string
	meta map[string]string
}

func newFakeRefs() *fakeRefs { return &fakeRefs{existing: map[string]string{}} }

func (f *fakeRefs) CreateTag(_ context.Context, repo, name, sha string, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.existing[name]; ok {
		return fmt.Errorf("CAS conflict: refs/tags/%s exists", name)
	}
	f.existing[name] = sha
	f.created = append(f.created, refCreate{repo: repo, name: name, sha: sha, meta: meta})
	return nil
}

// errStore fails every Get (exercises the policy-load store-error path).
type errStore struct {
	store.ObjectStore
	err error
}

func (s *errStore) Get(_ context.Context, _ string, _ store.GetOptions) (store.GetResult, error) {
	return nil, s.err
}

// --- harness -------------------------------------------------------------------

type harness struct {
	svc     *Service
	handler *Handler
	roles   *fakeRoles
	git     *fakeGit
	refs    *fakeRefs
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.NewMemory()
	roles := newFakeRoles()
	g := newFakeGit()
	g.commits[testSHA] = testSHA
	refs := newFakeRefs()
	svc := New(st, roles)
	svc.Git = g
	svc.Dirs = &fakeDirs{dir: t.TempDir() + "/repo.git"}
	svc.Refs = refs
	h := &Handler{Svc: svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return principalFor(r), nil
	}
	return &harness{svc: svc, handler: h, roles: roles, git: g, refs: refs}
}

func principalFor(r *http.Request) auth.Principal {
	name := r.Header.Get("X-Test-Principal")
	if name == "" || name == "anonymous" {
		return auth.Anonymous()
	}
	p := auth.Principal{Name: name}
	if r.Header.Get("X-Test-Admin") == "1" {
		p.Admin = true
	}
	if r.Header.Get("X-Test-Write") == "1" {
		p.Write = true
	}
	return p
}

func ctx() context.Context { return context.Background() }

func writer() auth.Principal { return auth.Principal{Name: "jane"} }

func admin() auth.Principal { return auth.Principal{Name: "root", Admin: true} }

func grantWrite(x *harness) { x.roles.grant("o", "r", "jane", "write") }

func nowFixed() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }

var _ = nowFixed
