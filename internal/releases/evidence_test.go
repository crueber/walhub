package releases

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// countingStore tallies backend round trips (the E8 evidence harness
// counts the real code path over the memory store — algorithmic shape,
// not network RTT).
type countingStore struct {
	store.ObjectStore
	gets, puts, lists, deletes, heads int
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.gets++
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.puts++
	return c.ObjectStore.Put(ctx, key, body, opts)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	c.lists++
	return c.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	c.deletes++
	return c.ObjectStore.Delete(ctx, key, v)
}

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	c.heads++
	return c.ObjectStore.Head(ctx, key)
}

func (c *countingStore) reset() { c.gets, c.puts, c.lists, c.deletes, c.heads = 0, 0, 0, 0, 0 }

func (c *countingStore) String() string {
	return fmt.Sprintf("GETs=%d PUTs=%d LISTs=%d DELETEs=%d HEADs=%d", c.gets, c.puts, c.lists, c.deletes, c.heads)
}

// spoolLeftovers counts files left in the spool dir (streaming proof:
// uploads stage to disk and clean up; memory holds no copy).
func spoolLeftovers(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	return len(ents), nil
}

func evidenceHarness(t *testing.T, nReleases int) (*harness, *countingStore) {
	t.Helper()
	x := newHarness(t)
	cs := &countingStore{ObjectStore: x.svc.Store}
	x.svc.Store = cs
	grantWrite(x)
	for i := 1; i <= nReleases; i++ {
		tag := fmt.Sprintf("v%d", i)
		x.git.tags[tag] = strings.Repeat("a", 39) + string("0123456789"[i%10])
		x.svc.Now = func() time.Time { return x.now.Add(time.Duration(i) * time.Minute) }
		mustPut(t, x, writer(), tag, ReleaseInput{})
	}
	x.svc.Now = func() time.Time { return x.now }
	cs.reset()
	return x, cs
}

func TestEvidenceLatestHotRead(t *testing.T) {
	for _, n := range []int{5, 200} {
		x, cs := evidenceHarness(t, n)
		got, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
		if err != nil || got.Tag != fmt.Sprintf("v%d", n) {
			t.Fatalf("n=%d: %+v %v", n, got, err)
		}
		t.Logf("latest hot read at %d releases: %s", n, cs)
		if cs.lists != 0 || cs.gets != 2 {
			t.Fatalf("n=%d: hot read must be 2 GETs, 0 LIST: %s", n, cs)
		}
	}
}

func TestEvidencePublishCost(t *testing.T) {
	x, cs := evidenceHarness(t, 5)
	x.git.tags["v6"] = strings.Repeat("b", 40)
	x.svc.Now = func() time.Time { return x.now.Add(time.Hour) }
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v6", ReleaseInput{}, ""); err != nil {
		t.Fatal(err)
	}
	t.Logf("publish (create-published + monotonic pointer CAS): %s", cs)
	if cs.gets != 3 || cs.puts != 2 || cs.lists != 0 {
		t.Fatalf("publish budget: %s", cs)
	}
}

func TestEvidenceListCost(t *testing.T) {
	for _, n := range []int{5, 200} {
		x, cs := evidenceHarness(t, n)
		rels, more, err := x.svc.ListReleases(ctx(), "o", "r", writer(), 50, "")
		wantLen, wantMore := n, false
		if n > 50 {
			wantLen, wantMore = 50, true
		}
		if err != nil || len(rels) != wantLen || more != wantMore {
			t.Fatalf("n=%d: %d %v %v", n, len(rels), more, err)
		}
		t.Logf("list page (n=50) at %d releases: %s", n, cs)
		if cs.lists != 1 || cs.gets != n {
			t.Fatalf("n=%d: list must be 1 LIST + %d header GETs: %s", n, n, cs)
		}
	}
}

func TestEvidenceAutodraftCost(t *testing.T) {
	for _, n := range []int{5, 50} {
		x, cs := evidenceHarness(t, 0)
		x.git.tags["v9"] = strings.Repeat("9", 40)
		var cards []map[string]any
		for i := 1; i <= n; i++ {
			cards = append(cards, prCard(i, titleN(i), "amy"))
			merge := mergeSHAN(i)
			seedPR(t, x, i, titleN(i), "amy", true, merge, "2026-09-01T10:00:00Z")
			x.git.ancestors[merge+"\x00"+strings.Repeat("9", 40)] = true
		}
		seedIndex(t, x, cards...)
		cs.reset()
		before := x.git.ancestorN
		ad, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v9", "")
		if err != nil || len(ad.PRs) != n {
			t.Fatalf("n=%d: %+v %v", n, ad, err)
		}
		probes := x.git.ancestorN - before
		t.Logf("autodraft at %d merged PRs: %s git-probes=%d", n, cs, probes)
		if probes != n {
			t.Fatalf("n=%d: one ancestry probe per candidate: %d", n, probes)
		}
		if cs.lists != 0 {
			t.Fatalf("n=%d: autodraft must not LIST: %s", n, cs)
		}
	}
}

func TestEvidenceAssetStreamingCap(t *testing.T) {
	x, cs := evidenceHarness(t, 0)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	cs.reset()
	// 1 MiB upload through a 4 MiB cap: bytes Create + header CAS only.
	body := bytes.Repeat([]byte("b"), 1<<20)
	e, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "big",
		bytes.NewReader(body), int64(len(body)), shaOf(body), "")
	if err != nil || e.Size != int64(len(body)) {
		t.Fatalf("upload: %+v %v", e, err)
	}
	t.Logf("1MiB asset upload: %s", cs)
	if cs.gets != 2 || cs.puts != 2 {
		t.Fatalf("upload budget (header probe + bytes Create + header CAS): %s", cs)
	}
	// The spool directory is empty afterwards (streamed, never buffered).
	left, derr := spoolLeftovers(x.spool)
	if derr != nil || left != 0 {
		t.Fatalf("spool leftovers: %d %v", left, derr)
	}
	// Over-cap upload 413s before any store write (declared and streamed).
	cs.reset()
	x.svc.MaxAssetBytes = 16
	huge := bytes.Repeat([]byte("h"), 32)
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "huge",
		bytes.NewReader(huge), int64(len(huge)), shaOf(huge), ""); !isErr(err, ErrTooLarge) {
		t.Fatalf("declared over cap: %v", err)
	}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "huge",
		bytes.NewReader(huge), 10, shaOf(huge), ""); !isErr(err, ErrTooLarge) {
		t.Fatalf("streamed over cap: %v", err)
	}
	if cs.puts != 0 {
		t.Fatalf("rejected upload must not write: %s", cs)
	}
}
