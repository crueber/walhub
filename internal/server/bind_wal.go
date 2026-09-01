package server

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// Engine is the internal/server seam over the WAL engine (internal/wal, doc
// 05; §2.4). The server composes git-layer calls directly for smart HTTP and
// calls the engine only through this narrow surface: sync levels, publish,
// registry open, placement, and bundle listing.
type Engine interface {
	// Sync materializes the repo to the given level (per-request refs sync).
	Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error

	// Repo opens the local repo for git operations; create=true auto-creates
	// with the given object format (server.auto_create_on_push). Unknown repo
	// with create=false → a *wal.WalError with Kind wal.WalErrNotFound.
	Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error)

	// Publish applies a parsed receive-pack request (commands already parsed,
	// pack already ingested + connectivity-checked by the git layer) to the
	// WAL; seq is the commit point. Ref conflicts surface in the result.
	Publish(ctx context.Context, id git.RepoId, req *git.PushRequest, principal string, access wal.ObjectAccess) (wal.PublishResult, error)

	// Placement answers where the repo is served/maintained (§4.3).
	Placement(ctx context.Context, id git.RepoId) (Placement, error)

	// Bundles answers the bundle list for the repo (fulls + chain), already
	// filtered. filter != "" and not accepted → the handler 400s first.
	Bundles(ctx context.Context, id git.RepoId, filter string) (BundleList, error)

	// AutoCreateOnPush reports the effective flag for this repo.
	AutoCreate(ctx context.Context, id git.RepoId) bool
}

// Placement is the maintainer-heartbeat-derived placement decision (§4.3,
// 10_maintenance.md). servedBy is the maintaining host when !serve.
type Placement struct {
	Serve           bool
	ServeExclude    bool
	Maintain        bool
	MaintainExclude bool
	ServedBy        string
}

// BundleEntry is one bundle object in a bundle list.
type BundleEntry struct {
	Strategy string // e.g. "full" | chain name
	Name     string
}

// BundleList is the git-config bundle list: fulls + chain (D17).
type BundleList struct {
	Fulls []BundleEntry
	Chain []BundleEntry
}

// Clone copies the list (callers may mutate).
func (bl BundleList) Clone() BundleList {
	out := BundleList{Fulls: make([]BundleEntry, len(bl.Fulls)), Chain: make([]BundleEntry, len(bl.Chain))}
	copy(out.Fulls, bl.Fulls)
	copy(out.Chain, bl.Chain)
	return out
}

// bundleKey maps a BundleEntry to its store key (§5 static path).
func bundleKey(id git.RepoId, e BundleEntry) string {
	return id.StorePrefix() + "bundles/" + e.Strategy + "/" + e.Name
}

// lfsKey is the store key for one LFS object.
func lfsKey(id git.RepoId, oid string) string { return id.StorePrefix() + "lfs/objects/" + oid }

// ---- the production engine over wal.Registry (Wave 4a integration) -----------

// WalEngine binds Engine (and the internal/api WalEngine structural seam) onto
// a live wal.Registry. One adapter per process.
type WalEngine struct {
	reg *wal.Registry
	cfg *config.Config
}

// Compile-time shape checks against wal's frozen types (types.go).
var (
	_ = wal.LevelRefs
	_ = wal.LevelServe
	_ = wal.LevelFull
	_ = wal.PublishResult{}
	_ = wal.RefResult{}
	_ = wal.ErrNotFound
)

// NewWalEngine assembles the production engine over the registry.
func NewWalEngine(reg *wal.Registry, cfg *config.Config) *WalEngine {
	return &WalEngine{reg: reg, cfg: cfg}
}

func (e *WalEngine) open(ctx context.Context, id git.RepoId) (*wal.RepoHandle, error) {
	h, err := e.reg.Open(ctx, id.String())
	if err != nil {
		if store.IsNotFound(err) {
			// Absent manifest surfaces as the engine's not-found error so the
			// HTTP layer maps it to 404 and auto-create sees a clean miss.
			return nil, wal.ErrNotFound(id.String())
		}
		return nil, err
	}
	return h, nil
}

// Sync materializes the repo to the level; unknown repo → wal not-found.
func (e *WalEngine) Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error {
	h, err := e.open(ctx, id)
	if err != nil {
		return err
	}
	g, err := h.Sync(ctx, level)
	if err != nil {
		return err
	}
	g.Release()
	return nil
}

// Repo opens (or creates) the serving copy and syncs it to the serve level so
// the git layer's connectivity check and ingest see the local pack set.
func (e *WalEngine) Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		var we *wal.WalError
		if create && errAsWalNotFound(err, &we) {
			h, err = e.reg.Create(ctx, id.String(), format)
		}
		if err != nil {
			return nil, err
		}
	}
	if !create {
		// Cold serving copies need the served pack set before ingest/connectivity.
		if g, serr := h.Sync(ctx, wal.LevelServe); serr == nil {
			g.Release()
		}
	}
	return h.Repo(), nil
}

func errAsWalNotFound(err error, target **wal.WalError) bool {
	for err != nil {
		if e, ok := err.(*wal.WalError); ok {
			*target = e
			return e.Kind == wal.WalErrNotFound
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Publish converts the receive-pack request onto the engine's publish funnel.
// The pack was ingested into the local serving copy by the git layer; the new
// pack is the one on disk that the manifest does not know yet (receive-pack
// is serialized per repo by the server's per-repo semaphore).
func (e *WalEngine) Publish(ctx context.Context, id git.RepoId, req *git.PushRequest, principal string, access wal.ObjectAccess) (wal.PublishResult, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		return wal.PublishResult{}, err
	}
	preq := wal.PublishRequest{Meta: map[string]string{"principal": principal}}
	txn := &proto.RefTransaction{Updates: make([]*proto.RefUpdate, 0, len(req.Commands)), PushOptions: req.PushOptions, Atomic: req.Has("atomic")}
	for _, c := range req.Commands {
		txn.Updates = append(txn.Updates, &proto.RefUpdate{Name: c.Ref, OldOid: c.Old, NewOid: c.New})
	}
	preq.Txn = txn

	if pack := e.newLocalPack(h); pack != nil {
		preq.Pack = pack
	}
	return h.Publish(ctx, preq)
}

// newLocalPack finds the freshly ingested pack: an idx in the local pack dir
// that the manifest's live pack set does not reference yet (newest first).
func (e *WalEngine) newLocalPack(h *wal.RepoHandle) *wal.PreparedPack {
	known := map[string]bool{}
	m, _ := h.ManifestSnapshot()
	if m != nil {
		for _, p := range m.Packs {
			known[p.Checksum] = true
		}
	}
	packDir := h.Repo().PackDir()
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return nil
	}
	type candidate struct {
		checksum string
		modTime  int64
	}
	var cands []candidate
	for _, f := range entries {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".idx") {
			continue
		}
		sum := strings.TrimSuffix(name, ".idx")
		if known[sum] {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		cands = append(cands, candidate{checksum: sum, modTime: info.ModTime().UnixNano()})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].modTime > cands[j].modTime })
	c := cands[0]
	pack := &wal.PreparedPack{Checksum: c.checksum}
	pack.PackPath = filepath.Join(packDir, c.checksum+".pack")
	pack.IdxPath = filepath.Join(packDir, c.checksum+".idx")
	if fi, err := os.Stat(pack.PackPath); err == nil {
		pack.PackSize = uint64(fi.Size())
	} else {
		pack.PackPath = ""
	}
	if fi, err := os.Stat(pack.IdxPath); err == nil {
		pack.IdxSize = uint64(fi.Size())
	}
	return pack
}

// Placement answers the §4.3 decision from the host placement config.
func (e *WalEngine) Placement(ctx context.Context, id git.RepoId) (Placement, error) {
	p := e.cfg.Placement
	serve := matchAnyGlob(p.Serve, id.String()) && !matchAnyGlob(p.ServeExclude, id.String())
	maintain := matchAnyGlob(p.Maintain, id.String()) && !matchAnyGlob(p.MaintainExclude, id.String())
	return Placement{Serve: serve, Maintain: maintain}, nil
}

// matchAnyGlob matches `*`, `owner/*`, or `owner/name` (§5 rule 7).
func matchAnyGlob(globs []string, id string) bool {
	owner, _, _ := strings.Cut(id, "/")
	for _, g := range globs {
		switch {
		case g == "*":
			return true
		case strings.HasSuffix(g, "/*") && strings.HasPrefix(id, strings.TrimSuffix(g, "*")):
			return true
		case g == id:
			return true
		case g == owner+"/*" && strings.HasPrefix(id, owner+"/"):
			return true
		}
	}
	return false
}

// Bundles loads the repo's bundle list object (absent → empty list).
func (e *WalEngine) Bundles(ctx context.Context, id git.RepoId, filter string) (BundleList, error) {
	if filter != "" && filter != "blob:none" {
		return BundleList{}, nil
	}
	body, _, err := store.GetBytes(ctx, e.reg.Store(), id.StorePrefix()+store.BundleList, store.GetOptions{})
	if err != nil || body == nil {
		return BundleList{}, nil // absent or unreadable list = nothing advertised
	}
	list, err := proto.UnmarshalBundleList(body)
	if err != nil {
		return BundleList{}, nil
	}
	out := BundleList{Fulls: []BundleEntry{}, Chain: []BundleEntry{}}
	for _, b := range list.Bundles {
		name := strings.TrimPrefix(b.Key, "bundles/"+b.Strategy+"/")
		entry := BundleEntry{Strategy: b.Strategy, Name: name}
		if b.Kind == "full" {
			out.Fulls = append(out.Fulls, entry)
		} else {
			out.Chain = append(out.Chain, entry)
		}
	}
	return out, nil
}

// AutoCreate reports the effective auto_create_on_push flag (per-repo settings
// may override; the host value is the default path).
func (e *WalEngine) AutoCreate(ctx context.Context, id git.RepoId) bool {
	return e.cfg.Server.AutoCreateOnPush
}

// ---- internal/api WalEngine structural seam ----------------------------------

// ObjectAccess builds the per-request object reader from the open handle.
func (e *WalEngine) ObjectAccess(ctx context.Context, id git.RepoId) (wal.ObjectAccess, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		return wal.ObjectAccess{}, err
	}
	return wal.ObjectAccess{Local: h.Repo()}, nil
}

// Revision stamps renders with the manifest revision.
func (e *WalEngine) Revision(ctx context.Context, id git.RepoId) (uint64, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		return 0, err
	}
	m, _ := h.ManifestSnapshot()
	if m == nil {
		return 0, nil
	}
	return m.Revision, nil
}

// Manifest returns the stable manifest snapshot.
func (e *WalEngine) Manifest(ctx context.Context, id git.RepoId) (*proto.Manifest, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		return nil, err
	}
	m, _ := h.ManifestSnapshot()
	return m, nil
}

// ReadLog streams live log entries [from, to].
func (e *WalEngine) ReadLog(ctx context.Context, id git.RepoId, from, to uint64) ([]*proto.LogEntry, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		return nil, err
	}
	return h.ReadLog(ctx, from, to)
}

// PublishSettings publishes the D24 payload and reports the new revision.
func (e *WalEngine) PublishSettings(ctx context.Context, id git.RepoId, body []byte, message, author string) (uint64, error) {
	h, err := e.open(ctx, id)
	if err != nil {
		return 0, err
	}
	if err := h.PublishSettings(ctx, string(body), author, message, nil); err != nil {
		return 0, err
	}
	m, _ := h.ManifestSnapshot()
	if m == nil || m.Settings == nil {
		return 0, nil
	}
	return m.Settings.Revision, nil
}
