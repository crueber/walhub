// loop_revindex_test.go — the loop goroutine (§3.1), the mid-pass heartbeat
// ticker (§7), and the rev-index unit (§10) end to end against a synthetic
// pack idx.
package maintain

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestMaintainer_RunLoopAndDrain(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}}
	eng := newFakeEngine(eff, repo)

	// Negative interval: Run parks on ctx.Done and exits on cancel.
	m := New(eng, Options{Leaser: &fakeLeaser{}, Interval: -1})
	done := make(chan struct{})
	go func() { m.Run(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("negative-interval Run must park until cancel")
	case <-time.After(50 * time.Millisecond):
	}
	m2 := New(eng, Options{Leaser: &fakeLeaser{}, Interval: -1})
	done2 := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { m2.Run(ctx); close(done2) }()
	cancel()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("cancelled Run did not exit")
	}
	close(done)
}

func TestMaintainer_RunTicksPasses(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Interval: 10 * time.Millisecond, HostName: "loop-host"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on cancel")
	}
	if m.Metrics().Passes < 2 {
		t.Fatalf("passes = %d, want ≥2 ticks", m.Metrics().Passes)
	}
}

func TestMaintainer_HbTickRewritesHeartbeat(t *testing.T) {
	eff := defaultEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, HostName: "tick-host"})
	ctx := context.Background()

	// No active heartbeat → hbTick is a no-op (no panic, no write).
	m.hbTick(ctx)

	hb := &proto.MaintainerHeartbeat{Host: "tick-host", LastPassAt: ptrTs(time.Now().Add(-time.Hour))}
	m.hbStart(hb)
	m.hbTick(ctx)
	m.hbStop()

	// The rewrite landed with a fresh LastPassAt (Overwrite, no CAS).
	body, _, err := store.GetBytes(ctx, eng.st, store.MaintainerKey("tick-host"), store.GetOptions{})
	if err != nil || body == nil {
		t.Fatalf("heartbeat not written: %v", err)
	}
	got := &proto.MaintainerHeartbeat{}
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LastPassAt == nil || time.Since(got.LastPassAt.Go()) > time.Minute {
		t.Fatalf("hb LastPassAt = %v, want fresh", got.LastPassAt)
	}
	if m.Metrics().HeartbeatWrites != 1 {
		t.Fatalf("heartbeat writes = %d", m.Metrics().HeartbeatWrites)
	}
	// After hbStop the ticker is gone: ticks channel is nil, another tick
	// writes nothing more.
	m.hbTick(ctx)
	if m.Metrics().HeartbeatWrites != 1 {
		t.Fatalf("post-stop write happened: %d", m.Metrics().HeartbeatWrites)
	}
}

// syntheticIdx builds a minimal v2 pack idx (sha1): 1 object, offset 12, with
// valid magic/fanout/trailer shape — enough for buildRevFile (§10).
func syntheticIdx() []byte {
	out := make([]byte, 0, 1100)
	out = append(out, 0xff, 't', 'O', 'c')
	out = binary.BigEndian.AppendUint32(out, 2)
	for i := 0; i < 256; i++ { // fanout: 1 object total
		n := uint32(0)
		if i == 255 {
			n = 1
		}
		out = binary.BigEndian.AppendUint32(out, n)
	}
	out = append(out, make([]byte, 20)...) // the object oid
	out = binary.BigEndian.AppendUint32(out, 0xdeadbeef)
	out = binary.BigEndian.AppendUint32(out, 12) // small offset
	trailer := sha1.Sum([]byte("pack"))
	out = append(out, trailer[:]...)
	sum := sha1.Sum(out)
	out = append(out, sum[:]...)
	return out
}

func TestRunRevIndex_Behaviors(t *testing.T) {
	dir := t.TempDir()
	checksum := "cafe000000000000000000000000000000000000"
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// buildRevFile covered byte-exactly elsewhere; here the unit's install
	// path is what matters, so reuse the writer itself for the expectation.
	idx := syntheticIdx()
	want, err := buildRevFile(idx, 20)
	if err != nil {
		t.Fatalf("synthetic idx rejected: %v", err)
	}

	candidate := pack(checksum, 1, 100, 300_000, 0) // ≥ threshold, no .rev
	mkRepo := func(withPack bool) *fakeRepo {
		packDirCopy := dir
		repo := &fakeRepo{id: "acme/widget", dir: packDirCopy}
		if withPack {
			repo.m = &proto.Manifest{Repo: "acme/widget", HeadSeq: 1, Packs: []*proto.PackRef{candidate}}
		} else {
			repo.m = &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}
		}
		return repo
	}
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false

	t.Run("no-candidate", func(t *testing.T) {
		repo := mkRepo(false)
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.runRevIndex(context.Background(), repo, &Snapshot{Eff: eff, Manifest: repo.m}, nopLogger{})
		if out != OutcomeOK || detail != "no candidate" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})

	t.Run("idx-missing", func(t *testing.T) {
		repo := mkRepo(true)
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.runRevIndex(context.Background(), repo, &Snapshot{Eff: eff, Manifest: repo.m}, nopLogger{})
		if out != OutcomeOK || detail != "pack not local; skipping" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})

	t.Run("install-upload-annotate", func(t *testing.T) {
		repo := mkRepo(true)
		if err := os.WriteFile(filepath.Join(packDir, "pack-"+checksum+".idx"), idx, 0o644); err != nil {
			t.Fatal(err)
		}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.runRevIndex(context.Background(), repo, &Snapshot{Eff: eff, Manifest: repo.m}, nopLogger{})
		if out != OutcomeOK {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
		revPath := filepath.Join(packDir, "pack-"+checksum+".rev")
		rev, err := os.ReadFile(revPath)
		if err != nil || string(rev) != string(want) {
			t.Fatalf("installed .rev: len=%d err=%v (want %d bytes)", len(rev), err, len(want))
		}
		if _, err := os.Stat(revPath + ".maintain-tmp"); !os.IsNotExist(err) {
			t.Fatal("temp file must be renamed away")
		}
		body, _, err := store.GetBytes(context.Background(), eng.st, "repos/acme/widget/wal/"+checksum+".rev", store.GetOptions{})
		if err != nil || string(body) != string(want) {
			t.Fatalf("uploaded .rev: err=%v body=%q", err, body)
		}
		if len(repo.annotated) != 1 || repo.annotated[0] != checksum {
			t.Fatalf("annotations = %v", repo.annotated)
		}
	})

	t.Run("superseded-mid-build", func(t *testing.T) {
		repo := mkRepo(false) // live manifest: pack gone (superseded by a fold)
		if err := os.WriteFile(filepath.Join(packDir, "pack-"+checksum+".idx"), idx, 0o644); err != nil {
			t.Fatal(err)
		}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		// The snapshot still lists the pack (that is where the candidate came
		// from); the fresh manifest read does not → abandon with a notice.
		snap := &Snapshot{Eff: eff, Manifest: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1, Packs: []*proto.PackRef{candidate}}}
		out, detail := m.runRevIndex(context.Background(), repo, snap, nopLogger{})
		if out != OutcomeOK || detail != "pack superseded during build" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
		if len(repo.annotated) != 0 {
			t.Fatalf("annotations = %v, want none", repo.annotated)
		}
		// The .rev was still installed locally before the abandonment (the
		// §10 install happens before the liveness check).
		if _, err := os.Stat(filepath.Join(packDir, "pack-"+checksum+".rev")); err != nil {
			t.Fatalf("installed .rev: %v", err)
		}
	})

	t.Run("store-rev-key", func(t *testing.T) {
		if got := storeRevKey(checksum); got != "wal/"+checksum+".rev" {
			t.Fatalf("storeRevKey = %q", got)
		}
	})
}

func TestUploadPackFiles_Errors(t *testing.T) {
	eff := defaultEff()
	eff.Cache.Dir = t.TempDir()
	repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})

	// A checksum with no scratch files: reads are absent → no-op success.
	empty := filepath.Join(eff.Cache.Dir, "empty-pack")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.uploadPackFiles(context.Background(), "repos/acme/widget/", empty, "deadbeef", filepath.Join(repo.dir, "objects", "pack")); err != nil {
		t.Fatalf("absent side files must no-op: %v", err)
	}
	serving := filepath.Join(repo.dir, "objects", "pack")
	if err := os.MkdirAll(serving, 0o755); err != nil {
		t.Fatal(err)
	}

	// A side file present but with a bad store put: memStore supports Put, so
	// the success path lands the bytes and installs into the serving pack dir.
	packDir := filepath.Join(eff.Cache.Dir, "full-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-abc.pack"), []byte("P"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-abc.idx"), []byte("I"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.uploadPackFiles(context.Background(), "repos/acme/widget/", packDir, "abc", serving); err != nil {
		t.Fatalf("upload: %v", err)
	}
	for key, want := range map[string]string{
		"repos/acme/widget/wal/abc.pack": "P",
		"repos/acme/widget/wal/abc.idx":  "I",
	} {
		body, _, err := store.GetBytes(context.Background(), eng.st, key, store.GetOptions{})
		if err != nil || string(body) != want {
			t.Fatalf("key %s: %q %v", key, body, err)
		}
	}
}
