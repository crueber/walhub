package identity

import (
	"context"
	"sync/atomic"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// countStore counts Get calls by key prefix class.
type countStore struct {
	store.ObjectStore
	gets   atomic.Int64
	cond   atomic.Int64 // conditional (IfNoneMatch set)
	full   atomic.Int64 // unconditional bodies
	nm     atomic.Int64 // NotModified answers
	puts   atomic.Int64
	bodies atomic.Int64 // full bodies returned
}

func (c *countStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.gets.Add(1)
	if opts.IfNoneMatch != "" {
		c.cond.Add(1)
	} else {
		c.full.Add(1)
	}
	res, err := c.ObjectStore.Get(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	if _, ok := res.(store.NotModified); ok {
		c.nm.Add(1)
	} else {
		c.bodies.Add(1)
	}
	return res, nil
}

func (c *countStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.puts.Add(1)
	return c.ObjectStore.Put(ctx, key, body, opts)
}

// TestAuthzReadPath is the E2 evidence harness (docs/EVIDENCE.md): the
// authz read path costs one conditional GET of access.json per read request
// plus one conditional GET per referenced team, and the version-stamped LRU
// turns steady-state re-reads into NotModified probes (no body transfer).
// Query count is O(1) per request: the binding list is bounded (validated,
// sorted, human-rate writes) and every probe is an exact key — no LIST.
func TestAuthzReadPath(t *testing.T) {
	mem := store.NewMemory()
	cs := &countStore{ObjectStore: mem}
	s := New(cs, config.Defaults())
	s.Now = testClock
	ctx := context.Background()

	seedOrg(t, s)
	mustAccess(t, s, "acme", "repo", VisibilityPrivate, []AccessBinding{
		{Subject: "user:carol@example.com", Role: RoleTriage},
		{Subject: "team:acme/platform", Role: RoleWrite},
		{Subject: "team:acme/empty", Role: RoleRead},
	})
	if _, err := s.CreateTeam(ctx, "acme", "empty", "E", ""); err != nil {
		t.Fatal(err)
	}

	teamy := authPrincipal("teamy@example.com")
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "teamy@example.com"); err != nil {
		t.Fatal(err)
	}

	// Cold read: access.json + 2 team probes (empty team object included).
	cs.gets.Store(0)
	cs.bodies.Store(0)
	role, _ := s.Resolve(ctx, "acme", "repo", teamy)
	if role != RoleWrite {
		t.Fatalf("role = %q", role)
	}
	if got := cs.gets.Load(); got != 3 {
		t.Fatalf("cold resolve = %d GETs, want 3 (access + 2 teams)", got)
	}
	if got := cs.bodies.Load(); got != 3 {
		t.Fatalf("cold resolve transferred %d bodies, want 3", got)
	}

	// Warm read: same request shape — 3 conditional GETs, all NotModified,
	// zero bodies. The per-request cost is constant regardless of how many
	// teams/orgs exist in the bucket.
	cs.gets.Store(0)
	cs.bodies.Store(0)
	cs.nm.Store(0)
	for i := 0; i < 50; i++ {
		if role, _ := s.Resolve(ctx, "acme", "repo", teamy); role != RoleWrite {
			t.Fatalf("warm role = %q", role)
		}
	}
	if got := cs.gets.Load(); got != 150 {
		t.Fatalf("warm resolves = %d GETs, want 150", got)
	}
	if got := cs.bodies.Load(); got != 0 {
		t.Fatalf("warm resolves transferred %d bodies, want 0", got)
	}
	if got := cs.nm.Load(); got != 150 {
		t.Fatalf("warm resolves NotModified = %d, want 150", got)
	}
	hits, misses := s.access.stats()
	t.Logf("access LRU hits=%d misses=%d", hits, misses)
	if hits == 0 || misses == 0 {
		t.Fatalf("LRU must show both hits and misses, got %d/%d", hits, misses)
	}

	// A binding change invalidates lazily: the next conditional GET sees the
	// new version (one body), then steady state resumes.
	if _, err := s.SetTeamMember(ctx, "acme", "empty", "teamy@example.com"); err != nil {
		t.Fatal(err)
	}
	cs.gets.Store(0)
	cs.bodies.Store(0)
	if role, _ := s.Resolve(ctx, "acme", "repo", teamy); role != RoleWrite {
		t.Fatalf("post-change role = %q", role)
	}
	if got := cs.gets.Load(); got != 3 {
		t.Fatalf("post-change resolve = %d GETs, want 3", got)
	}
	if got := cs.bodies.Load(); got != 1 {
		t.Fatalf("post-change resolve transferred %d bodies, want 1 (the changed team)", got)
	}

	// Anonymous public read: one access.json probe, no team reads.
	mustAccess(t, s, "acme", "pub", VisibilityPublic, []AccessBinding{
		{Subject: "team:acme/platform", Role: RoleWrite},
	})
	cs.gets.Store(0)
	if aerr := s.CheckRead(ctx, "acme", "pub", auth.Anonymous()); aerr != nil {
		t.Fatalf("anon public: %v", aerr)
	}
	if got := cs.gets.Load(); got != 1 {
		t.Fatalf("anon public read = %d GETs, want 1", got)
	}
}
