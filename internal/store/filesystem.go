// filesystem.go: the filesystem backend (03_store_backends.md §4, divergence
// D4). A pure-stdlib backend: keys map VERBATIM to files under the root; no
// HTTP clients, signing, or credentials.
//
// Mechanics per §4: guarded verbatim key→path mapping; "<size>:<mtime_ns>"
// version tokens (equality-only, also the ETag); conditionals through a
// sidecar ".lock" flock + stat-compare + same-directory temp + atomic rename
// (fsync the temp first); renameat2(RENAME_NOREPLACE) for Create on Linux
// (stdlib syscall, no cgo) with a portable lock+stat+rename fallback on other
// GOOS and under a test hook; compose = stream-concat + conditional rename;
// range reads via ReadAt; List/ListPrefixes walk in S3 byte order; no signed
// or accel URLs (the HTTP layer proxies bytes).
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"git.packden.us/crueber/walhub/internal/config"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// forcePortableRename forces the portable Create fallback in tests (the
// renameat2 path is otherwise what CI proves, 15_testing.md §2).
var forcePortableRename atomic.Bool

// Filesystem is the object-store-over-a-directory-tree backend.
type Filesystem struct {
	root string // symlink-resolved absolute root
	sem  *Weighted
}

// fsBulkConcurrency is store.fs.bulk_concurrency (§4, default 32).
const fsBulkConcurrency = 32

// NewFilesystem builds a filesystem backend rooted at cfg.Store.Root.
func NewFilesystem(cfg *config.Store) (*Filesystem, error) {
	return NewFilesystemRoot(cfg.Root, fsBulkConcurrency)
}

// NewFilesystemRoot builds the backend over an explicit root directory.
func NewFilesystemRoot(root string, bulkConcurrency int) (*Filesystem, error) {
	if root == "" {
		return nil, NewInvalid("", fmt.Errorf("filesystem backend: root required"))
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, NewOther("", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, NewOther("", err)
	}
	if bulkConcurrency < 1 {
		bulkConcurrency = 1
	}
	return &Filesystem{root: resolved, sem: NewWeighted(int64(bulkConcurrency))}, nil
}

func (f *Filesystem) Backend() string { return "filesystem" }

// ---- key guard (§4: every entry point, before I/O → InvalidArgument) ----

// checkKey enforces the key grammar: non-empty, relative, no empty/"."/".."
// segments, no trailing slash, never a ".lock" sidecar or a ".tmp-" temp, and
// (after Clean) confined under the root.
func (f *Filesystem) checkKey(key string) error {
	bad := func(why string) error { return NewInvalid(key, fmt.Errorf("bad key: %s", why)) }
	if key == "" {
		return bad("empty")
	}
	if strings.HasPrefix(key, "/") || filepath.IsAbs(key) {
		return bad("absolute")
	}
	if strings.HasSuffix(key, "/") {
		return bad("trailing slash")
	}
	if strings.HasSuffix(key, ".lock") {
		return bad(".lock sidecar namespace is reserved")
	}
	if strings.Contains(key, ".tmp-") {
		return bad(".tmp- temp namespace is reserved")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" {
			return bad("empty segment")
		}
		if seg == "." || seg == ".." {
			return bad("dot segment")
		}
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return bad("escapes root")
	}
	return nil
}

// path maps a guarded key verbatim under the root.
func (f *Filesystem) path(key string) string {
	return filepath.Join(f.root, filepath.FromSlash(key))
}

// resolveForWrite maps a guarded key to its path, creating the parent
// directory and re-checking via EvalSymlinks that no directory component is a
// symlink (symlinked directory components rejected, §4).
func (f *Filesystem) resolveForWrite(key string) (string, error) {
	p := f.path(key)
	parent := filepath.Dir(p)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", f.mapErr(key, err)
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", f.mapErr(key, err)
	}
	if resolved != parent {
		return "", NewInvalid(key, fmt.Errorf("symlinked directory component under %s", parent))
	}
	return p, nil
}

// resolveForRead maps a guarded key for reading, rejecting symlinked
// directory components when the object exists.
func (f *Filesystem) resolveForRead(key string) (string, error) {
	p := f.path(key)
	if _, err := os.Lstat(p); err != nil {
		// Absent: no symlink check needed; the caller turns this into NotFound.
		return p, nil
	}
	parent := filepath.Dir(p)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", f.mapErr(key, err)
	}
	if resolved != parent {
		return "", NewInvalid(key, fmt.Errorf("symlinked directory component under %s", parent))
	}
	return p, nil
}

// ---- version tokens: "<size>:<mtime_ns>" (decimal, from lstat) ----

func tokenFromStat(st fs.FileInfo) Version {
	return Version(fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano()))
}

// statToken returns the version token for an existing path.
func statToken(p string) (Version, fs.FileInfo, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return "", nil, err
	}
	return tokenFromStat(st), st, nil
}

// ---- conditional machinery: flock → stat → atomic rename ----

// acquireLock takes an exclusive flock on the sidecar "<path>.lock"
// (O_CREATE|O_RDWR). Dead holders release automatically; waiters block.
// The sidecar file itself persists (removing it races with flock waiters).
func acquireLock(p string) (*os.File, error) {
	lf, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := flockExclusive(lf); err != nil {
		lf.Close()
		return nil, err
	}
	return lf, nil
}

// writeTemp materializes body into a same-directory temp file, fsyncs it, and
// returns (tmpPath, size, token).
func writeTemp(p string, body PutBody) (string, int64, Version, error) {
	tmp := p + ".tmp-" + randHex(6)
	// O_EXCL: two writers must never share a temp.
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, "", err
	}
	ok := false
	defer func() {
		if !ok {
			tf.Close()
			os.Remove(tmp)
		}
	}()
	switch {
	case body.Bytes != nil:
		_, err = tf.Write(body.Bytes)
	case body.Stream != nil:
		err = copyExact(tf, body.Stream, body.StreamLen)
	case body.File != "":
		var src *os.File
		src, err = os.Open(body.File)
		if err == nil {
			_, err = io.Copy(tf, src)
			src.Close()
		}
	default:
		err = fmt.Errorf("empty put body")
	}
	if err != nil {
		return "", 0, "", err
	}
	if err := tf.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := tf.Close(); err != nil {
		return "", 0, "", err
	}
	ok = true
	st, err := os.Lstat(tmp)
	if err != nil {
		return "", 0, "", err
	}
	return tmp, st.Size(), tokenFromStat(st), nil
}

// copyExact copies exactly n bytes from r to w; a short source is an error.
func copyExact(w io.Writer, r io.Reader, n int64) error {
	if n < 0 {
		return fmt.Errorf("negative stream length")
	}
	got, err := io.Copy(w, io.LimitReader(r, n))
	if err != nil {
		return err
	}
	if got != n {
		return fmt.Errorf("stream body: got %d bytes, promised %d", got, n)
	}
	return nil
}

// bumpTokenIfCollision advances the temp's mtime when a CAS write would mint
// the token it replaces (same size, coarse-mtime fs): writes strictly advance
// version tokens (§4 collision guard).
func bumpTokenIfCollision(tmp string, newTok, oldTok Version) (Version, error) {
	if oldTok == "" || newTok != oldTok {
		return newTok, nil
	}
	// Coarse mtime: nudge in growing steps until the token differs.
	for step := time.Duration(1); ; step *= 2 {
		t := time.Now().Add(step * time.Nanosecond)
		if err := os.Chtimes(tmp, t, t); err != nil {
			return newTok, err
		}
		st, err := os.Lstat(tmp)
		if err != nil {
			return newTok, err
		}
		if tok := tokenFromStat(st); tok != oldTok {
			return tok, nil
		}
		if step > time.Second {
			// Give up nudging (pathological fs); the rename still proceeds.
			return newTok, nil
		}
	}
}

// needsDirSync reports whether the parent directory must be fsync'd after a
// rename for durability: lease/manifest keys; bulk skips it (crash ⇒ re-push;
// the WAL tolerates).
func needsDirSync(key string) bool {
	if strings.HasPrefix(key, "leases/") {
		return true
	}
	if strings.HasSuffix(key, "manifest.pb") {
		return true
	}
	return false
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// finalizePut installs tmp (fsynced, token tmpTok) at dstPath honoring
// opts.Mode. The tmp file is consumed (removed on failure paths).
func (f *Filesystem) finalizePut(key, dstPath, tmp string, tmpTok Version, opts PutOptions) (ObjectMeta, error) {
	switch opts.Mode {
	case PutCreate:
		if forcePortableRename.Load() {
			// Portable fallback: lock → stat present → PreconditionFailed → rename.
			if err := f.portableCreate(key, dstPath, tmp); err != nil {
				os.Remove(tmp)
				return ObjectMeta{}, err
			}
		} else {
			err := renameNoReplace(tmp, dstPath)
			if err == nil {
				break // installed
			}
			if errors.Is(err, syscall.EEXIST) {
				os.Remove(tmp)
				cur := ""
				if tok, _, serr := statToken(dstPath); serr == nil {
					cur = string(tok)
				}
				return ObjectMeta{}, NewPrecondition(key, Version(cur))
			}
			if !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EINVAL) {
				os.Remove(tmp)
				return ObjectMeta{}, f.mapErr(key, err)
			}
			// renameat2 unavailable on this kernel/GOOS → portable fallback.
			if err := f.portableCreate(key, dstPath, tmp); err != nil {
				os.Remove(tmp)
				return ObjectMeta{}, err
			}
		}

	case PutUpdate:
		lf, err := acquireLock(dstPath)
		if err != nil {
			os.Remove(tmp)
			return ObjectMeta{}, f.mapErr(key, err)
		}
		defer lf.Close()
		tok, _, serr := statToken(dstPath)
		if serr != nil {
			os.Remove(tmp)
			if errors.Is(serr, syscall.ENOENT) {
				return ObjectMeta{}, NewPrecondition(key, "")
			}
			return ObjectMeta{}, f.mapErr(key, serr)
		}
		if opts.IfVersion == "" || tok != opts.IfVersion {
			os.Remove(tmp)
			return ObjectMeta{}, NewPrecondition(key, tok)
		}
		if bumped, err := bumpTokenIfCollision(tmp, tmpTok, tok); err == nil {
			tmpTok = bumped
		}
		if err := os.Rename(tmp, dstPath); err != nil {
			os.Remove(tmp)
			return ObjectMeta{}, f.mapErr(key, err)
		}

	default: // PutOverwrite
		if tok, _, serr := statToken(dstPath); serr == nil {
			// Overwrite may also re-mint the token it replaces; bump then too.
			if bumped, err := bumpTokenIfCollision(tmp, tmpTok, tok); err == nil {
				tmpTok = bumped
			}
		}
		// A symlink AT the object path is replaced, never traversed: rename
		// replaces the link itself (POSIX), so no unlink is needed.
		if err := os.Rename(tmp, dstPath); err != nil {
			os.Remove(tmp)
			return ObjectMeta{}, f.mapErr(key, err)
		}
	}

	if needsDirSync(key) {
		_ = fsyncDir(filepath.Dir(dstPath)) // best effort
	}
	return ObjectMeta{Key: key, Size: sizeOfToken(tmpTok), Version: tmpTok}, nil
}

// sizeOfToken recovers the size half of a "<size>:<mtime_ns>" token.
func sizeOfToken(tok Version) int64 {
	s := string(tok)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		var n int64
		fmt.Sscanf(s[:i], "%d", &n)
		return n
	}
	return 0
}

// portableCreate implements Create without renameat2: lock → stat present →
// PreconditionFailed → rename. Same window as Update (§4).
func (f *Filesystem) portableCreate(key, dstPath, tmp string) error {
	lf, err := acquireLock(dstPath)
	if err != nil {
		return f.mapErr(key, err)
	}
	defer lf.Close()
	if _, _, serr := statToken(dstPath); serr == nil {
		tok, _, _ := statToken(dstPath)
		return NewPrecondition(key, tok)
	} else if !errors.Is(serr, syscall.ENOENT) {
		return f.mapErr(key, serr)
	}
	return os.Rename(tmp, dstPath)
}

// mapErr maps an OS error to the §4 error table.
func (f *Filesystem) mapErr(key string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ENOENT):
		return NewNotFound(key)
	case errors.Is(err, syscall.EEXIST):
		return NewPrecondition(key, "")
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.ENFILE), errors.Is(err, syscall.EMFILE):
		return NewRetryable(key, err)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EACCES),
		errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EROFS), errors.Is(err, syscall.EXDEV):
		return NewOther(key, err)
	default:
		return NewOther(key, err)
	}
}

// ---- interface ----

func (f *Filesystem) permit(ctx context.Context, key string, ranged bool) (func(), error) {
	if !IsBulkKey(key) && !ranged {
		return func() {}, nil // control-plane ops bypass the semaphore
	}
	_, release, err := AcquireBulk(ctx, f.sem, key)
	return release, err
}

func (f *Filesystem) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	if err := f.checkKey(key); err != nil {
		return nil, err
	}
	release, err := f.permit(ctx, key, opts.Range != nil)
	if err != nil {
		return nil, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	defer release()

	p, err := f.resolveForRead(key)
	if err != nil {
		return nil, err
	}
	tok, st, serr := statToken(p)
	if serr != nil {
		return nil, f.mapErr(key, serr) // ENOENT → NotFound
	}
	if opts.IfNoneMatch != "" && opts.IfNoneMatch == tok {
		return NotModified{Version: tok}, nil
	}
	if opts.IfMatch != "" && opts.IfMatch != tok {
		return nil, NewPrecondition(key, tok)
	}
	size := st.Size()
	start, end := int64(0), size
	if opts.Range != nil {
		start, end = opts.Range[0], opts.Range[1]
		if start < 0 || end < start {
			return nil, NewInvalid(key, fmt.Errorf("bad range [%d,%d)", start, end))
		}
		if end > size {
			end = size // clamp (no error)
		}
		if start > size {
			// 416 analog: range entirely past EOF. start == size reads the
			// empty suffix (the contract's empty-body case).
			return nil, NewPrecondition(key, tok)
		}
	}
	rf, err := os.Open(p)
	if err != nil {
		return nil, f.mapErr(key, err)
	}
	defer rf.Close()
	if opts.Range != nil {
		buf := make([]byte, end-start)
		if len(buf) > 0 {
			if _, err := rf.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
				return nil, f.mapErr(key, err)
			}
		}
		return Object{
			Meta: ObjectMeta{Key: key, Size: size, Version: tok},
			Body: io.NopCloser(bytes.NewReader(buf)),
		}, nil
	}
	// Full read: ReadAll (stateless; conditionals used the sizing stat).
	data, err := io.ReadAll(rf)
	if err != nil {
		return nil, f.mapErr(key, err)
	}
	return Object{
		Meta: ObjectMeta{Key: key, Size: size, Version: tok},
		Body: io.NopCloser(bytes.NewReader(data)),
	}, nil
}

func (f *Filesystem) Head(ctx context.Context, key string) (*ObjectMeta, error) {
	if err := f.checkKey(key); err != nil {
		return nil, err
	}
	p, err := f.resolveForRead(key)
	if err != nil {
		return nil, err
	}
	tok, st, serr := statToken(p)
	if serr != nil {
		if errors.Is(serr, syscall.ENOENT) {
			return nil, nil
		}
		return nil, f.mapErr(key, serr)
	}
	return &ObjectMeta{Key: key, Size: st.Size(), Version: tok}, nil
}

func (f *Filesystem) Put(ctx context.Context, key string, body PutBody, opts PutOptions) (ObjectMeta, error) {
	if err := f.checkKey(key); err != nil {
		return ObjectMeta{}, err
	}
	release, err := f.permit(ctx, key, false)
	if err != nil {
		return ObjectMeta{}, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	defer release()

	dst, err := f.resolveForWrite(key)
	if err != nil {
		return ObjectMeta{}, err
	}
	tmp, _, tok, err := writeTemp(dst, body)
	if err != nil {
		return ObjectMeta{}, f.mapErr(key, err)
	}
	return f.finalizePut(key, dst, tmp, tok, opts)
}

func (f *Filesystem) Delete(ctx context.Context, key string, ifVersion Version) error {
	if err := f.checkKey(key); err != nil {
		return err
	}
	release, err := f.permit(ctx, key, false)
	if err != nil {
		return &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	defer release()

	p, err := f.resolveForRead(key)
	if err != nil {
		return err
	}
	if ifVersion == "" {
		// Unconditional: absent → Ok (idempotent).
		if err := os.Remove(p); err != nil {
			if errors.Is(err, syscall.ENOENT) {
				return nil
			}
			return f.mapErr(key, err)
		}
		f.pruneParents(p)
		return nil
	}
	// CAS delete: lock → stat → remove.
	lf, err := acquireLock(p)
	if err != nil {
		return f.mapErr(key, err)
	}
	defer lf.Close()
	tok, _, serr := statToken(p)
	if serr != nil {
		if errors.Is(serr, syscall.ENOENT) {
			return NewNotFound(key)
		}
		return f.mapErr(key, serr)
	}
	if tok != ifVersion {
		return NewPrecondition(key, tok)
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return NewNotFound(key)
		}
		return f.mapErr(key, err)
	}
	f.pruneParents(p)
	return nil
}

// pruneParents removes now-empty parent directories up to (not including)
// the root; best effort.
func (f *Filesystem) pruneParents(p string) {
	dir := filepath.Dir(p)
	for dir != f.root && strings.HasPrefix(dir, f.root+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return // not empty (or raced); stop
		}
		dir = filepath.Dir(dir)
	}
}

func (f *Filesystem) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return nil, nil // no file:// URLs; the HTTP layer proxies bytes (§4)
}

func (f *Filesystem) AccelTarget(ctx context.Context, key string) (*AccelTarget, error) {
	return nil, nil
}

func (f *Filesystem) SupportsCompose() bool { return true }
func (f *Filesystem) ComposeIsNative() bool { return true }

// Compose stream-concats the sources in order into the dest temp (4 MiB
// buffer), then applies the dest PutMode like any Put. Sources stay.
func (f *Filesystem) Compose(ctx context.Context, dst string, sources []string, opts PutOptions) (ObjectMeta, error) {
	if len(sources) < 1 || len(sources) > 32 {
		return ObjectMeta{}, NewInvalid(dst, fmt.Errorf("compose needs 1..=32 sources, got %d", len(sources)))
	}
	for _, src := range sources {
		if err := f.checkKey(src); err != nil {
			return ObjectMeta{}, err
		}
	}
	if err := f.checkKey(dst); err != nil {
		return ObjectMeta{}, err
	}
	release, err := f.permit(ctx, dst, false)
	if err != nil {
		return ObjectMeta{}, &StoreError{Kind: ErrKindRetryable, Key: dst, Err: err}
	}
	defer release()

	dstPath, err := f.resolveForWrite(dst)
	if err != nil {
		return ObjectMeta{}, err
	}
	tmp := dstPath + ".tmp-" + randHex(6)
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return ObjectMeta{}, f.mapErr(dst, err)
	}
	ok := false
	defer func() {
		if !ok {
			tf.Close()
			os.Remove(tmp)
		}
	}()
	buf := make([]byte, 4<<20)
	for _, src := range sources {
		sp, err := f.resolveForRead(src)
		if err != nil {
			return ObjectMeta{}, err
		}
		sf, err := os.Open(sp)
		if err != nil {
			return ObjectMeta{}, f.mapErr(src, err)
		}
		_, cerr := io.CopyBuffer(tf, sf, buf)
		sf.Close()
		if cerr != nil {
			return ObjectMeta{}, f.mapErr(dst, cerr)
		}
	}
	if err := tf.Sync(); err != nil {
		return ObjectMeta{}, f.mapErr(dst, err)
	}
	if err := tf.Close(); err != nil {
		return ObjectMeta{}, f.mapErr(dst, err)
	}
	ok = true
	st, err := os.Lstat(tmp)
	if err != nil {
		return ObjectMeta{}, f.mapErr(dst, err)
	}
	return f.finalizePut(dst, dstPath, tmp, tokenFromStat(st), opts)
}

// ---- listing: S3 byte order over key strings ----

// listEntry is one walk node ordered by its key-space position: files by
// name, directories by name+"/" — so "a-x" < "a/b" lands the file before the
// subtree, exactly like the S3 byte-order listing (§4).
type listEntry struct {
	sortKey string
	name    string
	isDir   bool
}

// sidecar reports reserved-namespace files invisible to Head/List (§4).
func sidecar(name string) bool {
	return strings.HasSuffix(name, ".lock") || strings.Contains(name, ".tmp-")
}

func (f *Filesystem) List(ctx context.Context, prefix, startAfter string, fn func(ObjectMeta) error) error {
	release, err := f.permit(ctx, prefix, false)
	if err != nil {
		return &StoreError{Kind: ErrKindRetryable, Key: prefix, Err: err}
	}
	defer release()
	return f.walk("", prefix, startAfter, fn)
}

func (f *Filesystem) walk(rel, prefix, startAfter string, fn func(ObjectMeta) error) error {
	dir := f.path(rel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil // no keys under this prefix
		}
		return f.mapErr(prefix, err)
	}
	ents := make([]listEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			// Resolve through symlinks: a symlinked dir is rejected on
			// writes; for listing, treat it by what it points at.
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			e = fs.FileInfoToDirEntry(info)
		}
		isDir := e.IsDir()
		key := e.Name()
		if isDir {
			key += "/"
		}
		if isDir && sidecar(e.Name()) {
			continue
		}
		ents = append(ents, listEntry{sortKey: key, name: e.Name(), isDir: isDir})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].sortKey < ents[j].sortKey })

	for _, ent := range ents {
		childRel := rel + ent.name
		if ent.isDir {
			sub := childRel + "/"
			// Prune subtrees that cannot contain the prefix.
			if !strings.HasPrefix(sub, prefix) && !strings.HasPrefix(prefix, sub) {
				continue
			}
			if err := f.walk(childRel+"/", prefix, startAfter, fn); err != nil {
				return err
			}
			continue
		}
		if sidecar(ent.name) {
			continue
		}
		key := childRel
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		if startAfter != "" && key <= startAfter {
			continue
		}
		tok, st, serr := statToken(filepath.Join(dir, ent.name))
		if serr != nil {
			continue // raced away mid-walk
		}
		if err := fn(ObjectMeta{Key: key, Size: st.Size(), Version: tok}); err != nil {
			return err
		}
	}
	return nil
}

func (f *Filesystem) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	release, err := f.permit(ctx, prefix, false)
	if err != nil {
		return &StoreError{Kind: ErrKindRetryable, Key: prefix, Err: err}
	}
	defer release()

	dir := f.path(prefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return f.mapErr(prefix, err)
	}
	// ReadDir is filename-sorted (byte order); subdirectories are the
	// delimiter prefixes. Files directly under the prefix are objects, not
	// prefixes; a file deeper down (name containing no slash) cannot
	// contribute a common prefix.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := fn(prefix + e.Name() + "/"); err != nil {
			return err
		}
	}
	return nil
}
