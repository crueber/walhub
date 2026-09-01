// bind_wal.go adapts internal/wal's surface onto this package's interfaces —
// the key seam of 07_api.md §1: handler files NEVER import internal/wal; only
// this file (and recipes.go, its recipe helper) may. The engine is injected at
// startup as the WalEngine structural interface; the compiler checks the
// shapes against the concrete adapter in internal/server/bind_wal.go.
package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// WalEngine is the subset of the engine surface this binding needs. It is
// structural: internal/server passes its adapter in at startup and the
// compiler checks the shapes.
type WalEngine interface {
	// Sync materializes the repo to the requested level (§2 of doc 05).
	Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error
	// ObjectAccess builds the per-request object reader (local packs or the
	// remote reader) after a sync — the §1 "ObjectAccess bridging" seam.
	ObjectAccess(ctx context.Context, id git.RepoId) (wal.ObjectAccess, error)
	// Revision is the repo's current manifest revision (render-cache stamp).
	Revision(ctx context.Context, id git.RepoId) (uint64, error)
	// Manifest returns a stable copy of the repo manifest (overview/settings).
	Manifest(ctx context.Context, id git.RepoId) (*proto.Manifest, error)
	// ReadLog streams live log entries [from, to] (push/settings history).
	ReadLog(ctx context.Context, id git.RepoId, from, to uint64) ([]*proto.LogEntry, error)
	// PublishSettings publishes a D24 settings payload and returns the revision.
	PublishSettings(ctx context.Context, id git.RepoId, body []byte, message, author string) (uint64, error)
}

// syncLevelToWal mirrors the handler-side enum onto the frozen engine enum.
func syncLevelToWal(l SyncLevel) wal.SyncLevel {
	switch l {
	case SyncServe:
		return wal.LevelServe
	case SyncFull:
		return wal.LevelFull
	default:
		return wal.LevelRefs
	}
}

// taskFromWal converts the engine's frozen TaskRecord onto the wire shape.
// Field-for-field identical (doc 05 §6.8 frozen); the conversion exists so
// the wal import stays in this file alone.
func taskFromWal(t wal.TaskRecord) TaskRecord {
	var progress *Progress
	if t.Progress != nil {
		p := progressFromWal(*t.Progress)
		progress = &p
	}
	tail := t.LogTail
	if tail == nil {
		tail = []string{}
	}
	return TaskRecord{
		ID:        t.ID,
		Kind:      t.Kind,
		Repo:      t.Repo,
		Hostname:  t.Hostname,
		Started:   t.Started,
		Finished:  t.Finished,
		ElapsedMS: t.ElapsedMS,
		OK:        t.OK,
		Summary:   t.Summary,
		Progress:  progress,
		LogTail:   tail,
		Params:    t.Params,
	}
}

// progressFromWal converts one engine narration packet.
func progressFromWal(p wal.Progress) Progress {
	return Progress{
		Kind:    p.Kind,
		Text:    p.Text,
		Label:   p.Label,
		Done:    p.Done,
		Total:   p.Total,
		Unit:    p.Unit,
		Percent: p.Percent,
	}
}

// TaskRecordFromWal is the exported conversion for composition glue (cmd).
func TaskRecordFromWal(t wal.TaskRecord) TaskRecord { return taskFromWal(t) }

// ProgressFromWal is the exported conversion for composition glue (cmd).
func ProgressFromWal(p wal.Progress) Progress { return progressFromWal(p) }

// walView implements RepoView over the engine: every method syncs the repo to
// the level it needs, then runs the §9 git recipes over the local serving copy
// (recipes.go). Repos served through the remote reader answer errRemoteServed.
type walView struct {
	engine WalEngine
	layer  *git.Layer // ref-snapshot machinery over the serving copies
	binary string     // git.binary
}

var _ RepoView = (*walView)(nil)

// localView syncs and hands back the local serving copy + revision stamp.
func (v *walView) localView(ctx context.Context, id git.RepoId, level SyncLevel) (*git.LocalRepo, uint64, error) {
	if v.engine == nil {
		return nil, 0, ErrPending
	}
	if err := v.engine.Sync(ctx, id, syncLevelToWal(level)); err != nil {
		return nil, 0, notFoundOr(err)
	}
	access, err := v.engine.ObjectAccess(ctx, id)
	if err != nil {
		return nil, 0, notFoundOr(err)
	}
	if access.Local == nil {
		return nil, 0, errRemoteServed
	}
	rev, err := v.engine.Revision(ctx, id)
	if err != nil {
		return nil, 0, notFoundOr(err)
	}
	return access.Local, rev, nil
}

// notFoundOr maps engine not-found errors onto the wire ErrNotFound (→ 404).
func notFoundOr(err error) error {
	var we *wal.WalError
	if errors.As(err, &we) && we.Kind == wal.WalErrNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, we.Detail)
	}
	return err
}

// snapshot returns the parsed ref state after a refs sync.
func (v *walView) snapshot(ctx context.Context, id git.RepoId) (*git.LocalRepo, *git.RefSnapshot, uint64, error) {
	repo, rev, err := v.localView(ctx, id, SyncRefs)
	if err != nil {
		return nil, nil, 0, err
	}
	layer := v.layer
	snap, err := layer.Snapshot(repo)
	if err != nil {
		return nil, nil, 0, err
	}
	return repo, snap, rev, nil
}

func refFromEntry(e git.RefEntry) Ref {
	sha := string(e.Oid)
	if e.Peeled != "" {
		sha = string(e.Peeled) // tag shas are the peeled commit (§9.2)
	}
	return Ref{Name: e.Name, SHA: sha}
}

func (v *walView) Sync(ctx context.Context, id git.RepoId, level SyncLevel) error {
	if v.engine == nil {
		return ErrPending
	}
	return notFoundOr(v.engine.Sync(ctx, id, syncLevelToWal(level)))
}

func (v *walView) Resolve(ctx context.Context, id git.RepoId, rest string) (Resolution, error) {
	repo, snap, rev, err := v.snapshot(ctx, id)
	if err != nil {
		return Resolution{}, err
	}
	res := Resolution{Revision: rev}

	// Empty rest → the default branch (HEAD); unborn HEAD → not found.
	if rest == "" {
		if snap.HeadTarget == "" {
			return Resolution{}, fmt.Errorf("%w: unborn HEAD", ErrNotFound)
		}
		if e, ok := snap.Get(snap.HeadTarget); ok {
			res.Ref, res.SHA, res.Kind = e.Name, string(e.Oid), kindOf(e.Name)
			return res, nil
		}
		return Resolution{}, fmt.Errorf("%w: HEAD", ErrNotFound)
	}

	segs := strings.Split(rest, "/")
	// Longest ref prefix first; branch beats tag at each length (§9.3).
	for k := len(segs); k >= 1; k-- {
		name := strings.Join(segs[:k], "/")
		if e, ok := snap.Get("refs/heads/" + name); ok {
			res.Ref, res.SHA, res.Path, res.Kind = e.Name, string(e.Oid), strings.Join(segs[k:], "/"), "branch"
			return res, nil
		}
		if e, ok := snap.Get("refs/tags/" + name); ok {
			sha := string(e.Oid)
			if e.Peeled != "" {
				sha = string(e.Peeled)
			}
			res.Ref, res.SHA, res.Path, res.Kind = e.Name, sha, strings.Join(segs[k:], "/"), "tag"
			return res, nil
		}
	}

	// Rev-parse fallback for the first segment (raw commit / short sha).
	sha, err := v.revParse(ctx, repo, segs[0])
	if err != nil {
		return Resolution{}, err
	}
	if sha == "" {
		return Resolution{}, fmt.Errorf("%w: %s", ErrNotFound, segs[0])
	}
	res.Ref, res.SHA, res.Path, res.Kind = "", sha, strings.Join(segs[1:], "/"), "commit"
	return res, nil
}

func kindOf(name string) string {
	switch {
	case strings.HasPrefix(name, "refs/heads/"):
		return "branch"
	case strings.HasPrefix(name, "refs/tags/"):
		return "tag"
	default:
		return "commit"
	}
}

func (v *walView) Head(ctx context.Context, id git.RepoId) (Ref, bool, error) {
	_, snap, _, err := v.snapshot(ctx, id)
	if err != nil {
		return Ref{}, false, err
	}
	if snap.HeadTarget == "" {
		return Ref{}, false, nil
	}
	if e, ok := snap.Get(snap.HeadTarget); ok {
		return refFromEntry(e), true, nil
	}
	return Ref{}, false, nil
}

func (v *walView) RefList(ctx context.Context, id git.RepoId, ns string, q RefQuery) ([]Ref, bool, error) {
	_, snap, _, err := v.snapshot(ctx, id)
	if err != nil {
		return nil, false, err
	}
	prefix := "refs/" + ns + "/"
	if q.Prefix != "" {
		prefix += strings.Trim(q.Prefix, "/") + "/"
	}
	n := q.N
	if n <= 0 {
		n = 100
	}
	out := make([]Ref, 0, n)
	more := false
	for _, e := range snap.Refs { // name-sorted
		if !strings.HasPrefix(e.Name, prefix) {
			continue
		}
		if q.Q != "" && !strings.Contains(strings.ToLower(e.Name), strings.ToLower(q.Q)) {
			continue
		}
		if q.After != "" && e.Name <= q.After {
			continue
		}
		if len(out) >= n {
			more = true // asked for n+1 implicitly: the first overflow marks more
			break
		}
		out = append(out, refFromEntry(e))
	}
	// One extra scan to confirm `more` even when the page filled exactly.
	if len(out) == n && !more {
		rest := out[len(out)-1].Name
		for _, e := range snap.Refs {
			if e.Name > rest && strings.HasPrefix(e.Name, prefix) && (q.Q == "" || strings.Contains(strings.ToLower(e.Name), strings.ToLower(q.Q))) && (q.After == "" || e.Name > q.After) {
				more = true
				break
			}
		}
	}
	return out, more, nil
}

func (v *walView) Tree(ctx context.Context, id git.RepoId, sha, path string) (TreeResult, error) {
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return TreeResult{}, err
	}
	spec := sha
	if path != "" {
		// A path under the commit: resolve through the commit's tree.
		spec = sha + ":" + path
	}
	out, err := v.gitCmd(ctx, repo, "ls-tree", "-z", "-l", spec)
	if err != nil {
		return TreeResult{}, fmt.Errorf("%w: tree %s:%s not found (%v)", ErrNotFound, sha, path, err)
	}
	entries := parseLsTree(out)
	sortTreeEntries(entries)
	tr := TreeResult{Entries: entries, Path: path}
	if path != "" {
		tr.Readme = v.readmeOf(ctx, repo, entries)
	}
	return tr, nil
}

func (v *walView) Blob(ctx context.Context, id git.RepoId, sha, path string, raw bool) (BlobResult, error) {
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return BlobResult{}, err
	}
	spec := sha + ":" + path
	sizeOut, err := v.gitCmd(ctx, repo, "cat-file", "-s", spec)
	if err != nil {
		return BlobResult{}, fmt.Errorf("%w: blob %s not found", ErrNotFound, path)
	}
	var size int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(sizeOut)), "%d", &size); err != nil {
		return BlobResult{}, err
	}
	res := BlobResult{Path: path, Size: size}
	if size > maxBlobBytes && !raw {
		res.TooLarge = true
		return res, nil
	}
	body, err := v.gitCmd(ctx, repo, "cat-file", "blob", spec)
	if err != nil {
		return BlobResult{}, fmt.Errorf("%w: blob %s unreadable: %v", ErrNotFound, path, err)
	}
	res.Contents = body
	res.Binary = isBinary(body)
	return res, nil
}

func (v *walView) Commits(ctx context.Context, id git.RepoId, sha, path string, skip, n int) (CommitPage, error) {
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return CommitPage{}, err
	}
	if n <= 0 {
		n = 35
	}
	argv := []string{"log", "--format=" + gitFmtLog, "--max-count=" + itoa(n+1), "--skip=" + itoa(skip), "--no-renames", sha}
	if path != "" {
		argv = append(argv, "--", path)
	}
	out, err := v.gitCmd(ctx, repo, argv...)
	if err != nil {
		return CommitPage{}, fmt.Errorf("%w: history of %s: %v", ErrNotFound, sha, err)
	}
	commits := parseLogRecords(out)
	page := CommitPage{Commits: commits}
	if len(commits) > n {
		page.Commits = commits[:n]
		page.More = true
	}
	return page, nil
}

func (v *walView) Commit(ctx context.Context, id git.RepoId, sha string) (CommitDetail, error) {
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return CommitDetail{}, err
	}
	recOut, err := v.gitCmd(ctx, repo, "show", "--no-patch", "--format="+gitFmtShow, sha)
	if err != nil {
		return CommitDetail{}, fmt.Errorf("%w: commit %s not found", ErrNotFound, sha)
	}
	commit, ok := parseShowRecord(recOut)
	if !ok {
		return CommitDetail{}, fmt.Errorf("%w: commit %s malformed", ErrNotFound, sha)
	}
	detail := CommitDetail{Commit: commit}
	if statOut, serr := v.gitCmd(ctx, repo, "diff-tree", "--numstat", "-z", "-r", "--root", "--no-renames", sha); serr == nil {
		detail.Stats, _ = parseNumstatPatch(statOut)
	}
	if patchOut, perr := v.gitCmd(ctx, repo, "show", "--format=", "--no-renames", sha); perr == nil {
		detail.Patch = string(patchOut)
	}
	return detail, nil
}

func (v *walView) Summary(ctx context.Context, id git.RepoId) (SummaryData, error) {
	_, snap, _, err := v.snapshot(ctx, id)
	if err != nil {
		return SummaryData{}, err
	}
	s := SummaryData{Branches: 0, Tags: 0}
	for _, e := range snap.Refs {
		switch {
		case strings.HasPrefix(e.Name, "refs/heads/"):
			s.Branches++
			if e.Name == snap.HeadTarget {
				r := refFromEntry(e)
				s.Head = &r
			}
		case strings.HasPrefix(e.Name, "refs/tags/"):
			s.Tags++
		}
	}
	return s, nil
}

func (v *walView) Overview(ctx context.Context, id git.RepoId) (OverviewData, error) {
	if v.engine == nil {
		return OverviewData{}, ErrPending
	}
	m, err := v.engine.Manifest(ctx, id)
	if err != nil {
		return OverviewData{}, notFoundOr(err)
	}
	ov := OverviewData{
		Health: Health{Status: "ok", Issues: []string{}, Suggestions: []Suggestion{}},
		Manifest: ManifestInfo{
			Version:  m.Revision,
			NextSeq:  m.HeadSeq + 1,
			MinSeq:   m.MinSeq,
			Segments: []string{},
			Entries:  m.HeadSeq - m.MinSeq + 1,
		},
		Local:       LocalInfo{Version: m.Revision, NextSeq: m.HeadSeq + 1},
		Packs:       PacksInfo{Live: len(m.Packs), Pushes: uint64(len(m.Packs))},
		Bundles:     []BundleInfo{},
		BundlePlan:  BundlePlanInfo{Slots: []PlanSlot{}, Upcoming: []string{}, Maintainers: []string{}, Orphaned: []string{}},
		Compactions: []CompactionInfo{},
		Node:        NodeInfo{Counters: map[string]uint64{}},
	}
	for _, seg := range m.LogSegments {
		ov.Manifest.Segments = append(ov.Manifest.Segments, seg.Key)
	}
	for _, p := range m.Packs {
		ov.Packs.LiveBytes += int64(p.PackSize)
	}
	if m.UpdatedAt != nil {
		t := m.UpdatedAt.Go()
		ov.Manifest.LastPush = &t
	}
	if m.Checkpoint != nil {
		ov.Manifest.Checkpoint = map[string]any{"seq": m.Checkpoint.Seq, "key": m.Checkpoint.Key}
	}
	return ov, nil
}

func (v *walView) Settings(ctx context.Context, id git.RepoId) (SettingsDoc, error) {
	if v.engine == nil {
		return SettingsDoc{}, ErrPending
	}
	m, err := v.engine.Manifest(ctx, id)
	if err != nil {
		return SettingsDoc{}, notFoundOr(err)
	}
	if m.Settings == nil {
		return SettingsDoc{}, nil // revision 0 = none ever published (§11)
	}
	s := m.Settings
	doc := SettingsDoc{Revision: s.Revision, Author: s.Author, Message: s.Message, TOML: s.Toml}
	if s.UpdatedAt != nil {
		doc.UpdatedAt = s.UpdatedAt.Go()
	}
	return doc, nil
}

func (v *walView) PublishSettings(ctx context.Context, id git.RepoId, body []byte, message, author string) (uint64, error) {
	if v.engine == nil {
		return 0, ErrPending
	}
	rev, err := v.engine.PublishSettings(ctx, id, body, message, author)
	if err != nil {
		return 0, notFoundOr(err)
	}
	return rev, nil
}

func (v *walView) SettingsHistory(ctx context.Context, id git.RepoId) (SettingsHistory, error) {
	entries, minSeq, err := v.logEntries(ctx, id, proto.EntryKindSettings)
	if err != nil {
		return SettingsHistory{}, err
	}
	h := SettingsHistory{MinSeq: minSeq, Entries: []SettingsEntry{}}
	for _, e := range entries {
		row := SettingsEntry{Seq: e.Seq, At: e.CreatedAt.Go()}
		if e.Settings != nil {
			row.Revision = e.Settings.Revision
			row.Author = e.Settings.Author
			row.Message = e.Settings.Message
			row.TOML = e.Settings.Toml
		}
		h.Entries = append(h.Entries, row)
	}
	return h, nil
}

func (v *walView) HeadSeq(ctx context.Context, id git.RepoId) (uint64, error) {
	if v.engine == nil {
		return 0, ErrPending
	}
	m, err := v.engine.Manifest(ctx, id)
	if err != nil {
		return 0, notFoundOr(err)
	}
	return m.HeadSeq, nil
}

func (v *walView) PushHistory(ctx context.Context, id git.RepoId, last int) ([]PushRecord, error) {
	if v.engine == nil {
		return nil, ErrPending
	}
	if last <= 0 {
		last = 10
	}
	m, err := v.engine.Manifest(ctx, id)
	if err != nil {
		return nil, notFoundOr(err)
	}
	if m.HeadSeq == 0 {
		return []PushRecord{}, nil
	}
	from := m.MinSeq
	if head := m.HeadSeq; head >= uint64(last) && head-uint64(last)+1 > from {
		from = head - uint64(last) + 1
	}
	entries, err := v.engine.ReadLog(ctx, id, from, m.HeadSeq)
	if err != nil {
		return nil, notFoundOr(err)
	}
	out := []PushRecord{}
	for _, e := range entries {
		if e.Kind != proto.EntryKindPush || e.Txn == nil {
			continue
		}
		rec := PushRecord{Seq: e.Seq, Refs: []PushRef{}, Atomic: e.Txn.Atomic, Principal: e.Meta["principal"]}
		if e.CreatedAt != nil {
			rec.At = e.CreatedAt.Go()
		}
		for _, u := range e.Txn.Updates {
			rec.Refs = append(rec.Refs, PushRef{Name: u.Name, Old: u.OldOid, New: u.NewOid})
		}
		out = append(out, rec)
	}
	if err := v.deriveForce(ctx, id, out); err != nil {
		return nil, err
	}
	return out, nil
}

// deriveForce stamps the per-ref force flag: old non-zero AND new is not a
// descendant of old (merge-base --is-ancestor semantics, §10).
func (v *walView) deriveForce(ctx context.Context, id git.RepoId, records []PushRecord) error {
	if len(records) == 0 {
		return nil
	}
	repo, _, err := v.localView(ctx, id, SyncServe)
	if err != nil {
		return err
	}
	for i := range records {
		for j := range records[i].Refs {
			r := &records[i].Refs[j]
			if r.Old == "" || isZeroHex(r.Old) || r.New == "" || isZeroHex(r.New) {
				continue // create/delete: not a force question
			}
			if _, err := v.gitCmd(ctx, repo, "merge-base", "--is-ancestor", r.Old, r.New); err != nil {
				r.Force = true
			}
		}
	}
	return nil
}

// logEntries streams the live log and returns the entries of one kind.
func (v *walView) logEntries(ctx context.Context, id git.RepoId, kind proto.EntryKind) ([]*proto.LogEntry, uint64, error) {
	if v.engine == nil {
		return nil, 0, ErrPending
	}
	m, err := v.engine.Manifest(ctx, id)
	if err != nil {
		return nil, 0, notFoundOr(err)
	}
	if m.HeadSeq == 0 {
		return []*proto.LogEntry{}, 0, nil
	}
	entries, err := v.engine.ReadLog(ctx, id, m.MinSeq, m.HeadSeq)
	if err != nil {
		return nil, 0, notFoundOr(err)
	}
	out := []*proto.LogEntry{}
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out, m.MinSeq, nil
}

// NewEnv is the constructor internal/server calls once at startup (§8.10
// order). engine may be nil — handlers answer 503 for the pending surfaces.
// The tasks table (Env.Tasks) and version/hostname are bound by the caller.
func NewEnv(st store.ObjectStore, repos RepoRegistry, cfg *config.Config, engine WalEngine, version, hostname string) *Env {
	e := &Env{
		Store:    st,
		Repos:    repos,
		Cfg:      cfg,
		Version:  version,
		Hostname: hostname,
	}
	e.Ready()
	if engine != nil {
		binary := "git"
		if cfg != nil && cfg.Git.Binary != "" {
			binary = cfg.Git.Binary
		}
		e.Repo = &walView{engine: engine, layer: git.NewLayer(), binary: binary}
	}
	return e
}
