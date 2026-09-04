package pulls

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeForksCounter records IncForks calls (the 07 social.json forks
// increment, wired through the ForksCounter seam).
type fakeForksCounter struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (f *fakeForksCounter) IncForks(_ context.Context, owner, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{owner, repo})
	return f.err
}

func TestForkCompletionIncrementsSocial(t *testing.T) {
	e := newTestEnv()
	fc := &fakeForksCounter{}
	e.svc.Forks = fc
	rec := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", "f", "c", writer(), rec); err != nil {
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
	if err := e.svc.runFork(ctx(), "o", "r", "f", "c2", writer(), rec2); err != nil {
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
