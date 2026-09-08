package api

import (
	"encoding/json"
	"net/http"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// --- GET …/overview (§12.1, no-store) -------------------------------------------------

func (h *handlers) overview(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	id := RepoOf(r)
	ov, err := h.env.Repo.Overview(r.Context(), id)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	ov.Repo = id.Owner + "/" + id.Name
	ov.CloneURL = h.env.baseURL(r) + "/" + id.Owner + "/" + id.Name + ".git"
	if ov.Hostname == "" {
		ov.Hostname = h.env.Hostname
	}
	// The fsck projection (issue #209): one conditional GET probe of the
	// cached fsck.pb report (R1 B1: +1 stated, no-store admin page, off the
	// law-6 hot paths). Absent/unreadable → Fsck stays nil (never audited).
	if rep, ok := probeFsck(r.Context(), h.env.Store, id); ok && rep != nil {
		// Effective repair source (in-memory manifest read + TOML merge —
		// no new store round trip; any failure falls back to host config).
		var doc SettingsDoc
		if h.env.Repo != nil {
			doc, _ = h.env.Repo.Settings(r.Context(), id)
		}
		ov.Fsck = fsckProjection(rep, effectiveUpstreamGit(h.env.Cfg, doc), fsckIntervalOf(h), time.Now())
	}
	ov.Health.Issues = nonNil(ov.Health.Issues)
	ov.Health.Suggestions = nonNil(ov.Health.Suggestions)
	ov.Manifest.Segments = nonNil(ov.Manifest.Segments)
	ov.Bundles = nonNil(ov.Bundles)
	ov.BundlePlan.Slots = nonNil(ov.BundlePlan.Slots)
	ov.BundlePlan.Upcoming = nonNil(ov.BundlePlan.Upcoming)
	ov.BundlePlan.Maintainers = nonNil(ov.BundlePlan.Maintainers)
	ov.BundlePlan.Orphaned = nonNil(ov.BundlePlan.Orphaned)
	ov.Compactions = nonNil(ov.Compactions)
	if ov.Node.Counters == nil {
		ov.Node.Counters = map[string]uint64{}
	}
	if ov.Fsck != nil {
		ov.Fsck.Missing = nonNil(ov.Fsck.Missing)
	}
	raw, err := json.Marshal(ov)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "encode: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", ccNoStore)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// fsckProjection renders the admin machine interface for object health
// (issue #209 §3.4): the report's counts + bounded sample, the repair
// disarm flag, the last-audit time, the effective repair source, and the
// derived stall flag (R1 B2: read-side only — a report older than one full
// fsck interval with an upstream configured and repaired_seq still zero
// means repair had a whole audit cycle to land and did not; retry-forever
// per wave4b_test.go:512 stays untouched, R1 B3).
func fsckProjection(rep *proto.FsckReport, upstream string, interval time.Duration, now time.Time) *FsckInfo {
	info := &FsckInfo{
		MissingTotal: fsckMissingTotal(rep),
		Missing:      append([]string{}, rep.Missing...),
		Problems:     rep.Problems,
		RepairedSeq:  rep.RepairedSeq,
		Host:         rep.Host,
		Upstream:     upstream,
	}
	if rep.At != nil {
		at := rep.At.Go()
		info.At = &at
		// Derived stall (R1 B2): upstream set + repair disarm untouched +
		// the damaging report is older than a full audit cycle. No counter,
		// no config key, no protobuf change (R1 B3: give-up CUT for v1).
		if upstream != "" && rep.RepairedSeq == 0 && fsckHasMissing(rep) && now.Sub(at) > interval {
			info.RepairStalled = true
		}
	}
	return info
}

// fsckIntervalOf reads the maintainer's audit cadence for the stall
// derivation; non-positive or missing config falls back to the built-in
// default (7d, the config default) so the flag never misfires.
func fsckIntervalOf(h *handlers) time.Duration {
	const fallback = 7 * 24 * time.Hour
	if h == nil || h.env == nil || h.env.Cfg == nil {
		return fallback
	}
	if d := time.Duration(h.env.Cfg.Maintenance.FsckInterval); d > 0 {
		return d
	}
	return fallback
}
