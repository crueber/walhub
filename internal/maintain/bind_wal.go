// bind_wal.go — the real binding onto internal/wal + internal/git (§1
// dependency direction: maintain → wal, git, store, config). Narrow seams
// from units.go are wired here. The bundle planner seam is bound onto
// internal/bundle (doc 08).
package maintain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/bundle"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// walEngine binds Engine onto wal.Registry.
type walEngine struct{ reg *wal.Registry }

// NewWalEngine adapts the registry.
func NewWalEngine(reg *wal.Registry) Engine { return walEngine{reg} }

func (e walEngine) Repos() []string          { return e.reg.List() }
func (e walEngine) Store() store.ObjectStore { return e.reg.Store() }
func (e walEngine) Tasks() TaskRunner        { return taskRunnerAdapter{e.reg.Tasks()} }
func (e walEngine) HostConfig() *config.Config {
	return e.reg.Config()
}
func (e walEngine) InstanceID() string { return e.reg.InstanceID() }

func (e walEngine) Open(ctx context.Context, id string) (Repo, error) {
	h, err := e.reg.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return walRepo{h: h}, nil
}

// taskRunnerAdapter binds TaskRunner onto wal.TaskTable (the (repo,kind)
// single-flight join, §7).
type taskRunnerAdapter struct{ t *wal.TaskTable }

func (a taskRunnerAdapter) Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, t TaskLogger) error) error {
	_, err := a.t.Run(ctx, repo, kind, params, func(tctx context.Context, task *wal.Task) error {
		return fn(tctx, walLogger{task})
	})
	return err
}

type walLogger struct{ t *wal.Task }

func (l walLogger) Notice(text string) { l.t.Notice(text) }

func (l walLogger) Progress(label string, done, total uint64, unit string) {
	l.t.Progress(label, done, total, unit)
}

// walRepo binds Repo onto wal.RepoHandle.
type walRepo struct{ h *wal.RepoHandle }

func (r walRepo) ID() string                          { return r.h.ID }
func (r walRepo) Dir() string                         { return r.h.Dir() }
func (r walRepo) Local() *git.LocalRepo               { return r.h.Repo() }
func (r walRepo) GitOps() GitOps                      { return r.h.Layer() }
func (r walRepo) Manifest() (*proto.Manifest, string) { return r.h.ManifestSnapshot() }

func (r walRepo) Prefix() string {
	id, err := git.ParseRepoId(r.h.ID)
	if err != nil {
		return "repos/" + r.h.ID + "/"
	}
	return id.StorePrefix()
}

func (r walRepo) SyncRefs(ctx context.Context) error {
	g, err := r.h.Sync(ctx, wal.LevelRefs)
	if err != nil {
		return err
	}
	g.Release()
	return nil
}

func (r walRepo) RefValues(ctx context.Context) (map[string]string, error) {
	if err := r.SyncRefs(ctx); err != nil {
		return nil, err
	}
	snap, err := r.h.Layer().Snapshot(r.h.Repo())
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(snap.Refs))
	for _, ref := range snap.Refs {
		out[ref.Name] = ref.Oid
	}
	return out, nil
}

func (r walRepo) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	return r.h.ReadLog(ctx, from, to)
}

func refsView(v *wal.RefsView) *RefsView {
	if v == nil {
		return nil
	}
	return &RefsView{Seq: v.Seq, Refs: v.Refs, HeadTarget: v.HeadTarget}
}

func (r walRepo) RefsAtSeq(ctx context.Context, seq uint64) (*RefsView, error) {
	v, err := r.h.RefsAtSeq(ctx, seq)
	return refsView(v), err
}

func (r walRepo) RefsAsOf(ctx context.Context, at time.Time) (*RefsView, error) {
	v, err := r.h.RefsAsOf(ctx, at)
	return refsView(v), err
}

// WriteCheckpoint is the exported wal checkpoint writer (§11; refs-level,
// idempotent: cp.seq == head_seq returns the existing checkpoint).
func (r walRepo) WriteCheckpoint(ctx context.Context, trigger string) error {
	return r.h.WriteCheckpoint(ctx, wal.CheckpointTrigger(trigger))
}

func (r walRepo) PublishCompact(ctx context.Context, pack *PreparedPack, supersedes []string, meta map[string]string) (uint64, error) {
	res, err := r.h.PublishCompact(ctx, toWalPack(pack), supersedes, meta)
	return res.Seq, err
}

func (r walRepo) AddPack(ctx context.Context, path, checksum string, tier uint32, meta map[string]string) error {
	_, err := r.h.AddPack(ctx, path, checksum, tier, meta)
	return err
}

func (r walRepo) AnnotatePack(ctx context.Context, checksum string, hasRev, hasBitmap, hasCommitGraph bool) error {
	return r.h.AnnotatePack(ctx, checksum, hasRev, hasBitmap, hasCommitGraph)
}

func (r walRepo) PublishRefs(ctx context.Context, txn *proto.RefTransaction, meta map[string]string) (uint64, error) {
	res, err := r.h.Publish(ctx, wal.PublishRequest{Txn: txn, Meta: meta})
	return res.Seq, err
}

// TryLockPacks: wal exposes the try-only packMu (§6.1 b).
func (r walRepo) TryLockPacks() (func(), bool) { return r.h.TryLockPacks() }

func toWalPack(p *PreparedPack) *wal.PreparedPack {
	if p == nil {
		return nil
	}
	return &wal.PreparedPack{
		Checksum:    p.Checksum,
		PackPath:    p.PackPath,
		IdxPath:     p.IdxPath,
		PackSize:    p.PackSize,
		IdxSize:     p.IdxSize,
		ObjectCount: p.ObjectCount,
		Tier:        p.Tier,
	}
}

// ---- bundle planner seam: internal/bundle (doc 08) ------------------------------

// walPlanner binds the §5 BundlePlanner onto internal/bundle's exported
// planner/builders over the same registry the maintainer drives.
type walPlanner struct{ reg *wal.Registry }

// bundleList reads the repo's bundles/list.pb (absent → empty list).
func (p walPlanner) bundleList(ctx context.Context, repo string) *proto.BundleList {
	st := p.reg.Store()
	body, _, err := store.GetBytes(ctx, st, repoPrefixOf(repo)+store.BundleList, store.GetOptions{})
	if err != nil || body == nil {
		return &proto.BundleList{}
	}
	list, err := proto.UnmarshalBundleList(body)
	if err != nil {
		return &proto.BundleList{}
	}
	return list
}

func (p walPlanner) strategies(eff *config.Config) ([]bundle.Strategy, error) {
	return bundle.FromConfig(eff.Bundles.Strategy, eff.Bundles.MinCommits)
}

func (p walPlanner) Plan(ctx context.Context, repo string, eff *config.Config, m *proto.Manifest, now time.Time) ([]Slot, error) {
	strategies, err := p.strategies(eff)
	if err != nil {
		return nil, err
	}
	list := p.bundleList(ctx, repo)
	adapter := &bundle.WalAdapter{R: p.reg}
	first, ok := adapter.FirstStateAt(repo)
	firstStateAt := func(string) (time.Time, bool) { return first, ok }
	hostFits := func(*bundle.Strategy) bool { return fits(eff, m) }
	states := bundle.PlanStates(repo, strategies, list, now, firstStateAt, hostFits)
	byName := bundle.ByName(strategies)
	out := make([]Slot, 0, len(states))
	for _, s := range states {
		kind := "full"
		if st := byName[s.Strategy]; st != nil {
			kind = st.Kind
		}
		row := Slot{Strategy: s.Strategy, Kind: kind, Slot: s.Slot, State: s.State}
		if s.State == "built" {
			row.BundleID = s.Detail
		} else {
			row.Detail = s.Detail
		}
		out = append(out, row)
	}
	return out, nil
}

func (p walPlanner) Build(ctx context.Context, repo string, s Slot) (bool, error) {
	eff := p.reg.Config()
	strategies, err := p.strategies(eff)
	if err != nil {
		return false, err
	}
	byName := bundle.ByName(strategies)
	st := byName[s.Strategy]
	if st == nil {
		return false, fmt.Errorf("bundle: unknown strategy %q", s.Strategy)
	}
	list := p.bundleList(ctx, repo)
	for _, b := range list.Bundles { // already built → nothing to do
		if b.Strategy == s.Strategy && b.Slot == s.Slot {
			return false, nil
		}
	}
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return false, err
	}
	packDir := h.Repo().PackDir()
	deps := bundle.Deps{
		Wal:          &bundle.WalAdapter{R: p.reg},
		Prim:         &bundle.GitPrimitives{L: h.Layer()},
		St:           p.reg.Store(),
		Tasks:        bundleTaskRunner{t: p.reg.Tasks()},
		CacheDir:     eff.Cache.Dir,
		HostID:       p.reg.InstanceID(),
		RepoDir:      h.Dir(),
		MinCommits:   eff.Bundles.MinCommits,
		List:         list,
		MainOnly:     eff.Bundles.MainOnly,
		ExtraRefs:    eff.Bundles.ExtraRefs,
		ObjectFormat: objectFormatOf(h),
		LocalPack: func(checksum string) (string, bool) {
			path := filepath.Join(packDir, checksum+".pack")
			if _, err := os.Stat(path); err != nil {
				return "", false
			}
			return path, true
		},
	}
	if err := bundle.BuildSlot(ctx, &deps, repo, strategies, st, time.Unix(int64(s.Slot), 0).UTC()); err != nil {
		return false, err
	}
	if len(deps.Verdicts) > 0 {
		if err := bundle.RecordVerdicts(ctx, p.reg.Store(), deps.Verdicts); err != nil {
			return false, err
		}
	}
	return true, nil
}

// PreviousFire walks the schedule backwards in day steps via Between (the
// §6.2 trigger-4 window start); schedules fire at least yearly, so the cap is
// bounded.
func (walPlanner) PreviousFire(s config.BundleStrategy, now time.Time) time.Time {
	c, err := bundle.ParseSchedule(s.Schedule)
	if err != nil {
		return time.Time{}
	}
	start := now
	for range 400 {
		start = start.AddDate(0, 0, -1)
		fires, err := c.Between(start, now)
		if err != nil {
			return time.Time{}
		}
		if len(fires) > 0 {
			return fires[len(fires)-1]
		}
	}
	return time.Time{}
}

func objectFormatOf(h *wal.RepoHandle) string {
	m, _ := h.ManifestSnapshot()
	if m != nil && m.ObjectFormat != "" {
		return m.ObjectFormat
	}
	return "sha1"
}

// bundleTaskRunner binds the bundle TaskRunner onto wal.TaskTable (kind
// "bundle", params {strategy, slot}; §8.9.1).
type bundleTaskRunner struct{ t *wal.TaskTable }

func (r bundleTaskRunner) RunBundle(ctx context.Context, repo string, params map[string]string, fn func(ctx context.Context, tr bundle.Reporter) error) error {
	_, err := r.t.Run(ctx, repo, "bundle", params, func(ctx context.Context, t *wal.Task) error {
		return fn(ctx, walLogger{t})
	})
	return err
}

// repoPrefixOf returns "repos/<owner>/<repo>/" for an "owner/repo" id.
func repoPrefixOf(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return "repos/" + owner + "/" + name + "/"
}

// NewWalMaintainer assembles the production maintainer over a registry.
func NewWalMaintainer(reg *wal.Registry, opt Options) *Maintainer {
	cfg := reg.Config()
	if opt.Fscker == nil {
		opt.Fscker = execFscker{}
	}
	if opt.Follow == nil {
		opt.Follow = &execFollow{CacheDir: cfg.Cache.Dir, Binary: cfg.Git.Binary}
	}
	if opt.Planner == nil {
		opt.Planner = walPlanner{reg: reg}
	}
	if opt.Leaser == nil {
		opt.Leaser = StoreLeaser{St: reg.Store()}
	}
	if opt.Interval == 0 {
		opt.Interval = time.Duration(cfg.Maintenance.Interval)
	}
	if opt.FollowInterval == 0 {
		opt.FollowInterval = time.Duration(cfg.Maintenance.FollowInterval)
	}
	if opt.HostName == "" && cfg.Maintenance.Host != "" {
		opt.HostName = cfg.Maintenance.Host
	}
	return New(NewWalEngine(reg), opt)
}
