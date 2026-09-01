package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
)

func TestStoreErrorUnwrap(t *testing.T) {
	inner := errors.New("root cause")
	se := NewRetryable("k", inner)
	if got := errors.Unwrap(se); got != inner {
		t.Fatalf("Unwrap = %v", got)
	}
	if errors.Unwrap(NewNotFound("k")) != nil {
		t.Fatal("Unwrap without Err")
	}
}

func TestIsGetResultMarkers(t *testing.T) {
	// The marker methods exist to seal the GetResult union; call them so the
	// compiler-generated bodies are exercised.
	NotModified{}.isGetResult()
	Object{}.isGetResult()
}

func TestNewFilesystemFromConfig(t *testing.T) {
	f, err := NewFilesystem(&config.Store{Root: t.TempDir()})
	if err != nil || f == nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	if _, err := NewFilesystem(&config.Store{Root: ""}); !IsInvalidArgument(err) {
		t.Fatalf("empty root: %v", err)
	}
}

func TestFilesystemFsyncDirPaths(t *testing.T) {
	f := newFS(t)
	// Lease and manifest keys trigger the parent-dir fsync after rename.
	if _, err := f.Put(context.Background(), "leases/heartbeat.pb", PutBody{Bytes: []byte("x")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(context.Background(), "repos/o/r/manifest.pb", PutBody{Bytes: []byte("x")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemCopyExactNegative(t *testing.T) {
	f := newFS(t)
	if _, err := f.Put(context.Background(), "neg", PutBody{Stream: strings.NewReader("x"), StreamLen: -1}, PutOptions{}); err == nil {
		t.Fatal("negative StreamLen must fail")
	}
	// copyExact: reader error propagates.
	if err := copyExact(&strings.Builder{}, errReader{}, 4); err == nil {
		t.Fatal("reader error must propagate")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestFilesystemLockSidecarErrors(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	// acquireLock fails when the sidecar path is unusable (a directory).
	if err := os.MkdirAll(f.path("kdir.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(f.path("kdir")); err == nil {
		t.Fatal("acquireLock on directory sidecar must fail")
	}
	// PutUpdate: the lock acquisition error maps to Other.
	if _, err := f.Put(ctx, "kdir", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: "v"}); !IsOther(err) {
		t.Fatalf("update with unusable lock: %v", err)
	}
	// Portable create: the lock acquisition error maps to Other.
	forcePortableRename.Store(true)
	if _, err := f.Put(ctx, "kdir", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutCreate}); !IsOther(err) {
		t.Fatalf("portable create with unusable lock: %v", err)
	}
	forcePortableRename.Store(false)
	// Overwrite onto an existing DIRECTORY fails at rename → Other.
	if err := os.MkdirAll(f.path("dirasobj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(ctx, "dirasobj", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutOverwrite}); !IsPreconditionFailed(err) && !IsOther(err) {
		t.Fatalf("overwrite onto directory: %v", err)
	}
}

func TestFilesystemHeadAndListSymlinkGuards(t *testing.T) {
	root := t.TempDir()
	f, err := NewFilesystemRoot(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// Head of an existing object behind a symlinked dir is rejected.
	if _, err := f.Head(ctx, "link/secret"); !IsInvalidArgument(err) {
		t.Fatalf("head through symlink: %v", err)
	}
	// Listing resolves symlinks by what they point at (no error, no leak).
	if err := f.List(ctx, "", "", func(ObjectMeta) error { return nil }); err != nil {
		t.Fatalf("list with symlinked dir: %v", err)
	}
	// A broken symlink entry is skipped (e.Info fails).
	if err := os.Symlink(filepath.Join(root, "nope"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	if err := f.List(ctx, "", "", func(ObjectMeta) error { return nil }); err != nil {
		t.Fatalf("list with broken symlink: %v", err)
	}
}

func TestFilesystemWritePathBlockedByFile(t *testing.T) {
	f := newFS(t)
	// A parent path component occupied by a FILE blocks MkdirAll → Other.
	if _, err := f.Put(context.Background(), "plain", PutBody{Bytes: []byte("p")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(context.Background(), "plain/child", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsOther(err) {
		t.Fatalf("write under a file parent: %v", err)
	}
}

func TestFilesystemComposeMissingSourceFile(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	// checkKey passes but the object does not exist: os.Open fails ENOENT →
	// NotFound (distinct from the pre-check paths).
	if _, err := f.Compose(ctx, "c", []string{"ghost"}, PutOptions{}); !IsNotFound(err) {
		t.Fatalf("compose missing source file: %v", err)
	}
}

func TestFilesystemBumpTokenErrors(t *testing.T) {
	// Chtimes on a missing temp errors; the original token is returned.
	tok := Version("5:1000")
	if got, err := bumpTokenIfCollision(filepath.Join(t.TempDir(), "missing"), tok, tok); err == nil || got != tok {
		t.Fatalf("bump on missing tmp: %q %v", got, err)
	}
}

func TestMemoryURLsCancelledContext(t *testing.T) {
	m := NewMemory()
	m.Latency = 20 * time.Millisecond
	ctx := context.Background()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.SignedGetURL(cctx, "k", time.Minute); !IsRetryable(err) {
		t.Fatalf("SignedGetURL cancelled: %v", err)
	}
	if _, err := m.AccelTarget(cctx, "k"); !IsRetryable(err) {
		t.Fatalf("AccelTarget cancelled: %v", err)
	}
	if _, err := m.Compose(cctx, "c", []string{"a"}, PutOptions{}); !IsRetryable(err) {
		t.Fatalf("Compose cancelled: %v", err)
	}
}

func TestExistsError(t *testing.T) {
	m := NewMemory()
	m.Latency = 20 * time.Millisecond
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := Exists(cctx, m, "k"); ok || !IsRetryable(err) {
		t.Fatalf("Exists error: %v %v", ok, err)
	}
}

func TestS3ClockFallback(t *testing.T) {
	// A zero-value S3 (no test hook) falls back to time.Now.
	s := &S3{}
	if s.clock().IsZero() {
		t.Fatal("clock fallback")
	}
}

func TestS3SignedHeadersForAllBranches(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Amz-Date", "t")
	hdr.Set("X-Amz-Content-Sha256", "hash")
	hdr.Set("Content-Type", "application/x-protobuf")
	hdr.Set("Range", "bytes=0-1")
	hdr.Set("X-Amz-Security-Token", "tok")
	hdr.Set("X-Amz-Copy-Source", "bkt/src")
	hdr.Set("X-Amz-Copy-Source-Range", "bytes=0-4")
	headers := signedHeadersFor(hdr, "h.example")
	got := ""
	for _, h := range headers {
		got += h.name + ";"
	}
	want := "host;x-amz-date;x-amz-content-sha256;content-type;range;x-amz-security-token;x-amz-copy-source;x-amz-copy-source-range;"
	if got != want {
		t.Fatalf("signed headers %q want %q", got, want)
	}
}

func TestS3HeadAndListErrorStatuses(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)

	// Head hitting a non-retryable status surfaces the mapped error.
	f.statusOverride = func(method, key string, r *http.Request) int {
		if method == http.MethodHead {
			return http.StatusMethodNotAllowed
		}
		return 0
	}
	if _, err := s.Head(ctx, "k"); !IsInvalidArgument(err) {
		t.Fatalf("head 405: %v", err)
	}
	f.statusOverride = nil

	// List page failing with 500 → Retryable after retries.
	f.statusOverride = func(method, key string, r *http.Request) int {
		if r.Method == http.MethodGet && r.URL.Path == "/bkt/" {
			return http.StatusBadGateway
		}
		return 0
	}
	if err := s.List(ctx, "", "", func(ObjectMeta) error { t.Fatal("unexpected"); return nil }); !IsRetryable(err) {
		t.Fatalf("list 502: %v", err)
	}
	f.statusOverride = nil

	// ListPrefixes: callback errors propagate; a failing page errors too.
	s3fakePut(t, s, "a/1", "x", PutOptions{})
	sentinel := errors.New("stop")
	if err := s.ListPrefixes(ctx, "", func(string) error { return sentinel }); err != sentinel {
		t.Fatalf("listPrefixes callback: %v", err)
	}
	f.statusOverride = func(method, key string, r *http.Request) int {
		if r.Method == http.MethodGet && r.URL.Path == "/bkt/" {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	if err := s.ListPrefixes(ctx, "", func(string) error { t.Fatal("unexpected"); return nil }); !IsRetryable(err) {
		t.Fatalf("listPrefixes 503: %v", err)
	}
	f.statusOverride = nil
}

func TestS3ComposeCopyPartFailure(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) { c.MultipartPartSize = 8 << 20 })

	s3fakePut(t, s, "s1", strings.Repeat("A", 6<<20), PutOptions{})
	// The copy part PUT fails: Compose aborts and surfaces the error.
	abortsBefore := len(f.aborts)
	f.statusOverride = func(method, key string, r *http.Request) int {
		if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "" && r.Header.Get("X-Amz-Copy-Source") != "" {
			return http.StatusInternalServerError
		}
		return 0
	}
	if _, err := s.Compose(ctx, "cat", []string{"s1"}, PutOptions{}); err == nil {
		t.Fatal("copy-part failure must surface")
	}
	f.statusOverride = nil
	if len(f.aborts) != abortsBefore+1 {
		t.Fatal("failed compose did not abort")
	}
	// A source that disappears between Head and copy is also handled: the
	// ranged-read fallback of a tiny source surfaces Get errors.
	s3fakePut(t, s, "tiny", "ab", PutOptions{})
	if _, err := s.Compose(ctx, "cat2", []string{"tiny", "ghost-tail"}, PutOptions{}); !IsNotFound(err) {
		t.Fatalf("compose missing tail: %v", err)
	}
}

func TestS3DeleteHeadError(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	s := newS3Client(s3Config{Bucket: "bkt", Endpoint: deadURL, Region: "r", ForcePathStyle: true,
		Creds: testCreds, MaxRetries: 1, MultipartThreshold: 1, MultipartPartSize: 1})
	// CAS delete: the pre-flight HEAD fails → the error surfaces.
	if err := s.Delete(context.Background(), "k", "v"); !IsRetryable(err) {
		t.Fatalf("cas delete with dead endpoint: %v", err)
	}
}

func TestRenameat2SysnumTable(t *testing.T) {
	// The table covers the mainstream arches; the running arch must resolve.
	if got := renameat2Sysnum(); got == 0 && renameat2Sysnums[runtime.GOARCH] != 0 {
		t.Fatalf("running arch %s missing from the table", runtime.GOARCH)
	}
	for arch, num := range map[string]uintptr{"amd64": 316, "arm64": 276, "386": 353, "arm": 382, "s390x": 347} {
		if renameat2Sysnums[arch] != num {
			t.Errorf("sysnum[%s] = %d, want %d", arch, renameat2Sysnums[arch], num)
		}
	}
	if _, ok := renameat2Sysnums["sparc64"]; ok {
		t.Error("unexpected arch in table")
	}
}

func TestFilesystemDeletePermissionError(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	if _, err := f.Put(ctx, "ro/x", PutBody{Bytes: []byte("x")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.path("ro"), 0o555); err != nil {
		t.Skipf("chmod unsupported: %v", err)
	}
	defer os.Chmod(f.path("ro"), 0o755)
	if err := f.Delete(ctx, "ro/x", ""); !IsOther(err) {
		t.Fatalf("delete without write permission: %v", err)
	}
	if err := f.Delete(ctx, "ro/x", "someversion"); !IsOther(err) {
		t.Fatalf("cas delete without write permission: %v", err)
	}
}

func TestFsyncDirError(t *testing.T) {
	if err := fsyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("fsyncDir on a missing dir must error")
	}
}

func TestFilesystemListBulkPermitCancelled(t *testing.T) {
	f := newFS(t)
	f.sem = NewWeighted(1)
	if err := f.sem.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Bulk prefixes queue on the semaphore; a cancelled ctx → Retryable.
	if err := f.List(cctx, "wal/", "", func(ObjectMeta) error { return nil }); !IsRetryable(err) {
		t.Fatalf("bulk list cancelled: %v", err)
	}
	if err := f.ListPrefixes(cctx, "wal/", func(string) error { return nil }); !IsRetryable(err) {
		t.Fatalf("bulk listPrefixes cancelled: %v", err)
	}
}

func TestFilesystemComposeSourceIsDirectory(t *testing.T) {
	f := newFS(t)
	if err := os.MkdirAll(f.path("somedir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory source opens fine but reading it errors → mapped error.
	if _, err := f.Compose(context.Background(), "c", []string{"somedir"}, PutOptions{}); err == nil || IsNotFound(err) {
		t.Fatalf("compose over a directory: %v", err)
	}
}

func TestFilesystemHeadUnderFileParent(t *testing.T) {
	f := newFS(t)
	if _, err := f.Put(context.Background(), "plain", PutBody{Bytes: []byte("p")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Head under a file parent: lstat fails with ENOTDIR → Other.
	if _, err := f.Head(context.Background(), "plain/child"); !IsOther(err) {
		t.Fatalf("head under file parent: %v", err)
	}
}

func TestFilesystemWalkRacedAwayFile(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	for _, k := range []string{"r/a", "r/b"} {
		if _, err := f.Put(ctx, k, PutBody{Bytes: []byte("x")}, PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// The callback deletes "b" while "a" is visited; the walk stats "b" after
	// it is gone and must skip it (raced-away), not error.
	seen := 0
	if err := f.List(ctx, "r/", "", func(m ObjectMeta) error {
		seen++
		if m.Key == "r/a" {
			return f.Delete(ctx, "r/b", "")
		}
		return nil
	}); err != nil {
		t.Fatalf("walk with raced-away file: %v", err)
	}
	if seen != 1 {
		t.Fatalf("raced-away file was visited: %d", seen)
	}
}

func TestPartSizeForMidWindow(t *testing.T) {
	// ceil(size/1024) inside the clamp window: 100 GiB → 100 MiB part size.
	if got := partSizeFor(100 << 30); got != 100<<20 {
		t.Fatalf("mid-window part size = %d", got)
	}
}

func TestS3UploadPartCopyBadXML(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) { c.MultipartPartSize = 8 << 20 })
	s3fakePut(t, s, "s1", strings.Repeat("A", 6<<20), PutOptions{})
	f.badCopyXML.Store(true)
	if _, err := s.Compose(ctx, "cat", []string{"s1"}, PutOptions{}); !IsOther(err) {
		t.Fatalf("bad CopyPart xml: %v", err)
	}
	f.badCopyXML.Store(false)
}

func TestS3UploadPartDataMissingETag(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) {
		c.MultipartThreshold = 5 << 20
		c.MultipartPartSize = 1 << 20
	})
	f.partMissingETag.Store(true)
	if _, err := s.Put(ctx, "mp", PutBody{Bytes: make([]byte, 5<<20+1)}, PutOptions{Mode: PutOverwrite}); !IsOther(err) {
		t.Fatalf("missing part ETag: %v", err)
	}
	f.partMissingETag.Store(false)
}

func TestS3ReadRangeShortBody(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	// A < 5 MiB tail takes the ranged-read fallback; the GET body dies
	// mid-flight and readRange maps it to Retryable (§2.4).
	s3fakePut(t, s, "tiny", strings.Repeat("A", 1<<20), PutOptions{})
	f.dropRangeBody.Store(true)
	_, err := s.Compose(ctx, "cat", []string{"tiny"}, PutOptions{})
	if !IsRetryable(err) {
		t.Fatalf("dropped range body: %v", err)
	}
	f.dropRangeBody.Store(false)
}

func TestIsGetResultViaInterface(t *testing.T) {
	// Invoke the sealed-union markers through the interface (defeats inlining).
	for _, g := range []GetResult{NotModified{}, Object{}} {
		g.isGetResult()
	}
}

func TestFilesystemPutUpdateOntoDirectory(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	if err := os.MkdirAll(f.path("dirasobj"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The directory lstats fine, so Update CAS passes and the rename fails.
	tok, _, err := statToken(f.path("dirasobj"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(ctx, "dirasobj", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: tok}); !IsOther(err) && !IsPreconditionFailed(err) {
		t.Fatalf("update onto directory: %v", err)
	}
}

func TestFilesystemDeleteThroughSymlink(t *testing.T) {
	root := t.TempDir()
	f, err := NewFilesystemRoot(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(context.Background(), "link/secret", ""); !IsInvalidArgument(err) {
		t.Fatalf("delete through symlink: %v", err)
	}
}

func TestFilesystemReservedDirNamesInvisibleToListing(t *testing.T) {
	f := newFS(t)
	// A directory whose NAME is in the reserved namespace is skipped.
	if err := os.MkdirAll(f.path("skipme.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.path("skiptoo.tmp-d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.List(context.Background(), "", "", func(ObjectMeta) error { t.Fatal("reserved dir leaked"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemListPrefixesUnderFile(t *testing.T) {
	f := newFS(t)
	if _, err := f.Put(context.Background(), "plain", PutBody{Bytes: []byte("p")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// ReadDir on a path that is a file → ENOTDIR → Other (not silent empty).
	if err := f.ListPrefixes(context.Background(), "plain/sub", func(string) error { t.Fatal("unexpected"); return nil }); err == nil {
		t.Fatal("prefixes under a file must error")
	}
}

func TestFilesystemGetRangedBulkCancelled(t *testing.T) {
	f := newFS(t)
	f.sem = NewWeighted(1)
	if err := f.sem.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A RANGED get of any key takes the bulk semaphore → Retryable.
	if _, err := f.Get(cctx, "k", GetOptions{Range: &[2]int64{0, 1}}); !IsRetryable(err) {
		t.Fatalf("ranged get cancelled: %v", err)
	}
}

func TestFilesystemComposeGuards(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	if _, err := f.Put(ctx, "s", PutBody{Bytes: []byte("x")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Compose dest under a file parent → resolveForWrite error.
	if _, err := f.Put(ctx, "plain", PutBody{Bytes: []byte("p")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Compose(ctx, "plain/cat", []string{"s"}, PutOptions{}); err == nil {
		t.Fatal("compose under a file parent must fail")
	}
	// Compose with a symlinked source dir → Invalid.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.path("link")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Compose(ctx, "c", []string{"link/secret"}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("compose through symlink: %v", err)
	}
	// Bulk dst queues on the semaphore: cancelled ctx → Retryable.
	f.sem = NewWeighted(1)
	if err := f.sem.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Compose(cctx, "wal/cat", []string{"s"}, PutOptions{}); !IsRetryable(err) {
		t.Fatalf("bulk compose cancelled: %v", err)
	}
}

func TestS3RegionEnvDefault(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	t.Setenv("AWS_REGION", "")
	s, err := NewS3(&config.Store{Bucket: "b"})
	if err != nil || s.region != "us-east-1" {
		t.Fatalf("default region: %q %v", s.region, err)
	}
}

func TestS3RetryContextDone(t *testing.T) {
	// A retryable error with an already-cancelled context returns immediately.
	s := &S3{maxRetries: 3}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := s.retry(cctx, true, func() error {
		calls++
		return NewRetryable("k", errors.New("x"))
	})
	if !IsRetryable(err) || calls != 1 {
		t.Fatalf("retry on cancelled ctx: calls=%d err=%v", calls, err)
	}
}

func TestS3GetBadContentRange(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	s3fakePut(t, s, "k", "0123456789", PutOptions{})
	f.noContentRange.Store(true)
	if _, err := s.Get(ctx, "k", GetOptions{Range: &[2]int64{2, 5}}); !IsOther(err) {
		t.Fatalf("missing Content-Range: %v", err)
	}
	f.noContentRange.Store(false)
}

func TestS3CompleteFailureAborts(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) {
		c.MultipartThreshold = 5 << 20
		c.MultipartPartSize = 1 << 20
	})
	f.completeFail.Store(true)
	if _, err := s.Put(ctx, "cmp", PutBody{Bytes: make([]byte, 5<<20+1)}, PutOptions{Mode: PutOverwrite}); err == nil {
		t.Fatal("complete failure must surface")
	}
	f.completeFail.Store(false)
	if hm, _ := s.Head(ctx, "cmp"); hm != nil {
		t.Fatal("failed complete left an object")
	}
}

func TestS3SessionTokenSigning(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) {
		c.Creds.Session = "SESSIONTOKEN"
	})
	// Every request signs X-Amz-Security-Token when a session is set.
	if _, err := s.Head(ctx, "k"); err != nil {
		t.Fatalf("head with session: %v", err)
	}
	s3fakePut(t, s, "k", "v", PutOptions{})
	s3fakePutBytes(t, s, "mp", make([]byte, 5<<20+1), PutOptions{Mode: PutOverwrite})
}

func TestS3DeleteAbsent404StillOk(t *testing.T) {
	// Real S3 answers 204, but some gateways answer 404 for absent deletes;
	// unconditional delete must stay Ok either way.
	ctx := context.Background()
	f := newS3Fake()
	f.absentDelete404.Store(true)
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	if err := s.Delete(ctx, "absent", ""); err != nil {
		t.Fatalf("absent delete via 404: %v", err)
	}
}

func TestS3ListTruncatedWithoutToken(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	s3fakePut(t, s, "a", "x", PutOptions{})
	// IsTruncated with no continuation token: stop cleanly (defensive).
	f.truncatedNoToken.Store(true)
	if err := s.List(ctx, "", "", func(ObjectMeta) error { return nil }); err != nil {
		t.Fatalf("truncated-no-token list: %v", err)
	}
	f.truncatedNoToken.Store(false)
}

func TestS3ComposePrecheckErrors(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	s := newS3Client(s3Config{Bucket: "bkt", Endpoint: deadURL, Region: "r", ForcePathStyle: true,
		Creds: testCreds, MaxRetries: 1, MultipartThreshold: 1, MultipartPartSize: 1})
	ctx := context.Background()
	// Create-mode pre-check Head fails → error surfaces before any upload.
	if _, err := s.Compose(ctx, "c", []string{"s"}, PutOptions{Mode: PutCreate}); !IsRetryable(err) {
		t.Fatalf("compose precheck: %v", err)
	}
}

func TestS3ComposeUpdateAbsentDest(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	// Update on an absent dest → 412 with empty current.
	_, err := s.Compose(ctx, "nope", []string{"nope-src"}, PutOptions{Mode: PutUpdate, IfVersion: "v"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("update absent dest: %v", err)
	}
	if cur, _ := PreconditionCurrent(err); cur != "" {
		t.Fatalf("absent dest current: %q", cur)
	}
}

func TestJitterBackoffOverflow(t *testing.T) {
	// Huge shift overflows to <= 0 and must clamp to max, never go negative.
	for n := 60; n < 66; n++ {
		if d := jitterBackoff(n, 20*time.Millisecond, time.Second); d <= 0 || d > 1250*time.Millisecond {
			t.Fatalf("jitterBackoff(%d) = %v", n, d)
		}
	}
}

func TestAcquireBulkWarnPath(t *testing.T) {
	old := lockWaitWarn
	lockWaitWarn = 0 // every acquire "waits too long" → warn branch
	defer func() { lockWaitWarn = old }()
	w := NewWeighted(1)
	wait, release, err := AcquireBulk(context.Background(), w, "wal/x.pack")
	if err != nil || wait < 0 {
		t.Fatalf("AcquireBulk: %v %v", wait, err)
	}
	release()
}

func TestStripedUploadSmallPartCount(t *testing.T) {
	// n <= 32 takes the single-level compose path.
	ctx := context.Background()
	m := NewMemory()
	oldMin, oldThr := stripedUploadMinPart, stripedUploadThreshold
	stripedUploadMinPart, stripedUploadThreshold = 1, 2
	defer func() { stripedUploadMinPart, stripedUploadThreshold = oldMin, oldThr }()
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := PutFileParallel(ctx, m, "k", rf, 6, PutOptions{Mode: PutCreate}); err != nil {
		t.Fatal(err)
	}
}

type failMidComposeStore struct{ *Memory }

func (s *failMidComposeStore) Compose(ctx context.Context, dst string, sources []string, opts PutOptions) (ObjectMeta, error) {
	if IsPartKey(dst) && strings.Contains(dst, "mid") {
		return ObjectMeta{}, NewOther(dst, errors.New("mid compose failed"))
	}
	return s.Memory.Compose(ctx, dst, sources, opts)
}

func TestComposeTwoLevelsMidFailureCleans(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	oldMin, oldThr := stripedUploadMinPart, stripedUploadThreshold
	stripedUploadMinPart, stripedUploadThreshold = 1, 0
	defer func() { stripedUploadMinPart, stripedUploadThreshold = oldMin, oldThr }()
	// 40 parts → two levels; the mid compose fails → error, parts cleaned.
	for i := range 40 {
		if _, err := m.Put(ctx, partKey("k", i), PutBody{Bytes: []byte{byte(i)}}, PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := composeTwoLevels(ctx, &failMidComposeStore{Memory: m}, "k", 40, PutOptions{}); err == nil {
		t.Fatal("mid compose failure must surface")
	}
	// Nothing survives: parts and mids all deleted.
	var left int
	if err := m.List(ctx, "k", "", func(ObjectMeta) error { left++; return nil }); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("intermediates leaked: %d", left)
	}
}

type errBodyReader struct{}

func (errBodyReader) Read([]byte) (int, error) { return 0, errors.New("body kaboom") }

func TestReadRangeIntoBodyError(t *testing.T) {
	ctx := context.Background()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	err = readRangeInto(ctx, &errBodyStore{Memory: NewMemory()}, "k", 0, 10, out)
	if !IsRetryable(err) {
		t.Fatalf("body read error: %v", err)
	}
}

type errBodyStore struct{ *Memory }

func (s *errBodyStore) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	return Object{Meta: ObjectMeta{Key: key, Size: 10}, Body: io.NopCloser(errBodyReader{})}, nil
}

func TestGetBytesBodyError(t *testing.T) {
	_, _, err := GetBytes(context.Background(), &stubStore{res: Object{Meta: ObjectMeta{Key: "k"}, Body: io.NopCloser(errBodyReader{})}}, "k", GetOptions{})
	if !IsRetryable(err) {
		t.Fatalf("GetBytes body error: %v", err)
	}
}

func TestDecodeFramesMidBodyError(t *testing.T) {
	// A reader failing (non-EOF) mid-body surfaces the reader error.
	stream := AppendFrame(nil, []byte("0123456789"))
	r := &failAtReader{data: stream, failAt: 3}
	_, err := DecodeFrames(r, func([]byte) error { return nil })
	if err == nil || err.Error() != "boom mid-body" {
		t.Fatalf("mid-body error: %v", err)
	}
}

type failAtReader struct {
	data   []byte
	off    int
	failAt int
}

func (r *failAtReader) Read(p []byte) (int, error) {
	if r.off >= r.failAt {
		return 0, errors.New("boom mid-body")
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.off]
	r.off++
	return 1, nil
}

func TestPresignQueryExtraParams(t *testing.T) {
	extra := url.Values{"response-content-type": {"application/octet-stream"}}
	q := presignQuery("GET", "/k", "h.example", extra, testCreds, testScope, "20130524T000000Z", time.Minute)
	if q.Get("response-content-type") != "application/octet-stream" {
		t.Fatalf("extra params lost: %v", q)
	}
	if len(q.Get("X-Amz-Signature")) != 64 {
		t.Fatalf("signature missing: %v", q)
	}
}

func s3fakePutBytes(t *testing.T, s *S3, key string, body []byte, opts PutOptions) ObjectMeta {
	t.Helper()
	meta, err := s.Put(context.Background(), key, PutBody{Bytes: body}, opts)
	if err != nil {
		t.Fatalf("S3 Put %q: %v", key, err)
	}
	return meta
}

func TestS3ErrorCodeBodyError(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(errBodyReader{})}
	if code := (&S3{}).errorCode(resp); code != "" {
		t.Fatalf("unreadable error body → %q", code)
	}
}

func TestCryptoRandRead(t *testing.T) {
	var b [8]byte
	if _, err := cryptoRandRead(b[:]); err != nil {
		t.Fatal(err)
	}
}

func TestS3ListTransportAndXMLErrors(t *testing.T) {
	ctx := context.Background()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	s := newS3Client(s3Config{Bucket: "bkt", Endpoint: deadURL, Region: "r", ForcePathStyle: true,
		Creds: testCreds, MaxRetries: 1, MultipartThreshold: 1, MultipartPartSize: 1})
	// List transport error.
	if err := s.List(ctx, "", "", func(ObjectMeta) error { t.Fatal("unexpected"); return nil }); !IsRetryable(err) {
		t.Fatalf("list on dead endpoint: %v", err)
	}
	// ListPrefixes transport error.
	if err := s.ListPrefixes(ctx, "", func(string) error { t.Fatal("unexpected"); return nil }); !IsRetryable(err) {
		t.Fatalf("listPrefixes on dead endpoint: %v", err)
	}
	// Garbage list XML → Other.
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s2 := newS3TestBackend(t, f, nil)
	f.badListXML.Store(true)
	if err := s2.List(ctx, "", "", func(ObjectMeta) error { t.Fatal("unexpected"); return nil }); !IsOther(err) {
		t.Fatalf("garbage list xml: %v", err)
	}
	f.badListXML.Store(false)
	// A gateway that ignores start-after: the client-side filter still
	// enforces strictness.
	s3fakePut(t, s2, "a", "x", PutOptions{})
	s3fakePut(t, s2, "b", "x", PutOptions{})
	f.ignoreStartAfter.Store(true)
	var got []string
	if err := s2.List(ctx, "", "a", func(m ObjectMeta) error { got = append(got, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"b"}) {
		t.Fatalf("client-side start-after filter: %v", got)
	}
}

func TestS3ComposeCreateMultipartFailure(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	s3fakePut(t, s, "s1", "x", PutOptions{})
	f.statusOverride = func(method, key string, r *http.Request) int {
		if _, ok := r.URL.Query()["uploads"]; ok {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	if _, err := s.Compose(ctx, "c", []string{"s1"}, PutOptions{}); !IsRetryable(err) {
		t.Fatalf("compose create failure: %v", err)
	}
	f.statusOverride = nil
}

func TestS3UploadPartCopyTransportError(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) { c.MultipartPartSize = 8 << 20 })
	s3fakePut(t, s, "s1", strings.Repeat("A", 6<<20), PutOptions{})
	f.killCopyPart.Store(true)
	if _, err := s.Compose(ctx, "cat", []string{"s1"}, PutOptions{}); !IsRetryable(err) {
		t.Fatalf("copy-part transport failure: %v", err)
	}
	f.killCopyPart.Store(false)
}

func TestS3ReadRangeUnexpectedVariant(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	s3fakePut(t, s, "tiny", "ab", PutOptions{})
	// A gateway answering 304 to a ranged GET yields a non-Object result;
	// readRange must map it to Other, not panic.
	f.rangeReturns304.Store(true)
	if _, err := s.Compose(ctx, "cat", []string{"tiny"}, PutOptions{}); !IsOther(err) {
		t.Fatalf("ranged 304: %v", err)
	}
	f.rangeReturns304.Store(false)
}

func TestS3DeleteUnconditionalTransportError(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	s := newS3Client(s3Config{Bucket: "bkt", Endpoint: deadURL, Region: "r", ForcePathStyle: true,
		Creds: testCreds, MaxRetries: 1, MultipartThreshold: 1, MultipartPartSize: 1})
	if err := s.Delete(context.Background(), "k", ""); !IsRetryable(err) {
		t.Fatalf("unconditional delete on dead endpoint: %v", err)
	}
}

func TestPartSizeForCap(t *testing.T) {
	if got := partSizeFor(2 << 40); got != 1<<30 {
		t.Fatalf("2TiB part size = %d", got)
	}
}

func TestMemoryLatencySuccessPath(t *testing.T) {
	// The latency tick's non-cancelled branch (time.After) is the normal op
	// path when Latency is set.
	m := NewMemory()
	m.Latency = time.Millisecond
	ctx := context.Background()
	mustPut(t, m, "k", "v", PutOptions{})
	if _, err := m.Get(ctx, "k", GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "k", ""); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemListPrunesSiblingSubtrees(t *testing.T) {
	f := newFS(t)
	// "a-x" sorts before "a/b" (S3 byte order) but is not under "a/":
	// listing prefix "a/" descends only "a/" and prunes sibling subtrees
	// like "zz/" without walking them.
	fsPut(t, f, "a-x", "1", PutOptions{})
	fsPut(t, f, "a/b", "2", PutOptions{})
	fsPut(t, f, "zz/deep/leaf", "3", PutOptions{})
	var keys []string
	if err := f.List(context.Background(), "a/", "", func(m ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != fmt.Sprint([]string{"a/b"}) {
		t.Fatalf("byte-order listing with prune: %v", keys)
	}
}

func TestMemoryListPrefixesCallbackError(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "a/1", "x", PutOptions{})
	mustPut(t, m, "b/1", "x", PutOptions{})
	sentinel := errors.New("stop")
	if err := m.ListPrefixes(context.Background(), "", func(string) error { return sentinel }); err != sentinel {
		t.Fatalf("ListPrefixes callback error: %v", err)
	}
}

func TestMemoryComposeUpdateAbsentDest(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "s", "x", PutOptions{})
	_, err := m.Compose(context.Background(), "absent", []string{"s"}, PutOptions{Mode: PutUpdate, IfVersion: "v"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("compose update on absent dest: %v", err)
	}
	if cur, _ := PreconditionCurrent(err); cur != "" {
		t.Fatalf("absent dest current: %q", cur)
	}
}

func TestFilesystemComposeBadDestKey(t *testing.T) {
	f := newFS(t)
	fsPut(t, f, "s", "x", PutOptions{})
	if _, err := f.Compose(context.Background(), "bad.lock", []string{"s"}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("compose bad dst key: %v", err)
	}
}
