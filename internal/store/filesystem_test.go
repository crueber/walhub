package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newFS(t *testing.T) *Filesystem {
	t.Helper()
	f, err := NewFilesystemRoot(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("NewFilesystemRoot: %v", err)
	}
	return f
}

func fsPut(t *testing.T, f *Filesystem, key, body string, opts PutOptions) ObjectMeta {
	t.Helper()
	meta, err := f.Put(context.Background(), key, PutBody{Bytes: []byte(body)}, opts)
	if err != nil {
		t.Fatalf("Put %q: %v", key, err)
	}
	return meta
}

func TestNewFilesystemRootGuards(t *testing.T) {
	if _, err := NewFilesystemRoot("", 1); !IsInvalidArgument(err) {
		t.Fatalf("empty root: %v", err)
	}
	if _, err := NewFilesystemRoot(filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Fatal("missing root must fail (EvalSymlinks)")
	}
	// bulkConcurrency < 1 clamps to 1.
	f, err := NewFilesystemRoot(t.TempDir(), 0)
	if err != nil || f == nil {
		t.Fatalf("clamp: %v", err)
	}
	if f.Backend() != "filesystem" {
		t.Fatal("backend name")
	}
}

func TestFilesystemCheckKey(t *testing.T) {
	f := newFS(t)
	bad := []string{
		"", "/abs", "abs/", "a//b", "./a", "../a", "a/../b", "..", "a/./b",
		"x.lock", "dir/x.lock", "a.tmp-1", "dir/a.tmp-x",
	}
	for _, k := range bad {
		if err := f.checkKey(k); !IsInvalidArgument(err) {
			t.Errorf("checkKey(%q) = %v, want InvalidArgument", k, err)
		}
	}
	for _, k := range []string{"a", "a/b/c", "a-b.txt", "a..b", ".hidden", "lock", "atmp"} {
		if err := f.checkKey(k); err != nil {
			t.Errorf("checkKey(%q) = %v, want ok", k, err)
		}
	}
}

func TestFilesystemVersionTokens(t *testing.T) {
	f := newFS(t)
	v1 := fsPut(t, f, "k", "12345", PutOptions{}).Version
	// Token shape "<size>:<mtime_ns>".
	if !strings.Contains(string(v1), ":") || sizeOfToken(v1) != 5 {
		t.Fatalf("token %q size %d", v1, sizeOfToken(v1))
	}
	if sizeOfToken("nonsense") != 0 {
		t.Fatal("sizeOfToken garbage")
	}
	// Rewriting with different content changes the token (size half).
	v2 := fsPut(t, f, "k", "1234567890", PutOptions{}).Version
	if v2 == v1 {
		t.Fatal("token unchanged across content change")
	}
	if sizeOfToken(v2) != 10 {
		t.Fatalf("sizeOfToken=%d", sizeOfToken(v2))
	}
	// Same size, same coarse mtime: bumpTokenIfCollision must advance.
	tmp := filepath.Join(t.TempDir(), "tmp")
	if err := os.WriteFile(tmp, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	st0, err := os.Lstat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	tok0 := tokenFromStat(st0)
	bumped, err := bumpTokenIfCollision(tmp, tok0, tok0)
	if err != nil {
		t.Fatal(err)
	}
	st1, err := os.Lstat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if bumped == tok0 && tokenFromStat(st1) == tok0 {
		t.Fatalf("collision not bumped: %q", bumped)
	}
	// Distinct tokens pass through untouched.
	if got, err := bumpTokenIfCollision(tmp, v2, tok0); err != nil || got != v2 {
		t.Fatalf("no-collision path: %q %v", got, err)
	}
}

func TestFilesystemGetHeadDelete(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	v := fsPut(t, f, "dir/k", "0123456789", PutOptions{}).Version

	hm, err := f.Head(ctx, "dir/k")
	if err != nil || hm == nil || hm.Size != 10 || hm.Version != v || hm.Key != "dir/k" {
		t.Fatalf("Head: %+v %v", hm, err)
	}
	if hm, err = f.Head(ctx, "nope"); err != nil || hm != nil {
		t.Fatalf("Head absent: %+v %v", hm, err)
	}

	res, err := f.Get(ctx, "dir/k", GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	o := res.(Object)
	b, _ := io.ReadAll(o.Body)
	o.Body.Close()
	if string(b) != "0123456789" || o.Meta.Size != 10 || o.Meta.Version != v {
		t.Fatalf("Get: %q %+v", b, o.Meta)
	}

	res, err = f.Get(ctx, "dir/k", GetOptions{IfNoneMatch: v})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(NotModified); !ok {
		t.Fatalf("IfNoneMatch: %#v", res)
	}
	if _, err = f.Get(ctx, "dir/k", GetOptions{IfMatch: "stale"}); !IsPreconditionFailed(err) {
		t.Fatalf("IfMatch mismatch: %v", err)
	}

	rb := func(rng *[2]int64) (string, int64, error) {
		res, err := f.Get(ctx, "dir/k", GetOptions{Range: rng})
		if err != nil {
			return "", 0, err
		}
		o := res.(Object)
		defer o.Body.Close()
		b, _ := io.ReadAll(o.Body)
		return string(b), o.Meta.Size, nil
	}
	if b, size, err := rb(&[2]int64{2, 5}); err != nil || b != "234" || size != 10 {
		t.Fatalf("range: %q %d %v", b, size, err)
	}
	if b, _, err := rb(&[2]int64{9, 99}); err != nil || b != "9" {
		t.Fatalf("clamp: %q %v", b, err)
	}
	if b, _, err := rb(&[2]int64{10, 10}); err != nil || b != "" {
		t.Fatalf("empty suffix: %q %v", b, err)
	}
	if _, _, err := rb(&[2]int64{11, 20}); !IsPreconditionFailed(err) {
		t.Fatalf("past EOF: %v", err)
	}
	if _, _, err := rb(&[2]int64{5, 2}); !IsInvalidArgument(err) {
		t.Fatalf("inverted: %v", err)
	}

	if _, err := f.Get(ctx, "nope", GetOptions{}); !IsNotFound(err) {
		t.Fatalf("get absent: %v", err)
	}
	if _, err := f.Get(ctx, "../escape", GetOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("bad key: %v", err)
	}

	if err := f.Delete(ctx, "nope", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "nope", "v"); !IsNotFound(err) {
		t.Fatalf("cas absent: %v", err)
	}
	if err := f.Delete(ctx, "dir/k", "wrong"); !IsPreconditionFailed(err) {
		t.Fatalf("cas wrong: %v", err)
	}
	if err := f.Delete(ctx, "dir/k", v); err != nil {
		t.Fatal(err)
	}
	if hm, _ := f.Head(ctx, "dir/k"); hm != nil {
		t.Fatal("not deleted")
	}
	// The CAS delete's ".lock" sidecar persists by design, so "dir" is NOT
	// pruned here. An unconditional delete leaves no sidecar: its empty
	// parent is pruned up to the root.
	if _, err := os.Stat(f.path("dir")); err != nil {
		t.Fatalf("sidecar-bearing dir was pruned: %v", err)
	}
	fsPut(t, f, "clean/x", "x", PutOptions{})
	if err := f.Delete(ctx, "clean/x", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("clean")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("empty parent not pruned: %v", err)
	}
}

func TestFilesystemPutModes(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()

	v1 := fsPut(t, f, "k", "one", PutOptions{Mode: PutCreate}).Version
	_, err := f.Put(ctx, "k", PutBody{Bytes: []byte("two")}, PutOptions{Mode: PutCreate})
	if !IsPreconditionFailed(err) {
		t.Fatalf("create on existing: %v", err)
	}
	if cur, _ := PreconditionCurrent(err); cur == "" {
		t.Fatalf("create conflict lost current version: %v", err)
	}
	_, err = f.Put(ctx, "k", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: "nope"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("update wrong version: %v", err)
	}
	_, err = f.Put(ctx, "k", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate})
	if !IsPreconditionFailed(err) {
		t.Fatalf("update without version: %v", err)
	}
	_, err = f.Put(ctx, "absent", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: v1})
	if !IsPreconditionFailed(err) {
		t.Fatalf("update absent: %v", err)
	}
	if cur, _ := PreconditionCurrent(err); cur != "" {
		t.Fatalf("absent update current: %q", cur)
	}
	v2meta, err := f.Put(ctx, "k", PutBody{Bytes: []byte("one")}, PutOptions{Mode: PutUpdate, IfVersion: v1})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if v2meta.Version == v1 {
		t.Fatal("CAS update re-minted the same token")
	}
	v3, err := f.Put(ctx, "k", PutBody{Bytes: []byte("one")}, PutOptions{Mode: PutOverwrite})
	if err != nil {
		t.Fatal(err)
	}
	if v3.Version == v2meta.Version {
		t.Fatal("overwrite reused token")
	}
	if _, err := f.Put(ctx, "s", PutBody{Stream: strings.NewReader("abcd"), StreamLen: 4}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	fpath := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(fpath, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(ctx, "fi", PutBody{File: fpath}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(ctx, "short", PutBody{Stream: strings.NewReader("ab"), StreamLen: 5}, PutOptions{}); err == nil {
		t.Fatal("short stream must fail")
	}
	if _, err := f.Put(ctx, "e", PutBody{}, PutOptions{}); err == nil {
		t.Fatal("empty body must fail")
	}
	if _, err := f.Put(ctx, "k.lock", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatal("lock namespace")
	}
	if _, err := f.Put(ctx, "k.tmp-1", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatal("tmp namespace")
	}
}

func TestFilesystemPortableCreateFallback(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	defer forcePortableRename.Store(false)

	forcePortableRename.Store(true)
	v := fsPut(t, f, "p/k", "data", PutOptions{Mode: PutCreate}).Version
	// Second create via the portable path → 412 with current version.
	_, err := f.Put(ctx, "p/k", PutBody{Bytes: []byte("data")}, PutOptions{Mode: PutCreate})
	if !IsPreconditionFailed(err) {
		t.Fatalf("portable create on existing: %v", err)
	}
	if cur, _ := PreconditionCurrent(err); cur != v {
		t.Fatalf("portable current: %q want %q", cur, v)
	}
	// Portable create on a fresh key still works.
	if _, err := f.Put(ctx, "p/k2", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutCreate}); err != nil {
		t.Fatal(err)
	}
	forcePortableRename.Store(false)
}

func TestFilesystemSymlinkGuards(t *testing.T) {
	root := t.TempDir()
	f, err := NewFilesystemRoot(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(ctx, "link/evil", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("symlinked write: %v", err)
	}
	// Reads of absent keys stay NotFound (no traversal needed); reads of
	// existing objects through a symlinked dir are rejected.
	if _, err := f.Get(ctx, "link/absent", GetOptions{}); !IsNotFound(err) {
		t.Fatalf("absent through symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ctx, "link/secret", GetOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("read through symlink: %v", err)
	}
}

func TestFilesystemMapErr(t *testing.T) {
	f := newFS(t)
	cases := []struct {
		err  error
		want func(error) bool
	}{
		{syscall.ENOENT, IsNotFound},
		{syscall.EEXIST, IsPreconditionFailed},
		{syscall.EIO, IsRetryable},
		{syscall.ENFILE, IsRetryable},
		{syscall.EMFILE, IsRetryable},
		{syscall.ENOSPC, IsOther},
		{syscall.EACCES, IsOther},
		{syscall.EPERM, IsOther},
		{syscall.EROFS, IsOther},
		{syscall.EXDEV, IsOther},
		{syscall.EINVAL, IsOther},
	}
	for i, c := range cases {
		if got := f.mapErr("k", c.err); !c.want(got) {
			t.Fatalf("case %d: %v misclassified: %v", i, c.err, got)
		}
	}
	if f.mapErr("k", nil) != nil {
		t.Fatal("nil mapped to error")
	}
}

func TestFilesystemComposeAndList(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	fsPut(t, f, "src/a", "A", PutOptions{})
	fsPut(t, f, "src/b", "BB", PutOptions{})
	fsPut(t, f, "loose", "L", PutOptions{})

	if !f.SupportsCompose() || !f.ComposeIsNative() {
		t.Fatal("filesystem composes natively")
	}
	if _, err := f.Compose(ctx, "c", []string{"src/a", "src/b"}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	b, _, err := GetBytes(ctx, f, "c", GetOptions{})
	if err != nil || string(b) != "ABB" {
		t.Fatalf("compose bytes: %q %v", b, err)
	}
	if hm, _ := f.Head(ctx, "src/a"); hm == nil {
		t.Fatal("source removed")
	}
	if _, err := f.Compose(ctx, "c", []string{"src/a"}, PutOptions{Mode: PutCreate}); !IsPreconditionFailed(err) {
		t.Fatalf("compose create existing: %v", err)
	}
	if _, err := f.Compose(ctx, "c", []string{"src/nope"}, PutOptions{}); !IsNotFound(err) {
		t.Fatalf("compose missing source: %v", err)
	}
	if _, err := f.Compose(ctx, "c", nil, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("compose zero sources: %v", err)
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = "src/a"
	}
	if _, err := f.Compose(ctx, "c", many, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("compose 33 sources: %v", err)
	}
	if _, err := f.Compose(ctx, "c", []string{"bad.lock"}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("compose bad source key: %v", err)
	}

	// Listing: byte order, sidecars invisible, prefix + startAfter.
	if err := os.WriteFile(f.path("src/a.lock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path("src/.tmp-zzz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := f.List(ctx, "", "", func(m ObjectMeta) error { keys = append(keys, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{"c", "loose", "src/a", "src/b"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("List = %v, want %v", keys, want)
	}
	keys = nil
	if err := f.List(ctx, "src/", "src/a", func(m ObjectMeta) error { keys = append(keys, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != fmt.Sprint([]string{"src/b"}) {
		t.Fatalf("List prefix/startAfter = %v", keys)
	}
	sentinel := errors.New("stop")
	if err := f.List(ctx, "", "", func(ObjectMeta) error { return sentinel }); err != sentinel {
		t.Fatalf("callback error: %v", err)
	}
	if err := f.List(ctx, "zz/", "", func(ObjectMeta) error { t.Fatal("unexpected"); return nil }); err != nil {
		t.Fatal(err)
	}
	var pfx []string
	if err := f.ListPrefixes(ctx, "", func(s string) error { pfx = append(pfx, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pfx) != fmt.Sprint([]string{"src/"}) {
		t.Fatalf("ListPrefixes = %v", pfx)
	}
	if err := f.ListPrefixes(ctx, "zz/", func(string) error { t.Fatal("unexpected"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemNoURLsAndBulkPermits(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	if u, err := f.SignedGetURL(ctx, "k", time.Minute); u != nil || err != nil {
		t.Fatalf("SignedGetURL: %v %v", u, err)
	}
	if at, err := f.AccelTarget(ctx, "k"); at != nil || err != nil {
		t.Fatalf("AccelTarget: %v %v", at, err)
	}
	// Bulk ops with a cancelled context surface Retryable from the
	// semaphore path. The semaphore is exhausted first so the blocked
	// Acquire cannot complete before noticing the cancellation.
	f.sem = NewWeighted(1)
	if err := f.sem.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Put(cctx, "wal/x.pack", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsRetryable(err) {
		t.Fatalf("bulk put cancelled: %v", err)
	}
	if err := f.Delete(cctx, "wal/x.pack", ""); !IsRetryable(err) {
		t.Fatalf("bulk delete cancelled: %v", err)
	}
}

func TestNeedsDirSync(t *testing.T) {
	for _, k := range []string{"leases/publish.pb", "repos/o/r/manifest.pb", "manifest.pb"} {
		if !needsDirSync(k) {
			t.Errorf("needsDirSync(%q) = false", k)
		}
	}
	for _, k := range []string{"wal/x.pack", "repos/o/r/policy.json", "bundles/b"} {
		if needsDirSync(k) {
			t.Errorf("needsDirSync(%q) = true", k)
		}
	}
	if !sidecar("x.lock") || !sidecar("a.tmp-1") || sidecar("plain") {
		t.Fatal("sidecar classification")
	}
}
