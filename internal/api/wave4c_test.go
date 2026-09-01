// Final coverage pass (Wave 4c): policy store-error arms, pathExists, LaneOf,
// Head unborn arm — table-driven over the standard fixture and real git.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// failingStore delegates to memory but fails selected ops for policy.json.
type failingStore struct {
	store.ObjectStore
	failDelete bool
	failPut    bool
}

var errInjected = errors.New("injected store outage")

func (s *failingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.failPut && strings.HasSuffix(key, "policy.json") {
		return store.ObjectMeta{}, store.NewOther(key, errInjected)
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func (s *failingStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if s.failDelete && strings.HasSuffix(key, "policy.json") {
		return store.NewOther(key, errInjected)
	}
	return s.ObjectStore.Delete(ctx, key, ifVersion)
}

func TestPolicyStoreErrors(t *testing.T) {
	f := newFixture(t)
	fs := &failingStore{failDelete: true}
	fs.ObjectStore = f.env.Store
	f.env.Store = fs

	// non-NotFound delete error → 503 plain text carrying the store error
	w := f.req("DELETE", "/demo/walgit/api/policy")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete with store outage = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "injected store outage") {
		t.Fatalf("body should carry the store error verbatim: %s", w.Body.String())
	}

	// put with store outage → 503
	fs.failPut, fs.failDelete = true, false
	w = f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(`{"version":1,"groups":[],"rules":[]}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("put with store outage = %d body=%s", w.Code, w.Body.String())
	}
}

func TestPolicyDeleteNotFoundIs204(t *testing.T) {
	f := newFixture(t)
	// NotFound on delete → treated as success (204), not 503.
	if w := f.req("DELETE", "/demo/walgit/api/policy"); w.Code != http.StatusNoContent {
		t.Fatalf("delete absent = %d", w.Code)
	}
}

// pathExists: true for the root tree of a commit, false for a missing path and a
// bogus rev (direct unit test over a real bare repo).
func TestWalViewPathExists(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.InitLocalRepo(dir, git.RepoId{Owner: "o", Name: "pe"}, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	tree := runGit(t, repo.Path, "mktree") // empty tree
	commit := runGit(t, repo.Path, "commit-tree", tree, "-m", "root")
	runGit(t, repo.Path, "update-ref", "refs/heads/main", commit)

	v := &walView{}
	if !v.pathExists(context.Background(), repo, commit, "") {
		t.Fatal("root tree path should exist for a commit")
	}
	if v.pathExists(context.Background(), repo, commit, "no/such/file") {
		t.Fatal("missing path should not exist")
	}
	if v.pathExists(context.Background(), repo, strings.Repeat("a", 40), "x") {
		t.Fatal("bogus sha should not resolve")
	}
}

func TestLaneOfDefaultsToAPI(t *testing.T) {
	r := httptest.NewRequest("GET", "/o/r/api/refs", nil)
	if got := LaneOf(r); got != LaneAPI {
		t.Fatalf("LaneOf without ctx = %v", got)
	}
}

func TestHeadUnbornAndError(t *testing.T) {
	// An engine-less view answers 503-style ErrPending, not a head.
	v := &walView{}
	if _, _, err := v.Head(context.Background(), git.RepoId{Owner: "o", Name: "hb"}); err == nil {
		t.Fatal("Head without an engine should error")
	}
	// A serving copy with no HEAD file → unborn HEAD: ok=false, no error.
	dir := t.TempDir()
	id := git.RepoId{Owner: "o", Name: "hb"}
	f := newEngineFixture(t, &fakeEngine{obj: wal.ObjectAccess{Local: &git.LocalRepo{Root: dir, ID: id, Path: dir}}, rev: 1})
	ref, ok, err := f.env.Repo.(*walView).Head(context.Background(), id)
	if err != nil || ok {
		t.Fatalf("unborn head = %v ok=%v err=%v", ref, ok, err)
	}
}
