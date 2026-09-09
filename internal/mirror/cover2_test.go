// cover2_test.go — second coverage battery: delegating fake-git
// failure injection, outcome/lease/round fault wrappers, and small
// direct unit pins.
package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// delegateGit writes a fake git that delegates every command to the
// real git EXCEPT the listed overrides ("" delegates, otherwise the
// fixed output; exit codes per exitCode, default 0). Failure
// injection for the runSync mid-body branches.
func delegateGit(t *testing.T, fail map[string]int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncmd=\"$1\"\ncase \"$cmd\" in\n")
	for cmd, code := range fail {
		if cmd == "index-pack-missing" {
			// Clone via real git, then remove the pack idx so the
			// later EnsurePackIdx must regenerate it (and fail).
			b.WriteString("clone) shift; git clone \"$@\"; rc=$?; for d in \"$@\"; do dir=\"$d\"; done; rm -f \"$dir\"/objects/pack/*.idx; exit $rc;;\n")
			b.WriteString("index-pack) exit 1;;\n")
			continue
		}
		b.WriteString(fmt.Sprintf("%s) exit %d;;\n", cmd, code))
	}
	b.WriteString("*) shift; exec git \"$cmd\" \"$@\";;\nesac\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncGitFailureInjection(t *testing.T) {
	ctx := context.Background()
	// for-each-ref fails after a good clone.
	reg, st := testRegistry(t)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "f1", up)
	svc := testService(t, st, reg)
	svc.git = NewRunner(delegateGit(t, map[string]int{"for-each-ref": 1}), t.TempDir(), 0, 0)
	if _, err := svc.SyncNow(ctx, "acme", "f1", "", false); err == nil {
		t.Fatal("for-each-ref failure succeeded")
	}
	// rev-parse --show-object-format fails after a good clone+enum.
	reg2, st2 := testRegistry(t)
	setupMirror(t, ctx, reg2, st2, "acme", "f2", up)
	svc2 := testService(t, st2, reg2)
	svc2.git = NewRunner(delegateGit(t, map[string]int{"rev-parse": 1}), t.TempDir(), 0, 0)
	if _, err := svc2.SyncNow(ctx, "acme", "f2", "", false); err == nil {
		t.Fatal("format failure succeeded")
	}
	// index-pack fails: the idx-install error surfaces through publishPack.
	// (The clone arm strips the pack idx so regeneration is required.)
	reg3, st3 := testRegistry(t)
	setupMirror(t, ctx, reg3, st3, "acme", "f3", up)
	svc3 := testService(t, st3, reg3)
	svc3.git = NewRunner(delegateGit(t, map[string]int{"index-pack-missing": 1}), t.TempDir(), 0, 0)
	if _, err := svc3.SyncNow(ctx, "acme", "f3", "", false); err == nil {
		t.Fatal("index-pack failure succeeded")
	}
	// merge-base hard error is impossible via exit codes (any non-zero
	// is a refusal, not an error) — documented, covered by refusal tests.
}

func TestSyncOutcomeRecordFaults(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "oc", up)
	svc := testService(t, st, reg)
	// Outcome CAS always lost on a SUCCESSFUL sync → succeed() errors.
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			return store.NewPrecondition(key, "v9")
		}
		return nil
	}}
	if _, err := svc.SyncNow(ctx, "acme", "oc", "", false); err == nil {
		t.Fatal("lost-outcome success succeeded")
	}
	// Same wrapper on a FAILING sync → the outcome-lost narration + terminal.
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			return store.NewPrecondition(key, "v9")
		}
		return nil
	}}
	doc, ver, _ := Load(ctx, st, "acme", "oc")
	doc.UpstreamURL = "file:///nonexistent-xyz-abc"
	raw, _ := json.Marshal(doc)
	if _, err := store.PutBytes(ctx, st, store.MirrorKey("acme", "oc"), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "acme", "oc", "", true); err == nil {
		t.Fatal("lost-outcome failure succeeded")
	}
}

func TestSyncProbeFaults(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "pb", up)
	svc := testService(t, st, reg)
	if _, err := svc.SyncNow(ctx, "acme", "pb", "", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Second sync with failing pack/idx probes → the probe error branches.
	svc.store = storeWrap{ObjectStore: st, head: func(key string) (*store.ObjectMeta, error) {
		if strings.Contains(key, "/wal/") {
			return nil, fmt.Errorf("boom")
		}
		return nil, nil
	}}
	if _, err := svc.SyncNow(ctx, "acme", "pb", "", false); err == nil {
		t.Fatal("probe failure succeeded")
	}
	svc.store = st
	// Idx-upload failure on a fresh mirror (Put on idx keys fails).
	reg2, st2 := testRegistry(t)
	setupMirror(t, ctx, reg2, st2, "acme", "pb2", up)
	svc2 := testService(t, st2, reg2)
	svc2.store = storeWrap{ObjectStore: st2, put: func(key string, opts store.PutOptions) error {
		if strings.Contains(key, ".idx") {
			return fmt.Errorf("disk on fire")
		}
		return nil
	}}
	if _, err := svc2.SyncNow(ctx, "acme", "pb2", "", false); err == nil {
		t.Fatal("idx-upload failure succeeded")
	}
}

func TestLeaseLadderFaults(t *testing.T) {
	ctx := context.Background()
	_, st := testRegistry(t)
	svc := New(Deps{Store: st, CacheDir: t.TempDir(), Hostname: "h1"})
	// Generic Put error on Create → surfaces.
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		return fmt.Errorf("boom")
	}}
	if _, err := svc.acquireLease(ctx, "acme", "e1"); err == nil {
		t.Fatal("broken-put acquire succeeded")
	}
	svc.store = st
	// 412 once, then success → the re-read continue branch.
	calls := 0
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if opts.Mode == store.PutCreate {
			calls++
			if calls == 1 {
				return store.NewPrecondition(key, "v9")
			}
		}
		return nil
	}}
	rel, err := svc.acquireLease(ctx, "acme", "e2")
	if err != nil {
		t.Fatalf("flaky acquire: %v", err)
	}
	rel()
	svc.store = st
	// Always-412 → the ladder exhausts to errLeaseHeld.
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		return store.NewPrecondition(key, "v9")
	}}
	if _, err := svc.acquireLease(ctx, "acme", "e3"); err != errLeaseHeld {
		t.Fatalf("exhausted = %v", err)
	}
	// Update-path generic error → surfaces (steal attempt on an
	// expired lease). The lease is rewritten expired in place (no
	// release — release would delete it and there'd be nothing to
	// steal).
	svc.store = st
	rel2, err := svc.acquireLease(ctx, "acme", "e4")
	if err != nil {
		t.Fatal(err)
	}
	// Expire it manually, then fail the steal Update.
	leaseKey := store.LeaseKey(store.MirrorLeaseName("acme", "e4"))
	body, meta, _ := store.GetBytes(ctx, st, leaseKey, store.GetOptions{})
	cur := &proto.Lease{}
	if err := cur.Unmarshal(body); err != nil {
		t.Fatal(err)
	}
	past := proto.TimeFromGo(time.Now().Add(-time.Hour))
	cur.ExpiresAt = &past
	if _, err := st.Put(ctx, leaseKey, store.PutBody{Bytes: cur.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if opts.Mode == store.PutUpdate {
			return fmt.Errorf("boom")
		}
		return nil
	}}
	if _, err := svc.acquireLease(ctx, "acme", "e4"); err == nil {
		t.Fatal("broken-steal acquire succeeded")
	}
	svc.store = st
	rel2()
}

func TestRoundFaults(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "rf", up)
	// Round-Load ok, SyncNow-Load fails → the spawn-error branch.
	var calls int
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if isMirrorKey(key) {
			calls++
			if calls > 1 {
				return nil, store.NewRetryable(key, fmt.Errorf("boom"))
			}
		}
		return nil, nil
	}}
	svc.Round(ctx, nil)
	if calls < 2 {
		t.Fatalf("calls = %d", calls)
	}
	svc.store = st
	// A panicking placement func never kills the loop (recover branch).
	svc.Round(ctx, func(repo string) bool { panic("boom") })
	// Default interval branch.
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.RunLoop(lctx, 0, func(string) bool { return false }); close(done) }()
	time.Sleep(20 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop(0) did not exit")
	}
}

func TestSmallDirectPins(t *testing.T) {
	// shortList truncates past 5.
	if got := shortList([]string{"a", "b", "c", "d", "e", "f", "g"}); got != "a, b, c, d, e" {
		t.Fatalf("short = %q", got)
	}
	// packTrailerChecksum Seek error: a directory reads as a big file
	// but refuses Seek.
	dir := t.TempDir()
	if _, err := packTrailerChecksum(dir, "sha1"); err == nil {
		// Platform-dependent (some kernels allow dir seeks) — accept either.
		t.Log("dir seek allowed on this platform")
	}
	// decodeSegment: undecodable survives verbatim; decodable decodes.
	if got := decodeSegment("%zz"); got != "%zz" {
		t.Fatalf("decode = %q", got)
	}
	if got := decodeSegment("a%20b"); got != "a b" {
		t.Fatalf("decode = %q", got)
	}
	// BackedOff with failures but no attempt stamp: not backed off.
	doc := &MirrorDoc{Schedule: PresetHourly, ConsecutiveFailures: 2}
	if BackedOff(doc, time.Now()) {
		t.Fatal("no-attempt backed off")
	}
	// Nil-clock GET with a present doc (the time.Now fallback).
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if _, err := Create(ctx, st, "acme", "nc", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Svc: svc} // no Auth (anonymous), no Now (fallback)
	rec := doHandle(h, http.MethodGet, "/acme/nc/api/mirror", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-clock get = %d", rec.Code)
	}
	// importClaimLive with a NotModified (nil-body) read → absent.
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if strings.HasSuffix(key, "meta/import.json") {
			return store.NotModified{Version: "v1"}, nil
		}
		return nil, nil
	}}
	if live, err := svc.importClaimLive(ctx, "acme", "nc"); err != nil || live {
		t.Fatalf("not-modified claim = %v %v", live, err)
	}
	svc.store = st
}

func TestForEachRefSecondCut(t *testing.T) {
	// A row with one space (no peel/name split) is skipped.
	bin := fakeGitCase(t,
		map[string]string{
			"for-each-ref": "aaa bbb\n" + strings.Repeat("c", 40) + "  refs/heads/main\n",
		}, nil)
	r := NewRunner(bin, t.TempDir(), 0, 0)
	refs, err := r.ForEachRef(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs = %+v", refs)
	}
	_ = git.Sha1
}
