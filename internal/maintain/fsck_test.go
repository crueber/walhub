package maintain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestFsckRunner_RealGit: build a bare repo via plumbing, delete the loose
// blob, and assert the audit parses the missing oid from git's output
// (§9.1 exact argv, GIT_DIR pointing at the bare serving copy).
func TestFsckRunner_RealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "--bare", ".")
	blob := run("hash-object", "-w", "--stdin")
	var tree string
	{
		cmd := exec.Command(gitBin, "mktree")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader("100644 blob " + blob + "\tf.txt\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("mktree: %v: %s", err, out)
		}
		tree = strings.TrimSpace(string(out))
	}
	commit := run("commit-tree", tree, "-m", "c")
	run("update-ref", "refs/heads/main", commit)

	// Remove the loose blob: fsck must report it missing.
	objPath := filepath.Join(dir, "objects", blob[:2], blob[2:])
	if err := os.Remove(objPath); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	missing, _, err := execFscker{}.Fsck(context.Background(), gitBin, dir)
	if err != nil {
		t.Fatalf("fsck: %v", err)
	}
	found := false
	for _, m := range missing {
		if m == blob {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing = %v, want %s", missing, blob)
	}
}

// TestFsckUnit_WritesReport: the unit runs over a complete copy and writes
// fsck.pb (Overwrite) with the missing list (§9.1).
func TestFsckUnit_WritesReport(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Compaction.Enabled = false
	eff.Maintenance.FsckInterval = config.Duration(7 * 24 * time.Hour)

	repo := &fakeRepo{
		id:  "acme/widget",
		m:   &proto.Manifest{Repo: "acme/widget", HeadSeq: 7},
		git: &fakeGit{},
	}
	repo.m.Packs = append(repo.m.Packs, pack("local1", 1, 10, 1, 0))
	// The local pack file backing the manifest (full-copy rule).
	repo.dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo.dir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.dir, "objects", "pack", "pack-local1.pack"), []byte("P"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Fscker: execFscker{}})
	m.RunPass(context.Background())

	report := &proto.FsckReport{}
	has, err := getFsckReport(context.Background(), eng.Store(), repo.Prefix(), report)
	if err != nil || !has {
		t.Fatalf("fsck.pb missing: %v", err)
	}
	if report.Seq != 7 || report.Host != "test-host" || report.At == nil {
		t.Fatalf("report = %+v", report)
	}
	if snap := m.Metrics(); snap.MissingObjects != int64(report.MissingTotal) {
		t.Fatalf("missing gauge = %d, want %d", snap.MissingObjects, report.MissingTotal)
	}
}

// TestFsckUnit_PartialCopySkipped: a host without the full pack set never
// audits (§9.1 complete-copy rule).
func TestFsckUnit_PartialCopySkipped(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Compaction.Enabled = false
	eff.Maintenance.FsckInterval = config.Duration(time.Hour)

	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	// Two live packs, only one local.
	repo.m.Packs = append(repo.m.Packs, pack("p1", 1, 10, 1, 0), pack("p2", 2, 10, 1, 0))
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background())

	if eng.st.has(repo.Prefix() + "fsck.pb") {
		t.Fatal("partial copy must not be audited")
	}
}

// TestCheckpointUnit_WiresWalWriter: the checkpoint unit calls the wal
// checkpoint writer with the evaluated trigger (§11).
func TestCheckpointUnit_WiresWalWriter(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	// entries trigger: head 300, no checkpoint → 300 ≥ 256
	repo := &fakeRepo{
		id:  "acme/widget",
		m:   &proto.Manifest{Repo: "acme/widget", HeadSeq: 300, MinSeq: 1},
		git: &fakeGit{},
	}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background())

	if len(repo.checkpoints) != 1 || repo.checkpoints[0] != "entries" {
		t.Fatalf("checkpoint triggers = %v, want [entries]", repo.checkpoints)
	}
	if snap := m.Metrics(); snap.CheckpointLagEntries != 300 {
		t.Fatalf("lag gauge = %d, want 300", snap.CheckpointLagEntries)
	}
}

// TestGCSuperseded: wal/* objects absent from the manifest and superseded
// beyond retention are deleted; fresh and live ones survive (§6.1 GC).
func TestGCSuperseded(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)

	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 5, MinSeq: 1}, git: &fakeGit{}}
	repo.m.Packs = append(repo.m.Packs, pack("live", 4, 10, 1, 0))

	// Log: a COMPACT entry 8 days ago superseded old-gone + mystery; another
	// one an hour ago superseded recent (inside the window).
	repo.entries = []*proto.LogEntry{
		{Seq: 5, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-8 * 24 * time.Hour)),
			Supersedes: []string{"old-gone", "mystery"}},
		{Seq: 5, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-time.Hour)),
			Supersedes: []string{"recent"}},
	}

	eng := newFakeEngine(eff, repo)
	maint := New(eng, Options{Leaser: &fakeLeaser{}})
	st := eng.Store()
	ctx := context.Background()
	put := func(name string) {
		if _, err := st.Put(ctx, repo.Prefix()+"wal/"+name, store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("old-gone.pack")
	put("old-gone.idx")
	put("old-gone.rev")
	put("recent.pack")
	put("mystery.pack")
	put("live.pack")

	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4 (old-gone pack+idx+rev, mystery)", removed)
	}
	if eng.st.has(repo.Prefix() + "wal/old-gone.pack") {
		t.Fatal("old-gone.pack must be deleted")
	}
	if !eng.st.has(repo.Prefix() + "wal/recent.pack") {
		t.Fatal("recently superseded packs stay for the provenance window")
	}
	if !eng.st.has(repo.Prefix() + "wal/live.pack") {
		t.Fatal("live packs are never GC'd")
	}
}
