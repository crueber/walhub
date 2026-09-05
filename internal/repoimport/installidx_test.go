// installidx_test.go — #95: installIdx must install the .idx atomically
// (temp-file + rename in the same dir), so concurrent readers of the
// serving copy never observe a torn index.
package repoimport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeIdxSource stages a pack + .idx sibling pair in dir and returns the
// pack path. EnsurePackIdx takes an existing sibling as-is (no git spawn).
func writeIdxSource(t *testing.T, dir, checksum string, idxBody []byte) string {
	t.Helper()
	packPath := filepath.Join(dir, "pack-"+checksum+".pack")
	if err := os.WriteFile(packPath, []byte("pack-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(packPath, ".pack")+".idx", idxBody, 0o644); err != nil {
		t.Fatal(err)
	}
	return packPath
}

// TestInstallIdxAtomicReplace pins the atomic-replace contract: a reader
// holding an fd opened BEFORE the replacement install must still see the
// complete OLD bytes afterward. Rename installs a new inode; an in-place
// WriteFile would rewrite the same inode, so the old fd would read the
// new bytes (or a torn mix for large indexes). Fails pre-#95, passes post.
func TestInstallIdxAtomicReplace(t *testing.T) {
	svc, _ := testService(t, nil, nil)
	ctx := context.Background()
	h, err := svc.reg.Create(ctx, "acme/atom", 0)
	if err != nil {
		t.Fatal(err)
	}
	checksum := strings.Repeat("a", 40)
	v1 := bytes.Repeat([]byte("1"), 1<<16)
	v2 := bytes.Repeat([]byte("2"), 1<<16)
	scratch := t.TempDir()

	if _, err := svc.installIdx(ctx, h, writeIdxSource(t, scratch, checksum, v1), checksum); err != nil {
		t.Fatal(err)
	}
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")

	old, err := os.Open(servingIdx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	if _, err := svc.installIdx(ctx, h, writeIdxSource(t, scratch, checksum, v2), checksum); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, v1) {
		t.Fatalf("pre-open fd saw %d bytes, want complete old %d bytes (install rewrote in place)", len(got), len(v1))
	}
	cur, err := os.ReadFile(servingIdx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cur, v2) {
		t.Fatalf("serving idx = %d bytes, want complete new %d bytes", len(cur), len(v2))
	}
	if _, err := os.Lstat(servingIdx + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

// TestInstallIdxRenameFailureCleansTemp covers the failure paths: when the
// rename cannot land (destination occupied by a directory), installIdx
// reports "install idx" and leaves no temp file behind.
func TestInstallIdxRenameFailureCleansTemp(t *testing.T) {
	svc, _ := testService(t, nil, nil)
	ctx := context.Background()
	h, err := svc.reg.Create(ctx, "acme/atomfail", 0)
	if err != nil {
		t.Fatal(err)
	}
	checksum := strings.Repeat("b", 40)
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	if err := os.MkdirAll(servingIdx, 0o755); err != nil {
		t.Fatal(err)
	}
	packPath := writeIdxSource(t, t.TempDir(), checksum, []byte("idx-body"))
	if _, err := svc.installIdx(ctx, h, packPath, checksum); err == nil {
		t.Fatal("rename onto a directory must fail")
	} else if !strings.Contains(err.Error(), "install idx") {
		t.Fatalf("error = %q, want it to mention install idx", err)
	}
	if _, err := os.Lstat(servingIdx + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after failed rename: %v", err)
	}
}

// TestInstallIdxConcurrentReadersSeeCompleteVersions is the #95 property
// ratchet: while installs alternate between two payloads, every read of
// the serving path must be exactly one complete version — never a torn
// mix. Rename atomicity guarantees this; run under -race.
func TestInstallIdxConcurrentReadersSeeCompleteVersions(t *testing.T) {
	svc, _ := testService(t, nil, nil)
	ctx := context.Background()
	h, err := svc.reg.Create(ctx, "acme/atomrace", 0)
	if err != nil {
		t.Fatal(err)
	}
	checksum := strings.Repeat("c", 40)
	v1 := bytes.Repeat([]byte("1"), 1<<16)
	v2 := bytes.Repeat([]byte("2"), 1<<16)
	scratch := t.TempDir()
	if _, err := svc.installIdx(ctx, h, writeIdxSource(t, scratch, checksum, v1), checksum); err != nil {
		t.Fatal(err)
	}
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")

	stop := make(chan struct{})
	bad := make(chan string, 64)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := os.ReadFile(servingIdx)
				if err != nil {
					continue
				}
				if !bytes.Equal(b, v1) && !bytes.Equal(b, v2) {
					select {
					case bad <- fmt.Sprintf("torn read: %d bytes", len(b)):
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		body := v1
		if i%2 == 1 {
			body = v2
		}
		if _, err := svc.installIdx(ctx, h, writeIdxSource(t, scratch, checksum, body), checksum); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case s := <-bad:
		t.Fatal(s)
	default:
	}
}
