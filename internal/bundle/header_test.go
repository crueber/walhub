package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestRenderHeaderGolden is §8.9.4 byte-exact.
func TestRenderHeaderGolden(t *testing.T) {
	refs := []proto.Ref{
		{Name: "refs/heads/main", Oid: "1111111111111111111111111111111111111111"},
		{Name: "HEAD", Oid: "2222222222222222222222222222222222222222"},
		{Name: "refs/tags/v1", Oid: "3333333333333333333333333333333333333333"},
	}

	t.Run("v2 sha1 unfiltered", func(t *testing.T) {
		got := RenderHeader(false, "sha1", "", []string{"4444444444444444444444444444444444444444"}, refs)
		want := "# v2 git bundle\n" +
			"-4444444444444444444444444444444444444444 \n" +
			"2222222222222222222222222222222222222222 HEAD\n" +
			"1111111111111111111111111111111111111111 refs/heads/main\n" +
			"3333333333333333333333333333333333333333 refs/tags/v1\n" +
			"\n"
		if string(got) != want {
			t.Fatalf("v2 header =\n%q\nwant\n%q", got, want)
		}
		// The trailing space after the prerequisite oid is normative.
		if !strings.Contains(string(got), "4444444444444444444444444444444444444444 \n") {
			t.Fatal("prerequisite line must end with one space before \\n")
		}
	})

	t.Run("v3 sha256", func(t *testing.T) {
		got := RenderHeader(true, "sha256", "", nil, refs)
		want := "# v3 git bundle\n@object-format=sha256\n2222222222222222222222222222222222222222 HEAD\n1111111111111111111111111111111111111111 refs/heads/main\n3333333333333333333333333333333333333333 refs/tags/v1\n\n"
		if string(got) != want {
			t.Fatalf("v3 sha256 header =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("v3 filtered sha1", func(t *testing.T) {
		got := RenderHeader(false, "sha1", FilterBlobNone, []string{"4444444444444444444444444444444444444444"}, refs)
		want := "# v3 git bundle\n@object-format=sha1\n@filter=blob:none\n-4444444444444444444444444444444444444444 \n2222222222222222222222222222222222222222 HEAD\n1111111111111111111111111111111111111111 refs/heads/main\n3333333333333333333333333333333333333333 refs/tags/v1\n\n"
		if string(got) != want {
			t.Fatalf("v3 filtered header =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("composed fulls carry no prerequisites", func(t *testing.T) {
		got := RenderHeader(false, "sha1", "", nil, refs)
		if strings.Contains(string(got), "-") {
			t.Fatalf("composed fulls carry no prerequisites: %q", got)
		}
	})

	t.Run("HEAD ordering enforced", func(t *testing.T) {
		got := string(RenderHeader(false, "sha1", "", nil, refs))
		headIdx := strings.Index(got, "2222222222222222222222222222222222222222 HEAD")
		mainIdx := strings.Index(got, "refs/heads/main")
		if headIdx > mainIdx {
			t.Fatalf("HEAD must come first:\n%q", got)
		}
	})
}

// TestScanPackOffset covers the header/pack split scan (§8.9.3): magic at the
// start, mid-file, and straddling a chunk boundary.
func TestScanPackOffset(t *testing.T) {
	mk := func(name string, body []byte) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got, err := ScanPackOffset(mk("a", []byte("PACKrest"))); got != 0 || err != nil {
		t.Fatalf("start: %d %v", got, err)
	}
	if got, err := ScanPackOffset(mk("b", []byte(strings.Repeat("x", 100)+"PACK"+strings.Repeat("y", 10)))); got != 100 || err != nil {
		t.Fatalf("mid: %d %v", got, err)
	}
	// Straddle a 64 KiB chunk boundary.
	big := make([]byte, 64<<10-1)
	for i := range big {
		big[i] = 'x'
	}
	big = append(big, 'P', 'A', 'C', 'K')
	if got, err := ScanPackOffset(mk("c", big)); got != int64(len(big)-4) || err != nil {
		t.Fatalf("straddle: %d %v", got, err)
	}
	if _, err := ScanPackOffset(mk("d", []byte("no magic here"))); err == nil {
		t.Fatal("want ErrPackMagic for a magicless file")
	}
}
