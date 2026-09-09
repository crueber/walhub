// push_budget_test.go — Feature 09 (docs/features/09_rollout.md §5 invariant
// 3): the push fast path gains ZERO bucket round trips from the
// collaboration layer — with the ONE Forgejo #240 exception: the
// pull-only refusal revalidates every push with exactly one exact-key
// mirror.json probe (404-free class, law 4 probe-don't-list; "every
// read revalidates" — a push to a repo that just became a mirror must
// be refused). The probe is bounded below (one per discovery + one per
// funnel call); every other collaboration family stays at zero. It
// boots the SHIPPED composition (buildCollab — the same wiring
// serveHTTP uses, all feature packages mounted plus the identity
// require_read gate and the mirror guard) over a prefix-counting
// store, pushes twice through the real smart-HTTP receive-pack path
// with the real git binary, and asserts the bound. The totals are
// logged for the EVIDENCE.md entries (E10 + the #240 entry).
package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// countingStore decorates an ObjectStore and counts every bucket round
// trip by key. The push path must never touch isCollabKey keys.
type countingStore struct {
	store.ObjectStore
	mu   sync.Mutex
	ops  map[string]int // op -> count
	coll map[string]int // collab key -> count
}

func (c *countingStore) count(op, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ops == nil {
		c.ops = map[string]int{}
	}
	if c.coll == nil {
		c.coll = map[string]int{}
	}
	c.ops[op]++
	if isCollabKey(key) {
		c.coll[op+" "+key]++
	}
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.count("get", key)
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	c.count("head", key)
	return c.ObjectStore.Head(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.count("put", key)
	return c.ObjectStore.Put(ctx, key, body, opts)
}

func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	c.count("delete", key)
	return c.ObjectStore.Delete(ctx, key, v)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	c.count("list", prefix)
	return c.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	c.count("listprefixes", prefix)
	return c.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

func (c *countingStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	c.count("compose", dst)
	for _, s := range sources {
		c.count("compose-src", s)
	}
	return c.ObjectStore.Compose(ctx, dst, sources, opts)
}

func (c *countingStore) collabOps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.coll {
		n += v
	}
	return n
}

// snapshot returns (total ops, collab ops) counted so far.
func (c *countingStore) snapshot() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total, collab := 0, 0
	for _, v := range c.ops {
		total += v
	}
	for _, v := range c.coll {
		collab += v
	}
	return total, collab
}

// isCollabKey reports the collaboration key families (docs/features/README
// P1): orgs/*, users/*, and the repo-collab families under repos/<o>/<r>.
// policy.json is deliberately NOT collab: the push rule language predates
// the layer and its push-enforceable effects evaluate on the push path by
// design (09 §4 touch point 2). Git families (manifest.pb, log/, packs,
// refs, checkpoints, bundles) return false.
func isCollabKey(key string) bool {
	if strings.HasPrefix(key, "orgs/") || strings.HasPrefix(key, "users/") {
		return true
	}
	for _, fam := range []string{
		"/access.json", "/meta/", "/issues/", "/pulls/",
		"/checks/", "/releases/", "/collab-events/", "/webhooks/",
		"/fork.json",
	} {
		if strings.Contains(key, fam) {
			return true
		}
	}
	return false
}

// budgetGit runs the real git binary with an isolated config (never the
// user's) and fails the test on error.
func budgetGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	sandbox := t.TempDir()
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+sandbox,
		"GIT_CONFIG_GLOBAL="+filepath.Join(sandbox, "gitconfig"),
		"GIT_AUTHOR_NAME=Budget",
		"GIT_AUTHOR_EMAIL=budget@example.com",
		"GIT_COMMITTER_NAME=Budget",
		"GIT_COMMITTER_EMAIL=budget@example.com",
	)
	// Deterministic identity without touching any config file.
	full := append([]string{"-c", "user.name=Budget", "-c", "user.email=budget@example.com"}, args...)
	cmd.Args = append([]string{"git"}, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPushFastPathZeroCollabRoundTrips(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	cfg := config.FirstRunDefaults(dataDir)
	cfg.Store.Backend = "memory" // count shapes, not network RTT
	cfg.Cache.Dir = filepath.Join(dataDir, "cache")

	cs := &countingStore{ObjectStore: store.NewMemory()}
	reg := wal.NewRegistry(ctx, cs, cfg)
	defer reg.Close()
	engine := server.NewWalEngine(reg, cfg)
	env := api.NewEnv(cs, &repoRegistry{reg: reg, st: cs}, cfg, engine, "test", "test")

	// The shipped composition: every feature package mounted, require_read
	// gate wired, mirror guard wired — the push below runs through all of it.
	collab := buildCollab(cs, cfg, reg, env)
	srv := server.New(server.Options{
		Config:    cfg,
		Store:     cs,
		Engine:    engine,
		API:       server.NewAPIProvider(env),
		DataDir:   dataDir,
		CacheRoot: cfg.Cache.Dir,
		Boot:      server.BootState{Mode: "defaults"},
		ReadGate:  readGateOf(collab.ident),
		// Forgejo #240: the production refusal predicate (same store).
		// Each push revalidates it (probe, don't cache — a repo that
		// just became a mirror must refuse the next push).
		MirrorGuard: mirrorGuardOf(cs),
	})
	chainCollab(srv, collab)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	budgetGit(t, work, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hello budget\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	budgetGit(t, work, "add", "hello.txt")
	budgetGit(t, work, "commit", "-m", "first")
	url := ts.URL + "/e2e/budget.git"
	budgetGit(t, work, "remote", "add", "origin", url)

	// Cold push (repo auto-create) + warm push (existing repo): both must
	// keep the collaboration families untouched.
	budgetGit(t, work, "push", "-u", "origin", "main")
	coldOps, coldCollab := cs.snapshot()
	if err := os.WriteFile(filepath.Join(work, "second.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	budgetGit(t, work, "add", "second.txt")
	budgetGit(t, work, "commit", "-m", "second")
	budgetGit(t, work, "push", "origin", "main")
	warmOps, warmCollab := cs.snapshot()

	t.Logf("cold push (auto-create): %d bucket ops, %d collab", coldOps, coldCollab)
	t.Logf("warm push (existing):    %d bucket ops, %d collab", warmOps-coldOps, warmCollab-coldCollab)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for op, n := range cs.ops {
		t.Logf("store op %-12s %d", op, n)
	}
	t.Logf("total bucket round trips for 2 pushes: %d", warmOps)
	// Forgejo #240 bound: the ONLY collab touches allowed are the
	// mirror-refusal probes (exact-key get/head on meta/mirror.json —
	// one per discovery + one per funnel call). Everything else stays
	// at zero; the probe count itself is bounded (2 pushes × 2 + slack
	// for protocol revalidation — never a LIST, never a write).
	mirrorProbes := 0
	for k, n := range cs.coll {
		if isMirrorProbe(k) {
			mirrorProbes += n
			continue
		}
		t.Errorf("collab key touched on push fast path: %s x%d", k, n)
	}
	if mirrorProbes > 6 {
		t.Errorf("mirror refusal probes = %d, want ≤ 6 (1 discovery + 1 funnel per push + slack)", mirrorProbes)
	}
	if warmOps == 0 {
		t.Fatal("no store ops counted — the decorator is bypassed, measurement void")
	}
}

// isMirrorProbe reports the Forgejo #240 refusal probe (the one
// sanctioned collab touch on the push path): an exact-key read of the
// mirror sidecar. Writes to it (or any other op shape) are NOT covered.
func isMirrorProbe(opKey string) bool {
	op, key, ok := strings.Cut(opKey, " ")
	if !ok {
		return false
	}
	if op != "get" && op != "head" {
		return false
	}
	return strings.HasSuffix(key, "/meta/mirror.json")
}
