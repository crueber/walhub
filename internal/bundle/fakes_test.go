package bundle

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Test fakes (t.TempDir-friendly, no I/O beyond the memory store).

// fakeWal implements WalView over a fixed as-of table: for each slot instant,
// the ref tips + seq the WAL would fold to. Any other time errors (no-state).
type fakeWal struct {
	asOf   map[time.Time]fold // RefsAsOf lookups
	atSeq  map[uint64]fold    // RefsAtSeq lookups
	first  map[string]time.Time
	noHead bool // FirstStateAt reports absent
}

type fold struct {
	refs Refs
	seq  uint64
}

func (f *fakeWal) RefsAsOf(ctx context.Context, repo string, at time.Time) (Refs, uint64, error) {
	fd, ok := f.asOf[at]
	if !ok {
		return nil, 0, fmt.Errorf("wal: no state as of %s", at.Format(time.RFC3339))
	}
	return fd.refs, fd.seq, nil
}

func (f *fakeWal) RefsAtSeq(ctx context.Context, repo string, seq uint64) (Refs, error) {
	fd, ok := f.atSeq[seq]
	if !ok {
		return nil, fmt.Errorf("wal: no refs at seq %d", seq)
	}
	return fd.refs, nil
}

func (f *fakeWal) FirstStateAt(repo string) (time.Time, bool) {
	if f.noHead {
		return time.Time{}, false
	}
	t, ok := f.first[repo]
	return t, ok
}

// fakePrim implements Primitives with a canned commit count (gate math) and
// trivial pack/bundle bodies.
type fakePrim struct {
	counts []int // CountCommits returns counts[i] for the i-th call, last repeats
	calls  int
	deltas []string // recorded want/exclude/filter requests
}

func (p *fakePrim) BundleCreate(ctx context.Context, repoDir, outPath string, refs []string) (int64, int64, error) {
	return int64(len(refs) * 100), 12, nil
}

func (p *fakePrim) PackDelta(ctx context.Context, repoDir string, wants, excludes []string, filter string, w io.Writer) error {
	p.deltas = append(p.deltas, fmt.Sprintf("wants=%v excludes=%v filter=%q", wants, excludes, filter))
	_, _ = w.Write([]byte("PACKBYTES"))
	return nil
}

func (p *fakePrim) CountCommits(ctx context.Context, repoDir string, tips, notTips []string) (int, error) {
	p.calls++
	if p.calls-1 < len(p.counts) {
		return p.counts[p.calls-1], nil
	}
	return p.counts[len(p.counts)-1], nil
}

// fakeTasks implements TaskRunner inline.
type fakeTasks struct {
	ran   []string
	noted []string
}

func (f *fakeTasks) RunBundle(ctx context.Context, repo string, params map[string]string, fn func(ctx context.Context, tr Reporter) error) error {
	f.ran = append(f.ran, repo+"/"+params["strategy"]+"@"+params["slot"])
	return fn(ctx, reporterFunc(func(s string) { f.noted = append(f.noted, s) }))
}

type reporterFunc func(string)

func (f reporterFunc) Notice(text string)                           { f(text) }
func (f reporterFunc) Progress(label string, d, t uint64, u string) {}

// epoch helpers for the worked week (§8.6): all 00:00:00 UTC.
func epoch(t *testing.T, s string) uint64 {
	t.Helper()
	return uint64(at(s).Unix())
}

// entry is a small builder for BundleEntry test fixtures.
func entry(strategy, id string, kind string, slot, token, seq uint64, tips ...string) *proto.BundleEntry {
	e := &proto.BundleEntry{
		ID:            id,
		Key:           fmt.Sprintf("bundles/%s/%011d-abc.bundle", strategy, slot),
		Strategy:      strategy,
		Kind:          kind,
		CreationToken: token,
		Seq:           seq,
		Slot:          slot,
	}
	for _, name := range []string{"HEAD", "refs/heads/main"} {
		e.Tips = append(e.Tips, &proto.Ref{Name: name, Oid: oidFor(id + name)})
	}
	return e
}

// oidFor derives a deterministic 40-hex oid from a seed.
func oidFor(seed string) string {
	h := sha1Hex(seed)
	for len(h) < 40 {
		h += sha1Hex(seed + "x")
	}
	return h[:40]
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func tipRefs(oid string) Refs {
	return Refs{
		{Name: "HEAD", Oid: oid},
		{Name: "refs/heads/main", Oid: oid},
	}
}

func tipsPtrs(refs Refs) []*proto.Ref {
	out := make([]*proto.Ref, 0, len(refs))
	for i := range refs {
		out = append(out, &refs[i])
	}
	return out
}

// protoBundleList builds a BundleList fixture (§8.11 defaults).
func protoBundleList(entries ...*proto.BundleEntry) *proto.BundleList {
	return &proto.BundleList{Mode: "all", Heuristic: "creationToken", Bundles: entries}
}

func newMemStore(t *testing.T) store.ObjectStore {
	t.Helper()
	return store.NewMemory()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// storeGetList reads bundles/list.pb back from a store (test helper).
func storeGetList(t *testing.T, st store.ObjectStore) (*proto.BundleList, string, error) {
	t.Helper()
	body, meta, err := store.GetBytes(context.Background(), st, BundleListKey, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return &proto.BundleList{}, "", nil
		}
		return nil, "", err
	}
	l, err := proto.UnmarshalBundleList(body)
	if err != nil {
		return nil, "", err
	}
	return l, string(meta.Version), nil
}
