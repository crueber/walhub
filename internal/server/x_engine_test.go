package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// walTestConfig mirrors wal's registry test config.
func walTestCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	return cfg
}

func TestBundleListClone(t *testing.T) {
	bl := BundleList{Fulls: []BundleEntry{{Strategy: "full", Name: "a.bundle"}},
		Chain: []BundleEntry{{Strategy: "day1", Name: "c.bundle"}}}
	cp := bl.Clone()
	cp.Fulls[0].Name = "mutated"
	if bl.Fulls[0].Name != "a.bundle" {
		t.Fatal("Clone must copy")
	}
	if len(cp.Chain) != 1 {
		t.Fatalf("chain copy len = %d", len(cp.Chain))
	}
}

func TestBundleAndLFSKeys(t *testing.T) {
	id := mustRepoID(t, "o/r")
	if got := bundleKey(id, BundleEntry{Strategy: "full", Name: "b.bundle"}); got != "repos/o/r/bundles/full/b.bundle" {
		t.Fatalf("bundleKey = %q", got)
	}
	if got := lfsKey(id, "abc"); got != "repos/o/r/lfs/objects/abc" {
		t.Fatalf("lfsKey = %q", got)
	}
}

func TestMatchAnyGlob(t *testing.T) {
	cases := []struct {
		globs []string
		id    string
		want  bool
	}{
		{[]string{"*"}, "o/r", true},
		{[]string{"o/*"}, "o/r", true},
		{[]string{"o/r"}, "o/r", true},
		{[]string{"o/*", "x/*"}, "x/y", true},
		{[]string{"o/*"}, "p/r", false},
		{[]string{"o/r", "p/*"}, "p/r", true},
		{nil, "o/r", false},
		{[]string{"o/*"}, "o/r/deep", true},
	}
	for _, tc := range cases {
		if got := matchAnyGlob(tc.globs, tc.id); got != tc.want {
			t.Fatalf("matchAnyGlob(%v, %q) = %v, want %v", tc.globs, tc.id, got, tc.want)
		}
	}
}

func TestErrAsWalNotFound(t *testing.T) {
	var we *wal.WalError
	if !errAsWalNotFound(fmt.Errorf("wrap: %w", wal.ErrNotFound("o/r")), &we) {
		t.Fatal("wrapped not-found must resolve")
	}
	if we.Kind != wal.WalErrNotFound {
		t.Fatalf("kind = %v", we.Kind)
	}
	if errAsWalNotFound(errors.New("boom"), &we) {
		t.Fatal("plain error must not resolve")
	}
	if errAsWalNotFound(wal.ErrExists("o/r"), &we) {
		t.Fatal("exists is not not-found")
	}
}

func TestWalEnginePlacementAndAutoCreate(t *testing.T) {
	cfg := config.Defaults()
	cfg.Placement.Serve = []string{"*"}
	cfg.Placement.ServeExclude = []string{"private/*"}
	cfg.Placement.Maintain = []string{"*"}
	e := NewWalEngine(nil, cfg)
	id := mustRepoID(t, "o/r")
	pl, err := e.Placement(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !pl.Serve || !pl.Maintain {
		t.Fatalf("placement = %+v", pl)
	}
	pl, _ = e.Placement(context.Background(), mustRepoID(t, "private/x"))
	if pl.Serve {
		t.Fatalf("excluded repo served: %+v", pl)
	}
	if e.AutoCreate(context.Background(), id) {
		t.Fatal("defaults leave auto_create_on_push off")
	}
	cfg.Server.AutoCreateOnPush = true
	if !NewWalEngine(nil, cfg).AutoCreate(context.Background(), id) {
		t.Fatal("flag must be honored")
	}
}

func TestWalEngineBundlesFromStore(t *testing.T) {
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(context.Background(), st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")

	// Absent list → empty.
	bl, err := e.Bundles(context.Background(), id, "")
	if err != nil || len(bl.Fulls) != 0 || len(bl.Chain) != 0 {
		t.Fatalf("absent list: %+v %v", bl, err)
	}
	// Unaccepted filter → empty.
	if bl, _ := e.Bundles(context.Background(), id, "weird"); len(bl.Fulls) != 0 {
		t.Fatal("bad filter must yield empty list")
	}
	full := proto.BundleEntry{Key: "bundles/full/full-1.bundle", Strategy: "full", Kind: "full"}
	chain := proto.BundleEntry{Key: "bundles/day1/c1.bundle", Strategy: "day1", Kind: "incremental"}
	body := (&proto.BundleList{Bundles: []*proto.BundleEntry{&full, &chain}}).Marshal()
	if _, err := st.Put(context.Background(), id.StorePrefix()+store.BundleList,
		store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	bl, err = e.Bundles(context.Background(), id, "blob:none")
	if err != nil {
		t.Fatal(err)
	}
	if len(bl.Fulls) != 1 || bl.Fulls[0].Name != "full-1.bundle" || bl.Fulls[0].Strategy != "full" {
		t.Fatalf("fulls = %+v", bl.Fulls)
	}
	if len(bl.Chain) != 1 || bl.Chain[0].Name != "c1.bundle" {
		t.Fatalf("chain = %+v", bl.Chain)
	}
}

func TestWalEngineSyncUnknownRepo(t *testing.T) {
	cfg := walTestCfg(t)
	reg := wal.NewRegistry(context.Background(), store.NewMemory(), cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	err := e.Sync(context.Background(), mustRepoID(t, "no/such"), wal.LevelRefs)
	if !isNotFound(err) {
		t.Fatalf("unknown repo sync err = %v, want not-found", err)
	}
}

func TestWalEngineRepoCreateAndOpen(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")

	// open with create=false → not found.
	if _, err := e.Repo(ctx, id, false, git.Sha1); !isNotFound(err) {
		t.Fatalf("open missing = %v", err)
	}
	// create=true creates the repo + manifest.
	repo, err := e.Repo(ctx, id, true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if repo == nil || repo.Path == "" {
		t.Fatal("repo path missing")
	}
	// Re-open with create=false now succeeds and syncs to serve level.
	repo2, err := e.Repo(ctx, id, false, git.Sha1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if repo2 := repo2; repo2.Path != repo.Path {
		t.Fatalf("paths differ: %q vs %q", repo.Path, repo2.Path)
	}
	// errAsWalNotFound unwraps through wal.ErrNotFound only.
	var we *wal.WalError
	if errAsWalNotFound(errors.New("x"), &we) {
		t.Fatal("plain error must not resolve")
	}
}
