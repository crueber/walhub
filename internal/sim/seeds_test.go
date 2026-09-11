// seeds_test.go — TestSim_LivenessUnderRandomSeeds (15_testing.md §4 row
// 12): the safety-then-liveness core under every configured seed — the
// randomized consistency proof. Deterministic per seed; WALHUB_SIM_SEED /
// WALHUB_SIM_SEEDS override the default set.
package sim

import (
	"testing"
)

func TestSim_LivenessUnderRandomSeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	seeds, err := simSeeds()
	if err != nil {
		t.Fatalf("sim seeds: %v", err)
	}
	for _, seed := range seeds {
		seed := seed
		t.Run(seedName(seed), func(t *testing.T) {
			c := newCluster(t, defaultRepo)
			// Smaller than the headline scenario (2x2 under light chaos):
			// the proof is in the seed breadth, not per-seed volume.
			got := safetyCore(t, c, seed, 2, 2, 0.02)
			if len(got) != 2 {
				t.Fatalf("seed %d: expected 2 converged refs, got %d", seed, len(got))
			}
		})
	}
}
