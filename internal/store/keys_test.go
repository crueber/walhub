package store

import "testing"

func TestRepoPrefix(t *testing.T) {
	if got, want := RepoPrefix("octo", "hello"), "repos/octo/hello/"; got != want {
		t.Fatalf("RepoPrefix = %q, want %q", got, want)
	}
}

func TestKeyLayout(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{LogSegmentKey(0x1), "log/0000000000000001.pb"},
		{LogSegmentKey(0xdeadbeef), "log/00000000deadbeef.pb"},
		{PackKey("abc"), "wal/abc.pack"},
		{IdxKey("abc"), "wal/abc.idx"},
		{RevKey("abc"), "wal/abc.rev"},
		{BitmapKey("abc"), "wal/abc.bitmap"},
		{CommitGraphKey("abc"), "wal/abc.commit-graph"},
		{CheckpointDir(7), "checkpoints/0000000000000007/"},
		{CheckpointKey(7), "checkpoints/0000000000000007/checkpoint.pb"},
		{CheckpointRefsKey(7), "checkpoints/0000000000000007/refs.pb"},
		{CheckpointBundleKey(7, "sha"), "checkpoints/0000000000000007/sha.bundle"},
		{LeaseKey("publish"), "leases/publish.pb"},
		{BundleObjectKey("horizon", "b.pack"), "bundles/horizon/b.pack"},
		{LfsKey("abcdef"), "lfs/objects/ab/cd/abcdef"},
		{LfsKey("a"), "lfs/objects///a"},
		{LfsKey(""), "lfs/objects///"},
		{SharedRenderCacheKey("cafe"), "cache/api/v1/cafe.json"},
		{MaintainerKey("github.com"), "maintain/github.com.pb"},
		{PolicyKey("o", "r"), "repos/o/r/policy.json"},
		{EventsCursorKey, "events/cursor.json"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("key: got %q, want %q", c.got, c.want)
		}
	}
	for _, c := range []struct{ got, want string }{
		{Manifest, "manifest.pb"}, {LogDir, "log/"}, {WalDir, "wal/"},
		{CheckpointsDir, "checkpoints/"}, {LeasesDir, "leases/"},
		{BundlesDir, "bundles/"}, {BundleList, "bundles/list.pb"},
		{LfsDir, "lfs/objects/"}, {Fsck, "fsck.pb"}, {Catalog, "meta/repos.pb"},
	} {
		if c.got != c.want {
			t.Errorf("constant: got %q, want %q", c.got, c.want)
		}
	}
}

func TestLfsOidOK(t *testing.T) {
	ok := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !LfsOidOK(ok) {
		t.Fatalf("valid oid rejected")
	}
	for _, bad := range []string{
		"", "abc", ok[:63], ok + "0",
		"0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF", // uppercase
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg", // g
	} {
		if LfsOidOK(bad) {
			t.Errorf("oid %q accepted", bad)
		}
	}
}
