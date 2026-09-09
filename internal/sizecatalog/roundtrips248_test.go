// roundtrips248_test.go — Forgejo #248 EVIDENCE E16 harness (committed):
// counts store ops for one sweep pass + one detailed listing at two
// populations over the memory store (algorithmic shape, not network RTT).
package sizecatalog

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// countingStore decorates an ObjectStore with per-op counters.
type countingStore struct {
	store.ObjectStore
	mu      sync.Mutex
	gets    int
	puts    int
	heads   int
	lists   int
	deletes int
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.ObjectStore.Get(ctx, key, opts)
}
func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.ObjectStore.Put(ctx, key, body, opts)
}
func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	c.mu.Lock()
	c.heads++
	c.mu.Unlock()
	return c.ObjectStore.Head(ctx, key)
}
func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	c.mu.Lock()
	c.deletes++
	c.mu.Unlock()
	return c.ObjectStore.Delete(ctx, key, v)
}
func (c *countingStore) snapshot() (g, p, h, l, d int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.puts, c.heads, c.lists, c.deletes
}

func seedRepos(t *testing.T, ctx context.Context, st store.ObjectStore, owner string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s/r%02d", owner, i)
		m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, ObjectFormat: "sha1", HeadSeq: uint64(i + 1), Revision: 1,
			Packs: []*proto.PackRef{{Checksum: fmt.Sprintf("p%02d", i), PackSize: uint64(1000 * (i + 1)), IdxSize: 100, ObjectCount: uint64(i + 1)}}}
		if _, err := st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestSweepRoundTrips is the E16 measurement harness.
func TestSweepRoundTrips(t *testing.T) {
	for _, n := range []int{3, 12} {
		ctx := context.Background()
		base := store.NewMemory()
		cs := &countingStore{ObjectStore: base}
		// Seed through the UNWRAPPED store: the snapshot below must measure
		// the sweep only (seed PUTs are setup, not fold cost).
		ids := seedRepos(t, ctx, base, "o", n)
		fixed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
		res, err := Sweep(ctx, cs, SweepOptions{
			Now: func() time.Time { return fixed },
			ListRepos: func(context.Context) ([]string, error) {
				return append([]string{}, ids...), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		g, p, h, l, d := cs.snapshot()
		t.Logf("E16 sweep n=%d: folded=%d gets=%d puts=%d heads=%d lists=%d deletes=%d", n, res.Folded, g, p, h, l, d)
		// Exact fold cost (cold sidecars): 2 GETs (manifest probe + sidecar
		// probe) per repo + 1 catalog GET; n sidecar PUTs + 1 catalog CAS.
		if res.Folded != n {
			t.Fatalf("cold sweep must fold all %d repos, folded=%d", n, res.Folded)
		}
		if wantG := 2*n + 1; g != wantG {
			t.Fatalf("sweep GETs = %d, want %d (2/repo + 1 catalog)", g, wantG)
		}
		if wantP := n + 1; p != wantP {
			t.Fatalf("sweep PUTs = %d, want %d (%d sidecars + 1 catalog CAS)", p, wantP, n)
		}
		if h != 0 {
			t.Fatalf("sweep must probe (GET), never HEAD, got %d heads", h)
		}
		// Steady state: a second pass finds every sidecar current
		// (PUT-if-changed) — only the 1 catalog CAS remains.
		cs3 := &countingStore{ObjectStore: base}
		res2, err := Sweep(ctx, cs3, SweepOptions{
			Now: func() time.Time { return fixed },
			ListRepos: func(context.Context) ([]string, error) {
				return append([]string{}, ids...), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		g3, p3, _, l3, d3 := cs3.snapshot()
		t.Logf("E16 sweep steady n=%d: folded=%d unchanged=%d gets=%d puts=%d", n, res2.Folded, res2.Unchanged, g3, p3)
		if res2.Folded != 0 || res2.Unchanged != n {
			t.Fatalf("steady sweep: folded=%d unchanged=%d, want 0/%d", res2.Folded, res2.Unchanged, n)
		}
		if g3 != 2*n+1 || p3 != 1 {
			t.Fatalf("steady sweep: gets=%d puts=%d, want %d/1", g3, p3, 2*n+1)
		}
		// Listing query: exactly ONE catalog GET regardless of repo count.
		cs2 := &countingStore{ObjectStore: base}
		if _, err := ReadCatalog(ctx, cs2); err != nil {
			t.Fatal(err)
		}
		g2, _, _, _, _ := cs2.snapshot()
		t.Logf("E16 listing n=%d: catalog_gets=%d", n, g2)
		if g2 != 1 {
			t.Fatalf("listing must cost exactly 1 catalog GET, got %d", g2)
		}
		if l != 0 || d != 0 || l3 != 0 || d3 != 0 {
			t.Fatalf("sweep must issue zero LIST/DELETE, got lists=%d/%d deletes=%d/%d", l, l3, d, d3)
		}
	}
}
