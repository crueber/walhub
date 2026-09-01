package bundle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Header rendering is byte-exact (§8.9.4):
//
//	v2: "# v2 git bundle\n" + ("-<oid> \n" per prerequisite, note trailing space)
//	    + ("<oid> <name>\n" per ref, HEAD first then refs sorted by name) + "\n"
//	v3: "# v3 git bundle\n" + "@object-format=<sha1|sha256>\n"
//	    + "@filter=blob:none\n" (filtered family only) + prerequisites + refs + "\n"
//
// v2 is used for sha1 unfiltered; v3 whenever the repo is sha256 or the
// strategy carries a filter. Composed fulls carry no prerequisites.

// RenderHeader renders a bundle header. sha256 selects v3, as does a non-empty
// filter (§8.9.4). refs are re-ordered defensively: HEAD first, then name-sorted.
func RenderHeader(sha256 bool, objectFormat, filter string, prereqs []string, refs []proto.Ref) []byte {
	v3 := sha256 || filter != ""
	var buf bytes.Buffer
	if v3 {
		buf.WriteString("# v3 git bundle\n")
		if objectFormat == "sha256" {
			buf.WriteString("@object-format=sha256\n")
		} else {
			buf.WriteString("@object-format=sha1\n")
		}
		if filter != "" {
			buf.WriteString("@filter=" + filter + "\n")
		}
	} else {
		buf.WriteString("# v2 git bundle\n")
	}
	for _, p := range prereqs {
		buf.WriteString("-")
		buf.WriteString(p)
		buf.WriteString(" \n") // trailing space is normative
	}
	for _, r := range orderForHeader(refs) {
		buf.WriteString(r.Oid)
		buf.WriteString(" ")
		buf.WriteString(r.Name)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	return buf.Bytes()
}

// orderForHeader puts HEAD first, then the rest sorted by name (§8.9.4).
func orderForHeader(refs []proto.Ref) []proto.Ref {
	out := make([]proto.Ref, 0, len(refs))
	var head *proto.Ref
	for i := range refs {
		if refs[i].Name == "HEAD" && head == nil {
			head = &refs[i]
			continue
		}
		out = append(out, refs[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if head != nil {
		out = append([]proto.Ref{*head}, out...)
	}
	return out
}

// ErrPackMagic is returned when no PACK magic is found in a bundle file.
var ErrPackMagic = errors.New("bundle: PACK magic not found in bundle file")

// ScanPackOffset returns the byte offset of the PACK magic in a bundle file
// (§8.9.3: the header/pack split used for composition). Scanned with an
// 8-byte overlap window so a magic straddling a chunk boundary is found.
func ScanPackOffset(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	var base int64
	var carry []byte // up to 8 bytes carried between chunks (overlap window)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			data := make([]byte, 0, len(carry)+n)
			data = append(data, carry...)
			data = append(data, buf[:n]...)
			if idx := bytes.Index(data, []byte("PACK")); idx >= 0 {
				off := base - int64(len(carry)) + int64(idx)
				if off < 0 {
					off = int64(idx)
				}
				return off, nil
			}
			if len(data) > 8 {
				carry = append(carry[:0], data[len(data)-8:]...)
			} else {
				carry = append(carry[:0], data...)
			}
			base += int64(n)
		}
		if rerr == io.EOF {
			return 0, ErrPackMagic
		}
		if rerr != nil {
			return 0, rerr
		}
	}
}

// FileSize returns the on-disk size of a file (publish bookkeeping).
func FileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("bundle: stat %s: %w", path, err)
	}
	return st.Size(), nil
}
