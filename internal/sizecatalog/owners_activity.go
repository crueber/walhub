// owners_activity.go — Forgejo #283 per-owner activity rollup.
//
// The rollup is DERIVED, not stored: OwnerRollups folds the aggregate
// catalog's per-repo LastCommitTime rows (the #247 fields the publish path
// writes blind and the sweep heals) into a per-owner max at request time.
// One comparison per repo over the in-memory catalog, zero new bucket keys,
// zero new store round trips beyond the ONE catalog GET the listing already
// pays (law 6), no proto/codec/fixture change (law 5 — nothing to version).
//
// Incremental maintenance is structural: the per-repo activity is already
// incremental (blind push-path write + sweep backfill/heal — the #247
// contract), so a new push updates its owner's max without a rescan the
// moment its catalog entry folds. SortOwners shares the total order with
// FilterSort (sort=activity) and the explore fallback
// (orderOwnersByActivity): known times in the requested direction, unknowns
// always last, deterministic name-ascending tiebreak.
package sizecatalog

import (
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// OwnerRollup is one owner's derived activity: the max LastCommitTime over
// the owner's catalog rows. Time "" = unknown (no committed repo with a
// known time); SHA "" = unknown (never a fake epoch — readers fall back to
// name order or "unknown", same rule as Row nulls).
type OwnerRollup struct {
	// LastCommitTime is the newest RFC 3339 UTC commit time, "" when unknown.
	LastCommitTime string
	// LastCommitSHA is the tip sha of the row carrying LastCommitTime.
	LastCommitSHA string
}

// OwnerRollups folds a catalog into per-owner maxima. Nil/empty catalog →
// empty (never nil-vs-empty ambiguity for callers: range over it directly).
// Malformed repo ids (no "/" slash) are skipped — the catalog never invents
// repos and neither does this fold. Ties on time keep the
// lexicographically-smaller sha for determinism.
func OwnerRollups(cat *proto.RepoCatalog) map[string]OwnerRollup {
	out := map[string]OwnerRollup{}
	if cat == nil {
		return out
	}
	for _, e := range cat.Entries {
		if e == nil || e.LastCommitTime == nil {
			continue
		}
		owner, _, ok := splitID(e.Repo)
		if !ok {
			continue
		}
		ts := e.LastCommitTime.Go().UTC().Format(time.RFC3339)
		cur, seen := out[owner]
		if !seen || ts > cur.LastCommitTime ||
			(ts == cur.LastCommitTime && e.LastCommitSHA < cur.LastCommitSHA) {
			out[owner] = OwnerRollup{LastCommitTime: ts, LastCommitSHA: e.LastCommitSHA}
		}
	}
	return out
}

// SortOwners orders owner names for the owners listing surface.
// sortKey "activity" ranks by the rollup (order "asc"|"desc", default asc);
// anything else (including "") is the legacy ascending name order and never
// touches the rollup. Unknowns (owners absent from the map, or with an
// empty time) always sort LAST in either direction — they are not "old".
// Ties break deterministically on name ascending (byte order), mirroring
// FilterSort's (owner,name) key and the client's orderOwnersByActivity, so
// server order and client fallback agree. Nil input → empty non-nil slice.
func SortOwners(names []string, rollups map[string]OwnerRollup, sortKey, order string) []string {
	out := append([]string{}, names...)
	if !strings.EqualFold(sortKey, "activity") {
		sort.Strings(out)
		return out
	}
	desc := strings.EqualFold(order, "desc")
	timeOf := func(n string) string {
		if r, ok := rollups[n]; ok {
			return r.LastCommitTime
		}
		return ""
	}
	sort.Slice(out, func(i, j int) bool {
		ta, tb := timeOf(out[i]), timeOf(out[j])
		if (ta == "") != (tb == "") {
			return tb == "" // known before unknown, always
		}
		if ta != tb {
			if desc {
				return ta > tb
			}
			return ta < tb
		}
		return out[i] < out[j]
	})
	return out
}
