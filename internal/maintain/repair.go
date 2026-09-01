// repair.go — unit 2 (§9.2): heal missing oids via the §7.9 repair helper
// (fetch_objects_as_pack), publish as tier-0 COMPACT entries superseding
// nothing, and set repaired_seq in a fresh fsck.pb Overwrite write so the
// next pass re-audits and the repair does not re-fire.
package maintain

import (
	"context"
	"fmt"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// runRepair runs after checkpoint (priority 2) when fsck.pb lists missing
// oids, repaired_seq == 0, and upstream.git is configured. Lease omitted by
// design (§9.2 Concurrency: cheap + idempotent; the repaired_seq == 0
// predicate self-disarms; publish is the ordinary CAS ladder where duplicate
// pack create = success).
func (m *Maintainer) runRepair(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger) (Outcome, string) {
	missing := dedupe(snap.Fsck.Missing)
	if len(missing) == 0 {
		// missing_total > 0 with an empty bounded list: a repair without
		// wants cannot run; the next audit refreshes the report.
		return OutcomeOK, "no missing oids listed; re-audit needed"
	}
	spec := upstreamSpec(snap.Eff)

	healed := 0
	for start := 0; start < len(missing); start += repairBatch {
		end := min(start+repairBatch, len(missing))
		batch := missing[start:end]
		packPath, err := rep.GitOps().FetchObjectsAsPack(ctx, rep.Local(), spec, batch)
		if err != nil {
			// A refused want is an ERROR — never a silent hole; the batch
			// fails, repaired_seq stays 0, the next pass retries (§9.2.5).
			return OutcomeError, fmt.Sprintf("fetch batch [%d..%d): %v", start, end, err)
		}
		checksum := checksumFromPackPath(packPath)
		if _, err := rep.PublishCompact(ctx, &PreparedPack{
			Checksum: checksum,
			PackPath: packPath,
			Tier:     0,
		}, nil, map[string]string{"agent": "walgit maintenance repair"}); err != nil {
			return OutcomeError, fmt.Sprintf("publish repair pack %s: %v", checksum, err)
		}
		healed += len(batch)
		m.metrics.repairObjects.Add(int64(len(batch)))
	}

	// Fresh fsck.pb Overwrite with repaired_seq = head (§9.2.6): the next
	// pass re-audits and the repair does not re-fire.
	report := &proto.FsckReport{
		Seq:         snap.Manifest.HeadSeq,
		At:          ptrTs(m.now()),
		Host:        m.host(),
		RepairedSeq: snap.Manifest.HeadSeq,
	}
	if st := m.store(); st != nil {
		if err := putFsckReport(ctx, st, rep.Prefix(), report); err != nil {
			return OutcomeError, "fsck.pb write: " + err.Error()
		}
	}
	t.Notice(fmt.Sprintf("repaired %d objects from upstream", healed))
	return OutcomeOK, fmt.Sprintf("%d objects repaired", healed)
}

// upstreamSpec builds the git.UpstreamSpec from the effective config
// (§2: upstream.git / upstream.lfs / upstream.token_env).
func upstreamSpec(eff *config.Config) git.UpstreamSpec {
	spec := git.UpstreamSpec{URL: eff.Upstream.Git, LFS: eff.Upstream.Lfs != ""}
	spec.TokenEnv = eff.Upstream.TokenEnv
	if spec.TokenEnv == "" {
		spec.TokenEnv = "WALGIT_UPSTREAM_TOKEN"
	}
	return spec
}

// dedupe keeps order, drops duplicates.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
