package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// fakeGit scripts tag resolution and ancestry; counts calls for the
// evidence harness.
type fakeGit struct {
	mu          sync.Mutex
	tags        map[string]string // tag → sha
	ancestors   map[string]bool   // "a\x00b" → IsAncestor(a, b)
	ancestorErr map[string]error  // "a\x00b" → IsAncestor failure (probe-only)
	tagList     []string
	listErr     error
	resolveN    int
	ancestorN   int
	listN       int
	err         error
}

func newFakeGit() *fakeGit {
	return &fakeGit{tags: map[string]string{}, ancestors: map[string]bool{}}
}

func (f *fakeGit) ResolveRef(_ context.Context, _ string, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveN++
	if f.err != nil {
		return "", f.err
	}
	tag := strings.TrimPrefix(ref, "refs/tags/")
	if sha, ok := f.tags[tag]; ok {
		return sha, nil
	}
	// Non-tag refs resolve verbatim when scripted (since may name a branch).
	if sha, ok := f.tags[ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
}

func (f *fakeGit) IsAncestor(_ context.Context, _ string, a, b string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ancestorN++
	if err, ok := f.ancestorErr[a+"\x00"+b]; ok {
		return false, err
	}
	if f.err != nil {
		return false, f.err
	}
	return f.ancestors[a+"\x00"+b], nil
}

func (f *fakeGit) ListTags(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listN++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return append([]string{}, f.tagList...), nil
}

type fakeDirs struct{ dir string }

func (f *fakeDirs) Dir(_ context.Context, _ string) (string, error) { return f.dir, nil }

// flakyDirs fails the Nth Dir call (orchestrates mid-request git outages
// AFTER the up-front resolveTag gate succeeded).
type flakyDirs struct {
	dir    string
	failOn int
	calls  int
}

func (f *flakyDirs) Dir(_ context.Context, _ string) (string, error) {
	f.calls++
	if f.failOn > 0 && f.calls == f.failOn {
		return "", fmt.Errorf("synthetic dirs outage")
	}
	return f.dir, nil
}

type errDirs struct{}

func (errDirs) Dir(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("no git dir")
}

// --- harness -------------------------------------------------------------------

type harness struct {
	svc     *Service
	handler *Handler
	roles   *fakeRoles
	git     *fakeGit
	now     time.Time
	spool   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.NewMemory()
	roles := newFakeRoles()
	git := newFakeGit()
	svc := New(st, roles)
	svc.Git = git
	svc.Dirs = &fakeDirs{dir: t.TempDir() + "/repo.git"}
	svc.SpoolDir = t.TempDir()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	h := &Handler{Svc: svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return principalFor(r), nil
	}
	return &harness{svc: svc, handler: h, roles: roles, git: git, now: now, spool: svc.SpoolDir}
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

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }

func mustPut(t *testing.T, x *harness, p auth.Principal, tag string, in ReleaseInput) (*Release, bool) {
	t.Helper()
	rel, created, err := x.svc.PutRelease(ctx(), "o", "r", p, tag, in, "")
	if err != nil {
		t.Fatalf("PutRelease %q: %v", tag, err)
	}
	return rel, created
}

// seedIndex writes a shared issues/index.json with kind/title/author cards
// (02-owned shape; autodraft reads kind:"pr" rows).
func seedIndex(t *testing.T, x *harness, cards ...map[string]any) {
	t.Helper()
	open := make([]any, 0, len(cards))
	for _, c := range cards {
		open = append(open, c)
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "open": open, "closed_recent": []any{}})
	if _, err := store.PutBytes(ctx(), x.svc.Store, "repos/o/r/issues/index.json", raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func prCard(num int, title, author string) map[string]any {
	return map[string]any{"num": num, "kind": "pr", "title": title, "author": author}
}

// seedPR writes a thread header + pr.json sidecar (03-owned shapes).
func seedPR(t *testing.T, x *harness, num int, title, author string, merged bool, mergeSHA, mergedAt string) {
	t.Helper()
	th, _ := json.Marshal(map[string]any{"num": num, "kind": "pr", "title": title, "author": author})
	tkey := fmt.Sprintf("repos/o/r/issues/%06x/thread.json", num)
	if _, err := store.PutBytes(ctx(), x.svc.Store, tkey, th,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	var msha, mat *string
	if mergeSHA != "" {
		msha = &mergeSHA
	}
	if mergedAt != "" {
		mat = &mergedAt
	}
	pr, _ := json.Marshal(map[string]any{"num": num, "merged": merged, "merged_at": mat, "merge_commit_sha": msha})
	pkey := fmt.Sprintf("repos/o/r/pulls/%06x/pr.json", num)
	if _, err := store.PutBytes(ctx(), x.svc.Store, pkey, pr,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func writer() auth.Principal { return auth.Principal{Name: "jane"} }
func admin() auth.Principal  { return auth.Principal{Name: "root", Admin: true} }

func grantWrite(x *harness) { x.roles.grant("o", "r", "jane", "write") }
func grantMaintain(x *harness) {
	x.roles.grant("o", "r", "jane", "maintain")
}
