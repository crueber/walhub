package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreErrorStrings(t *testing.T) {
	cases := []struct {
		err  *StoreError
		want string
	}{
		{NewNotFound("k"), "object not found: k"},
		{NewPrecondition("k", "v1"), `precondition failed: k (current="v1")`},
		{NewRetryable("k", errors.New("boom")), "retryable store error: k: boom"},
		{NewInvalid("k", errors.New("x")), "invalid argument: k"},
		{NewCorrupt("k", errors.New("bad")), "corrupt: k: bad"},
		{NewOther("k", errors.New("meh")), "store error: k: meh"},
	}
	for _, c := range cases {
		if c.err.Error() != c.want {
			t.Errorf("Error() = %q, want %q", c.err.Error(), c.want)
		}
	}
}

type wrappedErr struct{ error }

func (w wrappedErr) Unwrap() error { return w.error }

func TestKindIsAndPreconditionCurrent(t *testing.T) {
	wrapped := wrappedErr{NewNotFound("k")}
	if !IsNotFound(wrapped) {
		t.Fatal("wrapped NotFound not detected")
	}
	if IsNotFound(errors.New("plain")) || IsNotFound(nil) {
		t.Fatal("plain errors misclassified")
	}
	if !IsPreconditionFailed(fmt.Errorf("wrap: %w", NewPrecondition("k", "v"))) {
		t.Fatal("PreconditionFailed lost through wrapping")
	}
	se := NewPrecondition("k", "cur")
	if cur, ok := PreconditionCurrent(se); !ok || cur != "cur" {
		t.Fatalf("PreconditionCurrent = %q, %v", cur, ok)
	}
	if _, ok := PreconditionCurrent(NewNotFound("k")); ok {
		t.Fatal("PreconditionCurrent on wrong kind")
	}
	if _, ok := PreconditionCurrent(errors.New("x")); ok {
		t.Fatal("PreconditionCurrent on non-StoreError")
	}
	if !IsRetryable(NewRetryable("k", nil)) || !IsInvalidArgument(NewInvalid("k", nil)) || !IsCorrupt(NewCorrupt("k", nil)) {
		t.Fatal("kind predicates")
	}
}

// unknownResult exercises GetBytes' default branch.
type unknownResult struct{}

func (unknownResult) isGetResult() {}

type stubStore struct {
	ObjectStore
	res GetResult
	err error
}

func (s *stubStore) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	return s.res, s.err
}

func TestGetBytesVariants(t *testing.T) {
	ctx := context.Background()
	// Object: body read.
	obj := Object{Meta: ObjectMeta{Key: "k", Size: 5, Version: "v"},
		Body: io.NopCloser(strings.NewReader("hello"))}
	b, meta, err := GetBytes(ctx, &stubStore{res: obj}, "k", GetOptions{})
	if err != nil || string(b) != "hello" || meta.Version != "v" {
		t.Fatalf("GetBytes object: %q %+v %v", b, meta, err)
	}
	// NotModified: nil bytes, version surfaced.
	b, meta, err = GetBytes(ctx, &stubStore{res: NotModified{Version: "v9"}}, "k", GetOptions{})
	if err != nil || b != nil || meta.Version != "v9" {
		t.Fatalf("GetBytes 304: %q %+v %v", b, meta, err)
	}
	// Unknown variant → Other.
	_, _, err = GetBytes(ctx, &stubStore{res: unknownResult{}}, "k", GetOptions{})
	if !IsOther(err) {
		t.Fatalf("unknown variant should map to Other, got %v", err)
	}
	// Error passthrough.
	_, _, err = GetBytes(ctx, &stubStore{err: NewNotFound("k")}, "k", GetOptions{})
	if !IsNotFound(err) {
		t.Fatalf("passthrough: %v", err)
	}
}

// IsOther is not exported in store.go — this mirrors kindIs for ErrKindOther.
func IsOther(err error) bool { return kindIs(err, ErrKindOther) }

func TestExtensionHelpers(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	// PutBytes + Exists.
	if _, err := PutBytes(ctx, m, "k", []byte("v"), PutOptions{Mode: PutCreate}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	ok, err := Exists(ctx, m, "k")
	if err != nil || !ok {
		t.Fatalf("Exists present: %v %v", ok, err)
	}
	if ok, err = Exists(ctx, m, "absent"); err != nil || ok {
		t.Fatalf("Exists absent: %v %v", ok, err)
	}
	// GetIfChanged: fresh version → NotModified.
	meta, _ := m.Head(ctx, "k")
	res, err := GetIfChanged(ctx, m, "k", meta.Version)
	if err != nil {
		t.Fatalf("GetIfChanged: %v", err)
	}
	if _, isNM := res.(NotModified); !isNM {
		t.Fatalf("GetIfChanged = %T", res)
	}
	// GetBytes on absent surfaces the backend's NotFound (the caller decides
	// absent-vs-unchanged via IsNotFound).
	b, _, err := GetBytes(ctx, m, "absent", GetOptions{})
	if !IsNotFound(err) || b != nil {
		t.Fatalf("GetBytes absent: %q %v", b, err)
	}
}

func TestNewPrefixedNormalization(t *testing.T) {
	m := NewMemory()
	if got := NewPrefixed(m, ""); got != ObjectStore(m) {
		t.Fatal("empty prefix must return the inner store")
	}
	p := NewPrefixed(m, "/repos/o/r")
	pf, ok := p.(*Prefixed)
	if !ok || pf.Prefix != "repos/o/r/" {
		t.Fatalf("prefix normalization: %#v", p)
	}
}

func TestPrefixedWrap(t *testing.T) {
	ctx := context.Background()
	inner := NewMemory()
	p := NewPrefixed(inner, "tenant").(*Prefixed)

	if _, err := p.Put(ctx, "a.txt", PutBody{Bytes: []byte("A")}, PutOptions{Mode: PutCreate}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The prefixed key exists in the inner store; the bare key does not.
	if hm, err := inner.Head(ctx, "a.txt"); err != nil || hm != nil {
		t.Fatalf("bare key should be absent in inner store, got %+v %v", hm, err)
	}
	if _, err := p.Get(ctx, "a.txt", GetOptions{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, meta, err := GetBytes(ctx, p, "a.txt", GetOptions{})
	if err != nil || string(b) != "A" || meta.Key != "a.txt" {
		t.Fatalf("GetBytes via prefix: %q %+v %v", b, meta, err)
	}
	hm, err := p.Head(ctx, "a.txt")
	if err != nil || hm == nil || hm.Key != "a.txt" {
		t.Fatalf("Head via prefix: %+v %v", hm, err)
	}

	// List/ListPrefixes strip the prefix.
	if _, err := p.Put(ctx, "d/x.txt", PutBody{Bytes: []byte("x")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := p.List(ctx, "", "", func(m ObjectMeta) error { keys = append(keys, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "a.txt" || keys[1] != "d/x.txt" {
		t.Fatalf("List via prefix: %v", keys)
	}
	var pfx []string
	if err := p.ListPrefixes(ctx, "", func(s string) error { pfx = append(pfx, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(pfx) != 1 || pfx[0] != "d/" {
		t.Fatalf("ListPrefixes via prefix: %v", pfx)
	}

	// Delete / Compose / URLs / passthrough flags.
	if err := p.Delete(ctx, "a.txt", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Put(ctx, "s1", PutBody{Bytes: []byte("1")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Put(ctx, "s2", PutBody{Bytes: []byte("2")}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Compose(ctx, "cat", []string{"s1", "s2"}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	bc, _, err := GetBytes(ctx, p, "cat", GetOptions{})
	if err != nil || string(bc) != "12" {
		t.Fatalf("Compose via prefix: %q %v", bc, err)
	}
	u, err := p.SignedGetURL(ctx, "cat", time.Minute)
	if err != nil || u == nil || !strings.Contains(*u, "tenant/cat") {
		t.Fatalf("SignedGetURL via prefix: %v %v", u, err)
	}
	if p.Backend() != "memory" || !p.SupportsCompose() || !p.ComposeIsNative() {
		t.Fatal("flag passthrough")
	}
	at, err := p.AccelTarget(ctx, "cat")
	if err != nil || at != nil { // memory: accel off by default
		t.Fatalf("AccelTarget: %v %v", at, err)
	}
}

func TestIsBulkKey(t *testing.T) {
	cases := []struct {
		key  string
		bulk bool
	}{
		{"repos/o/r/manifest.pb", false},
		{"repos/o/r/policy.json", false},
		{"fsck.pb", false},
		{"wal/abc.pack", true},
		{"wal/abc.idx", true},
		{"bundles/horizon/b", true},
		{"lfs/objects/aa/bb/oid", true},
		{"bundles/list.pb", false},
		{"repos/o/r/checkpoints/0001/checkpoint.pb", false},
		{"plain", false},
		{"dir/plain", false},
	}
	for _, c := range cases {
		if got := IsBulkKey(c.key); got != c.bulk {
			t.Errorf("IsBulkKey(%q) = %v, want %v", c.key, got, c.bulk)
		}
	}
}

func TestReadPutBody(t *testing.T) {
	f := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(f, []byte("filedata"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := readPutBody(PutBody{Bytes: []byte("bytes")})
	if err != nil || string(b) != "bytes" {
		t.Fatalf("bytes: %q %v", b, err)
	}
	b, err = readPutBody(PutBody{Stream: strings.NewReader("stream"), StreamLen: 6})
	if err != nil || string(b) != "stream" {
		t.Fatalf("stream: %q %v", b, err)
	}
	b, err = readPutBody(PutBody{File: f})
	if err != nil || string(b) != "filedata" {
		t.Fatalf("file: %q %v", b, err)
	}
	if _, err := readPutBody(PutBody{Stream: strings.NewReader("xy"), StreamLen: 5}); err == nil {
		t.Fatal("short stream must error")
	}
	if _, err := readPutBody(PutBody{Stream: strings.NewReader("xy"), StreamLen: -1}); err == nil {
		t.Fatal("negative stream length must error")
	}
	if _, err := readPutBody(PutBody{File: f + ".missing"}); err == nil {
		t.Fatal("missing file must error")
	}
	if _, err := readPutBody(PutBody{}); err == nil {
		t.Fatal("empty body must error")
	}
}

func TestRandHex(t *testing.T) {
	a, b := randHex(8), randHex(8)
	if len(a) != 16 || len(b) != 16 {
		t.Fatalf("lengths %q %q", a, b)
	}
	if a == b {
		t.Fatal("randHex repeated")
	}
}
