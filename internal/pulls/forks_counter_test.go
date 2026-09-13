package pulls

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// fakeForksCounter records IncForks/DecForks calls (the 07 social.json
// forks counter, wired through the ForksCounter seam).
type fakeForksCounter struct {
	mu    sync.Mutex
	calls [][2]string
	decs  [][2]string
	err   error
}

func (f *fakeForksCounter) IncForks(_ context.Context, owner, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{owner, repo})
	return f.err
}

func (f *fakeForksCounter) DecForks(_ context.Context, owner, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decs = append(f.decs, [2]string{owner, repo})
	return f.err
}

func TestForkCompletionIncrementsSocial(t *testing.T) {
	e := newTestEnv()
	fc := &fakeForksCounter{}
	e.svc.Forks = fc
	rec := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
		t.Fatalf("fork: %v", err)
	}
	fc.mu.Lock()
	calls := len(fc.calls)
	first := [2]string{}
	if calls > 0 {
		first = fc.calls[0]
	}
	fc.mu.Unlock()
	if calls != 1 || first != [2]string{"o", "r"} {
		t.Fatalf("counter calls: %d", calls)
	}
	// A counter shortfall is narrated, never fatal to the committed fork.
	fc.mu.Lock()
	fc.err = errors.New("store down")
	fc.mu.Unlock()
	rec2 := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c2"}, writer(), rec2); err != nil {
		t.Fatalf("shortfall must not fail the task: %v", err)
	}
	joined := false
	for _, p := range rec2.Progress {
		if strings.Contains(p, "shortfall") {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("no shortfall notice: %v", rec2.Progress)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.calls) != 2 {
		t.Fatalf("counter calls: %v", fc.calls)
	}
}

// seedForksIndex writes a parent-side forks.json directly (Create).
func seedForksIndex(t *testing.T, e *testEnv, owner, repo, raw string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), e.store, ForksKey(owner, repo), []byte(raw),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed forks: %v", err)
	}
}

// readForksRaw returns the raw parent-side forks.json ("" when absent).
func readForksRaw(t *testing.T, e *testEnv, owner, repo string) string {
	t.Helper()
	raw, _, err := e.svc.getJSON(ctx(), ForksKey(owner, repo))
	if err != nil {
		t.Fatalf("read forks: %v", err)
	}
	return string(raw)
}

// TestUnlistForkTable pins issue #457: the child-delete sweep CAS-removes
// the child's parent-index row. Removal bumps the version; misses write
// nothing (ETag-stable); corruption errors; the call is idempotent.
func TestUnlistForkTable(t *testing.T) {
	const fa = `{"repo":"f/a","forked_at":"2026-09-04T12:00:00Z"}`
	const fc = `{"repo":"f/c","forked_at":"2026-09-05T12:00:00Z"}`
	for _, tc := range []struct {
		name        string
		seed        string // "" = absent index; "{oops" = corrupt
		child       string
		wantRemoved bool
		wantErr     bool
		wantAfter   string // exact raw after ("ABSENT" = still absent)
	}{
		{"absent index is a miss", "", "f/c", false, false, "ABSENT"},
		{"row present is removed", `{"version":3,"forks":[` + fa + `,` + fc + `]}`, "f/c", true, false,
			`{"version":4,"forks":[` + fa + `]}`},
		{"row absent writes nothing", `{"version":3,"forks":[` + fa + `]}`, "f/c", false, false,
			`{"version":3,"forks":[` + fa + `]}`},
		{"last row leaves empty-not-null", `{"version":1,"forks":[` + fc + `]}`, "f/c", true, false,
			`{"version":2,"forks":[]}`},
		{"corrupt index errors", "{oops", "f/c", false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			if tc.seed != "" {
				seedForksIndex(t, e, "o", "r", tc.seed)
			}
			removed, err := e.svc.UnlistFork(ctx(), "o", "r", tc.child)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want corrupt-index error")
				}
				if removed {
					t.Fatal("removed on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unlist: %v", err)
			}
			if removed != tc.wantRemoved {
				t.Fatalf("removed = %v, want %v", removed, tc.wantRemoved)
			}
			after := readForksRaw(t, e, "o", "r")
			if tc.wantAfter == "ABSENT" {
				if after != "" {
					t.Fatalf("after = %q, want absent", after)
				}
				return
			}
			if after != tc.wantAfter {
				t.Fatalf("after = %q, want %q", after, tc.wantAfter)
			}
			// Idempotent: a second sweep is a miss with no further write.
			removed2, err := e.svc.UnlistFork(ctx(), "o", "r", tc.child)
			if err != nil {
				t.Fatalf("resweep: %v", err)
			}
			if tc.wantRemoved && removed2 {
				t.Fatal("second sweep must miss")
			}
			if again := readForksRaw(t, e, "o", "r"); again != after {
				t.Fatalf("resweep rewrote: %q → %q", after, again)
			}
		})
	}
}

// TestUnlistForkRelistConverges pins the delete→refork cycle: after a
// sweep removes the row, forking the same name again re-lists exactly one
// row (the adopt path in runFork converges with the sweep, no duplicate,
// no wedge).
func TestUnlistForkRelistConverges(t *testing.T) {
	e := newTestEnv()
	fc := &fakeForksCounter{}
	e.svc.Forks = fc
	rec := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
		t.Fatalf("fork: %v", err)
	}
	removed, err := e.svc.UnlistFork(ctx(), "o", "r", "f/c")
	if err != nil || !removed {
		t.Fatalf("unlist = %v, %v", removed, err)
	}
	page, err := e.svc.ListForks(ctx(), "o", "r", writer(), "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Forks) != 0 {
		t.Fatalf("forks after sweep = %+v", page.Forks)
	}
	rec2 := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec2); err != nil {
		t.Fatalf("refork: %v", err)
	}
	page, err = e.svc.ListForks(ctx(), "o", "r", writer(), "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Forks) != 1 || page.Forks[0].Repo != "f/c" {
		t.Fatalf("forks after refork = %+v", page.Forks)
	}
}
