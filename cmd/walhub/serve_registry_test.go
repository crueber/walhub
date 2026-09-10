// serve_registry_test.go — Forgejo issue #200 (deleted repos must vanish
// from /explore): the serving RepoRegistry (Owners/Repos) is manifest-gated.
// A prefix without repos/<o>/<r>/manifest.pb — a deleted repo's sidecar
// litter, or a fork-provisioned-but-unborn prefix — never lists. Table shape
// is deliberately flat: memory store, one registry, real Delete sweep.
package main

import (
	"context"
	"slices"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

func testRepoRegistry(t *testing.T) (*repoRegistry, context.Context) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	return &repoRegistry{reg: reg, st: st}, ctx
}

func mustCreate(t *testing.T, r *repoRegistry, ctx context.Context, id string) {
	t.Helper()
	if _, err := r.reg.Create(ctx, id, git.Sha1); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func mustPut(t *testing.T, ctx context.Context, st store.ObjectStore, key string) {
	t.Helper()
	if _, err := store.PutBytes(ctx, st, key, []byte("x"), store.PutOptions{}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func checkStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// Manifest-less prefixes (deleted-repo litter, unborn fork children) never list.
func TestRepoRegistryHidesManifestlessPrefixes(t *testing.T) {
	r, ctx := testRepoRegistry(t)
	mustCreate(t, r, ctx, "acme/api")
	mustPut(t, ctx, r.st, "repos/acme/ghost/note.json") // deleted-repo litter stand-in
	mustPut(t, ctx, r.st, "repos/acme/child/fork.json") // fork-provisioned, no manifest yet

	repos, err := r.Repos(ctx, "acme")
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	checkStrings(t, "Repos(acme)", repos, []string{"api"})

	owners, err := r.Owners(ctx)
	if err != nil {
		t.Fatalf("Owners: %v", err)
	}
	checkStrings(t, "Owners", owners, []string{"acme"})
}

// Unknown owners still answer [] (never 404), even with litter elsewhere.
func TestRepoRegistryUnknownOwnerEmpty(t *testing.T) {
	r, ctx := testRepoRegistry(t)
	mustPut(t, ctx, r.st, "repos/acme/ghost/note.json")

	repos, err := r.Repos(ctx, "nobody")
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	checkStrings(t, "Repos(nobody)", repos, []string{})

	// The litter-only owner has no live repo, so it lists nowhere.
	owners, err := r.Owners(ctx)
	if err != nil {
		t.Fatalf("Owners: %v", err)
	}
	checkStrings(t, "Owners", owners, []string{})
}

// Delete removes the repo from the listings; the last repo's delete removes
// the owner too. Re-create after delete succeeds and re-lists (the #200
// re-import case: Exists/Create are manifest-gated, so no 409).
func TestRepoRegistryDeleteVanishesAndRecreates(t *testing.T) {
	r, ctx := testRepoRegistry(t)
	mustCreate(t, r, ctx, "acme/api")
	mustCreate(t, r, ctx, "acme/keep")

	if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "api"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	repos, err := r.Repos(ctx, "acme")
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	checkStrings(t, "Repos(acme) after delete", repos, []string{"keep"})

	if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "keep"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if repos, err := r.Repos(ctx, "acme"); err != nil {
		t.Fatalf("Repos: %v", err)
	} else {
		checkStrings(t, "Repos(acme) after full delete", repos, []string{})
	}
	if owners, err := r.Owners(ctx); err != nil {
		t.Fatalf("Owners: %v", err)
	} else {
		checkStrings(t, "Owners after full delete", owners, []string{})
	}

	// Re-create the same name: no 409, listed again.
	if err := r.Create(ctx, git.RepoId{Owner: "acme", Name: "api"}, git.Sha1); err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
	if repos, err := r.Repos(ctx, "acme"); err != nil {
		t.Fatalf("Repos: %v", err)
	} else {
		checkStrings(t, "Repos(acme) after re-create", repos, []string{"api"})
	}
	if owners, err := r.Owners(ctx); err != nil {
		t.Fatalf("Owners: %v", err)
	} else {
		checkStrings(t, "Owners after re-create", owners, []string{"acme"})
	}
}

// OwnerRepoCounts is the Forgejo #307 count rail: per-owner live-repo counts
// over the same manifest-gated liveRepos walk Owners uses — ghosts (deleted
// litter, unborn fork prefixes) never count, litter-only owners are absent.
func TestRepoRegistryOwnerCounts(t *testing.T) {
	r, ctx := testRepoRegistry(t)
	mustCreate(t, r, ctx, "acme/api")
	mustCreate(t, r, ctx, "acme/keep")
	mustCreate(t, r, ctx, "solo/one")
	mustPut(t, ctx, r.st, "repos/acme/ghost/note.json") // deleted-repo litter: not a repo
	mustPut(t, ctx, r.st, "repos/acme/child/fork.json") // unborn fork child: not a repo yet
	mustPut(t, ctx, r.st, "repos/litter/ghost/note.json")

	counts, err := r.OwnerRepoCounts(ctx)
	if err != nil {
		t.Fatalf("OwnerRepoCounts: %v", err)
	}
	want := map[string]int{"acme": 2, "solo": 1}
	if len(counts) != len(want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
	for o, n := range want {
		if counts[o] != n {
			t.Fatalf("counts = %v, want %v", counts, want)
		}
	}
	// Instance total is the sum over the payload; the litter-only owner is absent.
	sum := 0
	for _, n := range counts {
		sum += n
	}
	if sum != 3 {
		t.Fatalf("instance total = %d, want 3 (%v)", sum, counts)
	}

	// Delete moves the count; deleting the last repo removes the owner —
	// the same membership rule Owners enforces.
	if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "api"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if counts, err = r.OwnerRepoCounts(ctx); err != nil {
		t.Fatalf("OwnerRepoCounts: %v", err)
	} else if counts["acme"] != 1 {
		t.Fatalf("counts after delete = %v", counts)
	}
	if err := r.Delete(ctx, git.RepoId{Owner: "solo", Name: "one"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if counts, err = r.OwnerRepoCounts(ctx); err != nil {
		t.Fatalf("OwnerRepoCounts: %v", err)
	} else if _, ok := counts["solo"]; ok {
		t.Fatalf("solo must vanish with its last repo: %v", counts)
	}
}
