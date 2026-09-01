package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Bundle primitives (04_git.md §10) and upstream-git helpers (§11).

// CreateBundle runs `git bundle create <out> --stdin` fed `<ref>` lines and
// `^<oid>` excludes (one per line, stdin closed after). Blocking, Pool.Run.
// Returns the size and the byte offset of the PACK magic (header/pack split
// for composition; magic may straddle a chunk boundary → 3-byte overlap).
func (l *Layer) CreateBundle(ctx context.Context, repo *LocalRepo, outPath string, refs []string, excludes []string) (size int64, packOffset int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	var b bytes.Buffer
	for _, r := range refs {
		b.WriteString(r + "\n")
	}
	for _, e := range excludes {
		b.WriteString("^" + e + "\n")
	}
	if _, err := l.runPooled(ctx, execSpec{
		argv:  []string{"bundle", "create", outPath, "--stdin"},
		dir:   repo.Path,
		stdin: &b,
	}); err != nil {
		return 0, 0, err
	}
	return scanPackOffset(outPath)
}

// scanPackOffset finds the PACK magic offset with a 3-byte overlap between
// chunks.
func scanPackOffset(path string) (int64, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	var overlap []byte
	var off int64
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			data := append(overlap, buf[:n]...)
			base := off - int64(len(overlap))
			if i := bytes.Index(data, []byte("PACK")); i >= 0 {
				return st.Size(), base + int64(i), nil
			}
			if len(data) >= 3 {
				overlap = append([]byte(nil), data[len(data)-3:]...)
			}
			off += int64(n)
		}
		if rerr != nil {
			break
		}
	}
	return st.Size(), -1, nil
}

// BundleHeader renders the v2 bundle header (no git, byte-for-byte tested):
// `# v2 git bundle\n` + `-<oid> \n` per prerequisite + `<oid> <name>\n` per
// ref + `\n`. Exact prerequisite bytes: `-` + oid + one space + `\n`.
func BundleHeader(refs []RefEntry, prereqs []Oid) []byte {
	var b bytes.Buffer
	b.WriteString("# v2 git bundle\n")
	for _, p := range prereqs {
		b.WriteString("-" + string(p) + " \n")
	}
	for _, r := range refs {
		b.WriteString(string(r.Oid) + " " + r.Name + "\n")
	}
	b.WriteString("\n")
	return b.Bytes()
}

// FullBundle composes header ∘ an existing pack's bytes (composed weeklies
// and import bundles with zero pack bytes through the host).
func FullBundle(header []byte, packBytes []byte) []byte {
	out := make([]byte, 0, len(header)+len(packBytes))
	out = append(out, header...)
	return append(out, packBytes...)
}

// --- upstream helpers (§11) --------------------------------------------------------------

// upstreamCredentialArgv is the inline config-pair credential helper, never on
// argv or the environment beyond the named env var (the empty helper first
// clears inherited helpers — argv order is significant).
func upstreamCredentialArgv() []string {
	return []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=!f(){ echo username=x-access-token; echo password=$WALGIT_UPSTREAM_TOKEN; };f",
	}
}

// upstreamBaseArgv composes the credential argv + repo config; extra -c pairs
// come first (fetch.negotiationAlgorithm etc.).
// UpstreamSpec is the upstream config (doc 12): source URL, LFS flag, the env
// var NAME holding the token (never the token), follow flag.
type UpstreamSpec struct {
	URL      string
	LFS      bool
	TokenEnv string // env var name; WALGIT_UPSTREAM_TOKEN set from it in the child env
	Follow   bool
}

func (l *Layer) upstreamEnv(u UpstreamSpec) []string {
	env := []string{}
	if u.TokenEnv != "" {
		if tok := os.Getenv(u.TokenEnv); tok != "" {
			env = append(env, "WALGIT_UPSTREAM_TOKEN="+tok)
		}
	}
	return env
}

// FetchObjectsAsPack (repair, §11.1): fetch missing oids from upstream in
// 500-oid batches into a scratch bare repo, then pack exactly the requested
// oids with `git pack-objects --no-reuse-delta --compression=6`; verify EVERY
// requested oid is present in the resulting idx (idx fanout + binary search —
// no subprocess); a refused want is an error, never a silent hole.
func (l *Layer) FetchObjectsAsPack(ctx context.Context, repo *LocalRepo, u UpstreamSpec, oids []Oid) (packPath string, err error) {
	ctx, cancel := context.WithTimeout(ctx, l.UpstreamTTL)
	defer cancel()
	scratch := filepath.Join(repo.Path, fmt.Sprintf("walgit-repair-%d-%d", os.Getpid(), nextSuffix()))
	defer os.RemoveAll(scratch)
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "pack"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(scratch, "refs"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "info"), 0o755); err != nil {
		return "", err
	}
	if err := writeHeadSeed(scratch); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"),
		[]byte(repo.ObjectsDir()+"\n"), 0o644); err != nil {
		return "", err
	}

	env := l.upstreamEnv(u)

	for start := 0; start < len(oids); start += 500 {
		end := start + 500
		if end > len(oids) {
			end = len(oids)
		}
		batch := oids[start:end]
		argv := append([]string{
			"-c", "fetch.negotiationAlgorithm=noop", "-c", "protocol.version=2",
		}, upstreamCredentialArgv()...)
		argv = append(argv, "fetch", "--no-tags", "--no-write-fetch-head", "--quiet", "--depth=1", u.URL)
		for _, o := range batch {
			argv = append(argv, string(o))
		}
		if _, err := l.runPooled(ctx, execSpec{
			argv: argv, dir: scratch, env: env, timeout: l.UpstreamTTL,
		}); err != nil {
			return "", err
		}
	}

	// pack-objects over exactly the requested oids (no closure).
	var wants bytes.Buffer
	for _, o := range oids {
		wants.WriteString(string(o) + "\n")
	}
	var packOut bytes.Buffer
	if _, err := l.runPooled(ctx, execSpec{
		argv:   []string{"pack-objects", "--no-reuse-delta", "--compression=6", "--stdout"},
		dir:    scratch,
		env:    env,
		stdin:  &wants,
		stdout: &packOut,
	}); err != nil {
		return "", err
	}
	// index it into the scratch and verify every requested oid via idx lookup.
	if _, err := l.runPooled(ctx, execSpec{
		argv:  []string{"index-pack", "--stdin", "--rev-index", "--threads=0"},
		dir:   scratch,
		env:   []string{"GIT_DIR=" + scratch},
		stdin: &packOut,
	}); err != nil {
		return "", err
	}
	idxs, _ := filepath.Glob(filepath.Join(scratch, "objects", "pack", "pack-*.idx"))
	if len(idxs) == 0 {
		return "", &PackRejectedError{Detail: "repair fetch produced no pack"}
	}
	idxPath := idxs[0]
	for _, o := range oids {
		if ok, err := idxContains(idxPath, string(o)); err != nil {
			return "", err
		} else if !ok {
			return "", missingObjects([]string{string(o)})
		}
	}
	dst := filepath.Join(repo.PackDir(), filepath.Base(idxPath[:len(idxPath)-4]))
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		src := idxPath[:len(idxPath)-4] + ext
		if _, err := os.Stat(src); err == nil {
			if err := copyFileTo(dst+ext, src); err != nil {
				return "", err
			}
		}
	}
	return dst + ".pack", nil
}

// idxContains looks an oid up in a pack idx via the fanout + binary search
// (preferred over `git verify-pack -v` — no subprocess).
func idxContains(idxPath, oid string) (bool, error) {
	data, err := os.ReadFile(idxPath)
	if err != nil {
		return false, err
	}
	n := len(oid) / 2 // first byte of the binary oid
	if n < 1 || n > 32 {
		return false, errInvalidInput("bad oid %q", oid)
	}
	if !bytes.Equal(data[:4], []byte{0xff, 't', 'O', 'c'}) {
		return false, &PackRejectedError{Detail: "bad idx magic"}
	}
	fanoutOff := 8
	firstByte := int(hexVal(oid[0]))<<4 | int(hexVal(oid[1]))
	hi := be32(data[fanoutOff+firstByte*4:])
	lo := uint32(0)
	if firstByte > 0 {
		lo = be32(data[fanoutOff+(firstByte-1)*4:])
	}
	entryLen := 20
	if len(oid) == 64 {
		entryLen = 32
	}
	namesOff := fanoutOff + 256*4
	for i := lo; i < hi; i++ {
		off := namesOff + int(i)*entryLen
		if off+entryLen > len(data) {
			return false, &PackRejectedError{Detail: "idx truncated"}
		}
		if string(hexEncode(data[off:off+n])) == oid {
			return true, nil
		}
	}
	return false, nil
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return 0
	}
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func hexEncode(b []byte) []byte {
	out := make([]byte, len(b)*2)
	const hexd = "0123456789abcdef"
	for i, c := range b {
		out[i*2] = hexd[c>>4]
		out[i*2+1] = hexd[c&0xf]
	}
	return out
}

func copyFileTo(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// FollowFetch (§11.2): the persistent follow scratch
// <cache.dir>/follow/<owner>/<name>.git with alternates → the serving objects
// dir; refs/follow/<ref> set via update-ref --stdin; then
//
//	git -c fetch.unpackLimit=1 -c transfer.unpackLimit=1 -c fetch.writeCommitGraph=false \
//	  -c gc.auto=0 -c protocol.version=2 fetch <upstream> +<ref>:refs/follow/<ref>…
//
// Tips read back via `git for-each-ref refs/follow/
// --format=%(objectname) %(refname)`. The fetched pack is discarded (the
// scratch's packs are trash — alternates make objects resolve anyway).
func (l *Layer) FollowFetch(ctx context.Context, root string, id RepoId, u UpstreamSpec, refs []string) (map[string]Oid, error) {
	ctx, cancel := context.WithTimeout(ctx, l.UpstreamTTL)
	defer cancel()
	followDir := filepath.Join(root, "follow", id.Owner, id.Name+".git")
	if _, err := os.Stat(followDir); os.IsNotExist(err) {
		if _, err := InitLocalRepo(filepath.Join(root, "follow"), id, Sha1); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(followDir, "objects", "info"), 0o755); err != nil {
			return nil, err
		}
	}
	servingRepo, err := OpenLocalRepo(root, id)
	if err != nil {
		return nil, err
	}
	if servingRepo == nil {
		return nil, errInvalidInput("follow: serving repo %s absent", id)
	}
	if err := os.WriteFile(filepath.Join(followDir, "objects", "info", "alternates"),
		[]byte(servingRepo.ObjectsDir()+"\n"), 0o644); err != nil {
		return nil, err
	}

	// Set refs/follow/<ref> to the WAL values via the §4.3 grammar.
	var stdinBuf bytes.Buffer
	stdinBuf.WriteString("start\n")
	for _, r := range refs {
		fmt.Fprintf(&stdinBuf, "delete refs/follow/%s\n", r)
	}
	stdinBuf.WriteString("prepare\ncommit\n")
	if _, _, err := l.runCollect(ctx, execSpec{
		argv: []string{"update-ref", "--stdin"}, dir: followDir, stdin: &stdinBuf,
	}); err != nil {
		return nil, err
	}
	env := l.upstreamEnv(u)
	argv := append([]string{
		"-c", "fetch.unpackLimit=1", "-c", "transfer.unpackLimit=1",
		"-c", "fetch.writeCommitGraph=false", "-c", "gc.auto=0", "-c", "protocol.version=2",
	}, upstreamCredentialArgv()...)
	argv = append(argv, "fetch", u.URL)
	for _, r := range refs {
		argv = append(argv, "+"+r+":refs/follow/"+r)
	}
	if _, err := l.runPooled(ctx, execSpec{
		argv: argv, dir: followDir, env: env, timeout: l.UpstreamTTL,
	}); err != nil {
		return nil, err
	}

	out, _, err := l.runCollect(ctx, execSpec{
		argv:    []string{"for-each-ref", "refs/follow/", "--format=%(objectname) %(refname)"},
		dir:     followDir,
		timeout: 60 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	tips := map[string]Oid{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		oid, name, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if ref, ok2 := strings.CutPrefix(name, "refs/follow/"); ok2 {
			tips[ref] = Oid(oid)
		}
	}
	// The fetched pack is discarded: the scratch's packs are trash.
	matches, _ := filepath.Glob(filepath.Join(followDir, "objects", "pack", "pack-*"))
	for _, m := range matches {
		os.Remove(m)
	}
	return tips, nil
}
