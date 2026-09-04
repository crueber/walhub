package social

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestStarUnstarIdempotent(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	// Anonymous star → 401.
	if _, err := x.svc.Star(ctx(), auth.Anonymous(), "o", "r"); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon star: %v", err)
	}
	// First star → 1.
	n, err := x.svc.Star(ctx(), jane(), "o", "r")
	if err != nil || n != 1 {
		t.Fatalf("star: %d %v", n, err)
	}
	// Repeat → same state, no double count.
	n, err = x.svc.Star(ctx(), jane(), "o", "r")
	if err != nil || n != 1 {
		t.Fatalf("restar: %d %v", n, err)
	}
	// Second principal → 2.
	n, err = x.svc.Star(ctx(), auth.Principal{Name: "bob"}, "o", "r")
	if err != nil || n != 2 {
		t.Fatalf("bob: %d %v", n, err)
	}
	// Unstar → 1; repeat → same; anonymous unstar → 401.
	n, err = x.svc.Unstar(ctx(), jane(), "o", "r")
	if err != nil || n != 1 {
		t.Fatalf("unstar: %d %v", n, err)
	}
	n, err = x.svc.Unstar(ctx(), jane(), "o", "r")
	if err != nil || n != 1 {
		t.Fatalf("re-unstar: %d %v", n, err)
	}
	if _, err := x.svc.Unstar(ctx(), auth.Anonymous(), "o", "r"); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon unstar: %v", err)
	}
	// Unstar floors at 0 (drift tolerance: record without counter bump).
	n, err = x.svc.Unstar(ctx(), auth.Principal{Name: "bob"}, "o", "r")
	if err != nil || n != 0 {
		t.Fatalf("zero: %d %v", n, err)
	}
}

func TestStarConcurrentConverge(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := auth.Principal{Name: "u"}
			if i%2 == 0 {
				p = jane()
			}
			if _, err := x.svc.Star(ctx(), p, "o", "r"); err != nil {
				t.Errorf("star: %v", err)
			}
		}(i)
	}
	wg.Wait()
	d, err := x.svc.Counts(ctx(), jane(), "o", "r")
	if err != nil || d.Stars != 2 {
		t.Fatalf("converge: %+v %v", d, err)
	}
}

// unstarGate forces the check-then-act race to overlap: Deletes on the
// star record wait until both unstarrs have read the record. On a correct
// implementation exactly one version-conditional Delete wins and the
// counter decrements exactly once.
type unstarGate struct {
	store.ObjectStore
	key  string
	gets atomic.Int64
	open chan struct{}
	once sync.Once
}

func (g *unstarGate) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == g.key && g.gets.Add(1) == 2 {
		g.once.Do(func() { close(g.open) })
	}
	return g.ObjectStore.Get(ctx, key, opts)
}

func (g *unstarGate) Delete(ctx context.Context, key string, ver store.Version) error {
	if key == g.key {
		<-g.open
	}
	return g.ObjectStore.Delete(ctx, key, ver)
}

func TestUnstarConcurrentSingleDecrement(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	// One record, count 2 (counter ahead — the drift a crash window or a
	// lost fan-out could leave): two overlapping unstarrs must decrement
	// exactly once, since only one conditional Delete removes the record.
	if _, err := x.svc.bumpStars(ctx(), "o", "r", +1); err != nil {
		t.Fatal(err)
	}
	key := StarKey("jane", "o", "r")
	x.svc.Store = &unstarGate{ObjectStore: x.svc.Store, key: key, open: make(chan struct{})}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err != nil {
				t.Errorf("unstar: %v", err)
			}
		}()
	}
	wg.Wait()
	d, err := x.svc.Counts(ctx(), jane(), "o", "r")
	if err != nil || d.Stars != 1 {
		t.Fatalf("concurrent unstar decremented %d, want exactly 1: %+v %v", 2-d.Stars, d, err)
	}
}

func TestSocialPreservesWatcherList(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	// A 06-written social.json (watcher_list owned by notify) round-trips
	// through star/fork mutations untouched.
	seedSocial(t, x, `{"stars":0,"watchers":1,"forks":0,"watcher_list":["amy@example.com"],"updated_at":"2026-09-04T12:00:00Z"}`)
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	if err := x.svc.IncForks(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	d, err := x.svc.Counts(ctx(), jane(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if d.Stars != 1 || d.Forks != 1 || d.Watchers != 1 || len(d.WatcherList) != 1 || d.WatcherList[0] != "amy@example.com" {
		t.Fatalf("passthrough: %+v", d)
	}
	// Corrupt social.json surfaces ErrCorrupt (never a silent zero).
	seedSocial(t, x, `{oops`)
	if _, err := x.svc.Counts(ctx(), jane(), "o", "r"); !isErr(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
}

func seedSocial(t *testing.T, x *harness, raw string) {
	t.Helper()
	seedSocialKey(t, x, SocialKey("o", "r"), raw)
}

func TestIncForksLazyCreate(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	// No social object: first increment creates it (zeros + forks 1).
	if err := x.svc.IncForks(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	d, err := x.svc.Counts(ctx(), jane(), "o", "r")
	if err != nil || d.Forks != 1 || d.Stars != 0 || d.Watchers != 0 {
		t.Fatalf("lazy: %+v %v", d, err)
	}
	if err := x.svc.IncForks(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	d, _ = x.svc.Counts(ctx(), jane(), "o", "r")
	if d.Forks != 2 {
		t.Fatalf("forks: %+v", d)
	}
}

func TestViewerState(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	// Anonymous viewer: flags false, no error.
	if s, w := x.svc.ViewerState(ctx(), auth.Anonymous(), "o", "r"); s || w {
		t.Fatal("anon viewer")
	}
	// Starred only.
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	if s, w := x.svc.ViewerState(ctx(), jane(), "o", "r"); !s || w {
		t.Fatalf("starred viewer: %v %v", s, w)
	}
	// Watching record (written by 06's shape) flips the flag.
	seedWatchRecord(t, x, "jane", "o", "r")
	if s, w := x.svc.ViewerState(ctx(), jane(), "o", "r"); !s || !w {
		t.Fatalf("watching viewer: %v %v", s, w)
	}
}

func TestStarredLists(t *testing.T) {
	x := newHarness(t)
	// Empty.
	entries, more, err := x.svc.Starred(ctx(), "jane", 50, "")
	if err != nil || len(entries) != 0 || more {
		t.Fatalf("empty: %+v %v %v", entries, more, err)
	}
	if _, _, err := x.svc.Starred(ctx(), "", 50, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("blank principal: %v", err)
	}
	if _, _, err := x.svc.Starred(ctx(), "jane", 50, "bogus"); !isErr(err, ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
	// Three stars, newest first.
	for i, repo := range []string{"o/a", "o/b", "o/c"} {
		x.svc.Now = func() (later time.Time) { return x.now.Add(time.Duration(i) * time.Minute) }
		o, r, ok := splitStarRepo(repo)
		if !ok {
			t.Fatalf("bad repo %q", repo)
		}
		seedRepo(t, x, o, r)
		if _, err := x.svc.Star(ctx(), jane(), o, r); err != nil {
			t.Fatal(err)
		}
	}
	x.svc.Now = func() time.Time { return x.now }
	entries, more, err = x.svc.Starred(ctx(), "jane", 2, "")
	if err != nil || len(entries) != 2 || !more {
		t.Fatalf("page1: %+v %v %v", entries, more, err)
	}
	if entries[0].Repo != "o/c" || entries[1].Repo != "o/b" {
		t.Fatalf("order: %+v", entries)
	}
	after := entries[1].StarredAt + "|" + entries[1].Repo
	entries2, more2, err := x.svc.Starred(ctx(), "jane", 2, after)
	if err != nil || len(entries2) != 1 || more2 || entries2[0].Repo != "o/a" {
		t.Fatalf("page2: %+v %v %v", entries2, more2, err)
	}
	// n clamps.
	if _, _, err := x.svc.Starred(ctx(), "jane", 0, ""); err != nil {
		t.Fatalf("n=0: %v", err)
	}
	entries3, _, err := x.svc.Starred(ctx(), "jane", 500, "")
	if err != nil || len(entries3) != 3 {
		t.Fatalf("clamp: %+v %v", entries3, err)
	}
	// Deleted repos are skipped (miss-tolerant reads): sweeping o/a's
	// manifest hides it while o/b and o/c still list.
	if err := x.svc.Store.Delete(ctx(), manifestKey("o", "a"), ""); err != nil {
		t.Fatal(err)
	}
	entries4, _, err := x.svc.Starred(ctx(), "jane", 500, "")
	if err != nil || len(entries4) != 2 || entries4[0].Repo != "o/c" || entries4[1].Repo != "o/b" {
		t.Fatalf("dead skip: %+v %v", entries4, err)
	}
}

// TestStarDeleteRecreateResync pins the #63 scenario: the prefix sweep
// removes the manifest + social.json while the userspace star record
// survives, and a recreate must reconverge instead of desyncing.
func TestStarDeleteRecreateResync(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	if n, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil || n != 1 {
		t.Fatalf("star: %d %v", n, err)
	}
	// Simulate the registry prefix sweep: manifest + social.json go, the
	// users/ record survives.
	if err := x.svc.Store.Delete(ctx(), manifestKey("o", "r"), ""); err != nil {
		t.Fatal(err)
	}
	if err := x.svc.Store.Delete(ctx(), SocialKey("o", "r"), ""); err != nil {
		t.Fatal(err)
	}
	// Reads tolerate the ghost: starred skips it, counts 404, starring 404s.
	if entries, _, err := x.svc.Starred(ctx(), "jane", 50, ""); err != nil || len(entries) != 0 {
		t.Fatalf("starred ghost: %+v %v", entries, err)
	}
	if _, err := x.svc.Counts(ctx(), jane(), "o", "r"); !isErr(err, ErrNotFound) {
		t.Fatalf("counts ghost: %v", err)
	}
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); !isErr(err, ErrNotFound) {
		t.Fatalf("star ghost: %v", err)
	}
	if s, w := x.svc.ViewerState(ctx(), jane(), "o", "r"); s || w {
		t.Fatalf("viewer ghost: %v %v", s, w)
	}
	// Unstar still cleans the record — and resurrects no social.json.
	if n, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("unstar ghost: %d %v", n, err)
	}
	if raw, _, _ := store.GetBytes(ctx(), x.svc.Store, SocialKey("o", "r"), store.GetOptions{}); raw != nil {
		t.Fatalf("unstar resurrected social.json: %s", raw)
	}
}

// TestStarRecreateRepairsCounter pins the (c) resync: after a
// delete+recreate the stale record's next Star repairs the zeroed counter
// instead of early-returning 0.
func TestStarRecreateRepairsCounter(t *testing.T) {
	for _, seedZero := range []bool{false, true} {
		x := newHarness(t)
		seedRepo(t, x, "o", "r")
		if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
			t.Fatal(err)
		}
		// Delete (sweep) + recreate (fresh manifest, no counters).
		if err := x.svc.Store.Delete(ctx(), manifestKey("o", "r"), ""); err != nil {
			t.Fatal(err)
		}
		if err := x.svc.Store.Delete(ctx(), SocialKey("o", "r"), ""); err != nil {
			t.Fatal(err)
		}
		seedRepo(t, x, "o", "r")
		if seedZero {
			// A sibling writer (watch/fork) created a zeroed counter
			// first — the repair must still fire.
			seedSocialKey(t, x, SocialKey("o", "r"), `{"stars":0,"watchers":1,"forks":0,"watcher_list":["amy"],"updated_at":"2026-09-04T12:00:00Z"}`)
		}
		if n, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil || n != 1 {
			t.Fatalf("seedZero=%v: restar after recreate = %d %v (want 1)", seedZero, n, err)
		}
		// A fresh starrer converges on top.
		if n, err := x.svc.Star(ctx(), auth.Principal{Name: "bob"}, "o", "r"); err != nil || n != 2 {
			t.Fatalf("seedZero=%v: bob = %d %v (want 2)", seedZero, n, err)
		}
	}
}
