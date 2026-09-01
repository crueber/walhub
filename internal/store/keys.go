// Package keys: bucket key layout helpers (02_storage_protobuf.md §2.1 — byte-for-byte
// from MASTER_RUST_SPEC §5.1). All keys are repo-relative; the store prefix is applied by
// the Prefixed wrapper.
package store

import "fmt"

// RepoPrefix returns "repos/<owner>/<repo>/" (always trailing slash).
func RepoPrefix(owner, name string) string { return fmt.Sprintf("repos/%s/%s/", owner, name) }

const (
	Manifest = "manifest.pb"
	LogDir   = "log/"
	WalDir   = "wal/"

	CheckpointsDir = "checkpoints/"
	LeasesDir      = "leases/"
	BundlesDir     = "bundles/"
	BundleList     = "bundles/list.pb"

	LfsDir  = "lfs/objects/"
	Fsck    = "fsck.pb"
	Catalog = "meta/repos.pb" // bucket root (only meaningful at bucket root scope)
)

// LogSegmentKey returns "log/<first_seq:016x>.pb" (lowercase hex, zero-padded 16).
func LogSegmentKey(firstSeq uint64) string { return fmt.Sprintf("log/%016x.pb", firstSeq) }

func PackKey(sha string) string        { return "wal/" + sha + ".pack" }
func IdxKey(sha string) string         { return "wal/" + sha + ".idx" }
func RevKey(sha string) string         { return "wal/" + sha + ".rev" }
func BitmapKey(sha string) string      { return "wal/" + sha + ".bitmap" }
func CommitGraphKey(sha string) string { return "wal/" + sha + ".commit-graph" }

func CheckpointDir(seq uint64) string     { return fmt.Sprintf("checkpoints/%016x/", seq) }
func CheckpointKey(seq uint64) string     { return fmt.Sprintf("checkpoints/%016x/checkpoint.pb", seq) }
func CheckpointRefsKey(seq uint64) string { return fmt.Sprintf("checkpoints/%016x/refs.pb", seq) }
func CheckpointBundleKey(seq uint64, sha string) string {
	return fmt.Sprintf("checkpoints/%016x/%s.bundle", seq, sha)
}

func LeaseKey(name string) string { return "leases/" + name + ".pb" }

func BundleObjectKey(strategy, name string) string { return "bundles/" + strategy + "/" + name }

// LfsKey returns "lfs/objects/<aa>/<bb>/<oid>" (aa=oid[0:2], bb=oid[2:4]).
func LfsKey(oid string) string {
	aa, bb := "", ""
	if len(oid) >= 2 {
		aa = oid[0:2]
	}
	if len(oid) >= 4 {
		bb = oid[2:4]
	}
	return "lfs/objects/" + aa + "/" + bb + "/" + oid
}

// LfsOidOK reports whether oid is a 64-char lowercase-hex sha256.
func LfsOidOK(oid string) bool {
	if len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// SharedRenderCacheKey returns "cache/api/v1/<sha1hex>.json" (repo-relative).
func SharedRenderCacheKey(sha1hex string) string { return "cache/api/v1/" + sha1hex + ".json" }

// EventsCursorKey is the events bridge cursor (repo-relative).
const EventsCursorKey = "events/cursor.json"

// MaintainerKey returns the BUCKET-ROOT key "maintain/<host>.pb" (not repo-relative).
func MaintainerKey(host string) string { return "maintain/" + host + ".pb" }

// PolicyKey returns the repo-relative policy document key.
func PolicyKey(owner, name string) string { return fmt.Sprintf("repos/%s/%s/policy.json", owner, name) }
