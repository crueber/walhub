// util.go — small shared helpers: effective config (D24 merge), local pack
// state, fsck.pb get/put, scratch copies (reflink when supported), disk-free
// pre-flight.
package maintain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// effectiveConfig evaluates the per-repo settings (D24) merged over the host
// config, fresh each pass (§3.1): a repo that turns compaction off stops
// triggering.
func effectiveConfig(host *config.Config, m *proto.Manifest) (*config.Config, error) {
	if m == nil || m.Settings == nil || m.Settings.Toml == "" {
		return host, nil
	}
	rs, err := config.ParseRepoSettings([]byte(m.Settings.Toml))
	if err != nil {
		return nil, fmt.Errorf("repo settings: %w", err)
	}
	return rs.Merge(host)
}

// localPackState stats the serving objects/pack dir for live pack presence
// (§3.2 step 6: local disk state via stat, never bulk store I/O).
func localPackState(rep Repo) LocalState {
	present := map[string]bool{}
	dir := rep.Local().PackDir()
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".pack") && strings.HasPrefix(name, "pack-") {
				present[strings.TrimSuffix(strings.TrimPrefix(name, "pack-"), ".pack")] = true
			}
		}
	}
	return LocalState{Present: present}
}

// getFsckReport reads fsck.pb (repo-relative); (false, nil) when absent.
func getFsckReport(ctx context.Context, st store.ObjectStore, prefix string, out *proto.FsckReport) (bool, error) {
	if st == nil {
		return false, nil
	}
	body, _, err := store.GetBytes(ctx, st, prefix+store.Fsck, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if body == nil {
		return false, nil
	}
	return true, out.Unmarshal(body)
}

// putFsckReport overwrites fsck.pb (§9.1: overwritten, never replayed).
func putFsckReport(ctx context.Context, st store.ObjectStore, prefix string, rep *proto.FsckReport) error {
	if st == nil {
		return nil
	}
	_, err := st.Put(ctx, prefix+store.Fsck, store.PutBody{Bytes: rep.Marshal()},
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/x-protobuf"})
	return err
}

// errReflinkUnsupported is returned by reflinkClone on filesystems without
// FICLONE (§6.2 step 1: "reflink when the FS supports it, else a plain copy").
var errReflinkUnsupported = errors.New("reflink unsupported")

// copyDir copies src → dst recursively; reflinking regular files when the FS
// supports FICLONE and falling back to a plain copy otherwise (§6.2 step 1:
// "Seconds on XFS; the serving pack set is never mutated").
func copyDir(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		return copyFile(target, path)
	})
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err == nil {
		if reflinkErr := reflinkClone(out, in); reflinkErr == nil {
			return out.Close()
		} else if !errors.Is(reflinkErr, errReflinkUnsupported) {
			out.Close()
			return reflinkErr
		}
		// Plain copy: truncate the pre-created file and stream.
		if err := out.Truncate(0); err != nil {
			out.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	// dst exists (resumed rebuild): leave the copy as-is.
	return nil
}

// statfsFree returns the available bytes under dir (§6.2 pre-flight: "statfs
// on cache.dir is the honest measure of can the scratch copy land here").
func statfsFree(dir string) (uint64, error) {
	return freeBytes(dir)
}

// hasObjectsDir reports whether dir is (or was) an initialized git repo with
// an objects dir — the §6.2 resume precondition.
func hasObjectsDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && info.IsDir()
}

// writeAtomic writes data via temp file + rename in the same directory
// (§6.2 step 2: "atomic write-temp + rename").
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readPackFile loads a pack side file from a pack dir, tolerating absence.
func readPackFile(packDir, checksum, ext string) ([]byte, bool, error) {
	b, err := os.ReadFile(filepath.Join(packDir, "pack-"+checksum+ext))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// checksumFromPackPath extracts "pack-<checksum>.pack" → <checksum>.
func checksumFromPackPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, "pack-")
	for _, ext := range []string{".pack", ".idx", ".rev", ".bitmap", ".keep"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// installSideFile links/copies one file into the serving copy's pack dir
// (§6.2 step 5: "Only at publish are the new files linked into the serving
// copy").
func installSideFile(packDir, srcPath string) error {
	dst := filepath.Join(packDir, filepath.Base(srcPath))
	if dst == srcPath {
		return nil
	}
	in, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return writeAtomic(dst, in)
}

// zeroOid returns the all-zero oid marker for the manifest's object format.
func zeroOid(m *proto.Manifest) string {
	f, err := git.ObjectFormatFrom(m.ObjectFormat)
	if err != nil {
		return ""
	}
	return f.ZeroHex()
}

// eqBytes is a tiny helper for tests and fsck parsing.
func eqBytes(a, b []byte) bool { return bytes.Equal(a, b) }
