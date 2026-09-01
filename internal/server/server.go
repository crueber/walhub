// Package server implements the walhub HTTP server (doc 06_server_http.md):
// the ordered middleware chain (§2.2), the chi route tree (§3), git smart HTTP
// (§4), immutable static objects (§5), LFS (§6), auth in all three modes (§8),
// setup/bootstrap (§3.4), health/metrics/startup (§10), TLS (§11), and
// two-phase drain (§12).
//
// The engine (internal/wal), the JSON API (internal/api), and the git
// subprocess layer (internal/git) are consumed through small seams defined in
// this package (§2.4): Engine (bind_wal.go) and RouteProvider (bind_api.go).
package server

import (
	"os"

	"log/slog"

	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// ServerKind is the "<kind>" of the Server header (§2.2 #4).
type ServerKind string

const (
	KindServerless ServerKind = "serverless"
	KindSSD        ServerKind = "ssd"
	KindDev        ServerKind = "dev"
)

// Server is the whole HTTP surface. One process serves git smart HTTP, the
// LFS basic protocol, immutable static objects, the JSON API with SSE, the SPA
// shell, the setup UI/API, and instance health/metrics (§1).
type Server struct {
	cfg    *config.Config
	store  store.ObjectStore
	engine Engine // bind_wal.go seam: sync levels, publish, registry open
	api    RouteProvider

	layer *git.Layer // git subprocess layer machinery (04_git.md)

	authSvc *AuthService
	metrics *Registry
	sem     *RepoSemaphores
	drain   *DrainState

	inflight  Inflight
	cacheRoot string // cache.dir; LFS spool + TLS files live under it

	version  string // build sha: build-time env → git short sha → "dev"
	instance string // name[/id] per MASTER_RUST_SPEC §3.4
	kind     ServerKind

	boot BootState // §3.4 boot decision: normal / defaults / setup-only

	tlsOn bool // whether this listener terminates TLS (scheme selection)

	// Now is overridable for tests.
	Now func() time.Time

	log *slog.Logger
}

// Options wires the composition (§10.4 step 5: build AppState).
type Options struct {
	Config    *config.Config
	Store     store.ObjectStore
	Engine    Engine
	API       RouteProvider // internal/api mount; nil in setup-only/defaults-less tests
	DataDir   string
	CacheRoot string // cache.dir; LFS spool + TLS files live under it
	Version   string
	Instance  string
	Kind      ServerKind
	TLSOn     bool
	Boot      BootState
	Log       *slog.Logger
	Now       func() time.Time
}

// BootState is the §3.4 boot decision tree outcome.
type BootState struct {
	// Mode: "normal" (config valid), "defaults" (no config file), or
	// "setup_only" (config file present but INVALID).
	Mode   string // "normal" | "defaults" | "setup_only"
	Errors []string
}

// InSetupOnly reports SETUP-ONLY MODE (§3.4).
func (b BootState) InSetupOnly() bool { return b.Mode == "setup_only" }

// New builds the AppState.
func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Kind == "" {
		o.Kind = KindDev
	}
	if o.Config == nil {
		o.Config = config.Defaults()
	}
	s := &Server{
		cfg:       o.Config,
		store:     o.Store,
		engine:    o.Engine,
		api:       o.API,
		layer:     git.NewLayer(),
		version:   o.Version,
		instance:  o.Instance,
		kind:      o.Kind,
		boot:      o.Boot,
		tlsOn:     o.TLSOn,
		Now:       o.Now,
		log:       o.Log,
		cacheRoot: o.CacheRoot,
	}
	s.inflight.high = int64(o.Config.Server.MaxConcurrentRequests)
	s.sem = NewRepoSemaphores(o.Config.Server.MaxConcurrentPerRepo)
	s.drain = NewDrainState()
	s.metrics = newRegistry()
	registerInventory(s.metrics)
	s.authSvc = NewAuthService(&o.Config.Server.Auth, o.Now)
	return s
}

// Version returns the build identity.
func (s *Server) Version() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

// serverHeaderValue is `walgit/<version> (<kind>; <name>[/<instance>])`.
func (s *Server) serverHeaderValue() string {
	v := "walgit/" + s.Version() + " (" + string(s.kind) + "; walhub"
	if s.instance != "" {
		v += "/" + s.instance
	}
	return v + ")"
}

// cacheDir resolves a directory under the cache root (§6.3 spool, §11 TLS);
// without a configured root it falls back to the OS temp dir (tests/dev).
func (s *Server) cacheDir(sub string) string {
	root := s.cacheRoot
	if root == "" {
		root = os.TempDir() + "/walgit-cache"
	}
	return root + "/" + sub
}

// Auth exposes the auth service for composition (§10.4 step 5).
func (s *Server) Auth() *AuthService { return s.authSvc }

// Drain exposes the DrainState for the signal handler (§12).
func (s *Server) Drain() *DrainState { return s.drain }

// Inflight exposes the global gauge for the drain waiter (§12).
func (s *Server) Inflight() *Inflight { return &s.inflight }

// Metrics exposes the registry for background loops.
func (s *Server) Metrics() *Registry { return s.metrics }

// Config exposes the effective config.
func (s *Server) Config() *config.Config { return s.cfg }

// isGitClient reports whether the UA looks like git (§4.2 condition 1).
func isGitClient(ua string) bool {
	return hasPrefixFold(ua, "git/") || hasPrefixFold(ua, "jgit/") || containsFold(ua, "git-lfs")
}
