package server

import (
	"context"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// fakeEngine implements Engine for tests.
type fakeEngine struct {
	placement     Placement // zero value lacks Serve; newTestServer enables it
	bundles       BundleList
	synced        []wal.SyncLevel
	published     int
	exists        bool
	noCreate      bool // AutoCreate returns false when set
	pubErr        error
	lastPrincipal string
	pubPerRef     []wal.RefResult
	syncErr       error
	repoErr       error
	repoCreate    func(root string, id git.RepoId, format git.ObjectFormat) (*git.LocalRepo, error)
}

func (f *fakeEngine) Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error {
	f.synced = append(f.synced, level)
	if f.syncErr != nil {
		return f.syncErr
	}
	if !f.exists {
		return wal.ErrNotFound(id.String())
	}
	return nil
}

func (f *fakeEngine) Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error) {
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	if !f.exists && !create {
		return nil, wal.ErrNotFound(id.String())
	}
	root := ctx.Value(repoRootKey{}).(string)
	return git.InitLocalRepo(root, id, format)
}

type repoRootKey struct{}

func (f *fakeEngine) Publish(ctx context.Context, id git.RepoId, req *git.PushRequest, principal string, access wal.ObjectAccess) (wal.PublishResult, error) {
	f.published++
	f.lastPrincipal = principal
	if f.pubErr != nil {
		return wal.PublishResult{}, f.pubErr
	}
	if f.pubPerRef != nil {
		return wal.PublishResult{PerRef: f.pubPerRef}, nil
	}
	return wal.PublishResult{PerRef: []wal.RefResult{{Name: "refs/heads/main"}}}, nil
}

func (f *fakeEngine) Placement(ctx context.Context, id git.RepoId) (Placement, error) {
	return f.placement, nil
}

func (f *fakeEngine) Bundles(ctx context.Context, id git.RepoId, filter string) (BundleList, error) {
	if !f.exists {
		return BundleList{}, wal.ErrNotFound(id.String())
	}
	return f.bundles, nil
}

func (f *fakeEngine) AutoCreate(ctx context.Context, id git.RepoId) bool { return !f.noCreate }

// fakeStore wraps the real memory store (02_storage_protobuf.md contract).
func newFakeStore() store.ObjectStore { return store.NewMemory() }

var _ = time.Now
var _ = auth.Anonymous

func TestFakesCompile(t *testing.T) {
	var _ Engine = &fakeEngine{}
	var _ store.ObjectStore = newFakeStore()
}
