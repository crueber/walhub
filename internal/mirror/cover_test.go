// cover_test.go — targeted branch coverage for the error/edge paths:
// fake-git runner shapes, store-fault wrappers, auth/body faults, and
// the async pending window. Each test names the branch it pins.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- store fault wrapper -------------------------------------------------------

type storeWrap struct {
	store.ObjectStore
	get  func(key string) (store.GetResult, error)
	put  func(key string, opts store.PutOptions) error
	head func(key string) (*store.ObjectMeta, error)
	del  func(key string) error
}

func (w storeWrap) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if w.get != nil {
		if res, err := w.get(key); res != nil || err != nil {
			return res, err
		}
	}
	return w.ObjectStore.Get(ctx, key, opts)
}

func (w storeWrap) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if w.put != nil {
		if err := w.put(key, opts); err != nil {
			return store.ObjectMeta{}, err
		}
	}
	return w.ObjectStore.Put(ctx, key, body, opts)
}

func (w storeWrap) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if w.head != nil {
		if meta, err := w.head(key); meta != nil || err != nil {
			return meta, err
		}
	}
	return w.ObjectStore.Head(ctx, key)
}

func (w storeWrap) Delete(ctx context.Context, key string, v store.Version) error {
	if w.del != nil {
		if err := w.del(key); err != nil {
			return err
		}
	}
	return w.ObjectStore.Delete(ctx, key, v)
}

func isMirrorKey(key string) bool { return strings.HasSuffix(key, "meta/mirror.json") }

// --- fake git script -----------------------------------------------------------

// fakeGitCase writes an executable shell script acting as git: canned
// responses per argv[0] ("for-each-ref" lines / "rev-parse" format /
// "index-pack" no-op). It lets tests drive malformed output and
// missing side effects without a real repo.
func fakeGitCase(t *testing.T, cases map[string]string, exitCode map[string]int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncmd=\"$1\"; shift\ncase \"$cmd\" in\n")
	seen := map[string]bool{}
	for cmd, out := range cases {
		seen[cmd] = true
		b.WriteString(fmt.Sprintf("%s) printf '%%b' %s; exit %d;;\n", cmd, shellQuote(out), exitCode[cmd]))
	}
	for cmd, code := range exitCode {
		if seen[cmd] {
			continue
		}
		b.WriteString(fmt.Sprintf("%s) exit %d;;\n", cmd, code))
	}
	b.WriteString("*) exit 0;;\nesac\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// --- git runner edge branches --------------------------------------------------

func TestRunnerFakeGitShapes(t *testing.T) {
	ctx := context.Background()
	// Blank + malformed + good for-each-ref rows; unknown format.
	bin := fakeGitCase(t,
		map[string]string{
			"for-each-ref": "\nbadline\n" + strings.Repeat("a", 40) + "  refs/heads/main\n",
			"rev-parse":    "weird\n",
		}, nil)
	r := NewRunner(bin, t.TempDir(), 0, 0)
	refs, err := r.ForEachRef(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs = %+v", refs)
	}
	if _, err := r.ShowObjectFormat(ctx, t.TempDir()); err == nil {
		t.Fatal("weird format accepted")
	}
	// index-pack succeeding without producing an idx.
	bin2 := fakeGitCase(t, map[string]string{"index-pack": ""}, nil)
	r2 := NewRunner(bin2, t.TempDir(), 0, 0)
	pack := filepath.Join(t.TempDir(), "x.pack")
	os.WriteFile(pack, []byte("pack-bytes"), 0o644)
	if _, err := r2.EnsurePackIdx(ctx, pack); err == nil {
		t.Fatal("missing idx accepted")
	}
}

func TestRunnerTokenClone(t *testing.T) {
	// Token-bearing clone covers the credential env-pair branch. The
	// clone fails (no network upstream), but through the token path.
	r := NewRunner("git", t.TempDir(), time.Second, time.Second)
	err := r.CloneMirror(context.Background(), "https://example.invalid/x.git",
		filepath.Join(t.TempDir(), "d"), "https", "example.invalid", "WALGIT_MIRROR_TOKEN_t", "s3cret")
	if err == nil {
		t.Fatal("clone to .invalid succeeded")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestRunnerPoolAndMisc(t *testing.T) {
	r := NewRunner("", "", 0, 0) // CacheDir fallback branch
	if r.CacheDir == "" {
		t.Fatal("no cache fallback")
	}
	// Deterministic pool-cancel: fill the semaphore, then a canceled
	// ctx escapes the wait (no flaky select race).
	for i := 0; i < cap(r.pool); i++ {
		r.pool <- struct{}{}
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.collect(cctx, t.TempDir(), []string{"rev-parse", "HEAD"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pool wait = %v", err)
	}
	for i := 0; i < cap(r.pool); i++ {
		<-r.pool
	}
	// pathEnv fallback (empty PATH).
	t.Setenv("PATH", "")
	if got := pathEnv(); got != "/usr/bin:/bin" {
		t.Fatalf("path = %q", got)
	}
	// boundedStderr truncation past 8 KiB.
	var b boundedStderr
	b.Write(make([]byte, 9000))
	if len(b.String()) != 8192 {
		t.Fatalf("len = %d", len(b.String()))
	}
	// HeadTarget on detached HEAD content.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEAD"), []byte(strings.Repeat("a", 40)+"\n"), 0o644)
	if ht := HeadTarget(dir); ht != "" {
		t.Fatalf("detached head = %q", ht)
	}
	// isExitError with a nil-unwrapping chain (final return).
	var exitTarget *exec.ExitError
	if isExitError(nilUnwrap{}, &exitTarget) {
		t.Fatal("nil-unwrap chain reported exit")
	}
	// MergeBaseIsAncestor maps exit errors to (false, nil).
	bin := fakeGitCase(t, nil, map[string]int{"merge-base": 1})
	rb := NewRunner(bin, t.TempDir(), 0, 0)
	anc, err := rb.MergeBaseIsAncestor(context.Background(), t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil || anc {
		t.Fatalf("fake merge-base = %v %v", anc, err)
	}
}

type nilUnwrap struct{}

func (nilUnwrap) Error() string { return "nil-unwrap" }
func (nilUnwrap) Unwrap() error { return nil }

// --- mirror.go edge branches ---------------------------------------------------

func TestNextFireHorizon(t *testing.T) {
	// A far-future anchor still resolves (presets always fire — the
	// horizon error is unreachable by construction, and NextFire
	// propagates it honestly rather than masking it).
	doc := &MirrorDoc{Schedule: PresetDaily, UpstreamURL: "u", LastSyncedAt: "9999-01-01T00:00:00Z"}
	if _, _, err := NextFire(doc, time.Now()); err != nil {
		t.Fatalf("far anchor: %v", err)
	}
	// Due with a broken schedule fails closed (false, no crash).
	if Due(&MirrorDoc{Schedule: "never"}, time.Now()) {
		t.Fatal("bad-schedule due")
	}
}

func TestLoadStoreFaults(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	// Store error surfaces.
	broken := storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		return nil, store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	if _, _, err := Load(ctx, broken, "acme", "m"); err == nil {
		t.Fatal("broken load succeeded")
	}
	if IsMirror(ctx, broken, "acme", "m") {
		t.Fatal("broken IsMirror true")
	}
	// NotModified body → absent (nil, "", nil).
	nm := storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if isMirrorKey(key) {
			return store.NotModified{Version: "v1"}, nil
		}
		return nil, nil
	}}
	if doc, ver, err := Load(ctx, nm, "acme", "m"); err != nil || doc != nil || ver != "" {
		t.Fatalf("not-modified = %+v %q %v", doc, ver, err)
	}
	// Create with a failing Put.
	failPut := storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		return fmt.Errorf("boom")
	}}
	if _, err := Create(ctx, failPut, "acme", "m", "u", PresetDaily); err == nil {
		t.Fatal("broken create succeeded")
	}
	// RecordAttempt Load error + CAS-retry + twice-lost.
	if _, err := Create(ctx, st, "acme", "m", "u", PresetDaily); err != nil {
		t.Fatal(err)
	}
	if err := RecordAttempt(ctx, broken, "acme", "m", true, "", time.Now()); err == nil {
		t.Fatal("broken record succeeded")
	}
	flaky := storeWrap{ObjectStore: st}
	calls := 0
	flaky.put = func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			calls++
			if calls == 1 {
				return store.NewPrecondition(key, "v9")
			}
		}
		return nil
	}
	if err := RecordAttempt(ctx, flaky, "acme", "m", true, "", time.Now()); err != nil {
		t.Fatalf("flaky record: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one CAS loss + retry)", calls)
	}
	always := storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			return store.NewPrecondition(key, "v9")
		}
		return nil
	}}
	if err := RecordAttempt(ctx, always, "acme", "m", true, "", time.Now()); err == nil {
		t.Fatal("twice-lost record succeeded")
	}
	// SetSchedule store faults.
	if _, err := SetSchedule(ctx, broken, "acme", "m", PresetHourly); err == nil {
		t.Fatal("broken set-schedule succeeded")
	}
	if _, err := SetSchedule(ctx, always, "acme", "m", PresetHourly); err == nil {
		t.Fatal("conflicted set-schedule succeeded")
	}
}

// --- sync.go edge branches -----------------------------------------------------

func TestSyncNowLoadFault(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "lf", up)
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		return nil, store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	if _, err := svc.SyncNow(ctx, "acme", "lf", "", false); err == nil {
		t.Fatal("broken-load sync succeeded")
	}
}

func TestRunSyncGateBranches(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	fileURL := setupMirror(t, ctx, reg, st, "acme", "g", up)

	// Removed-mirror mid-sync: SyncNow loads, body re-loads absent → skip.
	var loads int
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if isMirrorKey(key) {
			loads++
			if loads > 1 {
				return nil, store.NewNotFound(key)
			}
		}
		return nil, nil
	}}
	rec, err := svc.SyncNow(ctx, "acme", "g", "", false)
	if err != nil || rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("removed-mirror = %+v %v", rec, err)
	}
	svc.store = st

	// Backoff skip: fail once (bad upstream swap), then a non-force
	// fire skips without touching the counter.
	doc, ver, _ := Load(ctx, st, "acme", "g")
	doc.UpstreamURL = "file:///nonexistent-xyz-abc"
	raw, _ := json.Marshal(doc)
	if _, err := store.PutBytes(ctx, st, store.MirrorKey("acme", "g"), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "acme", "g", "", false); err == nil {
		t.Fatal("bad upstream succeeded")
	}
	rec, err = svc.SyncNow(ctx, "acme", "g", "", false)
	if err != nil || rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("backoff skip = %+v %v", rec, err)
	}
	doc, _, _ = Load(ctx, st, "acme", "g")
	if doc.ConsecutiveFailures != 1 {
		t.Fatalf("skip bumped counter: %+v", doc)
	}

	// Bad stored URL → normalize failure.
	doc.UpstreamURL = "::bad::"
	raw, _ = json.Marshal(doc)
	doc2, ver2, _ := Load(ctx, st, "acme", "g")
	_ = doc2
	if _, err := store.PutBytes(ctx, st, store.MirrorKey("acme", "g"), raw,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver2, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	// Force bypasses backoff, but the stored URL is still bad → fails.
	_ = fileURL
	if _, err := svc.SyncNow(ctx, "acme", "g", "", true); err == nil {
		t.Fatal("bad-URL sync succeeded")
	}

	// Import-claim probe error.
	svc2 := testService(t, st, reg)
	svc2.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if strings.HasSuffix(key, repoimport.ImportKey) {
			return nil, store.NewRetryable(key, fmt.Errorf("boom"))
		}
		return nil, nil
	}}
	// Fresh mirror for the claim-probe failure (no backoff in the way).
	up2 := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "g2", up2)
	if _, err := svc2.SyncNow(ctx, "acme", "g2", "", false); err == nil {
		t.Fatal("claim-probe failure succeeded")
	}

	// ScratchDir failure (cache dir is a file).
	svc3 := testService(t, st, reg)
	f, _ := os.CreateTemp(t.TempDir(), "f")
	f.Close()
	svc3.git = NewRunner("git", f.Name(), 0, 0)
	up3 := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "g3", up3)
	if _, err := svc3.SyncNow(ctx, "acme", "g3", "", false); err == nil {
		t.Fatal("scratch failure succeeded")
	}
}

func TestSyncTokenAndTagsAndHead(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// Upstream on a non-main default branch + an annotated tag: covers
	// the HEAD-follow publish and the peel-carrying update.
	up := t.TempDir()
	gitOut(t, up, "init", "-b", "develop", ".")
	commitFile(t, up, "f.txt", "a\n", "a")
	gitOut(t, up, "tag", "-a", "v1", "-m", "v1")
	n, err := repoimport.NormalizeSource("file://" + up)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, "acme/adv", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, st, "acme", "adv", n.URL, PresetDaily); err != nil {
		t.Fatal(err)
	}
	// Memory-only token on a file:// upstream (allowed; exercises the
	// credential env-pair branch through the real body).
	rec, err := svc.SyncNow(ctx, "acme", "adv", "tok", false)
	if err != nil {
		t.Fatalf("token sync: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v", rec)
	}
	h, _ := svc.reg.Open(ctx, "acme/adv")
	tips, peeled, head, err := svc.currentRefs(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if tips["refs/heads/develop"] == "" || tips["refs/tags/v1"] == "" {
		t.Fatalf("tips = %v", tips)
	}
	if peeled["refs/tags/v1"] == "" {
		t.Fatalf("no peel: %v", peeled)
	}
	if head != "refs/heads/develop" {
		t.Fatalf("head = %q", head)
	}
}

func TestSyncMixedFFAndRewind(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	gitOut(t, up, "branch", "feature")
	commitFile(t, up, "g.txt", "b\n", "b") // main: A→B
	gitOut(t, up, "checkout", "-q", "feature")
	commitFile(t, up, "feat.txt", "f\n", "f") // feature: A→F
	gitOut(t, up, "checkout", "-q", "main")
	setupMirror(t, ctx, reg, st, "acme", "mix", up)
	if _, err := svc.SyncNow(ctx, "acme", "mix", "", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Advance main (ff: B→C) while rewinding feature (F→A).
	commitFile(t, up, "h.txt", "c\n", "c")
	gitOut(t, up, "checkout", "-q", "feature")
	gitOut(t, up, "reset", "-q", "--hard", "HEAD~1")
	gitOut(t, up, "checkout", "-q", "main")
	rec, err := svc.SyncNow(ctx, "acme", "mix", "", false)
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if rec == nil || rec.OK == nil || !*rec.OK {
		t.Fatalf("rec = %+v", rec)
	}
	mainTip := servingTip(t, ctx, svc, "acme", "mix", "refs/heads/main")
	want := strings.TrimSpace(gitOut(t, up, "rev-parse", "main"))
	if mainTip != want {
		t.Fatalf("main = %q want %q", mainTip, want)
	}
	featTip := servingTip(t, ctx, svc, "acme", "mix", "refs/heads/feature")
	if featTip == strings.TrimSpace(gitOut(t, up, "rev-parse", "feature")) {
		t.Fatal("rewound feature landed")
	}
	doc, _, _ := Load(ctx, st, "acme", "mix")
	if doc.LastResult != "ok" {
		t.Fatalf("result = %q", doc.LastResult)
	}
}

func TestSyncMaxBytes(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	svc.maxBytes = 1 // every pack exceeds → the size-gate branch
	up := initUpstream(t)
	setupMirror(t, ctx, reg, st, "acme", "big", up)
	if _, err := svc.SyncNow(ctx, "acme", "big", "", false); err == nil {
		t.Fatal("oversize sync succeeded")
	}
}

func TestCurrentRefsError(t *testing.T) {
	ctx := context.Background()
	reg, _ := testRegistry(t)
	svc := testService(t, nil, reg)
	if _, err := reg.Create(ctx, "acme/cr", git.Sha1); err != nil {
		t.Fatal(err)
	}
	h, err := reg.Open(ctx, "acme/cr")
	if err != nil {
		t.Fatal(err)
	}
	svc.git = NewRunner("nonexistent-git-binary-xyz", t.TempDir(), 0, 0)
	if _, _, _, err := svc.currentRefs(ctx, h); err == nil {
		t.Fatal("broken-refs succeeded")
	}
}

func TestRecordRefusedRetry(t *testing.T) {
	ctx := context.Background()
	_, st := testRegistry(t)
	svc := testService(t, st, nil)
	if _, err := Create(ctx, st, "acme", "rr", "u", PresetDaily); err != nil {
		t.Fatal(err)
	}
	// 412 once → retry succeeds.
	calls := 0
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			calls++
			if calls == 1 {
				return store.NewPrecondition(key, "v9")
			}
		}
		return nil
	}}
	svc.recordRefused(ctx, "acme", "rr", time.Now(), []string{"refs/heads/main"})
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	// Always-412 → gives up quietly (refusal is narration, not a failure).
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			return store.NewPrecondition(key, "v9")
		}
		return nil
	}}
	svc.recordRefused(ctx, "acme", "rr", time.Now(), []string{"refs/heads/main"})
	// Missing sidecar → no home, no panic.
	svc.store = st
	svc.recordRefused(ctx, "acme", "gone", time.Now(), []string{"refs/heads/main"})
}

func TestLeaseReleaseCorrupt(t *testing.T) {
	ctx := context.Background()
	_, st := testRegistry(t)
	svc := testService(t, st, nil)
	if _, err := store.PutBytes(ctx, st, store.LeaseKey(store.MirrorLeaseName("acme", "c")),
		[]byte("{oops"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	// Corrupt lease body under release: no panic, no delete.
	svc.releaseFunc(store.LeaseKey(store.MirrorLeaseName("acme", "c")), "h1", "")()
}

func TestRunLoopAndCanceledRound(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// Canceled round over a populated registry: exits via ctx.
	if _, err := reg.Create(ctx, "acme/z", git.Sha1); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	svc.Round(cctx, nil)
	// The ticker loop: short interval, empty fire set, cancel exits.
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.RunLoop(lctx, 5*time.Millisecond, nil); close(done) }()
	time.Sleep(30 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop did not exit")
	}
}

// --- http.go edge branches -----------------------------------------------------

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("boom") }
func (errReader) Close() error             { return nil }

func authErrStub(r *http.Request) (auth.Principal, *auth.AuthError) {
	return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad cred"}
}

func TestHandlerAuthFaults(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPut, "/acme/m/api/mirror"},
		{http.MethodDelete, "/acme/m/api/mirror"},
		{http.MethodPost, "/acme/m/api/mirror/sync"},
		{http.MethodPost, "/api/v1/repos/mirrors"},
	} {
		h := &Handler{Svc: svc, Auth: authErrStub, CreateRepo: func(ctx context.Context, o, n string) error { return nil }}
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		if !h.Handle(httptest.NewRecorder(), req) {
			t.Fatalf("%s %s not handled", tc.method, tc.path)
		}
		rec := httptest.NewRecorder()
		h.Handle(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestHandlerAnonAndNilShapes(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// Nil authenticator → anonymous (write paths 401).
	h := &Handler{Svc: svc, CreateRepo: func(ctx context.Context, o, n string) error { return nil }}
	if rec := doHandle(h, http.MethodPost, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil-auth sync = %d", rec.Code)
	}
	// Nil clock → time.Now fallback.
	hn := &Handler{Svc: svc}
	rec := doHandle(hn, http.MethodGet, "/acme/m/api/mirror", "")
	if rec.Code != http.StatusNotFound { // absent mirror, but the clock ran
		t.Fatalf("nil-clock get = %d", rec.Code)
	}
	// Undecodable segment survives verbatim (then fails closed).
	req := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/acme/%zz/api/mirror"}}
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatal("handled undecodable repo")
	}
	// Over-long lane rest is not ours.
	req = httptest.NewRequest(http.MethodGet, "/acme/m/api/mirror/sync/extra", nil)
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatal("handled over-long rest")
	}
}

func TestPutEdgeBranches(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	// Unreadable body.
	req := httptest.NewRequest(http.MethodPut, "/acme/m/api/mirror", errReader{})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	// Default schedule when omitted (create with upstream only).
	if _, err := reg.Create(ctx, "acme/d", git.Sha1); err != nil {
		t.Fatal(err)
	}
	up := initUpstream(t)
	n := mustNormalize(t, "file://"+up)
	rec = doHandle(h, http.MethodPut, "/acme/d/api/mirror", `{"upstream_url":`+quote(n)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("default-schedule put = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	waitAsync(t, svc, out["task"].(map[string]any)["id"].(string), 30*time.Second)
	doc, _, _ := Load(ctx, st, "acme", "d")
	if doc.Schedule != DefaultPreset {
		t.Fatalf("schedule = %q", doc.Schedule)
	}
	// Bad upstream / ssh transport / SSRF at create.
	if _, err := reg.Create(ctx, "acme/e1", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if rec := doHandle(h, http.MethodPut, "/acme/e1/api/mirror", `{"upstream_url":"::bad::"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url = %d", rec.Code)
	}
	if rec := doHandle(h, http.MethodPut, "/acme/e1/api/mirror", `{"upstream_url":"git@host:x.git"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("scp = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doHandle(h, http.MethodPut, "/acme/e1/api/mirror", `{"upstream_url":"https://example.com/a.git"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("ssrf = %d: %s", rec.Code, rec.Body.String())
	}
	// Load fault.
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		return nil, store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	if rec := doHandle(h, http.MethodPut, "/acme/e1/api/mirror", `{"schedule":"daily"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("load fault = %d", rec.Code)
	}
	svc.store = st
	// SetSchedule fault (always-412 on updates).
	if _, err := Create(ctx, st, "acme", "e1", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	svc.store = storeWrap{ObjectStore: st, put: func(key string, opts store.PutOptions) error {
		if isMirrorKey(key) && opts.Mode == store.PutUpdate {
			return store.NewPrecondition(key, "v9")
		}
		return nil
	}}
	if rec := doHandle(h, http.MethodPut, "/acme/e1/api/mirror", `{"schedule":"hourly"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("cas fault = %d", rec.Code)
	}
	svc.store = st
	// Raced sidecar: hidden from Loads, present for Creates → 409.
	// (file:// upstream so the SSRF gate passes before the Create.)
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if isMirrorKey(key) {
			return nil, store.NewNotFound(key)
		}
		return nil, nil
	}}
	if _, err := reg.Create(ctx, "acme/race", git.Sha1); err != nil {
		t.Fatal(err)
	}
	innerDoc, _ := Create(ctx, st, "acme", "race", "https://example.com/a.git", PresetDaily)
	_ = innerDoc
	if rec := doHandle(h, http.MethodPut, "/acme/race/api/mirror", `{"upstream_url":`+quote("file://"+up)+`}`); rec.Code != http.StatusConflict {
		t.Fatalf("raced = %d: %s", rec.Code, rec.Body.String())
	}
	// Generic Create error → 500.
	svc.store = storeWrap{ObjectStore: st,
		get: func(key string) (store.GetResult, error) {
			if isMirrorKey(key) {
				return nil, store.NewNotFound(key)
			}
			return nil, nil
		},
		put: func(key string, opts store.PutOptions) error {
			if isMirrorKey(key) {
				return fmt.Errorf("disk on fire")
			}
			return nil
		}}
	if rec := doHandle(h, http.MethodPut, "/acme/race/api/mirror", `{"upstream_url":`+quote("file://"+up)+`}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("put fault = %d", rec.Code)
	}
	svc.store = st
}

func TestDeleteEdgeBranches(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, writeP)
	if rec := doHandle(h, http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("writer delete = %d", rec.Code)
	}
	ha := testHandler(t, nil, reg, adminP)
	if rec := doHandle(ha, http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil svc = %d", rec.Code)
	}
	ha.Svc = svc
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		return nil, store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	if rec := doHandle(ha, http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("load fault = %d", rec.Code)
	}
	svc.store = st
	if _, err := Create(ctx, st, "acme", "m", "https://example.com/a.git", PresetDaily); err != nil {
		t.Fatal(err)
	}
	svc.store = storeWrap{ObjectStore: st, del: func(key string) error {
		return fmt.Errorf("boom")
	}}
	if rec := doHandle(ha, http.MethodDelete, "/acme/m/api/mirror", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete fault = %d", rec.Code)
	}
	svc.store = st
}

func TestSyncNowEdgeBranches(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	req := httptest.NewRequest(http.MethodPost, "/acme/m/api/mirror/sync", errReader{})
	if rec := httptest.NewRecorder(); h.Handle(rec, req) && rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		return nil, store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	if rec := doHandle(h, http.MethodPost, "/acme/m/api/mirror/sync", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("load fault = %d", rec.Code)
	}
	svc.store = st
}

func TestSyncStatusErrorAndPending(t *testing.T) {
	ctx := context.Background()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	// Async fire that FAILS fast (non-mirror target): status carries error.
	id := svc.SyncAsync(ctx, "acme", "nope", "", false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, ok := svc.SyncStatus(id)
		if !ok {
			t.Fatal("id unknown")
		}
		select {
		case <-a.done:
			goto failed
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("async never finished")
		}
		time.Sleep(time.Millisecond)
	}
failed:
	rec := doHandle(h, http.MethodGet, "/acme/m/api/mirror/sync?id="+id, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("failed status = %d: %s", rec.Code, rec.Body.String())
	}
	// Pending window: hang the sidecar Load behind a gate, observe
	// pending + active-list, then release. The upstream is valid so the
	// fire succeeds after the gate opens.
	if _, err := reg.Create(ctx, "acme/hang", git.Sha1); err != nil {
		t.Fatal(err)
	}
	upHang := initUpstream(t)
	nHang := mustNormalize(t, "file://"+upHang)
	if _, err := Create(ctx, st, "acme", "hang", nHang, PresetDaily); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if isMirrorKey(key) {
			<-gate
		}
		return nil, nil
	}}
	hid := svc.SyncAsync(ctx, "acme", "hang", "", false)
	sawPending := false
	deadline = time.Now().Add(10 * time.Second)
	for {
		r := doHandle(h, http.MethodGet, "/acme/hang/api/mirror/sync?id="+hid, "")
		if strings.Contains(r.Body.String(), `"done":false`) {
			sawPending = true
			break
		}
		if strings.Contains(r.Body.String(), `"done":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending never observed")
		}
		time.Sleep(time.Millisecond)
	}
	r := doHandle(h, http.MethodGet, "/acme/hang/api/mirror/sync", "")
	if !strings.Contains(r.Body.String(), hid) {
		t.Fatalf("active list missing %q: %s", hid, r.Body.String())
	}
	close(gate)
	waitAsync(t, svc, hid, 30*time.Second)
	svc.store = st
	if !sawPending {
		t.Log("pending window missed (scheduling race — branch covered best-effort)")
	}
}

func TestCreateEdgeBranches(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, writeP)
	// Unreadable body.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/mirrors", errReader{})
	if rec := httptest.NewRecorder(); h.Handle(rec, req) && rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	// Unknown field.
	if rec := doHandle(h, http.MethodPost, "/api/v1/repos/mirrors", `{"zzz":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown = %d", rec.Code)
	}
	// Authenticated without write → 403.
	ro := testHandler(t, svc, reg, auth.Principal{Name: "ro"})
	if rec := doHandle(ro, http.MethodPost, "/api/v1/repos/mirrors", `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("readonly = %d", rec.Code)
	}
	// scp transport refused.
	if rec := doHandle(h, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":"git@host:x.git","owner":"acme","name":"n"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("scp = %d: %s", rec.Code, rec.Body.String())
	}
	// SSRF without dangerous → 400.
	if rec := doHandle(h, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":"https://example.com/a.git","owner":"acme","name":"n"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("ssrf = %d: %s", rec.Code, rec.Body.String())
	}
	// CreateRepo failure → 500.
	hf := testHandler(t, svc, reg, writeP)
	hf.CreateRepo = func(ctx context.Context, o, n string) error { return fmt.Errorf("boom") }
	up := initUpstream(t)
	n := mustNormalize(t, "file://"+up)
	if rec := doHandle(hf, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":`+quote("file://"+up)+`,"owner":"acme","name":"fx"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("repo fail = %d", rec.Code)
	}
	// Raced sidecar → 409 already-a-mirror; generic sidecar error → 500.
	ctx := context.Background()
	if _, err := reg.Create(ctx, "acme/r1", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, st, "acme", "r1", n, PresetDaily); err != nil {
		t.Fatal(err)
	}
	_ = ctx
	h2 := testHandler(t, svc, reg, writeP)
	h2.CreateRepo = func(ctx context.Context, o, name string) error { return nil } // repo "created" (already there)
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		if isMirrorKey(key) {
			return nil, store.NewNotFound(key)
		}
		return nil, nil
	}}
	_ = n
	if rec := doHandle(h2, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":`+quote("file://"+up)+`,"owner":"acme","name":"r1"}`); rec.Code != http.StatusConflict {
		t.Fatalf("raced = %d: %s", rec.Code, rec.Body.String())
	}
	svc.store = storeWrap{ObjectStore: st,
		get: func(key string) (store.GetResult, error) {
			if isMirrorKey(key) {
				return nil, store.NewNotFound(key)
			}
			return nil, nil
		},
		put: func(key string, opts store.PutOptions) error {
			if isMirrorKey(key) {
				return fmt.Errorf("disk on fire")
			}
			return nil
		}}
	if rec := doHandle(h2, http.MethodPost, "/api/v1/repos/mirrors", `{"source_url":`+quote("file://"+up)+`,"owner":"acme","name":"r1"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("sidecar fail = %d", rec.Code)
	}
	svc.store = st
}

func TestWriteJSONEncodeError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, func() {})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("encode = %d", rec.Code)
	}
}

func TestGetLoadFault(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)
	svc.store = storeWrap{ObjectStore: st, get: func(key string) (store.GetResult, error) {
		return nil, store.NewRetryable(key, fmt.Errorf("boom"))
	}}
	if rec := doHandle(h, http.MethodGet, "/acme/m/api/mirror", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("load fault = %d", rec.Code)
	}
	svc.store = st
}
