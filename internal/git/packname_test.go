package git

import "testing"

// TestPackChecksumFromIdx pins the bucket-contract derivation (#205):
// git's on-disk `pack-` infix never reaches PackRef.checksum
// (02_storage_protobuf.md §2.2: key = wal/<checksum>.pack).
func TestPackChecksumFromIdx(t *testing.T) {
	hex40 := "30d7554b1c8a3f2e9d4c5b6a7f8e9d0c1b2a39485"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"canonical sha1", "pack-" + hex40 + ".idx", hex40},
		{"canonical sha256", "pack-" + hex40 + hex40[:24] + ".idx", hex40 + hex40[:24]},
		{"non-canonical base passes through", "gen-" + hex40 + ".idx", "gen-" + hex40},
		{"no infix to strip", hex40 + ".idx", hex40},
		{"no suffix is still stripped of infix", "pack-" + hex40, hex40},
		{"empty stays empty", "", ""},
		{"bare pack- stays empty", "pack-.idx", ""},
		{"legacy doubled strips one layer", "pack-pack-" + hex40 + ".idx", "pack-" + hex40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PackChecksumFromIdx(tc.in); got != tc.want {
				t.Fatalf("PackChecksumFromIdx(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
