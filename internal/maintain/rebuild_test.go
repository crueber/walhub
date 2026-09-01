package maintain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// rebuildFixture builds an eff+repo pair with a base-due full slot and a
// deterministic fake git.
func rebuildFixture(t *testing.T, disk string) (*config.Config, *fakeRepo, *fakePlanner) {
	t.Helper()
	eff := defaultEff()
	eff.Maintenance.Disk = disk
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Cache.Dir = t.TempDir()

	repo := &fakeRepo{
		id:   "acme/widget",
		dir:  t.TempDir(), // the serving copy the scratch clones
		m:    &proto.Manifest{Repo: "acme/widget", HeadSeq: 42},
		refs: map[string]string{"refs/heads/main": "a1b2c3d"},
	}
	// No tier-2 base, packs exist → base-rebuild trigger 1 (§6.2).
	repo.m.Packs = append(repo.m.Packs, pack("fresh1", 1, 100, 10, 0))
	// objects dir present: hasObjectsDir(scratch) must hold after the copy
	// (§6.2 resume precondition).
	if err := os.MkdirAll(filepath.Join(repo.dir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.git = &fakeGit{
		fullDiff:    &git.PackDiff{New: []string{"pack-newbase.idx"}},
		historyPack: "pack-hist",
		commitGraph: "cg1",
	}
	planner := &fakePlanner{slots: map[string][]Slot{
		"acme/widget": {{Strategy: "weekly", Kind: "full", Slot: 7, State: "missing"}},
	}}
	return eff, repo, planner
}

// TestRebuild_PhaseMachineAndPublish: the ssd run walks copied → repacked →
// history_pack → commit_graph and publishes idempotently (§6.2).
func TestRebuild_PhaseMachineAndPublish(t *testing.T) {
	eff, repo, planner := rebuildFixture(t, "ssd")
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Planner: planner})
	m.RunPass(context.Background())

	if repo.git.repackCalls != 1 {
		t.Fatalf("full repack calls = %d, want 1", repo.git.repackCalls)
	}
	if len(repo.compacts) != 1 {
		t.Fatalf("compacts = %d, want 1 base publish", len(repo.compacts))
	}
	c := repo.compacts[0]
	if c.tier != 2 || c.checksum != "newbase" {
		t.Fatalf("base publish = %+v, want tier-2 newbase", c)
	}
	if len(c.supersedes) != 1 || c.supersedes[0] != "fresh1" {
		t.Fatalf("supersedes = %v, want [fresh1]", c.supersedes)
	}
	if !contains(repo.annotated, "newbase") {
		t.Fatalf("annotations = %v, want newbase annotated", repo.annotated)
	}
	// Marker consumed.
	scratch, marker := rebuildScratch(eff.Cache.Dir, mustParseID(t, "acme/widget"))
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("marker %s must be consumed after publish", marker)
	}
	if !hasObjectsDir(scratch) {
		t.Fatal("scratch must persist (resumable workspace) until consumed")
	}
}

func mustParseID(t *testing.T, id string) git.RepoId {
	t.Helper()
	p, err := git.ParseRepoId(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRebuild_ResumeRuleMatrix: marker vs head_seq (§6.2 step 4).
func TestRebuild_ResumeRuleMatrix(t *testing.T) {
	tests := []struct {
		name           string
		markerHead     uint64
		manifestHead   uint64
		objectsDir     bool
		wantRepackRuns int // total across both attempts
	}{
		{"resume-when-head-equal-and-scratch-intact", 42, 42, true, 1},
		{"restart-when-head-moved", 42, 43, true, 2},
		{"restart-when-scratch-missing", 42, 42, false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eff, repo, planner := rebuildFixture(t, "ssd")
			scratch, markerPath := rebuildScratch(eff.Cache.Dir, mustParseID(t, "acme/widget"))

			// First attempt: killed right after the repack phase wrote its
			// marker (the §6.2 kill-between-phases injection).
			phaseSeen := make(chan string, 4)
			eng := newFakeEngine(eff, repo)
			m := New(eng, Options{
				Leaser:  &fakeLeaser{},
				Planner: planner,
				RebuildPhaseHook: func(phase string) error {
					phaseSeen <- phase
					if phase == phaseRepacked {
						return errors.New("killed after repack")
					}
					return nil
				},
			})
			m.RunPass(context.Background())
			if repo.git.repackCalls != 1 {
				t.Fatalf("first attempt repack calls = %d", repo.git.repackCalls)
			}
			// Durable evidence: marker at phase repacked.
			marker, ok := readRebuildMarker(markerPath)
			if !ok || marker.Phase != phaseRepacked || marker.StartedHeadSeq != 42 {
				t.Fatalf("marker = %+v ok=%v, want phase=repacked head=42", marker, ok)
			}
			// Simulate the matrix conditions for the resumed run.
			if tt.manifestHead != 42 {
				repo.m.HeadSeq = tt.manifestHead
			}
			if !tt.objectsDir {
				removeAll(scratch)
			}

			// Second attempt: the resumed run.
			eng2 := newFakeEngine(eff, repo)
			m2 := New(eng2, Options{Leaser: &fakeLeaser{}, Planner: planner})
			m2.RunPass(context.Background())

			if repo.git.repackCalls != tt.wantRepackRuns {
				t.Fatalf("total repack calls = %d, want %d (exactly one git repack across all attempts unless invalidated)", repo.git.repackCalls, tt.wantRepackRuns)
			}
			// Same published checksums regardless of resume path.
			if len(repo.compacts) == 0 || repo.compacts[len(repo.compacts)-1].checksum != "newbase" {
				t.Fatalf("published = %+v, want newbase", repo.compacts)
			}
		})
	}
}

// TestRebuild_TmpfsRefusal: a tmpfs host plans the due base slot but reports
// wrong-host (§4.1) — and never repacks.
func TestRebuild_TmpfsRefusal(t *testing.T) {
	eff, repo, planner := rebuildFixture(t, "tmpfs")
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Planner: planner})
	m.RunPass(context.Background())

	if repo.git.repackCalls != 0 {
		t.Fatal("tmpfs hosts never rebuild bases")
	}
	if snap := m.Metrics(); snap.Units[KindBundle][OutcomeWrongHost] != 1 {
		t.Fatalf("bundle outcome = %v, want wrong-host", snap.Units[KindBundle])
	}
}

// TestRebuild_DiskFreePreflight: free < pack set → blocked, try next pass
// (§6.2 pre-flight).
func TestRebuild_DiskFreePreflight(t *testing.T) {
	eff, repo, planner := rebuildFixture(t, "ssd")
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{
		Leaser:  &fakeLeaser{},
		Planner: planner,
	})
	m.freeSpace = func(dir string) (uint64, error) { return 10, nil } // 10 bytes free
	m.RunPass(context.Background())

	if repo.git.repackCalls != 0 {
		t.Fatal("blocked pre-flight must not repack")
	}
	if snap := m.Metrics(); snap.Units[KindBundle][OutcomeHeld] != 1 {
		t.Fatalf("bundle outcome = %v, want held (blocked)", snap.Units[KindBundle])
	}
}

// TestRebuild_PublishIdempotent: duplicate create-if-absent uploads are
// success, and a resumed publish does not double-supersede (§6.2 step 5).
func TestRebuild_PublishIdempotent(t *testing.T) {
	eff, repo, planner := rebuildFixture(t, "ssd")
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Planner: planner})
	m.RunPass(context.Background())
	first := len(repo.compacts)
	// Run again: the base now exists with a bitmap → trigger 1 gone; nothing
	// to do. No duplicate publishes.
	m.RunPass(context.Background())
	if len(repo.compacts) != first {
		t.Fatalf("compacts = %d after second pass, want %d (idempotent)", len(repo.compacts), first)
	}
}

func removeAll(path string) { _ = os.RemoveAll(path) }
