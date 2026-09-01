package maintain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// fakeFollow implements FollowFetcher with scripted tips + ancestry.
type fakeFollow struct {
	mu       sync.Mutex
	tips     map[string]string
	ancient  bool // AncestorOf answer
	fetchErr error
	fetches  int
	lastOurs map[string]string
}

// countFetches is the race-safe fetch counter for loop tests.
func (f *fakeFollow) countFetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

func (f *fakeFollow) Fetch(ctx context.Context, repo, url, tokenEnv string, ours map[string]string, refs []string) (map[string]string, error) {
	f.mu.Lock()
	f.fetches++
	f.lastOurs = ours
	err := f.fetchErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range refs {
		if v, ok := f.tips[r]; ok {
			out[r] = v
		}
	}
	return out, nil
}

func (f *fakeFollow) AncestorOf(ctx context.Context, repo, old, new string) (bool, error) {
	return f.ancient, nil
}

func followFixture(t *testing.T, ours, tips map[string]string, ancient bool) (*config.Config, *fakeRepo, *fakeEngine, *fakeFollow) {
	t.Helper()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Maintenance.FollowInterval = -1 // loop off; we drive rounds directly
	eff.Upstream.Git = "https://github.com/acme/widget.git"
	eff.Upstream.Follow = []string{"refs/heads/main"}

	repo := &fakeRepo{
		id:   "acme/widget",
		m:    &proto.Manifest{Repo: "acme/widget", HeadSeq: 5},
		refs: map[string]string{},
	}
	for k, v := range ours {
		repo.refs[k] = v
	}
	eng := newFakeEngine(eff, repo)
	f := &fakeFollow{tips: tips, ancient: ancient}
	return eff, repo, eng, f
}

// TestFollow_FastForwardPublishes: an ff move publishes with
// principal="upstream" (§8.3/§8.4).
func TestFollow_FastForwardPublishes(t *testing.T) {
	_, repo, eng, f := followFixture(t,
		map[string]string{"refs/heads/main": "1111111"},
		map[string]string{"refs/heads/main": "2222222"},
		true,
	)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	m.followOnce(context.Background(), "acme/widget")

	if len(repo.published) != 1 {
		t.Fatalf("publishes = %d, want 1", len(repo.published))
	}
	pub := repo.published[0]
	if pub.meta["principal"] != "upstream" || pub.meta["agent"] != "walgit follow" ||
		pub.meta["upstream"] != "https://github.com/acme/widget.git" {
		t.Fatalf("meta = %v, want upstream principal/agent/url", pub.meta)
	}
	round, ok := m.LastRound("acme/widget")
	if !ok || round.Outcome != "published" {
		t.Fatalf("round = %+v ok=%v, want published", round, ok)
	}
	// Delta-request staging: ours was passed to the fetcher.
	if f.lastOurs["refs/heads/main"] != "1111111" {
		t.Fatalf("ours staged = %v", f.lastOurs)
	}
}

// TestFollow_RewoundRefused: a rewound upstream is refused EVERY round —
// not sticky-silenced (§8.3).
func TestFollow_RewoundRefused(t *testing.T) {
	_, repo, eng, f := followFixture(t,
		map[string]string{"refs/heads/main": "9999999"},
		map[string]string{"refs/heads/main": "1111111"},
		false,
	)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	for range 3 { // every 30 s, per the doc's cadence
		m.followOnce(context.Background(), "acme/widget")
	}
	if len(repo.published) != 0 {
		t.Fatal("a rewound upstream must never publish")
	}
	round, _ := m.LastRound("acme/widget")
	if round.Outcome != "refused" {
		t.Fatalf("outcome = %s, want refused", round.Outcome)
	}
	if f.fetches != 3 {
		t.Fatalf("fetches = %d, want 3 (refusal never silenced)", f.fetches)
	}
}

// TestFollow_InSyncAndDeletedRefs: no movement → in-sync; upstream deletions
// are left alone (§8.3).
func TestFollow_InSyncAndDeletedRefs(t *testing.T) {
	_, repo, eng, f := followFixture(t,
		map[string]string{"refs/heads/main": "1111111"},
		map[string]string{}, // upstream deleted the ref
		true,
	)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	m.followOnce(context.Background(), "acme/widget")

	if len(repo.published) != 0 {
		t.Fatal("deletions are a human's call")
	}
	round, _ := m.LastRound("acme/widget")
	if round.Outcome != "in-sync" {
		t.Fatalf("outcome = %s, want in-sync", round.Outcome)
	}
}

// TestFollow_FitGate: a pack set that does not fit fails the round with the
// exact detail, never half-negotiating (§8.1).
func TestFollow_FitGate(t *testing.T) {
	eff, _, eng, f := followFixture(t,
		map[string]string{"refs/heads/main": "1111111"},
		map[string]string{"refs/heads/main": "2222222"},
		true,
	)
	eff.Maintenance.MaxPackBytes = 10
	eng.byID["acme/widget"].m.Packs = append(eng.byID["acme/widget"].m.Packs, pack("big", 1, 1<<20, 1, 0))
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	m.followOnce(context.Background(), "acme/widget")

	round, _ := m.LastRound("acme/widget")
	if round.Outcome != "failed" || round.Detail != "pack set does not fit" {
		t.Fatalf("round = %+v, want failed/pack set does not fit", round)
	}
	if f.fetches != 0 {
		t.Fatal("must not fetch when the pack set does not fit")
	}
}

// TestFollow_FetchErr: fetch failure → outcome failed (§8.5).
func TestFollow_FetchErr(t *testing.T) {
	_, _, eng, f := followFixture(t,
		map[string]string{"refs/heads/main": "1111111"},
		map[string]string{"refs/heads/main": "2222222"},
		true,
	)
	f.fetchErr = errors.New("network down")
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	m.followOnce(context.Background(), "acme/widget")

	round, _ := m.LastRound("acme/widget")
	if round.Outcome != "failed" || !strings.Contains(round.Detail, "network down") {
		t.Fatalf("round = %+v, want failed", round)
	}
}

// TestFollowLoop_CadenceAndDrain: RunFollow ticks at follow_interval and
// exits on ctx cancel; interval 0/off never spawns rounds.
func TestFollowLoop_CadenceAndDrain(t *testing.T) {
	_, repo, eng, f := followFixture(t,
		map[string]string{"refs/heads/main": "1111111"},
		map[string]string{"refs/heads/main": "2222222"},
		true,
	)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f, FollowInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.RunFollow(ctx)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for f.countFetches() == 0 {
		select {
		case <-deadline:
			t.Fatal("follow loop never ran a round")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("follow loop must exit on drain cancel")
	}
	_ = repo
}

// TestFollowLoop_Off: follow_interval 0 = off (§8).
func TestFollowLoop_Off(t *testing.T) {
	_, _, eng, f := followFixture(t, nil, nil, true)
	eff := eng.cfg
	eff.Maintenance.FollowInterval = -1
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.RunFollow(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("off follow loop must exit immediately")
	}
	if f.countFetches() != 0 {
		t.Fatal("no rounds when off")
	}
}
