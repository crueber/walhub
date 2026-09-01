// striped.go: striped upload and striped download (03_store_backends.md §5).
// PutFileParallel uploads large files as concurrent part objects under
// "<key>.part/NNNN" and composes them server-side (two levels when more than
// 32 parts: 1024 = 32×32 parts max). DownloadFileParallel materializes a
// large object with 16 concurrent 32 MiB stripes; a short read is corrupt.
package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Tunables are vars so tests can shrink them (the contract suite injects
// smaller sizes for speed; production uses the §5 defaults).
var (
	// stripedUploadMinPart is the minimum part size: 64 MiB (§5.1).
	stripedUploadMinPart int64 = 64 << 20
	// stripedUploadMaxPart is the maximum part size: 1 GiB.
	stripedUploadMaxPart int64 = 1 << 30
	// stripedMaxParts = 32×32: two compose levels suffice (§5.1).
	stripedMaxParts = 32 * 32
	// stripedUploadThreshold: files ≤ this go through a single Put
	// (2 × MIN_PART = 128 MiB, matching the Rust put_file_parallel).
	stripedUploadThreshold = 2 * stripedUploadMinPart
	// stripedUploadStripes bounds concurrent part uploads (§5.1: 8).
	stripedUploadStripes = 8
	// downloadChunk / downloadStripes: 32 MiB chunks, 16 concurrent stripes (§5.2).
	downloadChunk   int64 = 32 << 20
	downloadStripes       = 16
)

// partSizeFor = clamp(ceil(size/1024), 64 MiB, 1 GiB).
func partSizeFor(size int64) int64 {
	ps := (size + int64(stripedMaxParts) - 1) / int64(stripedMaxParts)
	if ps < stripedUploadMinPart {
		ps = stripedUploadMinPart
	}
	if ps > stripedUploadMaxPart {
		ps = stripedUploadMaxPart
	}
	return ps
}

func partKey(key string, i int) string { return fmt.Sprintf("%s.part/%04d", key, i) }
func midKey(key string, g int) string  { return fmt.Sprintf("%s.part/mid%04d", key, g) }

// PutFileParallel uploads f (size bytes from its current position) as one
// object. Backends that cannot compose natively (S3) — and every file ≤ the
// threshold — take a single Put with the caller's PutMode (Create applies to
// the final object only). Otherwise parts upload concurrently as
// "<key>.part/NNNN" with PutOverwrite and are composed into key.
func PutFileParallel(ctx context.Context, s ObjectStore, key string, f *os.File, size int64, opts PutOptions) (ObjectMeta, error) {
	if !s.ComposeIsNative() || size <= stripedUploadThreshold {
		return s.Put(ctx, key, PutBody{Stream: f, StreamLen: size}, opts)
	}
	ps := partSizeFor(size)
	n := (size + ps - 1) / ps
	g, gctx := WithContext(ctx)
	g.SetLimit(stripedUploadStripes)
	for i := range n {
		i := i
		start := i * ps
		end := min(start+ps, size)
		g.Go(func() error {
			// Parts are content-addressed intermediates: overwrite is safe.
			_, err := s.Put(gctx, partKey(key, int(i)),
				PutBody{Stream: io.NewSectionReader(f, start, end-start), StreamLen: end - start},
				PutOptions{Mode: PutOverwrite})
			return err
		})
	}
	if err := g.Wait(); err != nil {
		cleanupParts(ctx, s, key, int(n))
		return ObjectMeta{}, err
	}
	return composeTwoLevels(ctx, s, key, int(n), opts) // deletes parts/mids best-effort
}

// composeTwoLevels composes n parts into key: ≤32 parts in one Compose; more
// go through per-32 intermediates ("mids") and one final compose (§5.1).
func composeTwoLevels(ctx context.Context, s ObjectStore, key string, n int, opts PutOptions) (ObjectMeta, error) {
	partKeys := make([]string, n)
	for i := range n {
		partKeys[i] = partKey(key, i)
	}
	cleanup := append([]string(nil), partKeys...)
	meta, err := func() (ObjectMeta, error) {
		if len(partKeys) <= 32 {
			return s.Compose(ctx, key, partKeys, opts)
		}
		ng := (len(partKeys) + 31) / 32
		var mids []string
		for g := range ng {
			chunk := partKeys[g*32 : min((g+1)*32, len(partKeys))]
			mk := midKey(key, g)
			if _, err := s.Compose(ctx, mk, chunk, PutOptions{Mode: PutOverwrite}); err != nil {
				return ObjectMeta{}, err
			}
			mids = append(mids, mk)
		}
		cleanup = append(cleanup, mids...)
		return s.Compose(ctx, key, mids, opts)
	}()
	// Best-effort cleanup after success OR failure; delete errors are never
	// propagated (§5.1). Cleanup runs on the PARENT context so it survives
	// the errgroup's cancellation (§5 Concurrency).
	for _, k := range cleanup {
		_ = s.Delete(ctx, k, "")
	}
	return meta, err
}

// cleanupParts deletes already-uploaded parts after a failed upload.
func cleanupParts(ctx context.Context, s ObjectStore, key string, n int) {
	for i := range n {
		_ = s.Delete(ctx, partKey(key, i), "")
	}
}

// DownloadFileParallel materializes key (size bytes known — from PackRef when
// available, else one HEAD first) into f: preallocate via Truncate, then 16
// concurrent stripes write 32 MiB chunks at their offsets via WriteAt. A
// short read is a Corrupt error — never silently padded (§5.2).
func DownloadFileParallel(ctx context.Context, s ObjectStore, key string, f *os.File, size int64) error {
	if err := f.Truncate(size); err != nil {
		return NewCorrupt(key, fmt.Errorf("preallocate: %w", err))
	}
	g, gctx := WithContext(ctx)
	g.SetLimit(downloadStripes)
	for off := int64(0); off < size; off += downloadChunk {
		end := min(off+downloadChunk, size) // half-open
		g.Go(func() error { return readRangeInto(gctx, s, key, off, end, f) })
	}
	return g.Wait()
}

// readRangeInto reads [start,end) of key and writes it at offset start of f.
func readRangeInto(ctx context.Context, s ObjectStore, key string, start, end int64, f *os.File) error {
	res, err := s.Get(ctx, key, GetOptions{Range: &[2]int64{start, end}})
	if err != nil {
		return err
	}
	o, ok := res.(Object)
	if !ok {
		return NewCorrupt(key, fmt.Errorf("unexpected GetResult %T for range read", res))
	}
	defer o.Body.Close()
	want := end - start
	got, err := io.Copy(io.NewOffsetWriter(f, start), o.Body)
	if err != nil {
		return NewRetryable(key, err)
	}
	if got != want {
		return NewCorrupt(key, fmt.Errorf("short read: got %d of %d bytes at offset %d", got, want, start))
	}
	return nil
}

// IsPartKey reports whether key is a striped-upload part or mid object
// ("<key>.part/NNNN" / "<key>.part/midNNNN"); used by cleanup and fsck.
func IsPartKey(key string) bool {
	i := strings.LastIndex(key, ".part/")
	if i < 0 {
		return false
	}
	rest := key[i+len(".part/"):]
	if len(rest) == 4 && allDigits(rest) {
		return true
	}
	return strings.HasPrefix(rest, "mid") && len(rest) == 7 && allDigits(rest[3:])
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}
