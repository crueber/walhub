// serve_registry_test.go — Forgejo issue #200 (deleted repos must vanish
// from /explore): the serving RepoRegistry (Owners/Repos) is manifest-gated.
// A prefix without repos/<o>/<r>/manifest.pb — a deleted repo's sidecar
// litter, or a fork-provisioned-but-unborn prefix — never lists. Table shape
// is deliberately flat: memory store, one registry, real Delete sweep.
package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/pulls"
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

// putJSON seeds one JSON object (fork.json / forks.json stand-ins).
func putJSON(t *testing.T, r *repoRegistry, ctx context.Context, key, raw string) {
	t.Helper()
	if _, err := store.PutBytes(ctx, r.st, key, []byte(raw),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// sweepRecorder captures onChildDelete calls.
type sweepRecorder struct {
	calls [][2]string
}

func (s *sweepRecorder) fn(_ context.Context, child, parent string) {
	s.calls = append(s.calls, [2]string{child, parent})
}

// TestRepoRegistryDeleteSweepsForkProvenance pins issue #457 at the serving
// layer: Delete captures the fork parent pre-wipe (the wipe deletes
// fork.json) and fires onChildDelete post-commit. Plain deletes, corrupt
// fork.json, and a nil callback never fire — and never fail the delete.
func TestRepoRegistryDeleteSweepsForkProvenance(t *testing.T) {
	t.Run("fork child fires sweep", func(t *testing.T) {
		r, ctx := testRepoRegistry(t)
		rec := &sweepRecorder{}
		r.onChildDelete = rec.fn
		mustCreate(t, r, ctx, "acme/api")
		mustCreate(t, r, ctx, "acme/child")
		putJSON(t, r, ctx, "repos/acme/child/fork.json", `{"parent":"acme/api","forked_at":"2026-09-04T12:00:00Z","version":1}`)
		if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "child"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(rec.calls) != 1 || rec.calls[0] != [2]string{"acme/child", "acme/api"} {
			t.Fatalf("sweep calls = %v", rec.calls)
		}
		repos, err := r.Repos(ctx, "acme")
		if err != nil {
			t.Fatalf("Repos: %v", err)
		}
		checkStrings(t, "Repos(acme) after child delete", repos, []string{"api"})
	})

	t.Run("plain repo is silent", func(t *testing.T) {
		r, ctx := testRepoRegistry(t)
		rec := &sweepRecorder{}
		r.onChildDelete = rec.fn
		mustCreate(t, r, ctx, "acme/solo")
		if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "solo"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("sweep calls = %v", rec.calls)
		}
	})

	t.Run("corrupt fork.json is silent but deletes", func(t *testing.T) {
		r, ctx := testRepoRegistry(t)
		rec := &sweepRecorder{}
		r.onChildDelete = rec.fn
		mustCreate(t, r, ctx, "acme/bad")
		putJSON(t, r, ctx, "repos/acme/bad/fork.json", "{oops")
		if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "bad"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("sweep calls = %v", rec.calls)
		}
		repos, _ := r.Repos(ctx, "acme")
		checkStrings(t, "Repos(acme)", repos, []string{})
	})

	t.Run("nil callback is safe", func(t *testing.T) {
		r, ctx := testRepoRegistry(t)
		mustCreate(t, r, ctx, "acme/kid")
		putJSON(t, r, ctx, "repos/acme/kid/fork.json", `{"parent":"acme/api","forked_at":"2026-09-04T12:00:00Z","version":1}`)
		if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "kid"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("litter name still sweeps", func(t *testing.T) {
		r, ctx := testRepoRegistry(t)
		rec := &sweepRecorder{}
		r.onChildDelete = rec.fn
		// No manifest: a stale fork.json from a half-provisioned child.
		putJSON(t, r, ctx, "repos/acme/ghost/fork.json", `{"parent":"acme/api","forked_at":"2026-09-04T12:00:00Z","version":1}`)
		if err := r.Delete(ctx, git.RepoId{Owner: "acme", Name: "ghost"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(rec.calls) != 1 || rec.calls[0] != [2]string{"acme/ghost", "acme/api"} {
			t.Fatalf("sweep calls = %v", rec.calls)
		}
	})
}

// fakeForksSeam records DecForks through the pulls.ForksCounter seam.
type fakeForksSeam struct {
	mu     sync.Mutex
	decs   [][2]string
	decErr error
}

func (f *fakeForksSeam) IncForks(_ context.Context, _, _ string) error { return nil }

func (f *fakeForksSeam) DecForks(_ context.Context, owner, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decs = append(f.decs, [2]string{owner, repo})
	return f.decErr
}

func (f *fakeForksSeam) decCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.decs)
}

// TestForkDeleteSweepTable pins the forkDeleteSweep wiring (the
// orgBirthObserver precedent — direct, never via a second buildCollab):
// the deleted child is unlisted from the parent index, and the social
// counter decrements exactly when a row was removed. Every shortfall is a
// silent no-op (best-effort — the delete already linearized).
func TestForkDeleteSweepTable(t *testing.T) {
	setup := func(t *testing.T, index string) (store.ObjectStore, *pulls.Service, *fakeForksSeam) {
		t.Helper()
		st := store.NewMemory()
		svc := pulls.New(st, nil)
		fc := &fakeForksSeam{}
		svc.Forks = fc
		if index != "" {
			if _, err := store.PutBytes(context.Background(), st, pulls.ForksKey("o", "r"), []byte(index),
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
				t.Fatal(err)
			}
		}
		return st, svc, fc
	}
	readRaw := func(t *testing.T, st store.ObjectStore) string {
		t.Helper()
		raw, _, err := store.GetBytes(context.Background(), st, pulls.ForksKey("o", "r"), store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				return "ABSENT"
			}
			t.Fatal(err)
		}
		return string(raw)
	}

	t.Run("row removed and counter decremented", func(t *testing.T) {
		st, svc, fc := setup(t, `{"version":1,"forks":[{"repo":"f/c","forked_at":"2026-09-04T12:00:00Z"}]}`)
		forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
		if got := readRaw(t, st); !strings.Contains(got, `"forks":[]`) {
			t.Fatalf("index = %q", got)
		}
		if fc.decCount() != 1 || fc.decs[0] != [2]string{"o", "r"} {
			t.Fatalf("decs = %v", fc.decs)
		}
	})

	t.Run("missing row skips counter", func(t *testing.T) {
		st, svc, fc := setup(t, `{"version":1,"forks":[{"repo":"f/other","forked_at":"2026-09-04T12:00:00Z"}]}`)
		before := readRaw(t, st)
		forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
		if got := readRaw(t, st); got != before {
			t.Fatalf("index moved: %q → %q", before, got)
		}
		if fc.decCount() != 0 {
			t.Fatalf("decs = %v", fc.decs)
		}
	})

	t.Run("absent index skips counter", func(t *testing.T) {
		_, svc, fc := setup(t, "")
		forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
		if fc.decCount() != 0 {
			t.Fatalf("decs = %v", fc.decs)
		}
	})

	t.Run("corrupt index skips counter", func(t *testing.T) {
		_, svc, fc := setup(t, "{oops")
		forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
		if fc.decCount() != 0 {
			t.Fatalf("decs = %v", fc.decs)
		}
	})

	t.Run("bad parent is a no-op", func(t *testing.T) {
		st, svc, fc := setup(t, `{"version":1,"forks":[]}`)
		forkDeleteSweep(svc)(context.Background(), "f/c", "oops")
		if got := readRaw(t, st); !strings.Contains(got, `"version":1`) {
			t.Fatalf("index = %q", got)
		}
		if fc.decCount() != 0 {
			t.Fatalf("decs = %v", fc.decs)
		}
	})

	t.Run("nil service is safe", func(t *testing.T) {
		forkDeleteSweep(nil)(context.Background(), "f/c", "o/r")
	})

	t.Run("nil seam still unlists", func(t *testing.T) {
		st, svc, _ := setup(t, `{"version":1,"forks":[{"repo":"f/c","forked_at":"2026-09-04T12:00:00Z"}]}`)
		svc.Forks = nil
		forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
		if got := readRaw(t, st); !strings.Contains(got, `"forks":[]`) {
			t.Fatalf("index = %q", got)
		}
	})

	t.Run("counter shortfall stays silent", func(t *testing.T) {
		st, svc, fc := setup(t, `{"version":1,"forks":[{"repo":"f/c","forked_at":"2026-09-04T12:00:00Z"}]}`)
		fc.decErr = errors.New("boom")
		forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
		if got := readRaw(t, st); !strings.Contains(got, `"forks":[]`) {
			t.Fatalf("index = %q", got)
		}
		if fc.decCount() != 1 {
			t.Fatalf("decs = %d, want 1", fc.decCount())
		}
	})

	t.Run("concurrent double sweep decrements once", func(t *testing.T) {
		// Two concurrent deletes of the same child both reach the sweep
		// (Registry.Delete is idempotent-success). Exactly one UnlistFork
		// lands the row removal, so exactly one DecForks must fire — a
		// stale per-attempt flag would double-decrement here.
		st, svc, fc := setup(t, `{"version":1,"forks":[{"repo":"f/c","forked_at":"2026-09-04T12:00:00Z"}]}`)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				forkDeleteSweep(svc)(context.Background(), "f/c", "o/r")
			}()
		}
		close(start)
		wg.Wait()
		if got := readRaw(t, st); !strings.Contains(got, `"forks":[]`) {
			t.Fatalf("index = %q", got)
		}
		if fc.decCount() != 1 {
			t.Fatalf("decs = %d, want exactly 1", fc.decCount())
		}
	})
}
