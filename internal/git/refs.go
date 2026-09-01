package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	exec "os/exec"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Refs (04_git.md §4): reading, snapshot cache, apply_ref_txn.

// ValidateRefName enforces the §4.3 name rules: HEAD always OK; everything
// else MUST start with refs/; forbidden bytes space \n \r ~ ^ : ? * [ \`; no
// `..`; no `@{`; no `//`; no leading/trailing `/`; no trailing `.`; no
// trailing `.lock`.
func ValidateRefName(name string) error {
	if name == "HEAD" {
		return nil
	}
	bad := func() error { return errInvalidInput("invalid ref name %q", name) }
	if !strings.HasPrefix(name, "refs/") {
		return bad()
	}
	for i := range name {
		switch c := name[i]; c {
		case ' ', '\n', '\r', '~', '^', ':', '?', '*', '[', '\\', 0x7f:
			return bad()
		}
		if c := name[i]; c < 0x20 {
			return bad()
		}
	}
	switch {
	case strings.Contains(name, ".."),
		strings.Contains(name, "@{"),
		strings.Contains(name, "//"),
		strings.HasPrefix(name, "/"),
		strings.HasSuffix(name, "/"),
		strings.HasSuffix(name, "."),
		strings.HasSuffix(name, ".lock"):
		return bad()
	}
	return nil
}

// RefEntry is one ref in a snapshot (04_git.md §4.2).
type RefUpdate = proto.RefUpdate

type RefEntry struct {
	Name   string // "refs/heads/main"
	Oid    Oid
	Peeled Oid // "" unless an annotated tag was peeled
}

// RefFingerprint keys cache validity: (Gen, packed-refs len + mtime, HEAD mtime).
type RefFingerprint struct {
	Gen         uint64
	PackedLen   int64
	PackedMtime int64 // UnixNano
	HeadMtime   int64 // UnixNano
	RefsMtime   int64 // UnixNano; newest mtime under refs/
}

// RefSnapshot is the parsed ref state (name-sorted).
type RefSnapshot struct {
	Gen        uint64
	Refs       []RefEntry
	HeadTarget string // "refs/heads/main" or ""
	HeadOid    Oid    // resolved HEAD, "" if unborn
	FP         RefFingerprint
}

// Get binary-searches by name.
func (s *RefSnapshot) Get(name string) (RefEntry, bool) {
	i := sort.Search(len(s.Refs), func(i int) bool { return s.Refs[i].Name >= name })
	if i < len(s.Refs) && s.Refs[i].Name == name {
		return s.Refs[i], true
	}
	return RefEntry{}, false
}

// RefCache is the ref view cache (04_git.md §4.2): last full parse + an
// in-flight pending overlay + the tag peel cache.
type RefCache struct {
	mu      sync.RWMutex
	base    *RefSnapshot
	pending map[string]Oid // name→oid; "" (nil value semantics) = deleted
	peel    sync.Map       // oid → peeled oid (LRU-capped by peelLRU)
	peelMu  sync.Mutex
	peelLRU []string
}

func NewRefCache() *RefCache { return &RefCache{} }

// fingerprint stats the files whose change invalidates the base parse: the
// §4.2 tuple (Gen, packed-refs len + mtime, HEAD mtime) plus the newest mtime
// under refs/ — loose refs can change without touching either file, and the
// read path (loose overrides packed) must see them.
func (r *LocalRepo) fingerprint(gen uint64) RefFingerprint {
	fp := RefFingerprint{Gen: gen}
	if st, err := os.Stat(filepath.Join(r.Path, "packed-refs")); err == nil {
		fp.PackedLen, fp.PackedMtime = st.Size(), st.ModTime().UnixNano()
	}
	if st, err := os.Stat(filepath.Join(r.Path, "HEAD")); err == nil {
		fp.HeadMtime = st.ModTime().UnixNano()
	}
	filepath.WalkDir(filepath.Join(r.Path, "refs"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if st, err := d.Info(); err == nil && st.ModTime().UnixNano() > fp.RefsMtime {
				fp.RefsMtime = st.ModTime().UnixNano()
			}
		}
		return nil
	})
	return fp
}

// fingerprintCurrent stats without a generation (for validity comparison).
func fingerprintOf(repo *LocalRepo) RefFingerprint { return repo.fingerprint(0) }

// Snapshot loads the full ref snapshot: HEAD, packed-refs (incl ^peeled), the
// loose refs/ walk (loose overrides packed), name-sorted. Returns the cached
// base when the fingerprint matches (no re-parse).
func (l *Layer) Snapshot(repo *LocalRepo) (*RefSnapshot, error) {
	return l.SnapshotFrom(cacheFor(repo), repo)
}

func cacheFor(repo *LocalRepo) *RefCache { return repo.cache() }

func (r *LocalRepo) cache() *RefCache {
	if r == nil {
		return NewRefCache()
	}
	if r.cacheRef == nil {
		r.cacheRef = NewRefCache()
	}
	return r.cacheRef
}

// SnapshotFrom parses (or reuses) the snapshot through the given cache.
func (l *Layer) SnapshotFrom(c *RefCache, repo *LocalRepo) (*RefSnapshot, error) {
	fp := fingerprintOf(repo)
	c.mu.RLock()
	base := c.base
	c.mu.RUnlock()
	if base != nil && base.FP.PackedLen == fp.PackedLen &&
		base.FP.PackedMtime == fp.PackedMtime &&
		base.FP.HeadMtime == fp.HeadMtime &&
		base.FP.RefsMtime == fp.RefsMtime {
		return base, nil
	}

	headTarget, headOid := readHead(repo)
	snap := &RefSnapshot{HeadTarget: headTarget, HeadOid: headOid}

	// packed-refs
	data, err := os.ReadFile(filepath.Join(repo.Path, "packed-refs"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	snap.Refs = parsePackedRefs(data)

	// loose refs override packed
	loose, err := readLooseRefs(repo.Path)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]RefEntry, len(snap.Refs)+len(loose))
	for _, e := range snap.Refs {
		byName[e.Name] = e
	}
	// symref resolution: loose `ref: <t>` resolves only when its target is
	// already known (packed or previously read); otherwise record unresolved.
	known := func(name string) (RefEntry, bool) { e, ok := byName[name]; return e, ok }
	var unresolved []RefEntry
	for _, e := range loose {
		if t, ok := strings.CutPrefix(e.Oid, "ref: "); ok {
			if tgt, ok := known(strings.TrimSpace(t)); ok {
				e.Oid = tgt.Oid
			} else {
				unresolved = append(unresolved, e)
				continue
			}
		}
		byName[e.Name] = e
	}
	snap.Refs = snap.Refs[:0]
	for _, e := range byName {
		snap.Refs = append(snap.Refs, e)
	}
	sort.Slice(snap.Refs, func(i, j int) bool { return snap.Refs[i].Name < snap.Refs[j].Name })

	// HEAD resolution: a symbolic HEAD resolves to its target's oid when the
	// target is in the snapshot; otherwise HEAD is unborn ("").
	if snap.HeadTarget != "" && snap.HeadOid == "" {
		if e, ok := snap.Get(snap.HeadTarget); ok {
			snap.HeadOid = e.Oid
		}
	}
	_ = unresolved // recorded-but-skipped per spec; kept for future resolution
	fp.Gen = baseGen(base)
	snap.FP = fp
	c.mu.Lock()
	c.base = snap
	c.mu.Unlock()
	return snap, nil
}

func baseGen(base *RefSnapshot) uint64 {
	if base == nil {
		return 0
	}
	return base.Gen
}

// parsePackedRefs parses `<oid> <name>` lines with `^<peeled>` continuations;
// the `# pack-refs …` header is skipped.
func parsePackedRefs(data []byte) []RefEntry {
	var refs []RefEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "^") {
			if len(refs) > 0 {
				refs[len(refs)-1].Peeled = Oid(line[1:])
			}
			continue
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok || !ValidOid(oid) {
			continue
		}
		refs = append(refs, RefEntry{Name: name, Oid: Oid(oid)})
	}
	return refs
}

// readLooseRefs walks refs/ recursively, skipping *.lock; a loose file starting
// `ref: ` is a symref recorded as "ref: <target>" for later resolution.
func readLooseRefs(repoPath string) ([]RefEntry, error) {
	root := filepath.Join(repoPath, "refs")
	var out []RefEntry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".lock") {
			return nil
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s := strings.TrimSpace(string(data))
		if strings.HasPrefix(s, "ref: ") {
			out = append(out, RefEntry{Name: "refs/" + name, Oid: s})
			return nil
		}
		if ValidOid(s) && !isZeroOid(s) {
			out = append(out, RefEntry{Name: "refs/" + name, Oid: Oid(s)})
		}
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}

// Patch folds a committed txn into the pending overlay (04_git.md §4.2: doc 05
// calls Patch after its CAS succeeds; O(n) patch, no re-parse).
func (c *RefCache) Patch(txn []RefUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == nil {
		return
	}
	if c.pending == nil {
		c.pending = map[string]Oid{}
	}
	for _, u := range txn {
		if u.Name == "HEAD" {
			continue
		}
		if u.NewOid == "" || isZeroOid(u.NewOid) {
			c.pending[u.Name] = ""
		} else {
			c.pending[u.Name] = u.NewOid
		}
	}
	c.applyPendingLocked()
}

// applyPendingLocked produces a NEW sorted slice with the overlay folded in
// (copy-on-write; base is never mutated) and swaps the pointer.
func (c *RefCache) applyPendingLocked() {
	if len(c.pending) == 0 || c.base == nil {
		return
	}
	next := make([]RefEntry, 0, len(c.base.Refs)+len(c.pending))
	applied := map[string]bool{}
	for _, e := range c.base.Refs {
		if oid, ok := c.pending[e.Name]; ok {
			applied[e.Name] = true
			if isZeroOid(string(oid)) || oid == "" {
				continue // deleted
			}
			e.Oid = oid
		}
		next = append(next, e)
	}
	for name, oid := range c.pending {
		if applied[name] || isZeroOid(string(oid)) || oid == "" {
			continue
		}
		next = append(next, RefEntry{Name: name, Oid: oid})
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Name < next[j].Name })
	headOid := c.base.HeadOid
	if headTarget := c.base.HeadTarget; headTarget != "" {
		for _, e := range next {
			if e.Name == headTarget {
				headOid = e.Oid
				break
			}
		}
	}
	c.base = &RefSnapshot{
		Gen:        c.base.Gen + 1,
		Refs:       next,
		HeadTarget: c.base.HeadTarget,
		HeadOid:    headOid,
		FP: RefFingerprint{
			Gen:       c.base.FP.Gen + 1,
			PackedLen: c.base.FP.PackedLen, PackedMtime: c.base.FP.PackedMtime,
			HeadMtime: c.base.FP.HeadMtime,
		},
	}
	c.pending = nil
}

// RefView is O(k): binary-search the base snapshot, merge the pending map — it
// NEVER materializes a full copy (04_git.md §4.2).
func (c *RefCache) RefView(name string) (RefEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.base == nil {
		return RefEntry{}, false
	}
	e, ok := c.base.Get(name)
	if ok {
		if oid, pending := c.pending[name]; pending {
			if isZeroOid(string(oid)) || oid == "" {
				return RefEntry{}, false
			}
			e.Oid = oid
		}
		return e, true
	}
	if oid, pending := c.pending[name]; pending && !isZeroOid(string(oid)) && oid != "" {
		return RefEntry{Name: name, Oid: oid}, true
	}
	return RefEntry{}, false
}

// PeekRef resolves one ref for callers that don't want a full snapshot: the
// loose file wins over packed-refs.
func PeekRef(repo *LocalRepo, name string) (RefEntry, bool) {
	loosePath := filepath.Join(repo.Path, filepath.FromSlash(name))
	if data, err := os.ReadFile(loosePath); err == nil {
		s := strings.TrimSpace(string(data))
		if strings.HasPrefix(s, "ref: ") {
			return RefEntry{Name: name, Oid: s}, true
		}
		if ValidOid(s) {
			return RefEntry{Name: name, Oid: Oid(s)}, true
		}
	}
	return RefEntry{}, false
}

// --- apply_ref_txn (§4.3) ------------------------------------------------------

// ValidateRefUpdate checks one update before any subprocess: ref name rules,
// oid rules (empty/all-zero = absent marker; else 40/64 hex).
func ValidateRefUpdate(u *RefUpdate) error {
	if err := ValidateRefName(u.Name); err != nil {
		return err
	}
	if !ValidOid(u.OldOid) {
		return errInvalidInput("invalid old oid for %s", u.Name)
	}
	if !ValidOid(u.NewOid) {
		return errInvalidInput("invalid new oid for %s", u.Name)
	}
	if u.NewSymbolicTarget != "" && u.Name != "HEAD" {
		return errInvalidInput("symbolic target only valid on HEAD, got %s", u.Name)
	}
	return nil
}

// ApplyRefTxn applies one transaction via ONE `git update-ref --stdin`
// subprocess with the exact start/create|update|delete/prepare/commit grammar.
// check_old verifies old values against the current snapshot view first.
// Symbolic HEAD updates are applied afterwards by direct file write.
func (l *Layer) ApplyRefTxn(ctx context.Context, repo *LocalRepo, txn []RefUpdate, checkOld bool) error {
	for _, u := range txn {
		if err := ValidateRefUpdate(&u); err != nil {
			return err
		}
	}

	snap, err := l.SnapshotFrom(repo.cache(), repo)
	if err != nil {
		return err
	}
	if checkOld {
		for _, u := range txn {
			if u.Name == "HEAD" {
				continue
			}
			cur, exists := snap.Get(u.Name)
			if e, loose := PeekRef(repo, u.Name); loose {
				exists, cur = true, e
			}
			expected, actual := u.OldOid, ""
			if exists {
				actual = string(cur.Oid)
			}
			if isZeroOid(expected) || expected == "" {
				if exists {
					return refConflict(u.Name, "", actual)
				}
			} else if !exists || actual != expected {
				return refConflict(u.Name, expected, actual)
			}
		}
	}

	var b bytes.Buffer
	b.WriteString("start\n")
	var symbolic []*RefUpdate
	for _, u := range txn {
		if u.Name == "HEAD" {
			symbolic = append(symbolic, &u)
			continue
		}
		newOid := u.NewOid
		if u.NewSymbolicTarget != "" {
			b.WriteString(fmt.Sprintf("create %s ref:%s\n", u.Name, u.NewSymbolicTarget))
			continue
		}
		switch {
		case isZeroOid(newOid) || newOid == "":
			if checkOld && !isZeroOid(u.OldOid) && u.OldOid != "" {
				fmt.Fprintf(&b, "delete %s %s\n", u.Name, u.OldOid)
			} else {
				fmt.Fprintf(&b, "delete %s\n", u.Name)
			}
		case isZeroOid(u.OldOid) || u.OldOid == "":
			if checkOld {
				// create: zero old verified = ref MUST NOT exist
				fmt.Fprintf(&b, "create %s %s\n", u.Name, newOid)
			} else {
				fmt.Fprintf(&b, "update %s %s\n", u.Name, newOid)
			}
		}
	}
	b.WriteString("prepare\ncommit\n")

	_, stderr, err := l.runCollect(ctx, execSpec{
		argv:  []string{"update-ref", "--stdin"},
		dir:   repo.Path,
		stdin: &b,
	})
	if err != nil {
		return l.refConflictFromStderr(repo, txn, stderr, snap, err)
	}
	for _, u := range symbolic {
		if u.NewSymbolicTarget != "" {
			if err := SetSymbolicHead(repo, u.NewSymbolicTarget); err != nil {
				return err
			}
		}
	}
	repo.cache().Patch(txn)
	return nil
}

// refConflictFromStderr parses the refname from git's stderr — the first
// single-quoted token in the offending line — and re-verifies against the
// snapshot so the caller gets expected/actual (04_git.md §4.3).
func (l *Layer) refConflictFromStderr(repo *LocalRepo, txn []RefUpdate, stderr string, snap *RefSnapshot, cause error) error {
	ref := ""
	for line := range strings.SplitSeq(stderr, "\n") {
		if i := strings.Index(line, "'"); i >= 0 {
			if j := strings.Index(line[i+1:], "'"); j >= 0 {
				ref = line[i+1 : i+1+j]
				break
			}
		}
	}
	if ref == "" {
		return cause
	}
	expected, actual := "", ""
	for _, u := range txn {
		if u.Name == ref {
			expected = u.OldOid
			break
		}
	}
	if e, ok := snap.Get(ref); ok {
		actual = string(e.Oid)
	} else if e, ok := PeekRef(repo, ref); ok {
		actual = string(e.Oid)
	}
	return refConflict(ref, expected, actual)
}

// --- snapshot load / offline txns (§4.4) ----------------------------------------

// LoadSnapshot writes packed-refs atomically (temp + rename), removes the
// loose refs tree and recreates the refs/heads + refs/tags skeletons, rewrites
// HEAD, then refreshes the cache.
func (l *Layer) LoadSnapshot(repo *LocalRepo, refs []RefEntry, headTarget string, headOid Oid) error {
	refs = append([]RefEntry(nil), refs...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	var b bytes.Buffer
	b.WriteString("# pack-refs with: peeled fully-peeled sorted\n")
	for _, e := range refs {
		fmt.Fprintf(&b, "%s %s\n", e.Oid, e.Name)
		if e.Peeled != "" {
			fmt.Fprintf(&b, "^%s\n", e.Peeled)
		}
	}
	if err := atomicWrite(filepath.Join(repo.Path, "packed-refs"), b.Bytes()); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(repo.Path, "refs")); err != nil {
		return err
	}
	for _, d := range []string{"refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(repo.Path, filepath.FromSlash(d)), 0o755); err != nil {
			return err
		}
	}
	if headTarget != "" {
		if err := SetSymbolicHead(repo, headTarget); err != nil {
			return err
		}
		// Resolved HEAD for the snapshot: HEAD's target ref value if known,
		// else the explicit headOid.
		for _, e := range refs {
			if e.Name == headTarget {
				headOid = e.Oid
				break
			}
		}
	} else if headOid != "" {
		if err := os.WriteFile(filepath.Join(repo.Path, "HEAD"), []byte(headOid+"\n"), 0o644); err != nil {
			return err
		}
	}
	repo.cache().mu.Lock()
	repo.cache().base = nil
	repo.cache().pending = nil
	repo.cache().mu.Unlock()
	warm, err := l.Snapshot(repo)
	_ = warm
	return err
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".walgit-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ApplyRefTxnsOffline is the txn merge performed purely in memory (works when
// objects are absent) — what snapshot/replay apply (doc 05).
func ApplyRefTxnsOffline(refs []RefEntry, txn []RefUpdate) []RefEntry {
	byName := make(map[string]RefEntry, len(refs))
	for _, e := range refs {
		byName[e.Name] = e
	}
	for _, u := range txn {
		if u.Name == "HEAD" {
			continue
		}
		if isZeroOid(u.NewOid) || u.NewOid == "" {
			delete(byName, u.Name)
			continue
		}
		e := byName[u.Name]
		e.Name, e.Oid = u.Name, u.NewOid
		if u.NewPeeled != "" {
			e.Peeled = u.NewPeeled
		}
		byName[u.Name] = e
	}
	out := make([]RefEntry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PackRefs runs `git pack-refs --all --prune` in the repo git-dir (60 s).
func (l *Layer) PackRefs(ctx context.Context, repo *LocalRepo) error {
	_, err := l.runPooled(ctx, execSpec{
		argv: []string{"pack-refs", "--all", "--prune"}, dir: repo.Path, timeout: 60 * time.Second,
	})
	return err
}

// --- tag peeling (§4.1) -----------------------------------------------------------

// maxTagHops caps annotated-tag peel chains (deeper = unpeelable).
const maxTagHops = 16

// Peel resolves an annotated tag to its commit (refs/tags/* only at call
// sites), through a persistent `git cat-file --batch` per repo, memoized in
// the per-repo peel cache (LRU-capped at cache.ref_advert_entries).
func (l *Layer) Peel(ctx context.Context, repo *LocalRepo, oid Oid) (Oid, bool) {
	if peeled, ok := l.cachedPeel(repo, oid); ok {
		return peeled, peeled != ""
	}
	pc := l.peelFor(repo)
	resolved, ok, err := pc.peel(ctx, l.Binary, repo.Path, string(oid))
	if err != nil || !ok {
		l.rememberPeel(repo, oid, "")
		return "", false
	}
	l.rememberPeel(repo, oid, resolved)
	return resolved, true
}

func (l *Layer) cachedPeel(repo *LocalRepo, oid Oid) (Oid, bool) {
	l.peelMu.Lock()
	pc, ok := l.peels[repo.Path]
	l.peelMu.Unlock()
	if !ok {
		return "", false
	}
	if v, ok := pc.cache.Load(oid); ok {
		s, _ := v.(string)
		return Oid(s), s != ""
	}
	return "", false
}

func (l *Layer) rememberPeel(repo *LocalRepo, oid, peeled Oid) {
	l.peelMu.Lock()
	pc, ok := l.peels[repo.Path]
	if !ok {
		pc = &peelClient{}
		l.peels[repo.Path] = pc
	}
	l.peelMu.Unlock()
	pc.cache.Store(oid, string(peeled))
}

func (l *Layer) peelFor(repo *LocalRepo) *peelClient {
	l.peelMu.Lock()
	defer l.peelMu.Unlock()
	pc, ok := l.peels[repo.Path]
	if !ok {
		pc = &peelClient{}
		l.peels[repo.Path] = pc
	}
	return pc
}

// peelClient holds the per-repo peel cache and persistent cat-file process.
type peelClient struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	cache  sync.Map
	lru    []string
}

func (pc *peelClient) lruKey(k string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if i := indexOf(pc.lru, k); i >= 0 {
		pc.lru = append(pc.lru[:i], pc.lru[i+1:]...)
	}
	pc.lru = append(pc.lru, k)
	if len(pc.lru) > peelCacheCap {
		drop := pc.lru[0]
		pc.lru = pc.lru[1:]
		pc.cache.Delete(drop)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func (pc *peelClient) cacheStore(k, v string) {
	pc.cache.Store(k, v)
	pc.lruKey(k)
}

func (pc *peelClient) cacheLookup(k string) (string, bool) {
	if v, ok := pc.cache.Load(k); ok {
		s, _ := v.(string)
		return s, true
	}
	return "", false
}

func (pc *peelClient) close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.stdin != nil {
		pc.stdin.Close()
		pc.stdin = nil
	}
	if pc.cmd != nil {
		pc.cmd.Process.Kill()
		pc.cmd = nil
	}
}

// peelCacheCap is cache.ref_advert_entries default.
const peelCacheCap = 256

// peel walks the tag chain: send <oid>\n, read `<oid> <type> <size>\n` +
// payload; for `tag` parse the `object <oid>` header line and repeat (≤ 16
// hops; deeper chains unpeelable).
func (pc *peelClient) peel(ctx context.Context, binary, repoPath, oid string) (string, bool, error) {
	cur := oid
	for range maxTagHops {
		if v, ok := pc.cacheLookup(cur); ok {
			if v == "" {
				return "", false, nil
			}
			return v, true, nil
		}
		typ, body, err := pc.catFile(ctx, binary, repoPath, cur)
		if err != nil || typ != "tag" {
			return "", false, err
		}
		next, ok := tagObjectTarget(body)
		if !ok {
			return "", false, nil
		}
		pc.cacheStore(cur, next)
		cur = next
	}
	return "", false, nil // chain deeper than 16 hops: unpeelable
}

func (pc *peelClient) catFile(ctx context.Context, binary, repoPath, oid string) (string, []byte, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.cmd == nil {
		cmd := exec.Command(binary, "cat-file", "--batch")
		cmd.Dir = repoPath
		cmd.Env = baseEnvFor(repoPath)
		in, err := cmd.StdinPipe()
		if err != nil {
			return "", nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return "", nil, err
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return "", nil, err
		}
		pc.cmd, pc.stdin, pc.stdout = cmd, in, bufio.NewReader(out)
	}
	if _, err := io.WriteString(pc.stdin, oid+"\n"); err != nil {
		pc.resetLocked()
		return "", nil, err
	}
	header, err := pc.stdout.ReadString('\n')
	if err != nil {
		pc.resetLocked()
		return "", nil, err
	}
	fields := strings.Fields(header)
	if len(fields) < 3 {
		return "", nil, nil // "<oid> missing"
	}
	size, err := parseSize(fields[2])
	if err != nil {
		return "", nil, err
	}
	body := make([]byte, size+1) // + trailing newline
	if _, err := io.ReadFull(pc.stdout, body); err != nil {
		pc.resetLocked()
		return "", nil, err
	}
	return fields[1], body[:size], nil
}

func (pc *peelClient) resetLocked() {
	if pc.stdin != nil {
		pc.stdin.Close()
		pc.stdin = nil
	}
	if pc.cmd != nil {
		pc.cmd.Process.Kill()
		pc.cmd = nil
	}
	pc.stdout = nil
}

func baseEnvFor(repoPath string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + repoPath}
}

func parseSize(s string) (int64, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, errInvalidInput("bad cat-file size %q", s)
	}
	return n, nil
}

// tagObjectTarget extracts the `object <oid>` header from an annotated tag body.
func tagObjectTarget(body []byte) (string, bool) {
	for line := range strings.SplitSeq(string(body), "\n") {
		if line == "" {
			break
		}
		if oid, ok := strings.CutPrefix(line, "object "); ok {
			return strings.TrimSpace(oid), true
		}
	}
	return "", false
}
