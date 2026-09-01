package maintain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- memory object store (fakes for the CAS'd control-plane keys) -------------

type memObject struct {
	data    []byte
	version store.Version
}

// memStore is a minimal ObjectStore: Get/Put/Delete/List/ListPrefixes; the
// bulk-only methods report unsupported. Versions are a global counter.
type memStore struct {
	mu       sync.Mutex
	objects  map[string]*memObject
	versionc uint64
}

func newMemStore() *memStore { return &memStore{objects: map[string]*memObject{}} }

var errUnsupported = errors.New("unsupported by memStore")

func (s *memStore) Backend() string { return "memory" }

func (s *memStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil, store.NewNotFound(key)
	}
	if opts.IfNoneMatch != "" && opts.IfNoneMatch == o.version {
		return store.NotModified{Version: o.version}, nil
	}
	cp := make([]byte, len(o.data))
	copy(cp, o.data)
	return store.Object{Meta: store.ObjectMeta{Key: key, Size: int64(len(cp)), Version: o.version}, Body: rc(cp)}, nil
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func rc(b []byte) nopCloser { return nopCloser{strings.NewReader(string(b))} }

func (s *memStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil, nil
	}
	return &store.ObjectMeta{Key: key, Size: int64(len(o.data)), Version: o.version}, nil
}

func (s *memStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	switch opts.Mode {
	case store.PutCreate:
		if ok {
			return store.ObjectMeta{}, store.NewPrecondition(key, o.version)
		}
	case store.PutUpdate:
		if !ok || o.version != opts.IfVersion {
			var cur store.Version
			if ok {
				cur = o.version
			}
			return store.ObjectMeta{}, store.NewPrecondition(key, cur)
		}
	}
	s.versionc++
	v := store.Version(fmt.Sprintf("%d", s.versionc))
	data := append([]byte(nil), body.Bytes...)
	s.objects[key] = &memObject{data: data, version: v}
	return store.ObjectMeta{Key: key, Size: int64(len(data)), Version: v}, nil
}

func (s *memStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil
	}
	if ifVersion != "" && o.version != ifVersion {
		return store.NewPrecondition(key, o.version)
	}
	delete(s.objects, key)
	return nil
}

func (s *memStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > startAfter {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	type snap struct {
		key string
		obj *memObject
	}
	items := make([]snap, 0, len(keys))
	for _, k := range keys {
		items = append(items, snap{k, s.objects[k]})
	}
	s.mu.Unlock()
	for _, it := range items {
		if err := fn(store.ObjectMeta{Key: it.key, Size: int64(len(it.obj.data)), Version: it.obj.version}); err != nil {
			return err
		}
	}
	return nil
}

func (s *memStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	return errUnsupported
}

func (s *memStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return nil, errUnsupported
}

func (s *memStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return nil, errUnsupported
}

func (s *memStore) SupportsCompose() bool { return false }
func (s *memStore) ComposeIsNative() bool { return false }
func (s *memStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errUnsupported
}

func (s *memStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

// ---- fake repos / engine -------------------------------------------------------

type packLocal struct{ checksum string }

// fakeRepo implements Repo in-memory (git machinery faked at the GitOps
// boundary).
type fakeRepo struct {
	id      string
	dir     string
	m       *proto.Manifest
	version string
	refs    map[string]string
	entries []*proto.LogEntry

	// knobs / observation
	packMuBusy  bool
	annotated   []string
	compacts    []compactCall
	adds        []addCall
	published   []publishCall
	checkpoints []string
	git         *fakeGit
	store       *memStore
	fsckReport  *proto.FsckReport
	syncErr     error
}

type compactCall struct {
	checksum   string
	supersedes []string
	tier       uint32
}
type addCall struct {
	checksum string
	tier     uint32
}
type publishCall struct {
	refs []string
	meta map[string]string
}

func (r *fakeRepo) ID() string  { return r.id }
func (r *fakeRepo) Dir() string { return r.dir }
func (r *fakeRepo) Local() *git.LocalRepo {
	return &git.LocalRepo{Root: r.dir, Path: r.dir}
}
func (r *fakeRepo) Prefix() string { return "repos/" + r.id + "/" }

func (r *fakeRepo) Manifest() (*proto.Manifest, string) { return r.m, r.version }

func (r *fakeRepo) SyncRefs(ctx context.Context) error { return r.syncErr }

func (r *fakeRepo) RefValues(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.refs {
		out[k] = v
	}
	return out, nil
}

func (r *fakeRepo) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	var out []*proto.LogEntry
	for _, e := range r.entries {
		if e.Seq >= from && (to == 0 || e.Seq <= to) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *fakeRepo) RefsAtSeq(ctx context.Context, seq uint64) (*RefsView, error) {
	return &RefsView{}, nil
}

func (r *fakeRepo) RefsAsOf(ctx context.Context, at time.Time) (*RefsView, error) {
	return &RefsView{}, nil
}

func (r *fakeRepo) WriteCheckpoint(ctx context.Context, trigger string) error {
	r.checkpoints = append(r.checkpoints, trigger)
	return nil
}

func (r *fakeRepo) PublishCompact(ctx context.Context, pack *PreparedPack, supersedes []string, meta map[string]string) (uint64, error) {
	r.compacts = append(r.compacts, compactCall{pack.Checksum, supersedes, pack.Tier})
	// Mirror what the manifest CAS commits: drop the superseded set, add the
	// new pack ref.
	var live []*proto.PackRef
	for _, p := range r.m.Packs {
		superseded := false
		for _, c := range supersedes {
			if p.Checksum == c {
				superseded = true
				break
			}
		}
		if !superseded {
			live = append(live, p)
		}
	}
	np := &proto.PackRef{Checksum: pack.Checksum, Seq: r.m.HeadSeq + 1, PackSize: pack.PackSize, Tier: pack.Tier}
	if pack.Tier == 2 {
		np.HasBitmap = true // the repack wrote --write-bitmap-index
	}
	r.m.Packs = append(live, np)
	r.m.HeadSeq++
	r.version = fmt.Sprintf("v%d", len(r.compacts))
	return r.m.HeadSeq, nil
}

func (r *fakeRepo) AddPack(ctx context.Context, path, checksum string, tier uint32, meta map[string]string) error {
	r.adds = append(r.adds, addCall{checksum, tier})
	return nil
}

func (r *fakeRepo) AnnotatePack(ctx context.Context, checksum string, hasRev, hasBitmap, hasCommitGraph bool) error {
	r.annotated = append(r.annotated, checksum)
	return nil
}

func (r *fakeRepo) PublishRefs(ctx context.Context, txn *proto.RefTransaction, meta map[string]string) (uint64, error) {
	names := make([]string, 0, len(txn.Updates))
	for _, u := range txn.Updates {
		names = append(names, u.Name)
		r.refs[u.Name] = u.NewOid
	}
	r.published = append(r.published, publishCall{names, meta})
	r.m.HeadSeq++
	return r.m.HeadSeq, nil
}

func (r *fakeRepo) TryLockPacks() (func(), bool) {
	if r.packMuBusy {
		return func() {}, false
	}
	r.packMuBusy = true
	return func() { r.packMuBusy = false }, true
}

func (r *fakeRepo) GitOps() GitOps { return r.git }

// fakeGit stands in for *git.Layer (the machinery boundary).
type fakeGit struct {
	mu            sync.Mutex
	repackCalls   int // FullRepack (the once-across-attempts invariant)
	geoCalls      int
	keepPacks     [][]string
	fullDiff      *git.PackDiff
	geoDiff       *git.PackDiff
	failRepack    bool
	historyCalls  int
	commitGraph   string
	historyPack   string
	fetchErr      error
	fetchPackPath string
}

func (g *fakeGit) GeometricRepack(ctx context.Context, repo *git.LocalRepo, factor int, bitmap bool, keepPacks []string) (*git.PackDiff, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.geoCalls++
	g.keepPacks = append(g.keepPacks, keepPacks)
	return g.geoDiff, nil
}

func (g *fakeGit) FullRepack(ctx context.Context, repo *git.LocalRepo, keepPacks []string) (*git.PackDiff, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repackCalls++
	g.keepPacks = append(g.keepPacks, keepPacks)
	if g.failRepack {
		return nil, errors.New("repack boom")
	}
	return g.fullDiff, nil
}

func (g *fakeGit) WriteCommitGraph(ctx context.Context, repo *git.LocalRepo, changedPaths bool, sideDir string) (string, error) {
	return g.commitGraph, nil
}

func (g *fakeGit) HistoryPack(ctx context.Context, repo *git.LocalRepo, base string) (string, error) {
	g.mu.Lock()
	g.historyCalls++
	g.mu.Unlock()
	return g.historyPack, nil
}

func (g *fakeGit) FetchObjectsAsPack(ctx context.Context, repo *git.LocalRepo, u git.UpstreamSpec, oids []string) (string, error) {
	if g.fetchErr != nil {
		return "", g.fetchErr
	}
	return g.fetchPackPath, nil
}

func (g *fakeGit) Snapshot(repo *git.LocalRepo) (*git.RefSnapshot, error) {
	return &git.RefSnapshot{}, nil
}

// fakeEngine implements Engine over fake repos.
type fakeEngine struct {
	cfg   *config.Config
	repos []string
	byID  map[string]*fakeRepo
	st    *memStore
	tasks *fakeTasks
	host  string
}

func newFakeEngine(cfg *config.Config, repos ...*fakeRepo) *fakeEngine {
	e := &fakeEngine{
		cfg:   cfg,
		byID:  map[string]*fakeRepo{},
		st:    newMemStore(),
		tasks: &fakeTasks{},
		host:  "test-host",
	}
	for _, r := range repos {
		e.repos = append(e.repos, r.id)
		e.byID[r.id] = r
		r.store = e.st
	}
	return e
}

func (e *fakeEngine) Repos() []string { return e.repos }

func (e *fakeEngine) Open(ctx context.Context, id string) (Repo, error) {
	r, ok := e.byID[id]
	if !ok {
		return nil, fmt.Errorf("no such repo %s", id)
	}
	return r, nil
}

func (e *fakeEngine) Store() store.ObjectStore   { return e.st }
func (e *fakeEngine) Tasks() TaskRunner          { return e.tasks }
func (e *fakeEngine) HostConfig() *config.Config { return e.cfg }
func (e *fakeEngine) InstanceID() string         { return e.host }

// fakeTasks executes unit bodies inline (synchronous) or through a hook.
type fakeTasks struct {
	mu     sync.Mutex
	starts []string
	hook   func(repo, kind string, fn func(ctx context.Context, t TaskLogger) error) error
}

func (t *fakeTasks) Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, t TaskLogger) error) error {
	t.mu.Lock()
	t.starts = append(t.starts, repo+" "+kind)
	hook := t.hook
	t.mu.Unlock()
	if hook != nil {
		return hook(repo, kind, fn)
	}
	return fn(ctx, nopLogger{})
}

type nopLogger struct{}

func (nopLogger) Notice(string)                           {}
func (nopLogger) Progress(string, uint64, uint64, string) {}

// fakePlanner implements BundlePlanner.
type fakePlanner struct {
	slots    map[string][]Slot // repo → table
	built    []string
	buildsOK bool
	fireAt   map[string]time.Time
	planErr  error
}

func (p *fakePlanner) Plan(ctx context.Context, repo string, eff *config.Config, m *proto.Manifest, now time.Time) ([]Slot, error) {
	if p.planErr != nil {
		return nil, p.planErr
	}
	return p.slots[repo], nil
}

func (p *fakePlanner) Build(ctx context.Context, repo string, s Slot) (bool, error) {
	p.built = append(p.built, repo+" "+s.Strategy)
	return p.buildsOK, nil
}

func (p *fakePlanner) PreviousFire(s config.BundleStrategy, now time.Time) time.Time {
	if t, ok := p.fireAt[s.Name]; ok {
		return t
	}
	return now.Add(-24 * time.Hour)
}

// fakeLeaser implements Leaser with an optional held-name set.
type fakeLeaser struct {
	held     map[string]bool
	acquired []string
}

func (l *fakeLeaser) Acquire(ctx context.Context, name, holder, purpose string, ttl, skew time.Duration) (func(), error) {
	if l.held[name] {
		return nil, ErrLeaseHeld
	}
	l.acquired = append(l.acquired, name)
	return func() {}, nil
}

// snapshotFor assembles a test Snapshot for plan-table tests.
func snapshotFor(eff *config.Config, m *proto.Manifest, fsck *proto.FsckReport, present []string) *Snapshot {
	local := LocalState{Present: map[string]bool{}}
	for _, c := range present {
		local.Present[c] = true
	}
	return &Snapshot{ID: "acme/widget", Manifest: m, Eff: eff, Fsck: fsck, Local: local}
}

func defaultEff() *config.Config {
	eff := config.Defaults()
	eff.Cache.Dir = "/cache"
	return eff
}

// pack is a PackRef shorthand.
func pack(checksum string, seq, size, objects uint64, tier uint32) *proto.PackRef {
	return &proto.PackRef{Checksum: checksum, Seq: seq, PackSize: size, ObjectCount: objects, Tier: tier}
}
