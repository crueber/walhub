// publish_extra_test.go — publish-path edges (05 §5.3): the COMPACT publish
// funnel (regression: nil-Txn jobs must commit), pack uploads + add-pack,
// annotate/settings, claim-slot exhaustion, and manifest CAS recovery.
package wal

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// fmtSum is the pack checksum of body: sha1 hex (pack trailing SHA).
func fmtSum(body []byte) string { return fmt.Sprintf("%x", sha1.Sum(body)) }

// packChecksum mirrors fmtSum for the pack bytes.
func packChecksum(t *testing.T, pack []byte) string {
	t.Helper()
	if len(pack) < 20 {
		t.Fatal("pack too short")
	}
	return fmt.Sprintf("%x", sha1.Sum(pack[:len(pack)-20]))
}

// fakeIdxBody builds a valid v2 idx with the given oid count (all-zero oids,
// offsets 0). Enough for buildRemoteIndex/openPackIndex round-trips.
func fakeIdxBody(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 't', 'O', 'c'})
	binary.Write(&buf, binary.BigEndian, uint32(2))
	counts := make([]uint32, 256)
	if n > 0 {
		counts[0] = uint32(n)
	}
	var run uint32
	for i := range counts {
		run += counts[i]
		binary.Write(&buf, binary.BigEndian, run)
	}
	for i := 0; i < n; i++ {
		raw := make([]byte, 20)
		raw[0] = byte(i)
		raw[1] = 0x11 // oid prefix "00..11.."
		buf.Write(raw)
	}
	for i := 0; i < n; i++ {
		binary.Write(&buf, binary.BigEndian, uint32(i*40))
		binary.Write(&buf, binary.BigEndian, uint32(0)) // crc
	}
	return buf.Bytes()
}

// publishWithPack seeds a repo with one ref publish, then publishes a COMPACT
// entry (nil Txn — the PublishCompact funnel). Regression for the bug where
// verifyTxn(nil) rejected every pack-only job: the pack never uploaded and
// nothing committed.
func TestPublish_CompactPublishCommitsPack(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("a", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}

	// A small synthetic pack: content is opaque to the WAL (git never opens
	// it in this test); the checksum keys the store objects.
	packData := []byte("PACK synthetic compact pack payload")
	sum := fmtSum(packData)
	packPath := filepath.Join(t.TempDir(), "pack-"+sum+".pack")
	idxPath := packPath + ".idx"
	if err := os.WriteFile(packPath, packData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("idx-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := h.PublishCompact(ctx, &PreparedPack{
		Checksum: sum, PackPath: packPath, IdxPath: idxPath,
		PackSize: uint64(len(packData)), ObjectCount: 3,
	}, []string{"deadbeef"}, map[string]string{"principal": "t"})
	if err != nil {
		t.Fatalf("compact publish: %v", err)
	}
	if res.Seq == 0 {
		t.Fatalf("compact publish committed nothing: %+v", res)
	}

	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != res.Seq {
		t.Fatalf("head %d != published seq %d", m.HeadSeq, res.Seq)
	}
	var found *proto.PackRef
	for _, p := range m.Packs {
		if p.Checksum == sum {
			found = p
		}
	}
	if found == nil {
		t.Fatalf("pack %s missing from manifest packs %+v", sum, m.Packs)
	}
	if found.Tier != 0 || found.ObjectCount != 3 {
		t.Fatalf("pack ref = %+v", found)
	}
	// The pack landed create-if-absent in the store.
	for _, suffix := range []string{".pack", ".idx"} {
		ok, err := store.Exists(ctx, st, "repos/acme/api/wal/"+sum+suffix)
		if err != nil || !ok {
			t.Fatalf("wal/%s%s missing: %v %v", sum, suffix, ok, err)
		}
	}
	// Superseded pack checksums land in pending removals.
	if len(h.state.PendingPackRemovals) == 0 || h.state.PendingPackRemovals[0] != "deadbeef" {
		t.Fatalf("pending removals = %v", h.state.PendingPackRemovals)
	}
}

func TestPublish_AddPackInstallsLocalFile(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("7", 40)
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-"+sum+".pack")
	if err := os.WriteFile(path, []byte("PACK addpack"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := h.AddPack(ctx, path, sum, 2, nil)
	if err != nil || res.Seq == 0 {
		t.Fatalf("add pack: res=%+v err=%v", res, err)
	}
	installed := filepath.Join(h.Repo().PackDir(), "pack-"+sum+".pack")
	data, err := os.ReadFile(installed)
	if err != nil || string(data) != "PACK addpack" {
		t.Fatalf("installed pack = %q err=%v", data, err)
	}
	m, _ := h.ManifestSnapshot()
	found := false
	for _, p := range m.Packs {
		if p.Checksum == sum {
			if p.Tier != 2 || p.PackSize != uint64(len("PACK addpack")) {
				t.Fatalf("pack ref = %+v", p)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("add-pack pack missing from manifest: %+v", m.Packs)
	}
	// A missing source file must fail cleanly (install error).
	if _, err := h.AddPack(ctx, filepath.Join(dir, "nope.pack"), sum, 2, nil); err == nil {
		t.Fatal("add-pack with missing file succeeded")
	}
}

func TestPublish_AnnotatePack(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("8", 40)
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-"+sum+".pack")
	if err := os.WriteFile(path, []byte("PACK annotate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPack(ctx, path, sum, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.AnnotatePack(ctx, sum, true, false, true); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	for _, p := range m.Packs {
		if p.Checksum == sum {
			if !p.HasRev || p.HasBitmap || !p.HasCommitGraph {
				t.Fatalf("flags = %+v", p)
			}
		}
	}
	// Annotating an unknown checksum is a no-op CAS that still bumps revision.
	rev := m.Revision
	if err := h.AnnotatePack(ctx, strings.Repeat("9", 40), true, true, true); err != nil {
		t.Fatalf("annotate unknown: %v", err)
	}
	m2, _ := h.ManifestSnapshot()
	if m2.Revision != rev+1 {
		t.Fatalf("revision %d, want %d", m2.Revision, rev+1)
	}
}

func TestPublish_SettingsLifecycle(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PublishSettings(ctx, "[gc]\nreflog = false\n", "alice", "tune", nil); err != nil {
		t.Fatalf("publish settings: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Settings == nil || m.Settings.Revision != 1 || m.Settings.Author != "alice" {
		t.Fatalf("settings = %+v", m.Settings)
	}
	if h.effCfg.valid {
		t.Fatal("effective-config cache valid before use")
	}
	// Second revision increments; invalid TOML publishes nothing.
	if err := h.PublishSettings(ctx, "[gc]\nreflog = false\n", "bob", "tune2", nil); err != nil {
		t.Fatal(err)
	}
	m2, _ := h.ManifestSnapshot()
	if m2.Settings.Revision != 2 {
		t.Fatalf("settings revision = %d", m2.Settings.Revision)
	}
	if err := h.PublishSettings(ctx, "not [ valid toml", "x", "y", nil); err == nil {
		t.Fatal("invalid TOML accepted")
	}
	m3, _ := h.ManifestSnapshot()
	if m3.Settings.Revision != 2 {
		t.Fatalf("failed publish moved settings: %+v", m3.Settings)
	}
}

func TestPublish_PlainPushWithoutTxnRejected(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Publish(ctx, PublishRequest{})
	if err != nil {
		t.Fatalf("rejection is a transport success: %v", err)
	}
	if res.Seq != 0 || len(res.PerRef) == 0 || res.PerRef[0].Err == nil ||
		res.PerRef[0].Err.Detail != "nil transaction" {
		t.Fatalf("res = %+v, want nil-transaction rejection", res)
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 0 {
		t.Fatalf("head moved: %d", m.HeadSeq)
	}
}

func TestPublish_UploadFailureFailsBatch(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("3", 40)
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-"+sum+".pack")
	if err := os.WriteFile(path, []byte("PACK fail"), 0o644); err != nil {
		t.Fatal(err)
	}
	// First PUT of the pack file fails (manifest PUTs pass: they come later).
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "wal/"+sum+".pack") {
			return store.NewOther(key, errors.New("disk full"))
		}
		return nil
	}
	if _, err := h.AddPack(ctx, path, sum, 0, nil); err == nil {
		t.Fatal("publish with failing pack upload succeeded")
	}
	// The batch is answered in full, the manifest untouched.
	m, _ := h.ManifestSnapshot()
	if len(m.Packs) != 0 || m.HeadSeq != 0 {
		t.Fatalf("manifest advanced on upload failure: %+v", m)
	}
}

func TestPublish_PublishContextCanceled(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The publisher loop parks between batches on the closed channel select;
	// a canceled caller context aborts the enqueue wait.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Publish(cctx, PublishRequest{Txn: refTxn("refs/heads/x", "", strings.Repeat("c", 40))}); err == nil {
		t.Fatal("publish with canceled ctx succeeded")
	}
}

func TestPublisher_ClosedMailbox(t *testing.T) {
	// MaxBatch=1: the mailbox buffer holds exactly one job, so a stuffed
	// mailbox makes the enqueue select deterministic.
	cfg := cfgWith(t, func(c *config.Config) { c.WAL.MaxBatch = 1 })
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	h.pub.Close()
	h.pub.Close() // idempotent: the second Close sees closed and returns
	// With the loop gone, stuff the mailbox so the enqueue select cannot use
	// the channel case; the caller must observe the shutdown signal.
	h.pub.ch <- &publishJob{req: PublishRequest{Txn: refTxn("refs/heads/x", "", strings.Repeat("d", 40))},
		reply: make(chan publishResult, 1)}
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/y", "", strings.Repeat("e", 40))}); err == nil ||
		!strings.Contains(err.Error(), "shut down") {
		t.Fatalf("publish after close = %v", err)
	}
	_ = ctx
}

// ---- claimSlot / casManifest recovery paths -----------------------------------------

// mustWriteSegment PUTs a framed single-entry segment at seq.
func putSegment(t *testing.T, st store.ObjectStore, repoKey string, seq uint64) {
	t.Helper()
	body := proto.EncodeSegment([]*proto.LogEntry{{
		Seq: seq, Kind: proto.EntryKindRefUpdate, Writer: "ghost",
		CreatedAt: TsPtr(time.Now().UTC()),
	}})
	if _, err := st.Put(context.Background(), repoKey, store.PutBody{Bytes: body}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSlot_BurnsConsecutiveOrphans(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Nine consecutive orphans at seqs 1..9: the burn streak caps at 8 and
	// the 9th claim fails with WalErrCorrupt (§5.4 step 3).
	for seq := uint64(1); seq <= 9; seq++ {
		putSegment(t, st, h.repoKey(store.LogSegmentKey(seq)), seq)
	}
	h.ensurePublisher()
	p := h.pub
	base, _ := h.ManifestSnapshot()
	entries := []*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, CreatedAt: TsPtr(time.Now().UTC())}}
	_, _, _, _, cerr := p.claimSlot(ctx, base, entries, map[uint64]string{})
	if cerr == nil || !strings.Contains(cerr.Error(), "orphan burns") {
		t.Fatalf("claimSlot error = %v, want burn-cap corruption", cerr)
	}
	if m := Metrics(); m.OrphansBurned < 8 {
		t.Fatalf("orphans burned = %d, want ≥ 8", m.OrphansBurned)
	}
}

func TestClaimSlot_CtxCanceled(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	cancel()
	base, _ := h.ManifestSnapshot()
	if _, _, _, _, err := h.pub.claimSlot(ctx, base, nil, nil); err == nil {
		t.Fatal("claimSlot with canceled ctx succeeded")
	}
}

func TestCasManifest_AmbiguousResponseRecovers(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()

	// The simulated lost response: the manifest PUT LANDS, the response is
	// an error, and the fresh re-read must observe our segment → committed
	// with the version recovered via HEAD.
	hk, _ := attachHooks(t, r)
	segBody := proto.EncodeSegment([]*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "w"}})
	segKey := h.repoKey(store.LogSegmentKey(1))
	if _, err := st.Put(ctx, segKey, store.PutBody{Bytes: segBody}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	base, version := h.ManifestSnapshot()
	next := buildNextManifest(base, nil, base.HeadSeq+1, 1, segBody, "w")
	hk.putErrAfter = func(key string, n int) error {
		if key == "repos/acme/api/manifest.pb" {
			return store.NewRetryable(key, errors.New("connection reset"))
		}
		return nil
	}
	gotVersion, committed, err := h.pub.casManifest(ctx, version, store.LogSegmentKey(1), "1", next)
	if err != nil {
		t.Fatalf("casManifest: %v", err)
	}
	if !committed || gotVersion == "" {
		t.Fatalf("recovered commit = %q committed=%v, want version+committed", gotVersion, committed)
	}

	// An ambiguous failure where the segment never landed → not committed,
	// ladder restarts, segment preserved.
	hk2, _ := attachHooks(t, r)
	_ = hk2
	h2, err := r.Create(ctx, "acme/beta", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h2.ensurePublisher()
	hk3, _ := attachHooks(t, r)
	hk3.putErr = func(key string, n int) error { return store.NewRetryable(key, errors.New("reset")) }
	_, committed2, err := h2.pub.casManifest(ctx, "", store.LogSegmentKey(1), "1", next)
	if committed2 || err == nil || !errors.Is(err, errRestartLadder) {
		t.Fatalf("uncommitted CAS = committed=%v err=%v", committed2, err)
	}
}

func TestCasManifest_AmbiguousReReadFailure(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	hk, _ := attachHooks(t, r)
	hk.putErr = func(key string, n int) error { return store.NewRetryable(key, errors.New("lost")) }
	hk.getErr = func(key string, n int) error { return store.NewRetryable(key, errors.New("lost read")) }
	base, version := h.ManifestSnapshot()
	next := buildNextManifest(base, nil, 1, 1, []byte("seg"), "w")
	_, _, err = h.pub.casManifest(ctx, version, store.LogSegmentKey(1), "1", next)
	if err == nil || !strings.Contains(err.Error(), "casLanded") {
		t.Fatalf("err = %v, want casLanded re-read failure", err)
	}
}

func TestFreshHead_AbsentManifest(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	// Delete the manifest: freshHead reports the NOT-FOUND (the caller
	// treats it as absent-vs-unchanged via store.IsNotFound).
	if err := r.st.Delete(ctx, "repos/acme/api/manifest.pb", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pub.freshHead(ctx); err == nil || !store.IsNotFound(err) {
		t.Fatalf("freshHead = %v, want a not-found error", err)
	}
}

func TestBuildNextManifest_Fold(t *testing.T) {
	base := &proto.Manifest{HeadSeq: 1, MinSeq: 1, Revision: 2}
	old := &proto.PackRef{Checksum: "old", Seq: 1}
	mid := &proto.PackRef{Checksum: "mid", Seq: 2}
	base.Packs = []*proto.PackRef{old, mid}
	newPack := &proto.PackRef{Checksum: "new", Seq: 3, PackSize: 10}
	entries := []*proto.LogEntry{
		{Seq: 2, Kind: proto.EntryKindSettings, Settings: &proto.RepoSettings{Toml: "[a]\n"}},
		{Seq: 3, Kind: proto.EntryKindCompact, Pack: newPack, Supersedes: []string{"mid"}},
	}
	next := buildNextManifest(base, entries, 2, 2, []byte("body"), "w")
	if next.HeadSeq != 3 || next.Revision != 3 || next.Writer != "w" {
		t.Fatalf("next = head %d rev %d writer %s", next.HeadSeq, next.Revision, next.Writer)
	}
	if next.Settings == nil || next.Settings.Toml != "[a]\n" {
		t.Fatal("settings not folded")
	}
	var checks []string
	for _, p := range next.Packs {
		checks = append(checks, p.Checksum)
	}
	if len(checks) != 2 || checks[0] != "old" || checks[1] != "new" {
		t.Fatalf("packs = %v, want superseded 'mid' dropped", checks)
	}
	if len(next.LogSegments) != 1 || next.LogSegments[0].FirstSeq != 2 || !next.LogSegments[0].Sealed || next.LogSegments[0].LastSeq != 3 {
		t.Fatalf("segments = %+v, want one sealed segment at [2,3]", next.LogSegments)
	}
}

func TestTrimManifest(t *testing.T) {
	base := &proto.Manifest{HeadSeq: 3, MinSeq: 1, Revision: 2,
		LogSegments: []*proto.LogSegmentRef{
			{Key: "log/0000000000000001.pb", FirstSeq: 1, LastSeq: 2},
			{Key: "log/0000000000000003.pb", FirstSeq: 3, LastSeq: 3},
		}}
	cp := &proto.CheckpointRef{Seq: 2, Key: "checkpoints/2/checkpoint.pb"}
	next := trimManifest(base, cp, 2, "w")
	if next.MinSeq != 3 || next.Revision != 3 || next.Checkpoint != cp {
		t.Fatalf("trimmed = min_seq %d rev %d cp %v", next.MinSeq, next.Revision, next.Checkpoint)
	}
	// The segment entirely below min_seq (1..2 < 3) is folded away.
	if len(next.LogSegments) != 1 || next.LogSegments[0].FirstSeq != 3 {
		t.Fatalf("segments = %+v", next.LogSegments)
	}
}

// ---- handle-level bits ---------------------------------------------------------------

func TestHandle_PacksReadyLifecycle(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh state: both counters at zero and no pending removals → ready.
	if !h.packsReady() {
		t.Fatal("fresh repo must report packs_ready")
	}
	// A pending removal clears readiness.
	if err := h.updateState(func(st *RepoState) { st.PendingPackRemovals = []string{"ghost"} }); err != nil {
		t.Fatal(err)
	}
	if h.packsReady() {
		t.Fatal("pending removals must clear packs_ready")
	}
	// Materializing the live set at the manifest revision restores readiness.
	if err := h.packMu.LockMeasured(ctx, "pack_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	err = h.reconcilePacks(ctx, LevelServe)
	h.packMu.Unlock()
	if err != nil {
		t.Fatalf("reconcile empty: %v", err)
	}
	if h.state.PacksRevision != h.manifest.Revision {
		t.Fatalf("packs_revision %d, want %d", h.state.PacksRevision, h.manifest.Revision)
	}
	h.invalidateSettings()
}

func TestHandle_RegistryAccessors(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if h.Registry() != r || h.Layer() != r.GitLayer() || h.Dir() != h.repo.Path {
		t.Fatal("handle accessors")
	}
	if h.Repo() != h.repo {
		t.Fatal("Repo()")
	}
	m, v := h.ManifestSnapshot()
	if m == nil || v == "" {
		t.Fatal("manifest snapshot empty")
	}
	// Read guard: Release is nil-safe and idempotent-ish.
	g := &ReadGuard{}
	g.Release() // no panic
	if h.Progress() == nil {
		t.Fatal("Progress nil")
	}
}

func TestHandle_TryLockPacks(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.Create(context.Background(), "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	unlock, ok := h.TryLockPacks()
	if !ok {
		t.Fatal("try-lock failed on an idle repo")
	}
	if _, ok := h.TryLockPacks(); ok {
		t.Fatal("second try-lock succeeded while held")
	}
	unlock()
	if _, ok := h.TryLockPacks(); !ok {
		t.Fatal("try-lock failed after unlock")
	}
}

func TestHandle_StateFileRoundTrip(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.Create(context.Background(), "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.updateState(func(st *RepoState) {
		st.AppliedSeq = 7
		st.ManifestVersion = "v9"
		st.RemoteServed = []string{"abc"}
	}); err != nil {
		t.Fatal(err)
	}
	loaded := loadState(h.repo.Path)
	if loaded.AppliedSeq != 7 || loaded.ManifestVersion != "v9" || len(loaded.RemoteServed) != 1 {
		t.Fatalf("loaded state = %+v", loaded)
	}
	// Corrupt state → zero-value defaults, never an error.
	if err := os.WriteFile(h.repo.Path+"/"+stateFileName, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := loadState(h.repo.Path); st.AppliedSeq != 0 {
		t.Fatalf("corrupt state = %+v", st)
	}
	// saveState failure surfaces (dir replaced by a file is awkward; instead
	// verify the atomic tmp+rename left no debris).
	if _, err := os.Stat(h.repo.Path + "/" + stateFileName + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp state file left behind")
	}
}

func TestReadAll_DrainsAndCloses(t *testing.T) {
	r := &readAllCloser{data: "hello"}
	data, err := readAll(r)
	if err != nil || string(data) != "hello" || !r.closed {
		t.Fatalf("readAll = %q closed=%v err=%v", data, r.closed, err)
	}
}

type readAllCloser struct {
	data   string
	off    int
	closed bool
}

func (r *readAllCloser) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
func (r *readAllCloser) Close() error { r.closed = true; return nil }

// ---- reconcile/materialize with a memory-store pack -----------------------------------

// seedPackObject PUTs a tiny pseudo-pack under wal/<sum>.pack and returns its
// checksum; content is opaque to the materialize path (it only downloads).
func seedPackObject(t *testing.T, st store.ObjectStore, repoPrefix string, checksum string, data []byte) {
	t.Helper()
	for _, key := range []string{store.PackKey(checksum), store.IdxKey(checksum)} {
		body := data
		if strings.HasSuffix(key, ".idx") {
			body = fakeIdxBody(t, 1)
		}
		if _, err := st.Put(context.Background(), repoPrefix+key, store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcile_MaterializeDownloadsPacks(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("4", 40)
	seedPackObject(t, st, h.repoKey(""), sum, []byte("PACK materialized"))

	// A manifest claiming the pack (tier 0 → fully local) with sizes.
	m, version := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: uint64(len("PACK materialized")), IdxSize: 1, Seq: 1, Kind: proto.PackKindObjects}}
	next.Revision = m.Revision + 1
	next.HeadSeq = 1
	meta, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: next.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version)})
	if err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, string(meta.Version), next.Revision
	h.syncMu.Unlock()

	g, err := h.Sync(ctx, LevelFull)
	if err != nil {
		t.Fatalf("sync full: %v", err)
	}
	g.Release()
	data, err := os.ReadFile(filepath.Join(h.Repo().PackDir(), "pack-"+sum+".pack"))
	if err != nil || string(data) != "PACK materialized" {
		t.Fatalf("materialized pack = %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.Repo().PackDir(), "pack-"+sum+".idx")); err != nil {
		t.Fatalf("idx missing: %v", err)
	}
	if !h.packsReady() {
		t.Fatal("packs not ready after full sync")
	}
}

func TestReconcile_ServeSkipsTier2PackButFetchesSideFiles(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("5", 40)
	seedPackObject(t, st, h.repoKey(""), sum, []byte("PACK tier2"))
	if _, err := st.Put(ctx, h.repoKey(store.RevKey(sum)), store.PutBody{Bytes: []byte("rev")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, h.repoKey(store.BitmapKey(sum)), store.PutBody{Bytes: []byte("bm")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, h.repoKey(store.CommitGraphKey(sum)), store.PutBody{Bytes: []byte("cg")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	m, version := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: uint64(len("PACK tier2")), IdxSize: 1, Tier: 2, Seq: 1, HasRev: true, HasBitmap: true, HasCommitGraph: true, Kind: proto.PackKindObjects}}
	next.Revision = m.Revision + 1
	next.HeadSeq = 1
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: next.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version)}); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	g, err := h.Sync(ctx, LevelServe)
	if err != nil {
		t.Fatalf("sync serve: %v", err)
	}
	g.Release()
	packDir := h.Repo().PackDir()
	// Tier-2 .pack stays remote-served; side files landed.
	if _, err := os.Stat(filepath.Join(packDir, "pack-"+sum+".pack")); !os.IsNotExist(err) {
		t.Fatal("tier-2 .pack was materialized under LevelServe")
	}
	for _, suf := range []string{".idx", ".rev", ".bitmap", ".commit-graph"} {
		if _, err := os.Stat(filepath.Join(packDir, "pack-"+sum+suf)); err != nil {
			t.Fatalf("%s missing: %v", suf, err)
		}
	}
	if len(h.state.RemoteServed) != 1 || h.state.RemoteServed[0] != sum {
		t.Fatalf("remote served = %v", h.state.RemoteServed)
	}
	// LevelFull over the same manifest materializes the .pack (retrofit from
	// the store mount has no mount → still skipped, but pack download runs).
	g2, err := h.Sync(ctx, LevelFull)
	if err != nil {
		t.Fatalf("sync full: %v", err)
	}
	g2.Release()
	if _, err := os.Stat(filepath.Join(packDir, "pack-"+sum+".pack")); err != nil {
		t.Fatalf("full sync did not materialize the pack: %v", err)
	}
}

func TestReconcile_Tier2PackLinkedFromStoreMount(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	mount := t.TempDir()
	r.vals.storeMount = mount
	sum := strings.Repeat("6", 40)
	// The pack file laid out under the store mount prefix.
	if err := os.MkdirAll(filepath.Join(mount, "repos", "acme", "api", "wal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "repos", "acme", "api", "wal", sum+".pack"), []byte("PACK mounted"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedPackObject(t, st, h.repoKey(""), sum, []byte("PACK tier2"))
	m, version := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: 12, IdxSize: 1, Tier: 2, Seq: 1, Kind: proto.PackKindObjects}}
	next.Revision = m.Revision + 1
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: next.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version)}); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	g, err := h.Sync(ctx, LevelServe)
	if err != nil {
		t.Fatalf("sync serve: %v", err)
	}
	g.Release()
	link := filepath.Join(h.Repo().PackDir(), "pack-"+sum+".pack")
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("mount link missing (err=%v mode=%v)", err, fi)
	}
}

func TestReconcile_CheckFitsBudget(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the full pack + idx the manifest claims so the LevelServe/Full
	// materializations below can actually succeed.
	seedPackObject(t, st, h.repoKey(""), strings.Repeat("e", 40), make([]byte, 1000))
	m, version := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: strings.Repeat("e", 40), PackSize: 1000, IdxSize: 1, Seq: 1}}
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	r.vals.cacheMaxBytes = 500
	if _, err := h.Sync(ctx, LevelFull); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("over-budget full sync = %v, want ErrTooLarge", err)
	}
	// LevelServe does not consult the budget (only full materializations).
	g, err := h.Sync(ctx, LevelServe)
	if err != nil {
		t.Fatalf("serve sync under budget: %v", err)
	}
	g.Release()

	// A zero budget disables the check entirely.
	r.vals.cacheMaxBytes = 0
	g2, err := h.Sync(ctx, LevelFull)
	if err != nil {
		t.Fatalf("full sync with disabled budget: %v", err)
	}
	g2.Release()
}

func TestReconcile_SupersededRemovalDeferredUnderReader(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A superseded pack whose files are present: without a reader they go.
	sum := strings.Repeat("f", 40)
	packDir := h.Repo().PackDir()
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-"+sum+".pack"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.updateState(func(st *RepoState) { st.PendingPackRemovals = []string{sum} }); err != nil {
		t.Fatal(err)
	}
	g, err := h.Sync(ctx, LevelServe)
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	if _, err := os.Stat(filepath.Join(packDir, "pack-"+sum+".pack")); !os.IsNotExist(err) {
		t.Fatal("superseded pack survived a clean sync")
	}
	if len(h.state.PendingPackRemovals) != 0 {
		t.Fatalf("pending removals = %v", h.state.PendingPackRemovals)
	}

	// With an active reader the try-write fails: files stay, pending list too.
	h2 := h
	h2.rw.RLock()
	defer h2.rw.RUnlock()
	if err := h2.updateState(func(st *RepoState) { st.PendingPackRemovals = []string{sum} }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-"+sum+".idx"), []byte("old idx"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A reader holds the guard: the removal is deferred, never destructive.
	h2.removeSuperseded()
	if _, err := os.Stat(filepath.Join(packDir, "pack-"+sum+".idx")); err != nil {
		t.Fatal("reader lost its file despite try-write deferral")
	}
	if len(h2.state.PendingPackRemovals) != 1 {
		t.Fatalf("pending removals = %v, want the checksum retained", h2.state.PendingPackRemovals)
	}
	h2.rw.RUnlock()
	h2.removeSuperseded()
	if _, err := os.Stat(filepath.Join(packDir, "pack-"+sum+".idx")); !os.IsNotExist(err) {
		t.Fatal("deferred removal never ran")
	}
}

func TestReconcile_FetchSideFileErrors(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// On backends that report a missing object as an error (memory), a
	// claimed side file absent from the store fails the fetch — both the
	// optional ones and the .idx.
	err = h.fetchSideFile(ctx, strings.Repeat("b", 40), ".rev", store.RevKey(strings.Repeat("b", 40)), 0)
	if err == nil || !strings.Contains(err.Error(), ".rev") {
		t.Fatalf("missing .rev = %v", err)
	}
	err = h.fetchSideFile(ctx, strings.Repeat("c", 40), ".idx", store.IdxKey(strings.Repeat("c", 40)), 0)
	if err == nil || !strings.Contains(err.Error(), ".idx") {
		t.Fatalf("missing .idx = %v", err)
	}
}

func TestSideFileSuffixes(t *testing.T) {
	got := sideFileSuffixes(&proto.PackRef{HasRev: true, HasBitmap: true, HasCommitGraph: true})
	want := []string{".idx", ".rev", ".bitmap", ".commit-graph"}
	if len(got) != len(want) {
		t.Fatalf("suffixes = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suffixes = %v", got)
		}
	}
	if got := sideFileSuffixes(&proto.PackRef{}); len(got) != 1 || got[0] != ".idx" {
		t.Fatalf("bare suffixes = %v", got)
	}
}

func TestSideKeyOf(t *testing.T) {
	cs := "abc"
	if sideKeyOf(".rev", cs) != store.RevKey(cs) ||
		sideKeyOf(".bitmap", cs) != store.BitmapKey(cs) ||
		sideKeyOf(".commit-graph", cs) != store.CommitGraphKey(cs) ||
		sideKeyOf(".other", cs) != store.IdxKey(cs) {
		t.Fatal("sideKeyOf mapping")
	}
}

func TestPlanBytesAndServePlan(t *testing.T) {
	m := &proto.Manifest{Packs: []*proto.PackRef{
		{Checksum: "a", PackSize: 10, Tier: 0, Kind: proto.PackKindObjects},
		{Checksum: "b", PackSize: 20, Tier: 2, Kind: proto.PackKindObjects},
		{Checksum: "c", PackSize: 40, Tier: 2, Kind: proto.PackKindHistory},
	}}
	if planBytes(m.Packs) != 70 {
		t.Fatal("planBytes")
	}
	p := servePlanOf(m, LevelServe)
	if len(p.local) != 2 || p.local[0].Checksum != "a" || p.local[1].Checksum != "c" {
		t.Fatalf("local = %+v", p.local)
	}
	if len(p.sideFiles) != 1 || p.sideFiles[0].Checksum != "b" {
		t.Fatalf("sideFiles = %+v", p.sideFiles)
	}
	// LevelFull overrides: everything local.
	pf := servePlanOf(m, LevelFull)
	if len(pf.local) != 3 || len(pf.sideFiles) != 0 {
		t.Fatalf("full plan = %+v/%+v", pf.local, pf.sideFiles)
	}
}

func TestLocalPacks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack-aa.pack"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := localPacks(dir)
	if len(got) != 1 || !got["pack-aa.pack"] {
		t.Fatalf("localPacks = %v", got)
	}
	if g := localPacks(filepath.Join(dir, "missing")); len(g) != 0 {
		t.Fatalf("missing dir = %v", g)
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "f")
	if err := atomicWriteFile(dst, []byte("data")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "data" {
		t.Fatalf("atomic write = %q %v", data, err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind")
	}
	// Writing into a missing directory fails (tmp create).
	if err := atomicWriteFile(filepath.Join(dir, "no", "such", "f"), nil); err == nil {
		t.Fatal("write into missing dir succeeded")
	}
}

// ---- remote reader paths ---------------------------------------------------------

func TestBuildRemoteIndex_RemoteReaderRoundTrip(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	pack, baseOff, deltaOff, baseOid, deltaOid := buildTestPack(t)
	sum := packChecksum(t, pack)
	if _, err := st.Put(ctx, h.repoKey(store.PackKey(sum)), store.PutBody{Bytes: pack}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, h.repoKey(store.IdxKey(sum)), store.PutBody{Bytes: fakeIdxBody(t, 0)}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = baseOid
	_ = deltaOid
	_ = baseOff
	_ = deltaOff

	m, version := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: uint64(len(pack)), IdxSize: 1, Seq: 1, Kind: proto.PackKindObjects}}
	next.Revision = m.Revision + 1
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: next.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version)}); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	rr, err := h.remoteReaderFor(ctx, &next)
	if err != nil {
		t.Fatalf("remoteReaderFor: %v", err)
	}
	if rr.Revision != next.Revision {
		t.Fatalf("reader revision = %d", rr.Revision)
	}
	// A second call hits the cached revision (no rebuild).
	rr2, err := h.remoteReaderFor(ctx, &next)
	if err != nil || rr2 == nil {
		t.Fatalf("second remoteReaderFor: %v", err)
	}
	// The idx landed in remote-idx/ and objects/pack stayed untouched.
	if _, err := os.Stat(filepath.Join(h.repo.Path, "remote-idx", sum+".idx")); err != nil {
		t.Fatalf("remote idx missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.Repo().PackDir(), "pack-"+sum+".pack")); !os.IsNotExist(err) {
		t.Fatal("buildRemoteIndex touched objects/pack")
	}
}

func TestBuildRemoteIndex_MissingIdxIsCorrupt(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("1", 40)
	m, _ := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: 10, IdxSize: 1, Seq: 1, Kind: proto.PackKindObjects}}
	next.Revision = m.Revision + 1
	// No idx in the store: the build task fails and remoteReaderFor surfaces
	// the failure naming the object (Run itself never propagates fn errors).
	if _, err := h.remoteReaderFor(ctx, &next); err == nil || !strings.Contains(err.Error(), ".idx") {
		t.Fatalf("err = %v, want a corrupt-idx failure naming the object", err)
	}
}

func TestRemoteReaderFor_RevisionRace(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := h.ManifestSnapshot()
	// Park a stale revision: remoteReaderFor sees the mismatch and returns
	// the retry error instead of a stale reader.
	h.remoteIdx.Store(&RemotePacks{Revision: m.Revision + 5})
	next := *m
	next.Revision = m.Revision + 1
	// Empty manifest pack set: build succeeds trivially and overwrites.
	rr, err := h.remoteReaderFor(ctx, &next)
	if err != nil || rr == nil {
		t.Fatalf("remoteReaderFor after swap: %v", err)
	}
	if rr.Revision != next.Revision {
		t.Fatalf("stale reader served: rev %d want %d", rr.Revision, next.Revision)
	}
}

func TestEngineFor_BindsRegistry(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.Create(context.Background(), "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	rp := &RemotePacks{Revision: 3}
	eng := h.engineFor(rp)
	if eng.packs != rp || eng.blocks != r.blocks || eng.st != r.st || eng.repoID != h.ID {
		t.Fatal("engineFor wiring")
	}
	if eng.ctx() == nil {
		t.Fatal("engine ctx nil")
	}
}

func TestPackRefOf(t *testing.T) {
	p := &PreparedPack{Checksum: "cs", PackSize: 5, IdxSize: 6, ObjectCount: 7}
	ref := packRefOf(p, 9, 2)
	if ref.Checksum != "cs" || ref.Seq != 9 || ref.Tier != 2 || ref.Kind != proto.PackKindObjects {
		t.Fatalf("packRefOf = %+v", ref)
	}
}
