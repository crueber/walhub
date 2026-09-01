package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
)

// The store contract suite (15_testing.md §2): ONE suite, every backend.
// runContract exercises every observable guarantee of ObjectStore against
// `store`, using keys under `prefix`. All state is cleaned up before return.
//
// Wiring:
//   - TestContract_Memory / TestContract_Filesystem always run (fast tier,
//     `make contract`).
//   - TestContractS3 runs when WALHUB_TEST_S3_ENDPOINT is set (rustfs from
//     `make dev-store`); TestContractGCS when WALHUB_TEST_GCS_BUCKET is set.
func runContract(t *testing.T, s ObjectStore, prefix string) {
	t.Helper()
	ctx := context.Background()

	// Cleanup: delete everything under the prefix before returning,
	// tolerating absent keys (15_testing.md §2: bounded by 30 s).
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var keys []string
		_ = s.List(cctx, prefix, "", func(m ObjectMeta) error { keys = append(keys, m.Key); return nil })
		for _, k := range keys {
			_ = s.Delete(cctx, k, "")
		}
	})

	t.Run("TestContract_PutCreateWinsOnce", func(t *testing.T) {
		k := prefix + "create.pb"
		m1, err := s.Put(ctx, k, PutBody{Bytes: []byte("winner")}, PutOptions{Mode: PutCreate})
		if err != nil {
			t.Fatalf("first create: %v", err)
		}
		_, err = s.Put(ctx, k, PutBody{Bytes: []byte("loser")}, PutOptions{Mode: PutCreate})
		if !IsPreconditionFailed(err) {
			t.Fatalf("second create: %v", err)
		}
		if cur, ok := PreconditionCurrent(err); ok && cur != m1.Version {
			t.Logf("note: current version %q differs from meta %q", cur, m1.Version)
		}
		b, _, err := GetBytes(ctx, s, k, GetOptions{})
		if err != nil || string(b) != "winner" {
			t.Fatalf("winner content: %q %v", b, err)
		}
	})

	t.Run("TestContract_PutUpdateCAS", func(t *testing.T) {
		k := prefix + "cas.pb"
		v1, err := s.Put(ctx, k, PutBody{Bytes: []byte("one")}, PutOptions{Mode: PutCreate})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Put(ctx, k, PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: "wrong"})
		if !IsPreconditionFailed(err) {
			t.Fatalf("wrong-version update: %v", err)
		}
		cur, ok := PreconditionCurrent(err)
		if !ok || cur == "" {
			t.Fatalf("PreconditionFailed must carry the current version (S3 HEADs after a lost CAS): %v", err)
		}
		v2, err := s.Put(ctx, k, PutBody{Bytes: []byte("two")}, PutOptions{Mode: PutUpdate, IfVersion: v1.Version})
		if err != nil {
			t.Fatalf("right-version update: %v", err)
		}
		if v2.Version == v1.Version {
			t.Fatal("version token did not advance")
		}
	})

	t.Run("TestContract_GetIfNoneMatch", func(t *testing.T) {
		k := prefix + "304.pb"
		meta, err := s.Put(ctx, k, PutBody{Bytes: []byte("body")}, PutOptions{Mode: PutCreate})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Get(ctx, k, GetOptions{IfNoneMatch: meta.Version})
		if err != nil {
			t.Fatalf("if-none-match on current version: %v", err)
		}
		nm, ok := res.(NotModified)
		if !ok || nm.Version != meta.Version {
			t.Fatalf("want NotModified{version}, got %#v", res)
		}
		// A stale version returns the full object.
		res, err = s.Get(ctx, k, GetOptions{IfNoneMatch: "stale-version-token"})
		if err != nil {
			t.Fatalf("if-none-match on stale version: %v", err)
		}
		o, ok := res.(Object)
		if !ok {
			t.Fatalf("want Object, got %#v", res)
		}
		defer o.Body.Close()
		b, _ := io.ReadAll(o.Body)
		if string(b) != "body" {
			t.Fatalf("stale get body %q", b)
		}
	})

	t.Run("TestContract_GetIfMatch_Mismatch", func(t *testing.T) {
		k := prefix + "ifmatch.pb"
		meta, err := s.Put(ctx, k, PutBody{Bytes: []byte("body")}, PutOptions{Mode: PutCreate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, k, GetOptions{IfMatch: "not-the-version"}); !IsPreconditionFailed(err) {
			t.Fatalf("if-match mismatch: %v", err)
		}
		res, err := s.Get(ctx, k, GetOptions{IfMatch: meta.Version})
		if err != nil {
			t.Fatalf("if-match on current version: %v", err)
		}
		if o, ok := res.(Object); !ok {
			t.Fatalf("want Object, got %#v", res)
		} else {
			o.Body.Close()
		}
	})

	t.Run("TestContract_RangeReads", func(t *testing.T) {
		k := prefix + "range.pack"
		body := []byte("0123456789")
		if _, err := s.Put(ctx, k, PutBody{Bytes: body}, PutOptions{Mode: PutCreate}); err != nil {
			t.Fatal(err)
		}
		read := func(start, end int64) (string, int64) {
			res, err := s.Get(ctx, k, GetOptions{Range: &[2]int64{start, end}})
			if err != nil {
				t.Fatalf("range [%d,%d): %v", start, end, err)
			}
			o := res.(Object)
			defer o.Body.Close()
			b, _ := io.ReadAll(o.Body)
			return string(b), o.Meta.Size
		}
		// Half-open [start,end), last byte, clamped end — and the size in the
		// returned meta is the WHOLE object, never the range.
		if b, size := read(2, 5); b != "234" || size != 10 {
			t.Fatalf("[2,5): %q size=%d", b, size)
		}
		if b, size := read(9, 10); b != "9" || size != 10 {
			t.Fatalf("[9,10): %q size=%d", b, size)
		}
		if b, size := read(5, 100); b != "56789" || size != 10 {
			t.Fatalf("clamped [5,100): %q size=%d", b, size)
		}
		// start == size is the legal empty-suffix read.
		if b, _ := read(10, 10); b != "" {
			t.Fatalf("empty suffix: %q", b)
		}
	})

	t.Run("TestContract_HeadAndAbsent", func(t *testing.T) {
		k := prefix + "head.pb"
		if hm, err := s.Head(ctx, prefix+"absent.pb"); err != nil || hm != nil {
			t.Fatalf("head absent: %+v %v", hm, err)
		}
		if _, err := s.Get(ctx, prefix+"absent.pb", GetOptions{}); !IsNotFound(err) {
			t.Fatalf("get absent: %v", err)
		}
		meta, err := s.Put(ctx, k, PutBody{Bytes: []byte("xyzzy")}, PutOptions{Mode: PutCreate})
		if err != nil {
			t.Fatal(err)
		}
		hm, err := s.Head(ctx, k)
		if err != nil || hm == nil {
			t.Fatalf("head present: %+v %v", hm, err)
		}
		if hm.Size != 5 || hm.Version != meta.Version {
			t.Fatalf("head meta: %+v want size 5 version %q", hm, meta.Version)
		}
	})

	t.Run("TestContract_Delete", func(t *testing.T) {
		k := prefix + "delete.pb"
		// Unconditional delete of an absent key is Ok (idempotence).
		if err := s.Delete(ctx, k, ""); err != nil {
			t.Fatalf("unconditional absent: %v", err)
		}
		meta, err := s.Put(ctx, k, PutBody{Bytes: []byte("doomed")}, PutOptions{Mode: PutCreate})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, k, "wrong-version"); !IsPreconditionFailed(err) {
			t.Fatalf("wrong-version delete: %v", err)
		}
		if err := s.Delete(ctx, k, meta.Version); err != nil {
			t.Fatalf("right-version delete: %v", err)
		}
		if hm, _ := s.Head(ctx, k); hm != nil {
			t.Fatal("object survived deletion")
		}
	})

	t.Run("TestContract_ListOrderingAndPrefixIsolation", func(t *testing.T) {
		base := prefix + "list/"
		for _, k := range []string{"b", "a", "c"} {
			if _, err := s.Put(ctx, base+k, PutBody{Bytes: []byte(k)}, PutOptions{Mode: PutCreate}); err != nil {
				t.Fatal(err)
			}
		}
		// A sibling subtree must never leak into the listing.
		if _, err := s.Put(ctx, prefix+"other/x", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutCreate}); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := s.List(ctx, base, "", func(m ObjectMeta) error { got = append(got, m.Key); return nil }); err != nil {
			t.Fatal(err)
		}
		want := []string{base + "a", base + "b", base + "c"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("list order: %v want %v", got, want)
		}
		// startAfter is strict.
		got = nil
		if err := s.List(ctx, base, base+"a", func(m ObjectMeta) error { got = append(got, m.Key); return nil }); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint([]string{base + "b", base + "c"}) {
			t.Fatalf("startAfter: %v", got)
		}
	})

	t.Run("TestContract_ListPrefixes", func(t *testing.T) {
		base := prefix + "delim/"
		for _, k := range []string{"g1/x", "g1/y", "g2/z", "loose"} {
			if _, err := s.Put(ctx, base+k, PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutCreate}); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		if err := s.ListPrefixes(ctx, base, func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatal(err)
		}
		want := []string{base + "g1/", base + "g2/"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("prefixes: %v want %v", got, want)
		}
	})

	t.Run("TestContract_LargeStreamedRoundtrip", func(t *testing.T) {
		k := prefix + "large.pack"
		body := make([]byte, 3<<20) // multi-MiB streamed body, KNOWN length
		for i := range body {
			body[i] = byte(i * 7)
		}
		sum := sha256.Sum256(body)
		meta, err := s.Put(ctx, k, PutBody{Stream: bytes.NewReader(body), StreamLen: int64(len(body))},
			PutOptions{Mode: PutCreate, ContentType: "application/octet-stream"})
		if err != nil {
			t.Fatalf("streamed put: %v", err)
		}
		if meta.Size != int64(len(body)) {
			t.Fatalf("size %d want %d", meta.Size, len(body))
		}
		res, err := s.Get(ctx, k, GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		o := res.(Object)
		defer o.Body.Close()
		h := sha256.New()
		if _, err := io.Copy(h, o.Body); err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != hex.EncodeToString(sum[:]) {
			t.Fatal("streamed roundtrip checksum mismatch")
		}
	})

	t.Run("TestContract_MultipartPath", func(t *testing.T) {
		// A body above the multipart threshold lands as ONE object with
		// identical bytes and leaves no part objects listed. The threshold is
		// injected per-backend (S3 via config); backends without multipart
		// semantics simply store the body whole.
		k := prefix + "multipart.pack"
		body := make([]byte, 5<<20)
		for i := range body {
			body[i] = byte(i * 13)
		}
		meta, err := s.Put(ctx, k, PutBody{Bytes: body}, PutOptions{Mode: PutOverwrite})
		if err != nil {
			t.Fatalf("multipart put: %v", err)
		}
		if meta.Size != int64(len(body)) {
			t.Fatalf("size %d want %d", meta.Size, len(body))
		}
		b, _, err := GetBytes(ctx, s, k, GetOptions{})
		if err != nil || !bytes.Equal(b, body) {
			t.Fatalf("multipart bytes: %d %v", len(b), err)
		}
		var parts []string
		if err := s.List(ctx, prefix, "", func(m ObjectMeta) error {
			if IsPartKey(m.Key) {
				parts = append(parts, m.Key)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(parts) != 0 {
			t.Fatalf("multipart left part objects: %v", parts)
		}
	})

	t.Run("TestContract_Compose", func(t *testing.T) {
		base := prefix + "compose/"
		// 1..=32 sources compose in order; sources remain in place.
		for _, n := range []int{1, 2, 32} {
			srcs := make([]string, n)
			for i := range srcs {
				srcs[i] = fmt.Sprintf("%ssrc-%d-%02d", base, n, i)
				if _, err := s.Put(ctx, srcs[i], PutBody{Bytes: []byte(fmt.Sprintf("%02d", i))}, PutOptions{Mode: PutOverwrite}); err != nil {
					t.Fatal(err)
				}
			}
			dst := fmt.Sprintf("%scat-%d", base, n)
			if _, err := s.Compose(ctx, dst, srcs, PutOptions{Mode: PutOverwrite}); err != nil {
				t.Fatalf("compose %d sources: %v", n, err)
			}
			b, _, err := GetBytes(ctx, s, dst, GetOptions{})
			if err != nil {
				t.Fatalf("compose %d readback: %v", n, err)
			}
			var want strings.Builder
			for i := range n {
				fmt.Fprintf(&want, "%02d", i)
			}
			if string(b) != want.String() {
				t.Fatalf("compose %d bytes: %q want %q", n, b, want.String())
			}
			for _, src := range srcs {
				if hm, _ := s.Head(ctx, src); hm == nil {
					t.Fatalf("source %q removed by compose", src)
				}
			}
		}
		// Compose honors the dest mode.
		dst := base + "mode"
		if _, err := s.Put(ctx, dst, PutBody{Bytes: []byte("dst")}, PutOptions{Mode: PutOverwrite}); err != nil {
			t.Fatal(err)
		}
		src1 := base + "m1"
		if _, err := s.Put(ctx, src1, PutBody{Bytes: []byte("s")}, PutOptions{Mode: PutOverwrite}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Compose(ctx, dst, []string{src1}, PutOptions{Mode: PutCreate}); !IsPreconditionFailed(err) {
			t.Fatalf("compose create on existing: %v", err)
		}
		// > 32 sources → InvalidArgument.
		many := make([]string, 33)
		for i := range many {
			many[i] = src1
		}
		if _, err := s.Compose(ctx, dst, many, PutOptions{Mode: PutOverwrite}); !IsInvalidArgument(err) {
			t.Fatalf("33 sources: %v", err)
		}
		// Missing source → NotFound.
		if _, err := s.Compose(ctx, dst, []string{base + "nope"}, PutOptions{Mode: PutOverwrite}); !IsNotFound(err) {
			t.Fatalf("missing source: %v", err)
		}
	})

	t.Run("TestContract_LeaseSteal", func(t *testing.T) {
		// Protocol-level lease case on top of CAS (§4.9): absent lease →
		// Create (epoch 0); steal only once now ≥ expires_at + 2s, via Update
		// with the observed version, writing epoch = existing.epoch + 1; an
		// early steal that lost the version race → 412; release deletes;
		// releasing an absent lease is Ok.
		k := prefix + "leases/test.pb"
		type lease struct {
			Holder    string `json:"holder"`
			Epoch     int    `json:"epoch"`
			ExpiresAt int64  `json:"expires_at"`
		}
		putLease := func(l lease, mode PutMode, ifV Version) (ObjectMeta, error) {
			b, _ := json.Marshal(l)
			return s.Put(ctx, k, PutBody{Bytes: b}, PutOptions{Mode: mode, IfVersion: ifV})
		}
		readLease := func() lease {
			b, _, err := GetBytes(ctx, s, k, GetOptions{})
			if err != nil {
				t.Fatalf("read lease: %v", err)
			}
			var l lease
			if err := json.Unmarshal(b, &l); err != nil {
				t.Fatal(err)
			}
			return l
		}
		head := func() Version {
			m, err := s.Head(ctx, k)
			if err != nil || m == nil {
				t.Fatalf("lease head: %v %v", m, err)
			}
			return m.Version
		}
		now := time.Now().Unix()

		// Absent lease → Create with epoch 0.
		if _, err := putLease(lease{Holder: "A", ExpiresAt: now + 60}, PutCreate, ""); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		cur := readLease()
		if cur.Epoch != 0 || cur.Holder != "A" {
			t.Fatalf("initial lease: %+v", cur)
		}
		// Early steal (now < expires_at): A refreshes first, so B's rewrite
		// against the version it observed loses the race → 412. The epoch
		// must NOT advance on a lost race.
		v1 := head()
		if _, err := putLease(lease{Holder: "A", ExpiresAt: now + 60}, PutUpdate, v1); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		_, err := putLease(lease{Holder: "B", Epoch: cur.Epoch + 1, ExpiresAt: now + 60}, PutUpdate, v1)
		if !IsPreconditionFailed(err) {
			t.Fatalf("early steal: %v", err)
		}
		// Steal after expiry: the lease content now reads expired, B rewrites
		// epoch = existing.epoch + 1 via Update with the observed version.
		v2 := head()
		if _, err := putLease(lease{Holder: "A", Epoch: cur.Epoch, ExpiresAt: now - 10}, PutUpdate, v2); err != nil {
			t.Fatalf("expire: %v", err)
		}
		v3 := head()
		expired := readLease()
		if now < expired.ExpiresAt+2 {
			t.Fatalf("lease not expired: %+v", expired)
		}
		stolen := lease{Holder: "B", Epoch: expired.Epoch + 1, ExpiresAt: now + 60}
		if _, err := putLease(stolen, PutUpdate, v3); err != nil {
			t.Fatalf("steal: %v", err)
		}
		if after := readLease(); after.Holder != "B" || after.Epoch != expired.Epoch+1 {
			t.Fatalf("stolen lease must carry epoch = existing.epoch + 1: %+v", after)
		}
		// Release deletes; releasing an already-released/absent lease is Ok.
		v4 := head()
		if err := s.Delete(ctx, k, v4); err != nil {
			t.Fatalf("release: %v", err)
		}
		if err := s.Delete(ctx, k, ""); err != nil {
			t.Fatalf("double release (unconditional): %v", err)
		}
		if err := s.Delete(ctx, k, v4); !IsNotFound(err) && err != nil {
			t.Fatalf("release of absent lease with stale version: %v", err)
		}
		if hm, _ := s.Head(ctx, k); hm != nil {
			t.Fatal("lease survived release")
		}
	})
}

// ---- wiring ----

func TestContract_Memory(t *testing.T) {
	runContract(t, NewMemory(), "")
}

func TestContract_Filesystem(t *testing.T) {
	f, err := NewFilesystemRoot(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	runContract(t, f, "test/fs/")
}

func TestContractS3(t *testing.T) {
	endpoint := os.Getenv("WALHUB_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set WALHUB_TEST_S3_ENDPOINT (rustfs from `make dev-store`) to run the S3 contract")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", firstNonEmpty(os.Getenv("WALHUB_TEST_S3_ACCESS_KEY"), "walhub"))
	t.Setenv("AWS_SECRET_ACCESS_KEY", firstNonEmpty(os.Getenv("WALHUB_TEST_S3_SECRET_KEY"), "walhub"))
	s, err := NewS3(&config.Store{
		Bucket:     firstNonEmpty(os.Getenv("WALHUB_TEST_S3_BUCKET"), "walhub-test"),
		MaxRetries: 4,
		S3: config.S3{
			Endpoint:       endpoint,
			ForcePathStyle: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runContract(t, s, contractRunPrefix("test/s3/"))
}

func TestContractGCS(t *testing.T) {
	if os.Getenv("WALHUB_TEST_GCS_BUCKET") == "" {
		t.Skip("set WALHUB_TEST_GCS_BUCKET to run the GCS contract")
	}
	t.Skip("no GCS backend implemented in this tree yet (03_store_backends.md §3); the wiring lands with it")
}

// contractRunPrefix isolates concurrent runs on shared backends (§2).
func contractRunPrefix(base string) string {
	var b [4]byte
	if _, err := cryptoRandRead(b[:]); err != nil {
		return base
	}
	return base + hex.EncodeToString(b[:]) + "/"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
