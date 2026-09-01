// registry.go — the repo registry (doc 05 §5.1.1, §5.1.5): open/create/delete/list,
// the 30s listing cache, and the registry-owned background goroutines.
package wal

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Registry is the process-wide entry point for repo handles (05 §5.1.1).
type Registry struct {
	st        store.ObjectStore
	cfg       *config.Config
	cacheRoot string // cache.dir; repo dir = <root>/<owner>/<name>.git

	mu       sync.Mutex // guards the maps below only; never held across I/O
	repos    map[string]*RepoHandle
	opens    Group // per-repo open single-flight (13 §3)
	creates  Group // per-repo create single-flight
	tasks    *TaskTable
	blocks   *BlockCache
	listing  listingCache
	wg       sync.WaitGroup // registry-owned background goroutines
	ctx      context.Context
	cancel   context.CancelFunc
	gitLayer *git.Layer
	vals     *configVals
	instance string // writer id stamped into manifests/entries
	hostname string
}

// NewRegistry builds a Registry over st and starts its background goroutines
// (evictor, listing refresher, task janitor). Close cancels and joins them.
func NewRegistry(ctx context.Context, st store.ObjectStore, cfg *config.Config) *Registry {
	cctx, cancel := context.WithCancel(ctx)
	host, _ := os.Hostname()
	r := &Registry{
		st:        st,
		cfg:       cfg,
		cacheRoot: cfg.Cache.Dir,
		repos:     map[string]*RepoHandle{},
		tasks:     newTaskTable(host, cctx),
		blocks:    newBlockCache(int64(cfg.Cache.RemoteBlockBytes)),
		cancel:    cancel,
		ctx:       cctx,
		hostname:  host,
		instance:  instanceID(host),
		vals:      newConfigVals(cfg),
	}
	if w := time.Duration(cfg.Telemetry.LockWaitWarn); w > 0 {
		SetLockWaitWarn(w)
	}

	r.wg.Add(3)
	go r.evictorLoop(cctx)
	go r.listingRefresher(cctx)
	go func() { defer r.wg.Done(); r.tasks.janitor(cctx) }()
	return r
}

// Close shuts down: cancels the registry context (draining tasks, stopping the
// publisher per handle teardown) and waits for the owned goroutines.
func (r *Registry) Close() {
	r.cancel()
	r.wg.Wait()
	r.mu.Lock()
	handles := make([]*RepoHandle, 0, len(r.repos))
	for _, h := range r.repos {
		handles = append(handles, h)
	}
	r.repos = map[string]*RepoHandle{}
	r.mu.Unlock()
	for _, h := range handles {
		h.teardown()
	}
}

// Store exposes the object store (server-level callers: bundles, events).
func (r *Registry) Store() store.ObjectStore { return r.st }

// Config exposes the host config.
func (r *Registry) Config() *config.Config { return r.cfg }

// GitLayer returns the git layer used for local repo operations.
func (r *Registry) GitLayer() *git.Layer {
	if r.gitLayer == nil {
		r.gitLayer = git.NewLayer()
	}
	return r.gitLayer
}

// Tasks returns the instance task table.
func (r *Registry) Tasks() *TaskTable { return r.tasks }

// InstanceID is the writer id this instance stamps into the WAL.
func (r *Registry) InstanceID() string { return r.instance }

// manifestKey is the repo-relative manifest object key.
func manifestKey(id string) string {
	owner, name, _ := strings.Cut(id, "/")
	return git.RepoId{Owner: owner, Name: name}.StorePrefix() + store.Manifest
}

// ---- open -------------------------------------------------------------------

// Open returns the handle for id, opening it if needed (05 §5.1.2).
func (r *Registry) Open(ctx context.Context, id string) (*RepoHandle, error) {
	if _, err := git.ParseRepoId(id); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if h, ok := r.repos[id]; ok {
		r.mu.Unlock()
		return h, nil
	}
	r.mu.Unlock()

	val, err := r.opens.DoCtx(ctx, "open:"+id, func() (any, error) {
		// Double-check under the single-flight: another opener may have won.
		r.mu.Lock()
		if h, ok := r.repos[id]; ok {
			r.mu.Unlock()
			return h, nil
		}
		r.mu.Unlock()
		return r.openSlow(ctx, id)
	})
	if err != nil {
		return nil, err
	}
	return val.(*RepoHandle), nil
}

func (r *Registry) openSlow(ctx context.Context, id string) (*RepoHandle, error) {
	// 1. GET manifest.pb. Absent → ErrNotFound.
	body, meta, err := store.GetBytes(ctx, r.st, manifestKey(id), store.GetOptions{})
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, ErrNotFound(id)
	}
	m, err := proto.UnmarshalManifest(body)
	if err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: "manifest", Wrapped: err}
	}

	// 2. Open-or-init the local dir.
	repo, err := r.openOrInitLocal(m)
	if err != nil {
		return nil, err
	}

	// 3. Load persisted state; corrupt/missing → zero-value defaults.
	state := loadState(repo.Path)

	h := newHandle(r, id, m, string(meta.Version), repo, state)
	r.mu.Lock()
	// A racing opener may have published its handle first; keep the winner.
	if existing, ok := r.repos[id]; ok {
		r.mu.Unlock()
		h.teardown()
		return existing, nil
	}
	r.repos[id] = h
	r.mu.Unlock()

	// 4. Catch up immediately when the state lags the manifest.
	if state.AppliedSeq < m.HeadSeq {
		if err := h.catchUp(ctx); err != nil {
			// The handle stays usable; the next sync replays the delta.
			logWarnf("open %s: initial applyDelta failed: %v", id, err)
		}
	}
	return h, nil
}

// openOrInitLocal opens <cache.dir>/<owner>/<name>.git, running git init when
// absent with the manifest's object format (05 §5.1.2 step 2).
func (r *Registry) openOrInitLocal(m *proto.Manifest) (*git.LocalRepo, error) {
	id, err := git.ParseRepoId(m.Repo)
	if err != nil {
		return nil, err
	}
	if repo, err := git.OpenLocalRepo(r.cacheRoot, id); err == nil && repo != nil {
		return repo, nil
	}
	format, ferr := git.ObjectFormatFrom(m.ObjectFormat)
	if ferr != nil {
		format = git.Sha1
	}
	return git.InitLocalRepo(r.cacheRoot, id, format)
}

// ---- create -----------------------------------------------------------------

// Create makes a new repo: PutCreate of the manifest, then the local init
// (05 §5.1.2 create). A 412 surfaces as ErrExists.
func (r *Registry) Create(ctx context.Context, id string, format git.ObjectFormat) (*RepoHandle, error) {
	if _, err := git.ParseRepoId(id); err != nil {
		return nil, err
	}
	val, err := r.creates.DoCtx(ctx, "create:"+id, func() (any, error) {
		return r.createSlow(ctx, id, format)
	})
	if err != nil {
		return nil, err
	}
	return val.(*RepoHandle), nil
}

func (r *Registry) createSlow(ctx context.Context, id string, format git.ObjectFormat) (*RepoHandle, error) {
	rid, _ := git.ParseRepoId(id)
	key := rid.StorePrefix() + store.Manifest
	m := &proto.Manifest{
		FormatVersion: proto.WALFormatVersion,
		Repo:          id,
		ObjectFormat:  format.String(),
		HeadSeq:       0,
		MinSeq:        0,
		Revision:      1,
		Writer:        r.instance,
		UpdatedAt:     TsPtr(time.Now().UTC()),
	}
	meta, err := r.st.Put(ctx, key, store.PutBody{Bytes: m.Marshal()},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
	if err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, ErrExists(id)
		}
		return nil, &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
	}

	repo, err := git.InitLocalRepo(r.cacheRoot, rid, format)
	if err != nil {
		return nil, err
	}
	state := &RepoState{} // fresh: applied_seq 0, revision 1 lands on first sync
	saveState(repo.Path, state)

	h := newHandle(r, id, m, string(meta.Version), repo, state)
	r.mu.Lock()
	if existing, ok := r.repos[id]; ok {
		r.mu.Unlock()
		h.teardown()
		return existing, nil
	}
	r.repos[id] = h
	r.mu.Unlock()
	return h, nil
}

// ---- delete -----------------------------------------------------------------

// Delete removes a repo. The manifest delete is the linearization point; it
// happens FIRST so new opens fail immediately, then the repo's objects page
// away and the local dir goes (05 §5.1.2 delete).
func (r *Registry) Delete(ctx context.Context, id string) error {
	if _, err := git.ParseRepoId(id); err != nil {
		return err
	}

	r.mu.Lock()
	h := r.repos[id]
	delete(r.repos, id)
	r.mu.Unlock()
	if h != nil {
		h.teardown()
	}

	rid, _ := git.ParseRepoId(id)
	prefix := rid.StorePrefix()

	// Linearization point: delete the manifest first.
	if err := r.st.Delete(ctx, prefix+store.Manifest, ""); err != nil && !store.IsNotFound(err) {
		return &WalError{Kind: WalErrStore, Detail: prefix + store.Manifest, Wrapped: err}
	}

	// Page through the rest sequentially — slow and paged, not a hot path.
	var after string
	for {
		var keys []string
		err := r.st.List(ctx, prefix, after, func(m store.ObjectMeta) error {
			keys = append(keys, m.Key)
			return nil
		})
		if err != nil {
			return &WalError{Kind: WalErrStore, Detail: prefix, Wrapped: err}
		}
		if len(keys) == 0 {
			break
		}
		for _, k := range keys {
			if err := r.st.Delete(ctx, k, ""); err != nil && !store.IsNotFound(err) {
				return &WalError{Kind: WalErrStore, Detail: k, Wrapped: err}
			}
		}
		after = keys[len(keys)-1]
	}

	if err := os.RemoveAll(rid.LocalDir(r.cacheRoot)); err != nil {
		return &WalError{Kind: WalErrIo, Detail: rid.LocalDir(r.cacheRoot), Wrapped: err}
	}
	return nil
}

// Get returns the open handle without opening (nil when not open).
func (r *Registry) Get(id string) *RepoHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.repos[id]
}

// ---- listing (05 §5.1.5) ----------------------------------------------------

type listingCache struct {
	mu    sync.Mutex
	at    time.Time
	repos []string
	ttl   time.Duration
}

// List returns the repo ids, cached for 30s. Never errors on partial refresh
// failures: the last good snapshot is served (05 §5.1.5).
func (r *Registry) List() []string {
	r.listing.mu.Lock()
	ttl := r.listing.ttl
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	fresh := time.Since(r.listing.at) < ttl && r.listing.repos != nil
	if fresh {
		out := r.listing.repos
		r.listing.mu.Unlock()
		return out
	}
	r.listing.mu.Unlock()

	repos := r.refreshList(r.ctx)
	r.listing.mu.Lock()
	if repos != nil {
		r.listing.repos = repos
		r.listing.at = time.Now()
		out := repos
		r.listing.mu.Unlock()
		return out
	}
	// Refresh failed: serve the last good snapshot.
	out := r.listing.repos
	r.listing.mu.Unlock()
	return out
}

func (r *Registry) refreshList(ctx context.Context) []string {
	owners, err := listPrefixes(ctx, r.st, "repos/")
	if err != nil {
		logWarnf("repo list refresh: %v", err)
		return nil
	}
	var mu sync.Mutex
	var out []string
	for _, owner := range owners {
		candidates, err := listPrefixes(ctx, r.st, owner)
		if err != nil {
			logWarnf("repo list refresh %s: %v", owner, err)
			continue
		}
		g, gctx := store.WithContext(ctx)
		g.SetLimit(8)
		for _, cand := range candidates {
			cand := cand // repo prefix "repos/<owner>/<name>/"
			g.Go(func() error {
				// Absent manifest = deleted repo: skip.
				ok, err := store.Exists(gctx, r.st, cand+store.Manifest)
				if err == nil && ok {
					mu.Lock()
					out = append(out, strings.TrimSuffix(strings.TrimPrefix(cand, "repos/"), "/"))
					mu.Unlock()
				}
				return nil // list never fails on one repo
			})
		}
		_ = g.Wait()
	}
	return out
}

func listPrefixes(ctx context.Context, s store.ObjectStore, prefix string) ([]string, error) {
	var out []string
	err := s.ListPrefixes(ctx, prefix, func(p string) error {
		out = append(out, p)
		return nil
	})
	return out, err
}

// listingRefresher keeps the 30s cache warm so List() answers from memory.
func (r *Registry) listingRefresher(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if repos := r.refreshList(ctx); repos != nil {
				r.listing.mu.Lock()
				r.listing.repos = repos
				r.listing.at = time.Now()
				r.listing.mu.Unlock()
			}
		}
	}
}

func instanceID(host string) string {
	if host == "" {
		host = "unknown"
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err == nil {
		return fmt.Sprintf("%s-%x", host, b)
	}
	return host
}
