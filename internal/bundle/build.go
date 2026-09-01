package bundle

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Primitives are the §7.8 git seams (§8.2; exact argv normative in 08_bundles.md).
// Every call runs with GIT_TERMINAL_PROMPT=0 and the repo dir as GIT_DIR.
type Primitives interface {
	// BundleCreate runs `git bundle create <out> --stdin` fed the selected ref
	// lines; blocking; returns size + PACK-magic offset (header/pack split).
	BundleCreate(ctx context.Context, repoDir, outPath string, refs []string) (size, packOffset int64, err error)
	// PackDelta runs `git pack-objects --revs --delta-base-offset --stdout -q
	// [--filter=blob:none]` with wants on stdin followed by ^excludes. NEVER
	// --thin (§8.9.2: self-contained packs; thin deltas cost clients 48 s +
	// 420 MB of appended bases on a 32 GB base).
	PackDelta(ctx context.Context, repoDir string, wants, excludes []string, filter string, w io.Writer) error
	// CountCommits is `git rev-list --count <tips…> --not <notTips…>` (§8.7
	// min_commits gate; commits/trees are local on every maintaining host).
	CountCommits(ctx context.Context, repoDir string, tips, notTips []string) (int, error)
}

// Reporter is the narration seam of a running task (§8.9.1: every build is a
// narrated task per 05_wal_engine.md §tasks). *wal.Task satisfies it.
type Reporter interface {
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

// TaskRunner is the task seam (§8.9.1): task kind `bundle`, params
// {strategy, slot}; join semantics per (repo, kind).
type TaskRunner interface {
	RunBundle(ctx context.Context, repo string, params map[string]string, fn func(ctx context.Context, tr Reporter) error) error
}

// Deps wires the package to its environment. Tests supply fakes.
type Deps struct {
	Wal      WalView
	Prim     Primitives
	St       store.ObjectStore
	Tasks    TaskRunner // nil → fn runs inline
	Now      func() time.Time
	CacheDir string // temp files under cache.dir (§8.9.6 step 1)
	HostID   string // lease holder
	RepoDir  string // local bare repo (packs must be local for incrementals)

	// MinCommits is bundles.min_commits (§8.7 default 25).
	MinCommits int
	// List is the pass's read of bundles/list.pb (gate + base resolution view).
	List *proto.BundleList
	// MainOnly + ExtraRefs implement §8.8 ref selection.
	MainOnly  bool
	ExtraRefs []string

	// LocalPack resolves a pack checksum to the local pack file path for the
	// §8.9.4 big-repo compose path; nil/absent → fulls use `git bundle create`.
	LocalPack func(checksum string) (path string, ok bool)

	// ObjectFormat is the repo hash algo ("sha1"|"sha256"); sha256 forces v3.
	ObjectFormat string

	// Verdicts collects closed-slot verdicts for the pass's batch CAS (§8.7).
	// The caller commits them in one RecordVerdicts CAS after the pass.
	Verdicts []*proto.SkippedSlot
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// BuildSlot builds one slot through the full pipeline (§8.9): task + lease +
// gates + pack/bundle-create/compose + publish. Slots before first_state_at
// are never built; closed-slot verdicts are appended to d.Verdicts (batched
// into one CAS by the pass, §8.7).
func BuildSlot(ctx context.Context, d *Deps, repo string, strategies []Strategy, s *Strategy, slot time.Time) error {
	if d.Tasks != nil {
		params := map[string]string{"strategy": s.Name, "slot": FormatSlot(slot)}
		return d.Tasks.RunBundle(ctx, repo, params, func(bctx context.Context, tr Reporter) error {
			return buildSlotInner(bctx, d, tr, repo, strategies, s, slot)
		})
	}
	return buildSlotInner(ctx, d, nullReporter{}, repo, strategies, s, slot)
}

type nullReporter struct{}

func (nullReporter) Notice(string)                           {}
func (nullReporter) Progress(string, uint64, uint64, string) {}

func buildSlotInner(ctx context.Context, d *Deps, tr Reporter, repo string, strategies []Strategy, s *Strategy, slot time.Time) error {
	// §8.9.1: cross-host exclusion — lease before any work.
	release, err := acquireLease(ctx, d.St, "bundle-"+s.Name, d.HostID)
	if err != nil {
		return fmt.Errorf("bundle: lease %s: %w", s.Name, err)
	}
	defer release()

	// §8.4 content resolution.
	content, err := ContentAt(ctx, d.Wal, repo, slot)
	if err != nil {
		return err
	}
	switch content.Verdict {
	case VerdictUnavail:
		tr.Notice(fmt.Sprintf("slot %s is before first_state_at; unavailable", FormatSlot(slot)))
		return nil
	case VerdictNoState:
		tr.Notice("no state as of the slot")
		d.addVerdict(proto.SkippedSlot{
			Strategy: s.Name, Slot: uint64(slot.Unix()), BaseID: "", AsOfSeq: 0,
			Reason: "no state as of the slot", At: tsPtr(d.now()),
		})
		return nil
	}

	// §8.8 ref selection against the as-of tips.
	refNames := make([]string, 0, len(content.Tips))
	for _, r := range content.Tips {
		refNames = append(refNames, r.Name)
	}
	selected := SelectRefs(s, d.MainOnly, d.ExtraRefs, refNames)
	tips := refsIn(content.Tips, selected)
	if len(tips) == 0 {
		tr.Notice("no refs selected for the slot; nothing to build")
		return nil
	}

	byName := ByName(strategies)
	var base *proto.BundleEntry
	if s.Kind == KindIncremental {
		base = BaseFor(s, uint64(slot.Unix()), d.List, byName)
		if base == nil {
			return ErrBlocked // waiting for the first base bundle (§8.6)
		}
	}

	// §8.7 gates — incrementals only; fulls are never gated.
	closed := d.now().Sub(slot) > CloseGrace
	if s.Kind == KindIncremental {
		if verdict, stop := d.gates(ctx, tr, s, slot, content, base, closed); stop {
			_ = verdict
			return nil
		}
	}

	tr.Notice(fmt.Sprintf("building %s slot %s (%s)", s.Name, FormatSlot(slot), s.Kind))
	entry, err := buildAndPublish(ctx, d, tr, repo, s, slot, tips, base)
	if err != nil {
		return err
	}
	tr.Notice(fmt.Sprintf("published %s (%d bytes)", entry.ID, entry.Size))
	return nil
}

// gates applies the unchanged + min_commits gates (§8.7, evaluation order 2-3).
// Returns stop=true when the slot is skipped; closed-slot verdicts are
// recorded in d.Verdicts, open slots are re-measured each pass.
func (d *Deps) gates(ctx context.Context, tr Reporter, s *Strategy, slot time.Time, content Content, base *proto.BundleEntry, closed bool) (*proto.SkippedSlot, bool) {
	slotEpoch := uint64(slot.Unix())

	// 2. Unchanged gate: tip set equals the newest BundleEntry of the same
	// strategy (§8.7 — regardless of base_id; an idle night must not cut 24
	// identical bundles). Pure in-memory comparison.
	if prev := NewestEntry(d.List, s.Name, slotEpoch, false); prev != nil && SameTips(content.Tips, derefRefs(prev.Tips)) {
		tr.Notice(fmt.Sprintf("unchanged since %s", prev.ID))
		if closed {
			v := &proto.SkippedSlot{Strategy: s.Name, Slot: slotEpoch, BaseID: baseIDOrEmpty(base),
				AsOfSeq: content.AsOfSeq, Reason: "unchanged since " + prev.ID, At: tsPtr(d.now())}
			d.Verdicts = append(d.Verdicts, v)
			return v, true
		}
		return nil, true
	}

	// 3. bundles.min_commits gate (§8.7): commits since the base.
	if base != nil {
		minCommits := s.EffectiveMinCommits(d.MinCommits)
		if minCommits > 0 && d.Prim != nil {
			n, err := d.Prim.CountCommits(ctx, d.RepoDir, TipOids(content.Tips), TipOids(derefRefs(base.Tips)))
			if err != nil {
				tr.Notice(fmt.Sprintf("commit count failed: %v", err))
				return nil, false // gate not evaluable → proceed to build (safe)
			}
			if n < minCommits {
				reason := fmt.Sprintf("too-small: %d commits (min %d)", n, minCommits)
				tr.Notice(reason)
				if closed {
					v := &proto.SkippedSlot{Strategy: s.Name, Slot: slotEpoch, BaseID: base.ID,
						AsOfSeq: content.AsOfSeq, Reason: reason, At: tsPtr(d.now())}
					d.Verdicts = append(d.Verdicts, v)
					return v, true
				}
				return nil, true
			}
		}
	}
	return nil, false
}

func baseIDOrEmpty(base *proto.BundleEntry) string {
	if base == nil {
		return ""
	}
	return base.ID
}

// addVerdict appends a closed-slot verdict (§8.7).
func (d *Deps) addVerdict(v proto.SkippedSlot) {
	d.Verdicts = append(d.Verdicts, &v)
}

// refsIn returns the tips whose names are in `names`, preserving the
// HEAD-first name-sorted order of `names`.
func refsIn(tips Refs, names []string) []proto.Ref {
	byName := make(map[string]proto.Ref, len(tips))
	for _, r := range tips {
		byName[r.Name] = r
	}
	out := make([]proto.Ref, 0, len(names))
	for _, n := range names {
		if r, ok := byName[n]; ok {
			out = append(out, r)
		}
	}
	return out
}

// buildAndPublish renders/streams content, hashes, uploads, and CAS-upserts
// the entry (§8.9.6).
func buildAndPublish(ctx context.Context, d *Deps, tr Reporter, repo string, s *Strategy, slot time.Time, tips []proto.Ref, base *proto.BundleEntry) (*proto.BundleEntry, error) {
	sha256 := d.ObjectFormat == "sha256"
	filter := s.Filter
	var prereqs []string
	if base != nil {
		for _, r := range base.Tips {
			prereqs = append(prereqs, r.Oid)
		}
	}

	tmpDir := d.CacheDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(tmpDir, "bundle-*.part")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	var hasher hash.Hash = sha1.New()
	var size int64
	switch {
	case s.Kind == KindIncremental:
		// Header (own render, with prerequisites) ∘ pack-objects stdout (§8.9.2).
		hdr := RenderHeader(sha256, d.ObjectFormat, filter, prereqs, tips)
		if _, err := tmp.Write(hdr); err != nil {
			tmp.Close()
			return nil, err
		}
		hasher.Write(hdr)
		size += int64(len(hdr))
		wants := TipOids(tips)
		var excludes []string
		if base != nil {
			excludes = TipOids(derefRefs(base.Tips))
		}
		// Stream: tee pack-objects stdout into (temp file, sha1, size).
		cw := &countingHashWriter{w: tmp, h: hasher}
		if err := d.Prim.PackDelta(ctx, d.RepoDir, wants, excludes, filter, cw); err != nil {
			tmp.Close()
			return nil, fmt.Errorf("bundle: pack-objects: %w", err)
		}
		size += cw.n
		tr.Progress("pack", 1, 1, "pack")
	case base != nil && d.LocalPack != nil:
		// Big-repo weekly compose: header ∘ local base pack (§8.9.4).
		if path, ok := d.LocalPack(baseChecksum(base)); ok {
			tmp.Close()
			return d.composeFull(ctx, repo, s, slot, base, path)
		}
		fallthrough
	default:
		// Full: `git bundle create <tmpfile> --stdin` (§8.9.3).
		tmp.Close()
		refLines := make([]string, 0, len(tips))
		for _, r := range tips {
			refLines = append(refLines, r.Oid+" "+r.Name)
		}
		size, _, err = d.Prim.BundleCreate(ctx, d.RepoDir, tmpPath, refLines)
		if err != nil {
			return nil, fmt.Errorf("bundle: git bundle create: %w", err)
		}
		f, err := os.Open(tmpPath)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(hasher, f); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
	}
	if err := tmp.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return nil, err
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	key := store.BundleObjectKey(s.Name, FormatSlot(slot)+"-"+sum+".bundle")
	meta, err := d.St.Put(ctx, key, store.PutBody{File: tmpPath},
		store.PutOptions{Mode: store.PutCreate, Immutable: true})
	if err != nil && !store.IsPreconditionFailed(err) {
		return nil, fmt.Errorf("bundle: put %s: %w", key, err)
	}
	// A lost Create race is harmless: slot content is deterministic, the
	// winner uploaded byte-identical content under the same key (§8.9.1).

	entry := &proto.BundleEntry{
		ID:            EntryID(s.Name, slot),
		Key:           key,
		Strategy:      s.Name,
		Kind:          s.Kind,
		CreationToken: uint64(slot.Unix()),
		Seq:           baseSeqOrZero(base),
		Size:          uint64(size),
		BaseID:        baseIDOrEmpty(base),
		CreatedAt:     tsPtr(d.now()),
		Version:       string(meta.Version),
		Tips:          refPtrs(tips),
		Slot:          uint64(slot.Unix()),
		Filter:        filter,
	}
	if err := UpsertEntry(ctx, d.St, entry); err != nil {
		return nil, fmt.Errorf("bundle: list cas: %w", err)
	}
	return entry, nil
}

// composeFull is ComposeFullFromBase (§8.9.4): header ∘ local base pack via
// store.Compose — zero bucket bytes through the host.
func (d *Deps) composeFull(ctx context.Context, repo string, s *Strategy, slot time.Time, base *proto.BundleEntry, basePackPath string) (*proto.BundleEntry, error) {
	// Refs at the base pack's seq (checkpoint written on the spot when none
	// exists and no ref moved since — §8.4).
	refs, err := d.Wal.RefsAtSeq(ctx, repo, base.Seq)
	if err != nil {
		return nil, fmt.Errorf("bundle: refs at base seq %d: %w", base.Seq, err)
	}
	SortRefs(refs)
	sel := SelectRefs(nil, d.MainOnly, d.ExtraRefs, refNames(refs))
	refTips := refsIn(refs, sel)

	filter := s.Filter
	hdr := RenderHeader(d.ObjectFormat == "sha256", d.ObjectFormat, filter, nil, refTips)

	// sum = sha1(header ∘ base pack bytes), streamed from the LOCAL file.
	h := sha1.New()
	h.Write(hdr)
	pf, err := os.Open(basePackPath)
	if err != nil {
		return nil, fmt.Errorf("bundle: base pack: %w", err)
	}
	packSize, err := io.Copy(h, pf)
	pf.Close()
	if err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	size := uint64(len(hdr)) + uint64(packSize)

	scratch := composeScratchKey(repo, s.Name, slot)
	if _, err := d.St.Put(ctx, scratch, store.PutBody{Bytes: hdr}, store.PutOptions{Mode: store.PutCreate}); err != nil && !store.IsPreconditionFailed(err) {
		return nil, fmt.Errorf("bundle: scratch put: %w", err)
	}
	key := store.BundleObjectKey(s.Name, FormatSlot(slot)+"-"+sum+".bundle")
	_, cerr := d.St.Compose(ctx, key, []string{scratch, base.Key}, store.PutOptions{Mode: store.PutCreate, Immutable: true})
	_ = d.St.Delete(ctx, scratch, "") // best-effort (§8.9.4); a leftover scratch is swept
	if cerr != nil && !store.IsPreconditionFailed(cerr) {
		return nil, fmt.Errorf("bundle: compose: %w", cerr)
	}
	if cerr != nil && store.IsPreconditionFailed(cerr) {
		// A lost Create race: byte-identical content already there.
		cerr = nil
	}

	entry := &proto.BundleEntry{
		ID:            EntryID(s.Name, slot),
		Key:           key,
		Strategy:      s.Name,
		Kind:          KindFull,
		CreationToken: uint64(slot.Unix()),
		Seq:           base.Seq,
		Size:          size,
		BaseID:        "",
		CreatedAt:     tsPtr(d.now()),
		Tips:          refPtrs(refTips),
		Slot:          uint64(slot.Unix()),
		Filter:        filter,
	}
	if err := UpsertEntry(ctx, d.St, entry); err != nil {
		return nil, fmt.Errorf("bundle: list cas: %w", err)
	}
	return entry, nil
}

// composeScratchKey is wal/_compose/<o>/<r>/<strategy>-<slotUTC>.header (§8.9.4).
func composeScratchKey(repo, strategy string, slot time.Time) string {
	owner, name := splitRepo(repo)
	return fmt.Sprintf("wal/_compose/%s/%s/%s-%s.header", owner, name, strategy, FormatSlot(slot))
}

func splitRepo(repo string) (owner, name string) {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			return repo[:i], repo[i+1:]
		}
	}
	return repo, ""
}

func refNames(refs Refs) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

// countingHashWriter tees into a hash while counting (§8.9.2 streaming).
type countingHashWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (c *countingHashWriter) Write(p []byte) (int, error) {
	c.h.Write(p)
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// baseChecksum extracts the base pack checksum from a base entry's key
// (wal/<checksum>.pack naming; §8.9.4).
func baseChecksum(base *proto.BundleEntry) string {
	k := filepath.Base(base.Key)
	for _, suffix := range []string{".pack", ".bundle", ".idx"} {
		if len(k) > len(suffix) && k[len(k)-len(suffix):] == suffix {
			return k[:len(k)-len(suffix)]
		}
	}
	return k
}

func baseSeqOrZero(base *proto.BundleEntry) uint64 {
	if base == nil {
		return 0
	}
	return base.Seq
}

// strategyFilterOf resolves the family filter for a composed full from the
// base entry (a whole chain shares one filter — §8.5).
func strategyFilterOf(d *Deps, base *proto.BundleEntry) string {
	return base.Filter
}

func tsPtr(t time.Time) *proto.Timestamp {
	ts := proto.TimeFromGo(t)
	return &ts
}

// refPtrs converts the value-slice tips to the entry's pointer form.
func refPtrs(refs []proto.Ref) []*proto.Ref {
	out := make([]*proto.Ref, 0, len(refs))
	for i := range refs {
		out = append(out, &refs[i])
	}
	return out
}
