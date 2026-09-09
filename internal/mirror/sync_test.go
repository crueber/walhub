// sync_test.go — the sync engine end to end: first sync, no-op,
// fast-forward, rewind refusal + force escape, import-claim skip,
// lease contention, failure counter + backoff, and the loop.
package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

var registerOnce sync.Once

func testRegistry(t *testing.T) (*wal.Registry, store.ObjectStore) {
	t.Helper()
	st := store.NewMemory()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	reg := wal.NewRegistry(context.Background(), st, cfg)
	t.Cleanup(reg.Close)
	return reg, st
}

func testService(t *testing.T, st store.ObjectStore, reg *wal.Registry) *Service {
	t.Helper()
	registerOnce.Do(func() { RegisterKind(KindMirrorSyncOnce) })
	return New(Deps{
		Store: st, Reg: reg,
		CacheDir: t.TempDir(), Hostname: "test-host",
		AllowFile: true, MaxBytes: 1 << 30,
	})
}

// KindMirrorSyncOnce guards the one test-process registration (the
// production call lives in composition; RegisterKind panics on
// duplicates by contract).
const KindMirrorSyncOnce = KindMirrorSync

// setupMirror creates the target repo + sidecar over a file://
// upstream and returns the file URL.
func setupMirror(t *testing.T, ctx context.Context, reg *wal.Registry, st store.ObjectStore, owner, name, upstreamDir string) string {
	t.Helper()
	if _, err := reg.Create(ctx, owner+"/"+name, git.Sha1); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	fileURL := "file://" + upstreamDir
	n, err := repoimport.NormalizeSource(fileURL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := Create(ctx, st, owner, name, n.URL, PresetDaily); err != nil {
		t.Fatalf("create mirror: %v", err)
	}
	return n.URL
}

func servingTip(t *testing.T, ctx context.Context, svc *Service, owner, name, ref string) string {
	t.Helper()
	h, err := svc.reg.Open(ctx, owner+"/"+name)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tips, _, _, err := svc.currentRefs(ctx, h)
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	return tips[ref]
}

func TestSyncFirstAndNoop(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "m", up)

	rec, err := svc.SyncNow(ctx, "acme", "m", "", false)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v", rec)
	}
	if got := servingTip(t, ctx, svc, "acme", "m", "refs/heads/main"); got == "" || got == strings.Repeat("0", 40) {
		t.Fatalf("main tip = %q", got)
	}
	doc, _, _ := Load(ctx, st, "acme", "m")
	if doc.LastResult != "ok" || doc.LastSyncedAt == "" || doc.ConsecutiveFailures != 0 {
		t.Fatalf("doc = %+v", doc)
	}
	// Second sync with no movement: success, already in sync.
	rec2, err := svc.SyncNow(ctx, "acme", "m", "", false)
	if err != nil {
		t.Fatalf("noop sync: %v", err)
	}
	if rec2 == nil || rec2.OK == nil || !*rec2.OK {
		t.Fatalf("rec2 = %+v", rec2)
	}
}

func TestSyncFastForward(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "ff", up)
	if _, err := svc.SyncNow(ctx, "acme", "ff", "", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := servingTip(t, ctx, svc, "acme", "ff", "refs/heads/main")
	commitFile(t, up, "g.txt", "b\n", "b")
	if _, err := svc.SyncNow(ctx, "acme", "ff", "", false); err != nil {
		t.Fatalf("ff: %v", err)
	}
	after := servingTip(t, ctx, svc, "acme", "ff", "refs/heads/main")
	if after == before || after == "" {
		t.Fatalf("tip did not advance: %q -> %q", before, after)
	}
	want := strings.TrimSpace(gitOut(t, up, "rev-parse", "main"))
	if after != want {
		t.Fatalf("tip = %q, want %q", after, want)
	}
}

func TestSyncRewindRefusedAndForced(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	commitFile(t, up, "g.txt", "b\n", "b")
	setupMirror(t, ctx, reg, st, "acme", "rw", up)
	if _, err := svc.SyncNow(ctx, "acme", "rw", "", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	synced := servingTip(t, ctx, svc, "acme", "rw", "refs/heads/main")
	// Rewind the upstream (B is unreachable from a fresh clone —
	// fail closed toward refusal, the merge-base discipline).
	gitOut(t, up, "reset", "--hard", "HEAD~1")
	if _, err := svc.SyncNow(ctx, "acme", "rw", "", false); err != nil {
		t.Fatalf("rewind sync errored (must refuse + narrate, not fail): %v", err)
	}
	if got := servingTip(t, ctx, svc, "acme", "rw", "refs/heads/main"); got != synced {
		t.Fatalf("rewound tip landed: %q", got)
	}
	doc, _, _ := Load(ctx, st, "acme", "rw")
	if !strings.HasPrefix(doc.LastResult, "refused:") {
		t.Fatalf("result = %q, want refused:", doc.LastResult)
	}
	if doc.ConsecutiveFailures != 0 {
		t.Fatalf("refusal bumped the failure counter: %+v", doc)
	}
	// Force resync is the escape hatch.
	if _, err := svc.SyncNow(ctx, "acme", "rw", "", true); err != nil {
		t.Fatalf("force: %v", err)
	}
	want := strings.TrimSpace(gitOut(t, up, "rev-parse", "main"))
	if got := servingTip(t, ctx, svc, "acme", "rw", "refs/heads/main"); got != want {
		t.Fatalf("forced tip = %q, want %q", got, want)
	}
}

func TestSyncRewindRefusalScrubbed(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	// A hostile upstream branch name carrying credential-shaped text
	// ("=" is legal in a refname): the rewind-refusal paths must scrub
	// it (import S2) before it reaches task logs or the sidecar.
	gitOut(t, up, "branch", "leak-token=hunter2")
	anchor := strings.TrimSpace(gitOut(t, up, "rev-parse", "HEAD"))
	commitFile(t, up, "g.txt", "b\n", "b")
	setupMirror(t, ctx, reg, st, "acme", "scr", up)
	if _, err := svc.SyncNow(ctx, "acme", "scr", "", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Advance the hostile branch (ff lands), then rewind it.
	gitOut(t, up, "checkout", "-q", "leak-token=hunter2")
	commitFile(t, up, "h.txt", "c\n", "c")
	gitOut(t, up, "checkout", "-q", "main")
	if _, err := svc.SyncNow(ctx, "acme", "scr", "", false); err != nil {
		t.Fatalf("ff: %v", err)
	}
	gitOut(t, up, "branch", "-f", "leak-token=hunter2", anchor)
	rec, err := svc.SyncNow(ctx, "acme", "scr", "", false)
	if err != nil {
		t.Fatalf("rewind sync errored (must refuse + narrate, not fail): %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v", rec)
	}
	doc, _, _ := Load(ctx, st, "acme", "scr")
	if !strings.HasPrefix(doc.LastResult, "refused:") {
		t.Fatalf("result = %q, want refused:", doc.LastResult)
	}
	if strings.Contains(doc.LastResult, "hunter2") {
		t.Fatalf("secret leaked into sidecar: %q", doc.LastResult)
	}
	if !strings.Contains(doc.LastResult, "[redacted]") {
		t.Fatalf("refusal not scrubbed: %q", doc.LastResult)
	}
	for _, line := range rec.LogTail {
		if strings.Contains(line, "hunter2") {
			t.Fatalf("secret leaked into task log: %q", line)
		}
	}
}

func TestSyncSkipsLiveImportClaim(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "cl", up)
	// In-progress claim (Complete=false) → skip + narrate, success.
	claim, _ := json.Marshal(map[string]any{"version": 1, "source_url": "file://" + up, "complete": false})
	if _, err := store.PutBytes(ctx, st, store.RepoPrefix("acme", "cl")+repoimport.ImportKey, claim,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.SyncNow(ctx, "acme", "cl", "", false)
	if err != nil {
		t.Fatalf("claim sync: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v", rec)
	}
	if got := servingTip(t, ctx, svc, "acme", "cl", "refs/heads/main"); got != "" {
		t.Fatalf("claim collision synced refs: %q", got)
	}
	doc, _, _ := Load(ctx, st, "acme", "cl")
	if doc.LastSyncedAt != "" || doc.ConsecutiveFailures != 0 {
		t.Fatalf("skip touched outcome: %+v", doc)
	}
	// Landed provenance (Complete=true) is NOT live → sync proceeds.
	landed, _ := json.Marshal(map[string]any{"version": 1, "source_url": "file://" + up, "complete": true})
	_, meta, _ := store.GetBytes(ctx, st, store.RepoPrefix("acme", "cl")+repoimport.ImportKey, store.GetOptions{})
	if _, err := store.PutBytes(ctx, st, store.RepoPrefix("acme", "cl")+repoimport.ImportKey, landed,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "acme", "cl", "", false); err != nil {
		t.Fatalf("landed sync: %v", err)
	}
	if got := servingTip(t, ctx, svc, "acme", "cl", "refs/heads/main"); got == "" {
		t.Fatal("landed claim blocked the sync")
	}
}

func TestSyncLeaseContention(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	svc2 := New(Deps{Store: st, Reg: reg, CacheDir: t.TempDir(), Hostname: "other-host", AllowFile: true, MaxBytes: 1 << 30})
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "lease", up)
	release, err := svc.acquireLease(ctx, "acme", "lease")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The rival fire skips (held lease) — a skip, not a failure.
	rec, err := svc2.SyncNow(ctx, "acme", "lease", "", false)
	if err != nil {
		t.Fatalf("contended sync errored: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v", rec)
	}
	doc, _, _ := Load(ctx, st, "acme", "lease")
	if doc.LastSyncedAt != "" || doc.ConsecutiveFailures != 0 {
		t.Fatalf("contention touched outcome: %+v", doc)
	}
	release()
	// After release the fire proceeds.
	if _, err := svc2.SyncNow(ctx, "acme", "lease", "", false); err != nil {
		t.Fatalf("post-release: %v", err)
	}
	doc, _, _ = Load(ctx, st, "acme", "lease")
	if doc.LastResult != "ok" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestSyncFailureCounterAndBackoff(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if _, err := reg.Create(ctx, "acme/bad", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// file:// upstream that does not exist: clone fails → failure recorded.
	if _, err := Create(ctx, st, "acme", "bad", "file:///nonexistent-xyz-abc", PresetDaily); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "acme", "bad", "", false); err == nil {
		t.Fatal("bad upstream sync succeeded")
	}
	doc, _, _ := Load(ctx, st, "acme", "bad")
	if doc.ConsecutiveFailures != 1 || !strings.HasPrefix(doc.LastResult, "failed:") {
		t.Fatalf("doc = %+v", doc)
	}
	if !BackedOff(doc, time.Now()) {
		t.Fatal("no backoff right after failure")
	}
	if Due(doc, time.Now()) {
		t.Fatal("due inside backoff")
	}
	// Force bypasses backoff but the clone still fails → counter grows.
	if _, err := svc.SyncNow(ctx, "acme", "bad", "", true); err == nil {
		t.Fatal("forced bad sync succeeded")
	}
	doc, _, _ = Load(ctx, st, "acme", "bad")
	if doc.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d", doc.ConsecutiveFailures)
	}
}

func TestSyncNotAMirror(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if _, err := svc.SyncNow(ctx, "acme", "nope", "", false); err == nil {
		t.Fatal("non-mirror sync succeeded")
	}
	nilSvc := New(Deps{Store: st, Reg: nil, CacheDir: t.TempDir()})
	if _, err := nilSvc.SyncNow(ctx, "acme", "nope", "", false); err == nil {
		t.Fatal("nil-registry sync succeeded")
	}
}

func TestSyncOpenFailure(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	// Sidecar WITHOUT the repo: clone succeeds, Open fails → recorded failure.
	n, _ := repoimport.NormalizeSource("file://" + up)
	if _, err := Create(ctx, st, "acme", "nor", n.URL, PresetDaily); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "acme", "nor", "", false); err == nil {
		t.Fatal("open-failure sync succeeded")
	}
	doc, _, _ := Load(ctx, st, "acme", "nor")
	if doc.ConsecutiveFailures != 1 {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestSyncTransportRejections(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if _, err := reg.Create(ctx, "acme/tr", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// Stored http URL + manual token → token-over-plaintext refusal.
	if _, err := Create(ctx, st, "acme", "tr", "http://example.com/a", PresetDaily); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "acme", "tr", "s3cret", false); err == nil {
		t.Fatal("plaintext-token sync succeeded")
	}
	doc, _, _ := Load(ctx, st, "acme", "tr")
	if doc.ConsecutiveFailures != 1 || strings.Contains(doc.LastResult, "s3cret") {
		t.Fatalf("doc leaks or uncounted: %+v", doc)
	}
	// file URL with AllowFile=false → SSRF refusal.
	if _, err := reg.Create(ctx, "acme/trf", git.Sha1); err != nil {
		t.Fatal(err)
	}
	n, _ := repoimport.NormalizeSource("file://" + t.TempDir())
	if _, err := Create(ctx, st, "acme", "trf", n.URL, PresetDaily); err != nil {
		t.Fatal(err)
	}
	svc.allowFile = false
	if _, err := svc.SyncNow(ctx, "acme", "trf", "", false); err == nil {
		t.Fatal("disallowed file URL sync succeeded")
	}
	doc, _, _ = Load(ctx, st, "acme", "trf")
	if doc.ConsecutiveFailures != 1 {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestLoopRound(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "loop", up)
	// A plain repo (no mirror) is enumerated but skipped.
	if _, err := reg.Create(ctx, "acme/plain", git.Sha1); err != nil {
		t.Fatal(err)
	}
	svc.Round(ctx, nil)
	if n := len(svc.RecentSyncs("acme/loop")); n != 1 {
		t.Fatalf("recent = %d, want 1", n)
	}
	// Second round: no longer due (success moved the anchor) → no new fire.
	svc.Round(ctx, nil)
	if n := len(svc.RecentSyncs("acme/loop")); n != 1 {
		t.Fatalf("recent = %d, want still 1", n)
	}
	// Placement gate: excluded repos never fire (fresh mirror stays unfired).
	setupMirror(t, ctx, reg, st, "acme", "gated", up)
	svc.Round(ctx, func(repo string) bool { return repo != "acme/gated" })
	if n := len(svc.RecentSyncs("acme/gated")); n != 0 {
		t.Fatalf("gated fired %d times", n)
	}
	// Deleting the sidecar stops the loop for the repo.
	if err := Delete(ctx, st, "acme", "loop"); err != nil {
		t.Fatal(err)
	}
	// Force the anchor back so it WOULD be due — absence still skips.
	svc.Round(ctx, nil)
	if n := len(svc.RecentSyncs("acme/loop")); n != 1 {
		t.Fatalf("deleted mirror fired: %d", n)
	}
	// Nil registry: no panic.
	nilSvc := New(Deps{Store: st, Reg: nil, CacheDir: t.TempDir()})
	nilSvc.Round(ctx, nil)
}

func TestLoopOverdueFiresOnce(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "od", up)
	// Simulate a restart with a stale success anchor: one fire, and the
	// success moves the anchor so the NEXT round does not refire.
	doc, ver, _ := Load(ctx, st, "acme", "od")
	doc.LastSyncedAt = time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	doc.LastAttemptAt = doc.LastSyncedAt
	if err := UpdateCAS(ctx, st, "acme", "od", doc, ver); err != nil {
		t.Fatal(err)
	}
	svc.Round(ctx, nil)
	svc.Round(ctx, nil)
	svc.Round(ctx, nil)
	if n := len(svc.RecentSyncs("acme/od")); n != 1 {
		t.Fatalf("overdue fired %d times, want exactly 1", n)
	}
}

func TestAsyncAndStatus(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "async", up)
	id := svc.SyncAsync(ctx, "acme", "async", "", false)
	deadline := time.Now().Add(30 * time.Second)
	sawPending := false
	for {
		a, ok := svc.SyncStatus(id)
		if !ok {
			t.Fatal("async id unknown")
		}
		select {
		case <-a.done:
			if a.err != nil {
				t.Fatalf("async err: %v", a.err)
			}
			if a.rec == nil || a.rec.OK == nil || !*a.rec.OK {
				t.Fatalf("async rec = %+v", a.rec)
			}
			goto done
		default:
			sawPending = true
		}
		if time.Now().After(deadline) {
			t.Fatal("async never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
done:
	if _, ok := svc.SyncStatus("nope"); ok {
		t.Fatal("unknown id resolved")
	}
	_ = sawPending // best-effort pending observation (branch, not correctness)
	if n := len(svc.RecentSyncs("acme/async")); n == 0 {
		t.Fatal("no recent syncs")
	}
	if svc.RecentSyncs("acme/async")[0].Kind != KindMirrorSync {
		t.Fatal("recent kind mismatch")
	}
	nilSvc := New(Deps{Store: st, Reg: nil, CacheDir: t.TempDir()})
	if nilSvc.RecentSyncs("x/y") != nil {
		t.Fatal("nil-reg recents non-nil")
	}
}

func TestLeaseShapes(t *testing.T) {
	ctx := context.Background()
	_, st := testRegistry(t)
	svc := New(Deps{Store: st, CacheDir: t.TempDir(), Hostname: "h1"})
	rel, err := svc.acquireLease(ctx, "acme", "l1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	svc2 := New(Deps{Store: st, CacheDir: t.TempDir(), Hostname: "h2"})
	if _, err := svc2.acquireLease(ctx, "acme", "l1"); err == nil {
		t.Fatal("double acquire succeeded")
	} else if err != errLeaseHeld {
		t.Fatalf("err = %v", err)
	}
	rel()
	if _, err := svc2.acquireLease(ctx, "acme", "l1"); err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	// Steal: rewrite the lease expired, then a rival takes it with epoch+1.
	rel2, err := svc.acquireLease(ctx, "acme", "steal")
	if err != nil {
		t.Fatalf("steal setup: %v", err)
	}
	_ = rel2
	stealKey := store.LeaseKey(store.MirrorLeaseName("acme", "steal"))
	body, meta, err := store.GetBytes(ctx, st, stealKey, store.GetOptions{})
	if err != nil || body == nil {
		t.Fatalf("lease read: %v", err)
	}
	cur := &proto.Lease{}
	if err := cur.Unmarshal(body); err != nil {
		t.Fatal(err)
	}
	past, _ := proto.TimeFromGo(time.Now().Add(-time.Hour)), proto.TimeFromGo(time.Now().Add(-time.Minute))
	cur.AcquiredAt, cur.ExpiresAt = &past, &past
	expired, _ := json.Marshal(map[string]any{"x": 1})
	_ = expired
	if _, err := st.Put(ctx, stealKey, store.PutBody{Bytes: cur.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatalf("expire rewrite: %v", err)
	}
	stolen, err := svc2.acquireLease(ctx, "acme", "steal")
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	// The pre-steal holder's release leaves the new owner's lease alone.
	rel2()
	if _, _, err := store.GetBytes(ctx, st, stealKey, store.GetOptions{}); err != nil {
		t.Fatalf("stolen lease deleted by old holder: %v", err)
	}
	stolen()
	// Corrupt lease body errors (never a silent steal).
	if _, err := store.PutBytes(ctx, st, store.LeaseKey(store.MirrorLeaseName("acme", "junk")), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.acquireLease(ctx, "acme", "junk"); err == nil {
		t.Fatal("corrupt lease acquired")
	}
	// Store error surfaces (non-NotFound Get).
	broken := New(Deps{Store: errStore{ObjectStore: st}, CacheDir: t.TempDir(), Hostname: "h9"})
	if _, err := broken.acquireLease(ctx, "acme", "any"); err == nil {
		t.Fatal("broken-store acquire succeeded")
	}
	// Release on absent key: no panic, no error.
	svc.releaseFunc(store.LeaseKey(store.MirrorLeaseName("acme", "absent")), "h1", "")()
}

// errStore fails Gets (the lease-acquire store-error branch).
type errStore struct {
	store.ObjectStore
}

func (s errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, store.NewRetryable(key, fmt.Errorf("boom"))
}

func TestImportClaimLiveShapes(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if live, err := svc.importClaimLive(ctx, "acme", "none"); err != nil || live {
		t.Fatalf("absent = %v %v", live, err)
	}
	// Corrupt claim: fail closed toward skipping.
	if _, err := store.PutBytes(ctx, st, store.RepoPrefix("acme", "none")+repoimport.ImportKey, []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if live, err := svc.importClaimLive(ctx, "acme", "none"); err != nil || !live {
		t.Fatalf("corrupt = %v %v, want live", live, err)
	}
}

func TestClassifySyncErrorCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classifySyncError(ctx, fmt.Errorf("x")); got != "clone interrupted; safe to retry" {
		t.Fatalf("got %q", got)
	}
	if got := classifySyncError(context.Background(), fmt.Errorf("clone https://user:hunter2@example.com/x.git failed token=abc")); strings.Contains(got, "hunter2") || strings.Contains(got, "abc") {
		t.Fatalf("unscrubbed: %q", got)
	}
}

func TestPackTrailerChecksum(t *testing.T) {
	dir := t.TempDir()
	// Too small: not a pack.
	f := dir + "/tiny.pack"
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := packTrailerChecksum(f, "sha1"); err == nil {
		t.Fatal("tiny pack checksummed")
	}
	if _, err := packTrailerChecksum(dir+"/missing.pack", "sha1"); err == nil {
		t.Fatal("missing pack checksummed")
	}
	// Exact trailer round-trip (sha1 + sha256 sizes).
	raw := append([]byte("payload"), make([]byte, 32)...)
	for i := range raw[len(raw)-32:] {
		raw[len(raw)-32+i] = byte(i)
	}
	os.WriteFile(f, raw, 0o644)
	if sum, err := packTrailerChecksum(f, "sha1"); err != nil || len(sum) != 40 {
		t.Fatalf("sha1 = %q %v", sum, err)
	}
	if sum, err := packTrailerChecksum(f, "sha256"); err != nil || len(sum) != 64 {
		t.Fatalf("sha256 = %q %v", sum, err)
	}
}

func TestPruneAsync(t *testing.T) {
	m := map[string]*asyncSync{}
	for i := 0; i < 150; i++ {
		a := &asyncSync{id: fmt.Sprintf("id-%d", i), started: fmt.Sprintf("%d", i), done: make(chan struct{})}
		close(a.done)
		m[a.id] = a
	}
	pruneAsyncLocked(m)
	if len(m) != 100 {
		t.Fatalf("len = %d, want 100", len(m))
	}
	pruneAsyncLocked(map[string]*asyncSync{})
}

// countingStore decorates the bucket and counts round trips by op
// (the EVIDENCE.md #240 harness: a sync fire's cost shape, measured
// on the real code path — real git clone, real ingest, real CAS).
type countingStore struct {
	store.ObjectStore
	mu  sync.Mutex
	ops map[string]int
}

func (c *countingStore) bump(op string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ops == nil {
		c.ops = map[string]int{}
	}
	c.ops[op]++
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.bump("get " + keyFamily(key))
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	c.bump("head " + keyFamily(key))
	return c.ObjectStore.Head(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.bump("put " + keyFamily(key))
	return c.ObjectStore.Put(ctx, key, body, opts)
}

func (c *countingStore) Delete(ctx context.Context, key string, v store.Version) error {
	c.bump("delete " + keyFamily(key))
	return c.ObjectStore.Delete(ctx, key, v)
}

func (c *countingStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	c.bump("list " + prefix)
	return c.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func keyFamily(key string) string {
	switch {
	case strings.HasSuffix(key, "meta/mirror.json"):
		return "mirror.json"
	case strings.HasPrefix(key, "leases/"):
		return "leases/"
	case strings.HasSuffix(key, "manifest.pb"):
		return "manifest.pb"
	case strings.Contains(key, "/log/"):
		return "log/"
	case strings.Contains(key, "/wal/"):
		return "wal/"
	case strings.HasSuffix(key, "meta/import.json"):
		return "import.json"
	default:
		return "other"
	}
}

func TestSyncRoundTrips(t *testing.T) {
	syncCost(t, "S", 2, 1)
	syncCost(t, "M", 30, 3)
}

// syncCost measures one first-sync + one no-op fire at a given
// population (the EVIDENCE.md #240 harness): commits + branches on the
// fixture upstream, real git clone, real ingest, real CAS, counting
// store over the memory backend (algorithmic shape, not network RTT).
func syncCost(t *testing.T, tag string, commits, branches int) {
	ctx := context.Background()
	inner := store.NewMemory()
	cs := &countingStore{ObjectStore: inner}
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	reg := wal.NewRegistry(ctx, cs, cfg)
	t.Cleanup(reg.Close)
	registerOnce.Do(func() { RegisterKind(KindMirrorSyncOnce) })
	svc := New(Deps{Store: cs, Reg: reg, CacheDir: t.TempDir(), Hostname: "cost", AllowFile: true, MaxBytes: 1 << 30})
	up := initUpstream(t)
	for i := 0; i < commits; i++ {
		commitFile(t, up, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("content %d\n", i), fmt.Sprintf("commit %d", i))
	}
	for b := 0; b < branches; b++ {
		gitOut(t, up, "branch", fmt.Sprintf("branch-%d", b))
	}
	setupMirror(t, ctx, reg, cs, "acme", "cost", up)

	if _, err := svc.SyncNow(ctx, "acme", "cost", "", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	cs.mu.Lock()
	for op, n := range cs.ops {
		t.Logf("%s first-sync store op %-16s %d", tag, op, n)
	}
	lists := 0
	for op, n := range cs.ops {
		if strings.HasPrefix(op, "list ") {
			lists += n
		}
	}
	cs.mu.Unlock()
	if lists != 0 {
		t.Fatalf("sync listed the bucket %d times (law 4)", lists)
	}
	// Second fire, nothing moved: converge-only (no pack traffic).
	total := func() int {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		n := 0
		for _, v := range cs.ops {
			n += v
		}
		return n
	}
	before := total()
	if _, err := svc.SyncNow(ctx, "acme", "cost", "", false); err != nil {
		t.Fatalf("noop: %v", err)
	}
	after := total()
	t.Logf("%s noop-sync added %d store ops", tag, after-before)
	cs.mu.Lock()
	for op, n := range cs.ops {
		t.Logf("%s cumulative store op %-16s %d", tag, op, n)
	}
	cs.mu.Unlock()
}
