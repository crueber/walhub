package identity

import (
	"context"
	"errors"
	"testing"
)

func TestBootstrap(t *testing.T) {
	s := testService()
	ctx := context.Background()
	// No repos: nothing to do.
	c, sk, err := s.Bootstrap(ctx)
	if err != nil || c != 0 || sk != 0 {
		t.Errorf("empty bootstrap = %d/%d, %v", c, sk, err)
	}
	// Two repos via stub lister.
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"acme", "one"}, {"bob", "two"}}, nil
	}
	c, sk, err = s.Bootstrap(ctx)
	if err != nil || c != 2 || sk != 0 {
		t.Errorf("bootstrap = %d/%d, %v", c, sk, err)
	}
	doc, ver, err := s.GetAccess(ctx, "acme", "one")
	if err != nil || ver == "" {
		t.Fatalf("materialized read: %v %q", err, ver)
	}
	if doc.Version != 1 || doc.Visibility != VisibilityPublic {
		t.Errorf("materialized doc: %+v", doc)
	}
	// Non-email owner namespaces materialize empty bindings (org-owner
	// resolution covers org repos; host flags still apply per P6 step 3).
	if len(doc.RoleBindings) != 0 {
		t.Errorf("materialized bindings: %+v", doc.RoleBindings)
	}
	// Second sweep skips.
	c, sk, err = s.Bootstrap(ctx)
	if err != nil || c != 0 || sk != 2 {
		t.Errorf("re-sweep = %d/%d, %v", c, sk, err)
	}
	// A repo touched by an admin edit is skipped, not clobbered.
	cur, curVer, _ := s.GetAccess(ctx, "bob", "two")
	_ = cur
	if _, err := s.PutAccess(ctx, "bob", "two", curVer, VisibilityPrivate, nil); err != nil {
		t.Fatal(err)
	}
	c, sk, err = s.Bootstrap(ctx)
	if err != nil || c != 0 || sk != 2 {
		t.Errorf("post-edit sweep = %d/%d, %v", c, sk, err)
	}
	if doc, _, _ := s.GetAccess(ctx, "bob", "two"); doc.Visibility != VisibilityPrivate {
		t.Errorf("bootstrap must not clobber edits: %+v", doc)
	}
	// Lister error aborts.
	s.Repos = func(ctx context.Context) ([][2]string, error) { return nil, errBoom }
	if _, _, err := s.Bootstrap(ctx); !errors.Is(err, errBoom) {
		t.Errorf("lister error: %v", err)
	}
	// Cancelled context aborts.
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"a", "b"}}, nil
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.Bootstrap(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sweep: %v", err)
	}
	// bootstrapOne put error surfaces.
	s2 := testService()
	s2.Store = &errStore{ObjectStore: s2.Store, putErr: errBoom}
	s2.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"a", "b"}}, nil
	}
	if _, _, err := s2.Bootstrap(ctx); !errors.Is(err, errBoom) {
		t.Errorf("put error: %v", err)
	}
	// BootstrapRepo: per-repo materialization (the Seam 5 op body).
	created, err := s.BootstrapRepo(ctx, "solo", "repo")
	if err != nil || !created {
		t.Errorf("BootstrapRepo = %v, %v", created, err)
	}
	created, err = s.BootstrapRepo(ctx, "solo", "repo")
	if err != nil || created {
		t.Errorf("BootstrapRepo re-run = %v, %v", created, err)
	}
}

func TestListReposDefault(t *testing.T) {
	s := testService()
	ctx := context.Background()
	mustAccess(t, s, "acme", "one", VisibilityPublic, nil)
	mustAccess(t, s, "acme", "two", VisibilityPublic, nil)
	mustAccess(t, s, "bob", "three", VisibilityPublic, nil)
	repos, err := s.listRepos(ctx)
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 3 {
		t.Errorf("listRepos = %v", repos)
	}
	sErr := testService()
	sErr.Store = &errStore{ObjectStore: sErr.Store, listErr: errBoom}
	if _, err := sErr.listRepos(ctx); !errors.Is(err, errBoom) {
		t.Errorf("listRepos error: %v", err)
	}
}
