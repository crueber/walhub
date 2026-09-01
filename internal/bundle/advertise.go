package bundle

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// v2 advertisement, band-2 narration, and D17 forcing (§8.12, §8.13).

// D17Refusal is the exact pkt-ERR text refusing an unbounded zero-have fetch
// of a bundles.require repo (§8.13).
const D17Refusal = "unbounded fetch refused: this repo requires bundle-uri; use bundle-uri per the setup recipe, or pass -c transfer.bundleURI=false for shallow/filtered fetches"

// D17Warning is the loud band-2 WARNING emitted for the one-shot fallback
// (§8.13).
const D17Warning = "warning: full fetch allowed without bundle-uri (fallback, once per 6 h); switch to the bundle-uri recipe"

// AdvertiseLines answers `command=bundle-uri` (no arguments): the clone list
// inline as key=value pkt-lines, flush-terminated by the caller (§8.12).
// advertise_filtered appends the filtered family's entries with their filter
// line — only for a patched git (§8.13 hazard: stock git never consults
// bundle.<id>.filter; families must never mix on one list).
func AdvertiseLines(ctx context.Context, srv *Server, repo string, list *proto.BundleList, advertiseFiltered bool) ([]string, error) {
	lines := []string{
		"bundle.version=1",
		"bundle.mode=" + modeOf(list),
		"bundle.heuristic=" + heuristicOf(list),
	}
	entries, err := FamilyFilter(list, "")
	if err != nil {
		return nil, err
	}
	entries = intersectIDs(entries, CloneEntries(list))
	for _, e := range entries {
		lines = append(lines,
			"bundle."+e.ID+".uri="+srv.URI(ctx, repo, e),
			"bundle."+e.ID+".creationToken="+strconv.FormatUint(e.CreationToken, 10),
		)
	}
	if advertiseFiltered {
		filtered, ferr := FamilyFilter(list, FilterBlobNone)
		if ferr != nil {
			return nil, ferr
		}
		for _, e := range filtered {
			lines = append(lines,
				"bundle."+e.ID+".uri="+srv.URI(ctx, repo, e),
				"bundle."+e.ID+".creationToken="+strconv.FormatUint(e.CreationToken, 10),
				"bundle."+e.ID+".filter="+e.Filter,
			)
		}
	}
	return lines, nil
}

// NarrationLine renders one band-2 narration line (§8.12): a v2 fetch response
// under active advertisement echoes each advertised bundle before the packfile
// section, one line per plain-list entry ascending token:
//
//   - bundle-uri: <path> (<human size>, <kind>, seq <seq>, token <token>)
func NarrationLine(e *proto.BundleEntry) string {
	return fmt.Sprintf("* bundle-uri: %s (%s, %s, seq %d, token %d)",
		e.Key, HumanBytes(e.Size), e.Kind, e.Seq, e.CreationToken)
}

// NarrationLines renders the band-2 echo for a list (ascending creationToken).
func NarrationLines(entries []*proto.BundleEntry) []string {
	sorted := make([]*proto.BundleEntry, len(entries))
	copy(sorted, entries)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1].CreationToken > sorted[j].CreationToken; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	out := make([]string, 0, len(sorted))
	for _, e := range sorted {
		out = append(out, NarrationLine(e))
	}
	return out
}

// HumanBytes formats a size one-decimal, 1000-based B/KB/MB/GB/TB (§8.12).
func HumanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	div, exp := uint64(1), 0
	for m := n; m >= unit && exp < len(units); m /= unit {
		div *= unit
		exp++
		if div == 0 {
			break
		}
	}
	if exp == 0 {
		return strconv.FormatUint(n, 10) + " B"
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp-1])
}

// --- D17 forcing tracker (§8.13) -------------------------------------------

// d17ListFetchTTL: a principal that fetched bundles/list within the last hour
// demonstrably tried bundle-uri (git never retries a failed bundle download
// and then falls back).
const d17ListFetchTTL = time.Hour

// d17FallbackGap: one upload-pack full clone per 6 h per principal (§8.13).
const d17FallbackGap = 6 * time.Hour

// d17EntryTTL is the entry expiry (lazy + a 10 m sweep dropping entries older
// than 6 h; §8.13).
const d17EntryTTL = d17FallbackGap

// d17Cap bounds the tracker at 100 000 keys with drop-oldest (§8.13).
const d17Cap = 100_000

type d17Key struct{ repo, principal string }

type d17Entry struct {
	listFetch time.Time
	fallback  time.Time
}

// D17Tracker is the per-instance, in-memory D17 state (§8.13). A restart
// resets it, granting one fresh fallback per principal — accepted.
type D17Tracker struct {
	mu      sync.Mutex
	entries map[d17Key]*d17Entry
	order   []d17Key // insertion order for drop-oldest
}

// NewD17Tracker returns an empty tracker.
func NewD17Tracker() *D17Tracker {
	return &D17Tracker{entries: make(map[d17Key]*d17Entry)}
}

// RecordListFetch records that (repo, principal) fetched the bundle list
// (called by the list GET handler, §8.13).
func (g *D17Tracker) RecordListFetch(repo, principal string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := d17Key{repo, principal}
	e, ok := g.entries[k]
	if !ok {
		g.evictIfNeededLocked()
		e = &d17Entry{}
		g.entries[k] = e
		g.order = append(g.order, k)
	}
	e.listFetch = now
}

// Decide is the guard decision for an unbounded zero-have fetch (§8.13):
//
//	allow := now.Sub(e.listFetch) <= 1h && (e.fallback.IsZero() || now.Sub(e.fallback) >= 6h)
//	on allow: e.fallback = now
//
// Returns (allow, warning) — warning is D17Warning when allowed.
func (g *D17Tracker) Decide(repo, principal string, now time.Time) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := d17Key{repo, principal}
	e, ok := g.entries[k]
	if !ok || now.Sub(e.listFetch) > d17ListFetchTTL {
		return false, ""
	}
	if !e.fallback.IsZero() && now.Sub(e.fallback) < d17FallbackGap {
		return false, ""
	}
	e.fallback = now
	return true, D17Warning
}

// Sweep drops entries older than 6 h (called by the 10 m sweep goroutine,
// §8.13). Entries whose last activity (listFetch or fallback) is older than
// the 6 h gap can never allow a fallback again.
func (g *D17Tracker) Sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, e := range g.entries {
		last := e.listFetch
		if e.fallback.After(last) {
			last = e.fallback
		}
		if now.Sub(last) > d17EntryTTL {
			delete(g.entries, k)
		}
	}
	// Compact the insertion order.
	order := g.order[:0]
	for _, k := range g.order {
		if _, ok := g.entries[k]; ok {
			order = append(order, k)
		}
	}
	g.order = order
}

// evictIfNeededLocked drops the oldest entry beyond the cap (§8.13).
func (g *D17Tracker) evictIfNeededLocked() {
	if len(g.entries) < d17Cap {
		return
	}
	for len(g.order) > 0 && len(g.entries) >= d17Cap {
		oldest := g.order[0]
		g.order = g.order[1:]
		delete(g.entries, oldest)
	}
}

// RefusalLine is the pkt-ERR payload for a refused fetch: the exact fix text.
func RefusalLine() string { return "ERR " + D17RefusalText() }

// D17RefusalText returns the §8.13 refusal text (identical to D17Refusal,
// kept as a func so call sites read naturally at the fetch seam).
func D17RefusalText() string { return D17Refusal }

// IsUnboundedZeroHave classifies a v2 fetch request (§8.13): unbounded means
// no haves, no deepen*, no filter. Bounded zero-have fetches (CI
// --depth/--filter) and all fetches with haves proceed.
func IsUnboundedZeroHave(haves []string, deepen, filter string) bool {
	return len(haves) == 0 && deepen == "" && filter == ""
}
