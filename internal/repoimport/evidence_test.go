// evidence_test.go — E11 harness (docs/EVIDENCE.md): the import cost
// model over a PINNED single-pack fixture at two populations (S/M).
// Asserts an ops RANGE (never an exact N — S10) and flatness across
// populations: control-plane ops must not grow with commit count.
package repoimport

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore tallies backend calls by key class (no mocks for the
// layer under test — the real Service runs over it).
type countingStore struct {
	store.ObjectStore
	mu    sync.Mutex
	gets  map[string]int64
	puts  map[string]int64
	lists int64
	putB  map[string]int64
	getB  map[string]int64
	byID  bool
}

func classOf(key string) string {
	switch {
	case strings.HasSuffix(key, "manifest.pb"):
		return "manifest"
	case strings.Contains(key, "/log/"):
		return "log"
	case strings.Contains(key, "checkpoint") || strings.HasSuffix(key, "refs.pb"):
		return "checkpoint+refs"
	case strings.HasSuffix(key, ".pack"):
		return "pack(bulk)"
	case strings.HasSuffix(key, ".idx"):
		return "idx"
	case strings.HasSuffix(key, "import.json"):
		return "import.json"
	case strings.HasSuffix(key, "access.json"):
		return "access.json"
	case strings.HasSuffix(key, "policy.json"):
		return "policy.json"
	default:
		return "other:" + key
	}
}

func newCounting(inner store.ObjectStore) *countingStore {
	return &countingStore{ObjectStore: inner, gets: map[string]int64{}, puts: map[string]int64{}, putB: map[string]int64{}, getB: map[string]int64{}}
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	res, err := c.ObjectStore.Get(ctx, key, opts)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets[classOf(key)]++
	if o, ok := res.(store.Object); ok {
		c.getB[classOf(key)] += o.Meta.Size
	}
	return res, err
}

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	m, err := c.ObjectStore.Head(ctx, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets[classOf(key)+":head"]++
	return m, err
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	m, err := c.ObjectStore.Put(ctx, key, body, opts)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts[classOf(key)]++
	if body.Bytes != nil {
		c.putB[classOf(key)] += int64(len(body.Bytes))
	} else {
		c.putB[classOf(key)] += body.StreamLen
	}
	return m, err
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) snapshot() (gets, puts, lists int64, controlPuts int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cls, n := range c.gets {
		if !strings.HasSuffix(cls, "(bulk)") {
			gets += n
		}
	}
	for cls, n := range c.puts {
		puts += n
		if cls != "pack(bulk)" {
			controlPuts += n
		}
	}
	return gets, puts, c.lists, controlPuts
}

func (c *countingStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = map[string]int64{}
	c.puts = map[string]int64{}
	c.lists = 0
	c.putB = map[string]int64{}
	c.getB = map[string]int64{}
}

// TestEvidenceImportBudget pins the §7 cost model (E11): one import over
// the pinned single-pack layout costs a bounded RANGE of control-plane
// ops (zero LIST), flat across S/M populations; wall time grows with
// pack bytes only. Bulk pack bytes are reported, not budgeted (plan §7:
// "bulk pack bytes excluded by definition").
func TestEvidenceImportBudget(t *testing.T) {
	for _, tc := range []struct {
		name              string
		commits, branches int
		tags              int
	}{
		{name: "S", commits: 50, branches: 2, tags: 2},
		{name: "M", commits: 400, branches: 10, tags: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := t.TempDir() + "/src"
			srcURL := fixtureRepo(t, remote, tc.commits, tc.branches, tc.tags)
			repackSingle(t, remote) // pinned: exactly one source pack
			cfg := testConfig(t)
			cs := newCounting(store.NewMemory())
			svc, _ := testServiceOnStore(t, cfg, realRoles(cs, cfg), cs)
			cs.reset()
			start := time.Now()
			p := fileParams("acme", "ev"+tc.name, srcURL)
			p.importer = "evidence@walhub.test"
			o, err := svc.RunHeadless(context.Background(), p, "", "evidence@walhub.test")
			wall := time.Since(start)
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			gets, puts, lists, controlPuts := cs.snapshot()
			cs.mu.Lock()
			packBytes := cs.putB["pack(bulk)"]
			cs.mu.Unlock()
			t.Logf("%s: %d commits/%d branches/%d tags → %d GETs+HEADs, %d PUTs (%d control), %d LISTs, %d pack bytes, wall %v, refs %d",
				tc.name, tc.commits, tc.branches, tc.tags, gets, puts, controlPuts, lists, packBytes, wall, len(o.HeadSHAs))
			if lists != 0 {
				t.Fatalf("%s: %d LISTs, want 0 (probe, don't list)", tc.name, lists)
			}
			// RANGE assertions (S10: never exact N — CAS retries,
			// checkpoint pairing, and size-class splits add variance):
			// control-plane PUTs: manifest Create + log segment(s) +
			// checkpoint/refs pair + access + import.json + idx ∈ [5,14].
			if controlPuts < 5 || controlPuts > 14 {
				t.Fatalf("%s: control PUTs = %d, want in [5,14]", tc.name, controlPuts)
			}
			// Reads: manifest probe + import.json probe + ref/policy
			// reads + repack reads, all exact-key, ∈ [2,20].
			if gets < 2 || gets > 20 {
				t.Fatalf("%s: GETs = %d, want in [2,20]", tc.name, gets)
			}
			// Imported ref surface matches the fixture.
			if want := tc.branches + tc.tags + 1; len(o.HeadSHAs) != want {
				t.Fatalf("%s: refs = %d, want %d", tc.name, len(o.HeadSHAs), want)
			}
			// Flatness is asserted across populations in TestEvidenceImportFlat.
		})
	}
}

// TestEvidenceImportFlat replays S then M and asserts the control-plane
// op count does not grow with population (flat, not linear).
func TestEvidenceImportFlat(t *testing.T) {
	measure := func(t *testing.T, name string, commits, branches, tags int) (gets, controlPuts int64) {
		t.Helper()
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, commits, branches, tags)
		repackSingle(t, remote)
		cfg := testConfig(t)
		cs := newCounting(store.NewMemory())
		svc, _ := testServiceOnStore(t, cfg, realRoles(cs, cfg), cs)
		cs.reset()
		p := fileParams("acme", name, srcURL)
		p.importer = "evidence@walhub.test"
		if _, err := svc.RunHeadless(context.Background(), p, "", "evidence@walhub.test"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		g, _, _, c := cs.snapshot()
		return g, c
	}
	gS, cS := measure(t, "flatS", 50, 2, 2)
	gM, cM := measure(t, "flatM", 400, 10, 10)
	t.Logf("flatness: S(%d gets, %d control puts) vs M(%d gets, %d control puts)", gS, cS, gM, cM)
	if gM > gS+4 || cM > cS+4 {
		t.Fatalf("not flat: S=(%d,%d) M=(%d,%d)", gS, cS, gM, cM)
	}
}
