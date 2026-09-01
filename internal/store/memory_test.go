package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPut(t *testing.T, m *Memory, key, body string, opts PutOptions) ObjectMeta {
	t.Helper()
	meta, err := m.Put(context.Background(), key, PutBody{Bytes: []byte(body)}, opts)
	if err != nil {
		t.Fatalf("Put %q: %v", key, err)
	}
	return meta
}

func TestMemoryBasicRoundtrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if m.Backend() != "memory" {
		t.Fatal("backend name")
	}
	m1 := mustPut(t, m, "a/b", "hello", PutOptions{ContentType: "text/plain"})
	m2 := mustPut(t, m, "x", "abc", PutOptions{})
	if m1.Version == m2.Version || m1.Version == "" {
		t.Fatalf("versions must be globally unique: %s %s", m1.Version, m2.Version)
	}
	if m1.Size != 5 || m2.Size != 3 {
		t.Fatalf("sizes: %+v %+v", m1, m2)
	}
	res, err := m.Get(ctx, "a/b", GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	o := res.(Object)
	defer o.Body.Close()
	got, _ := io.ReadAll(o.Body)
	if string(got) != "hello" || o.Meta.Size != 5 || o.Meta.Key != "a/b" {
		t.Fatalf("roundtrip: %q %+v", got, o.Meta)
	}
	hm, err := m.Head(ctx, "a/b")
	if err != nil || hm == nil || hm.Size != 5 || hm.Version != m1.Version {
		t.Fatalf("Head: %+v %v", hm, err)
	}
	if hm, err = m.Head(ctx, "absent"); err != nil || hm != nil {
		t.Fatalf("Head absent: %+v %v", hm, err)
	}
	// Put of an identical body still mints a new version.
	m3 := mustPut(t, m, "a/b", "hello", PutOptions{})
	if m3.Version == m1.Version {
		t.Fatal("version reused")
	}
}

func TestMemoryConditionals(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	v1 := mustPut(t, m, "k", "one", PutOptions{}).Version

	res, err := m.Get(ctx, "k", GetOptions{IfNoneMatch: v1})
	if err != nil {
		t.Fatal(err)
	}
	if nm, ok := res.(NotModified); !ok || nm.Version != v1 {
		t.Fatalf("IfNoneMatch: %#v", res)
	}

	_, err = m.Get(ctx, "k", GetOptions{IfMatch: "stale"})
	se := err.(*StoreError)
	if se.Kind != ErrKindPreconditionFailed || se.Current != v1 {
		t.Fatalf("IfMatch mismatch: %#v", err)
	}

	// Create on existing → 412 with the current version.
	_, err = m.Put(ctx, "k", PutBody{Bytes: []byte("two")}, PutOptions{Mode: PutCreate})
	se = err.(*StoreError)
	if se.Kind != ErrKindPreconditionFailed || se.Current != v1 {
		t.Fatalf("PutCreate on existing: %#v", err)
	}
	// Update with missing/wrong version → 412.
	_, err = m.Put(ctx, "absent", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: v1})
	if se = err.(*StoreError); se.Kind != ErrKindPreconditionFailed || se.Current != "" {
		t.Fatalf("Update on absent: %#v", err)
	}
	_, err = m.Put(ctx, "k", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: "wrong"})
	if se = err.(*StoreError); se.Kind != ErrKindPreconditionFailed || se.Current != v1 {
		t.Fatalf("Update wrong version: %#v", err)
	}
	// Update without IfVersion → 412.
	_, err = m.Put(ctx, "k", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate})
	if !IsPreconditionFailed(err) {
		t.Fatalf("Update without version: %v", err)
	}
	// Update with the right version wins; then NotModified sees the new token.
	v2 := mustPut(t, m, "k", "two", PutOptions{Mode: PutUpdate, IfVersion: v1}).Version
	if v2 == v1 {
		t.Fatal("update kept the old version")
	}
}

func TestMemoryRanges(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	mustPut(t, m, "r", "0123456789", PutOptions{})

	read := func(rng *[2]int64) (string, int64, error) {
		res, err := m.Get(ctx, "r", GetOptions{Range: rng})
		if err != nil {
			return "", 0, err
		}
		o := res.(Object)
		defer o.Body.Close()
		b, _ := io.ReadAll(o.Body)
		return string(b), o.Meta.Size, nil
	}
	b, size, err := read(&[2]int64{2, 5}) // half-open [2,5)
	if err != nil || b != "234" || size != 10 {
		t.Fatalf("range: %q size=%d err=%v", b, size, err)
	}
	b, _, err = read(&[2]int64{9, 100}) // end clamps
	if err != nil || b != "9" {
		t.Fatalf("clamp: %q %v", b, err)
	}
	b, _, err = read(&[2]int64{10, 10}) // empty suffix read is legal
	if err != nil || b != "" {
		t.Fatalf("empty suffix: %q %v", b, err)
	}
	_, err = m.Get(ctx, "r", GetOptions{Range: &[2]int64{11, 20}}) // past EOF
	if !IsPreconditionFailed(err) {
		t.Fatalf("past EOF: %v", err)
	}
	_, err = m.Get(ctx, "r", GetOptions{Range: &[2]int64{5, 2}})
	if !IsInvalidArgument(err) {
		t.Fatalf("inverted range: %v", err)
	}
	_, err = m.Get(ctx, "r", GetOptions{Range: &[2]int64{-1, 2}})
	if !IsInvalidArgument(err) {
		t.Fatalf("negative start: %v", err)
	}
}

func TestMemoryPutBodiesAndGuards(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	// Stream body of known length.
	meta, err := m.Put(ctx, "s", PutBody{Stream: strings.NewReader("abcdef"), StreamLen: 6}, PutOptions{})
	if err != nil || meta.Size != 6 {
		t.Fatalf("stream put: %+v %v", meta, err)
	}
	// File body.
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("file!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if meta, err = m.Put(ctx, "f", PutBody{File: f}, PutOptions{}); err != nil || meta.Size != 5 {
		t.Fatalf("file put: %+v %v", meta, err)
	}
	// Empty body → Other.
	if _, err = m.Put(ctx, "e", PutBody{}, PutOptions{}); !IsOther(err) {
		t.Fatalf("empty body: %v", err)
	}
	// Short stream → Other (readPutBody wraps).
	if _, err = m.Put(ctx, "e", PutBody{Stream: strings.NewReader("ab"), StreamLen: 5}, PutOptions{}); err == nil {
		t.Fatal("short stream must fail")
	}
	// Empty key → Invalid.
	if _, err = m.Put(ctx, "", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("empty key: %v", err)
	}
}

func TestMemoryDelete(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	v := mustPut(t, m, "k", "v", PutOptions{}).Version
	// Unconditional delete of an absent key is Ok.
	if err := m.Delete(ctx, "absent", ""); err != nil {
		t.Fatal(err)
	}
	// Conditional delete of an absent key → NotFound.
	if err := m.Delete(ctx, "absent", "v"); !IsNotFound(err) {
		t.Fatalf("conditional absent: %v", err)
	}
	// Wrong version → 412 with current.
	if err := m.Delete(ctx, "k", "wrong"); !IsPreconditionFailed(err) {
		t.Fatalf("wrong version: %v", err)
	}
	// Right version deletes.
	if err := m.Delete(ctx, "k", v); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "k", GetOptions{}); !IsNotFound(err) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestMemoryList(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	for _, k := range []string{"a/2", "a/1", "b/1", "top"} {
		mustPut(t, m, k, "x", PutOptions{})
	}
	var keys []string
	err := m.List(ctx, "a/", "", func(m ObjectMeta) error { keys = append(keys, m.Key); return nil })
	if err != nil || len(keys) != 2 || keys[0] != "a/1" || keys[1] != "a/2" {
		t.Fatalf("List: %v %v", keys, err)
	}
	keys = nil
	if err := m.List(ctx, "", "a/2", func(m ObjectMeta) error { keys = append(keys, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "b/1" || keys[1] != "top" {
		t.Fatalf("startAfter: %v", keys)
	}
	// Callback error propagates.
	sentinel := errors.New("stop")
	if err := m.List(ctx, "", "", func(ObjectMeta) error { return sentinel }); err != sentinel {
		t.Fatalf("callback error: %v", err)
	}
	// Prefixes: distinct segments, files ignored.
	var pfx []string
	if err := m.ListPrefixes(ctx, "", func(s string) error { pfx = append(pfx, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(pfx) != 2 || pfx[0] != "a/" || pfx[1] != "b/" {
		t.Fatalf("ListPrefixes: %v", pfx)
	}
	if err := m.ListPrefixes(ctx, "zz/", func(string) error { t.Fatal("no prefixes expected"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCompose(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if !m.SupportsCompose() || !m.ComposeIsNative() {
		t.Fatal("memory composes natively")
	}
	mustPut(t, m, "s1", "a", PutOptions{})
	mustPut(t, m, "s2", "b", PutOptions{})
	meta, err := m.Compose(ctx, "cat", []string{"s1", "s2"}, PutOptions{ContentType: "x/y"})
	if err != nil || string(mustRead(t, m, "cat")) != "ab" {
		t.Fatalf("compose: %+v %v", meta, err)
	}
	// Sources remain.
	if _, err := m.Head(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	// Source missing → NotFound.
	if _, err := m.Compose(ctx, "c2", []string{"s1", "nope"}, PutOptions{}); !IsNotFound(err) {
		t.Fatalf("missing source: %v", err)
	}
	// Count guards.
	for _, n := range []int{0, 33} {
		srcs := make([]string, n)
		for i := range srcs {
			srcs[i] = "s1"
		}
		if _, err := m.Compose(ctx, "c3", srcs, PutOptions{}); !IsInvalidArgument(err) {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
	// Compose honors Create.
	dv := mustPut(t, m, "d", "dst", PutOptions{}).Version
	if _, err := m.Compose(ctx, "d", []string{"s1"}, PutOptions{Mode: PutCreate}); !IsPreconditionFailed(err) {
		t.Fatalf("compose create on existing: %v", err)
	}
	if _, err := m.Compose(ctx, "d", []string{"s1"}, PutOptions{Mode: PutUpdate, IfVersion: dv}); err != nil {
		t.Fatalf("compose update: %v", err)
	}
}

func TestMemoryURLsAndKnobs(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	mustPut(t, m, "k", "v", PutOptions{})

	u, err := m.SignedGetURL(ctx, "k", time.Minute)
	if err != nil || u == nil || !strings.HasPrefix(*u, "https://memory.walhub.invalid/k?") {
		t.Fatalf("SignedGetURL: %v %v", u, err)
	}
	// Accel off by default.
	at, err := m.AccelTarget(ctx, "k")
	if err != nil || at != nil {
		t.Fatalf("AccelTarget default: %v %v", at, err)
	}
	m.FakeObjectURL = true
	at, err = m.AccelTarget(ctx, "k")
	if err != nil || at == nil || at.URL == "" || at.Authorization == "" {
		t.Fatalf("AccelTarget fake: %+v %v", at, err)
	}
	// Signing failure knob (VPC-SC analog).
	m.SigningFails = true
	if _, err = m.SignedGetURL(ctx, "k", time.Minute); !IsOther(err) {
		t.Fatalf("SigningFails: %v", err)
	}
	m.SigningFails = false

	// Op counter ticks.
	base := m.OpCounter().Load()
	_, _ = m.Head(ctx, "k")
	if m.OpCounter().Load() != base+1 {
		t.Fatal("op counter did not tick")
	}

	// Latency + cancelled context → Retryable.
	m.Latency = 50 * time.Millisecond
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Get(cctx, "k", GetOptions{}); !IsRetryable(err) {
		t.Fatalf("cancelled get: %v", err)
	}
	if _, err := m.Put(cctx, "k2", PutBody{Bytes: []byte("x")}, PutOptions{}); !IsRetryable(err) {
		t.Fatalf("cancelled put: %v", err)
	}
	if err := m.Delete(cctx, "k", ""); !IsRetryable(err) {
		t.Fatalf("cancelled delete: %v", err)
	}
	if err := m.List(cctx, "", "", func(ObjectMeta) error { return nil }); !IsRetryable(err) {
		t.Fatalf("cancelled list: %v", err)
	}
	if err := m.ListPrefixes(cctx, "", func(string) error { return nil }); !IsRetryable(err) {
		t.Fatalf("cancelled listPrefixes: %v", err)
	}
}

func mustRead(t *testing.T, m *Memory, key string) []byte {
	t.Helper()
	b, _, err := GetBytes(context.Background(), m, key, GetOptions{})
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return b
}
