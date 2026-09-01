package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noComposeStore wraps Memory with ComposeIsNative()=false, mirroring S3.
type noComposeStore struct{ *Memory }

func (noComposeStore) ComposeIsNative() bool { return false }

func TestPartSizeFor(t *testing.T) {
	if got := partSizeFor(0); got != 64<<20 {
		t.Fatalf("tiny size part size = %d", got)
	}
	if got := partSizeFor(1 << 40); got != 1<<30 {
		t.Fatalf("huge size part size = %d", got)
	}
	// ceil(size/1024) inside the clamp window.
	size := int64(100) << 20 // 100 MiB → ceil/1024 < 64 MiB → clamped up
	if got := partSizeFor(size); got != 64<<20 {
		t.Fatalf("100MiB part size = %d", got)
	}
}

func TestIsPartKey(t *testing.T) {
	yes := []string{"wal/x.pack.part/0000", "wal/x.pack.part/1234", "a.part/mid0001", "a.part/mid0032"}
	no := []string{"wal/x.pack", "wal/x.pack.part/", "wal/x.pack.part/12", "wal/x.pack.part/12345",
		"wal/x.pack.part/mid1", "wal/x.pack.part/mid001", "wal/x.pack.part/midx001",
	}
	for _, k := range yes {
		if !IsPartKey(k) {
			t.Errorf("IsPartKey(%q) = false", k)
		}
	}
	for _, k := range no {
		if IsPartKey(k) {
			t.Errorf("IsPartKey(%q) = true", k)
		}
	}
}

func TestPutFileParallelSinglePut(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	data := strings.Repeat("x", 1<<20)
	f := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(f, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	// Non-native compose → single Put even for big files.
	meta, err := PutFileParallel(ctx, noComposeStore{m}, "big", rf, int64(len(data)), PutOptions{Mode: PutCreate})
	if err != nil || meta.Size != int64(len(data)) {
		t.Fatalf("single put: %+v %v", meta, err)
	}
	b, _, err := GetBytes(ctx, m, "big", GetOptions{})
	if err != nil || string(b) != data {
		t.Fatalf("bytes mismatch: %d %v", len(b), err)
	}
	// Small file on a native backend also takes the single Put path.
	if _, err := rf.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	meta, err = PutFileParallel(ctx, m, "small", rf, 10, PutOptions{Mode: PutCreate})
	if err != nil || meta.Size != 10 {
		t.Fatalf("small: %+v %v", meta, err)
	}
}

func TestPutFileParallelStriped(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	oldMin, oldMax, oldThr, oldStripes := stripedUploadMinPart, stripedUploadMaxPart, stripedUploadThreshold, stripedUploadStripes
	stripedUploadMinPart, stripedUploadMaxPart, stripedUploadThreshold, stripedUploadStripes = 1, 1<<30, 8, 4
	defer func() {
		stripedUploadMinPart, stripedUploadMaxPart, stripedUploadThreshold, stripedUploadStripes = oldMin, oldMax, oldThr, oldStripes
	}()

	// 40 parts (size 40, part size 1): exercises the two-level compose with
	// a non-multiple-of-32 remainder — historically the dropped tail.
	var data []byte
	for i := range 40 {
		data = append(data, byte('0'+i%10))
	}
	f := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	meta, err := PutFileParallel(ctx, m, "big", rf, int64(len(data)), PutOptions{Mode: PutCreate})
	if err != nil || meta.Size != int64(len(data)) {
		t.Fatalf("striped put: %+v %v", meta, err)
	}
	b, _, err := GetBytes(ctx, m, "big", GetOptions{})
	if err != nil || string(b) != string(data) {
		t.Fatalf("composed bytes mismatch: got %d bytes (%q), want %q", len(b), b, data)
	}
	// Parts and mids are cleaned up; nothing but the final object remains.
	var left []string
	if err := m.List(ctx, "big", "", func(mm ObjectMeta) error { left = append(left, mm.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != "big" {
		t.Fatalf("leftover intermediates: %v", left)
	}
}

type failPartStore struct{ *Memory }

func (s *failPartStore) Put(ctx context.Context, key string, body PutBody, opts PutOptions) (ObjectMeta, error) {
	if IsPartKey(key) {
		return ObjectMeta{}, NewOther(key, errors.New("part upload failed"))
	}
	return s.Memory.Put(ctx, key, body, opts)
}

func TestPutFileParallelFailureCleansParts(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	oldMin, oldThr := stripedUploadMinPart, stripedUploadThreshold
	stripedUploadMinPart, stripedUploadThreshold = 1, 0
	defer func() {
		stripedUploadMinPart, stripedUploadThreshold = oldMin, oldThr
	}()

	fail := &failPartStore{Memory: m}
	fpath := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(fpath, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(fpath)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := PutFileParallel(ctx, fail, "k", rf, 4, PutOptions{Mode: PutCreate}); err == nil {
		t.Fatal("part failure must surface")
	}
	// No part objects survive.
	var left []string
	if err := m.List(ctx, "k", "", func(mm ObjectMeta) error { left = append(left, mm.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("parts leaked: %v", left)
	}
}

func TestDownloadFileParallel(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	data := make([]byte, 200<<10)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if _, err := m.Put(ctx, "dl", PutBody{Bytes: data}, PutOptions{}); err != nil {
		t.Fatal(err)
	}

	oldChunk, oldStripes := downloadChunk, downloadStripes
	downloadChunk, downloadStripes = 32<<10, 4
	defer func() { downloadChunk, downloadStripes = oldChunk, oldStripes }()

	out := filepath.Join(t.TempDir(), "out")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := DownloadFileParallel(ctx, m, "dl", f, int64(len(data))); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil || len(got) != len(data) {
		t.Fatalf("readback: %d %v", len(got), err)
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("byte %d: got %d want %d", i, got[i], data[i])
		}
	}
}

// truncRangeStore serves ranged reads short — the corrupt-download fault.
type truncRangeStore struct{ *Memory }

func (s *truncRangeStore) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	res, err := s.Memory.Get(ctx, key, opts)
	if err != nil {
		return res, err
	}
	if o, ok := res.(Object); ok {
		b, _ := io.ReadAll(io.LimitReader(o.Body, 3))
		o.Body.Close()
		o.Body = io.NopCloser(strings.NewReader(string(b)))
		return o, nil
	}
	return res, nil
}

// notObjectStore answers every Get with NotModified — the wrong-variant fault.
type notObjectStore struct{ *Memory }

func (s *notObjectStore) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	return NotModified{Version: "x"}, nil
}

func TestDownloadFileParallelCorruptShortRead(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if _, err := m.Put(ctx, "dl", PutBody{Bytes: make([]byte, 100)}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	oldChunk := downloadChunk
	downloadChunk = 64
	defer func() { downloadChunk = oldChunk }()

	out := filepath.Join(t.TempDir(), "out")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := DownloadFileParallel(ctx, &truncRangeStore{m}, "dl", f, 100); !IsCorrupt(err) || !strings.Contains(err.Error(), "short read") {
		t.Fatalf("short read: %v", err)
	}
	// Wrong GetResult variant is Corrupt too, not a hang.
	if err := DownloadFileParallel(ctx, &notObjectStore{m}, "dl", f, 100); !IsCorrupt(err) {
		t.Fatalf("wrong variant: %v", err)
	}
	// Truncate failure (read-only fd) is Corrupt with a "preallocate" cause.
	ro, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := DownloadFileParallel(ctx, m, "dl", ro, 100); !IsCorrupt(err) || !strings.Contains(err.Error(), "preallocate") {
		t.Fatalf("preallocate: %v", err)
	}
	// Upstream Get errors propagate unchanged (needs a writable fd: the
	// preallocate Truncate runs before the Get).
	wo, err := os.OpenFile(filepath.Join(t.TempDir(), "w"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer wo.Close()
	if err := DownloadFileParallel(ctx, m, "absent", wo, 100); !IsNotFound(err) {
		t.Fatalf("absent download: %v", err)
	}
}
